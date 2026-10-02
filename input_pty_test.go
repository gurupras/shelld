//go:build linux || darwin

package shelld

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestHelperInputEcho is not a test: run as a window's program with
// SHELLD_TEST_HELPER=echo-input, it prints SHELLD_TEST_MODES (escape sequences
// that set terminal modes), then "ready", then every byte it reads from the
// terminal as hex, one read per "in:" line.
func TestHelperInputEcho(t *testing.T) {
	if os.Getenv("SHELLD_TEST_HELPER") != "echo-input" {
		return
	}
	os.Stdout.WriteString(os.Getenv("SHELLD_TEST_MODES") + "ready\r\n")
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			fmt.Fprintf(os.Stdout, "in:%s\r\n", hex.EncodeToString(buf[:n]))
		}
		if err != nil {
			os.Exit(0)
		}
	}
}

// startInputEcho runs TestHelperInputEcho on a raw, non-echoing terminal after
// the program has written modes, and waits until it is reading.
func startInputEcho(t *testing.T, modes string, opts WindowOpts) *Window {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	opts.Name = "input-echo"
	opts.Command = []string{"/bin/sh", "-c", `stty raw -echo && exec "$0" -test.run='^TestHelperInputEcho$'`, exe}
	opts.Env = append(opts.Env, [2]string{"SHELLD_TEST_HELPER", "echo-input"}, [2]string{"SHELLD_TEST_MODES", modes})
	opts.Cols, opts.Rows = 80, 24
	w, err := NewWindow(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Kill)
	// The grid, not the raw scrollback, is the sync point: modes the helper
	// printed before "ready" have been processed once the grid shows it.
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(w.CaptureVisible(0), "ready")
	}, "input helper never started")
	return w
}

