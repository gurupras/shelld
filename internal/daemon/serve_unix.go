//go:build linux || darwin

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/gurupras/shelld/internal/sock"
)

// listen binds the socket, owner-only unless opts.SharedAccess. The caller
// holds the socket's instance lock, so replacing a dead socket cannot race
// another daemon starting.
//
// The listener keeps Go's unlink-on-close, which is what removes the socket file
// when the daemon stops. That timing is the whole reason the file is not removed
// anywhere else: UnixListener.Close unlinks the path and only then closes the
// descriptor, so the removal happens while this process is still bound.
//
// What that buys is not certainty about the inode -- unlink names a path, and a
// process that replaces a socket it can see is live could still have swapped this
// one -- but it narrows the loser to a server that had to ignore a live socket to
// get there. Unlinking after the descriptor is closed has no such floor: by then
// the socket really is dead, so a server doing exactly what removeDeadSocket does
// replaces it correctly and gets unlinked anyway, and is left serving under a name
// no client can reach. Measured against a racing binder, that is the difference
// between losing none of 600 sockets and losing some of every 600.
func listen(path string, opts ListenOptions) (net.Listener, error) {
	if err := removeDeadSocket(path); err != nil {
		return nil, err
	}
	// The socket is created with the umask's mode, so it is private from the
	// moment it exists. The umask is process-wide: at this point the daemon
	// creates no other files, and 0o077 still leaves anything created
	// meanwhile usable by its owner.
	old := syscall.Umask(0o077)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		if owner := ownerOf(path); owner != "" && (errors.Is(err, syscall.EADDRINUSE) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)) {
			return nil, fmt.Errorf("bind %s (%s): %w", path, owner, err)
		}
		return nil, fmt.Errorf("bind %s: %w", path, err)
	}
	if opts.SharedAccess {
		_ = os.Chmod(path, 0o666)
	}
	return ln, nil
}

// removeDeadSocket removes path only if it is a socket nothing holds -- nothing
// listens on it and no live descriptor is bound to it. A live socket (a daemon that
// doesn't take the instance lock, or another server) and anything that is not a
// socket are left alone.
func removeDeadSocket(path string) error {
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("bind %s: it exists and is not a socket; not replacing it", path)
	}
	// The message has to name the path, because it is the only place the operator
	// is told which file this is about, and it has to say what was found, because
	// what to do about it differs per state -- see whatHoldsIt. An undetermined
	// probe is not ErrInstanceHeld: nothing showed a holder, so run exits 1 and
	// not "held", in shelld-rs's words (remove_dead_socket in its serve.rs).
	switch b, why := isBoundWith(path, addressHeld); b {
	case bound:
		return fmt.Errorf("bind %s: %w -- %s", path, ErrInstanceHeld, whatHoldsIt(why))
	case boundUnknown:
		return fmt.Errorf("bind %s: cannot tell whether a process is listening on it; not replacing it -- %s", path, whatHoldsIt(why))
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove stale socket %s (%s): %w", path, ownerOf(path), err)
	}
	return nil
}

