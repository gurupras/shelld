package shelld

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gurupras/shelld/internal/tracing"
	"github.com/gurupras/shelld/pty"
)

// ErrInputQueueFull is returned by input calls when the bytes would exceed
// the window's input queue limit or the shared InputBudget. The program is
// not reading its input fast enough (or at all); retrying doesn't help until
// it does. tmux queues without a limit; shelld bounds the memory a client
// can pin in a wedged window.
var ErrInputQueueFull = errors.New("input queue full")

// ErrInputTooLarge is the full queue that waiting cannot clear: the write is
// larger than the whole of the window's input queue limit, or of the budget
// windows share, so no state of either has room for it. The two have to be told
// apart by any caller that retries a full queue, because the answer to one is to
// wait for the program to read and the answer to the other is to send less: the
// injection drain loop requeues on ErrInputQueueFull, and for these bytes that is
// not a retry but a loop with no exit.
//
// It wraps ErrInputQueueFull, so a caller that only asks whether the input was
// accepted reads it exactly as before, and that wrapping is what kept telling the
// two apart a change to one function rather than to every caller. The cost of the
// wrapping is that it makes the ORDER of a caller's errors.Is calls load-bearing: a
// too-large write satisfies both tests, so whichever is asked first wins, and
// asking the general one first silently answers the specific case wrongly. Both
// callers that care now ask this one first -- writeInputError, which would
// otherwise answer 429 and invite a retry no state of the queue can satisfy, and
// drainLoop, which would otherwise requeue and spin. A third caller that treats
// the two alike will not fail to compile; it will just be wrong in the direction
// of retrying for ever.
var ErrInputTooLarge = fmt.Errorf("%w: larger than the whole input queue", ErrInputQueueFull)

// InputTooLargeError is the ErrInputTooLarge push returns, carrying the two
// numbers a client needs to send less: what it asked for and the most the queue
// can ever hold. Its text is shelld-rs's InputTooLarge (shelld-core input.rs),
// which the 413 body carries on both daemons. It unwraps to ErrInputTooLarge, so
// every errors.Is above still answers as it did.
type InputTooLargeError struct {
	// Wanted is the bytes the push asked for, every chunk of it: text and the
	// Enter that submits it are one push.
	Wanted int
	// Limit is the window's queue limit, or the shared budget's where smaller.
	Limit int64
}

func (e *InputTooLargeError) Error() string {
	return fmt.Sprintf("input of %d bytes exceeds the %d-byte input queue", e.Wanted, e.Limit)
}

func (e *InputTooLargeError) Unwrap() error { return ErrInputTooLarge }

const (
	// DefaultInputQueueLimit is a window's input queue limit when
	// WindowOpts.InputQueueLimit is 0.
	DefaultInputQueueLimit = 16 << 20
	// DefaultInputBudget is the limit of the budget windows share when
	// WindowOpts.InputBudget is nil.
	DefaultInputBudget = 256 << 20
	// maxReplyBacklog bounds queued terminal replies (OSC color answers). A
	// program that asks and doesn't read gets the rest dropped.
	maxReplyBacklog = 64 << 10
)

// MaxInputGap is the longest gap a window holds its input for before one
// chunk, shelld-rs's MAX_INPUT_GAP (shelld-core window.rs:896). The writer
// waits gaps out in order, so a longer one is not a slow submit but a window
// whose input has stopped. The daemon refuses a longer enter_delay_ms; the
// window clamps what reaches it from a library caller or SendKeysDelayMS.
const MaxInputGap = 60 * time.Second

// MaxQueuedGap bounds the pacing queued on one window at once, shelld-rs's
// MAX_QUEUED_GAP (shelld-core input.rs:81). The per-chunk bound is not enough
// on its own and the byte bound cannot help, since a gap-carrying Enter is one
// byte: a hundred requests each queueing a minute-long gap leave the window's
// input, the Ctrl-C a client would interrupt with included, stopped for a
// hundred minutes. Twice MaxInputGap, so one gap can be waited with the next
// queued behind it.
const MaxQueuedGap = 2 * MaxInputGap

// InputBudget bounds the bytes queued for terminals across every window that
// shares it.
type InputBudget struct {
	limit int64
	used  atomic.Int64
}

// NewInputBudget returns a budget of limit bytes.
func NewInputBudget(limit int64) *InputBudget { return &InputBudget{limit: limit} }

