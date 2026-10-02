//go:build linux

package daemon

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// addressHeld reports whether a live socket has path as its bound address, and
// whether that question could be answered at all.
//
// This is the distinction connect(2) cannot make. Three states answer ECONNREFUSED,
// and only the last of them may have its file removed:
//
//	a socket bound without listen()      -- every net.Listen passes through it
//	a listener after shutdown(SHUT_RDWR) -- stable until the fd is closed
//	a socket file whose process is gone  -- the state removeDeadSocket exists for
//
// /proc/net/unix lists every socket the kernel holds, and a bound one with the path
// it was bound to, so a path that appears there is held by a descriptor that still
// exists and a path that does not appear is held by nothing. Measured against those
// three states in order: present, present, absent.
//
// The comparison is lexical, on the string the peer passed to bind rather than on the
// identity of the file it names. Resolving the listed paths instead would mean calling
// stat on arbitrary paths belonging to other users, where a single unresponsive mount
// would hang a startup. What it costs is that a peer which bound the same file under a
// different spelling is not recognised, and the removal then goes ahead against a live
// address -- so the spellings that can be reconciled without touching the filesystem
// are: filepath.Clean settles the ones that are purely notation ("dir//s.sock",
// "dir/./s.sock", a trailing "/."), and a relative path bound from the same directory
// remains unrecognised, because telling that one apart needs to know a cwd this process
// does not have. A symlink cannot reach here at all: removeDeadSocket refuses anything
// Lstat does not call a socket.
//
// Cleaning both sides can only ever widen what is recognised, which is the direction that
// is safe. Clean is a function, so byte-equal inputs give byte-equal outputs: every pair
// that matched before it still matches after, and the only pairs whose answer changes are
// ones that now match and did not. So this cannot manufacture the answer that authorises a
// removal -- an address that would have read as held cannot be made to read as free -- and
// the worst it can do is refuse a startup it would previously have allowed, which is the
// recoverable half of the trade isBound already makes. (Being a function is the whole of the
// argument; Clean's idempotence is not what carries it.) One new false "held" comes with it:
// Clean resolves ".." lexically, so a peer that bound "dir/link/../s.sock" where link is a
// symlink holds a different file than the "dir/s.sock" this then compares against. That
// needs a symlink whose lexical parent is this socket's own directory, and it errs towards
// the refusal.
//
// A path containing a newline is the one spelling that errs the other way, so it is refused
// outright rather than compared -- see addressHeldIn.
func addressHeld(path string) (held, known bool) {
	f, err := os.Open("/proc/net/unix")
	if err != nil {
		return false, false
	}
	defer f.Close()
	return addressHeldIn(f, path)
}

// addressHeldIn is addressHeld over an already-open listing, so that the parsing and
// the read failure can be tested without a host that has to be missing /proc.
//
// The listing is line-oriented and sun_path is not: the kernel prints the bytes passed to
// bind verbatim, so a socket bound to a path containing a newline appears as a line that
// stops mid-path followed by a remainder that reads as an entry of its own -- measured. Two
// things follow, and they go in opposite directions. A path this is *asked* about cannot be
// found if it holds a newline, and answering "not bound" for a socket that is bound is the
// one wrong answer that authorises removing a live socket's file. In the other direction a
// peer can bind a path whose bytes spell out a plausible second line and name someone
// else's socket in it; that manufactures a false "held", which costs a refused startup and
// is the half of the trade isBound already accepts, so it is left alone rather than
// defended against by parsing the listing more cleverly than the kernel wrote it.
//
// So a queried path containing a newline answers held, and it answers it as *known*, which
// is the opposite of what an unanswerable query looks like everywhere else here. The reason
// is that "unknown" carries two facts and the caller can only act on one of them: isBound
// maps `!known` to removable, deliberately, because a host with no readable /proc must
// still be able to reclaim a killed daemon's socket. That is "this host cannot perform the
// lookup". A newline path on a host that answers for every other path is "this question
// cannot be answered", and the asymmetry this file is built on says an open question reads
// as bound -- refuse, keep the file, leave it to a human. Returning `!known` here would
// have routed it into the removable branch and replaced the live address anyway, which is
// measurably what an earlier version of this guard did.
//
// What that costs is worth naming, because it is the same cost isBound's comment weighs the
// `!known` fallback against. The answer here does not depend on anything that can change,
// so it is the answer forever: a daemon SIGKILLed on a path containing a newline leaves a
// socket file this code will never remove, and every later start on that path refuses. The
// difference from the fallback is only its size -- there, a whole host can never reclaim a
// killed daemon's socket; here, one path spelling can't. The way out is a human removing the
// file, and it is the only way out: whatHoldsIt sends the operator to `ss -xlp`, whose output
// stops at the newline in the same place this listing does -- measured, both of them printing
// a live listener's path as "<dir>/a" for a path spelled "<dir>/a\nb.sock" -- so the path they
// are told to look for is not there to find. Refusing a path shape nobody types is the cheaper
// side of that trade, and the other side is binding over a live daemon.
func addressHeldIn(r io.Reader, path string) (held, known bool) {
	want := filepath.Clean(path)
	if strings.ContainsRune(want, '\n') {
		return true, true
	}
	sc := bufio.NewScanner(r)
	sc.Scan() // The column headings.
	for sc.Scan() {
		if p := unixSocketPath(sc.Text()); p != "" && filepath.Clean(p) == want {
			return true, true
		}
	}
	// A read that stopped partway has seen some of the sockets and not the rest, so it
	// cannot report an absence. Answering "nothing is bound" from a truncated listing is
	// the one wrong answer that authorises a removal.
	if sc.Err() != nil {
		return false, false
	}
	return false, true
}

// unixSocketPath is the Path column of a /proc/net/unix line, and "" for a line that
// has none -- a socket that was never bound to a path stops at the Inode column.
//
// The path is taken as the whole remainder of the line rather than as a field,
// because a path passed to bind may contain spaces and the kernel prints it verbatim.
// One spelling is beyond recovery: a path whose first byte is a space cannot be told
// from the Inode column's own padding. An absolute path cannot begin with one, so that
// collapses into the relative-path case addressHeld already documents.
func unixSocketPath(line string) string {
	// Num RefCount Protocol Flags Type St Inode Path, and the columns are separated
	// by runs of spaces rather than single ones.
	const columnsBeforePath = 7
	for range columnsBeforePath {
		line = strings.TrimLeft(line, " ")
		space := strings.IndexByte(line, ' ')
		if space < 0 {
			return ""
		}
		line = line[space:]
	}
	return strings.TrimLeft(line, " ")
}
