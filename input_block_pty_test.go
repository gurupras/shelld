//go:build linux || darwin

package shelld

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gurupras/shelld/vt"
)

// releaseOnFailure guards a test that could leave a writer blocked on w's
// terminal, or spinning a core once the program exits, if input blocking
// regresses. When the test fails, or is still running at the earlier of 60 s
// and 5 s before the -timeout deadline, it closes w's master directly (no
// Kill, no ptyMu), which is the documented thing that releases such a writer.
//
// Closing the master is the whole remedy; taking the binary out is only the
// fallback for when it does not work. It has to be, because os.Exit from a
// cleanup discards the test's buffered output: a red then reports FAIL for the
// package with no --- FAIL: line and no sign of which assertion failed. So the
// two paths each close the master, wait for the queue to drain, and exit only
// if it does not — an ordinary red reports itself, and a writer that a closed
// master cannot release still cannot outlive the run. Exiting skips later
// cleanups: run these tests with TMPDIR and GOTMPDIR in a scratch directory.
func releaseOnFailure(t *testing.T, w *Window) {
	w.ptyMu.Lock()
	p := w.pty
	w.ptyMu.Unlock()
	limit := 60 * time.Second
	if d, ok := t.Deadline(); ok {
		limit = max(min(limit, time.Until(d)-5*time.Second), time.Second)
	}
	name := t.Name()
	release := func() {
		if p != nil {
			_ = p.Close()
		}
	}
	// released reports whether the queue emptied within d. close drops what is
	// merely queued but leaves a chunk whose write has not returned counted, so
	// an empty queue is precisely the statement that no goroutine is parked on
	// the terminal and none is spinning on a master that reports ready while
	// refusing writes.
	released := func(d time.Duration) bool {
		for deadline := time.Now().Add(d); ; time.Sleep(10 * time.Millisecond) {
			if !w.input.pending() {
				return true
			}
			if time.Now().After(deadline) {
				return false
			}
		}
	}
	watchdog := time.AfterFunc(limit, func() {
		release()
		// Whether to exit is the same question on both paths, and it is asked of the
		// queue rather than of the test: a drained queue says the remedy worked, so
		// the test is left to report its own failure however long it takes, and a
		// queue that will not drain says it did not, which is the only thing exiting
		// is for. Waiting on the test to return instead would tie this to cleanup
		// order -- the caller registers Kill before this and cleanups run last in
		// first out, so the signal would arrive while a hanging Kill still held the
		// run, disarming the guard exactly where it is needed.
		fmt.Fprintf(os.Stderr, "%s still running after %s; closed its terminal\n", name, limit)
		if !released(10 * time.Second) {
			fmt.Fprintf(os.Stderr, "%s did not release its terminal; exiting so no writer stays blocked or spinning\n", name)
			os.Exit(2)
		}
	})
	t.Cleanup(func() {
		watchdog.Stop()
		if !t.Failed() {
			return
		}
		release()
		if !released(5 * time.Second) {
			fmt.Fprintf(os.Stderr, "%s failed and its input queue did not drain once the terminal closed; exiting so no writer stays blocked or spinning\n", name)
			os.Exit(1)
		}
	})
}