var defaultInputBudget = NewInputBudget(DefaultInputBudget)

// reserve claims n bytes of the budget, reporting whether there was room.
//
// The claim is a compare-and-swap rather than an add-then-back-out because a
// failing add publishes used+n to every other window sharing the budget before
// undoing it, and a reserve that samples the budget inside that window refuses
// input the budget had room for. The CAS never publishes a total the budget did
// not hold, so a concurrent reserve only ever sees committed bytes.
//
// The bug that fixes is a false refusal and never over-admission: Add returns
// its own post-add value, so a set of reserves whose sum fits cannot push each
// other over the limit, and the poisoning n has to come from a reserve that
// fails. It therefore needs the budget to be near its limit already — with the
// defaults, most of 256 MB pinned across wedged windows — but a caller that
// shares a smaller budget reaches it with much less queued.
//
// used+n cannot overflow, and the reason is that both terms are bytes that
// exist: n is the length of a slice a caller allocated, and the CAS only ever
// commits a total the budget had room for, so used is the sum of the lengths of
// the slices currently queued. Reaching math.MaxInt64 would need more bytes
// queued than the machine can address. Bounding n on its own would not settle
// it — a limit near math.MaxInt64 with used already near it overflows for any n
// at all — so the argument has to be about what used can hold.
//
// The cost per call, measured against the add it replaced (see the three
// InputBudget benchmarks):
//
//   - One window, budget far from its limit: 3.4 ns against 2.9, so 0.4 ns
//     dearer. That is under 1% of the ~55 ns push/next/done round trip the call
//     sits inside, and below that arm's own run-to-run spread.
//   - Many windows contending for one budget with room to spare: 59 ns against
//     32, about 1.9x, because the CAS retries where the add cannot.
//   - Budget at its limit, so every reserve fails: 0.08 ns against 32, about
//     300x cheaper. This is the regime the false refusal lives in, and the CAS
//     writes nothing in it, so the cache line stays shared across every core
//     instead of being taken for exclusive access twice per failing call.
//
// The one arm where the fix costs something end to end is 24 windows over one
// budget each pushing as fast as it can: 62 ns/op against the add's 47
// (BenchmarkInputQueueKeystrokeRoundTripSharedBudget). That arm is 16 million
// queued writes a second across the box, and real input is keystrokes and
// pastes, a paste being one push of many bytes rather than many pushes. None of
// the three is worth trading the exactness of the bound for.
func (b *InputBudget) reserve(n int64) bool {
	for {
		used := b.used.Load()
		if used+n > b.limit {
			return false
		}
		// A lost CAS means another window committed or released in between, so
		// the retry re-tests against a total the budget really held.
		if b.used.CompareAndSwap(used, used+n) {
			return true
		}
	}
}

func (b *InputBudget) release(n int64) { b.used.Add(-n) }

// inputChunk is one write to the terminal.
type inputChunk struct {
	data []byte
	// gap is the least time between the end of the previous write and this
	// one: Enter after text, so a TUI's paste-burst detection sees a submit.
	gap   time.Duration
	reply bool
}

// inputQueue holds a window's input in arrival order. Its writer goroutine
// is the only caller of PTY.Write, and it holds no window lock while writing,
// so a program that stops reading blocks nothing but the writer; Kill and EOF
// release it by closing the master.
type inputQueue struct {
	limit  int
	budget *InputBudget

	mu      sync.Mutex
	chunks  []inputChunk
	bytes   int           // queued and in flight, until Write returns
	gaps    time.Duration // the gaps of the same chunks
	replies int           // of which terminal replies
	dropped uint64
	closed  bool
	err     error         // why input fails once closed
	wake    chan struct{} // capacity 1: chunks were added
	stop    chan struct{} // closed with the queue
}

func newInputQueue(limit int, budget *InputBudget) *inputQueue {
	if limit <= 0 {
		limit = DefaultInputQueueLimit
	}
	if budget == nil {
		budget = defaultInputBudget
	}
	return &inputQueue{limit: limit, budget: budget, wake: make(chan struct{}, 1), stop: make(chan struct{})}
}

