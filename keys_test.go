package shelld

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// tmuxKeyTable is testdata/keys/tmux-3.7c-keys.json: what tmux 3.7c's send-keys
// delivered for each name, recorded by a probe script kept outside this repo. The
// recording names its own provenance and that is the copy to trust -- the file's
// tmux, commit and options fields carry the version, the tag, and the fact that
// the rows were taken with -f /dev/null and extended-keys off.
type tmuxKeyTable struct {
	Tmux   string `json:"tmux"`
	Commit string `json:"commit"`
	Rows   []struct {
		Name string `json:"name"`
		Mode string `json:"mode"` // "none", or "app": DECCKM and DECKPAM set
		Hex  string `json:"hex"`
	} `json:"rows"`
}

func loadTmuxKeys(t *testing.T) tmuxKeyTable {
	t.Helper()
	raw, err := os.ReadFile("testdata/keys/tmux-3.7c-keys.json")
	if err != nil {
		t.Fatal(err)
	}
	var tab tmuxKeyTable
	if err := json.Unmarshal(raw, &tab); err != nil {
		t.Fatal(err)
	}
	if tab.Tmux != "tmux 3.7c" || len(tab.Rows) == 0 {
		t.Fatalf("unexpected table %q with %d rows", tab.Tmux, len(tab.Rows))
	}
	return tab
}

// keyDeviations are the rows where shelld deliberately differs from tmux
// (see EncodeKey), with shelld's bytes per mode.
var keyDeviations = map[string]map[string]string{
	// xterm's Home and End; tmux sends CSI 1~ and CSI 4~.
	"Home": {"none": "\x1b[H", "app": "\x1bOH"},
	"End":  {"none": "\x1b[F", "app": "\x1bOF"},
	// xterm's KPEnter without DECKPAM; tmux sends LF.
	"KPEnter":   {"none": "\r"},
	"kpenter":   {"none": "\r"},
	"M-KPEnter": {"none": "\x1b\r"},
	// tmux writes an ESC for Meta, then types the name it could not encode.
	"C-M-#": {"none": "C-M-#", "app": "C-M-#"}, "C-M-$": {"none": "C-M-$", "app": "C-M-$"},
	"C-M-%": {"none": "C-M-%", "app": "C-M-%"}, "C-M-&": {"none": "C-M-&", "app": "C-M-&"},
	"C-M-*": {"none": "C-M-*", "app": "C-M-*"},

	"C-M-BSpace": {"none": "C-M-BSpace", "app": "C-M-BSpace"}, "C-M-S-BSpace": {"none": "C-M-S-BSpace", "app": "C-M-S-BSpace"},
	"C-M-Escape": {"none": "C-M-Escape", "app": "C-M-Escape"}, "C-M-S-Escape": {"none": "C-M-S-Escape", "app": "C-M-S-Escape"},
}

func TestEncodeKeyNamesMatchesTmux37c(t *testing.T) {
	tab := loadTmuxKeys(t)
	used := map[string]bool{}
	for _, row := range tab.Rows {
		want, err := hex.DecodeString(row.Hex)
		if err != nil {
			t.Fatal(err)
		}
		if dev, ok := keyDeviations[row.Name][row.Mode]; ok {
			want = []byte(dev)
			used[row.Name+" "+row.Mode] = true
		}
		m := KeyModes{AppCursor: row.Mode == "app", AppKeypad: row.Mode == "app"}
		got, err := EncodeKeyNames([]string{row.Name}, m, false)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%q (%s): got %q, tmux %q", row.Name, row.Mode, got, want)
		}
	}
	for name, modes := range keyDeviations {
		for mode := range modes {
			if !used[name+" "+mode] {
				t.Errorf("deviation %q (%s) matches no table row", name, mode)
			}
		}
	}
}