// within runs f in a goroutine and reports whether it returned within d. A
// goroutine still blocked when the test fails is released by releaseOnFailure,
// which closes the terminal rather than killing the program: killing it leaves
// the master reporting ready while refusing writes, which turns a parked writer
// into a spinning one, and releaseOnFailure's own comment on `released` is where
// that distinction is spelled out.
func within(d time.Duration, f func()) bool {
	done := make(chan struct{})
	go func() {
		f()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// A paste larger than the terminal's input queue, into a program that never
// reads its input, returns promptly, and the window can still be killed.
// tmux 3.7c returns paste-buffer in 17 ms and kill-window in 4 ms here.
func TestPasteIntoProgramNotReadingDoesNotBlock(t *testing.T) {
	w, err := NewWindow(WindowOpts{
		Name:    "noread",
		Command: []string{"/bin/sh", "-c", "stty raw -echo; printf ready; exec sleep 1000"},
		Cols:    80, Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	releaseOnFailure(t, w)
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(w.CaptureVisible(0), "ready")
	}, "program never started")

	text := strings.Repeat("0123456789abcdef", 100_000) // 1.6 MiB
	var pasteErr error
	if !within(2*time.Second, func() { pasteErr = w.PasteWith(text, PasteOpts{Mode: "raw"}) }) {
		t.Fatal("paste into a program that doesn't read blocked for 2 s")
	}
	if pasteErr != nil {
		t.Fatal(pasteErr)
	}
	if !within(2*time.Second, func() { _ = w.SendRawKey("\x03") }) {
		t.Fatal("a key after the paste blocked for 2 s")
	}
	if !within(2*time.Second, w.Kill) {
		t.Fatal("Kill blocked for 2 s behind the paste")
	}
}

// With colors set, a program in raw mode that prints OSC 11 queries and never
// reads its input fills the terminal's input queue with shelld's replies.
// Output must keep flowing and the window must stay killable.
func TestColorRepliesToProgramNotReadingDoNotBlock(t *testing.T) {
	queries := filepath.Join(t.TempDir(), "queries")
	var b strings.Builder
	for i := 0; i < 20_000; i++ {
		b.WriteString("\x1b]11;?\x07line\r\n")
	}
	b.WriteString("all-queries-done\r\n")
	if err := os.WriteFile(queries, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := NewWindow(WindowOpts{
		Name:    "replies",
		Command: []string{"/bin/sh", "-c", `stty raw -echo; printf ready; sleep 0.2; cat "$0"; exec sleep 1000`, queries},
		Cols:    80, Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	releaseOnFailure(t, w)
	w.SetColors(map[int]vt.RGB{IdxBackground: {R: 0x10, G: 0x10, B: 0x10}})
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(w.CaptureVisible(0), "ready")
	}, "program never started")

	deadline := time.Now().Add(10 * time.Second)
	for {
		if out, _ := w.Capture(3); strings.Contains(out, "all-queries-done") {
			break
		}
		if time.Now().After(deadline) {
			_, total := w.Capture(1)
			t.Fatalf("output stopped after %d lines: reading blocked on replies", total)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !within(2*time.Second, w.Kill) {
		t.Fatal("Kill blocked for 2 s")
	}
}

// A program that exits while a paste to it is still pending: the pending
// write must not spin. On Linux, once the terminal's slave side closes the
// master reports ready (EPOLLHUP) while writes keep failing with EAGAIN, so a
// write left pending busy-loops until the master is closed.
func TestPendingWriteToExitedProgramDoesNotSpin(t *testing.T) {
	w, err := NewWindow(WindowOpts{
		Name:    "exits",
		Command: []string{"/bin/sh", "-c", "stty raw -echo; printf ready; sleep 1"},
		Cols:    80, Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	releaseOnFailure(t, w)
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(w.CaptureVisible(0), "ready")
	}, "program never started")
	go func() { _ = w.PasteWith(strings.Repeat("0123456789abcdef", 100_000), PasteOpts{Mode: "raw"}) }()
	select {
	case <-w.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("program never exited")
	}
	time.Sleep(200 * time.Millisecond)
	before := processCPU()
	time.Sleep(time.Second)
	if spent := processCPU() - before; spent > 200*time.Millisecond {
		t.Fatalf("%v of CPU in the second after the program exited: a pending write is spinning", spent)
	}
	if !within(2*time.Second, w.Kill) {
		t.Fatal("Kill blocked for 2 s")
	}
}

func processCPU() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// TestHelperInputRecorder is not a test: run as a window's program with
// SHELLD_TEST_HELPER=record-input, it prints "ready" and appends every byte
// it reads to the file SHELLD_TEST_INPUT_FILE. With SHELLD_TEST_SLOW_READ
// set it reads 4 KiB every ~2 ms, like a busy TUI, and when a read ends
// with CR it prints "cr-gap-ms=<ms since the previous read returned>".
func TestHelperInputRecorder(t *testing.T) {
	if os.Getenv("SHELLD_TEST_HELPER") != "record-input" {
		return
	}
	f, err := os.OpenFile(os.Getenv("SHELLD_TEST_INPUT_FILE"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		os.Exit(1)
	}
	slow := os.Getenv("SHELLD_TEST_SLOW_READ") != ""
	os.Stdout.WriteString("ready\r\n")
	buf := make([]byte, 4096)
	last := time.Now()
	for {
		n, err := os.Stdin.Read(buf)
		now := time.Now()
		if n > 0 {
			_, _ = f.Write(buf[:n])
			if slow && buf[n-1] == '\r' {
				os.Stdout.WriteString("cr-gap-ms=" + strconv.FormatInt(now.Sub(last).Milliseconds(), 10) + "\r\n")
			}
		}
		if err != nil {
			os.Exit(0)
		}
		last = now
		if slow {
			time.Sleep(2 * time.Millisecond)
		}
	}
}

// startRecorder runs TestHelperInputRecorder in raw mode (no line
// discipline limits, CR translation or echo) and returns the window and the
// file it records into.
func startRecorder(t *testing.T, slow bool, opts WindowOpts) (*Window, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "input")
	opts.Name = "recorder"
	opts.Command = []string{"/bin/sh", "-c", `stty raw -echo && exec "$0" -test.run='^TestHelperInputRecorder$'`, exe}
	opts.Env = append(opts.Env, [2]string{"SHELLD_TEST_HELPER", "record-input"}, [2]string{"SHELLD_TEST_INPUT_FILE", file})
	if slow {
		opts.Env = append(opts.Env, [2]string{"SHELLD_TEST_SLOW_READ", "1"})
	}
	opts.Cols, opts.Rows = 80, 24
	w, err := NewWindow(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Kill)
	releaseOnFailure(t, w) // runs before Kill
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(w.CaptureVisible(0), "ready")
	}, "recorder never started")
	return w, file
}

// notReadingMark is the byte sequence startNotReading waits for, emitted by
// startNotReading itself between the stty and the caller's script.
//
// Waiting for a marker of its own rather than for "the script produced visible
// output" is the second attempt, and the first one was a bug this helper
// introduced into an unrelated test. TestReplyBacklogDropped's script writes
// nothing but OSC queries until it is done, and those render no glyphs, so a wait
// for non-empty output sat there until `replies-done` — i.e. until every query had
// already been answered or dropped — and the SetColors the test makes immediately
// after this returns then landed after the queries it was meant to enable replies
// for. dropped was 0 and the test failed 6 times in 6 standing alone, while
// passing in the full suite on timing, which is the worst shape a test failure has.
//
// It is on its own line because one caller's script prints the ❯ that
// IdleDetector's default pattern matches as `(?m)^\s*❯\s*$` — a marker sharing
// that line would stop the line matching, so the barrier would have broken a
// second test in the act of fixing the first.
const notReadingMark = "rawready"

// startNotReading runs a raw-mode program that never reads its input.
//
// It returns only once the program has written notReadingMark. What the wait is
// for is the raw mode rather than the script: `stty raw -echo` runs ahead of the
// marker, so the marker's arrival is evidence the ioctl has already taken effect,
// and until it has, input goes to a terminal in CANONICAL mode, where a queue that
// fills does not hold.
//
// The marker is emitted here rather than required of each caller because a caller
// that forgets does not fail — it waits for something else and returns at the
// wrong moment, which is what happened above.
//
// Sending before that is a race the two platforms lose differently, which is how
// this was found. Linux blocks the writer when a canonical queue fills, so input
// piles up in the window's queue either way and the tests passed; darwin DISCARDS
// past the canonical queue's limit, so the write completes, the queue drains, and
// the limit under test is never reached. Measured on macOS 26.4 arm64 with the
// two 400 KB + 300 KB sends TestInputBudgetShared makes: 700,000 bytes charged to
// the budget, then 0 within 50ms. With this wait in place they stay at 700,000 for
// as long as the sampling ran. The tests this fixed -- TestInputQueueLimit and
// TestInputBudgetShared -- were both asserting against a queue that had silently
// emptied, so on darwin they were testing nothing even before they failed.
func startNotReading(t *testing.T, opts WindowOpts, script string) *Window {
	t.Helper()
	opts.Command = []string{"/bin/sh", "-c",
		`stty raw -echo; printf '` + notReadingMark + `\r\n'; ` + script + "; exec sleep 1000"}
	if opts.Name == "" {
		opts.Name = "noread"
	}
	opts.Cols, opts.Rows = 80, 24
	w, err := NewWindow(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Kill)
	releaseOnFailure(t, w) // runs before Kill
	waitFor(t, 5*time.Second, func() bool {
		out, _ := w.Capture(5)
		return strings.Contains(out, notReadingMark)
	}, "the program never printed "+notReadingMark+", so nothing establishes that it reached raw mode")
	return w
}

// Enter keeps its delay after text larger than the terminal's buffer: it is
// written at least the delay after the text's write finishes, not after the
// text was queued, so a slow reader still sees a pause before CR.
//
// The measurement is the queue's and not the recorder's, because the recorder can
// only time the gap between two of its own reads, and a read sees a byte some time
// after the write that delivered it returned -- the terminal buffers it. So the gap
// it reports undershoots the contract by however far the reader is lagging: the
// same 100 ms delay reads as 89 to 94 ms here, and the shortfall grows as the
// reader gets slower, which is the wrong thing for a bound to depend on. The queue
// straddles exactly the interval writeInput times from: its byte count falls to the
// CR's single byte when the text's write returns, and to zero when the CR's does.
// Exactly one byte, not approximately: SendKeys pushes the text and the CR as two
// chunks, and the count only ever loses a whole chunk, once, after that chunk's
// write returns, so no partial write can surface here. What that leans on outside
// the queue is that nothing arrives from the other direction, since a terminal reply
// is pushed onto the same count by the read loop -- the recorder sends no queries,
// which is a property of the fixture's program and not of the queue.
//
// Sizing the text so its own write outlasts the delay is what makes the bound
// discriminate rather than merely hold. A delay timed from the enqueue would be
// spent before the writer ever reached the CR, so the CR would follow the text with
// no pause at all; the check that the write really did outlast the delay is there
// because a run where it did not could not tell the two apart.
func TestSendKeysEnterGapAfterLargeText(t *testing.T) {
	w, _ := startRecorder(t, true, WindowOpts{})
	queued := func() int {
		w.input.mu.Lock()
		defer w.input.mu.Unlock()
		return w.input.bytes
	}
	// until returns the interval a transition happened in: the last time cond was
	// seen false and the first time it was seen true. Both ends are needed because
	// the two checks below want opposite ones.
	//
	// Polling is far tighter than waitFor's 20 ms, which would be a fifth of the
	// delay being measured, but the interval is not 500 us and cannot be assumed to
	// be: a poll is late by however long its goroutine was descheduled, which under
	// load is unbounded. So "first seen true" can sit well after the transition, and
	// a gap taken between two such estimates errs in an unknown direction -- a
	// correct writeInput can measure short simply because the poll that caught the
	// first edge was the one that got delayed. Every estimate below is therefore the
	// one that cannot produce a red on its own: the earliest possible start and the
	// latest possible end for the gap, the latest possible end for the write. That
	// makes this test miss a shortened delay on a machine loaded enough to skew it,
	// and never report one that did not happen -- the right way round for a suite
	// whose reds have to mean something.
	until := func(what string, cond func() bool) (lastFalse, firstTrue time.Time) {
		t.Helper()
		lastFalse = time.Now()
		for deadline := lastFalse.Add(30 * time.Second); ; time.Sleep(500 * time.Microsecond) {
			if cond() {
				return lastFalse, time.Now()
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d bytes still queued", what, queued())
			}
			lastFalse = time.Now()
		}
	}

	const delay = 100 * time.Millisecond
	ms := uint64(delay / time.Millisecond)
	// The recorder reads 4 KiB about every 2 ms, so it paces the write at roughly
	// 2 MB/s and 300 KB takes appreciably longer to write than the delay.
	start := time.Now()
	if err := w.SendKeys(strings.Repeat("a", 300_000), true, &ms); err != nil {
		t.Fatal(err)
	}
	textAtLeast, textAtMost := until("the text was never written", func() bool { return queued() <= 1 })
	_, crAtMost := until("the CR was never written", func() bool { return queued() == 0 })

	wrote, gap := textAtMost.Sub(start), crAtMost.Sub(textAtLeast)
	t.Logf("the text's write took at most %v; CR written at most %v after it returned", wrote, gap)
	if wrote < delay {
		t.Fatalf("the text's write took %v, less than the %v delay: a CR timed from the enqueue would still have waited after it, so this run cannot tell that apart from a CR timed from the write", wrote, delay)
	}
	// Nearly all of the delay, not half of it. The measurement above cannot
	// undershoot and writeInput sleeps out the remainder of a gap it timed from the
	// previous write's return, so a correct one has no room to land short: 5 runs
	// idle and 5 under 24 spinners at a load average of 17 measured 100.3 to
	// 102.6 ms, and load moves that up rather than down.
	//
	// What the 5% costs is the small shortfalls. Shaving the delay by 40% measures 60
	// to 61 ms and is caught 5 of 5; by 15%, caught 3 of 3; by 10%, 2 of 3; by 6%, once
	// in three runs or not at all, because the poll's own lateness is the same few
	// milliseconds -- 1.5 ms idle, and as much as 6 ms at a load average of 32. So this
	// catches a delay cut by more than about a seventh and not one trimmed at the edges
	// -- a limit
	// worth stating rather than closing, because closing it means bounding the poll's
	// lateness, which load makes unbounded. At half it caught almost nothing: a
	// writeInput that slept 60% of what was asked passed 5 runs of 5, which made the
	// write-side rewrite weaker on this axis than the reader-side test it replaced.
	const least = delay * 95 / 100
	if gap < least {
		t.Errorf("CR written at most %v after the text's write returned, want at least %v of the %v delay", gap, least, delay)
	}
}

// Input from many goroutines arrives whole and in each caller's order.
func TestInputQueueOrder(t *testing.T) {
	w, file := startRecorder(t, false, WindowOpts{})
	const callers, chunks = 8, 50
	var wg sync.WaitGroup
	total := 0
	for k := 0; k < callers; k++ {
		for seq := 0; seq < chunks; seq++ {
			total += 2 + 100 + (k*7+seq*13)%200
		}
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			for seq := 0; seq < chunks; seq++ {
				rec := append([]byte{byte(k), byte(seq)}, bytes.Repeat([]byte{'a' + byte(k)}, 100+(k*7+seq*13)%200)...)
				var err error
				if k%2 == 0 {
					err = w.SendRawKey(string(rec))
				} else {
					err = w.WriteInput(rec)
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		}(k)
	}
	wg.Wait()
	var got []byte
	waitFor(t, 10*time.Second, func() bool {
		got, _ = os.ReadFile(file)
		return len(got) >= total
	}, "input never all arrived")
	next := make([]int, callers)
	for i := 0; i < len(got); {
		k, seq := int(got[i]), int(got[i+1])
		n := 100 + (k*7+seq*13)%200
		if k >= callers || seq != next[k] || i+2+n > len(got) || !bytes.Equal(got[i+2:i+2+n], bytes.Repeat([]byte{'a' + byte(k)}, n)) {
			t.Fatalf("at byte %d: record k=%d seq=%d, want seq %d whole", i, k, seq, next[min(k, callers-1)])
		}
		next[k]++
		i += 2 + n
	}
}

// An exited window that wasn't killed still resizes: the grid follows,
// attached views are woken, and the closed terminal's ioctl is skipped.
func TestResizeAfterProgramExits(t *testing.T) {
	w, err := NewWindow(WindowOpts{Name: "bye", Command: []string{"/bin/sh", "-c", "printf bye"}, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	waitFor(t, 5*time.Second, func() bool { return !w.IsAlive() }, "terminal never closed")
	woken := w.Dirty().Wait()
	if err := w.Resize(100, 30); err != nil {
		t.Fatalf("resize after exit: %v", err)
	}
	select {
	case <-woken:
	case <-time.After(time.Second):
		t.Fatal("resize after exit didn't wake attached views")
	}
	if w.Cols() != 100 || w.Rows() != 30 {
		t.Fatalf("size %dx%d, want 100x30", w.Cols(), w.Rows())
	}
}

// Input still queued for the program holds injections, as typing does; an
// idle prompt with nothing queued gets them.
func TestInjectWaitsForQueuedInput(t *testing.T) {
	prompt := `printf '\342\235\257\r\n'` // ❯, the default idle pattern
	idle := startNotReading(t, WindowOpts{Name: "idle"}, prompt)
	busy := startNotReading(t, WindowOpts{Name: "busy"}, prompt)
	for _, w := range []*Window{idle, busy} {
		waitFor(t, 5*time.Second, func() bool {
			out, _ := w.Capture(5)
			return strings.Contains(out, "❯")
		}, "prompt never shown")
	}
	if err := busy.PasteWith(strings.Repeat("x", 1<<20), PasteOpts{Mode: "raw"}); err != nil {
		t.Fatal(err)
	}
	for _, w := range []*Window{idle, busy} {
		if _, err := w.InjectQueue().Enqueue("injected", 0); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 5*time.Second, func() bool { return idle.InjectQueue().Len() == 0 }, "injection into an idle window never delivered")
	time.Sleep(time.Second)
	if n := busy.InjectQueue().Len(); n != 1 {
		t.Fatalf("injection delivered behind %d queued bytes of a paste", 1<<20)
	}
}

// Past the window's limit, input fails as a whole with ErrInputQueueFull,
// and the window stays killable.
func TestInputQueueLimit(t *testing.T) {
	w := startNotReading(t, WindowOpts{InputQueueLimit: 1 << 20}, "printf ready")
	text := strings.Repeat("y", 700_000)
	if err := w.SendKeys(text, true, nil); err != nil {
		t.Fatal(err)
	}
	queued := func() int {
		w.input.mu.Lock()
		defer w.input.mu.Unlock()
		return w.input.bytes
	}
	before := queued()
	if err := w.SendKeys(text, true, nil); !errors.Is(err, ErrInputQueueFull) {
		t.Fatalf("second 700 KB: %v, want ErrInputQueueFull", err)
	}
	// Text that fits exactly, with an Enter that doesn't.
	if err := w.SendKeys(strings.Repeat("y", 1<<20-before), true, nil); !errors.Is(err, ErrInputQueueFull) {
		t.Fatalf("text filling the queue plus Enter: %v, want ErrInputQueueFull", err)
	}
	if after := queued(); after != before {
		t.Fatalf("a failed SendKeys queued %d bytes: text and Enter must fail together", after-before)
	}
	if err := w.SendRawKey("z"); err != nil {
		t.Fatalf("a key within the limit: %v", err)
	}
	if !within(2*time.Second, w.Kill) {
		t.Fatal("Kill blocked for 2 s")
	}
	if err := w.SendRawKey("z"); err == nil {
		t.Fatal("input to a killed window succeeded")
	}
}

// Windows sharing an InputBudget are bounded together; killing one returns
// its bytes to the budget.
func TestInputBudgetShared(t *testing.T) {
	budget := NewInputBudget(1 << 20)
	a := startNotReading(t, WindowOpts{Name: "a", InputBudget: budget}, "printf ready")
	b := startNotReading(t, WindowOpts{Name: "b", InputBudget: budget}, "printf ready")
	// Two chunks, so one is queued behind the one being written when a is
	// killed: both must come back.
	for _, n := range []int{400_000, 300_000} {
		if err := a.SendKeys(strings.Repeat("y", n), false, nil); err != nil {
			t.Fatal(err)
		}
	}
	text := strings.Repeat("y", 700_000)
	if err := b.SendKeys(text, false, nil); !errors.Is(err, ErrInputQueueFull) {
		t.Fatalf("second window past the shared budget: %v, want ErrInputQueueFull", err)
	}
	a.Kill()
	waitFor(t, 2*time.Second, func() bool { return budget.used.Load() == 0 }, "killed window's bytes never returned to the budget")
	if err := b.SendKeys(text, false, nil); err != nil {
		t.Fatalf("after the other window was killed: %v", err)
	}
}

// Replies to terminal queries from a program that doesn't read are dropped
// past the backlog cap instead of piling up, and output keeps flowing: color
// queries (answered once colors are set) and cursor position reports alike.
func TestReplyBacklogDropped(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		count       int // enough replies to pass the backlog and the kernel's buffer
	}{
		{"osc11", `\033]11;?\007`, 8000},
		{"cpr", `\033[6n`, 30000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := startNotReading(t, WindowOpts{}, fmt.Sprintf(`sleep 0.2; i=0; while [ $i -lt %d ]; do printf '%s'; i=$((i+1)); done; printf replies-done`, tc.count, tc.query))
			w.SetColors(map[int]vt.RGB{IdxBackground: {R: 1, G: 2, B: 3}})
			waitFor(t, 10*time.Second, func() bool {
				out, _ := w.Capture(3)
				return strings.Contains(out, "replies-done")
			}, "output stopped")
			w.input.mu.Lock()
			dropped, replies := w.input.dropped, w.input.replies
			w.input.mu.Unlock()
			if dropped == 0 || replies > maxReplyBacklog {
				t.Fatalf("dropped %d replies with %d bytes queued; want drops and at most %d queued", dropped, replies, maxReplyBacklog)
			}
		})
	}
}