// whatHoldsIt describes a socket isBound would not call dead, given the error its
// connect failed with -- nil when the connect was accepted. It is the operator-facing
// half of the refusal, and it exists because one sentence cannot serve all five states
// isBound answers "bound" for, which this had to learn: a single line ending in "if you
// know it is dead, remove the file" was correct in some of them and an instruction to
// unlink a running daemon's address in the rest. An operator who followed it there
// removed the file, this daemon bound over it, and the peer was left serving under a name
// no client can reach -- the one outcome isBound's asymmetry exists to prevent, arrived
// at by doing what the error said. The audience makes that worse rather than better:
// cmdRun takes the instance lock before it gets here, so a same-version shelld never
// sees this message and everyone who does has something else on the path.
//
// The line the advice falls on is whether anything answered at this path.
//
// An accepted connect and EAGAIN both came back from a socket bound to this very file, so
// the file is a live listener's address and removing it is wrong however sure the operator
// is. Neither says to.
//
// The other three never got a reply, and none of them can settle what the file is.
// ECONNREFUSED reaches here only when addressHeld found the name, and that comparison is
// on the bind string: it cannot tell a live daemon's address from a dead file whose name
// an unrelated peer still holds after unlinking its own -- a real state, built by
// TestAStaleBindNameKeepsADeadSocketFile, where the file is dead and removing it is the
// only way past. EACCES says this process may not ask. A timeout says the connect did not
// finish. So all three name what was found, and offer the removal under a condition the
// operator can check.
//
// Which condition, exactly, is the part this had to get right twice. The first attempt
// offered the removal "if you know nothing serves this path", and that is not a gate: in
// every state that reaches it, nothing does. ECONNREFUSED *is* nothing serving the path, so
// an operator who checks it honestly and correctly passes it -- including in the two states
// the refusal exists to protect, a listener after shutdown(SHUT_RDWR) and a descriptor bound
// without listen. Both were measured following it to the outcome the refusal is for: the
// replacement binds and is reachable, then the holder closes its listener, Go unlinks the
// path on close, and the replacement is serving under a name with no file; or the holder's
// own listen() lands on an address whose file is gone and a second server takes the name
// unopposed.
//
// So the question the advice asks is not whether anything *serves* the path -- unanswerable
// here, and the same "yes" in all three -- but *which process holds it*, which the operator
// can answer even though this code will not. `ss -xlp` names the holder of a unix socket,
// including one that is bound without listening (state UNCONN, with users:(("name",pid=,fd=))).
// Gated on "the holder is not a server for this path", the stale-bind-name operator passes and
// the other two do not. Naming the tool is what turns a condition nobody can fail into one
// that discriminates: this code is already holding the inode that identifies the holder, and
// telling an operator to "stop that process if it is yours" without saying how to find out
// whose it is was the rest of the same defect. What the tool needs to answer is nameTheHolder's
// subject.
func whatHoldsIt(why error) string {
	switch {
	case why == nil:
		return "something is listening on it and accepted a connection, so this file is a live daemon's address; stop that daemon, or serve a different path"
	case errors.Is(why, syscall.EAGAIN):
		return "a listener is bound to it and its backlog is full, so a daemon is up and not keeping up; wait for it, or stop it"
	case errors.Is(why, syscall.ECONNREFUSED):
		// Still the two states it always was, and a full accept queue is deliberately
		// not a third: where a refusal can mean that, isBoundWith re-probes and reports
		// what the re-probe found, so a queue that had drained arrives here as the nil
		// above and not as a refusal. Naming it in this sentence as well would describe
		// a state that cannot reach it.
		return "a live descriptor holds this address without listening on it -- either a process that has it bound, or an unrelated peer that unlinked its own file and still holds the name; " + nameTheHolder
	case errors.Is(why, syscall.EACCES), errors.Is(why, syscall.EPERM):
		return "connecting to it is not permitted, so whether anything serves it cannot be determined here; " + nameTheHolder
	default:
		return fmt.Sprintf("the connect did not finish (%v), so nothing shows it is dead; retry, and if it persists %s", why, nameTheHolder)
	}
}

