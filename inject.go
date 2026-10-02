package shelld

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gurupras/shelld/internal/utf8x"
	"github.com/gurupras/shelld/uniwidth"
)

// InjectMessage is a queued payload delivered to the PTY once the window is
// idle. Fields are declared in JSON key order (serde_json sorts keys).
type InjectMessage struct {
	CreatedAt int64 `json:"createdAt"` // ms since epoch
	// ID identifies the message within its queue. Enqueue assigns it — "inj-N",
	// counted per queue — so it is unique among a window's messages but not
	// across windows or across restarts of the daemon. It is what a caller
	// matches a message in Pending against; nothing here can remove a queued
	// message by ID.
	ID string `json:"id"`
	// Payload is typed into the program verbatim, followed by a carriage return.
	// Unlike Paste it is neither bracketed nor sanitized, so it has to be text
	// the caller trusts: a control character in it reaches the program as one.
	Payload string `json:"payload"`
	// Priority orders the queue, higher first, FIFO within one value. It may be
	// negative, which is how a caller queues something behind the default 0.
	Priority int32 `json:"priority"`
}

// InjectExpiry is a message the delivery loop gave up on, kept so that a caller
// can tell a delivered payload from a discarded one. Fields are declared in JSON
// key order (serde_json sorts keys).
//
// Without this an expiry would be as invisible as the unbounded wait it
// replaces: the enqueue already returned 202, so a caller that never sees the
// payload typed has no other way to learn it was dropped rather than still
// queued. Reason is why the window never looked ready, which is the part a
// caller cannot reconstruct.
type InjectExpiry struct {
	ExpiredAt int64         `json:"expiredAt"` // ms since epoch
	Message   InjectMessage `json:"message"`
	// Reason is why the payload never went to the program, which is the part a
	// caller cannot reconstruct:
	//
	//	scan-refused  the newest output held something the idle scan could not
	//	              place on the screen -- the case where a window can stay
	//	              unreadable indefinitely, since an idle program writes
	//	              nothing further
	//	not-idle      every other way the window was not ready: the scan read the
	//	              screen and it was not a prompt, a human was typing, or input
	//	              was still queued for the program
	//	outranked     the window was ready and kept delivering, but higher-priority
	//	              payloads went first for longer than the deadline
	//	no-room       the window was ready and its input queue had no room yet: the
	//	              program has stopped reading it, or another window is holding the
	//	              budget they share. Recoverable -- the payload would have gone
	//	              through if either changed before the deadline
	//	too-large     the payload is larger than the whole of that queue's limit, or
	//	              of the shared budget, so no state of either would ever have
	//	              taken it
	//	window-gone   the window's input queue is closed, so nothing will be typed
	//	              into it again. Written for every payload still queued at that
	//	              moment, and the only reason not describing a screen or a wait
	//
	// A reason is the delivery loop's verdict for the poll that dropped the payload,
	// not a property of the payload, and a poll drops everything past the deadline
	// at once: a payload that had been waiting behind the one the loop actually
	// tried carries the same reason, whether or not it would have fit or been
	// outranked on its own. What the three deadline reasons -- not-idle,
	// scan-refused, outranked -- do mean in every case is that the deadline was spent
	// waiting and not mid-delivery: a payload the loop is about to type is not
	// expired.
	//
	// The other two are not deadlines at all, and for a caller the practical
	// difference is that neither implies any time was spent. too-large is dropped on
	// the poll that tries it, however much time is left, because no amount of waiting
	// makes the payload smaller; it is kept apart from no-room for the reason
	// ErrInputTooLarge is kept apart from ErrInputQueueFull, that one of them is worth
	// retrying and the other never is. window-gone is dropped because there is no
	// longer anything to wait for, and it is the only reason that can carry a payload
	// enqueued moments earlier. shelld-rs is being held to the no-room/too-large pair
	// of names; see the reason constants in window.go for what that rests on.
	Reason string `json:"reason"`
}

// injectExpiryLogSize is how many expiries a window remembers. A caller polling
// the queue wants the recent ones; keeping every expiry would be an unbounded
// buffer fed by a timer, which is the shape this whole change exists to remove.
const injectExpiryLogSize = 16

// expiryLog is a window's bounded record of expired injections, newest last.
type expiryLog struct {
	mu      sync.Mutex
	entries []InjectExpiry
}

func (l *expiryLog) add(e InjectExpiry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
	if n := len(l.entries); n > injectExpiryLogSize {
		l.entries = append(l.entries[:0], l.entries[n-injectExpiryLogSize:]...)
	}
}

func (l *expiryLog) all() []InjectExpiry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]InjectExpiry(nil), l.entries...)
}

// InjectConfig tunes idle detection and delivery.
//
// Nothing outside this package sets one directly: newWindowState takes it, and
// both callers start from DefaultInjectConfig. NewWindow then replaces
// IdlePattern with the pattern for the shell it is about to run and honours
// WindowOpts.PasteMode and WindowOpts.StallTimeoutMS; every other field is the
// default below on every window. Two fields still have no implementation behind
// them and are inert even there — StashIdleThresholdMS and
// StashRestoreTimeoutMS.
type InjectConfig struct {
	// IdlePattern is the prompt regex, in the Rust regex crate's syntax, that
	// decides a window is waiting for input. It is translated for Go's engine
	// by RustRegex, and an empty value means the default ❯ prompt. NewWindow
	// derives it from the shell instead; see IdlePatternForProgram.
	IdlePattern string
	// ScanLines is how many lines back from the end of raw history the idle
	// scan reads. Only the newest line with text on it is matched, but a
	// program can write a line in pieces, so the scan needs more than one line
	// to find it. Zero means 5.
	ScanLines int
	// DebounceMS is how long after the last byte a human typed a window counts
	// as quiet. Nothing is injected before then: the delivery would interleave
	// with what the user is in the middle of typing. Zero means 500.
	DebounceMS uint64
	// DeliveryCooldownMS is how long the delivery loop waits after writing a
	// payload before it considers the next one, so that a queue of messages is
	// not typed into a program all at once while the first is still starting.
	// Zero means no wait.
	DeliveryCooldownMS uint64
	// StallTimeoutMS is how long a queued payload may wait to be delivered
	// before the delivery loop discards it, measured from its own enqueue.
	// Zero means no deadline: it waits until the window is ready or dies.
	// Values past maxStallMS are clamped rather than wrapped; see stallTimeout.
	//
	// It exists because the idle scan's refusal is safe but can be permanent. A
	// window whose newest output holds a control the scan cannot place — `clear`
	// is enough, since the shell then draws its prompt on the line holding ED —
	// refuses for as long as it stays idle, and nothing the program writes by
	// itself restores trust. A keystroke from any client does: a bare CR makes
	// the shell draw a fresh prompt on a line of its own. But an idle window with
	// no human at it receives none, so without a deadline the payload waits for
	// ever, having already been accepted.
	//
	// An expiry drops the payload; it never types it. The scan cannot tell a
	// prompt it failed to model from a full-screen program (vi at its splash
	// screen refuses just as `clear` does), so delivering on a deadline would be
	// an unprompted keystroke into whatever is running, which is the failure the
	// refusal exists to prevent. A dropped payload can be re-sent; a buffer
	// edited by a stray injection cannot be un-edited. Expiries are logged and
	// recorded in Window.InjectExpiries.
	StallTimeoutMS uint64
	// MaxQueueSize is how many messages may be queued at once; Enqueue returns
	// ErrQueueFull beyond it. Zero means 64.
	MaxQueueSize int
	// StashIdleThresholdMS and StashRestoreTimeoutMS are not implemented
	// either. StashState exists, but it is driven explicitly through Begin and
	// Complete and holds no timers, so nothing times a stash out.
	StashIdleThresholdMS  uint64
	StashRestoreTimeoutMS uint64
	// SendKeysDelayMS is the least gap between text and the Enter that follows
	// it, where the Enter is meant as a keypress rather than part of the text:
	// an unbracketed paste and a SendKeys that asks for Enter. A TUI that
	// watches how fast input arrives reads a newline in the same burst as the
	// text as part of the paste, and one after a gap as a submit. Zero sends
	// them back to back. See Window.PasteWith and Window.SendKeys.
	SendKeysDelayMS uint64
	// PasteMode is the default for Paste when PasteOpts.Mode is empty: "auto",
	// "bracketed" or "raw". See Window.Paste.
	PasteMode string
}

