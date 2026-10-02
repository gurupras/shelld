//go:build linux || darwin

package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// shortDir is a temporary directory with a short path: t.TempDir includes the
// test name and can push a socket path past sun_path's limit (108 bytes on
// Linux, 104 on macOS).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestEnsurePrivateDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "new")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", st.Mode())
	}
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatalf("existing private dir: %v", err)
	}
	open := filepath.Join(base, "open")
	os.Mkdir(open, 0o755)
	os.Chmod(open, 0o777)
	if err := EnsurePrivateDir(open); err == nil || !strings.Contains(err.Error(), "unsafe permissions") {
		t.Fatalf("0777 dir: %v", err)
	}
	if st, _ := os.Stat(open); st.Mode().Perm() != 0o777 {
		t.Fatal("an unsafe dir was chmodded")
	}
	link := filepath.Join(base, "link")
	os.Symlink(dir, link)
	if err := CheckPrivateDir(link); err == nil {
		t.Fatal("symlink accepted")
	}
	file := filepath.Join(base, "file")
	os.WriteFile(file, nil, 0o600)
	if err := EnsurePrivateDir(file); err == nil {
		t.Fatal("regular file accepted")
	}
	if err := CheckPrivateDir(filepath.Join(base, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing dir: %v", err)
	}
}

func TestDefaultPathsPerUser(t *testing.T) {
	t.Setenv("SHELLD_SOCKET", "")
	t.Setenv("SHELLD_PIDFILE", "")
	want := filepath.Join(os.TempDir(), "shelld-"+strconv.Itoa(os.Getuid()))
	if DefaultDir() != want || DefaultSocket() != filepath.Join(want, "shelld.sock") || DefaultPidfile() != filepath.Join(want, "shelld.pid") {
		t.Fatalf("defaults %s %s %s", DefaultDir(), DefaultSocket(), DefaultPidfile())
	}
	t.Setenv("SHELLD_SOCKET", "/x/y.sock")
	if DefaultSocket() != "/x/y.sock" {
		t.Fatal("SHELLD_SOCKET ignored")
	}
}

func TestSocketMode(t *testing.T) {
	for _, shared := range []bool{false, true} {
		path := filepath.Join(shortDir(t), "s.sock")
		ln, err := listen(path, ListenOptions{SharedAccess: shared})
		if err != nil {
			t.Fatal(err)
		}
		st, _ := os.Lstat(path)
		want := os.FileMode(0o600)
		if shared {
			want = 0o666
		}
		if got := st.Mode().Perm() &^ 0o111; got != want {
			t.Errorf("shared=%v: socket mode %04o, want %04o", shared, st.Mode().Perm(), want)
		}
		ln.Close()
	}
}

func TestListenReplacesOnlyDeadSockets(t *testing.T) {
	dir := shortDir(t)
	file := filepath.Join(dir, "file.sock")
	os.WriteFile(file, []byte("data"), 0o600)
	if _, err := listen(file, ListenOptions{}); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("regular file: %v", err)
	}
	if b, _ := os.ReadFile(file); string(b) != "data" {
		t.Fatal("regular file removed")
	}

	live := filepath.Join(dir, "live.sock")
	other, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := listen(live, ListenOptions{}); !errors.Is(err, ErrInstanceHeld) {
		t.Fatalf("live socket: %v", err)
	}

	dead := filepath.Join(dir, "dead.sock")
	l, err := net.Listen("unix", dead)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	ln, err := listen(dead, ListenOptions{})
	if err != nil {
		t.Fatalf("dead socket: %v", err)
	}
	ln.Close()
}

