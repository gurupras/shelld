package daemon

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLockfileNonceAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shelld.pid")
	want := LockInfo{Pid: 7, Socket: "s", Version: "v", StartedAt: 1, Nonce: NewNonce()}
	if len(want.Nonce) != 32 || want.Nonce == NewNonce() {
		t.Fatalf("nonce %q", want.Nonce)
	}
	if err := WriteLockfile(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLockfile(path)
	if err != nil || *got != want {
		t.Fatalf("read back %+v, %v", got, err)
	}
	// Split rather than folded into one condition: st is nil when the stat fails, so
	// reporting st.Mode() in the same message would panic on the one path the message
	// exists to describe.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the lockfile just written: %v", err)
	}
	if st.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("lockfile mode %v: others must not read the nonce", st.Mode())
	}
	// A lockfile from an older daemon has no nonce and still parses.
	if err := os.WriteFile(path, []byte(`{"pid":1,"socket":"s","version":"v","startedAt":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadLockfile(path); err != nil || got.Nonce != "" {
		t.Fatalf("old lockfile: %+v, %v", got, err)
	}
}

// TestReadLockfileRejectsFieldsThatArePresentButUnusable arms the gap between a
// key being there and the value being one a caller can act on. The discriminating
// input is all-null JSON: it satisfies a presence check on every one of the four
// fields and unmarshals to the zero LockInfo, so a reader that only counts keys
// hands back pid 0 and socket "" as though a daemon had written them. Both are
// then read as permission -- pid 0 as "no daemon is named here, remove the file",
// socket "" as "this file claims no socket, so act on the default one".
//
// Each case must fail for the field it names rather than for being malformed, so
// every one of them is well-formed JSON with all four keys present, differing from
// the accepted lockfile below in exactly one value.
func TestReadLockfileRejectsFieldsThatArePresentButUnusable(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct{ name, body, want string }{
		{"all null", `{"pid":null,"socket":null,"version":null,"startedAt":null}`, "`pid`"},
		{"pid zero", `{"pid":0,"socket":"s","version":"v","startedAt":1}`, "`pid`"},
		{"pid negative", `{"pid":-1,"socket":"s","version":"v","startedAt":1}`, "`pid`"},
		{"socket empty", `{"pid":1,"socket":"","version":"v","startedAt":1}`, "`socket`"},
		{"version empty", `{"pid":1,"socket":"s","version":"","startedAt":1}`, "`version`"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, "unusable.pid")
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := ReadLockfile(path)
			if err == nil {
				t.Fatalf("ReadLockfile accepted %s and returned %+v", c.body, info)
			}
			if info != nil {
				t.Errorf("ReadLockfile returned info %+v beside its error, so a caller can still act on it", info)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name the field %s that made it unusable", err, c.want)
			}
			// It must be unreadable, not absent: the whole point is that a caller does
			// not read this as "there is no lockfile here" and remove the file.
			if got := (LockRead{Path: path, Info: info, Err: err}); !got.Unreadable() {
				t.Error("an unusable lockfile does not report itself unreadable")
			}
		})
	}
	// The control: change nothing but the values, and the same reader accepts it.
	// Without this the assertions above are satisfied by a reader that rejects
	// everything.
	path := filepath.Join(dir, "good.pid")
	if err := os.WriteFile(path, []byte(`{"pid":1,"socket":"s","version":"v","startedAt":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := ReadLockfile(path); err != nil || info == nil {
		t.Fatalf("ReadLockfile rejected a usable lockfile: %+v, %v", info, err)
	}
}

// TestRemoveLockfileIfOursKeysTheUnlinkToTheSocket arms the two ways the unlink
// used to reach a file that was never this socket's to remove. Each case asserts
// survival with os.SameFile and a byte comparison rather than os.Stat, because a
// truncated file still stats.
func TestRemoveLockfileIfOursKeysTheUnlinkToTheSocket(t *testing.T) {
	const sock = "/run/mine.sock"
	survives := func(t *testing.T, path string, want []byte, before os.FileInfo) {
		t.Helper()
		after, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("%s was removed: %v", path, err)
		}
		if !os.SameFile(before, after) {
			t.Errorf("%s was replaced, so it is not the file that was there", path)
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s reads back %q (%v), want %q: existing is not surviving", path, got, err, want)
		}
	}

	// A lockfile naming another socket. Nothing about this socket being free says
	// anything about the daemon on that one, and the file parses, so every check
	// that looks at the contents passes.
	t.Run("a lockfile naming another socket", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "other.pid")
		if err := WriteLockfile(path, LockInfo{Pid: 1, Socket: "/run/theirs.sock", Version: "v", StartedAt: 1}); err != nil {
			t.Fatal(err)
		}
		want, _ := os.ReadFile(path)
		before, _ := os.Lstat(path)
		err := RemoveLockfileIfOurs(path, sock)
		if err == nil {
			t.Fatal("RemoveLockfileIfOurs removed a lockfile naming another socket, and reported success")
		}
		if !strings.Contains(err.Error(), "/run/theirs.sock") {
			t.Errorf("error %q does not name the socket the file belongs to", err)
		}
		survives(t, path, want, before)
	})

	// A symlink. The read follows it and finds a lockfile that names this very
	// socket, so the contents authorise the unlink; the unlink does not follow it,
	// so what it would take is the link and not the file that authorised it.
	t.Run("a symlink to a lockfile that does name this socket", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target.pid")
		link := filepath.Join(dir, "link.pid")
		if err := WriteLockfile(target, LockInfo{Pid: 1, Socket: sock, Version: "v", StartedAt: 1}); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("cannot create a symlink: %v", err)
		}
		want, _ := os.ReadFile(target)
		before, _ := os.Lstat(target)
		// The premise: the read really does follow the link and agree, so this case
		// gets past every check that reads the contents. Without it the refusal below
		// could be coming from a disagreement rather than from the file's type.
		if got := ReadLock(link).AimsAt(sock); got != LockfileAgrees {
			t.Fatalf("ReadLock(the symlink).AimsAt = %v, want LockfileAgrees: this case is not testing what it means to", got)
		}
		err := RemoveLockfileIfOurs(link, sock)
		if err == nil {
			t.Fatal("RemoveLockfileIfOurs removed a symlink, and reported success")
		}
		if !strings.Contains(err.Error(), "symbolic link") {
			t.Errorf("error %q does not say what it found, so an operator cannot tell why the file was left", err)
		}
		if _, err := os.Lstat(link); err != nil {
			t.Errorf("the symlink itself was removed: %v", err)
		}
		survives(t, target, want, before)
	})

	// The control. Without it every assertion above is satisfied by a function that
	// refuses everything and removes nothing.
	t.Run("a lockfile naming this socket is removed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "mine.pid")
		if err := WriteLockfile(path, LockInfo{Pid: 1, Socket: sock, Version: "v", StartedAt: 1}); err != nil {
			t.Fatal(err)
		}
		if err := RemoveLockfileIfOurs(path, sock); err != nil {
			t.Fatalf("RemoveLockfileIfOurs refused this socket's own lockfile: %v", err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("this socket's lockfile survived: %v", err)
		}
	})

	// An absent file is the job already done, and must not become a refusal: every
	// caller reaches this on a box where no daemon has run.
	t.Run("an absent lockfile is success", func(t *testing.T) {
		if err := RemoveLockfileIfOurs(filepath.Join(t.TempDir(), "absent.pid"), sock); err != nil {
			t.Errorf("RemoveLockfileIfOurs on an absent path = %v, want success", err)
		}
	})
}