// push queues chunks as one unit: all of them or, past a limit, none. Empty
// chunks without a gap are skipped.
func (q *inputQueue) push(chunks ...inputChunk) error {
	n := 0
	var gaps time.Duration
	for _, c := range chunks {
		n += len(c.data)
		gaps += c.gap
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return q.err
	}
	// Ahead of the room test rather than folded into it: a write this big fails
	// the room test on an empty queue against an idle budget too, so without this
	// the one failure no amount of draining fixes is reported as the one that
	// clears itself.
	if limit := min(int64(q.limit), q.budget.limit); int64(n) > limit {
		return &InputTooLargeError{Wanted: n, Limit: limit}
	}
	// Refused as a full queue, because it clears the same way: the total falls
	// as the writer waits out the gaps ahead.
	if q.gaps+gaps > MaxQueuedGap {
		return ErrInputQueueFull
	}
	if q.bytes+n > q.limit || !q.budget.reserve(int64(n)) {
		return ErrInputQueueFull
	}
	for _, c := range chunks {
		if len(c.data) > 0 || c.gap > 0 {
			q.chunks = append(q.chunks, c)
		}
	}
	q.bytes += n
	q.gaps += gaps
	q.signal()
	return nil
}

// pushReply queues a terminal reply, or drops it when the program already
// has maxReplyBacklog of replies unread or a limit is reached. It never
// blocks readLoop.
func (q *inputQueue) pushReply(data []byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	n := len(data)
	if q.replies+n > maxReplyBacklog || q.bytes+n > q.limit || !q.budget.reserve(int64(n)) {
		q.dropped++
		if q.dropped == 1 {
			slog.Debug("dropping terminal replies: the program isn't reading its input")
		}
		return
	}
	q.chunks = append(q.chunks, inputChunk{data: data, reply: true})
	q.bytes += n
	q.replies += n
	q.signal()
}

func (q *inputQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// pending reports whether input is queued or being written.
func (q *inputQueue) pending() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.bytes > 0
}

// next waits for the next chunk; ok is false once the queue is closed.
func (q *inputQueue) next() (c inputChunk, ok bool) {
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return inputChunk{}, false
		}
		if len(q.chunks) > 0 {
			c = q.chunks[0]
			q.chunks[0] = inputChunk{}
			q.chunks = q.chunks[1:]
			q.mu.Unlock()
			return c, true
		}
		q.mu.Unlock()
		select {
		case <-q.wake:
		case <-q.stop:
		}
	}
}

// done accounts for a chunk whose write returned.
func (q *inputQueue) done(c inputChunk) {
	q.mu.Lock()
	q.bytes -= len(c.data)
	q.gaps -= c.gap
	if c.reply {
		q.replies -= len(c.data)
	}
	q.mu.Unlock()
	q.budget.release(int64(len(c.data)))
}

// close drops queued input and makes later input fail with err. A chunk in
// flight stays counted until its write returns. Idempotent.
func (q *inputQueue) close(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	queued := 0
	for _, c := range q.chunks {
		queued += len(c.data)
		q.gaps -= c.gap
		if c.reply {
			q.replies -= len(c.data)
		}
	}
	q.chunks = nil
	q.bytes -= queued
	q.budget.release(int64(queued))
	q.closed, q.err = true, err
	close(q.stop)
}

// writeInput is the window's writer. It writes chunks in order, waiting out
// each chunk's gap, until the queue closes or a write fails.
//
// The master must stay non-blocking (registered with the runtime poller, as
// os.File and pollFile are): closing it is what releases a Write parked on a
// program that isn't reading. A blocking write(2) is released by nothing, so
// Kill and EOF could no longer stop the writer.
func (w *Window) writeInput(p *pty.PTY) {
	q := w.input
	var lastEnd time.Time
	for {
		c, ok := q.next()
		if !ok {
			return
		}
		if wait := c.gap - time.Since(lastEnd); c.gap > 0 && wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-t.C:
			case <-q.stop:
				t.Stop()
				q.done(c)
				return
			}
		}
		var err error
		if len(c.data) > 0 {
			var span uint64
			if tracing.Enabled {
				span = tracing.Begin(tracing.KindPTYWrite, w.traceID, int64(len(c.data)))
			}
			_, err = p.Write(c.data)
			lastEnd = time.Now()
			if tracing.Enabled {
				tracing.End(tracing.KindPTYWrite, w.traceID, span, int64(len(c.data)), 0)
			}
		}
		q.done(c)
		if err != nil {
			q.close(w.deadErr())
			return
		}
	}
}
