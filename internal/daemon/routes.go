package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/gurupras/shelld"
	"github.com/gurupras/shelld/pty"
	"github.com/gurupras/shelld/vt"
	"github.com/gurupras/shelld/ws/wsserver"
)

// State is shared by all handlers.
type State struct {
	Mgr        *shelld.Manager
	StartedAt  time.Time
	SocketPath string
	// Nonce is returned by /health; see LockInfo.Nonce.
	Nonce string
	// Shutdown, set by Serve, stops serving for POST /shutdown so the daemon
	// removes its files on the way out. Without it, Exit terminates the
	// process.
	Shutdown func()
	Exit     func(code int)

	// InputQueueLimit and InputBudget bound the input queued for each window
	// and across windows (see shelld.WindowOpts); MaxInputBody bounds the
	// body of an input request. NewState reads the first two from
	// SHELLD_INPUT_QUEUE_LIMIT and SHELLD_INPUT_BUDGET (bytes).
	InputQueueLimit int
	InputBudget     *shelld.InputBudget
	MaxInputBody    int64

	// StallTimeoutMS is how long a queued injection may wait before the window
	// discards it rather than typing it; see shelld.InjectConfig.StallTimeoutMS,
	// which is where the behaviour and the reasons for it are documented. NewState
	// reads it from SHELLD_INJECT_STALL_TIMEOUT_MS. Nil leaves the library default
	// of five minutes and a pointer to zero waits for ever, so zero stays reachable
	// through the variable as well.
	//
	// It is configurable because the useful deadline is a property of the caller
	// rather than of the daemon: an interactive client would rather be told
	// its payload was dropped than have it typed minutes later into whatever the
	// user has since started, while a caller queueing work for a window it knows
	// will free up wants to wait.
	StallTimeoutMS *uint64

	// WSWriteTimeoutMS bounds how long one write to an attached client may block
	// before the daemon gives up on that client. NewState reads it from
	// SHELLD_WS_WRITE_TIMEOUT_MS. Nil leaves defaultWSWriteTimeout, and a pointer
	// to zero waits for ever, so the old unbounded behaviour stays reachable
	// through the variable.
	//
	// The bound is needed because a client that stops reading is not a client that
	// disconnects: the socket buffer fills, the daemon's write blocks, and it
	// blocks inside ws.Conn's write mutex, so that connection's render loop never
	// runs again. What makes it more than a leaked goroutine is that the loop's
	// deferred Attacher.Release never runs either, and an attacher still counts as
	// a voting attacher -- the window's size is min over those -- so one wedged
	// client holds every other client's grid at the wedged client's size for as
	// long as the daemon lives.
	WSWriteTimeoutMS *uint64

	// MaxAttaches bounds the attaches the daemon holds at once, observers
	// included, since an observer holds a socket and two goroutines exactly as a
	// writable attach does. NewState reads it from SHELLD_MAX_ATTACHES. Nil leaves
	// defaultMaxAttaches and a pointer to zero removes the cap; a value that is not
	// a count is ignored with a warning, so a typo leaves the default in force
	// rather than removing the bound. The same variable, default and 503 as
	// shelld-rs (attach.rs DEFAULT_MAX_ATTACHES, routes.rs attach).
	MaxAttaches *uint64
	// attaches is how many attach slots are held; see acquireAttach.
	attaches atomic.Uint64
	// upgraded is the attaches a shutdown has to reach itself; see drain.
	upgraded upgraded

	// DefaultCwd is the working directory for a window whose create request
	// names no cwd. cmd/shelld sets it to the directory the daemon was started
	// in, so a window inherits that the way tmux's does, rather than starting in
	// $HOME. It is used exactly as a cwd in the request would be, so a
	// DefaultCwd that no longer exists fails the create with the same 400 as a
	// bad cwd: a window that silently ran somewhere else is the outcome
	// shelld.WindowOpts refuses for an explicit cwd, and the default should not
	// be looser. Empty leaves WindowOpts's own default ($HOME) in place, which
	// is what a Manager used as a library gets.
	DefaultCwd string

	// DefaultShell is the shell command line for a window whose create request
	// names no shell, set by cmd/shelld from --shell. It is used exactly as a
	// shell in the request would be -- split on whitespace by
	// shelld.splitCommandLine, so it may carry arguments -- and it is not
	// validated here, because the create already reports a shell that cannot be
	// spawned and validating it twice would report the daemon's flag as the
	// window's error. Empty leaves shelld.DefaultShell() in place, which is what
	// a Manager used as a library gets.
	DefaultShell string

	release *releaser
}

// defaultMaxInputBody leaves room for a full input queue's text JSON-escaped.
const defaultMaxInputBody = 64 << 20

func NewState(socketPath string) *State {
	st := &State{Mgr: shelld.NewManager(), StartedAt: time.Now(), SocketPath: socketPath, Exit: os.Exit, release: newReleaser(), MaxInputBody: defaultMaxInputBody}
	st.InputQueueLimit = int(envBytes("SHELLD_INPUT_QUEUE_LIMIT", shelld.DefaultInputQueueLimit))
	st.InputBudget = shelld.NewInputBudget(envBytes("SHELLD_INPUT_BUDGET", shelld.DefaultInputBudget))
	st.StallTimeoutMS = envUint("SHELLD_INJECT_STALL_TIMEOUT_MS")
	st.WSWriteTimeoutMS = envUint("SHELLD_WS_WRITE_TIMEOUT_MS")
	st.MaxAttaches = envUint("SHELLD_MAX_ATTACHES")
	return st
}

// defaultWSWriteTimeout is how long a write to an attached client may block. It
// matches the ten seconds serveAttach already allows a client to answer a close
// frame: both are "this peer has stopped participating", and a client that has
// not drained a frame in ten seconds is not one that is about to.
const defaultWSWriteTimeout = 10 * time.Second

// maxWSWriteTimeoutMS is the largest SHELLD_WS_WRITE_TIMEOUT_MS that survives the
// multiply into a Duration. It repeats the shelld package's bound rather than
// sharing it: the clamp there is unexported, and a millisecond ceiling is not
// worth widening a library's API for.
const maxWSWriteTimeoutMS = uint64(math.MaxInt64) / uint64(time.Millisecond)

