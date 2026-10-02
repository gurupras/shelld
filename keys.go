package shelld

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// KeyModes are the input modes a program has set on its terminal that change
// what a key or a paste sends.
type KeyModes struct {
	AppCursor      bool // DECCKM (CSI ?1h): cursor keys send SS3 sequences
	AppKeypad      bool // DECKPAM (ESC =): keypad keys send SS3 sequences
	BracketedPaste bool // CSI ?2004h: pastes are wrapped in CSI 200~ / CSI 201~
}

// EncodeKey returns the bytes tmux 3.7c's send-keys sends for the key name
// under modes m. ok is false when name is not a key tmux can send; send-keys,
// and EncodeKeyNames, then type the name as text.
//
// Names follow tmux: case-insensitive key names (Enter, Tab, BTab, Escape,
// Space, BSpace, Up, Down, Left, Right, Home, End, IC/Insert, DC/Delete,
// PPage/PageUp/PgUp, NPage/PageDown/PgDn, F1-F12, KP0-KP9, KP/ KP* KP- KP+
// KP. KPEnter, the C0 names [NUL]..[US] as tmux spells them, Any, UserN), a
// single character, or 0xN (a code point, sent as UTF-8), with C-, M- and S-
// prefixes in any order, or ^x for Ctrl.
//
// Encoding is tmux's standard key mode: cursor and keypad keys follow DECCKM
// and DECKPAM, modified cursor, editing and function keys use xterm's CSI 1;m
// forms, Meta prefixes ESC and Ctrl maps characters to C0 controls. Extended
// key modes (CSI u, modifyOtherKeys) are not implemented, so some modifiers
// are lost, as they are in tmux with extended-keys off:
//   - Shift on characters, Tab, Enter, Escape, BSpace and Space is dropped
//     (S-Enter sends CR, S-a sends a). S-Tab sends Tab: for Shift+Tab, which
//     Claude Code uses to cycle modes, send BTab (CSI Z);
//   - Ctrl on Tab, Enter and non-ASCII characters is dropped, and Ctrl on the
//     digits and punctuation tmux maps to printable characters sends that
//     character (C-1 sends 1);
//   - modifiers on BTab are dropped;
//   - Shift or Ctrl on a keypad key, Any and UserN send nothing.
//
// A Ctrl that cannot be encoded (C-BSpace, C-Escape, C-#) makes ok false.
// Programs that asked for kitty keyboard flags (Claude Code and Codex push
// CSI >5u) still receive legacy encodings.
//
// Three deliberate differences from tmux:
//   - unmodified Home and End send xterm's CSI H / CSI F (SS3 H / SS3 F under
//     DECCKM) where tmux sends CSI 1~ / CSI 4~. shelld sets no TERM, so which
//     is right depends on the TERM a program inherits: xterm-256color's
//     terminfo has khome=\EOH, tmux-256color's and screen's have \E[1~.
//     Readline, crossterm and Ink accept both;
//   - KPEnter without DECKPAM sends CR, as xterm does, where tmux sends LF;
//   - Meta with a key that cannot be encoded (C-M-BSpace) is not a key; tmux
//     writes an ESC before typing the name.
func EncodeKey(name string, m KeyModes) (b []byte, ok bool) {
	b, st := encodeKeyName(name, m)
	return b, st != keyRejected
}

// KeyNameError lists the names EncodeKeyNames refused in strict mode.
type KeyNameError struct {
	// Unknown names are not keys, or are keys that cannot be encoded.
	Unknown []string
	// Lossy names are keys whose encoding drops a modifier or sends nothing.
	Lossy []string
}

// Error lists the refused names, naming both categories when both are non-empty.
// Read Unknown and Lossy to tell them apart programmatically.
func (e *KeyNameError) Error() string {
	var parts []string
	if len(e.Unknown) > 0 {
		parts = append(parts, fmt.Sprintf("unknown key names %q", e.Unknown))
	}
	if len(e.Lossy) > 0 {
		parts = append(parts, fmt.Sprintf("key names that lose modifiers %q", e.Lossy))
	}
	return strings.Join(parts, "; ")
}