// TestListenRefusesASocketItMayNotConnectTo is what isBound's asymmetry costs, and
// the arm above is the same code getting the other answer. A socket nothing serves
// keeps a startup out when its mode denies connect, because that errno cannot tell
// a dead socket from a live one. What makes that the recoverable half of the trade
// is the refusal itself: it has to name the path and say the probe could not tell,
// so removing it by hand is the obvious next step rather than a guess. It is not
// ErrInstanceHeld (L8-L11): nothing showed a holder, and shelld-rs answers it as
// undetermined, not held.
func TestListenRefusesASocketItMayNotConnectTo(t *testing.T) {
	path := filepath.Join(shortDir(t), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	denyConnect(t, path)

	got, err := listen(path, ListenOptions{})
	if err == nil {
		got.Close()
		t.Fatal("listen replaced a socket it could not connect to: the errno it read as death says nothing about whether a listener is there")
	}
	if errors.Is(err, ErrInstanceHeld) {
		t.Errorf("listen refused with %v; want an undetermined refusal, not ErrInstanceHeld, since nothing showed a holder", err)
	}
	if want := "bind " + path + ": cannot tell whether a process is listening on it; not replacing it"; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("the refusal is %q; want it to begin %q", err, want)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("after the refusal, Lstat(%q): %v -- the file was removed anyway", path, err)
	}
}

func TestDialChecksPeer(t *testing.T) {
	path := filepath.Join(shortDir(t), "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	conn, err := Dial(t.Context(), path, 0)
	if err != nil {
		t.Fatalf("same-user socket refused: %v", err)
	}
	uid, err := peerUID(conn.(*net.UnixConn))
	conn.Close()
	if err != nil || uid != os.Getuid() {
		t.Fatalf("peer uid %d, %v", uid, err)
	}
}

// TestClosingTheListenerRemovesTheSocketFile pins where the removal happens. It
// is Close and nothing later, because Close unlinks while the descriptor is still
// bound and so removes a file this process is demonstrably still the listener for.
func TestClosingTheListenerRemovesTheSocketFile(t *testing.T) {
	path := filepath.Join(shortDir(t), "s.sock")
	ln, err := listen(path, ListenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("setup: nothing was bound at %s, so its removal proves nothing: %v", path, err)
	}

	ln.Close()

	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("closing the listener left %s behind: %v", path, err)
	}
}

// TestAServerThatBindsAfterShutdownKeepsItsSocket is the failure the removal's
// timing exists to prevent, imposed rather than raced for.
//
// A server that does not take the instance lock -- an older shelld, or shelld-rs
// sharing the socket path -- finds a socket nothing answers on and replaces it,
// exactly as listen does. If the departing daemon then unlinks the path, it
// unlinks the replacement's socket instead of its own, because unlink names a path
// and the path no longer names the same file. That leaves a server running under a
// name no client can reach, which is the one failure a client cannot tell apart
// from the daemon having exited.
func TestAServerThatBindsAfterShutdownKeepsItsSocket(t *testing.T) {
	path := filepath.Join(shortDir(t), "s.sock")
	ln, err := listen(path, ListenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	state := NewState(path)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	go func() { done <- ServeListener(ctx, ln, state) }()
	if c, err := net.Dial("unix", path); err != nil {
		t.Fatalf("setup: the daemon does not answer on %s, so there is no shutdown here to race: %v", path, err)
	} else {
		c.Close()
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}

	// The replacement binds after this daemon has stopped serving, which is the
	// only moment it could: until now the path was taken.
	other, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("setup: the replacement could not bind, so the shutdown left the path occupied: %v", err)
	}
	defer other.Close()

	// Everything the daemon does after it stops serving, to completion.
	CloseListener(ln)

	c, err := net.Dial("unix", path)
	if err != nil {
		t.Errorf("nothing reaches the server now bound to %s (%v): the departing daemon unlinked a path that had stopped naming its own socket", path, err)
		return
	}
	c.Close()
}