func TestEncodeKeyStrict(t *testing.T) {
	for name, want := range map[string]keyStatus{
		"Enter": keyExact, "C-c": keyExact, "^C": keyExact, "M-x": keyExact, "C-M-a": keyExact,
		"S-Up": keyExact, "C-M-S-F5": keyExact, "M-KP5": keyExact, "BTab": keyExact, "é": keyExact,
		"M-é": keyExact, "0x1b": keyExact, "C-Space": keyExact, "C-?": keyExact, "M-BSpace": keyExact,
		"KP5": keyExact, "[NUL]": keyExact, "Home": keyExact,

		"S-Enter": keyLossy, "C-Enter": keyLossy, "S-a": keyLossy, "C-é": keyLossy, "C-1": keyLossy,
		"C-=": keyLossy, "S-BTab": keyLossy, "S-KP5": keyLossy, "C-KP5": keyLossy, "Any": keyLossy,
		"User0": keyLossy, "S-Tab": keyLossy, "S-Space": keyLossy, "C-Tab": keyLossy, "S-BSpace": keyLossy,

		"Entr": keyRejected, "hello": keyRejected, "Shift+Enter": keyRejected, "None": keyRejected,
		"C-BSpace": keyRejected, "C-M-BSpace": keyRejected, "C-#": keyRejected, "[ESC]": keyRejected,
		"F13": keyRejected, "0x": keyRejected, "X-a": keyRejected, "MouseDown1": keyRejected, "": keyRejected,
	} {
		if _, got := encodeKeyName(name, KeyModes{}); got != want {
			t.Errorf("%q: status %d, want %d", name, got, want)
		}
	}

	_, err := EncodeKeyNames([]string{"Up", "Entr", "S-Enter", "C-c"}, KeyModes{}, true)
	var kerr *KeyNameError
	if !errors.As(err, &kerr) || len(kerr.Unknown) != 1 || kerr.Unknown[0] != "Entr" || len(kerr.Lossy) != 1 || kerr.Lossy[0] != "S-Enter" {
		t.Fatalf("strict error: %v", err)
	}
	if b, err := EncodeKeyNames([]string{"Up", "C-c"}, KeyModes{AppCursor: true}, true); err != nil || string(b) != "\x1bOA\x03" {
		t.Fatalf("strict ok: %q %v", b, err)
	}
	if b, err := EncodeKeyNames([]string{"echo hi", "Enter", "S-Enter", "a\xffb"}, KeyModes{}, false); err != nil || string(b) != "echo hi\r\rab" {
		t.Fatalf("literal fallback: %q %v", b, err)
	}
	if b, ok := EncodeKey("Entr", KeyModes{}); ok || b != nil {
		t.Fatalf("EncodeKey unknown: %q %v", b, ok)
	}
	if b, ok := EncodeKey("Any", KeyModes{}); !ok || len(b) != 0 {
		t.Fatalf("EncodeKey Any: %q %v", b, ok)
	}
}

// TestPasteSanitizeMatchesTmux37c compares shelld's paste bytes with
// testdata/keys/tmux-3.7c-paste.json, what tmux 3.7c's paste-buffer wrote for
// each buffer, recorded by a probe script kept outside this repo. Unlike the key
// table this file carries no provenance of its own -- 56 rows of pane, buffer,
// flags, in and out, and no tmux or commit field -- so the version it was taken
// against is claimed by its name and by this comment and by nothing else.
// Without -p the payload is the sanitized text; with -p it is bracketed only in
// the pane that enabled bracketed paste. (-S is not compared: tmux still maps
// LF to CR there, and a literal shelld paste doesn't.)
func TestPasteSanitizeMatchesTmux37c(t *testing.T) {
	raw, err := os.ReadFile("testdata/keys/tmux-3.7c-paste.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Pane, Buffer, In, Flags, Out string
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	compared := 0
	for _, row := range rows {
		if row.Flags != "" && row.Flags != "-p" {
			continue
		}
		in, err1 := hex.DecodeString(row.In)
		want, err2 := hex.DecodeString(row.Out)
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		bracketed := row.Flags == "-p" && row.Pane == "2004"
		if got := appendPaste(nil, string(in), bracketed, false); !bytes.Equal(got, want) {
			t.Errorf("%s %s %q: got %q, tmux %q", row.Pane, row.Buffer, row.Flags, got, want)
		}
		compared++
	}
	if compared != 28 {
		t.Errorf("compared %d rows, want 28", compared)
	}
	if got := appendPaste(nil, "a\x1b[201~\n\x03", true, true); string(got) != "\x1b[200~a\x1b[201~\n\x03\x1b[201~" {
		t.Errorf("literal: %q", got)
	}
}
