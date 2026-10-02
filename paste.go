package shelld

import (
	"fmt"
	"unicode/utf8"
)

// PasteOpts chooses how PasteWith delivers text.
type PasteOpts struct {
	// Mode is "auto" (bracket the text when the program has enabled
	// bracketed paste, DECSET 2004), "bracketed" (always) or "raw" (never).
	// Empty means the window's PasteMode.
	Mode string
	// Literal writes text's bytes unchanged, LF included, instead of
	// sanitizing them. It changes nothing else: Mode still decides the
	// brackets and Enter follows as usual. Sanitized text is what tmux
	// 3.7c's paste-buffer writes without -S: LF becomes CR, TAB, BS, BEL and
	// CR pass, and other control characters, DEL and bytes that aren't UTF-8
	// arrive as visible text (ESC as ^[, ^C as ^C). So sanitized text can't
	// end the bracket or send ESC or ^C, though without brackets each CR
	// still submits a line. Literal text can end the bracket early with
	// ESC[201~ and type keys, or interrupt the program with ^C.
	Literal bool
}

// ValidPasteMode reports whether m is "auto", "bracketed" or "raw".
func ValidPasteMode(m string) bool { return m == "auto" || m == "bracketed" || m == "raw" }

// Paste is PasteWith with the window's paste mode, sanitized.
//
// This default changed: Paste used to bracket every paste unless the window
// was "raw", and sent the text unchanged. Callers that add their own
// ESC[200~ … ESC[201~ must stop and pass Mode "bracketed" instead, or the
// program receives the markers as visible ^[[200~ text.
func (w *Window) Paste(text string) error { return w.PasteWith(text, PasteOpts{}) }

// PasteWith pastes text and then presses Enter. Bracketed, both go in one
// write: the CR after the closing bracket is a keypress. Unbracketed, Enter
// is written at least SendKeysDelayMS after the text's write finishes, as in
// SendKeys, so a TUI's paste-burst detection sees a submit.
//
// "auto" follows the modes the grid has processed, not what the program has
// written: a paste that arrives before a just-started TUI's first
// ESC[?2004h has been read (Node startup takes hundreds of milliseconds), or
// while reading lags a flood, is decided on older modes, as it is in tmux.
// Callers pasting into a TUI that brackets, such as Claude Code or Codex,
// should pass "bracketed". Whether ConPTY on Windows passes DECSET 2004
// through to shelld is unverified, which is another reason to.
//
// Pasting is not typing: unlike SendKeys it doesn't touch the InputTracker
// and isn't held by an active stash, which model a human at the keyboard.
//
// PasteWith returns once the paste is queued for the program; see
// ErrInputQueueFull.
func (w *Window) PasteWith(text string, opts PasteOpts) error {
	if !w.IsAlive() {
		return w.deadErr()
	}
	mode := opts.Mode
	if mode == "" {
		mode = w.PasteMode()
	}
	var bracketed bool
	switch mode {
	case "auto":
		bracketed = w.KeyModes().BracketedPaste
	case "bracketed":
		bracketed = true
	case "raw":
	default:
		return fmt.Errorf("invalid paste mode %q", mode)
	}
	payload := appendPaste(make([]byte, 0, len(text)+len(bracketStart)+len(bracketEnd)+1), text, bracketed, opts.Literal)
	if bracketed {
		return w.input.push(inputChunk{data: append(payload, '\r')})
	}
	// enterDelay, not a bare multiply: this is the same SendKeysDelayMS that
	// SendKeys clamps, and the gap it produces is read by the same writeInput,
	// whose `c.gap > 0` turns a wrapped negative into no gap at all.
	return w.input.push(inputChunk{data: payload}, enterAfter(enterDelay(w.injectCfg.SendKeysDelayMS)))
}

var (
	bracketStart = []byte("\x1b[200~")
	bracketEnd   = []byte("\x1b[201~")
)

// appendPaste appends text as a paste: between ESC[200~ and ESC[201~ when
// bracketed, and sanitized unless literal (see PasteOpts).
func appendPaste(dst []byte, text string, bracketed, literal bool) []byte {
	if bracketed {
		dst = append(dst, bracketStart...)
	}
	if literal {
		dst = append(dst, text...)
	} else {
		dst = appendPasteVis(dst, text)
	}
	if bracketed {
		dst = append(dst, bracketEnd...)
	}
	return dst
}

// appendPasteVis appends text as tmux 3.7c's paste-buffer writes it without
// -S: LF becomes CR, and the rest goes through vis(3) with
// VIS_SAFE|VIS_NOSLASH. Valid UTF-8 (C1 characters included), printable
// ASCII, space, TAB, BS, BEL and CR pass. Other C0 controls become ^@ to ^_
// and DEL ^?. A byte that isn't part of valid UTF-8 becomes M- or M^
// followed by its low seven bits shown the same way, or \240 for 0xa0.
//
// Validity is Go's (utf8.DecodeRuneInString): overlong forms, surrogates and
// sequences above U+10FFFF are escaped byte by byte. tmux's decoder accepts
// some of these, e.g. f4 90 80 80 passes raw, so shelld is stricter there.
func appendPasteVis(dst []byte, text string) []byte {
	for i := 0; i < len(text); {
		c := text[i]
		if c >= 0x80 {
			if _, n := utf8.DecodeRuneInString(text[i:]); n > 1 {
				dst = append(dst, text[i:i+n]...)
				i += n
				continue
			}
		}
		i++
		switch {
		case c == '\n':
			dst = append(dst, '\r')
		case c >= ' ' && c < 0x7f, c == '\t', c == '\b', c == '\a', c == '\r':
			dst = append(dst, c)
		case c == 0xa0:
			dst = append(dst, `\240`...)
		default:
			if c >= 0x80 {
				dst = append(dst, 'M')
				c &= 0x7f
			}
			switch {
			case c == 0x7f:
				dst = append(dst, '^', '?')
			case c < ' ':
				dst = append(dst, '^', c+'@')
			default:
				dst = append(dst, '-', c)
			}
		}
	}
	return dst
}