// TestListenRefusingALiveSocketDoesNotOfferToRemoveIt is the assertion below carried
// through the code an operator actually reaches, and it is the one that catches the
// version this replaced.
//
// whatHoldsIt can be entirely right on its own and still not be what anybody reads,
// because removeDeadSocket has to route the errno to it. The wording it replaced was a
// single fixed sentence ending in an instruction to remove the file, built without
// consulting the errno at all -- and that passes every assertion on whatHoldsIt, because
// whatHoldsIt is never called. So these two arms go through listen, against a socket that
// is live for real, and ask what came out.
//
// The arms are the two states where something answered at this path, which is what makes
// the file a live socket's address; the states where nothing answered are covered below
// and by TestAStaleBindNameKeepsADeadSocketFile, which needs the opposite.
func TestListenRefusingALiveSocketDoesNotOfferToRemoveIt(t *testing.T) {
	const advice = "remove the file"
	for _, tc := range []struct {
		name string
		path func(*testing.T, string) string
	}{
		{"a listener that accepts", func(t *testing.T, path string) string {
			return acceptingListener(t, path)
		}},
		{"a listener whose backlog is full", func(t *testing.T, path string) string {
			// May skip -- the BSDs cannot build this state; see busyListener. That is
			// why the arms are subtests: a skip here must not take the other one with
			// it.
			p, _ := busyListener(t, path)
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path(t, filepath.Join(shortDir(t), "live.sock"))
			ln, err := listen(path, ListenOptions{})
			if err == nil {
				ln.Close()
				t.Fatal("listen replaced a socket that is live, which is the failure every part of this file exists to prevent")
			}
			if strings.Contains(err.Error(), advice) {
				t.Errorf("the refusal tells the operator to %q: something answered at this path, so the file is this listener's address, and removing it lets the next start bind over a running daemon and leave it serving where no client can reach it -- %v", advice, err)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("the refusal does not name %s, which is the only place the operator is told which file this is about: %v", path, err)
			}
		})
	}
}

// TestARefusalNeverTellsAnOperatorToUnlinkALiveAddress pins the one thing the refusal's
// wording has to get right, and nothing else pinned it: for a long time every state got
// the same sentence, ending in an instruction to remove the file, and no test noticed
// either that wording or its deletion.
//
// The property is not the wording but the advice, so the sentences stay free to improve.
// It is keyed on the phrase that has to be there for the advice to have been given at all,
// and the split is the one whatHoldsIt explains: a state where something answered at this
// path proves the file is a live socket's address, and must not offer a removal; a state
// where nothing answered cannot say what the file is, and the removal is the only way
// past, offered on the condition that makes it safe.
//
// Distinctness is asserted too, because a switch collapsed to one arm would satisfy
// everything above for whichever arm it collapsed to.
//
// The arms that may remove are held to the rest of the condition as well. "remove the file"
// alone was satisfied by the wording this all replaced -- "if you know nothing serves this
// path, remove the file" -- which is a condition every state reaching here already meets, so
// the phrase was present and the gate was not. What makes it a gate is a holder the operator
// can name, so the lookup, the privilege it needs and the caveat on its blank answer are
// asserted here too.
func TestARefusalNeverTellsAnOperatorToUnlinkALiveAddress(t *testing.T) {
	const (
		advice = "remove the file"
		// The lookup the removal's condition rests on. The gate is "the holder it names
		// is not a server for this path", and an operator with no way to name the holder
		// cannot fail it.
		lookup = "ss -xlp"
		// The privilege that lookup needs, and the caveat on running it without. Linux
		// gates /proc/<pid>/fd on ptrace access, so an unprivileged ss leaves the Process
		// column empty for any holder this process is not an ancestor of -- processes of
		// its own uid included, which is exactly the shape of a daemonized shelld seen
		// from an operator's later shell. An operator who reads that blank as "nothing
		// holds it" performs the removal the refusal exists to prevent.
		//
		// Both are asserted because either alone is satisfiable by a message that
		// misleads: the command without the caveat, or a caveat about a command the
		// message never gives.
		privilege = "sudo"
		caveat    = "not an answer"
	)
	seen := map[string]string{}
	for _, tc := range []struct {
		what      string
		why       error
		mayRemove bool
	}{
		{"a listener that accepted the connect", nil, false},
		{"a listener whose backlog is full", syscall.EAGAIN, false},
		{"an address a live descriptor holds", syscall.ECONNREFUSED, true},
		{"a socket this process may not connect to", syscall.EACCES, true},
		{"a connect that never finished", os.ErrDeadlineExceeded, true},
	} {
		got := whatHoldsIt(tc.why)
		switch offers := strings.Contains(got, advice); {
		case offers && !tc.mayRemove:
			t.Errorf("%s is told to %q: something answered at this path, so the file is a live socket's address, and an operator who does that unlinks it, lets this daemon bind over it, and leaves the peer serving where no client can reach it -- %q", tc.what, advice, got)
		case !offers && tc.mayRemove:
			t.Errorf("%s is not told it can %q: nothing answered, so this cannot say the file is anybody's address, and removing it is the only way past the refusal -- %q", tc.what, advice, got)
		}
		if tc.mayRemove {
			for _, want := range []string{lookup, privilege, caveat} {
				if !strings.Contains(got, want) {
					t.Errorf("%s offers the removal without %q, so the condition it is offered on -- that the holder it names is not a server for this path -- is one the operator has no way to check: %q", tc.what, want, got)
				}
			}
		}
		if prior, dup := seen[got]; dup {
			t.Errorf("%s and %s are described identically, so the message cannot say which state a caller is in: %q", tc.what, prior, got)
		}
		seen[got] = tc.what
	}
}

