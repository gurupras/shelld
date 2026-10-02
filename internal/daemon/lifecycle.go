package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// LockInfo is the lockfile (pidfile) content.
type LockInfo struct {
	Pid       int32  `json:"pid"`
	Socket    string `json:"socket"`
	Version   string `json:"version"`
	StartedAt int64  `json:"startedAt"`
	// Nonce is a random value the daemon also returns from /health. The
	// lockfile is readable only by its user, so a client that finds the
	// same nonce on both knows it reached that user's daemon, not a process
	// that took the socket or pipe name first. Empty in lockfiles from older
	// daemons.
	Nonce string `json:"nonce,omitempty"`
}

// NewNonce returns a random 128-bit hex nonce.
func NewNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ErrNonceMismatch is returned when the process answering on the socket is
// not the daemon that wrote the lockfile.
var ErrNonceMismatch = errors.New("the process answering is not the daemon that wrote the lockfile")

// ErrNoLockfile is returned when a process answers on the socket but there is
// no lockfile to verify it against. A daemon that stopped cleanly removed its
// lockfile, so this is exactly when another process may have taken the
// socket or pipe name.
var ErrNoLockfile = errors.New("there is no lockfile to verify the process answering")

// ErrLockfileUnreadable is returned when the lockfile is there and cannot be
// read. It carries no detail on purpose: which file, and why, belong to the
// caller's message, because the caller is what knows the path it asked about.
//
// It must stay a different error from ErrNoLockfile, and its sentence must not
// contain "no lockfile" anywhere. "There is no lockfile" is a statement about the
// filesystem that a reader acts on -- an operator told it goes looking for a file
// that is in fact sitting there -- and a damaged or unopenable file needs a
// different repair from a missing one.
var ErrLockfileUnreadable = errors.New("the lockfile cannot be read, so it cannot verify the process answering")

// LockRead is what reading a lockfile found, and it exists because that read has
// three outcomes rather than two. A *LockInfo can express "this file names a
// daemon" and "there is no such file"; it has no value for "the file is there and
// I could not read it", so every caller had to fold the third outcome into one of
// the first two. Folding it into absence is what let shelld report that there is
// no lockfile at a path where one exists, report "not running" without having
// looked, and act on the default socket when --pidfile named another daemon's.
//
// It is a single value with no error to discard, deliberately. `lock, _ :=
// ReadLockfile(path)` is how the fold happened at seven call sites; ReadLock
// returns one value, so there is no second result to write `_` for, and a caller
// that means to ignore unreadability has to say so in a way that reads like the
// claim it is.
type LockRead struct {
	// Path is the file this result is about, so a caller can name it in a message
	// without carrying the path alongside.
	Path string
	// Info is the lockfile's contents, nil unless the file was read.
	Info *LockInfo
	// Err is why the file could not be read. Info and Err are never both set, and
	// both are nil when the file is simply not there.
	Err error
}

// ReadLock reads the lockfile at path and reports all three of the outcomes that
// read has; see LockRead. Prefer it to ReadLockfile wherever the answer decides
// something, and keep ReadLockfile for callers that only want the contents.
func ReadLock(path string) LockRead {
	info, err := ReadLockfile(path)
	return LockRead{Path: path, Info: info, Err: err}
}

// Unreadable reports that the lockfile is there and could not be read, which is
// not absence and must not be reported as absence.
func (r LockRead) Unreadable() bool { return r.Err != nil }

// LockfileTarget is what a lockfile says about the socket a command is acting on.
type LockfileTarget int

const (
	// LockfileAbsent: there is no lockfile, so it names no socket and there is
	// nothing of its to remove.
	LockfileAbsent LockfileTarget = iota
	// LockfileAgrees: the lockfile names this socket, so it is the lockfile of the
	// daemon that a command acting on this socket is acting on.
	LockfileAgrees
	// LockfileNamesAnother: the lockfile names a different socket, so it is some
	// other daemon's and this socket being free says nothing about it.
	LockfileNamesAnother
	// LockfileUnreadable: the file is there and could not be read, so what it names
	// is unknown. This is not absence; see LockRead.
	LockfileUnreadable
)

// AimsAt reports what r says about socket, and exists because that question had
// two answers in the tree rather than one. Working out which socket to act on and
// deciding whether a lockfile may be removed are the same comparison -- does this
// file name this socket -- and each was written separately: the first compared
// Info.Socket to the socket in hand, the second never read Info.Socket at all and
// removed whatever path it was handed. One predicate is what keeps them from
// drifting apart again, since a guard added to either one covers only its own
// caller.
func (r LockRead) AimsAt(socket string) LockfileTarget {
	switch {
	case r.Err != nil:
		return LockfileUnreadable
	case r.Info == nil:
		return LockfileAbsent
	case r.Info.Socket == socket:
		return LockfileAgrees
	}
	return LockfileNamesAnother
}