// nameTheHolder is the sentence the three open answers share: how to find out which process
// holds the path, and what that has to say before the file may go. One string rather than
// three, because the gate is the same in all three and it is the part that took two attempts
// to phrase -- see whatHoldsIt.
//
// It says sudo, and the reason is worth the words. /proc/net/unix gives the socket's inode,
// but turning an inode into a pid means matching it against every process's descriptors, and
// an unprivileged ss reads only the descriptors of processes it is allowed to inspect: for the
// rest it prints the socket and leaves the Process column empty. Measured here -- of 153
// listening unix sockets, 53 carried a users:(("name",pid=,fd=)) column and the other 100
// carried nothing.
//
// Same uid is not the permission, which matters because it is the assumption that makes the
// blank look safe. Linux gates /proc/<pid>/fd on ptrace access, and this host has
// /proc/sys/kernel/yama/ptrace_scope at 1, so the reader must be an *ancestor* of the holder
// and not merely its owner. Twelve of those 100 blank rows are paths under /run/user/1000
// owned by this very uid -- gpg-agent's among them, pid 100962, /proc/100962 owned by this
// user and /proc/100962/fd refusing to open. A daemonized shelld is the same shape: the
// operator's later shell is not its ancestor, so the process the operator most needs named is
// one their own uid does not buy them.
//
// That blank is this message's audience either way. cmdRun takes the instance lock before
// reaching here, so a same-version shelld never sees this -- everyone who does has some other
// process on the path, and an operator who ran the command unprivileged, saw no holder, and
// read that as "nothing holds it" would perform the removal this file exists to refuse. So
// the caveat is in the message and not only in this comment, and it says the column is often
// empty rather than that it is empty for other people's processes: the second reading is the
// one that would tell this operator the blank row was not theirs.
//
// ss is iproute2's and a minimal image may not have it. That is not the same set of hosts as
// the ones addressHeld cannot answer for, and it is larger: only the ECONNREFUSED branch
// consults addressHeld, so EAGAIN, EACCES and a timeout reach this advice on a host with no
// readable /proc too. By hand it is the same lookup at the same privilege -- the inode from
// /proc/net/unix, then the process holding it from /proc/*/fd.
const nameTheHolder = "run `sudo ss -xlp` and find this path to name the holder (unprivileged, the Process column is often empty even for your own processes, and empty is not an answer); then stop that process if it is yours, or remove the file only if the holder it names is not a server for this path"

func ownerOf(path string) string {
	if st, err := os.Lstat(path); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			return fmt.Sprintf("owned by uid %d", sys.Uid)
		}
	}
	return ""
}

// isBound reports whether anything may be bound to path. A connect that is
// accepted says so outright; past that the errno decides, and only one of them
// proves nothing is:
//
//	ENOENT        no file
//
// ECONNREFUSED reads like it belongs on that list and does not, which is why the
// kernel is asked a second question below. Linux returns it for three states, and two
// of them have a live descriptor holding the address:
//
//	bound, listen() never called          net.Listen passes through this state on
//	                                      every startup -- bind and listen are two
//	                                      syscalls, measured 6.4us apart
//	listening, then shutdown(SHUT_RDWR)    not a race at all: it lasts until the
//	                                      descriptor is closed
//	a socket file whose process is gone    the one state a removal is for
//
// So ECONNREFUSED decides nothing by itself, and addressHeld decides it instead: the
// file may go only when nothing holds the address. Where that cannot be answered, a
// refusal that survives being re-probed is read as removable -- see refusalIsStable,
// which is the whole of what is available there. Answering "bound" outright instead
// would make a SIGKILLed daemon's socket permanently unreclaimable, and that would
// bite every start on a host that cannot answer rather than only the rare state this
// exists for.
//
// That fallback is not darwin's alone. It is reachable on Linux too -- /proc not
// mounted, as in a distroless container or a chroot; mounted with subset=pid; openat
// denied by seccomp; a read failing partway through the file -- and on such a host this
// silently becomes the behaviour it replaced, with nothing recording that it did.
// Recording it is not obviously right either: the answer is asked on every start, so a
// log line would be per-start noise on exactly the hosts that can never do better.
//
// ENOTDIR and ENOTSOCK prove it too, as they do to shelld-rs (socket_is_bound in
// its lifecycle.rs): a path whose directory is not one, or whose file is not a
// socket, has nothing listening on it.
//
// Every other answer leaves the question open, and isBoundWith says so as
// boundUnknown; isBound answers an open question as bound. That asymmetry is the
// whole point, because the caller's next move on "not bound" is to unlink the file: reading a live daemon as gone makes
// removeDeadSocket unlink its socket and bind over it, leaving it running under a
// name no client can reach, which is the one failure a client cannot tell apart
// from the daemon having exited. Two open answers are reachable rather than
// theoretical, and both were measured here:
//
//	EAGAIN   a listener is bound and its backlog is full -- a daemon that is up
//	         and not keeping up. This is the state the asymmetry exists for.
//	EACCES   connect is denied by the socket's mode, or by a directory on the way
//	         to it. Mode 000 denies its own owner, and the check happens before
//	         the listener's state is looked at, so a dead socket answers EACCES
//	         too: the errno does not say which state the file is in, only that
//	         this process is not allowed to find out.
//
// A timeout is open in the same way for the same reason: it says the connect did
// not finish, not that nothing was there.
//
// Bound is all it answers, and deliberately less than serving. The kernel queues a
// connect to a listener nobody is accepting on, so a process that binds and then
// stops calling accept answers a connect successfully until its backlog fills and
// EAGAIN after it does -- read as bound either way, and read that way before any of
// this too. What that costs is that a socket nothing serves can keep a startup out,
// and two states reach it: such a listener, and a dead socket whose mode or
// directory denies connect. Both keep the file until the process is killed or the
// file removed by hand, which is why the refusal has to name the path. There is no
// equivalent way back from having bound over a daemon that was running, and that is
// what makes this the right way round.
//
// EAGAIN is Linux's alone. XNU's unp_connect and FreeBSD's unp_connect_peer both
// answer ECONNREFUSED when the peer is not listening *or* when cloning the
// connection fails because the queue is full, and EAGAIN is in neither one's error
// set, so the errno does not tell a busy daemon from a dead socket there. One probe
// cannot either, which is where every platform was before this; what separates them
// is that the busy one stops refusing, and refusalIsStable is that second look.
// EAGAIN's own row above is then Linux saying in one syscall what darwin takes 1ms
// to say, and both end at bound.
//
// It is a probe and not a claim about the future — the answer can change as soon
// as it is given. In listen that costs little: a removal there is followed by a bind,
// so a caller that loses the race is bounded to one that had to ignore a live socket
// to get there, which is the argument listen makes in full. RemoveStale has no such
// floor. It removes the file and puts nothing in its place, and its own doc says the
// instance lock does not cover an older shelld or a shelld-rs sharing the path, so a
// wrong "not bound" there unlinks a live peer's address and leaves the path empty.
// That asymmetry is why the open answers below are answered as bound.
func isBound(path string) bool {
	b, _ := isBoundWith(path, addressHeld)
	return b != notBound
}