// TestABusyDaemonIsNotDead is the state isBound has to get right and the one it is
// easiest to get wrong: a daemon that is bound and running, whose backlog has
// filled because it is not keeping up. connect to it fails, and a probe that reads
// any failure as death concludes the socket is stale.
//
// The arms here are the states isBound settles by errno alone: ENOENT proves nothing
// is bound, and the rest only say the connect did not get through. They are not every
// state a socket path can be in -- the three that answer ECONNREFUSED need a second
// question and are covered by TestAnAddressALiveDescriptorHoldsIsNotDead, and a
// datagram socket by TestADatagramSocketIsNotDead. Claiming completeness here is what
// left the dangerous states untested, so the two lists say what they cover instead.
//
// So the last arms are the ones that
// matter, and they are deliberately opposite. A full backlog is a daemon that is
// up, and reading it as dead unlinks a running daemon's socket. A socket whose mode
// denies connect says nothing either way -- mode 000 denies its own owner, and the
// permission check happens before the listener's state is looked at, so live and
// dead both answer EACCES -- and being unable to tell is answered as bound. The
// dead half of that is what the rule costs, pinned here so it reads as a decision
// and not a surprise.
func TestABusyDaemonIsNotDead(t *testing.T) {
	dir := shortDir(t)

	if isBound(filepath.Join(dir, "absent.sock")) {
		t.Error("a path with no file reads as served")
	}

	dead := filepath.Join(dir, "dead.sock")
	ln, err := net.Listen("unix", dead)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Lstat(dead); err != nil {
		t.Fatalf("setup: the socket file did not outlive its listener, so this arm is not the state it names: %v", err)
	}
	if isBound(dead) {
		t.Error("a socket file whose listener has gone reads as served")
	}

	busy, held := busyListener(t, filepath.Join(dir, "busy.sock"))
	if !isBound(busy) {
		t.Errorf("a listener bound with %d queued connects reads as dead; removeDeadSocket would unlink it and bind over a running daemon", held)
	}

	live := acceptingListener(t, filepath.Join(dir, "denied.sock"))
	for _, arm := range []struct{ what, path string }{
		{"a listener that accepts", live},
		{"a socket file whose listener has gone", dead},
	} {
		denyConnect(t, arm.path)
		if !isBound(arm.path) {
			t.Errorf("%s reads as dead once connect to it is denied; that errno says only that this process may not find out, so it must not authorise unlinking the file", arm.what)
		}
	}
}