// EncodeKeyNames encodes send-keys arguments as tmux's send-keys does without
// -l: an argument that is a key name is encoded (see EncodeKey), and any other
// is typed as its UTF-8 characters (invalid bytes are skipped). With strict,
// an unknown name or a lossy one is an error (a *KeyNameError listing them)
// and nothing is returned.
func EncodeKeyNames(names []string, m KeyModes, strict bool) ([]byte, error) {
	var out []byte
	var bad KeyNameError
	for _, name := range names {
		b, st := encodeKeyName(name, m)
		switch {
		case st == keyRejected && strict:
			bad.Unknown = append(bad.Unknown, name)
		case st == keyLossy && strict:
			bad.Lossy = append(bad.Lossy, name)
		case st == keyRejected:
			out = appendValidUTF8(out, name)
		default:
			out = append(out, b...)
		}
	}
	if len(bad.Unknown) > 0 || len(bad.Lossy) > 0 {
		return nil, &bad
	}
	return out, nil
}

func appendValidUTF8(out []byte, s string) []byte {
	for len(s) > 0 {
		r, n := utf8.DecodeRuneInString(s)
		if r != utf8.RuneError || n > 1 {
			out = append(out, s[:n]...)
		}
		s = s[n:]
	}
	return out
}

// ── tmux key codes ─────────────────────────────────────────────────────────
//
// A port of tmux 3.7c's key-string.c (name → key code) and input-keys.c (key
// code → bytes, standard mode). Key codes keep tmux's layout: the key in the
// low bits, modifiers and flags above. Characters are their code points;
// named keys sit above the Unicode range.

type keyCode uint64

const (
	kMeta         keyCode = 0x00100000000000
	kCtrl         keyCode = 0x00200000000000
	kShift        keyCode = 0x00400000000000
	kKeypad       keyCode = 0x02000000000000
	kCursor       keyCode = 0x04000000000000
	kImpliedMeta  keyCode = 0x08000000000000
	kMaskMods     keyCode = 0x00ff0000000000
	kMaskKey      keyCode = 0x0000ffffffffff
	kUnknown      keyCode = 1<<64 - 1
	kSpecialStart keyCode = 0x200000
)

const (
	kAny keyCode = kSpecialStart + iota
	kBSpace
	kF1
	kF2
	kF3
	kF4
	kF5
	kF6
	kF7
	kF8
	kF9
	kF10
	kF11
	kF12
	kIC
	kDC
	kHome
	kEnd
	kNPage
	kPPage
	kBTab
	kUp
	kDown
	kLeft
	kRight
	kKPSlash
	kKPStar
	kKPMinus
	kKP7
	kKP8
	kKP9
	kKPPlus
	kKP4
	kKP5
	kKP6
	kKP1
	kKP2
	kKP3
	kKPEnter
	kKP0
	kKPPeriod
	kUser // kUser+n is UserN
)

const keyUserCount = 1000 // tmux's KEYC_NUSER

func (k keyCode) isSpecial() bool { return k&kMaskKey >= kSpecialStart }

var keyNameTable = func() map[string]keyCode {
	t := map[string]keyCode{
		"F1": kF1 | kImpliedMeta, "F2": kF2 | kImpliedMeta, "F3": kF3 | kImpliedMeta,
		"F4": kF4 | kImpliedMeta, "F5": kF5 | kImpliedMeta, "F6": kF6 | kImpliedMeta,
		"F7": kF7 | kImpliedMeta, "F8": kF8 | kImpliedMeta, "F9": kF9 | kImpliedMeta,
		"F10": kF10 | kImpliedMeta, "F11": kF11 | kImpliedMeta, "F12": kF12 | kImpliedMeta,
		"IC": kIC | kImpliedMeta, "Insert": kIC | kImpliedMeta,
		"DC": kDC | kImpliedMeta, "Delete": kDC | kImpliedMeta,
		// Home and End carry the cursor flag, unlike tmux, for the xterm
		// encodings (see EncodeKey).
		"Home": kHome | kCursor | kImpliedMeta, "End": kEnd | kCursor | kImpliedMeta,
		"NPage": kNPage | kImpliedMeta, "PageDown": kNPage | kImpliedMeta, "PgDn": kNPage | kImpliedMeta,
		"PPage": kPPage | kImpliedMeta, "PageUp": kPPage | kImpliedMeta, "PgUp": kPPage | kImpliedMeta,
		"BTab": kBTab, "Space": ' ', "BSpace": kBSpace,
		"Tab": '\t', "Enter": '\r', "Escape": 0x1b,
		"Up": kUp | kCursor | kImpliedMeta, "Down": kDown | kCursor | kImpliedMeta,
		"Left": kLeft | kCursor | kImpliedMeta, "Right": kRight | kCursor | kImpliedMeta,
		"KP/": kKPSlash | kKeypad, "KP*": kKPStar | kKeypad, "KP-": kKPMinus | kKeypad,
		"KP7": kKP7 | kKeypad, "KP8": kKP8 | kKeypad, "KP9": kKP9 | kKeypad,
		"KP+": kKPPlus | kKeypad, "KP4": kKP4 | kKeypad, "KP5": kKP5 | kKeypad,
		"KP6": kKP6 | kKeypad, "KP1": kKP1 | kKeypad, "KP2": kKP2 | kKeypad,
		"KP3": kKP3 | kKeypad, "KPEnter": kKPEnter | kKeypad, "KP0": kKP0 | kKeypad,
		"KP.": kKPPeriod | kKeypad,
	}
	// C0 names; tmux names HT, CR and ESC Tab, Enter and Escape.
	for i, n := range []string{"NUL", "SOH", "STX", "ETX", "EOT", "ENQ", "ASC", "BEL", "BS", "", "LF",
		"VT", "FF", "", "SO", "SI", "DLE", "DC1", "DC2", "DC3", "DC4", "NAK", "SYN", "ETB", "CAN", "EM",
		"SUB", "", "FS", "GS", "RS", "US"} {
		if n != "" {
			t["["+n+"]"] = keyCode(i)
		}
	}
	lower := make(map[string]keyCode, len(t))
	for k, v := range t {
		lower[strings.ToLower(k)] = v
	}
	return lower
}()