const defaultIdlePattern = `(?m)^\s*❯\s*$`

// DefaultInjectConfig returns the configuration every window starts from.
//
// The timings, sizes and pattern are shelld-rs's, so injection behaves the same
// under either daemon. PasteMode is the one deliberate difference: "auto" here
// against shelld-rs's "bracketed", so that a paste into a program that never
// asked for bracketed paste is not wrapped in markers it will show as text. See
// Window.Paste.
func DefaultInjectConfig() InjectConfig {
	return InjectConfig{
		IdlePattern:           defaultIdlePattern,
		ScanLines:             5,
		DebounceMS:            500,
		DeliveryCooldownMS:    2000,
		StallTimeoutMS:        300_000,
		MaxQueueSize:          64,
		StashIdleThresholdMS:  30_000,
		StashRestoreTimeoutMS: 60_000,
		SendKeysDelayMS:       50,
		PasteMode:             "auto",
	}
}

// maxStallMS is the largest StallTimeoutMS that survives the conversion to a
// duration: time.Duration counts nanoseconds in an int64, so a larger value
// overflows the multiply by time.Millisecond. It bounds every millisecond field
// rather than that one field, because the arithmetic is the field's unit and not
// its meaning; window.go's enterDelay says the same thing from the other side.
const maxStallMS = uint64(math.MaxInt64) / uint64(time.Millisecond)

// msDuration converts a millisecond count to a duration, clamping at the largest
// value that fits instead of wrapping.
//
// Five inputs reach it: the StallTimeoutMS, DeliveryCooldownMS, DebounceMS and
// SendKeysDelayMS config fields, plus SendKeys' enterDelayMS argument, which
// overrides SendKeysDelayMS per call and so is the one that arrives straight off
// a request rather than out of a config. They share this because they share the
// hazard rather than merely the unit: each is an unsigned count that a caller may
// set enormously high to mean "effectively never", and the wrap yields an
// arbitrary value, not a large one. Which arbitrary value decides how it fails, so it is worth being
// concrete: maxStallMS+1 wraps to about -2562047h, while 18446744073710 wraps to
// 448.384µs. A consumer comparing against zero therefore sees either "no bound
// at all" or "a bound that expires immediately", from two inputs a caller would
// read as the same request. stallTimeout below records what that cost in the one
// case where the wrapped negative is also the documented opt-out.
func msDuration(ms uint64) time.Duration {
	if ms > maxStallMS {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ms) * time.Millisecond
}

// stallTimeout converts a StallTimeoutMS to the duration the delivery loop
// compares ages against, clamping instead of wrapping.
//
// The clamp is load-bearing, not defensive. StallTimeoutMS is an unsigned count
// of milliseconds, so a caller wanting "effectively never" may reasonably write
// something enormous, and the wrap is not a large number but an arbitrary one:
// 18446744073710 lands on 448µs, which discards every payload on its first poll,
// and math.MaxUint64 lands on -1ms, which ExpireOlderThan reads as the opt-out
// and so removes the deadline altogether. Both are silent. Clamping gives such a
// caller what it asked for — the maximum is about 292 years — and keeps zero the
// only value that means no deadline.
func stallTimeout(ms uint64) time.Duration { return msDuration(ms) }

// ErrQueueFull is returned by Enqueue at capacity.
var ErrQueueFull = errors.New("queue full")

// ErrPayloadTooLarge is returned, as a *PayloadTooLargeError naming the sizes,
// by Enqueue for a payload larger than the whole of the queue's byte cap or of
// the budget queues share, so no state of either admits it. Kept apart from
// ErrQueueFull, as ErrInputTooLarge is kept apart from ErrInputQueueFull: that
// one clears as the queue drains and this one never does.
var ErrPayloadTooLarge = errors.New("payload too large")

// PayloadTooLargeError is the ErrPayloadTooLarge Enqueue returns. Its text is
// the one agreed with shelld-rs for the 413 (H16), naming the limit so a
// client knows how much less to send.
type PayloadTooLargeError struct {
	Wanted int   // the payload's bytes
	Limit  int64 // the queue's byte cap, or the shared budget's where smaller
}

func (e *PayloadTooLargeError) Error() string {
	return fmt.Sprintf("payload of %d bytes exceeds the %d-byte inject queue", e.Wanted, e.Limit)
}

func (e *PayloadTooLargeError) Unwrap() error { return ErrPayloadTooLarge }

// DefaultInjectBudget is the limit of the budget inject queues share, as
// shelld-rs's DEFAULT_INJECT_BUDGET (inject.rs:293). The per-queue cap alone
// does not bound the daemon, since window count is a client's to choose. A
// budget of its own rather than the input one: the bytes a queue retains are a
// copy beside the ones the input queue holds once delivery starts.
const DefaultInjectBudget = 256 << 20

var defaultInjectBudget = NewInputBudget(DefaultInjectBudget)

// ErrQueueClosed is returned by Enqueue once the queue is closed: its window's
// delivery loop has exited, so nothing would ever deliver or expire the message.
var ErrQueueClosed = errors.New("queue closed")

