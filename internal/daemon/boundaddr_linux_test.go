//go:build linux

package daemon

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestUnixSocketPathReadsTheLastColumn covers the parsing, because addressHeld is only
// as good as it and the whole point of it is to be trusted over an errno. A line
// misread as having no path reads as nothing bound, which is the direction that
// unlinks a live peer's address.
func TestUnixSocketPathReadsTheLastColumn(t *testing.T) {
	for _, tc := range []struct{ what, line, want string }{
		{
			"the column headings, which have a Path column of their own",
			"Num       RefCount Protocol Flags    Type St Inode Path",
			"Path",
		},
		{
			"a listening socket",
			"ffff9a1b02e1c000: 00000002 00000000 00010000 0001 01 41231 /run/user/1000/shelld.sock",
			"/run/user/1000/shelld.sock",
		},
		{
			"a socket bound without listen(), whose flags are zero",
			"ffff9a1b02e1c000: 00000002 00000000 00000000 0001 01 41231 /run/user/1000/shelld.sock",
			"/run/user/1000/shelld.sock",
		},
		{
			"a socket that was never bound to a path, so the line stops at Inode",
			"ffff9a1b02e1c000: 00000003 00000000 00000000 0001 03 41232",
			"",
		},
		{
			"runs of spaces between columns rather than single ones",
			"ffff9a1b02e1c000: 00000002 00000000 00010000 0001 01    41231   /run/s.sock",
			"/run/s.sock",
		},
		{
			"a bind path containing spaces, which the kernel prints verbatim",
			"ffff9a1b02e1c000: 00000002 00000000 00010000 0001 01 41231 /tmp/a b/s.sock",
			"/tmp/a b/s.sock",
		},
		{
			"a relative bind path, which is recorded as it was given",
			"ffff9a1b02e1c000: 00000002 00000000 00010000 0001 01 41231 s.sock",
			"s.sock",
		},
		{"a truncated line", "ffff9a1b02e1c000: 00000002", ""},
		{"an empty line", "", ""},
	} {
		if got := unixSocketPath(tc.line); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.what, got, tc.want)
		}
	}
}