// lookupKeyName is key_string_lookup_string: kUnknown when name is not a key.
func lookupKeyName(s string) keyCode {
	if strings.EqualFold(s, "None") {
		return kUnknown // KEYC_NONE: send-keys types it
	}
	if strings.EqualFold(s, "Any") {
		return kAny
	}
	if len(s) >= 2 && s[0] == '0' && s[1] == 'x' {
		u, ok := scanHex(s[2:])
		if !ok {
			return kUnknown
		}
		if u < 32 {
			return keyCode(u)
		}
		if !utf8.ValidRune(rune(u)) || u > utf8.MaxRune {
			return kUnknown
		}
		return keyCode(u)
	}
	var mods keyCode
	if len(s) >= 2 && s[0] == '^' {
		if len(s) == 2 {
			c := s[1]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			return keyCode(c) | kCtrl
		}
		mods |= kCtrl
		s = s[1:]
	}
	for len(s) >= 2 && s[1] == '-' {
		switch s[0] {
		case 'C', 'c':
			mods |= kCtrl
		case 'M', 'm':
			mods |= kMeta
		case 'S', 's':
			mods |= kShift
		default:
			return kUnknown
		}
		s = s[2:]
	}
	if s == "" {
		return kUnknown
	}
	if len(s) == 1 && s[0] <= 127 {
		if s[0] < 32 {
			return kUnknown
		}
		return keyCode(s[0]) | mods
	}
	if s[0] >= 0xc2 && s[0] <= 0xf4 { // a UTF-8 lead byte: tmux's utf8_open
		r, n := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError || n != len(s) {
			return kUnknown
		}
		return keyCode(r) | mods
	}
	key, ok := keyNameTable[strings.ToLower(s)]
	if !ok {
		if n, ok := userKey(s); ok {
			return (kUser + keyCode(n)) | mods
		}
		return kUnknown
	}
	if mods&kMeta == 0 {
		key &^= kImpliedMeta
	}
	return key | mods
}

// scanHex reads a number as sscanf("%x") does: optional spaces and sign, an
// optional 0x, then at least one hex digit; trailing text is ignored.
func scanHex(s string) (uint32, bool) {
	s = strings.TrimLeft(s, " \t\n\v\f\r")
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg, s = s[0] == '-', s[1:]
	}
	if len(s) >= 3 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') && isHexDigit(s[2]) {
		s = s[2:]
	}
	var u uint32
	n := 0
	for ; n < len(s) && isHexDigit(s[n]); n++ {
		d := s[n]
		switch {
		case d <= '9':
			d -= '0'
		case d >= 'a':
			d -= 'a' - 10
		default:
			d -= 'A' - 10
		}
		u = u<<4 | uint32(d)
	}
	if neg {
		u = -u
	}
	return u, n > 0
}