// InjectQueue orders messages by priority (higher first), FIFO within a
// priority.
//
// It is a plain slice scanned for the highest priority on each Dequeue, not a
// heap: MaxQueueSize is 64 by default and a dequeue happens at most once per
// delivery cooldown, so the scan costs nothing measurable and keeps FIFO within
// a priority without a sequence number. All of its methods are safe to call
// from several goroutines.
type InjectQueue struct {
	mu       sync.Mutex
	messages []InjectMessage
	maxSize  int
	maxBytes int          // bound on bytes, the sum of the payloads queued
	bytes    int          // that sum
	budget   *InputBudget // shared across queues; holds the same bytes
	nextID   uint64
	signal   chan struct{}
	closed   bool
}

// NewInjectQueue returns an empty queue holding at most maxSize messages; zero
// means 64, as InjectConfig.MaxQueueSize does.
// Its payloads may total DefaultInputQueueLimit bytes, from a budget all such
// queues share; a window's queue follows the window's InputQueueLimit instead.
func NewInjectQueue(maxSize int) *InjectQueue {
	return newInjectQueue(maxSize, DefaultInputQueueLimit, defaultInjectBudget)
}

func newInjectQueue(maxSize, maxBytes int, budget *InputBudget) *InjectQueue {
	if maxSize == 0 {
		maxSize = 64
	}
	return &InjectQueue{maxSize: maxSize, maxBytes: maxBytes, budget: budget, signal: make(chan struct{}, 1)}
}

// TryRecvSignal consumes a pending enqueue signal, if any.
func (q *InjectQueue) TryRecvSignal() bool {
	select {
	case <-q.signal:
		return true
	default:
		return false
	}
}

// Signal fires (at most once buffered) on each enqueue.
func (q *InjectQueue) Signal() <-chan struct{} { return q.signal }

// Enqueue adds a message and returns it filled in: the queue assigns the ID
// ("inj-N", counted per queue) and the creation time, so the returned message —
// not the payload the caller passed — is what identifies it later. It returns
// ErrQueueClosed once the queue is closed, ErrPayloadTooLarge for a payload no
// state of the queue admits, and ErrQueueFull when it is at its message count,
// retaining the payload would pass its byte cap, or the shared budget has no
// room, having added nothing. That order is shelld-rs's (inject.rs:765-820).
//
// Enqueue does not deliver. It fires Signal, and the window's delivery loop
// writes the payload once the program looks idle, which may be much later or,
// if the window dies first, never.
func (q *InjectQueue) Enqueue(payload string, priority int32) (InjectMessage, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return InjectMessage{}, ErrQueueClosed
	}
	n := len(payload)
	// First of the caps, because it is the only permanent one.
	if limit := min(int64(q.maxBytes), q.budget.limit); int64(n) > limit {
		return InjectMessage{}, &PayloadTooLargeError{Wanted: n, Limit: limit}
	}
	if len(q.messages) >= q.maxSize {
		return InjectMessage{}, ErrQueueFull
	}
	// Last, so the refusals above never have a reservation to give back.
	if q.bytes+n > q.maxBytes || !q.budget.reserve(int64(n)) {
		return InjectMessage{}, ErrQueueFull
	}
	q.bytes += n
	q.nextID++
	msg := InjectMessage{
		ID:        fmt.Sprintf("inj-%d", q.nextID),
		Payload:   payload,
		CreatedAt: time.Now().UnixMilli(),
		Priority:  priority,
	}
	q.messages = append(q.messages, msg)
	select {
	case q.signal <- struct{}{}:
	default:
	}
	return msg, nil
}

// Dequeue removes and returns the message that should be delivered next: the
// earliest of those with the highest priority. It reports false when the queue
// is empty.
//
// A dequeued message is out of the queue: it no longer appears in Pending, and
// nothing else holds it. A caller that dequeues and then cannot deliver must
// Requeue it or the payload is lost.
func (q *InjectQueue) Dequeue() (InjectMessage, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.messages) == 0 {
		return InjectMessage{}, false
	}
	best := 0
	for i := 1; i < len(q.messages); i++ {
		if q.messages[i].Priority > q.messages[best].Priority {
			best = i
		}
	}
	msg := q.messages[best]
	q.messages = append(q.messages[:best], q.messages[best+1:]...)
	q.release(len(msg.Payload))
	return msg, true
}

// Requeue puts a dequeued message back at the front, ahead of others of its
// priority, even when the queue is full: it was already accepted. Its bytes are
// counted again past either cap for the same reason, as shelld-rs's
// force_reserve does; the overshoot is bounded by the one payload dequeued.
func (q *InjectQueue) Requeue(msg InjectMessage) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(msg.Payload)
	q.bytes += n
	q.budget.used.Add(int64(n))
	q.messages = append([]InjectMessage{msg}, q.messages...)
}

// Len is how many messages are queued. It is a sample: a delivery loop running
// against the same queue can change it before the caller reads the next line.
func (q *InjectQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.messages)
}

// Pending returns a copy of the queued messages in the order they happen to be
// stored, which is not necessarily either the order they were enqueued in or the
// order they will be delivered in: Requeue puts a message back at the front, and
// Dequeue takes from wherever the highest priority sits. On a queue that has only
// ever been enqueued and dequeued it is the enqueue order, since Enqueue appends
// and Dequeue removes in place — but that is a consequence of the implementation,
// not a promise. A caller showing a queue should sort it by descending priority
// then CreatedAt, which is the delivery order as far as a millisecond timestamp
// can distinguish it.
func (q *InjectQueue) Pending() []InjectMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]InjectMessage(nil), q.messages...)
}

// ExpireOlderThan removes every message enqueued more than age ago and returns
// them, in the order they were stored — the same order Pending reports, with the
// same caveat that it is not necessarily either enqueue or delivery order. An age
// of zero or less expires nothing.
//
// The deadline is per message, measured from its own CreatedAt, so neither
// priority reordering nor a Requeue extends it: Requeue puts back the same
// struct, and a message that waited out its deadline behind a higher-priority
// one is as stale as the caller's payload is. Both sides of the comparison are
// wall-clock, so a clock step moves the deadline with it. The caller is expected
// to log what it gets back — dropping the return value discards those payloads
// silently. DrainAll is the other method with that property; between them they are
// the only two ways a queued payload leaves without being delivered.
//
// The scan is two passes so that the common case allocates nothing: a delivery
// loop calls this every poll, and almost every call finds nothing to expire.
func (q *InjectQueue) ExpireOlderThan(age time.Duration) []InjectMessage {
	if age <= 0 {
		return nil
	}
	cutoff := time.Now().Add(-age).UnixMilli()
	q.mu.Lock()
	defer q.mu.Unlock()
	stale := false
	for _, m := range q.messages {
		if m.CreatedAt <= cutoff {
			stale = true
			break
		}
	}
	if !stale {
		return nil
	}
	var expired []InjectMessage
	kept := make([]InjectMessage, 0, len(q.messages))
	for _, m := range q.messages {
		if m.CreatedAt <= cutoff {
			expired = append(expired, m)
			q.release(len(m.Payload))
			continue
		}
		kept = append(kept, m)
	}
	q.messages = kept
	return expired
}