// TestAddressHeldSeparatesALiveAddressFromADeadFile is the claim isBound now rests on,
// against the three states that answer ECONNREFUSED identically. The dead file is the
// one that must read as free: making it unremovable would mean a daemon killed with
// SIGKILL could never bind its own socket again.
func TestAddressHeldSeparatesALiveAddressFromADeadFile(t *testing.T) {
	dir := shortDir(t)

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

	// A socket file that outlived its listener: SetUnlinkOnClose(false) is what a
	// daemon killed outright leaves behind.
	dead := filepath.Join(dir, "dead.sock")
	ln, err := net.Listen("unix", dead)
	if err != nil {
		t.Fatalf("setup: listen: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()

	live := acceptingListener(t, filepath.Join(dir, "live.sock"))

	for _, tc := range []struct {
		what string
		path string
		want bool
	}{
		{"a path with no file", filepath.Join(dir, "absent.sock"), false},
		{"a socket file whose process has gone", dead, false},
		{"a socket bound before its listen()", unlistened, true},
		{"a listener after shutdown(SHUT_RDWR)", shut, true},
		{"a listener that accepts", live, true},
	} {
		held, known := addressHeld(tc.path)
		if !known {
			t.Fatalf("%s: addressHeld could not read /proc/net/unix, so nothing below was tested", tc.what)
		}
		if held != tc.want {
			t.Errorf("%s: addressHeld = %v, want %v", tc.what, held, tc.want)
		}
	}
}

// TestAddressHeldInReconcilesNotationalSpellings covers the half of the spelling gap
// that can be closed without touching the filesystem.
//
// The kernel records the string the peer passed to bind, so a peer that bound
// "dir//s.sock" holds the address a query for "dir/s.sock" is asking about, and a
// lexical mismatch there reads as nothing bound -- the direction that unlinks a live
// address. filepath.Clean settles those. It cannot settle a relative path, because
// resolving one needs the cwd of the process that bound it, and that arm is here to
// pin the limit rather than to approve of it.
func TestAddressHeldInReconcilesNotationalSpellings(t *testing.T) {
	const header = "Num       RefCount Protocol Flags    Type St Inode Path\n"
	line := func(path string) string {
		return header + "ffff9a1b02e1c000: 00000002 00000000 00000000 0001 01 41231 " + path + "\n"
	}
	for _, tc := range []struct {
		what        string
		bound, want string
		held        bool
	}{
		{"the same spelling", "/run/u/s.sock", "/run/u/s.sock", true},
		{"a doubled separator in the listing", "/run/u//s.sock", "/run/u/s.sock", true},
		{"a doubled separator in the query", "/run/u/s.sock", "/run/u//s.sock", true},
		{"a dot component in the listing", "/run/u/./s.sock", "/run/u/s.sock", true},
		{"a dot component in the query", "/run/u/s.sock", "/run/u/./s.sock", true},
		{"a different socket in the same directory", "/run/u/other.sock", "/run/u/s.sock", false},
		{"a path that is a prefix of the query", "/run/u/s", "/run/u/s.sock", false},
		// Not a defect being pinned as correct: a relative bind is unrecognised, and
		// the caller is left where it was before any of this existed.
		{"a relative path against the absolute one it may name", "s.sock", "/run/u/s.sock", false},
	} {
		held, known := addressHeldIn(strings.NewReader(line(tc.bound)), tc.want)
		if !known {
			t.Errorf("%s: addressHeldIn could not answer from a listing it was handed", tc.what)
			continue
		}
		if held != tc.held {
			t.Errorf("%s: a socket bound as %q, queried as %q: held = %v, want %v", tc.what, tc.bound, tc.want, held, tc.held)
		}
	}
}

// TestAddressHeldInCannotReportAnAbsenceFromATruncatedRead pins the one answer a
// partial listing must not give. Having seen some of the sockets and not the rest is
// not evidence that a path is absent, and "absent" is the answer that authorises a
// removal -- so a read that fails has to come back unknown, where isBound treats it as
// the platform that cannot answer at all.
func TestAddressHeldInCannotReportAnAbsenceFromATruncatedRead(t *testing.T) {
	const header = "Num       RefCount Protocol Flags    Type St Inode Path\n"
	body := header + "ffff9a1b02e1c000: 00000002 00000000 00000000 0001 01 41231 /run/u/other.sock\n"
	r := io.MultiReader(strings.NewReader(body), errReader{})
	if held, known := addressHeldIn(r, "/run/u/s.sock"); known || held {
		t.Errorf("addressHeldIn over a listing that stopped partway = (%v, %v), want (false, false): an absence it did not finish reading is not an absence", held, known)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read stopped partway") }

// TestAddressHeldInReadsPastTheColumnHeadings pins the skipped first line, and
// TestAddressHeldInIgnoresAPathlessSocket the guard that a line with no Path column
// matches nothing. Both are asked with the one query that collides if they are dropped:
// the headings' own last column is the word Path, and filepath.Clean turns the empty
// string into ".".
func TestAddressHeldInReadsPastTheColumnHeadings(t *testing.T) {
	const header = "Num       RefCount Protocol Flags    Type St Inode Path\n"
	if held, known := addressHeldIn(strings.NewReader(header), "Path"); !known || held {
		t.Errorf("addressHeldIn over nothing but the headings = (%v, %v), want (false, true): the headings are not a socket", held, known)
	}
}

func TestAddressHeldInIgnoresAPathlessSocket(t *testing.T) {
	const header = "Num       RefCount Protocol Flags    Type St Inode Path\n"
	body := header + "ffff9a1b02e1c000: 00000003 00000000 00000000 0001 03 41232\n"
	if held, known := addressHeldIn(strings.NewReader(body), "."); !known || held {
		t.Errorf("addressHeldIn over a socket with no bound path, queried as %q = (%v, %v), want (false, true): a socket bound to no path holds no path", ".", held, known)
	}
}

// A path with a newline in it reads as held, and as answered.
//
// The kernel prints sun_path verbatim, so such a socket's entry is split across what a
// line-oriented reader sees as two lines and the path it was bound to appears in neither.
// The fixture is that split listing, built the way the kernel writes it: the query is the
// whole path and the listing holds the halves. Searching it can only come back "not found",
// which for a socket that is bound is the answer that authorises removing a live address.
//
// Asserted with the bound path present-but-split rather than absent, because a listing that
// simply lacks the path would pass on the absence alone and this has to fail a guard that is
// looking at the query.
//
// The answer is (held, known) and not (false, false), because unknown is not a way of
// declining to answer: isBoundWith maps it to removable on purpose, so returning it here
// would authorise the very removal this refuses. What that pair means at the decision is
// TestALiveSocketWhoseNameFragmentsTheListingIsNotReplaced's subject; this one only pins
// which pair comes back.
func TestAddressHeldInRefusesAPathItCannotFindInLines(t *testing.T) {
	const header = "Num       RefCount Protocol Flags    Type St Inode Path\n"
	const path = "/run/u/a\nb.sock"
	body := header + "ffff9a1b02e1c000: 00000002 00000000 00010000 0001 01 41233 " + path + "\n"
	if held, known := addressHeldIn(strings.NewReader(body), path); !known || !held {
		t.Errorf("addressHeldIn(%q) = (%v, %v), want (true, true): the socket is in the listing but split across two lines, so a search of it reports an absence -- and reporting that as unknown reaches isBound's removable branch rather than refusing", path, held, known)
	}
}

// The same fact at the decision that acts on it, over a socket the kernel really did split.
//
// addressHeldIn's return value is not what unlinks a file; isBound is, and it collapses
// "cannot answer" into "removable". So this asserts the three things an operator would
// observe -- the address reads as bound, a listen() over it is refused, and the file is
// still there afterwards -- and it does so with a descriptor actually bound to a path
// containing a newline, which is what makes /proc/net/unix fragment. A version of this
// guard that answered the query side only, by returning (false, false), passed the test
// above and replaced the live address here.
//
// Skipped rather than failed if the connect does not come back ECONNREFUSED, because that
// errno is the premise: it is the state the three-way discrimination exists for, and a
// host where a bound-but-unlistened socket answers otherwise is not the state this names.
func TestALiveSocketWhoseNameFragmentsTheListingIsNotReplaced(t *testing.T) {
	// A newline in the middle of the name, so neither fragment is the whole path: the
	// listing holds "…/a" on one line and "b.sock" on the next.
	path := filepath.Join(shortDir(t), "a\nb.sock")
	boundSocket(t, path, syscall.SOCK_STREAM)

	if c, err := net.DialTimeout("unix", path, 250*time.Millisecond); err == nil {
		c.Close()
		t.Skip("connect to a socket before its listen() was accepted, so this is not the state it names")
	} else if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Skipf("connect gave %v rather than ECONNREFUSED, so this is not the state it names", err)
	}

	if !isBound(path) {
		t.Errorf("isBound(%q) = false for a descriptor bound to that very path: the name splits /proc/net/unix into two lines, and the search that cannot find it reports the absence that authorises a removal", path)
	}

	ln, err := listen(path, ListenOptions{})
	if err == nil {
		ln.Close()
		t.Errorf("listen(%q) succeeded over an address a live descriptor holds: the peer is left bound to a name no client can reach", path)
	}

	// Refusing is only half of it. The refusal has to leave the file, because the peer's
	// clients reach it by that name.
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("after the refusal, Lstat(%q): %v -- the live socket's file was removed anyway", path, err)
	}
}

// TestIsBoundKeepsASocketItCannotAskAbout reaches the branch a machine with /proc
// cannot: the platform where nothing can say what holds an address. It must read
// ECONNREFUSED as removable there, because the alternative makes a SIGKILLed daemon's
// socket unreclaimable on every host that cannot answer -- and it must do that on the
// flag rather than on the answer, since a lookup that failed returns both.
func TestIsBoundKeepsASocketItCannotAskAbout(t *testing.T) {
	path := filepath.Join(shortDir(t), "unlistened.sock")
	boundSocket(t, path, syscall.SOCK_STREAM)
	if c, err := net.DialTimeout("unix", path, 250*time.Millisecond); err == nil {
		c.Close()
		t.Skip("connect to a socket before its listen() was accepted, so this arm is not the state it names")
	} else if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Skipf("connect gave %v rather than ECONNREFUSED, so this arm is not the state it names", err)
	}

	for _, tc := range []struct {
		what        string
		held, known bool
		want        boundness
	}{
		{"a host that cannot answer", false, false, notBound},
		{"a host that cannot answer and says held anyway", true, false, notBound},
		{"a host that answers held", true, true, bound},
		{"a host that answers not held", false, true, notBound},
	} {
		got, why := isBoundWith(path, func(string) (bool, bool) { return tc.held, tc.known })
		if got != tc.want {
			t.Errorf("%s: isBound = %v, want %v", tc.what, got, tc.want)
		}
		// The errno comes back with the answer because the refusal is phrased from
		// it, and every arm here is the same connect: a lookup that changes the
		// answer must not change what the operator is told was found.
		if !errors.Is(why, syscall.ECONNREFUSED) {
			t.Errorf("%s: isBound reported %v as the reason, want ECONNREFUSED, which is what makes removeDeadSocket name a held address rather than an undeterminable one", tc.what, why)
		}
	}
}