// TestAnAddressALiveDescriptorHoldsIsNotDead covers the two states that answer
// ECONNREFUSED while a descriptor still holds the address. Both must read as bound:
// the caller's next move on "not bound" is to unlink the file and bind over it, which
// leaves the peer running under a name no client can reach.
//
// Neither is hypothetical. net.Listen passes through the first on every startup,
// because bind and listen are two syscalls. The second is not a race at all -- a
// listener that has been shut down holds its address until the descriptor is closed.
// Both were reachable before addressHeld existed, and the errno cannot tell either of
// them from the socket file of a process that has gone, which is the one state a
// removal is for and is pinned in TestABusyDaemonIsNotDead.
func TestAnAddressALiveDescriptorHoldsIsNotDead(t *testing.T) {
	dir := shortDir(t)
	if _, known := addressHeld(filepath.Join(dir, "probe.sock")); !known {
		t.Skipf("%s cannot say which descriptor holds an address, so the states below are indistinguishable from a dead socket file here and isBound is left where it was", runtime.GOOS)
	}

	unlistened := filepath.Join(dir, "unlistened.sock")
	boundSocket(t, unlistened, syscall.SOCK_STREAM)

	shut := filepath.Join(dir, "shut.sock")
	fd := boundSocket(t, shut, syscall.SOCK_STREAM)
	if err := syscall.Listen(fd, 1); err != nil {
		t.Fatalf("setup: listen: %v", err)
	}
	if err := syscall.Shutdown(fd, syscall.SHUT_RDWR); err != nil {
		t.Fatalf("setup: shutdown: %v", err)
	}

	// Skipped arms are counted rather than skipping the test, because a skip inside the
	// loop would abandon the arms after it and report the whole test as skipped: one
	// platform's answer for the first state would silently take the second state's
	// assertion with it.
	tested := 0
	for _, arm := range []struct{ what, path string }{
		{"a socket bound before its listen()", unlistened},
		{"a listener after shutdown(SHUT_RDWR)", shut},
	} {
		// An arm is only the state it names if the kernel refuses the connect for
		// it; otherwise it says nothing about the errno it exists for.
		c, err := net.DialTimeout("unix", arm.path, 250*time.Millisecond)
		if err == nil {
			c.Close()
			t.Errorf("setup: connect to %s was accepted, so this arm is not the state it names", arm.what)
			continue
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Logf("skipping %s: connect gave %v rather than ECONNREFUSED on %s, so this arm is not the state it names", arm.what, err, runtime.GOOS)
			continue
		}
		tested++
		if !isBound(arm.path) {
			t.Errorf("%s reads as dead: a descriptor still holds the address, so removeDeadSocket would unlink it and bind over a live peer", arm.what)
		}
	}
	if tested == 0 {
		t.Skipf("no state here answers a connect with ECONNREFUSED, so %s cannot reach the ambiguity isBound exists for", runtime.GOOS)
	}
}

// TestADatagramSocketIsNotDead pins the other direction of the same rule. A datagram
// socket bound to the path answers a stream connect with EPROTOTYPE, which says
// nothing about whether anything is bound there, so it must not authorise a removal.
// It already read as bound before any of this, and that is the point: adding
// EPROTOTYPE to the errnos that prove nothing is bound would look like completing the
// list and would unlink a live peer's address.
func TestADatagramSocketIsNotDead(t *testing.T) {
	path := filepath.Join(shortDir(t), "dgram.sock")
	boundSocket(t, path, syscall.SOCK_DGRAM)
	if !isBound(path) {
		t.Error("a bound datagram socket reads as dead; removeDeadSocket would unlink an address a live process holds")
	}
}