// DrainAll empties the queue and returns everything that was in it, in the order
// it happened to be stored — the same order Pending reports, with the same caveat
// that it is neither enqueue nor delivery order. It returns nil for an empty
// queue, so a caller can treat a nil answer as nothing to report.
//
// For the delivery loop giving up on a window whose input queue has been closed:
// nothing will ever be typed into that window again, so every payload still queued
// is undeliverable, and leaving them in place is worse than losing them. They stay
// visible through Pending, which the /inject/queue route reports as `pending` — so
// a client polling for the payload it holds a 202 for is told, for as long as
// anyone asks, that it is still waiting its turn. Removing them makes that answer
// change, and recording them under a reason makes it change to something true.
//
// Unlike ExpireOlderThan this is not a deadline and does not consult CreatedAt: the
// window is gone, so a payload enqueued one millisecond ago is exactly as
// deliverable as one that has waited an hour.
func (q *InjectQueue) DrainAll() []InjectMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.drainLocked()
}

// Close drains the queue and refuses every later Enqueue with ErrQueueClosed,
// under one lock, so no message can arrive between the drain and the close and
// be left with nothing to deliver or expire it.
func (q *InjectQueue) Close() []InjectMessage {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	return q.drainLocked()
}

func (q *InjectQueue) drainLocked() []InjectMessage {
	if len(q.messages) == 0 {
		return nil
	}
	drained := q.messages
	q.messages = nil
	q.release(q.bytes)
	return drained
}

// release gives back n bytes of messages that have left the queue.
func (q *InjectQueue) release(n int) {
	q.bytes -= n
	q.budget.release(int64(n))
}

// InputTracker records when a human last typed, so injection waits for a
// debounce window of quiet.
type InputTracker struct {
	mu       sync.Mutex
	last     time.Time
	debounce time.Duration
}

// NewInputTracker returns a tracker that counts a window quiet debounceMS after
// the last Touch; zero means 500, as InjectConfig.DebounceMS does. A tracker
// that has never been touched is quiet.
func NewInputTracker(debounceMS uint64) *InputTracker {
	if debounceMS == 0 {
		debounceMS = 500
	}
	return &InputTracker{debounce: msDuration(debounceMS)}
}

// Touch records that a human has just typed, restarting the debounce window.
// Only input that models a person at the keyboard should touch it: a paste or an
// injected payload deliberately does not, or a delivery would hold off the next
// one for a window it caused itself.
func (t *InputTracker) Touch() {
	t.mu.Lock()
	t.last = time.Now()
	t.mu.Unlock()
}

// IsQuiet reports whether the debounce window since the last Touch has elapsed.
// It is a sample of the clock, so a caller that acts on true races a keystroke
// arriving next; the window exists to make that unlikely, not impossible.
func (t *InputTracker) IsQuiet() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.last.IsZero() || time.Since(t.last) >= t.debounce
}

// IdleDetector matches a prompt pattern against ANSI-stripped recent output.
type IdleDetector struct {
	pattern   *regexp.Regexp
	scanLines int
}

// NewIdleDetector compiles pattern, which is Rust regex syntax translated by
// RustRegex, and reads scanLines lines back from the end of history. An empty
// pattern means the default ❯ prompt and zero scanLines means 5, as
// InjectConfig's defaults do.
//
// It panics on a pattern that does not compile, since a window cannot run
// without one; validate an untrusted pattern with regexp.Compile(RustRegex(p))
// first. A detector is immutable once built and safe to share.
func NewIdleDetector(pattern string, scanLines int) *IdleDetector {
	if pattern == "" {
		pattern = defaultIdlePattern
	}
	if scanLines == 0 {
		scanLines = 5
	}
	return &IdleDetector{pattern: regexp.MustCompile(RustRegex(pattern)), scanLines: scanLines}
}

// IsIdle matches the prompt pattern against the last non-empty line of the
// recent output, escape sequences stripped. Earlier lines don't count: a
// shell's prompt and the command echoed after it can arrive in separate
// reads and be stored as separate lines, so a bare prompt line stays among
// the recent lines while the command runs. (shelld-rs matches all of them.)
func (d *IdleDetector) IsIdle(lastNLines string) bool {
	return d.pattern.MatchString(lastNonEmptyLine(StripANSI(lastNLines)))
}

// IsIdleScan is IsIdle over the idle scan's lines, which carry whether the scan
// could establish what the terminal is showing for each of them.
//
// It reads back from the newest line to the first one with text on it, the same
// line IsIdle would match, and refuses — reports not idle — when it reaches a line
// the scan could not model. Refusing is not the same as skipping such a line: a
// line whose text cannot be placed on the screen can be hiding a prompt or hiding
// a program still running, and skipping it would match an older line instead. That
// older line is the last prompt the shell drew, so skipping turns an unreadable
// screen into a false idle, which types into a running program; refusing delays an
// injection until the program writes a line the scan can read.
//
// A line that renders to nothing but white space is skipped as IsIdle skips it,
// but only once its trust has been established — an untrusted blank line still
// stops the scan, because "the render produced no text" is not evidence that the
// screen holds none.
func (d *IdleDetector) IsIdleScan(lines []scanLine) bool {
	idle, _, _ := d.idleScan(lines)
	return idle
}

// idleScan is IsIdleScan, also reporting the line it refused at. A refusal is
// indefinite — drainLoop has no deadline, and a shell sitting at a prompt the
// scan cannot read writes nothing that would restore trust — so the caller
// delivering injections says so once rather than leaving the queue silent. It
// is one function with IsIdleScan so that what is logged is what was decided.
func (d *IdleDetector) idleScan(lines []scanLine) (idle bool, refusedAt string, refused bool) {
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if !lines[i].trusted {
			return false, lines[i].text, true
		}
		// Stripped per line, where IsIdle strips the joined text. The two differ
		// only for an escape sequence spanning a line boundary, which the render
		// above already treats as two lines.
		if stripped := StripANSI(lines[i].text); strings.TrimFunc(stripped, unicode.IsSpace) != "" {
			last = stripped
			break
		}
	}
	return d.pattern.MatchString(last), "", false
}

// scanLine is one line of the idle scan's view of recent output: the text a
// terminal would be showing for that line, and whether the scan could establish
// that this is that text — that the line holds nothing the render does not model,
// and that it begins where the program began a line rather than where a byte
// bound cut one.
//
// The trust travels beside the text, not inside it. The earlier shape marked an
// untrusted line by prefixing a NUL to its text, which put a byte no program
// wrote into a stream that is otherwise the program's own output and made the
// guarantee depend on every reader treating that byte as the scan meant it. In-band
// markers in a byte stream are how the paste-injection and idle-detection bugs
// happened; a field cannot be confused with content.
type scanLine struct {
	text    string
	trusted bool
}

