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

	"github.com/gurupras/shelld"
	"github.com/gurupras/shelld/ws"
)

// Once the daemon has given up writing to a client, it stops reading that client's
// input too, as shelld-rs does: nothing more can reach the client, so whatever it types
// is typed blind, and the read loop used to keep delivering it for the ten seconds it
// otherwise gives a client to answer a close.
//
// The window's program records its raw input to a file while writing dense output
// without end, so a client that never reads wedges on it (see
// TestAClientThatStopsReadingIsDroppedRatherThanPinningEveryOtherClient for why the
// grid is this large). The wedged client sends 'B's only after the tick loop has given
// up; the healthy client's 'C', sent after them, is what bounds the absence check: once
// the program has read it, any 'B' the daemon was going to type has had its chance.
func TestInputAfterTheWriterGivesUpIsNotTyped(t *testing.T) {
	var st *State
	ms := uint64(500)
	sock := bootWith(t, func(s *State) {
		st = s
		s.WSWriteTimeoutMS = &ms
	})
	openWindow(t, sock, "s")
	win := st.Mgr.Session("s").Window("w")

	gaveUp := make(chan struct{})
	// By width as well as window: the healthy client is on the same window, and its
	// giving up instead would say nothing about the wedged one.
	observe := func(w *shelld.Window, cols uint16) {
		if w == win && cols == 500 {
			select {
			case <-gaveUp:
			default:
				close(gaveUp)
			}
		}
	}
	gaveUpObserver.Store(&observe)
	t.Cleanup(func() { gaveUpObserver.Store(nil) })

	healthy, hch := attachAt(t, sock, "s", shelld.MaxCols, shelld.MaxRows)
	go func() {
		for range hch {
		}
	}()
	wedged := openWS(t, sock, "s")
	if err := wedged.WriteMessage(ws.OpText, resizeCtl(500, shelld.MaxRows)); err != nil {
		t.Fatal(err)
	}
	waitSize(t, sock, "s", [2]int{500, shelld.MaxRows})

	out := filepath.Join(t.TempDir(), "input")
	if err := healthy.WriteMessage(ws.OpBinary, fmt.Appendf(nil,
		"stty raw -echo; (while :; do echo \"%s\"; done) & exec cat > %s\n",
		strings.Repeat("abcdefghij", 50), out)); err != nil {
		t.Fatal(err)
	}

	select {
	case <-gaveUp:
	case <-time.After(40 * time.Second):
		t.Fatal("setup: the daemon never gave up on the client that stopped reading")
	}
	for range 5 {
		if err := wedged.WriteMessage(ws.OpBinary, []byte("B")); err != nil {
			// The daemon may have closed the socket already, which is the fix too.
			break
		}
	}
	if err := healthy.WriteMessage(ws.OpBinary, []byte("C")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, "the healthy client's input reaching the program", func() bool {
		b, _ := os.ReadFile(out)
		return bytes.Contains(b, []byte("C"))
	})
	time.Sleep(300 * time.Millisecond)
	if b, _ := os.ReadFile(out); bytes.Contains(b, []byte("B")) {
		t.Errorf("the program read %q: input from a client the daemon had stopped writing to was still typed", b)
	}
}