func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// userKey parses sscanf("User%u") <= KEYC_NUSER.
func userKey(s string) (int, bool) {
	rest, ok := strings.CutPrefix(s, "User")
	if !ok {
		return 0, false
	}
	rest = strings.TrimLeft(rest, " \t\n\v\f\r")
	rest = strings.TrimPrefix(rest, "+")
	n, digits := 0, 0
	for ; digits < len(rest) && '0' <= rest[digits] && rest[digits] <= '9'; digits++ {
		n = n*10 + int(rest[digits]-'0')
		if n > keyUserCount {
			return 0, false
		}
	}
	return n, digits > 0
}

type keyStatus int

const (
	keyExact    keyStatus = iota // every modifier given is in the bytes
	keyLossy                     // sent, but a modifier is dropped or nothing is sent
	keyRejected                  // not a key; send-keys types the name
)

func encodeKeyName(name string, m KeyModes) ([]byte, keyStatus) {
	key := lookupKeyName(name)
	if key == kUnknown {
		return nil, keyRejected
	}
	return encodeKey(key, m)
}

// Sequences for named keys, keyed by key code with flags and modifiers:
// input_key_defaults plus the rows input_key_build derives for
// KEYC_BUILD_MODIFIERS.
var keySequences = func() map[keyCode]string {
	t := map[keyCode]string{
		kF1: "\x1bOP", kF2: "\x1bOQ", kF3: "\x1bOR", kF4: "\x1bOS",
		kF5: "\x1b[15~", kF6: "\x1b[17~", kF7: "\x1b[18~", kF8: "\x1b[19~",
		kF9: "\x1b[20~", kF10: "\x1b[21~", kF11: "\x1b[23~", kF12: "\x1b[24~",
		kIC: "\x1b[2~", kDC: "\x1b[3~", kNPage: "\x1b[6~", kPPage: "\x1b[5~",
		kBTab: "\x1b[Z",
		// xterm's Home and End (see EncodeKey); tmux has "\x1b[1~", "\x1b[4~".
		kHome: "\x1b[H", kEnd: "\x1b[F",
		kHome | kCursor: "\x1bOH", kEnd | kCursor: "\x1bOF",

		kUp | kCursor: "\x1bOA", kDown | kCursor: "\x1bOB",
		kRight | kCursor: "\x1bOC", kLeft | kCursor: "\x1bOD",
		kUp: "\x1b[A", kDown: "\x1b[B", kRight: "\x1b[C", kLeft: "\x1b[D",

		kKPSlash | kKeypad: "\x1bOo", kKPStar | kKeypad: "\x1bOj", kKPMinus | kKeypad: "\x1bOm",
		kKP7 | kKeypad: "\x1bOw", kKP8 | kKeypad: "\x1bOx", kKP9 | kKeypad: "\x1bOy",
		kKPPlus | kKeypad: "\x1bOk", kKP4 | kKeypad: "\x1bOt", kKP5 | kKeypad: "\x1bOu",
		kKP6 | kKeypad: "\x1bOv", kKP1 | kKeypad: "\x1bOq", kKP2 | kKeypad: "\x1bOr",
		kKP3 | kKeypad: "\x1bOs", kKPEnter | kKeypad: "\x1bOM", kKP0 | kKeypad: "\x1bOp",
		kKPPeriod | kKeypad: "\x1bOn",

		kKPSlash: "/", kKPStar: "*", kKPMinus: "-", kKP7: "7", kKP8: "8", kKP9: "9",
		kKPPlus: "+", kKP4: "4", kKP5: "5", kKP6: "6", kKP1: "1", kKP2: "2", kKP3: "3",
		kKP0: "0", kKPPeriod: ".",
		// xterm's CR; tmux sends LF (see EncodeKey).
		kKPEnter: "\r",
	}
	modified := map[keyCode]string{
		kF1: "\x1b[1;_P", kF2: "\x1b[1;_Q", kF3: "\x1b[1;_R", kF4: "\x1b[1;_S",
		kF5: "\x1b[15;_~", kF6: "\x1b[17;_~", kF7: "\x1b[18;_~", kF8: "\x1b[19;_~",
		kF9: "\x1b[20;_~", kF10: "\x1b[21;_~", kF11: "\x1b[23;_~", kF12: "\x1b[24;_~",
		kUp: "\x1b[1;_A", kDown: "\x1b[1;_B", kRight: "\x1b[1;_C", kLeft: "\x1b[1;_D",
		kHome: "\x1b[1;_H", kEnd: "\x1b[1;_F", kPPage: "\x1b[5;_~", kNPage: "\x1b[6;_~",
		kIC: "\x1b[2;_~", kDC: "\x1b[3;_~",
	}
	// input_key_modifiers[2..8]: the xterm parameter is the index.
	mods := [...]keyCode{2: kShift, 3: kMeta | kImpliedMeta, 4: kShift | kMeta | kImpliedMeta,
		5: kCtrl, 6: kShift | kCtrl, 7: kMeta | kImpliedMeta | kCtrl, 8: kShift | kMeta | kImpliedMeta | kCtrl}
	for key, seq := range modified {
		for j := 2; j < len(mods); j++ {
			t[key|mods[j]] = strings.Replace(seq, "_", string(rune('0'+j)), 1)
		}
	}
	return t
}()