// wsWriteTimeout clamps instead of wrapping, which matters more here than the
// arithmetic suggests. The value arrives from the environment through envUint,
// which bounds it only at 64 bits, so a mistyped extra digit reaches this
// multiply -- and the two wrapped results are wrong in opposite directions. Just
// past the bound wraps negative, and ws.Conn applies the timeout under
// `writeTimeout > 0`, so a negative is no deadline at all: the wedge described on
// WSWriteTimeoutMS comes back, and with it one unread client holding every other
// client's grid at its size. 18446744073710 instead wraps to 448.384µs, which
// drops every attached client on its first write that is not instant. Both come
// from an input a caller would read as the same request, so neither can be left
// to chance; zero stays the only way to ask for no deadline.
// defaultMaxAttaches is shelld-rs's figure. It is not a tuned one: what matters
// is that a bound exists on the descriptors and goroutines a client can make the
// daemon hold, and 256 is far above the attaches any real client opens.
const defaultMaxAttaches = 256

// maxAttaches is the attach cap in force, and whether there is one at all.
func (s *State) maxAttaches() (limit uint64, capped bool) {
	if s.MaxAttaches == nil {
		return defaultMaxAttaches, true
	}
	return *s.MaxAttaches, *s.MaxAttaches != 0
}

// acquireAttach takes an attach slot, or reports that the daemon is at its cap.
// A compare-and-swap rather than an add and a rollback, so a count that is about
// to be refused is never published for a concurrent attach to be refused by.
func (s *State) acquireAttach() bool {
	limit, capped := s.maxAttaches()
	for {
		n := s.attaches.Load()
		if capped && n >= limit {
			return false
		}
		if s.attaches.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// releaseAttach gives back a slot acquireAttach took.
func (s *State) releaseAttach() { s.attaches.Add(^uint64(0)) }

func (s *State) wsWriteTimeout() time.Duration {
	if s.WSWriteTimeoutMS == nil {
		return defaultWSWriteTimeout
	}
	if *s.WSWriteTimeoutMS > maxWSWriteTimeoutMS {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(*s.WSWriteTimeoutMS) * time.Millisecond
}

// envUint reads a non-negative count from the environment, returning nil when the
// variable is unset or unusable so the caller's own default stands. Zero is a
// value rather than an absence here — it is how a caller asks for no deadline at
// all — which is why this returns a pointer where envBytes returns a number.
func envUint(name string) *uint64 {
	v := os.Getenv(name)
	if v == "" {
		return nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		slog.Warn("ignoring invalid count", "variable", name, "value", v)
		return nil
	}
	return &n
}

// envBytes reads a positive byte count from the environment, or def.
func envBytes(name string, def int64) int64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		slog.Warn("ignoring invalid byte count", "variable", name, "value", v)
		return def
	}
	return n
}

// maxControlBody bounds the body of a route that configures something -- creating
// a session or a window, resizing, setting colors, moving a window -- as opposed
// to an input route, which carries a program's keystrokes and is bounded by
// MaxInputBody instead.
//
// None of those bodies has a field that is large by nature. The only one with any
// claim to size is a new window's env, and two independent limits land on the same
// number for it: the kernel will not exec a process whose environment and
// arguments exceed ARG_MAX, which is this value on Linux, so a larger body cannot
// describe a window that could be started; and the reference daemon leaves axum's
// DefaultBodyLimit at its default, which is also this value, so it already refuses
// more than this on every route. Picking it needs no judgement about what a caller
// might reasonably send.
const maxControlBody = 2 << 20

// inputBody reads an input route's JSON body, at most MaxInputBody bytes.
func (s *State) inputBody(w http.ResponseWriter, r *http.Request) (b body, ok, handled bool) {
	return readBounded(w, r, s.inputLimit())
}

// inputBodyInto is inputBody for a route that decodes into a typed struct; see
// decodeInto for when that is worth doing and what it has to preserve.
func (s *State) inputBodyInto(w http.ResponseWriter, r *http.Request, v any) (ok, handled bool) {
	return readBoundedInto(w, r, s.inputLimit(), v)
}

func (s *State) inputLimit() int64 {
	if s.MaxInputBody <= 0 {
		return defaultMaxInputBody
	}
	return s.MaxInputBody
}

// controlBody reads a configuration route's JSON body, at most maxControlBody
// bytes.
func controlBody(w http.ResponseWriter, r *http.Request) (b body, ok, handled bool) {
	return readBounded(w, r, maxControlBody)
}

// readBounded reads a JSON body of at most limit bytes. A larger body is answered
// with 413 here and handled is true, so a caller that ignored handled would write
// its own status after this one. Every route reads its body through one of the
// wrappers above (inputBody, inputBodyInto, controlBody): an unbounded read lets a
// local client make the daemon allocate whatever it sends, and there is no route
// whose body has to be unbounded.
//
// 413 rather than the 400 the reference answers. That 400 is not a decision there
// but a consequence of extracting bodies as Option<Json<T>>, which turns the size
// rejection into a None indistinguishable from a missing field -- the same
// mechanism described at decodeBody. This daemon has answered 413 with this message
// on its input routes since they were written, so 413 is already its answer to a
// body that is too large, and the routes below were the ones out of step.
func readBounded(w http.ResponseWriter, r *http.Request, limit int64) (b body, ok, handled bool) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	b, err := decodeBody(r)
	if bodyTooLarge(w, err) {
		return nil, false, true
	}
	if bodyUnfaithful(w, err) {
		return nil, false, true
	}
	return b, err == nil, false
}

// readBoundedInto is readBounded for a route that decodes into a typed struct
// instead of a body map; see decodeInto for when that is worth doing and what it
// has to preserve. The 413 and the "any other error is the route's own 400"
// mapping are the same, so a converted route answers what it answered before.
func readBoundedInto(w http.ResponseWriter, r *http.Request, limit int64, v any) (ok, handled bool) {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	err := decodeInto(r, v)
	if bodyTooLarge(w, err) {
		return false, true
	}
	if bodyUnfaithful(w, err) {
		return false, true
	}
	return err == nil, false
}