// renderScanLines replaces every line of s with what a terminal would be showing
// for it, for the part of a terminal this can model exactly: carriage returns and
// the erase-in-line sequences.
//
// Raw history keeps a line as the program wrote it, so a line the program drew
// over in place — a progress bar, a prompt redrawn from column 0 — holds every
// state it passed through. What a terminal shows is the result of replaying them,
// and that is what the idle pattern is written against: against the whole line a
// pattern anchored at the start of one matches nothing, and the earlier states are
// text the terminal is no longer showing.
//
// A '\r' returns to column 0 and what follows *overwrites*; it does not replace.
// So a redraw shorter than what it drew over leaves the tail of the old text on
// screen, and the rendered line keeps it — a line reading "Running 100 tests...\r❯ "
// is showing "❯ nning 100 tests...", which is not a prompt, and must not be
// reduced to one. For the same reason a line that merely ends in '\r' still shows
// all of its text.
//
// A sequence that moves the cursor or edits the screen can put the text anywhere,
// and a line carrying one cannot be rendered from its own bytes. Such a line comes
// back with trusted false rather than guessed at, and IsIdleScan refuses to read
// idle from it: a line this cannot model must not be able to report a prompt. The
// price is that a full-screen program's cursor-addressed prompt no longer matches,
// where before it matched because StripANSI removed the addressing and left the
// glyphs looking like a line they were never on.
func renderScanLines(s string) []scanLine {
	lines := strings.Split(s, "\n")
	out := make([]scanLine, len(lines))
	for i, line := range lines {
		if !hasControl(line) {
			out[i] = scanLine{text: line, trusted: true}
			continue
		}
		rendered, exact, _ := renderCR(line)
		out[i] = scanLine{text: rendered, trusted: exact}
	}
	return out
}

// hasControl reports whether s holds anything renderCR would treat as other than
// a printable character. A line without one renders to itself.
//
// "Anything renderCR would treat as other than printable" is the whole predicate,
// not just the C0 controls: renderCR also gives up (exact = false) on every rune
// uniwidth gives a width of zero or less — DEL, the 8-bit C1s, combining marks,
// zero-width runes. Testing r < 0x20 alone let such a line past as trusted
// verbatim, which is worse than mis-rendering it, because trusted is what lets
// IsIdleScan read a prompt off the line. U+0085 (NEL) is the sharp case, and it is
// the width test rather than r < 0x20 that catches it: six runes are both a space to
// unicode.IsSpace and given no width by uniwidth, and NEL is the only one of the six
// outside the C0 controls (the other five are U+0009 to U+000D, which r < 0x20
// already had). So a line holding only that byte pair was trusted, treated as blank,
// and the scan
// walked back past a running program's output to the shell's previous prompt —
// reporting idle, which is the gate for typing a payload into whatever is in the
// foreground.
//
// The ASCII printables are short-circuited before the uniwidth call because this
// runs over every scanned line and they are almost all of it.
func hasControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool {
		if r >= 0x20 && r < 0x7f {
			return false
		}
		return r < 0x20 || uniwidth.Width(r) <= 0
	}) >= 0
}

// wideSpacer stands for the continuation cell of a rune that occupies more than
// one column. A terminal holds such a rune in its leading cell and leaves the
// following cells as spacers; keeping them in the column buffer is what makes a
// redraw that overwrites part of an earlier wide glyph land at the columns the
// terminal used. A spacer carries no text and is dropped when the line is built.
const wideSpacer rune = -1

// renderCR replays one line over a column buffer, the way a terminal does.
//
// Columns are counted with uniwidth.Width, the same table vt uses to lay out the
// grid, so a line of double-width glyphs redrawn over itself renders at the
// columns the terminal put it at. One rune per column would put the redraw two
// cells left of where it belongs for every wide glyph before it, which turns a
// line still showing output into one that looks like a bare prompt.
//
// exact is false when the line holds a control this does not model, in which case
// the text is whatever the rest produced and the caller must not trust it as what
// is on screen.
//
// fromScratch reports that the line erased its row outright — CSI 2 K, or CSI K at
// column 0 — after which nothing before that point can still be showing. It is
// what lets a caller trust the render of a line it holds only the end of: a byte
// bound can cut a line anywhere, and a render that begins mid-line otherwise puts
// text at columns the program never wrote it at. A bare '\r' is not enough, since
// the text it returns over may run past the redraw and stay on screen.
func renderCR(line string) (text string, exact, fromScratch bool) {
	var buf []rune
	col := 0
	exact = true
	pad := func() {
		for len(buf) < col {
			buf = append(buf, ' ')
		}
	}
	// blank clears the cell at c together with the rest of the glyph it belongs
	// to: a terminal that overwrites or erases either half of a wide rune blanks
	// both cells rather than leaving half a glyph on screen.
	blank := func(c int) {
		if c < 0 || c >= len(buf) {
			return
		}
		for c > 0 && buf[c] == wideSpacer {
			c--
		}
		buf[c] = ' '
		for c++; c < len(buf) && buf[c] == wideSpacer; c++ {
			buf[c] = ' '
		}
	}
	for i := 0; i < len(line); {
		r, size := utf8.DecodeRuneInString(line[i:])
		i += size
		switch {
		case r == '\r':
			col = 0
		case r == '\b':
			if col > 0 {
				col--
			}
		case r == '\t':
			col = (col/8 + 1) * 8
		case r == '\a': // a bell draws nothing
		case r == 0x1b:
			n, kind, mode := classifyEscape(line[i-size:])
			i = i - size + n
			switch kind {
			case escHarmless: // colours, modes, a window title: no glyph moves
			case escEraseLine:
				pad()
				switch mode {
				case 0: // erase from the cursor to the end of the line
					blank(col) // a wide glyph the cut lands inside goes whole
					buf = buf[:col]
					fromScratch = fromScratch || col == 0
				// This is the one branch where the render deliberately parts from vt,
				// and so from alacritty_terminal 0.26, which vt reproduces
				// faithfully: the terminal leaves the far cell of a glyph the erase
				// cut still flagged as that glyph's spacer, and a wide glyph written
				// there next is cleared again by the overwrite rule, losing a
				// character the line displayed. Blanking both halves is what a
				// terminal shows, and it is the safe direction for this scan besides:
				// the scan may call an idle line busy, never a busy line idle.
				// TestIdleScanKeepsAWideGlyphAlacrittyDrops pins the divergence and
				// fails if the grid ever agrees.
				case 1: // erase from the start of the line through the cursor
					for c := 0; c <= col && c < len(buf); c++ {
						buf[c] = ' '
					}
					// Only when the erase ended on the lead of a wide glyph, whose
					// other half goes with it. Blanking col+1 whatever it holds
					// erases one cell more than the sequence does, and on a line of
					// ordinary characters that cell is one the terminal still shows.
					if col+1 < len(buf) && buf[col+1] == wideSpacer {
						blank(col + 1)
					}
				case 2: // erase the whole line, cursor unmoved
					buf = buf[:0]
					fromScratch = true
				}
			default:
				exact = false
			}
		case r < 0x20:
			exact = false
		default:
			w := uniwidth.Width(r)
			if w <= 0 {
				// uniwidth.None is a C1 control or DEL: it draws nothing, and the
				// C1s a terminal acts on move the cursor the way an escape does.
				// Width 0 is a combining or zero-width rune, which composes into
				// the cell before it — a column buffer holding one rune per cell
				// cannot carry it, and dropping it would change the line's text.
				// Neither is a glyph this can place, so the render is not exact.
				exact = false
				break
			}
			// A grid cell holds one rune and at most one spacer, so vt.input
			// advances two columns for anything wider than one — U+17D8 is the
			// only rune uniwidth gives three — and this has to advance the same
			// number. Sharing the width table is not enough by itself: the two
			// also have to use its answer the same way, or they drift on exactly
			// the runes the table treats specially.
			w = min(w, 2)
			pad()
			for c := col; c < col+w; c++ {
				blank(c)
			}
			for c := col; c < col+w; c++ {
				cell := wideSpacer
				if c == col {
					cell = r
				}
				if c < len(buf) {
					buf[c] = cell
				} else {
					buf = append(buf, cell)
				}
			}
			col += w
		}
	}
	out := make([]rune, 0, len(buf))
	for _, r := range buf {
		if r != wideSpacer {
			out = append(out, r)
		}
	}
	return string(out), exact, fromScratch
}