// receivedInput returns everything the helper has read so far, once it has
// read at least want bytes (or the wait times out).
func receivedInput(t *testing.T, w *Window, want int) []byte {
	t.Helper()
	var got []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		got = got[:0]
		out, _ := w.Capture(10_000)
		lines := strings.Split(out, "\n")
		for i, line := range lines {
			h, ok := strings.CutPrefix(line, "in:")
			if !ok {
				continue
			}
			b, err := hex.DecodeString(h)
			if err != nil {
				// Capture returns the newest line as it stands -- its own doc says the
				// last line's text can change between captures -- and the helper emits
				// one "in:%s\r\n" per read, so a capture landing inside that write sees
				// an odd number of hex digits. Skip it: the count comes up short, the
				// caller polls again, and the finished line is read next time round.
				// On any earlier line a bad digit is a real defect, because those are
				// terminated and will not change, so it still fails there.
				//
				// This was a Fatalf on every line, which would fail with a message
				// about the helper rather than about the test. How often that would
				// actually happen is not known: two probes taking 201,967 captures
				// while input drained saw no partial in: line at all, at one byte per
				// write and at 4000. So this is a guard against a state that is
				// reachable by construction and was never observed, kept because it
				// costs nothing and because a caller polling while thousands of lines
				// drain takes far more snapshots than those probes did.
				if i == len(lines)-1 {
					continue
				}
				t.Fatalf("helper line %d of %d, %q: %v", i+1, len(lines), line, err)
			}
			got = append(got, b...)
		}
		if len(got) >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A line longer than one PTY read comes back from raw capture as one line.
func TestCaptureLongLineAcrossReads(t *testing.T) {
	w, err := NewWindow(WindowOpts{
		Name:    "long",
		Command: []string{"/bin/sh", "-c", `head -c 9000 /dev/zero | tr '\0' x; echo; echo done; exec sleep 30`},
		Cols:    80, Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	waitFor(t, 5*time.Second, func() bool {
		out, _ := w.Capture(10)
		return strings.HasSuffix(out, "done")
	}, "output never finished")
	out, total := w.Capture(10)
	if want := strings.Repeat("x", 9000) + "\ndone"; out != want || total != 2 {
		t.Fatalf("capture has %d lines (total %d), lengths %v; want [9000 4]", strings.Count(out, "\n")+1, total, lineLens(out))
	}
}

// Paste brackets text only when the program asked for bracketed paste
// (DECSET 2004), as tmux's paste-buffer -p does.
func TestPasteBracketsOnlyWhenEnabled(t *testing.T) {
	for _, tc := range []struct {
		name, modes, want string
	}{
		{"not enabled", "", "hi\r"},
		{"enabled", "\x1b[?2004h", "\x1b[200~hi\x1b[201~\r"},
		{"enabled then disabled", "\x1b[?2004h\x1b[?2004l", "hi\r"},
		{"enabled then reset", "\x1b[?2004h\x1bc", "hi\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := startInputEcho(t, tc.modes, WindowOpts{})
			if err := w.Paste("hi"); err != nil {
				t.Fatal(err)
			}
			if got := string(receivedInput(t, w, len(tc.want))); got != tc.want {
				t.Fatalf("program read %q, want %q", got, tc.want)
			}
		})
	}
}

// Pasted text cannot end the bracket early or send a control character: ESC
// and ^C arrive as visible text, as with tmux 3.7's paste-buffer.
func TestPasteCannotEndBracketEarly(t *testing.T) {
	for _, tc := range []struct {
		name, modes, want string
	}{
		{"bracketed", "\x1b[?2004h", "\x1b[200~a^[[201~b^Cc\rd\x1b[201~\r"},
		{"unbracketed", "", "a^[[201~b^Cc\rd\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := startInputEcho(t, tc.modes, WindowOpts{})
			if err := w.Paste("a\x1b[201~b\x03c\nd"); err != nil {
				t.Fatal(err)
			}
			if got := string(receivedInput(t, w, len(tc.want))); got != tc.want {
				t.Fatalf("program read %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPasteLiteral(t *testing.T) {
	w := startInputEcho(t, "\x1b[?2004h", WindowOpts{})
	if err := w.PasteWith("a\x1b[201~\n\x03", PasteOpts{Literal: true}); err != nil {
		t.Fatal(err)
	}
	want := "\x1b[200~a\x1b[201~\n\x03\x1b[201~\r"
	if got := string(receivedInput(t, w, len(want))); got != want {
		t.Fatalf("program read %q, want %q", got, want)
	}
}

// An explicit mode, per paste or per window, overrides what the program set.
func TestPasteModeOverride(t *testing.T) {
	for _, tc := range []struct {
		name, modes string
		window      WindowOpts
		paste       PasteOpts
		want        string
	}{
		{"bracketed paste, no 2004", "", WindowOpts{}, PasteOpts{Mode: "bracketed"}, "\x1b[200~hi\x1b[201~\r"},
		{"raw paste, 2004", "\x1b[?2004h", WindowOpts{}, PasteOpts{Mode: "raw"}, "hi\r"},
		{"bracketed window, no 2004", "", WindowOpts{PasteMode: "bracketed"}, PasteOpts{}, "\x1b[200~hi\x1b[201~\r"},
		{"raw window, auto paste, 2004", "\x1b[?2004h", WindowOpts{PasteMode: "raw"}, PasteOpts{Mode: "auto"}, "\x1b[200~hi\x1b[201~\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := startInputEcho(t, tc.modes, tc.window)
			if err := w.PasteWith("hi", tc.paste); err != nil {
				t.Fatal(err)
			}
			if got := string(receivedInput(t, w, len(tc.want))); got != tc.want {
				t.Fatalf("program read %q, want %q", got, tc.want)
			}
		})
	}
	w := startInputEcho(t, "", WindowOpts{})
	if err := w.PasteWith("hi", PasteOpts{Mode: "brackets"}); err == nil {
		t.Fatal("invalid mode accepted")
	}
	if _, err := NewWindow(WindowOpts{Name: "bad", Command: []string{"/bin/true"}, PasteMode: "brackets"}); err == nil {
		t.Fatal("invalid window paste mode accepted")
	}
}

// Without brackets, Enter is a separate write after SendKeysDelayMS, so the
// program reads it apart from the text.
func TestPasteUnbracketedEnterIsSeparate(t *testing.T) {
	w := startInputEcho(t, "", WindowOpts{})
	if err := w.Paste("hello"); err != nil {
		t.Fatal(err)
	}
	receivedInput(t, w, len("hello\r"))
	out, _ := w.Capture(100)
	var reads []string
	for _, line := range strings.Split(out, "\n") {
		if h, ok := strings.CutPrefix(line, "in:"); ok {
			reads = append(reads, h)
		}
	}
	if len(reads) < 2 || reads[len(reads)-1] != "0d" || strings.Contains(strings.Join(reads[:len(reads)-1], ""), "0d") {
		t.Fatalf("reads %v: want the text, then 0d alone", reads)
	}
}

// Key names follow the modes the program set: cursor keys DECCKM, keypad keys
// DECKPAM.
func TestSendKeyNamesFollowModes(t *testing.T) {
	for _, tc := range []struct {
		name, modes string
		keyModes    KeyModes
		want        string
	}{
		{"normal", "", KeyModes{}, "\x1b[A\x1b[B5\x03hi\r"},
		{"DECCKM only", "\x1b[?1h", KeyModes{AppCursor: true}, "\x1bOA\x1bOB5\x03hi\r"},
		{"DECKPAM only", "\x1b=", KeyModes{AppKeypad: true}, "\x1b[A\x1b[B\x1bOu\x03hi\r"},
		{"both", "\x1b[?1h\x1b=", KeyModes{AppCursor: true, AppKeypad: true}, "\x1bOA\x1bOB\x1bOu\x03hi\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := startInputEcho(t, tc.modes, WindowOpts{})
			if got := w.KeyModes(); got != tc.keyModes {
				t.Fatalf("KeyModes %+v, want %+v", got, tc.keyModes)
			}
			if err := w.SendKeyNames([]string{"Up", "Down", "KP5", "C-c", "hi", "Enter"}, false); err != nil {
				t.Fatal(err)
			}
			if got := string(receivedInput(t, w, len(tc.want))); got != tc.want {
				t.Fatalf("program read %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSendKeyNamesStrictWritesNothing(t *testing.T) {
	w := startInputEcho(t, "", WindowOpts{})
	var kerr *KeyNameError
	if err := w.SendKeyNames([]string{"Up", "Entr", "S-Enter"}, true); !errors.As(err, &kerr) {
		t.Fatalf("strict: %v", err)
	}
	if err := w.SendKeyNames([]string{"x"}, true); err != nil {
		t.Fatal(err)
	}
	if got := string(receivedInput(t, w, 1)); got != "x" {
		t.Fatalf("program read %q, want %q", got, "x")
	}
}

// Output reaches the raw scrollback only after the grid has processed it, so
// a caller that sees text in Capture reads modes that include it. The program
// prints 100 ms after reading a key; while the test holds the grid's lock
// across that, the output must stay out of Capture. (The key is sent before
// taking the lock: writing input takes ptyMu, which sits above vtMu.)
func TestReadLoopAdvancesGridBeforeScrollback(t *testing.T) {
	w, err := NewWindow(WindowOpts{
		Name:    "order",
		Command: []string{"/bin/sh", "-c", `stty raw -echo; printf ready; head -c 1 >/dev/null; sleep 0.1; printf '\033[?2004hgo'; exec sleep 30`},
		Cols:    80, Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Kill()
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(w.CaptureVisible(0), "ready")
	}, "program never started")

	if err := w.SendRawKey("x"); err != nil {
		t.Fatal(err)
	}
	w.LockVT()
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if out, _ := w.Capture(5); strings.Contains(out, "go") {
			w.UnlockVT()
			t.Fatal("output reached the scrollback before the grid processed it")
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.UnlockVT()
	waitFor(t, 5*time.Second, func() bool {
		out, _ := w.Capture(5)
		return strings.Contains(out, "go")
	}, "output never arrived")
	if !w.KeyModes().BracketedPaste {
		t.Fatal("scrollback shows output whose modes the grid hasn't processed")
	}
}

// A stash holds a half-typed line aside while an injection is delivered, and
// every path a human's keystrokes reach diverts into it: SendKeys, SendRawKey,
// and typeInput behind both SendKeyNames and WriteInput. Nothing else covers the
// divert — StashState's own lifecycle test drives a standalone stash, which cannot
// tell a window that diverts from one that writes through.
//
// Paste is the deliberate exception, and here it is also the instrument: input
// goes to the program through one queue in order, so once the paste's bytes have
// arrived, anything the four calls before it had written would have arrived too.
// That makes their absence a fact about the window rather than about how long the
// test waited.
//
// The wait is for the paste's Enter, not for its text. Paste is PasteWith, which
// presses Enter after the text and, unbracketed, writes it at least
// SendKeysDelayMS later — so "P" arrives while the CR is still to come, and a test
// that stopped at "P" would be reading the queue before the paste itself had
// finished draining. Waiting for the CR is the same ordering argument one write
// further along.
func TestStashDivertsEveryTypingPath(t *testing.T) {
	w := startInputEcho(t, "", WindowOpts{})
	if err := w.WriteInput([]byte("A")); err != nil {
		t.Fatal(err)
	}
	if got := receivedInput(t, w, 1); string(got) != "A" {
		t.Fatalf("before the stash the program read %q, want %q", got, "A")
	}

	w.Stash().Begin("half-typed")
	if !w.Stash().IsActive() {
		t.Fatal("Begin left no stash open")
	}
	up, err := EncodeKeyNames([]string{"Up"}, w.KeyModes(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		name string
		call func() error
	}{
		{"SendKeys", func() error { return w.SendKeys("ls", true, nil) }},
		{"SendRawKey", func() error { return w.SendRawKey("\x03") }},
		{"SendKeyNames", func() error { return w.SendKeyNames([]string{"Up"}, true) }},
		{"WriteInput", func() error { return w.WriteInput([]byte("x")) }},
	} {
		if err := step.call(); err != nil {
			t.Fatalf("%s while the stash is open: %v", step.name, err)
		}
	}
	if err := w.Paste("P"); err != nil {
		t.Fatal(err)
	}
	if got := receivedInput(t, w, 3); string(got) != "AP\r" {
		t.Fatalf("with the stash open the program read %q, want %q: the paste went through and the four typing calls did not", got, "AP\r")
	}

	// Complete hands back the line with everything typed meanwhile appended in
	// the order it arrived, Enter included, and closes the stash.
	want := "half-typed" + "ls\r" + "\x03" + string(up) + "x"
	if got := w.Stash().Complete(); got != want {
		t.Fatalf("Complete returned %q, want %q", got, want)
	}
	if w.Stash().IsActive() {
		t.Fatal("Complete left the stash open")
	}

	// And the paths are open again.
	if err := w.WriteInput([]byte("B")); err != nil {
		t.Fatal(err)
	}
	if got := receivedInput(t, w, 4); string(got) != "AP\rB" {
		t.Fatalf("after Complete the program read %q, want %q", got, "AP\rB")
	}
}