// bodyUnfaithful answers a body carrying a string encoding/json would rewrite
// with 400 and reports that it wrote a response, so the caller stops.
//
// It carries its own messages rather than falling through to the route's generic
// one because the fault is not something a caller can see in its own request: the
// body is well-formed JSON with every field present and the right type, and the
// only thing wrong with it is a string that cannot survive being one. A route
// answering "invalid request" would send a caller looking for a missing field.
//
// The two faults get their own text because they have different causes and
// different fixes, and the response is the only place a client learns which it
// hit; see unfaithful. The surrogate message names the JavaScript slice that
// produces one, because that is how a client sending ordinary text reaches this
// at all.
//
// 400 matches the reference, which refuses the same bodies through serde_json --
// a raw 0x80 as "invalid unicode code point", a lone \ud800 as "unexpected end of
// hex escape". The text differs; the status does not.
func bodyUnfaithful(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, errNotUTF8):
		writeError(w, http.StatusBadRequest, "request body contains a byte that is not valid UTF-8: a JSON string cannot carry it, and it would be delivered as U+FFFD")
	case errors.Is(err, errLoneSurrogate):
		writeError(w, http.StatusBadRequest, "request body contains a \\u escape naming half a surrogate pair: a JSON string cannot carry it, and it would be delivered as U+FFFD. Slicing a JavaScript string at a fixed length cuts an emoji in half and produces one; slice by code point instead")
	default:
		return false
	}
	return true
}

// bodyTooLarge answers a body over the read limit with 413 and reports that it
// wrote a response. See readBounded for why 413 and not the reference's 400.
func bodyTooLarge(w http.ResponseWriter, err error) bool {
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		return false
	}
	writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
	return true
}

// writeInputError answers a failed input call: 413 when the write is larger than
// the whole input queue or the shared budget, 429 when the queue is merely full
// right now, 500 otherwise (a dead window).
//
// The order of the two errors.Is calls is the whole of this function, because
// ErrInputTooLarge WRAPS ErrInputQueueFull (see input.go) so a too-large write
// satisfies both. Testing ErrInputQueueFull first therefore answers 429 for both
// and that is the bug this ordering fixes: 429 tells a client the condition is
// transient and invites a retry, and for a write no state of the queue would ever
// accept there is nothing to wait for — the client retries for ever and the daemon
// refuses for ever. Reversing this pair is not a cosmetic change of status code, it
// reintroduces a retry loop with no exit.
//
// 413 rather than 400 because the request is well-formed and the fault is its size,
// which is also why bodyTooLarge answers 413 for a body over the read limit. A
// client that needs to tell the two apart reads the message; they are different
// limits — the read limit bounds one request, this bounds what a window has room to
// type — but the corrective action is the same one, send less.
func writeInputError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, shelld.ErrInputTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, shelld.ErrInputQueueFull):
		writeError(w, http.StatusTooManyRequests, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// Handler builds the HTTP API.
func Handler(s *State) http.Handler {
	rt := &router{}
	rt.handle("GET", "/health", s.health)
	rt.handle("POST", "/shutdown", s.shutdown)
	rt.handle("GET", "/sessions", s.listSessions)
	rt.handle("POST", "/sessions", s.createSession)
	rt.handle("GET", "/sessions/:session", s.getSession)
	rt.handle("DELETE", "/sessions/:session", s.killSession)
	rt.handle("GET", "/sessions/:session/windows", s.listWindows)
	rt.handle("POST", "/sessions/:session/windows", s.createWindow)
	rt.handle("DELETE", "/sessions/:session/windows/:window", s.killWindow)
	rt.handle("POST", "/sessions/:session/windows/:window/keys", s.sendKeys)
	rt.handle("POST", "/sessions/:session/windows/:window/raw-key", s.sendRawKey)
	rt.handle("POST", "/sessions/:session/windows/:window/key-names", s.sendKeyNames)
	rt.handle("POST", "/sessions/:session/windows/:window/paste", s.paste)
	rt.handle("GET", "/sessions/:session/windows/:window/capture", s.capture)
	rt.handle("GET", "/sessions/:session/windows/:window/scroll", s.scroll)
	rt.handle("POST", "/sessions/:session/windows/:window/resize", s.resize)
	rt.handle("POST", "/sessions/:session/windows/:window/colors", s.setColors)
	rt.handle("GET", "/sessions/:session/windows/:window/attach", s.attach)
	rt.handle("GET", "/sessions/:session/windows/:window/frame", s.frame)
	rt.handle("POST", "/sessions/:session/windows/:window/inject", s.inject)
	rt.handle("GET", "/sessions/:session/windows/:window/inject/queue", s.injectQueue)
	rt.handle("POST", "/move-window", s.moveWindow)
	return rt
}

func (s *State) health(w http.ResponseWriter, _ *http.Request, _ params) {
	sessions := s.Mgr.ListSessions()
	windows := 0
	for _, sess := range sessions {
		windows += len(sess.ListWindows())
	}
	// Process-wide counters, monotonic since start, so a caller differences them
	// across a window of interest. reflowCells is cells and not bytes on purpose:
	// multiply by reflowCellBytes if you want bytes, and read the product as the
	// size of the grids produced rather than as memory retained — a reflow that
	// reuses capacity allocates nothing. Reported per daemon, not per window: the
	// question they answer ("did the two arms of this scenario really do the same
	// amount of reflowing?") is about the run, and a per-window breakdown would
	// have to be reconciled with windows that have since been deleted.
	reflows, cells := shelld.ReflowStats()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"pid":             os.Getpid(),
		"socket":          s.SocketPath,
		"version":         Version,
		"startedAt":       s.StartedAt.Unix(),
		"nonce":           s.Nonce,
		"uptime":          int64(time.Since(s.StartedAt).Seconds()),
		"sessions":        len(sessions),
		"windows":         windows,
		"reflows":         reflows,
		"reflowCells":     cells,
		"reflowCellBytes": shelld.ReflowCellBytes,
	})
}

func (s *State) shutdown(w http.ResponseWriter, _ *http.Request, _ params) {
	go func() {
		time.Sleep(100 * time.Millisecond)
		if s.Shutdown != nil {
			s.Shutdown()
			return
		}
		s.Exit(0)
	}()
	writeJSON(w, http.StatusOK, okBody)
}