// CheckNonce verifies that /health came from the daemon that wrote the lockfile
// lock describes, by comparing nonces. A lockfile without a nonce (an older Go
// daemon, or shelld-rs) verifies nothing and is accepted.
//
// An unreadable lockfile is its own answer rather than ErrNoLockfile. Both mean
// the process answering has not been shown to be the daemon, so both leave the
// caller unverified -- but only one of them is a true sentence about the file.
func CheckNonce(health *LockInfo, lock LockRead) error {
	switch {
	case lock.Err != nil:
		return ErrLockfileUnreadable
	case lock.Info == nil:
		return ErrNoLockfile
	case lock.Info.Nonce == "" || health.Nonce == lock.Info.Nonce:
		return nil
	}
	return ErrNonceMismatch
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// DefaultSocket is $SHELLD_SOCKET, else shelld.sock in DefaultDir, or on Windows
// \\.\pipe\shelld-<user SID>.
func DefaultSocket() string { return envOr("SHELLD_SOCKET", defaultSocket()) }

// DefaultPidfile is $SHELLD_PIDFILE, else shelld.pid in DefaultDir.
func DefaultPidfile() string {
	return envOr("SHELLD_PIDFILE", filepath.Join(DefaultDir(), "shelld.pid"))
}

// DefaultLogfile is $SHELLD_LOGFILE, else shelld.log in DefaultDir.
func DefaultLogfile() string {
	return envOr("SHELLD_LOGFILE", filepath.Join(DefaultDir(), "shelld.log"))
}

// WriteLockfile writes atomically via a temp file and rename.
func WriteLockfile(path string, info LockInfo) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	// Only the user may read it: it holds the nonce.
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := replaceFile(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// ReadLockfile returns nil, nil when the file does not exist.
//
// The four fields are checked for a usable value and not only for being present,
// because the two come apart and the difference is destructive. `{"pid":null,
// "socket":null,"version":null,"startedAt":null}` has every key a presence check
// asks for and unmarshals to the zero LockInfo -- pid 0, socket "" -- which is
// indistinguishable from a lockfile this never read. Every caller that decides
// whether a file may be removed then reads a pid of 0 as "no daemon is named
// here" and removes it, and a socket of "" as "this file makes no claim about
// which socket to act on", which is how a lockfile that does parse ends up
// authorising the removal of files it never named. A value that cannot be acted
// on is the third outcome of the read, not the first; see LockRead.
func ReadLockfile(path string) (*LockInfo, error) {
	data, err := readFileShared(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	for _, k := range []string{"pid", "socket", "version", "startedAt"} {
		if _, ok := raw[k]; !ok {
			return nil, fmt.Errorf("missing field `%s`", k)
		}
	}
	var info LockInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	// A pid of 0 is not a smaller pid, it is the caller's own process group as far
	// as kill(2) is concerned; negative is a process group. Neither can be what a
	// daemon wrote about itself.
	if info.Pid <= 0 {
		return nil, fmt.Errorf("field `pid` is %d, which is not a process", info.Pid)
	}
	if info.Socket == "" {
		return nil, errors.New("field `socket` is empty, so it names no socket")
	}
	if info.Version == "" {
		return nil, errors.New("field `version` is empty")
	}
	return &info, nil
}

// describeMode names what a path holds, so a refusal can say what it found rather
// than only that it refused. An operator who gave the wrong path needs to be told
// a directory was left alone, not that something was.
func describeMode(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return "a symbolic link"
	case m.IsDir():
		return "a directory"
	case m&fs.ModeSocket != 0:
		return "a socket"
	case m&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case m&fs.ModeDevice != 0:
		return "a device"
	case m.IsRegular():
		return "a regular file"
	}
	return "of type " + m.Type().String()
}

// removeRegularFile unlinks path only when path is a regular file, and reports a
// path that is already gone as success.
//
// Lstat rather than Stat, and that is the whole of the check. A lockfile is read
// with a call that follows symlinks and unlinked with one that does not, so a
// symlink at the path means the bytes that were read and the file that gets
// removed are two different files: the read follows the link to a well-formed
// lockfile, every check passes on the strength of what it found there, and the
// unlink then takes the link while the file that justified it stays. What the
// symlink stood in for is what is lost, and nothing in the outcome says so -- the
// command reports success. Refusing every path that is not a regular file covers
// that without having to reason about which of the two calls to change.
//
// An absent path is success because the caller wanted it gone and it is gone.
func removeRegularFile(path string) error {
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot tell what %s is, so not removing it: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is %s, not a regular file, so not removing it", path, describeMode(st.Mode()))
	}
	if err := removeFile(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// RemoveLockfileIfOurs removes the lockfile at path only when it names socket.
//
// Keyed on the socket rather than the pid because of what the caller knows: it has
// established that nothing serves this socket, and the pid in the file belongs to
// a process that has exited, so there is nothing left for a pid to be compared
// against. Keying it at all is what stops --pidfile from being a delete. The path
// used to be removed on the strength of having been given, so a lockfile naming
// another daemon's socket -- or any file at all that happened to parse as one --
// was removed by a command that had established only that some unrelated socket
// was free.
func RemoveLockfileIfOurs(path, socket string) error {
	lock := ReadLock(path)
	switch lock.AimsAt(socket) {
	case LockfileAbsent:
		return nil
	case LockfileUnreadable:
		return fmt.Errorf("cannot read the lockfile at %s, so not removing it: %w", path, lock.Err)
	case LockfileNamesAnother:
		return fmt.Errorf("the lockfile at %s names the socket %s, not %s, so not removing it", path, lock.Info.Socket, socket)
	}
	return removeRegularFile(path)
}

// RemoveLockfileIfPid removes the lockfile only if it still names pid, so a
// daemon never removes a lockfile a newer daemon wrote. This is the daemon's own
// exit path, where the pid is what the caller knows and the socket it was started
// on may since have been taken by the newer daemon whose file this must not touch.
func RemoveLockfileIfPid(path string, pid int) {
	if info, err := ReadLockfile(path); err == nil && info != nil && int(info.Pid) == pid {
		_ = removeRegularFile(path)
	}
}

// NotReachableError is the one failure that means no daemon is there: the
// socket could not be dialled at all. Every other failure a round trip can
// have -- a reply that is not HTTP, a connection dropped before the response,
// a body that ends mid-chunk -- happens on a socket something is holding, and a
// caller that wants to tell those apart cannot use the status code to do it:
// it stays 0 whenever the response header never parsed. Error() and Unwrap()
// are shaped so the printed message is the same "connect <addr>: <cause>" it
// has always been.
type NotReachableError struct {
	Addr string
	Err  error
}

func (e *NotReachableError) Error() string { return "connect " + e.Addr + ": " + e.Err.Error() }
func (e *NotReachableError) Unwrap() error { return e.Err }

// badInRequestLine reports a rune that cannot appear in a request-line target.
// Space ends the target, CR and LF end the line, and the remaining C0 and C1
// controls are rejected as well rather than reasoned about one at a time. Copied
// from ws.Client's guard rather than shared, because exporting it from ws would put
// a request-line detail in the API of a WebSocket package.
func badInRequestLine(r rune) bool {
	return r == ' ' || r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// HTTPRoundTrip sends a raw HTTP/1.1 request to the daemon and returns the
// status code and decoded body. A failure to dial is a *NotReachableError;
// anything else is a failure of a socket that answered.
func HTTPRoundTrip(addr, method, path string, timeout time.Duration) (int, []byte, error) {
	// The request line below is concatenated rather than built by net/http, so a
	// space or a CR LF in path would end the target or the line and turn the rest
	// into headers or a second request. Callers interpolate session and window names,
	// which shelld does not restrict, so this is checked on the value; ws.Client
	// carries the same guard over the same hazard.
	if i := strings.IndexFunc(path, badInRequestLine); i >= 0 {
		// Decoded rather than indexed, for the reason ws.Client's copy of this gives:
		// IndexFunc returns a byte offset, the predicate judges a rune, and %q on the
		// byte at that offset renders a C1's 0xC2 lead byte as 'Â'.
		r, _ := utf8.DecodeRuneInString(path[i:])
		return 0, nil, fmt.Errorf("path contains %q at byte %d; it must be URL-escaped", r, i)
	}
	conn, err := Dial(context.Background(), addr, timeout)
	if err != nil {
		return 0, nil, &NotReachableError{Addr: addr, Err: err}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	req := method + " " + path + " HTTP/1.1\r\nHost: shelld\r\nConnection: close\r\n"
	if method == http.MethodPost {
		req += "Content-Length: 0\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		return 0, nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

// Probe checks /health; any failure means the daemon is not usable.
func Probe(addr string, timeout time.Duration) (*LockInfo, error) {
	status, body, err := HTTPRoundTrip(addr, http.MethodGet, "/health", timeout)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("health status %d", status)
	}
	var h struct {
		Pid       int64  `json:"pid"`
		Socket    string `json:"socket"`
		Version   string `json:"version"`
		StartedAt int64  `json:"startedAt"`
		Nonce     string `json:"nonce"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&h); err != nil {
		return nil, fmt.Errorf("parse health body: %w", err)
	}
	return &LockInfo{Pid: int32(h.Pid), Socket: h.Socket, Version: h.Version, StartedAt: h.StartedAt, Nonce: h.Nonce}, nil
}

// IsAlive is Probe as a boolean: whether the daemon answered a request, in
// time. That is not the same question as whether one is there, and the gap is
// not a corner case -- a daemon whose backlog has filled, or that is merely
// slower than timeout, is running and fails this. So it must not be what
// decides that a socket or lockfile may be removed, which is NothingServes.
func IsAlive(addr string, timeout time.Duration) bool {
	_, err := Probe(addr, timeout)
	return err == nil
}

// TailLines returns the last n lines of text.
func TailLines(text string, n int) []string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines[max(len(lines)-n, 0):]
}