// TestAStaleBindNameKeepsADeadSocketFile pins what asking /proc/net/unix costs, so
// that nobody removes the cost by removing the fix.
//
// The kernel prints the string a socket was bound to for as long as the descriptor
// lives, whether or not that string still names it, so a peer that bound this path and
// unlinked it holds the NAME after the file it made is gone. A dead socket file left at
// the same path therefore reads as held, and listen refuses a file that really is dead
// -- for as long as the unrelated peer runs.
//
// It is refused rather than replaced on purpose. The comparison is on the bind string
// and cannot tell this apart from a live daemon's address (see addressHeld), and of the
// two ways to be wrong, refusing to start says so on stderr and names the file to
// remove, while binding over a running daemon leaves it serving where no client can
// reach it and says nothing at all. Removing the file is the whole way back, which is
// the second half of what this pins: nothing else has to be killed or restarted.
func TestAStaleBindNameKeepsADeadSocketFile(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "stale.sock")
	if _, known := addressHeld(path); !known {
		t.Skipf("%s cannot say which descriptor holds an address, so this state cannot arise here", runtime.GOOS)
	}

	// The peer: bound to path, listening, and then its file unlinked. Its descriptor
	// outlives the name, which is the state the whole test is about.
	peer := boundSocket(t, path, syscall.SOCK_STREAM)
	if err := syscall.Listen(peer, 1); err != nil {
		t.Fatalf("setup: listen: %v", err)
	}
	if err := syscall.Unlink(path); err != nil {
		t.Fatalf("setup: unlink %s: %v", path, err)
	}

	// The dead file: a second socket bound to the same path and closed. A raw bind
	// leaves the file behind, unlike a UnixListener, so what sits at path now is a
	// socket nothing holds.
	dead, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("setup: socket: %v", err)
	}
	if err := syscall.Bind(dead, &syscall.SockaddrUnix{Name: path}); err != nil {
		syscall.Close(dead)
		t.Fatalf("setup: bind %s: %v", path, err)
	}
	syscall.Close(dead)

	// The two halves of the state, checked rather than assumed. Without them this test
	// passes on any refusal for any reason -- including a host where the connect times
	// out, or one where the file never got created -- and the state it is named for is
	// the pair: a connect that is refused, and a name that is still held.
	if c, err := net.DialTimeout("unix", path, 250*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("setup: the dead file accepted a connect, so it is not dead and this is not the state this test names")
	} else if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Skipf("connect to the dead file gave %v rather than ECONNREFUSED, so the second question is never asked and this is not the state this test names", err)
	}
	if held, known := addressHeld(path); !known || !held {
		t.Fatalf("setup: addressHeld = (%v, %v) after a live peer bound this name and unlinked its file, want (true, true): the whole state is a name outliving the file that carried it", held, known)
	}

	_, err = listen(path, ListenOptions{})
	if !errors.Is(err, ErrInstanceHeld) {
		t.Fatalf("listen over a dead file whose name a live descriptor still holds = %v, want ErrInstanceHeld: the address cannot be shown to be free, and replacing it on a guess is how a running daemon loses its socket", err)
	}
	// This is the state where the file really is dead and the refusal is still right, so
	// it is also the state that has to tell the operator the removal below is available.
	// A message that only said to stop the process would leave no way out of a file
	// nothing serves. Which of the two it is cannot be told from here -- addressHeld
	// compares bind strings -- so saying both is the whole of what can be said.
	if !strings.Contains(err.Error(), "remove the file") {
		t.Errorf("the refusal does not say the file can be removed, which is the only way past it here: %v", err)
	}

	// And the way back, which has to keep working or the refusal above is a wedge.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
	ln, err := listen(path, ListenOptions{})
	if err != nil {
		t.Fatalf("listen after removing the file by hand = %v, want success: removing it is the only way past the refusal", err)
	}
	ln.Close()
}

// boundSocket binds a socket of type typ to path, keeps it open for the rest of the
// test, and returns its descriptor. Raw syscalls because the states above are the ones
// net gives no way to stop in: net.Listen does not pause between bind and listen, and
// a UnixListener's Close drops the address rather than shutting it down.
func boundSocket(t *testing.T, path string, typ int) int {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_UNIX, typ, 0)
	if err != nil {
		t.Fatalf("setup: socket: %v", err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		t.Fatalf("setup: bind %s: %v", path, err)
	}
	return fd
}

// acceptingListener binds path and accepts everything, so connect to it succeeds
// until something other than the listener's state stops it.
func acceptingListener(t *testing.T, path string) string {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return path
}

// denyConnect makes connect to path fail with EACCES without touching whatever is
// or is not bound to it, and skips if the kernel does not answer that way: the arm
// is about an errno, and asserting on a state the platform will not produce would
// be a red about the platform rather than about isBound. The mode goes back in
// cleanup so the enclosing directory's own cleanup can still remove the file.
func denyConnect(t *testing.T, path string) {
	t.Helper()
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, before.Mode().Perm()) })
	if _, err := net.DialTimeout("unix", path, 250*time.Millisecond); !errors.Is(err, syscall.EACCES) {
		t.Skipf("connect to a mode 000 socket gave %v, not EACCES: %s does not deny its owner, so this arm is not the state it names", err, runtime.GOOS)
	}
}