// escKind is what an escape sequence does to the line being rendered.
type escKind int

const (
	// escUnknown moves the cursor or edits the screen, so where the line's text
	// ends up is not a function of the line.
	escUnknown escKind = iota
	// escHarmless changes colours, a terminal mode or the window title. It moves
	// no glyph, so the rendered text is the same with or without it — which is
	// what StripANSI has always assumed of every sequence.
	escHarmless
	// escEraseLine is CSI n K, the one editing sequence whose effect on a line is
	// a function of that line alone, and the one a prompt redraw relies on.
	escEraseLine
)

// classifyEscape returns the length and kind of the escape sequence at the start
// of s, and the erase mode when it is one. Length comes from ansiRegex, so this
// and StripANSI agree on where a sequence ends. An unterminated sequence consumes
// the ESC alone and reads as unknown.
func classifyEscape(s string) (n int, kind escKind, mode int) {
	loc := ansiRegex.FindStringIndex(s)
	if loc == nil || loc[0] != 0 {
		return 1, escUnknown, 0
	}
	n = loc[1]
	seq := s[:n]
	final := seq[n-1]
	if n < 2 {
		return n, escUnknown, 0
	}
	switch {
	case seq[1] == '[': // CSI
		if final == 'K' {
			switch seq[2 : n-1] {
			case "", "0":
				return n, escEraseLine, 0
			case "1":
				return n, escEraseLine, 1
			case "2":
				return n, escEraseLine, 2
			}
			return n, escUnknown, 0
		}
		// SGR: colours and attributes, no glyph moved. Everything else with a
		// different final — cursor moves, ED, IL, DL, DCH, scrolling regions —
		// lands the text somewhere this cannot follow.
		if final == 'm' {
			return n, escHarmless, 0
		}
		if final == 'h' || final == 'l' {
			if harmlessMode(seq[2 : n-1]) {
				return n, escHarmless, 0
			}
			return n, escUnknown, 0
		}
	case seq[1] == ']', seq[1] == 'P', seq[1] == 'X', seq[1] == '^', seq[1] == '_':
		return n, escHarmless, 0 // OSC, DCS, SOS, PM, APC: a string, not a glyph
	case seq[1] >= 0x28 && seq[1] <= 0x2f && seq[1] != 0x2c:
		// Charset designation, ESC ( B and friends: it selects which glyph a byte
		// draws, not where it is drawn. The rest of the intermediate range is
		// excluded because it is not all harmless — ESC # 8 (DECALN) overwrites
		// the screen and homes the cursor, and ESC # 3 through 6 make a line
		// double height or double width, so each glyph on it covers two columns.
		return n, escHarmless, 0
	case final == '=', final == '>':
		return n, escHarmless, 0 // keypad mode
	}
	return n, escUnknown, 0
}

// harmlessMode reports whether CSI params h or l — a mode set or reset, params
// being everything between the CSI and the final byte — leaves the rendered line
// a function of its own bytes.
//
// It is an allowlist because most modes are not harmless and the two that matter
// here look exactly like the ones that are. `CSI ?1049h` takes the alternate
// screen and `CSI ?7l` turns off autowrap; `CSI 4h` (IRM) makes every later glyph
// on the line shift the text to its right instead of overwriting it, and `CSI 20h`
// (LNM) changes what a bare CR does, which is the control the replay models most
// carefully. Treating all of h and l as harmless — which is what a check on the
// final byte alone does — would let the scan place a line it cannot place, and
// the scan may call an idle prompt busy but must never call a busy program idle.
//
// What is on the list is what a shell puts around a prompt (bracketed paste,
// cursor visibility and blink, application cursor keys) plus modes that only
// change what the terminal sends back. A list of several modes in one sequence is
// not accepted even when each would be: it is rare enough not to be worth
// parsing, and refusing is the safe direction.
//
// Which mode a shell writes there depends on its readline, and that is a platform
// difference rather than a preference: bash 5.2 brackets its prompt with ?2004,
// while bash 3.2 -- the /bin/bash and /bin/sh of every stock macOS -- has no
// bracketed paste and writes ?1034 instead. Measured on macOS 26.4 arm64, the raw
// history of a window one prompt old is "\x1b[?1034h$ " exactly. Leaving that mode
// off the list cost the whole injection feature there: the prompt line renders
// untrusted, IsIdleScan refuses rather than reading past it, and since a shell
// sitting at a prompt writes nothing more, the refusal never lifts and no payload
// is ever delivered. Five of the root package's PTY tests fail on it -- and they
// were the first tests ever run on darwin, so the platform, not the tests, is where
// this was broken.
func harmlessMode(params string) bool {
	switch params {
	case "?1", // DECCKM: application cursor keys, an input encoding
		"?12",   // cursor blink
		"?25",   // cursor visibility, ConPTY's repaint bracket
		"?1004", // focus reporting, an input event
		"?1034", // meta sets the eighth bit, an input encoding; bash 3.2's prompt bracket
		"?2004", // bracketed paste, bash 5.2's prompt bracket
		"?2026": // synchronized output
		return true
	}
	return false
}

// lastNonEmptyLine is the last line of s that has more than white space.
func lastNonEmptyLine(s string) string {
	for s != "" {
		i := strings.LastIndexByte(s, '\n')
		if line := s[i+1:]; strings.TrimFunc(line, unicode.IsSpace) != "" {
			return line
		}
		if i < 0 {
			break
		}
		s = s[:i]
	}
	return ""
}