// probeDial is the connect isBoundWith and refusalIsStable make, a variable so a
// test can reach a timeout, which needs a connect the kernel never finishes.
var probeDial = dialUnix

// boundness is isBoundWith's verdict. It has three values because its callers
// want opposite safe defaults from an open answer -- removeDeadSocket must not
// replace the file and must not report a holder it never saw -- so collapsing
// boundUnknown into either of the others is wrong for one of them. shelld-rs's
// Bound is the same three.
type boundness int

const (
	notBound boundness = iota
	bound
	// boundUnknown is an answer that does not distinguish the two: EACCES, a
	// timeout, an errno nobody classified.
	boundUnknown
)

// isBoundWith is isBound with the address lookup injected, so a test can reach the
// answer for a host that cannot perform one -- see the !known branch, which is
// otherwise unreachable on a machine that has /proc.
//
// It also returns the error the connect failed with, or nil when the connect was
// accepted, because that errno is the only thing that distinguishes the states behind a
// "bound" answer and removeDeadSocket has to tell an operator which one it hit. The
// value is meaningless when the verdict is notBound.
func isBoundWith(path string, held func(string) (bool, bool)) (boundness, error) {
	conn, err := probeDial(context.Background(), path, 250*time.Millisecond)
	switch {
	case err == nil:
		conn.Close()
		return bound, nil
	case errors.Is(err, syscall.EAGAIN):
		return bound, err
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ENOTDIR), errors.Is(err, syscall.ENOTSOCK):
		return notBound, err
	case errors.Is(err, syscall.ECONNREFUSED):
		held, known := held(path)
		if known {
			if held {
				return bound, err
			}
			return notBound, err
		}
		// A refusal that did not survive re-probing is reported as what ended the
		// re-probing and not as the refusal, because the refusal is the one thing it
		// has just been shown not to be. Accepted reads as nil, the same as an accept
		// on the first probe; any other errno replaces it, so an operator told the
		// connect never finished is told which connect and why.
		if b, why := refusalIsStable(path); b != notBound {
			return b, why
		}
		return notBound, err
	}
	return boundUnknown, err
}