// TestStartupLeavesABusyDaemonsSocketAlone is what reading a busy daemon as dead
// costs, and it needs no race to reach: listen replaces the socket of a daemon that
// is still running, which leaves it serving under a name no client can reach. It is
// the same loss as TestAServerThatBindsAfterShutdownKeepsItsSocket, arrived at from
// the other side -- there by unlinking too late, here by replacing something live.
func TestStartupLeavesABusyDaemonsSocketAlone(t *testing.T) {
	path, held := busyListener(t, filepath.Join(shortDir(t), "s.sock"))
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := listen(path, ListenOptions{})
	if err == nil {
		defer ln.Close()
		after, statErr := os.Lstat(path)
		t.Fatalf("listen bound over a daemon that was up with %d queued connects: %s went from inode %d to %d (%v), so the running daemon is now unreachable",
			held, path, inodeOf(before), inodeOf(after), statErr)
	}
	if !errors.Is(err, ErrInstanceHeld) {
		t.Errorf("listen refused with %v; want ErrInstanceHeld, which is what tells a caller the socket is somebody's and not broken", err)
	}
	if after, err := os.Lstat(path); err != nil || inodeOf(after) != inodeOf(before) {
		t.Errorf("the refusal still changed %s: inode %d, was %d", path, inodeOf(after), inodeOf(before))
	}
}

// busyListener binds path, never accepts, and fills the backlog, so connect to it
// fails while it is demonstrably live. It returns path and how many connects the
// backlog took, and holds those connections open for the rest of the test: closing
// one frees a slot and the next connect succeeds again.
func busyListener(t *testing.T, path string) (string, int) {
	t.Helper()
	// The backlog is asked for here rather than left to net.Listen, which takes
	// somaxconn -- 4096 on this host. Filling that needs 4097 descriptors, so on a
	// machine with the common limit of 1024 the dials would stop on EMFILE, which is
	// not the state this fixture is for and would read as one. Asking for 1 keeps it
	// to a couple of connections whatever somaxconn is.
	//
	// ForkLock and CloseOnExec are what net.Listen does for the descriptors it makes:
	// a listening socket inherited by a child would keep answering on path after this
	// test closed it, which is the hazard half this package's tests are about.
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		t.Fatalf("bind %s: %v", path, err)
	}
	if err := syscall.Listen(fd, 1); err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			c.Close()
		}
	})
	// Bounded so a kernel that never refuses fails the setup instead of hanging the
	// package, and well above a backlog of 1.
	for len(held) <= 1024 {
		c, err := net.Dial("unix", path)
		if err == nil {
			held = append(held, c)
			continue
		}
		// Which errno stopped it is the whole point: only EAGAIN says the backlog is
		// full and a listener is there. Anything else -- a descriptor limit above all
		// -- is a broken fixture reporting itself as the state under test.
		if errors.Is(err, syscall.EAGAIN) {
			return path, len(held)
		}
		// Except on a kernel that does not report the state at all. The BSDs answer
		// ECONNREFUSED for a full backlog as well as for no listener, so there the
		// fixture cannot be built and isBound cannot tell a busy daemon from a dead
		// one; see its doc. The skip is narrow on purpose -- that errno on Linux is
		// still a broken fixture, and this must not become the reason a Linux
		// regression goes unreported.
		if runtime.GOOS != "linux" && errors.Is(err, syscall.ECONNREFUSED) {
			t.Skipf("connect %d to a listener that never accepts was refused rather than deferred: %s does not distinguish a full backlog from no listener, so this state is not observable here", len(held)+1, runtime.GOOS)
		}
		t.Fatalf("setup: connect %d to a listener that never accepts failed with %v, not EAGAIN, so its backlog is not full and nothing here is the state it names", len(held)+1, err)
	}
	t.Fatalf("setup: %d connects to a listener asked for a backlog of 1 all succeeded, so the backlog never filled and nothing here is the state it names", len(held))
	return "", 0
}