// ScanLines is how many lines of history the detector reads, so that a caller
// gathering those lines gathers the same number the detector expects.
func (d *IdleDetector) ScanLines() int { return d.scanLines }

// Pattern is the compiled prompt pattern, for a caller that needs to match it
// against something other than history — ExtractPendingInput takes it. The
// *regexp.Regexp is shared, not copied, which is safe: it is immutable and its
// methods may be called concurrently.
func (d *IdleDetector) Pattern() *regexp.Regexp { return d.pattern }

// ansiRegex matches ECMA-48 escape sequences: CSI with any parameter bytes
// (including private markers such as ? and >), intermediates and final byte;
// OSC ended by BEL or ST; DCS, SOS, PM and APC strings ended by ST; and
// two-byte escapes (charset selection, keypad mode, DECSC/DECRC, RI). An
// unterminated string or CSI is left whole.
var ansiRegex = regexp.MustCompile(
	"\x1b\\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]" +
		"|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)" +
		"|\x1b[PX^_][^\x1b]*\x1b\\\\" +
		"|\x1b[\x20-\x2f]*[\x30-\x4f\x51-\x57\x59\x5a\\x5c\x60-\x7e]") // \x5c escaped for the regex, not Go

// StripANSI removes escape sequences, so a prompt that a shell or terminal
// surrounds with mode changes (bash's ESC[?2004h before each prompt,
// ConPTY's ESC[?25l … ESC[?25h around repaints) still reads as the prompt.
func StripANSI(s string) string { return ansiRegex.ReplaceAllString(s, "") }

// Unicode class bodies matching the Rust regex crate's \s (White_Space) and
// \w (word characters); Go's are ASCII-only.
const (
	rustSpaceClass = `\t\n\v\f\r\x{85}\p{Z}`
	rustWordClass  = `\p{L}\p{M}\p{Nd}\p{Nl}\p{Pc}\x{200C}\x{200D}`
)

// RustRegex rewrites \s and \w in a Rust regex into Go equivalents with
// Rust's Unicode semantics. The prompt patterns shelld uses need nothing
// else.
//
// Whether a \s is inside a bracket expression decides which form it becomes, so
// getting the bracket tracking wrong silently changes the language the pattern
// matches. A POSIX class is the case that got it wrong: `[[:alpha:]\s]` has three
// '[' and ']' between its outer brackets, and a flag toggled by each of them was
// back to "outside" by the time it reached the \s, which then became the bracketed
// form nested inside a class it believed it had left. The result still compiles and
// matches something else — for an idle pattern, a prompt detector that fires on
// output lines, or never fires at all.
//
// So a '[' that opens a POSIX class is copied whole, terminator included, and an
// inner '[' that opens nothing is a literal that does not nest, which is how Go's
// own parser reads it and Go is what compiles the output. Escaped brackets never
// reach this switch — the branch above consumes both bytes.
//
// Two rules of the bracket language beyond that, both read off regex-syntax 0.8.11,
// the parser behind the patterns this translates:
//
//   - A ']' in the first position of a body is a literal ']' and does not close
//     ("If `]` is the *first* char in a set, then interpret it as a literal `]`",
//     ast/parse.rs:2059). Go agrees, so this is tracked below rather than treated
//     as a dialect difference.
//   - Rust's classes nest: an inner '[' that is not a POSIX class pushes a new set
//     (push_class_open, parse.rs:1893) that the next ']' pops (pop_class, :1899),
//     and '&&', '--' and '~~' between sets are intersection, difference and
//     symmetric difference (:1907-1925). Go has none of this, so there is nothing
//     to translate a set operation into and the flat reading above is what the
//     output gets compiled by. A prompt pattern using a nested set or an operator
//     is therefore outside what this function handles; it is not what the shells
//     here produce, and the alternative to a flat reading is not a better
//     translation but an error return this signature does not have.
func RustRegex(p string) string {
	var b strings.Builder
	inClass := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '\\' && i+1 < len(p) {
			next := p[i+1]
			i++
			switch {
			case next == 's' && inClass:
				b.WriteString(rustSpaceClass)
			case next == 's':
				b.WriteString("[" + rustSpaceClass + "]")
			case next == 'w' && inClass:
				b.WriteString(rustWordClass)
			case next == 'w':
				b.WriteString("[" + rustWordClass + "]")
			default:
				b.WriteByte('\\')
				b.WriteByte(next)
			}
			continue
		}
		switch c {
		case '[':
			if inClass {
				if end := posixClassEnd(p, i); end > i {
					b.WriteString(p[i:end])
					i = end - 1
					continue
				}
				break // a literal '[' inside a class: no nesting to track
			}
			inClass = true
			// A ']' in the first position of the body is a literal ']', so the
			// class it looks like it closes is still open. Copy it with the
			// bracket that opened it and stay inside. A leading '-' cancels the
			// rule: Rust accepts a run of '-' as literals before reaching it
			// (parse.rs:2046), which leaves the set non-empty, so the ']' after
			// one really is the terminator. Nothing here consumes a '-'.
			if j := i + 1; j < len(p) {
				if p[j] == '^' {
					j++
				}
				if j < len(p) && p[j] == ']' {
					b.WriteString(p[i : j+1])
					i = j
					continue
				}
			}
		case ']':
			inClass = false
		}
		b.WriteByte(c)
	}
	return b.String()
}

// posixClassEnd returns the index just past the POSIX class token at i
// ("[:alpha:]", "[:^alpha:]"), or i if there is none there. The name is not
// validated: an unknown one is Go's regexp to reject, and passing it through
// unchanged is what makes that error the user's pattern rather than this rewrite.
func posixClassEnd(p string, i int) int {
	if !strings.HasPrefix(p[i:], "[:") {
		return i
	}
	end := strings.Index(p[i+2:], ":]")
	if end < 0 {
		return i
	}
	return i + 2 + end + 2
}