func TestCheckNonce(t *testing.T) {
	lock := &LockInfo{Nonce: "abc"}
	for _, c := range []struct {
		health *LockInfo
		lock   *LockInfo
		ok     bool
	}{
		{&LockInfo{Nonce: "abc"}, lock, true},
		{&LockInfo{Nonce: "xyz"}, lock, false},
		{&LockInfo{}, lock, false},                   // an impostor that returns no nonce
		{&LockInfo{Nonce: "xyz"}, &LockInfo{}, true}, // an older daemon's lockfile
		{&LockInfo{Nonce: "xyz"}, nil, false},        // no lockfile: a squatter after a clean stop
	} {
		if err := CheckNonce(c.health, LockRead{Info: c.lock}); (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrNonceMismatch) && !errors.Is(err, ErrNoLockfile)) {
			t.Errorf("CheckNonce(%q, %+v) = %v", c.health.Nonce, c.lock, err)
		}
	}
}

// TestCheckNonceSeparatesAnUnreadableLockfileFromAnAbsentOne pins the third
// outcome of reading a lockfile. Absent and unreadable both leave the process
// answering unverified, so folding one into the other keeps every exit code
// right -- and makes shelld say there is no lockfile at a path where one is
// sitting, which sends an operator looking for a file rather than repairing the
// one that is there.
func TestCheckNonceSeparatesAnUnreadableLockfileFromAnAbsentOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shelld.pid")
	// Unparseable rather than mode 000: a suite running as root reads a mode-000
	// file, and the fixture would stop reproducing without failing.
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock := ReadLock(path)
	if lock.Err == nil {
		t.Fatalf("setup: the damaged lockfile read as %+v", lock.Info)
	}
	if !lock.Unreadable() || lock.Info != nil || lock.Path != path {
		t.Errorf("ReadLock = %+v, want unreadable, no info, and the path it was asked about", lock)
	}
	err := CheckNonce(&LockInfo{Nonce: "abc"}, lock)
	if !errors.Is(err, ErrLockfileUnreadable) {
		t.Errorf("CheckNonce = %v, want ErrLockfileUnreadable", err)
	}
	if errors.Is(err, ErrNoLockfile) || errors.Is(err, ErrNonceMismatch) {
		t.Errorf("CheckNonce = %v: nothing was compared and nothing was found missing", err)
	}
	// The sentence itself, not just the sentinel: this error is printed.
	if strings.Contains(err.Error(), "no lockfile") {
		t.Errorf("ErrLockfileUnreadable reads %q, which states the file is not there", err)
	}
}