// inodeOf is a socket file's inode, for telling "the same file is still there"
// from "a different file is there now". It takes nil, which is not defensiveness:
// callers pair it with an os.Lstat that fails precisely when the socket has been
// removed, and that removal is the case they are looking for. A message reporting
// it evaluates its arguments whatever the condition short-circuited, so panicking
// on nil here would turn one red assertion into a crashed test binary and leave
// every test after it in the package unrun.
func inodeOf(fi os.FileInfo) uint64 {
	if fi == nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// TestShutdownNeverUnlinksAReplacementUnderContention is the same claim without
// the imposed ordering, because the imposed one can only reach the state where the
// replacement binds first. Here the replacement races the shutdown, so it also
// lands in the gaps between the shutdown's own steps -- the gaps a check performed
// before the unlink cannot close, since unlink names the path and not the file that
// was checked.
//
// The replacement decides with isBound, the predicate this daemon ships, rather
// than one spelled out here, and that is load-bearing: a replacement that reads
// every failed connect as death removes this listener's socket while it is still
// bound and serving, and the shutdown's unlink then names the replacement's socket.
// Measured at GOMAXPROCS=1, that shape loses 1 or 2 of 300 in 3 runs out of 12,
// always after a connect that returned EAGAIN with the path still present -- which
// is to say it is TestABusyDaemonIsNotDead, reached by racing for it. What is out
// of reach is a peer built before that predicate was right; unlink names a path, so
// no shutdown of ours can keep such a peer's socket safe.
//
// The loss rate is not a statistic to compare against a threshold: a single loss
// is a socket a client could not reach, so the bound is zero. What reps sets is how
// hard the window is looked for, and it has to be chosen against the smallest loss
// rate worth catching rather than against how long the test takes, because a run
// that finds nothing is indistinguishable from a shutdown that loses nothing.
// Unlinking after the descriptor is closed -- the shape this shutdown is written to
// avoid -- loses only about 0.7% of replacements, so at 300 reps that defect went
// unreported in 3 runs of 30, and at 1500 in none of 20. A correct shutdown takes
// 0.4 s a run for it, and 29 s at GOMAXPROCS=1, where every rep pays a scheduler
// round trip to overlap two goroutines on one P. That is the whole cost of the rep
// count and it is paid in the sweep rather than the suite. boundAtLeast guards the
// run from passing because the replacement never bound at all.
func TestShutdownNeverUnlinksAReplacementUnderContention(t *testing.T) {
	const reps = 1500
	const boundAtLeast = reps / 2

	bound, lost := 0, 0
	for i := 0; i < reps; i++ {
		path := filepath.Join(shortDir(t), "s.sock")
		ln, err := listen(path, ListenOptions{})
		if err != nil {
			t.Fatalf("rep %d: %v", i, err)
		}
		accepted := make(chan struct{})
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					close(accepted)
					return
				}
				c.Close()
			}
		}()

		start := make(chan struct{})
		var wg sync.WaitGroup
		var theirs net.Listener
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				// What listen does: leave a socket something is bound to alone, replace
				// one nothing is, then bind.
				if isBound(path) {
					continue
				}
				os.Remove(path)
				l, err := net.Listen("unix", path)
				if err != nil {
					continue
				}
				l.(*net.UnixListener).SetUnlinkOnClose(false)
				theirs = l
				return
			}
		}()

		close(start)
		time.Sleep(50 * time.Microsecond) // let the replacement reach its spin
		CloseListener(ln)
		wg.Wait()
		// Bounded, because the accept loop ends only when the listener closes: a
		// CloseListener that stopped closing would otherwise park rep 0 here until
		// the package timed out, reporting neither a rep count nor a bound count.
		select {
		case <-accepted:
		case <-time.After(30 * time.Second):
			t.Fatalf("rep %d: the accept loop on %s never ended, so CloseListener left the listener open", i, path)
		}

		if theirs == nil {
			continue
		}
		bound++
		if c, err := net.Dial("unix", path); err != nil {
			lost++
		} else {
			c.Close()
		}
		theirs.Close()
	}

	if bound < boundAtLeast {
		t.Fatalf("the replacement bound in only %d of %d reps, fewer than the %d this needs: it is not reaching the window, so a zero loss count here would mean nothing", bound, reps, boundAtLeast)
	}
	if lost > 0 {
		t.Errorf("%d of the %d replacements that bound ended up unreachable: the shutdown unlinked a path that had stopped naming its own socket", lost, bound)
	}
}
