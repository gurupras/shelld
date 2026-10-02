//go:build linux || darwin

package daemon

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gurupras/shelld/ws"
)

// rawReader puts the window of session in raw mode with echo off and has od record
// the next n bytes of input, as hex, to the returned file. It returns once the program
// is reading, so every byte a test sends from then on reaches od and nothing else.
func rawReader(t *testing.T, sock, session string, n int) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "keys")
	c, ch := attachAt(t, sock, session, 80, 24)
	defer c.Close()
	// READY is printed split in two in the command, so the tty's echo of the command
	// line cannot be mistaken for it.
	_ = c.WriteMessage(ws.OpBinary, fmt.Appendf(nil,
		"stty raw -echo; printf 'RE''ADY'; od -An -tx1 -N%d > %s\n", n, out))
	var seen []byte
	deadline := time.Now().Add(10 * time.Second)
	for !bytes.Contains(seen, []byte("READY")) {
		if time.Now().After(deadline) {
			t.Fatalf("the window never reached raw mode; saw %q", seen)
		}
		if m, ok := next(ch, 200*time.Millisecond); ok && m.op == ws.OpBinary {
			seen = append(seen, m.data...)
		}
	}
	return out
}

// awaitKeys waits for od to have written its n bytes and returns them as hex.
func awaitKeys(t *testing.T, out string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, err := os.ReadFile(out); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			return hexBytes(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("od never received all of its input, so a frame expected as input was not typed")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Only a text frame that starts 0x01 '{' is a control frame. Each case reads exactly
// as many bytes as it expects typed, ending with a binary 'Z' sent last, so a frame
// that was wrongly typed shows up in od's bytes ahead of the Z, and one that was wrongly
// dropped leaves od short of its count.
//
// Both positions are covered because they are classified by the same function but act
// on the result in two places: the opening frame in serveAttach's handshake and every
// later one in its read loop.
func TestOnlyA0x01BraceTextFrameIsControl(t *testing.T) {
	cases := []struct {
		name   string
		frames []string // text frames, the first of them the opening frame
		want   string
	}{
		{"an opening lone 0x01 is typed", []string{"\x01"}, "01 5a"},
		{"an opening 0x01 0x0b is typed", []string{"\x01\x0b"}, "01 0b 5a"},
		{"an opening unparsable control frame is not typed", []string{"\x01{nope"}, "5a"},
		{"an opening empty control object is not typed", []string{"\x01{}"}, "5a"},
		{
			"later frames are classified the same way",
			[]string{string(resizeCtl(80, 24)), "\x01", "\x01{nope", "\x01{}", "\x01\x0b"},
			"01 01 0b 5a",
		},
	}
	sock := boot(t)
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := fmt.Sprintf("m%d", i)
			openWindow(t, sock, session)
			out := rawReader(t, sock, session, len(strings.Fields(tc.want)))
			c := openWS(t, sock, session)
			for _, f := range tc.frames {
				if err := c.WriteMessage(ws.OpText, []byte(f)); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.WriteMessage(ws.OpBinary, []byte("Z")); err != nil {
				t.Fatal(err)
			}
			if got := awaitKeys(t, out); got != tc.want {
				t.Errorf("the program read %s, want %s for text frames %q", got, tc.want, tc.frames)
			}
		})
	}
}
