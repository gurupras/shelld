// Package shelld runs programs in pseudo-terminal windows and keeps what they
// draw: a VT grid with scrollback, raw output lines, input helpers and an
// idle-aware inject queue. The shelld daemon (cmd/shelld) serves these
// windows over HTTP and WebSocket; a Go program can use them directly.
//
//	w, err := shelld.NewWindow(shelld.WindowOpts{
//		Name:    "build",
//		Command: []string{"make", "test"},
//		Cwd:     "/src/project",
//		Cols:    120, Rows: 40,
//		Environ: []string{"PATH=/usr/bin:/bin", "HOME=/home/ci"},
//	})
//	if err != nil { ... }
//	defer w.Kill()
//
//	<-w.Done()
//	st, _ := w.ExitStatus()       // st.Code, or st.Signal when killed
//	screen := w.CaptureVisible(40) // what the terminal shows now
//
// Input goes through four methods:
//   - SendKeys writes text, optionally followed by Enter after a short delay;
//   - SendRawKey writes bytes as given, e.g. "\x03" for Ctrl-C;
//   - SendKeyNames sends tmux send-keys names ("Up", "C-c", "Enter"),
//     encoded for the modes the program has set, such as DECCKM;
//   - Paste writes text as a paste, then Enter. It brackets the text when
//     the program enabled bracketed paste ("auto"), and sanitizes it as tmux
//     3.7c's paste-buffer does: ESC, ^C and other control characters arrive
//     as visible text, so pasted content can't end the bracket, send ESC or
//     interrupt the program. TAB, BS, BEL and CR pass and LF becomes CR, so
//     without brackets each line break still submits a line. Callers
//     pasting into Claude Code or Codex should use PasteWith with Mode
//     "bracketed": it doesn't depend on the TUI having started, and ConPTY's
//     handling of bracketed paste is unverified.
//
// Breaking change for callers of Paste and the daemon's POST …/paste: pastes
// used to be bracketed always and sent unchanged. A caller that wraps text in
// ESC[200~ … ESC[201~ itself (as a Codex client might) must
// pass Mode "bracketed" instead, or the program sees the markers as text.
//
// The inject queue (InjectQueue, POST …/inject) still writes its payloads
// unchanged, followed by CR: it types into an idle prompt the way shelld-rs
// does, with no brackets or sanitizing, so its payloads must be trusted.
//
// "Idle" is decided by replaying the newest lines of raw history over a column
// buffer and matching the prompt pattern against what a terminal would be
// showing. The replay models carriage returns, backspace, tabs and the
// erase-in-line sequences, and skips what draws nothing: colours, the handful of
// mode changes a shell brackets its prompt with, charset designation, and OSC and
// DCS strings. A line carrying anything else cannot be placed from its own bytes
// — a cursor move, an erase that is not erase-in-line, a mode change that alters
// how the line itself renders — and the scan then reports not idle rather than
// guessing. A prompt drawn with cursor addressing
// — a full-screen program's, or a shell that redraws with CSI J or positions a
// right-hand prompt — is therefore not recognised, and its injections wait. That
// is the deliberate direction — the scan may call an idle prompt busy, never a
// busy program idle, because the cost of the second is typing into a running
// command.
//
// The wait is bounded unless a caller asks for it not to be. The window logs at
// Info once when a payload starts waiting on output the scan cannot read and once
// when it can read it again, and InjectConfig.StallTimeoutMS
// (WindowOpts.StallTimeoutMS, five minutes by default, zero to wait for ever)
// discards a payload that waits that long, at Warn, recording it in
// Window.InjectExpiries and in GET …/inject/queue. A deadline is necessary
// because a payload can wait without any bound of its own in two ways. The
// refusal can be permanent rather than merely long: an idle program writes
// nothing further, so the line the scan cannot read stays the newest one, and
// nothing the window does by itself writes the output that would clear it. A
// keystroke from any client does — a bare CR makes the shell draw a fresh prompt
// — but an idle window with nobody at it receives none. One `clear` reaches that
// state in an ordinary shell, and `clear` is a payload a caller may inject. The
// other way is priority: the queue delivers higher priorities first, so a stream
// of them can be typed past a low-priority payload for as long as it lasts. The
// deadline is therefore checked on a poll that delivers as well as on one that
// does not, after the write rather than before it, so that the payload a poll
// chose is never the one it drops.
//
// An expiry drops the payload rather than typing it, because the scan cannot
// distinguish a prompt it failed to model from a full-screen program — vi at its
// splash screen refuses exactly as a redrawn prompt does — so delivering on a
// deadline would type into whatever is running, with nobody watching. That is
// the failure the refusal exists to prevent, and a deadline must not undo it.
//
// All four queue their bytes for the program and return: a window's writer
// delivers them in order, so a program that stops reading its input blocks
// neither the caller nor Kill. Queued input is bounded per window and across
// windows (WindowOpts.InputQueueLimit, InputBudget); past a bound, input
// fails with ErrInputQueueFull, or with the ErrInputTooLarge that wraps it when
// the write is larger than the bound itself and so would fail against an empty
// queue too.
//
// Output is kept two ways. Capture and ScrollRange read raw history: the bytes
// the program wrote, with a line ending where the program ended it and nowhere
// else — a line longer than one PTY read is one line, and control sequences,
// bare CRs and the frames a progress bar overwrote are all still in it.
// CaptureVisible renders the live grid instead, so it shows in-place redraws as
// the terminal does. Because a raw line has no length limit, every read of
// history is bounded in bytes as well as in lines: Capture and ScrollRange take
// a default bound from their line count (see readByteCap), and CaptureBytes,
// ScrollRangeBytes and Scrollback.RangeBytes take one explicitly and report
// whether it dropped anything. A bound never splits a rune, and a read returns
// part of a line rather than nothing when even one line does not fit — lineBytes
// says how large a bound that line needs — so a client paging forwards always
// makes progress.
//
// On Windows a program's output reaches shelld through ConPTY, and conhost
// sends its own rendering of the screen: cursor positioning, its own line
// breaks, and a full repaint after a resize. Capture and ScrollRange (and
// the daemon's raw capture and scroll) therefore hold that rendering, not the
// program's bytes: long lines are split at the console width and each resize
// adds a copy of the screen. Clients on Windows should read
// CaptureVisible (capture?mode=visible).
//
// NewWindow fails with a *CwdError when WindowOpts.Cwd is not a directory,
// where tmux and shelld-rs start the program in the home directory. When the
// spawn itself fails it wraps the cause, so errors.Is reaches the errno behind
// it -- fs.ErrNotExist for a shell that is not there, fs.ErrPermission for one
// that may not be executed.
//
// Sessions and Manager group windows by name the way the daemon's API does.
package shelld