// refusalIsStable re-probes an address that refused a connect, and reports whether
// it refused every time. It is asked only where addressHeld cannot answer, and it is
// what keeps that fallback from unlinking a live daemon's socket.
//
// The two states behind ECONNREFUSED there differ in TIME and not in errno, which is
// what makes a second sample worth taking when a second question is unavailable. A
// socket whose process is gone refuses for ever. A listener whose accept queue is
// full refuses only until its owner calls accept again, which is the next thing that
// owner does. So a refusal that goes away was never absence, and one that persists is
// the state a removal is for.
//
// This does not reverse the choice isBound documents -- persistent refusal still
// reads as removable, so a SIGKILLed daemon's socket is still reclaimable on a host
// that cannot answer, 25ms later than before. It narrows the window in which a live
// daemon is mistaken for that, and on darwin, where EAGAIN does not exist and a full
// queue is reported as a refusal, that window is the only thing separating the two.
//
// Measured, because the queue looked too deep to fill by accident. A client's Close
// does not dequeue its connection from the listener's backlog -- the queued socket
// stays there until the owner accepts it -- so a dial/close loop that outruns an
// accept loop fills the queue monotonically, and both platforms' limits are reachable
// in under a second: macOS 26.4 arm64 refused the 129th connect with ECONNREFUSED,
// linux the 4097th with EAGAIN. That is how TestShutdownNeverUnlinksAReplacementUnder
// Contention was reaching this branch on darwin and removing a socket that was
// listening at the time: 0 to 20 of its 1500 replacements ended up unreachable
// because the original's later Close unlinked them.
//
// The schedule is three probes inside 25ms. The first sleep is the load-bearing one:
// draining a full queue is one accept per entry by a loop already in that syscall,
// and the two states this tells apart are 1ms and for ever, so the later probes are
// margin for a host under load rather than a different hypothesis. It is bounded
// because callers wait on it, and one of them waits holding a lock: RemoveStale takes
// the instance lock before asking, so 25ms is also the longest this adds to a
// competing start's wait for that lock. Paid once per call, and only where there is
// no /proc to ask instead.
//
// The error is the one the probe that ended the loop failed with, nil when that probe
// was accepted, and meaningless when the refusal was stable (notBound) -- the caller
// reports it instead of the original refusal, which is the state that has just been
// ruled out.
func refusalIsStable(path string) (boundness, error) {
	for _, wait := range []time.Duration{time.Millisecond, 4 * time.Millisecond, 20 * time.Millisecond} {
		time.Sleep(wait)
		conn, err := probeDial(context.Background(), path, 250*time.Millisecond)
		switch {
		case err == nil:
			conn.Close()
			return bound, nil // serving, so the first refusal was a full queue
		case errors.Is(err, syscall.EAGAIN):
			return bound, err
		case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ENOTDIR):
			return notBound, err // the file went away on its own; nothing is left to protect
		case errors.Is(err, syscall.ECONNREFUSED):
			continue
		default:
			// Any other errno leaves the question open, as it does to shelld-rs
			// (verdict_from_samples), and the caller keeps the file. Which errno it
			// was has to travel back: running out of descriptors mid-probe would
			// otherwise be reported to an operator as a live descriptor holding the
			// address, naming a holder that the probe never saw.
			return boundUnknown, err
		}
	}
	return notBound, nil
}

// Dial connects to a daemon socket within timeout and refuses one served by
// another user; see sock.Dial.
func Dial(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	return sock.Dial(ctx, addr, timeout)
}

// dialUnix connects to a unix socket within timeout, with no owner check.
func dialUnix(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	return sock.DialUnix(ctx, addr, timeout)
}
