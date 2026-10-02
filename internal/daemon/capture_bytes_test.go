//go:build linux || darwin

package daemon

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// captureLines is how many lines the byte cap has to be measured against, and
// captureLineBytes the size of each. A line of raw history runs to 64 KiB, so
// twenty of them are 1.2 MB where fifty lines of a program that ends a line at
// every read were 200 KiB — the bound the cap restores.
const (
	captureLines     = 20
	captureLineBytes = 60_000
)

// A capture asks for lines, and a line of raw history is bounded at 64 KiB
// rather than at the 4 KiB a PTY read used to bound it. Without a byte cap
// GET …/capture?lines=50 can hand back 3.2 MB over a socket, so the cap is what
// keeps the response the size a client asked for, and "truncated" is how the
// client learns it did not get everything.
//
// The window's program writes nothing, so raw history holds exactly the stream
// this writes and every size below is a known number.
func TestCaptureByteCap(t *testing.T) {
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
	var lines []string
	for i := range captureLines {
		line := strings.Repeat(string(rune('a'+i)), captureLineBytes)
		lines = append(lines, line)
		sb.Write([]byte(line + "\n"))
	}
	whole := strings.Join(lines, "\n")

	get := func(path string) (output string, truncated bool, v map[string]any) {
		t.Helper()
		code, v, raw := request(t, sock, "GET", path, "")
		if code != 200 {
			t.Fatalf("GET %s: status %d: %s", path, code, raw)
		}
		out, ok := v["output"].(string)
		if !ok {
			t.Fatalf("GET %s: no output: %s", path, clipRaw(raw))
		}
		trunc, ok := v["truncated"].(bool)
		if !ok {
			t.Fatalf("GET %s: no truncated flag: %s", path, clipRaw(raw))
		}
		return out, trunc, v
	}

	// The default bound is what fifty lines cost when a line ended at every
	// 4 KiB read, and the lines it keeps are the newest ones: a capture is a
	// caller asking what just happened.
	output, truncated, v := get("/sessions/s/windows/w/capture?lines=50")
	if len(output) > 50*4096 || !truncated {
		t.Errorf("a default capture returned %d bytes, truncated %v; want at most %d and truncated",
			len(output), truncated, 50*4096)
	}
	if !strings.HasSuffix(whole, output) {
		t.Errorf("a bounded capture is not the end of the history: it starts %q", output[:min(40, len(output))])
	}
	if got, want := v["totalLines"], float64(captureLines); got != want {
		t.Errorf("totalLines %v, want %v", got, want)
	}
	if got, want := v["totalBytes"], float64(captureLines*(captureLineBytes+1)); got != want {
		t.Errorf("totalBytes %v, want %v: every byte written, terminators included", got, want)
	}
	// A capture says how many lines it returned, which is fewer than the fifty
	// asked for because the byte bound dropped the older ones. totalLines counts
	// lines started over the window's life, so it cannot answer this.
	if got, want := v["linesReturned"], float64(len(strings.Split(output, "\n"))); got != want {
		t.Errorf("linesReturned %v, want %v", got, want)
	} else if got == float64(50) || got == v["totalLines"] {
		t.Errorf("linesReturned %v: a bounded capture of a history this long returned neither "+
			"the fifty lines asked for nor all %v, so this figure is not distinguishing anything",
			got, v["totalLines"])
	}

	// max_bytes raises or lowers the bound, and a bound past the history costs
	// nothing and reports nothing.
	output, truncated, _ = get("/sessions/s/windows/w/capture?lines=50&max_bytes=1000000")
	if len(output) > 1_000_000 || !truncated || !strings.HasSuffix(whole, output) {
		t.Errorf("max_bytes=1000000 returned %d bytes, truncated %v", len(output), truncated)
	}
	output, truncated, _ = get("/sessions/s/windows/w/capture?lines=50&max_bytes=2000000")
	if output != whole || truncated {
		t.Errorf("a bound past the history returned %d bytes of %d, truncated %v", len(output), len(whole), truncated)
	}
	// A bound above the ceiling is the ceiling, not an error and not an
	// allocation the size of the request.
	output, truncated, _ = get("/sessions/s/windows/w/capture?lines=50&max_bytes=99999999999")
	if output != whole || truncated {
		t.Errorf("max_bytes over the ceiling returned %d bytes, truncated %v", len(output), truncated)
	}
	if _, _, raw := request(t, sock, "GET", "/sessions/s/windows/w/capture?max_bytes=nope", ""); !strings.Contains(raw, "invalid digit") {
		t.Errorf("a non-numeric max_bytes: %s", clipRaw(raw))
	}

	// mode=visible reads the grid, which is bounded by the window's size, so it
	// takes no byte bound and grows no key.
	code, v, raw := request(t, sock, "GET", "/sessions/s/windows/w/capture?mode=visible&lines=10", "")
	if code != 200 {
		t.Fatal(raw)
	}
	for _, key := range []string{"truncated", "linesReturned"} {
		if _, ok := v[key]; ok {
			t.Errorf("mode=visible reports %q: %s", key, clipRaw(raw))
		}
	}

	// Paging with a bound has to visit every line exactly once: the client adds
	// the lines the response says it returned to its offset, so that count is what
	// makes paging sound. It is read from the response here rather than recovered
	// from the text, because the response is all a client outside this process has.
	// The bound is max(max_bytes, utf8.UTFMax), not max_bytes: a page returns a
	// whole first rune even when that rune is over the bound, so a client can
	// always advance. Every bound swept here is far above utf8.UTFMax, where the
	// max() collapses and len(output) <= max_bytes is unconditional, so the
	// concession is written as the contract rather than exercised here — the band
	// where it is spendable is swept separately below.
	for _, maxBytes := range []int{captureLineBytes, 200_000, 1 << 20} {
		var got []string
		for offset := 0; offset < captureLines; {
			path := fmt.Sprintf("/sessions/s/windows/w/scroll?offset=%d&lines=1000&max_bytes=%d", offset, maxBytes)
			output, _, v := get(path)
			if len(output) > max(maxBytes, utf8.UTFMax) {
				t.Fatalf("max_bytes %d: the page at %d is %d bytes", maxBytes, offset, len(output))
			}
			if got, want := v["totalLines"], float64(captureLines); got != want {
				t.Fatalf("max_bytes %d: totalLines %v, want %v", maxBytes, got, want)
			}
			returned, ok := v["linesReturned"].(float64)
			if !ok || returned < 1 {
				t.Fatalf("max_bytes %d: the page at %d reports linesReturned %v, so a client "+
					"cannot advance", maxBytes, offset, v["linesReturned"])
			}
			page := strings.Split(output, "\n")
			if float64(len(page)) != returned {
				t.Fatalf("max_bytes %d: the page at %d reports %v lines and holds %d",
					maxBytes, offset, returned, len(page))
			}
			// lineBytes is how long the line at the offset renders, and a client
			// derives "this page is a fragment" from lineBytes > len(output). Every
			// bound here is above that line's length, so the page starts at a line
			// boundary and the derivation has to come out false. This fixture's lines
			// are all captureLineBytes long, so this pins the derivation and not which
			// line was measured; the band below has lines of four lengths and pins
			// that.
			lineBytes, ok := v["lineBytes"].(float64)
			if !ok || lineBytes != float64(len(page[0])) {
				t.Fatalf("max_bytes %d: the page at %d reports lineBytes %v and begins with %d bytes "+
					"of a whole line", maxBytes, offset, v["lineBytes"], len(page[0]))
			}
			got = append(got, page...)
			offset += int(returned)
		}
		if len(got) != captureLines {
			t.Fatalf("max_bytes %d: paging returned %d lines, want %d", maxBytes, len(got), captureLines)
		}
		for i, line := range got {
			if line != lines[i] {
				t.Fatalf("max_bytes %d: page line %d is %d bytes of %q, want %d of %q",
					maxBytes, i, len(line), first(line), len(lines[i]), first(lines[i]))
			}
		}
	}
	// A page keeps its oldest lines, since a client paging forwards continues
	// from where the page ended.
	output, truncated, _ = get("/sessions/s/windows/w/scroll?offset=0&lines=1000&max_bytes=200000")
	if !truncated || !strings.HasPrefix(whole, output) {
		t.Errorf("a bounded page is not the start of the history: truncated %v, %d bytes", truncated, len(output))
	}

	// max_bytes of 1, 2 or 3 is the only band where a page can come back over the
	// bound, and only for a line whose first rune is longer than the bound. Nothing
	// in this repo sends such a bound, but max_bytes is clamped only at the top, so
	// /scroll?max_bytes=2 reaches RangeBytes unchanged. Swept here on its own
	// fixture, because a page in this band cannot reproduce a line and the sweep
	// above asserts that it does.
	if code, _, raw := request(t, sock, "POST", "/sessions/s/windows",
		`{"name":"n","cols":80,"rows":24,"shell":"sleep 60"}`); code != 200 {
		t.Fatal(raw)
	}
	narrow := st.Mgr.Session("s").Window("n")
	// "𝒳" is four bytes, so it is over every bound in the band. The lines are of
	// four different lengths so that a lineBytes naming the wrong line of a page
	// is a wrong number rather than the right one by coincidence.
	var runeLines []string
	for i := range 4 {
		line := "𝒳" + strings.Repeat(string(rune('a'+i)), i+1)
		runeLines = append(runeLines, line)
		narrow.Scrollback().Write([]byte(line + "\n"))
	}
	for _, maxBytes := range []int{1, 2, 3} {
		var pages int
		for offset := 0; offset < len(runeLines); {
			path := fmt.Sprintf("/sessions/s/windows/n/scroll?offset=%d&lines=1000&max_bytes=%d", offset, maxBytes)
			output, truncated, v := get(path)
			// The concession is one rune, not one rune per line: the bound may be
			// exceeded by at most utf8.UTFMax-1 bytes however long the history is.
			if len(output) > max(maxBytes, utf8.UTFMax) {
				t.Fatalf("max_bytes %d: the page at %d is %d bytes, over the bound by more than one rune",
					maxBytes, offset, len(output))
			}
			if output == "" {
				t.Fatalf("max_bytes %d: the page at %d is empty while %d lines remain, so a client cannot advance",
					maxBytes, offset, len(runeLines)-offset)
			}
			if !strings.HasPrefix(runeLines[offset], output) {
				t.Fatalf("max_bytes %d: the page at %d is %q, which is not the start of %q",
					maxBytes, offset, output, runeLines[offset])
			}
			if !truncated {
				t.Fatalf("max_bytes %d: the page at %d returned %d of %d bytes and reports truncated false",
					maxBytes, offset, len(output), len(runeLines[offset]))
			}
			returned, ok := v["linesReturned"].(float64)
			if !ok || returned != 1 {
				t.Fatalf("max_bytes %d: the page at %d reports linesReturned %v, want 1",
					maxBytes, offset, v["linesReturned"])
			}
			// This band is the only place a page over the wire is a fragment, so it
			// is where lineBytes earns its keep: it names the bound that would return
			// the line whole, which truncated cannot, and it is what makes
			// lineBytes > len(output) — the fragment test — true here and false in
			// the sweep above.
			lineBytes, ok := v["lineBytes"].(float64)
			if !ok || lineBytes != float64(len(runeLines[offset])) {
				t.Fatalf("max_bytes %d: the page at %d reports lineBytes %v, want %d, the rendered "+
					"length of %q", maxBytes, offset, v["lineBytes"], len(runeLines[offset]), runeLines[offset])
			}
			if lineBytes <= float64(len(output)) {
				t.Fatalf("max_bytes %d: the page at %d is %d bytes of a line reported as %v, so a client "+
					"cannot tell it holds part of a line", maxBytes, offset, len(output), lineBytes)
			}
			offset += int(returned)
			pages++
		}
		// Every line is visited exactly once even where none can be reproduced,
		// which is the paging guarantee the byte bound does not weaken.
		if pages != len(runeLines) {
			t.Fatalf("max_bytes %d: paging took %d pages over %d lines", maxBytes, pages, len(runeLines))
		}
	}

	// A tail read is anchored at the newest line and that is the only line its
	// bound can cut into, so lineBytes describes that line rather than the page's
	// first. Both sides are asserted on one fixture, because the field is only
	// useful if the same comparison — lineBytes > len(output) — separates them.
	newest := runeLines[len(runeLines)-1]
	output, truncated, v = get("/sessions/s/windows/n/capture?lines=1&max_bytes=3")
	lineBytes, ok := v["lineBytes"].(float64)
	if !ok || lineBytes != float64(len(newest)) {
		t.Errorf("a cut capture reports lineBytes %v, want %d, the rendered length of the newest line %q",
			v["lineBytes"], len(newest), newest)
	}
	if !truncated || lineBytes <= float64(len(output)) || !strings.HasSuffix(newest, output) {
		t.Errorf("a capture bounded to 3 bytes returned %q, truncated %v, lineBytes %v; want the end of "+
			"%q reported as part of a longer line", output, truncated, v["lineBytes"], newest)
	}
	output, truncated, v = get("/sessions/s/windows/n/capture?lines=1000&max_bytes=1000000")
	if lineBytes, _ := v["lineBytes"].(float64); truncated || lineBytes != float64(len(newest)) ||
		lineBytes >= float64(len(output)) {
		t.Errorf("an unbounded capture of %d lines returned %d bytes, truncated %v, lineBytes %v; want the "+
			"newest line's %d bytes, which is less than the whole text and so is not a fragment",
			len(runeLines), len(output), truncated, v["lineBytes"], len(newest))
	}
}

// first is a line's opening characters: every line here is one repeated
// character, so that is what tells two of them apart.
func first(line string) string {
	if len(line) > 8 {
		return line[:8] + "..."
	}
	return line
}

func clipRaw(raw string) string {
	if len(raw) > 200 {
		return raw[:100] + "..." + raw[len(raw)-100:]
	}
	return raw
}