func (s *State) listSessions(w http.ResponseWriter, _ *http.Request, _ params) {
	sessions := s.Mgr.ListSessions()
	arr := make([]map[string]any, 0, len(sessions))
	for _, sess := range sessions {
		arr = append(arr, map[string]any{"name": sess.Name, "windows": sess.WindowNames()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": arr})
}

// checkName answers 400 when a name this API is about to create holds a
// control character, and reports whether name passed. A control character in
// a name forges the output of whatever lists it: a session named "x\r\ny"
// prints as two lines in `shelld sessions`, a tab in a window name invents a
// column in `shelld windows`. The test is Unicode's (category Cc), so it also
// covers C1, where U+009B is CSI. tmux refuses the same names at creation.
// '?', '#', '/' and space are allowed, as tmux allows them: a client that
// percent-encodes its path reaches them. The character is named by codepoint
// and never echoed, since the error body would carry it out again. shelld-rs's
// check_name is the same rule with the same message.
func checkName(w http.ResponseWriter, field, name string) bool {
	for _, c := range name {
		if unicode.IsControl(c) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must not contain a control character, and it contains U+%04X", field, c))
			return false
		}
	}
	return true
}

func (s *State) createSession(w http.ResponseWriter, r *http.Request, _ params) {
	b, ok, handled := controlBody(w, r)
	if handled {
		return
	}
	name, valid := b.str("name", true)
	if !ok || !valid || name == "" {
		writeError(w, http.StatusBadRequest, "name required")
		return
	}
	if !checkName(w, "name", name) {
		return
	}
	if err := s.Mgr.CreateSession(name); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, okBody)
}

func (s *State) getSession(w http.ResponseWriter, _ *http.Request, p params) {
	if s.Mgr.SessionExists(p["session"]) {
		writeJSON(w, http.StatusOK, map[string]any{"exists": true})
	} else {
		writeJSON(w, http.StatusNotFound, map[string]any{"exists": false})
	}
}

func (s *State) killSession(w http.ResponseWriter, _ *http.Request, p params) {
	s.Mgr.KillSession(p["session"])
	s.release.schedule()
	writeJSON(w, http.StatusOK, okBody)
}

func (s *State) session(w http.ResponseWriter, name string) *shelld.Session {
	sess := s.Mgr.Session(name)
	if sess == nil {
		writeError(w, http.StatusNotFound, "session not found")
	}
	return sess
}

func (s *State) window(w http.ResponseWriter, p params) *shelld.Window {
	sess := s.session(w, p["session"])
	if sess == nil {
		return nil
	}
	win := sess.Window(p["window"])
	if win == nil {
		writeError(w, http.StatusNotFound, "window not found")
	}
	return win
}

func (s *State) listWindows(w http.ResponseWriter, _ *http.Request, p params) {
	sess := s.session(w, p["session"])
	if sess == nil {
		return
	}
	wins := sess.ListWindows()
	arr := make([]map[string]any, 0, len(wins))
	for _, win := range wins {
		m := win.KeyModes()
		// One snapshot, not two reads: Liveness takes a single RLock over both
		// fields, so the pair a client receives is a state the window was
		// actually in. Both are published as they are — see exitStatusJSON for
		// why every combination of them is legitimate.
		alive, st := livenessJSON(win)
		cols, rows := win.Size()
		arr = append(arr, map[string]any{
			"name":       win.Name(),
			"pid":        win.Pid(),
			"alive":      alive,
			"exitStatus": st,
			"cols":       cols,
			"rows":       rows,
			"inputModes": map[string]bool{
				"appCursor":      m.AppCursor,
				"appKeypad":      m.AppKeypad,
				"bracketedPaste": m.BracketedPaste,
			},
			"pasteMode": win.PasteMode(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"windows": arr})
}

// livenessJSON is the (alive, exitStatus) pair the listing publishes for one
// window, read as a single snapshot so a client never sees a combination the
// window was not in.
//
// Both are reported as they are, and no combination of them is suppressed,
// because they are facts about different things: alive is the terminal, and is
// true while ANY process holds it open; exitStatus is the window's own process.
// All four combinations are legitimate and all four are reachable.
//
//   - alive, no status — running, the ordinary case.
//   - not alive, status — exited and reaped, the ordinary case.
//   - not alive, no status — exited, status not in yet. See exitStatusJSON.
//   - alive, status — the process exited and something it left behind holds the
//     terminal open. Measured on this box (TestHolderKeepsTerminalOpenAfterExit):
//     with `(trap ” HUP; sleep 5) &` before the shell's exit, alive stayed true
//     for 4.988s after the status was recorded, 246 writes were accepted in that
//     span, and the holder's output reached the grid a second after the exit.
//     Bounded by the holder, not by any race.
//
// That last one is Linux only, and the reason is the kernel's rather than this
// daemon's: BSD revokes a controlling terminal when its controlling process
// exits, which invalidates every descriptor for the tty including the holder's.
// So on macOS the master sees EOF the moment the shell exits and alive goes
// false immediately, however many processes still have the slave open. Measured
// with the same script as the case above — ignore the hangup, fork a 3s holder,
// exit — reading the master
// until it ended: linux 3.001s, macOS 26.4 arm64 14ms. A caller that treats
// alive as "a background job is still running" is reading a guarantee that only
// Linux gives; exitStatus means the same thing on both.
//
// WHAT A SUPERVISOR SHOULD READ: exitStatus, not alive. It is the unambiguous
// "the process ended, and here is how" signal — waitLoop writes it once and
// never clears it, including when p.Wait() itself errors, so a poll for it
// always terminates. alive answers a different question ("can I still write to
// this terminal, and can more output still arrive") and a supervisor that waits
// on it will keep waiting for as long as a stray background process lives.
func livenessJSON(win *shelld.Window) (bool, map[string]any) {
	alive, st, ok := win.Liveness()
	return alive, exitStatusJSON(st, ok)
}

// exitStatusJSON is how a window's program ended, or nil before it has been
// reaped. alive alone cannot answer the question a supervisor asks — a clean
// exit, a crash and a kill are all alive=false — so this reports the code, and
// the signal name when a signal ended it. Code is -1 in that case, as
// pty.ExitStatus documents; on Windows there is no signal and code is the
// process exit code.
//
// "before it has been reaped" and not "while it is still running": the status is
// recorded by waitLoop after p.Wait() returns, which is later than the process
// actually ending, and later or earlier than readLoop clearing alive at PTY EOF
// depending on the run. Measured here over 200 short-lived windows: alive=false
// arrived first in 155 of them, by a median of 109µs and a worst case of 6ms.
// So a client can be told alive=false with no status yet, and must read that as
// "exited, status not in yet" and poll again rather than as "exited, status
// unknowable".
func exitStatusJSON(st shelld.ExitStatus, ok bool) map[string]any {
	if !ok {
		return nil
	}
	out := map[string]any{"code": st.Code}
	if st.Signal != "" {
		out["signal"] = st.Signal
	}
	return out
}

// createWindow takes {"name", "cwd"?, "shell"?, "cols"?, "rows"?, "env"?,
// "unsetEnv"?, "pasteMode"?}; pasteMode has the name the window listing reports
// it under. A name already in use is 409, checked before a cwd that is not a
// directory (400; tmux and shelld-rs start in $HOME instead).
//
// An absent cwd means DefaultCwd, the directory the daemon was started in, and
// an absent shell means DefaultShell, the daemon's --shell.
func (s *State) createWindow(w http.ResponseWriter, r *http.Request, p params) {
	sess := s.session(w, p["session"])
	if sess == nil {
		return
	}
	b, ok, handled := controlBody(w, r)
	if handled {
		return
	}
	name, v1 := b.str("name", true)
	cwd, v2 := b.str("cwd", false)
	shell, v3 := b.str("shell", false)
	cols, v4 := b.uint("cols", false, 0xffff)
	rows, v5 := b.uint("rows", false, 0xffff)
	pasteMode, _, v6 := b.optStr("pasteMode")
	// env stays loose (parseEnv ignores what it cannot read); unsetEnv is
	// strict, because a name it silently dropped is a variable the caller asked
	// to have removed and would still be there.
	unsetEnv, v7 := b.strs("unsetEnv")
	if !ok || !v1 || !v2 || !v3 || !v4 || !v5 || name == "" {
		writeError(w, http.StatusBadRequest, "name required")
		return
	}
	if !checkName(w, "name", name) {
		return
	}
	if !v6 || (pasteMode != "" && !shelld.ValidPasteMode(pasteMode)) {
		writeError(w, http.StatusBadRequest, "invalid pasteMode")
		return
	}
	if !v7 {
		writeError(w, http.StatusBadRequest, "invalid unsetEnv")
		return
	}
	if err := shelld.CheckSize(uint16(cols), uint16(rows)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if cwd == "" {
		cwd = s.DefaultCwd
	}
	if shell == "" {
		shell = s.DefaultShell
	}
	win, err := sess.CreateWindow(shelld.WindowOpts{
		Name:     name,
		Cwd:      cwd,
		Shell:    shell,
		Cols:     uint16(cols),
		Rows:     uint16(rows),
		Env:      parseEnv(b["env"]),
		UnsetEnv: unsetEnv,

		InputQueueLimit: s.InputQueueLimit,
		InputBudget:     s.InputBudget,
		StallTimeoutMS:  s.StallTimeoutMS,

		PasteMode: pasteMode,
	})
	if err != nil {
		msg := err.Error()
		if errors.Is(err, shelld.ErrSessionKilled) {
			msg = "session not found"
		}
		writeError(w, createWindowStatus(err), msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pid": win.Pid()})
}

// createWindowStatus is the status for a failed window create, split by whose
// fault the failure is:
//
//	name in use          409  a retry under another name works
//	cwd invalid          400  the request is wrong; no retry fixes it
//	shell cannot be run  400  likewise
//	quote left open      400  likewise: the shell line cannot be split
//	anything else        500  the host's: openpty, the cgroup, the fork
//
// This used to be 409 for everything but a cwd, which told a client that
// retries conflicts to retry a missing shell forever, and told one that treats
// 409 as "already live, attach" to attach to a window that was never made.
// shelld-rs answers the same split (map_create_err in its routes.rs).
// Unclassified lands on 500 because that is the recoverable side: a bad request
// reported as 500 costs a retry, a host failure reported as 400 tells the
// caller to give up on a request that would have worked.
func createWindowStatus(err error) int {
	var nameErr *shelld.NameInUseError
	var cwdErr *shelld.CwdError
	var progErr *pty.ProgramError
	var lineErr *shelld.CommandLineError
	switch {
	case errors.As(err, &nameErr):
		return http.StatusConflict
	case errors.Is(err, shelld.ErrSessionKilled):
		// Deleted while the window spawned: answer what the lookup would now.
		return http.StatusNotFound
	case errors.As(err, &cwdErr), errors.As(err, &progErr), errors.As(err, &lineErr),
		errors.Is(err, shelld.ErrEmptyCommand):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// parseEnv accepts ["KEY=val", ...] or {"KEY": "val", ...}; other entries
// are ignored.
func parseEnv(raw []byte) [][2]string {
	var out [][2]string
	var arr []any
	if unmarshalLoose(raw, &arr) == nil {
		for _, e := range arr {
			if s, ok := e.(string); ok {
				if k, v, found := strings.Cut(s, "="); found {
					out = append(out, [2]string{k, v})
				}
			}
		}
		return out
	}
	// An object applies in key order (serde_json's map is a BTreeMap).
	var obj map[string]any
	if unmarshalLoose(raw, &obj) == nil {
		for _, k := range slices.Sorted(maps.Keys(obj)) {
			if s, ok := obj[k].(string); ok {
				out = append(out, [2]string{k, s})
			}
		}
	}
	return out
}

func unmarshalLoose(raw []byte, v any) error {
	if len(raw) == 0 {
		return errors.New("absent")
	}
	return json.Unmarshal(raw, v)
}

// stringMap decodes a defaulted HashMap<String, String>: absent is empty,
// anything but an object of strings is invalid.
func stringMap(raw json.RawMessage) (map[string]string, bool) {
	if raw == nil {
		return nil, true
	}
	if isNull(raw) {
		return nil, false
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	return m, true
}

func (s *State) killWindow(w http.ResponseWriter, _ *http.Request, p params) {
	fmt.Fprintf(os.Stderr, "[shelld] DELETE window %s/%s\n", p["session"], p["window"])
	sess := s.session(w, p["session"])
	if sess == nil {
		return
	}
	sess.KillWindow(p["window"])
	s.release.schedule()
	writeJSON(w, http.StatusOK, okBody)
}

func (s *State) sendKeys(w http.ResponseWriter, r *http.Request, p params) {
	win := s.window(w, p)
	if win == nil {
		return
	}
	b, ok, handled := s.inputBody(w, r)
	if handled {
		return
	}
	keys, v1 := b.str("keys", true)
	enter, enterSet, v2 := b.optBool("enter")
	delay, delaySet, v3 := b.optUint("enter_delay_ms", ^uint64(0))
	if !ok || !v1 || !v2 || !v3 {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	// Refused rather than clamped, as shelld-rs refuses it too: a clamp answers
	// 200 for a delay it did not perform, and the client cannot tell.
	if maxMS := uint64(shelld.MaxInputGap / time.Millisecond); delaySet && delay > maxMS {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("enter_delay_ms must be at most %d", maxMS))
		return
	}
	if !enterSet {
		enter = true
	}
	var delayPtr *uint64
	if delaySet {
		delayPtr = &delay
	}
	if err := win.SendKeys(keys, enter, delayPtr); err != nil {
		writeInputError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, okBody)
}

func (s *State) sendRawKey(w http.ResponseWriter, r *http.Request, p params) {
	win := s.window(w, p)
	if win == nil {
		return
	}
	b, ok, handled := s.inputBody(w, r)
	if handled {
		return
	}
	key, valid := b.str("key", true)
	if !ok || !valid {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := win.SendRawKey(key); err != nil {
		writeInputError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, okBody)
}

// sendKeyNames sends tmux key names: {"names": [...], "strict": bool}. A
// strict failure is 400 with the offending names in "unknown" and "lossy".
func (s *State) sendKeyNames(w http.ResponseWriter, r *http.Request, p params) {
	win := s.window(w, p)
	if win == nil {
		return
	}
	b, ok, handled := s.inputBody(w, r)
	if handled {
		return
	}
	strict, _, v2 := b.optBool("strict")
	var names []string
	if !ok || !v2 || !stringArray(b["names"]) || json.Unmarshal(b["names"], &names) != nil {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	err := win.SendKeyNames(names, strict)
	var kerr *shelld.KeyNameError
	switch {
	case errors.As(err, &kerr):
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   err.Error(),
			"unknown": nonNil(kerr.Unknown),
			"lossy":   nonNil(kerr.Lossy),
		})
	case err != nil:
		writeInputError(w, err)
	default:
		writeJSON(w, http.StatusOK, okBody)
	}
}

// stringArray reports whether raw is a JSON array of strings (no nulls,
// which encoding/json would decode as "").
func stringArray(raw json.RawMessage) bool {
	var elems []json.RawMessage
	if isNull(raw) || json.Unmarshal(raw, &elems) != nil || elems == nil {
		return false
	}
	for _, e := range elems {
		var s string
		if isNull(e) || json.Unmarshal(e, &s) != nil {
			return false
		}
	}
	return true
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// pasteReq is paste's body, decoded in one pass because text is the only field
// on any route that can reach the input read limit: holding it as a
// json.RawMessage copies it and then unescapes it again on access. mode and
// literal stay raw and go through the shared raw* helpers, which is what keeps
// this byte-for-byte as strict as the body map it replaced -- see decodeInto.
type pasteReq struct {
	Text    *string         `json:"text"`
	Mode    json.RawMessage `json:"mode"`
	Literal json.RawMessage `json:"literal"`
}

// paste pastes {"text", "mode"?, "literal"?} and presses Enter; see
// shelld.PasteWith. mode is "auto", "bracketed" or "raw" and defaults to the
// window's pasteMode (set at creation, "auto" by default).
//
// Breaking change: this route used to bracket always and send text
// unchanged. It now brackets only when the program enabled bracketed paste
// and sanitizes the text, so clients that add ESC[200~ … ESC[201~ themselves
// must send mode "bracketed" instead.
func (s *State) paste(w http.ResponseWriter, r *http.Request, p params) {
	win := s.window(w, p)
	if win == nil {
		return
	}
	var req pasteReq
	ok, handled := s.inputBodyInto(w, r, &req)
	if handled {
		return
	}
	mode, _, v2 := rawStr(req.Mode)
	literal, _, v3 := rawBool(req.Literal)
	// A nil Text is an absent field, a null one, or a body that failed to decode
	// at all; str("text", true) rejected all three too.
	if !ok || req.Text == nil || !v2 || !v3 {
		writeError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if mode != "" && !shelld.ValidPasteMode(mode) {
		writeError(w, http.StatusBadRequest, "invalid paste mode")
		return
	}
	if err := win.PasteWith(*req.Text, shelld.PasteOpts{Mode: mode, Literal: literal}); err != nil {
		writeInputError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, okBody)
}

func badQuery(w http.ResponseWriter, err string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte("Failed to deserialize query string: " + err))
}

// capture returns raw history (default) or the visible grid (mode=visible).
// On Windows raw history is conhost's rendering, not the program's output;
// see the package shelld documentation.
func (s *State) capture(w http.ResponseWriter, r *http.Request, p params) {
	win := s.window(w, p)
	if win == nil {
		return
	}
	// Strictly, because every parameter here has a default and net/url drops the
	// whole query rather than the segment it objects to. `?lines=1000;mode=visible`
	// read loosely is not a smaller answer but a different one: lines falls back to
	// 50 and mode falls back to raw history, returned with a 200 that says nothing
	// about either. See parseQueryStrict.
	q, ok := parseQueryStrict(w, r)
	if !ok {
		return
	}
	if refuseNearMissKey(w, q, "lines", "mode", "max_bytes") {
		return
	}
	n, ok := queryUint(q, "lines", 50)
	if !ok {
		badQuery(w, "invalid digit found in string")
		return
	}
	if q.Get("mode") == "visible" {
		writeJSON(w, http.StatusOK, map[string]any{
			"output":     win.CaptureVisible(n),
			"totalLines": win.ScrollbackLen(),
			"mode":       "visible",
		})
		return
	}
	maxBytes, ok := maxBytesQuery(q)
	if !ok {
		badQuery(w, "invalid digit found in string")
		return
	}
	// Sampled before the read, not after. A client polls totalBytes as its change
	// signal and banks it: whatever the count covers, it takes as already handed
	// over. Read after the text, the count includes bytes the program wrote while
	// this response was being built — bytes that are not in output, and that the
	// client will therefore never come back for. Read first, the count can only lag
	// the text, by bytes the next poll returns.
	totalBytes := win.OutputBytes()
	output, returned, lineBytes, total, truncated := win.CaptureBytes(n, maxBytes)
	if captureGapHook != nil {
		captureGapHook()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"output":        output,
		"linesReturned": returned,
		"lineBytes":     lineBytes,
		"totalLines":    total,
		"totalBytes":    totalBytes,
		"truncated":     truncated,
	})
}

// captureGapHook runs in the window between a capture's read of the text and the
// response being built, and is nil outside tests. The ordering above is only
// observable when the program writes in exactly that window, which is a few
// nanoseconds wide — 1 200 polls against a window being written to every 30 µs
// never landed in it — so a test that raced for it would pass whichever order the
// handler used. This makes the window reachable on demand:
// TestCaptureTotalBytesNeverRunsAheadOfTheOutput writes a line here and asserts
// the count does not cover it.
var captureGapHook func()

// maxReadBytes is the ceiling on the bytes one capture or scroll response
// carries, whatever max_bytes asks for.
//
// shelld.wMaxReadBytes is the same number for the same reason on the other side
// of the API: that one clamps the default the window derives when no bound is
// named. They are separate on purpose — this one is the HTTP request's limit —
// so changing one is a reason to look at the other.
const maxReadBytes = 64 << 20

// maxBytesQuery reads the optional max_bytes parameter, clamped to the ceiling.
// Absent, it is 0, which the window reads as the default: what the requested
// line count cost when a line ended at every PTY read.
func maxBytesQuery(q url.Values) (int, bool) {
	n, ok := queryUint(q, "max_bytes", 0)
	if !ok {
		return 0, false
	}
	return min(n, maxReadBytes), true
}

// scroll pages through raw history; on Windows that is conhost's rendering.
func (s *State) scroll(w http.ResponseWriter, r *http.Request, p params) {
	win := s.window(w, p)
	if win == nil {
		return
	}
	// Strictly, for the reason the test below this route's defaults gives: a parse
	// this route let slide would hand back the first page with a 200, and nothing in
	// the response says which page it is. That already held for `?offset=abc`; it
	// holds equally for `?offset=10;lines=1000`, which net/url drops in full, so
	// both parameters silently take their defaults at once. See parseQueryStrict.
	q, ok := parseQueryStrict(w, r)
	if !ok {
		return
	}
	if refuseNearMissKey(w, q, "offset", "lines", "max_bytes") {
		return
	}
	offset, ok1 := queryUint(q, "offset", 0)
	lines, ok2 := queryUint(q, "lines", 50)
	if !ok1 || !ok2 {
		badQuery(w, "invalid digit found in string")
		return
	}
	maxBytes, ok := maxBytesQuery(q)
	if !ok {
		badQuery(w, "invalid digit found in string")
		return
	}
	output, returned, lineBytes, truncated := win.ScrollRangeBytes(offset, lines, maxBytes)
	// totalLines is read after the text here, the opposite order from capture,
	// which reads it first. The orders are not interchangeable but both are
	// safe, in opposite directions: a pager that over-counts asks next for a
	// page that exists, where under-counting would stop paging early. Capture
	// needs the other order because its count describes the text it returned.
	writeJSON(w, http.StatusOK, map[string]any{
		"output":        output,
		"offset":        offset,
		"lines":         lines,
		"linesReturned": returned,
		"lineBytes":     lineBytes,
		"totalLines":    win.ScrollbackLen(),
		"truncated":     truncated,
	})
}

func (s *State) resize(w http.ResponseWriter, r *http.Request, p params) {
	win := s.window(w, p)
	if win == nil {
		return
	}
	b, ok, handled := controlBody(w, r)
	if handled {
		return
	}
	cols, v1 := b.uint("cols", true, 0xffff)
	rows, v2 := b.uint("rows", true, 0xffff)
	if !ok || !v1 || !v2 || cols == 0 || rows == 0 {
		writeError(w, http.StatusBadRequest, "cols and rows required")
		return
	}
	if err := shelld.CheckSize(uint16(cols), uint16(rows)); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := win.Resize(uint16(cols), uint16(rows)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, okBody)
}

// parseHexRGB accepts #rrggbb or rrggbb, parsing each channel like Rust's
// u8::from_str_radix (which tolerates a leading '+').
func parseHexRGB(s string) (vt.RGB, bool) {
	h := strings.TrimPrefix(s, "#")
	if len(h) != 6 {
		return vt.RGB{}, false
	}
	var ch [3]uint8
	for i := range ch {
		pair := h[i*2 : i*2+2]
		if pair[0] == '-' {
			return vt.RGB{}, false
		}
		v, err := strconv.ParseUint(strings.TrimPrefix(pair, "+"), 16, 8)
		if err != nil || pair == "+" {
			return vt.RGB{}, false
		}
		ch[i] = uint8(v)
	}
	return vt.RGB{R: ch[0], G: ch[1], B: ch[2]}, true
}

// setColors applies the colours a request names, drops the ones it cannot read,
// and answers 200 as long as one survived; 400 only when none did.
//
// A partly-wrong request being partly applied is the one place this daemon does
// that, and it is deliberate rather than a gap in the port: it is what the
// reference does in crates/shelld-daemon/src/routes.rs (set_colors), down to
// dropping a palette key that is not a number or is above 255. Refusing the
// whole request instead would be a divergence a caller written against
// shelld-rs would feel, so the behaviour is stated here for whoever reads this
// function and reaches for the tidier answer. TestSetColorsAppliesWhatItCanParse
// pins it, and says why partial is the right shape for a theme.
func (s *State) setColors(w http.ResponseWriter, r *http.Request, p params) {
	win := s.window(w, p)
	if win == nil {
		return
	}
	b, ok, handled := controlBody(w, r)
	if handled {
		return
	}
	bg, bgSet, v1 := b.optStr("background")
	fg, fgSet, v2 := b.optStr("foreground")
	cur, curSet, v3 := b.optStr("cursor")
	palette, v4 := stringMap(b["palette"])
	if !ok || !v1 || !v2 || !v3 || !v4 {
		writeError(w, http.StatusBadRequest, "colors required")
		return
	}
	entries := map[int]vt.RGB{}
	add := func(idx int, hex string, set bool) {
		if rgb, ok := parseHexRGB(hex); set && ok {
			entries[idx] = rgb
		}
	}
	add(shelld.IdxBackground, bg, bgSet)
	add(shelld.IdxForeground, fg, fgSet)
	add(shelld.IdxCursor, cur, curSet)
	addPalette(entries, palette)
	if len(entries) == 0 {
		writeError(w, http.StatusBadRequest, "no valid colors provided")
		return
	}
	win.SetColors(entries)
	writeJSON(w, http.StatusOK, okBody)
}

// addPalette adds the palette entries that parse to entries. Keys naming the
// same index ("3", "03", "+3") are resolved as shelld-rs resolves them
// (routes.rs:1588-1589): among those that parse, the shortest raw key wins and a
// tie goes to the lexicographically smaller, so the answer does not depend on the
// map's iteration order (H30).
func addPalette(entries map[int]vt.RGB, palette map[string]string) {
	winner := map[int]string{}
	for k, v := range palette {
		// TrimPrefix, not a bare ParseUint, because the key is parsed like Rust's
		// usize::from_str, which tolerates one leading '+' — the same allowance
		// parseHexRGB makes, for the same reason. Go's ParseUint takes no sign
		// at all, so without this "+7" is dropped here and applies slot 7 in the
		// reference. Neither of parseHexRGB's two sign guards is needed: '-' survives
		// the trim and ParseUint rejects it, and a bare "+" trims to "" which
		// ParseUint also rejects, both matching from_str.
		idx, err := strconv.ParseUint(strings.TrimPrefix(k, "+"), 10, 64)
		rgb, ok := parseHexRGB(v)
		if err != nil || !ok || idx > 255 {
			continue
		}
		if w, seen := winner[int(idx)]; seen && (len(w) < len(k) || len(w) == len(k) && w < k) {
			continue
		}
		winner[int(idx)] = k
		entries[int(idx)] = rgb
	}
}

func (s *State) attach(w http.ResponseWriter, r *http.Request, p params) {
	if err := wsserver.CheckUpgrade(r); err != nil {
		var ue *wsserver.UpgradeError
		errors.As(err, &ue)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(ue.Status)
		_, _ = w.Write([]byte(ue.Message))
		return
	}
	win := s.window(w, p)
	if win == nil {
		return
	}
	// ?observer=1 attaches read-only and non-voting. It is read before the
	// upgrade so no client byte can be mistaken for input first, and before the
	// 101 so a rejection is still an ordinary HTTP error the client can read.
	//
	// A parameter the daemon does not know is still ignored, which is what lets
	// an older shelld-go or shelld-rs degrade to a normal attach. But a value
	// this daemon cannot parse is a different case and is refused: the two
	// failure modes are not symmetric, because a rejected attach is a visible
	// error the client can fix while a silently-writable attach is a client that
	// believes it is read-only and is not.
	// The query is parsed strictly rather than through r.URL.Query, which
	// discards net/url's error and would let an unreadable query string decide
	// the flag by leaving it absent.
	q, ok := parseQueryStrict(w, r)
	if !ok {
		return
	}
	if refuseNearMissKey(w, q, "observer") {
		return
	}
	observer, ok := queryBool(q, "observer")
	if !ok {
		badQuery(w, "invalid value for `observer`, expected exactly one of `1`, `true`, `0`, `false` or a bare `observer`")
		return
	}
	// Last of the checks, so a missing window or a bad query is answered for what
	// it is rather than as a busy daemon, and still before the 101, for the reason
	// observer is read before it: past the upgrade there is no answer a client
	// can read, only a close frame to a peer that has not spoken yet.
	if !s.acquireAttach() {
		writeError(w, http.StatusServiceUnavailable, "too many attaches")
		return
	}
	defer s.releaseAttach()
	stop := s.upgraded.join()
	defer s.upgraded.leave()
	conn, err := wsserver.Upgrade(w, r)
	if err != nil {
		return
	}
	// Before serveAttach, which writes the opening frame: that write is as able to
	// block on a client that never reads as any later one.
	conn.SetWriteTimeout(s.wsWriteTimeout())
	serveAttach(conn, win, observer, stop, s.upgraded.stopDeadline)
}

func (s *State) frame(w http.ResponseWriter, _ *http.Request, p params) {
	win := s.window(w, p)
	if win == nil {
		return
	}
	// One read for the pair, so the two numbers are one size rather than two.
	// They can still disagree with the frame beside them, which is rendered in a
	// separate acquisition after this one: a resize landing between the two is
	// reported as the old geometry around a repaint at the new one. Clients
	// redraw on the next frame, and the status frame on the attach wire is the
	// interface that does not have this gap -- see internal/daemon/attach.go.
	cols, rows := win.Size()
	writeJSON(w, http.StatusOK, map[string]any{
		"frame": string(win.RenderFrame()),
		"cols":  cols,
		"rows":  rows,
	})
}

func (s *State) inject(w http.ResponseWriter, r *http.Request, p params) {
	win := s.window(w, p)
	if win == nil {
		return
	}
	b, ok, handled := s.inputBody(w, r)
	if handled {
		return
	}
	payload, v1 := b.str("payload", true)
	priority, v2 := b.int32("priority")
	if !ok || !v1 || !v2 || payload == "" {
		writeError(w, http.StatusBadRequest, "payload required")
		return
	}
	q := win.InjectQueue()
	msg, err := q.Enqueue(payload, priority)
	if errors.Is(err, shelld.ErrQueueClosed) {
		// The window's delivery loop has exited. Answered as /keys answers a
		// dead window, not with a 202 for a payload nothing will deliver.
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("window %q is dead", win.Name()))
		return
	}
	if errors.Is(err, shelld.ErrPayloadTooLarge) {
		// No state of the queue admits it, so not the 429 that invites a retry.
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	n := q.Len()
	writeJSON(w, http.StatusAccepted, map[string]any{"id": msg.ID, "position": n, "queueLength": n})
}

func (s *State) injectQueue(w http.ResponseWriter, _ *http.Request, p params) {
	win := s.window(w, p)
	if win == nil {
		return
	}
	// Both arrays are normalized, because a nil slice marshals as null and an empty
	// queue is not an absent one. The reference daemon hands serde a Vec for
	// pending, so [] is also what a client of either daemon gets to rely on.
	pending := win.InjectQueue().Pending()
	if pending == nil {
		pending = []shelld.InjectMessage{}
	}
	// expired is what turns a stall timeout from silent loss into something a
	// polling client can see: the enqueue already returned 202, so a payload that
	// is neither pending nor typed is otherwise indistinguishable from one still
	// waiting. Always present, empty when nothing has expired.
	expired := win.InjectExpiries()
	if expired == nil {
		expired = []shelld.InjectExpiry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"expired": expired, "length": len(pending), "pending": pending,
	})
}

func (s *State) moveWindow(w http.ResponseWriter, r *http.Request, _ params) {
	const msg = "srcSession, srcName, dstSession required"
	b, ok, handled := controlBody(w, r)
	if handled {
		return
	}
	srcSession, v1 := b.str("srcSession", true)
	srcName, v2 := b.str("srcName", true)
	dstSession, v3 := b.str("dstSession", true)
	dstName, v4 := b.str("dstName", false)
	if !ok || !v1 || !v2 || !v3 || !v4 || srcSession == "" || srcName == "" || dstSession == "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// The two names this route creates: a window's new name, and the session
	// MoveWindow makes when dstSession does not exist. srcSession and srcName are
	// lookups, so a control character in either misses and is answered 409.
	// dstName first, as shelld-rs checks them; an empty one keeps the source name
	// and passes.
	if !checkName(w, "dstName", dstName) || !checkName(w, "dstSession", dstSession) {
		return
	}
	if err := s.Mgr.MoveWindow(srcSession, srcName, dstSession, dstName); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, okBody)
}