// encodeKey is input_key for standard key mode.
func encodeKey(key keyCode, m KeyModes) ([]byte, keyStatus) {
	given := key & kMaskMods

	if key&kMaskKey == kBSpace {
		if key&kMaskMods == 0 {
			return []byte{0x7f}, keyExact
		}
		key = 0x7f | key&^kMaskKey
	}
	if key&kMaskKey == kBTab {
		key &^= kMaskMods
	}
	if key&^kMaskKey == 0 {
		if key == '\t' || key == '\r' || key == 0x1b || (key >= 0x20 && key <= 0x7f) {
			return []byte{byte(key)}, keyExact
		}
		if key > 0x7f && !key.isSpecial() {
			return utf8.AppendRune(nil, rune(key)), keyExact
		}
	}

	if !m.AppKeypad {
		key &^= kKeypad
	}
	if !m.AppCursor {
		key &^= kCursor
	}
	seq, found := keySequences[key]
	if !found && key&kMeta != 0 && key&kImpliedMeta == 0 {
		seq, found = keySequences[key&^kMeta]
	}
	if !found && key&kCursor != 0 {
		seq, found = keySequences[key&^kCursor]
	}
	if !found && key&kKeypad != 0 {
		seq, found = keySequences[key&^kKeypad]
	}
	if found {
		var out []byte
		if key&kMeta != 0 && key&kImpliedMeta == 0 {
			out = append(out, 0x1b)
		}
		// Every modifier still on key is in the sequence (a modifier row, or
		// the ESC prefix); only BTab's were cleared above.
		return append(out, seq...), statusOf(given, key&kMaskMods)
	}
	if key.isSpecial() {
		return nil, keyLossy // tmux ignores special keys it has no sequence for
	}
	return encodeVT10x(key, given)
}

// ctrlRemap is input_key_vt10x's table of Ctrl keys that are not simply
// masked to C0.
var ctrlRemap = map[byte]byte{
	'1': '1', '!': '1', '9': '9', '(': '9', '0': '0', ')': '0', '=': '=', '+': '+',
	';': ';', ':': ';', '\'': '\'', '"': '\'', ',': ',', '<': ',', '.': '.', '>': '.',
	'/': 0x1f, '-': 0x1f, '8': 0x7f, '?': 0x7f, ' ': 0, '2': 0,
}

func encodeVT10x(key, given keyCode) ([]byte, keyStatus) {
	var out []byte
	var consumed keyCode
	if key&kMeta != 0 {
		out = append(out, 0x1b)
		consumed |= kMeta
	}
	only := key & kMaskKey
	if only > 0x7f {
		out = utf8.AppendRune(out, rune(only))
		return out, statusOf(given, consumed)
	}
	if only == '\r' || only == '\n' || only == '\t' {
		key &^= kCtrl
	}
	c := byte(only)
	if key&kCtrl != 0 {
		switch r, ok := ctrlRemap[c]; {
		case ok:
			c = r
		case c >= '3' && c <= '7':
			c -= 0x18
		case c >= '@' && c <= '~':
			c &= 0x1f
		default:
			// tmux has already written the Meta ESC here; shelld sends
			// nothing and lets the name be typed instead.
			return nil, keyRejected
		}
		if c < 0x20 || c == 0x7f {
			consumed |= kCtrl
		}
	}
	out = append(out, c&0x7f)
	return out, statusOf(given, consumed)
}

func statusOf(given, consumed keyCode) keyStatus {
	if given&^consumed != 0 {
		return keyLossy
	}
	return keyExact
}
