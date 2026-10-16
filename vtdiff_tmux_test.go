package shelld

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gurupras/shelld/vt"
)

// tmuxInput reads one of tmux's regress inputs (vt/testdata/tmux) and applies
// the pty's onlcr, which turns each bare \n the program wrote into \r\n before
// tmux, or here the grid, reads it.
func tmuxInput(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("vt", "testdata", "tmux", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
}

// TestVtDiffTmuxRegress feeds tmux's own regress inputs to the grid and to
// alacritty and compares the result. tmux's .result files are not the oracle:
// they encode tmux's width tables and capture format, and this grid's reference
// is alacritty. The inputs are what is worth reusing: Markus Kuhn's UTF-8 stress
// file, combining marks, SGR resets and the cursor-test resize sequences.
//
// Each input runs at the size its tmux script uses and at small sizes whose
// screen it overflows, so that every line of it passes through history and
// through a reflowing resize, which is where history rows are stored packed.
func TestVtDiffTmuxRegress(t *testing.T) {
	bin := buildRsdump(t)
	type tc struct {
		name, file string
		cols, rows int
		post       string
		resizes    [][2]int
		// want names the pack counters the case must move: the cases at tmux's own
		// sizes whose input fits the screen never reach history and want none.
		want string
	}
	cases := []tc{
		// regress/utf8-test.sh: new-window -x 80 -y 25 (the window's size)
		{name: "utf8", file: "UTF-8-test.txt", cols: 80, rows: 25, want: "Packed Ext"},
		{name: "utf8-small", file: "UTF-8-test.txt", cols: 20, rows: 5, resizes: [][2]int{{13, 5}, {41, 7}, {7, 3}, {80, 25}}, want: "Packed Ext Settles"},
		{name: "combine", file: "combine-test.bin", cols: 80, rows: 25},
		{name: "combine-small", file: "combine-test.bin", cols: 10, rows: 3, resizes: [][2]int{{4, 3}, {23, 6}, {80, 25}}, want: "Packed Ext Settles"},
		{name: "sgr0", file: "capture-pane-sgr0.bin", cols: 80, rows: 25},
		{name: "sgr0-small", file: "capture-pane-sgr0.bin", cols: 5, rows: 2, resizes: [][2]int{{3, 2}, {11, 4}}, want: "Packed Settles"},
		// regress/cursor-test1.sh: 40x10, cursor 9;15, resizew -x10, -x50.
		{name: "cursor1", file: "cursor-test.txt", cols: 40, rows: 10, post: "\x1b[9;15H", resizes: [][2]int{{10, 10}, {50, 10}}, want: "Packed Settles"},
		// regress/cursor-test2.sh: 10x10, cursor 8;10, resizew -x5, -x50.
		{name: "cursor2", file: "cursor-test.txt", cols: 10, rows: 10, post: "\x1b[8;10H", resizes: [][2]int{{5, 10}, {50, 10}}, want: "Packed Settles"},
		{name: "cursor-small", file: "cursor-test.txt", cols: 8, rows: 3, post: "\x1b[2;7H", resizes: [][2]int{{3, 3}, {50, 3}, {8, 2}}, want: "Packed Settles"},
	}
	// regress/cursor-test3.sh: 7x2, abcdefabcdefab, cursor 2;7, resizew -x5, -x7.
	// regress/cursor-test4.sh: 10x3, abcdef\n, resizew -x20, -x3, -x10.
	inline := []struct {
		name       string
		cols, rows int
		data       string
		resizes    [][2]int
	}{
		{"cursor3", 7, 2, "abcdefabcdefab\x1b[2;7H", [][2]int{{5, 2}, {7, 2}}},
		{"cursor4", 10, 3, "abcdef\r\n", [][2]int{{20, 3}, {3, 3}, {10, 3}}},
	}
	run := func(name string, cols, rows int, data []byte, resizes [][2]int, seed int64, want string) {
		o := newOps(cols, rows)
		if seed == 0 {
			o.feed(data)
		} else {
			// Random splits put every chunk boundary inside a UTF-8 sequence somewhere.
			rng := rand.New(rand.NewSource(seed))
			for len(data) > 0 {
				k := 1 + rng.Intn(min(len(data), 64))
				o.feed(data[:k])
				data = data[k:]
			}
		}
		o.dump()
		for _, rs := range resizes {
			o.resize(rs[0], rs[1])
			o.dump()
		}
		var s vt.PackStats
		old := vt.SetPackStats(&s)
		compare(t, name, o.Bytes(), bin)
		vt.SetPackStats(old)
		t.Logf("%s: packed %d (ext %d), plain %d, settles %d", name, s.Packed, s.Ext, s.Plain, s.Settles)
		got := map[string]int{"Packed": s.Packed, "Ext": s.Ext, "Settles": s.Settles}
		for _, c := range strings.Fields(want) {
			if got[c] == 0 {
				t.Errorf("%s: pack counter %s is 0: %+v", name, c, s)
			}
		}
	}
	for _, c := range cases {
		data := append(tmuxInput(t, c.file), c.post...)
		run(c.name, c.cols, c.rows, data, c.resizes, 0, c.want)
		run(c.name+"-split", c.cols, c.rows, data, c.resizes, 7, c.want)
	}
	for _, c := range inline {
		// Two lines at most: they never reach history, and want nothing.
		run(c.name, c.cols, c.rows, []byte(c.data), c.resizes, 0, "")
	}
	fmt.Fprintln(os.Stderr, "tmux regress inputs compared:", len(cases)*2+len(inline))
}