// StashState holds a line a user was part way through typing aside while an
// injected message is delivered, accumulating anything typed meanwhile.
//
// It exists because an injection types a whole line and presses Enter, which
// would submit whatever the user had already typed along with it. The intended
// cycle is: read the partial line, Begin with it, clear the line, deliver the
// injection, then Complete and type the result back — the stashed text with
// everything typed during the delivery appended, in the order it arrived.
//
// Only half of that cycle is wired up, and this is the sharp version: Begin has
// no non-test caller in this module, so no stash is ever active in production, so
// the divert that Window.SendKeys, Window.SendRawKey, Window.SendKeyNames and
// Window.WriteInput reach — three branches behind those four methods, since the
// last two share Window.typeInput — never fires.
//
// TestStash covers this type's own lifecycle on a standalone StashState: an
// accumulate discarded because no stash is open, a Begin, two more accumulates,
// and a Complete that returns the stashed text with both appended, with IsActive
// asserted on either side of it. That is the type in isolation, which cannot tell a
// Window that diverts from one that writes through.
//
// The divert itself is covered as well, by TestStashDivertsEveryTypingPath: it Begins
// on a real Window's stash, calls all four typing methods, and asserts both that none
// of their bytes reached the program and that Complete hands them back in arrival
// order. All three branches are taken there, since those four methods reach exactly
// those three. TestStashRaceLosesNoKeystroke and its control Begin on a Window's
// stash too, 5000 times each, against a Complete racing the write.
// Window.Paste deliberately does not divert, stash or no stash.
//
// It is exposed through Window.Stash for a caller that wants to drive the cycle
// itself, and left in place because shelld-rs has the same half: removing it here
// would be a divergence between the two daemons argued from a docs pass.
//
// A stash has no timers, so a Begin that is never Completed swallows the user's
// input indefinitely: whoever calls Begin owns calling Complete. All of its
// methods are safe to call from several goroutines.
type StashState struct {
	mu          sync.Mutex
	active      bool
	stashedText string
	inputDuring []byte
}

// IsActive reports whether a stash is open as of the moment it returns, and not for
// any moment after that, so it cannot be the check a divert turns on: a Complete
// landing between the answer and the append is the race accumulateIfActive exists to
// close. The input paths call that instead. The one IsActive left on an input path,
// in window.go's SendKeys, keeps an allocation off the common path when no stash is
// open; the accumulateIfActive under it is what decides. It is not only an
// allocation, though, since it is a concurrency check: a Begin landing just after it
// answers false gets the keystroke typed, where without the guard the same Begin
// would have stashed it. Both are legitimate under this type's contract, which is why
// the guard is safe -- not because the answer does not matter.
//
// It remains the right call for a caller that wants the state itself rather than a
// branch on it -- a test asserting that a stash opened, or a status reader.
func (s *StashState) IsActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// Begin opens a stash holding currentText, the line the user had typed so far,
// and starts accumulating. Beginning while a stash is already open replaces the
// held text and drops whatever had accumulated: a stash is one line, not a
// stack.
func (s *StashState) Begin(currentText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = true
	s.stashedText = currentText
	s.inputDuring = s.inputDuring[:0]
}

// AccumulateInput appends data to what will be typed back, or discards it if no
// stash is open -- and returns nothing either way, so a caller cannot tell which
// happened. That is the whole reason no input path uses it: bytes an input path hands
// over and cannot account for are bytes the user typed and will not see again. Those
// paths call accumulateIfActive, whose bool is the accounting.
//
// What is left for this method is a caller holding a StashState whose Begin and
// Complete it issues itself, in one goroutine, where "no stash is open" is a state it
// can rule out by construction rather than a race it has to survive. core_test.go is
// the only such caller in the tree; it stays exported because an embedder driving its
// own stash cycle is the case it is for.
//
// One deliberate exception, so that a reader grepping the callers is not misled by the
// paragraph above: stash_race_test.go's TestTheTwoStepShapeLosesKeystrokes calls it
// concurrently on purpose, to reproduce the defect this method's shape used to cause
// and prove the test harness can still see it. That caller wants the silent discard.
//
// Prefer accumulateIfActive for anything concurrent. If this ever needs to be usable
// there, it needs a return value, not a comment.
//
// The bytes are kept raw and decoded once in Complete, so a multi-byte rune
// split across two writes survives.
func (s *StashState) AccumulateInput(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		s.inputDuring = append(s.inputDuring, data...)
	}
}

// accumulateIfActive appends data to what will be typed back and reports whether
// it did. A false means no stash is open and the caller still owns the bytes, so
// every caller must fall through to typing them.
//
// One acquisition, and that is the point. Asking IsActive and then handing the
// bytes over as two steps decides the branch under one answer and appends under
// another: a Complete landing in between closes the stash, the append is
// discarded, and the caller -- having already committed to the stash's side of
// the branch -- returns success for a keystroke that was never typed and is no
// longer anywhere. The bytes would have reached the program had the check run a
// moment later, so the race turns a delivered keystroke into a silently dropped
// one. Answering from inside the lock makes the answer the caller acts on the
// same answer the append was made under.
//
// What a false does not promise is a position. A caller that falls through pushes
// onto the input queue, which is FIFO, and a driver replaying a just-Completed stash
// pushes onto the same queue; nothing orders those two pushes against each other. So
// a byte that loses this race can land ahead of the replayed line -- stash holding
// "half-typed", a racing "d", and the program reads "dhalf-typed". That is not new:
// the two-step version reached the same reorder whenever its IsActive returned false.
// What changed is that rounds which used to be dropped are now reorderable, which is
// the better end of the trade, because a keystroke in the wrong place is visible and
// recoverable and a missing one is neither. Anything needing the replay and the
// fall-through ordered has to order them itself; this method cannot.
//
// If a byte budget is ever added here, a refusal must not share this false: an
// inactive stash means "type it yourself", a full one means "this failed", and
// collapsing them rebuilds the dropped-keystroke branch inside the lock.
func (s *StashState) accumulateIfActive(data []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return false
	}
	s.inputDuring = append(s.inputDuring, data...)
	return true
}

// Complete closes the stash and returns what should be typed back: the text Begin
// was given, followed by everything accumulated since, whether that arrived through
// accumulateIfActive (every input path) or AccumulateInput (a caller driving its own
// cycle). The stash is emptied, so a second Complete returns "" — the caller has the
// only copy and must not drop it.
//
// A keystroke racing this method can only be stashed or typed, never both and never
// neither, and the reason is that this method and accumulateIfActive both do all of
// their work under s.mu: either the append lands first and the drain below returns
// the byte, or this method lands first and accumulateIfActive reports false so its
// caller types the byte itself. Clearing active and draining inputDuring in the same
// critical section is what makes those the only two outcomes.
//
// It also closes the stash unconditionally, including when it was already closed.
// That is not what guards the property above -- an early return when !active would
// leave it intact, since inputDuring is empty whenever active is false. What relies
// on it is a test: stash_race_test.go asserts the stash is closed after each round.
//
// The accumulated bytes are decoded here, once, so a rune split across two
// writes is one character; bytes that are not valid UTF-8 become U+FFFD rather
// than being passed through, since the result is typed at a program as text.
func (s *StashState) Complete() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.stashedText + utf8x.Lossy(s.inputDuring)
	s.stashedText = ""
	s.inputDuring = s.inputDuring[:0]
	s.active = false
	return out
}

// ExtractPendingInput returns what follows the idle prompt on lastLine.
func ExtractPendingInput(lastLine string, idlePattern *regexp.Regexp) string {
	stripped := StripANSI(lastLine)
	loc := idlePattern.FindStringIndex(stripped)
	if loc == nil {
		return ""
	}
	return strings.TrimSpace(stripped[loc[1]:])
}
