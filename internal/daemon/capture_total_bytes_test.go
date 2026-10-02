//go:build linux || darwin

package daemon

import (
	"testing"
)

// A capture answers with the text and with totalBytes, the count of every byte
// the window has written. A client polling totalBytes as its change signal banks
// it: whatever the count covers, it takes as already handed over. So the count
// must not run ahead of the text it came with — a byte counted but not returned
// is a byte the client never comes back for, and no client can detect the loss.
//
// The handler therefore samples the count before it reads the text, where the
// count can only lag. Which order it uses is invisible unless the program writes
// between the two reads, and that window is a few nanoseconds wide: an earlier
// form of this test polled 400 times against a window written to every 30 µs,
// three times over, and never once saw the count even lag — so a test that raced
// for the window would have passed whichever order the handler used. captureGapHook
// is that window made reachable, and the write below is placed in it deliberately.
func TestCaptureTotalBytesNeverRunsAheadOfTheOutput(t *testing.T) {
	var st *State
	sock := bootWith(t, func(s *State) { st = s })
	if code, _, raw := request(t, sock, "POST", "/sessions", `{"name":"s"}`); code != 200 {
		t.Fatal(raw)
	}
	if code, _, raw := request(t, sock, "POST", "/sessions/s/windows",
		`{"name":"w","cols":80,"rows":24,"shell":"sleep 60"}`); code != 200 {
		t.Fatal(raw)
	}
	win := st.Mgr.Session("s").Window("w")
	if got := win.OutputBytes(); got != 0 {
		t.Fatalf("the window's program wrote %d bytes; this test needs a silent one", got)
	}
	sb := win.Scrollback()

	// One write is one line of this length, so a counted-but-unreturned write is 40
	// bytes and cannot hide inside the one-byte slack established below.
	const line = "0123456789abcdefghijklmnopqrstuvwxyz-+*\n"
	const path = "/sessions/s/windows/w/capture?lines=100000&max_bytes=1000000"

	get := func() (output string, total int, truncated bool) {
		t.Helper()
		code, v, raw := request(t, sock, "GET", path, "")
		if code != 200 {
			t.Fatalf("GET capture: status %d: %s", code, raw)
		}
		out, ok1 := v["output"].(string)
		tot, ok2 := v["totalBytes"].(float64)
		trunc, ok3 := v["truncated"].(bool)
		if !ok1 || !ok2 || !ok3 {
			t.Fatalf("GET capture: missing fields: %s", clipRaw(raw))
		}
		return out, int(tot), trunc
	}

	// The premise, with nothing racing: the whole history comes back untruncated and
	// the count exceeds the text by exactly the terminator raw history has not given
	// the newest line yet. That gap is the slack the assertion allows, and it is
	// measured rather than assumed — otherwise a change in either would leave the
	// assertion comparing two things this test does not pin.
	sb.Write([]byte(line + line))
	output, total, truncated := get()
	if truncated {
		t.Fatalf("the premise read is truncated at %d bytes of history; raise max_bytes", total)
	}
	const slack = 1
	if total-len(output) != slack {
		t.Fatalf("at rest the count is %d and the text %d bytes, a gap of %d; this test allows %d, "+
			"so it can no longer tell a counted-but-unreturned write from its own baseline",
			total, len(output), total-len(output), slack)
	}
	restBytes, restTotal := len(output), total

	// Now one write lands between the read of the text and the response: the case the
	// ordering exists for. The count must not cover it.
	gapWrites := 0
	captureGapHook = func() {
		gapWrites++
		if gapWrites == 1 {
			sb.Write([]byte(line))
		}
	}
	t.Cleanup(func() { captureGapHook = nil })
	output, total, truncated = get()
	captureGapHook = nil
	if truncated {
		t.Fatalf("the read is truncated at %d bytes; this comparison needs output to be every "+
			"byte the count covers", total)
	}
	if gapWrites != 1 {
		t.Fatalf("the hook ran %d times, so the write this test is about did not land in the "+
			"window it names", gapWrites)
	}
	// The text cannot contain the gap write — the hook runs after the read — so the
	// premise of the assertion below is that this response is the one the handler
	// built from a history the write had not yet reached.
	if len(output) != restBytes {
		t.Fatalf("the response carries %d bytes against %d at rest, so the gap write reached the "+
			"text too and the hook is not running in the window it claims", len(output), restBytes)
	}
	if ahead := total - len(output) - slack; ahead > 0 {
		t.Fatalf("totalBytes %d covers %d bytes the response did not carry (text %d bytes): the "+
			"count was read after the text, and a client that banks it never asks for them again",
			total, ahead, len(output))
	}

	// And the write the count held back is still there to be fetched, which is what
	// makes lagging the safe direction rather than a second kind of loss.
	output2, total2, _ := get()
	if len(output2) != restBytes+len(line) || total2 != restTotal+len(line) {
		t.Errorf("the next poll returned %d bytes with a count of %d; the write held back by the "+
			"first poll should have brought those to %d and %d", len(output2), total2,
			restBytes+len(line), restTotal+len(line))
	}
}