// A failed rename leaves no <pidfile>.tmp behind.
func TestWriteLockfileRemovesTempOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shelld.pid")
	if err := os.Mkdir(path, 0o700); err != nil { // renaming a file over a directory fails
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "keep"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Untimed on purpose. On Windows replaceFile is not a bare os.Rename: it wraps one in
	// retryBusy with a five-second budget, and whether this failure looks busy to it depends
	// on whether MoveFileEx reports a directory target as ERROR_ACCESS_DENIED, which isBusy
	// matches, or ERROR_ALREADY_EXISTS, which it does not. There is no Windows host here to
	// settle that, and a bound asserting the fast answer would be the wrong instrument in both
	// directions: on Unix replaceFile is the bare rename and this is tens of microseconds
	// against any bound worth writing, so it can never fire for the reason it was added, while
	// a shared machine can stall three syscalls and an allocation long enough to fire it for a
	// reason that is not a defect. On Windows it would be worse than useless. If MoveFileEx
	// does report ERROR_ACCESS_DENIED, retryBusy spending its budget is the policy working --
	// lockfile_windows.go retries that errno *because* a held handle and an antivirus scan look
	// like it and the daemon cannot start without its lockfile, and the errno does not say
	// which. Narrowing isBusy to make a bound pass would delete the case that file exists for.
	//
	// So: if this test takes about five seconds on Windows, MoveFileEx reports ACCESS_DENIED
	// for a directory target and isBusy matches it. That is the retry policy behaving as
	// designed on an input production never supplies -- the daemon's lockfile path is a file,
	// never a directory -- and the thing to change is this fixture, not isBusy.
	//
	// The subject here is platform-independent and holds on both: the os.Remove of the temp
	// file after a failed rename.
	err := WriteLockfile(path, LockInfo{Pid: 1, Socket: "s", Version: "v", StartedAt: 1})
	if err == nil {
		t.Fatal("rename over a non-empty directory succeeded")
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file left behind: %v", err)
	}
}
