package shelld

import (
	"errors"
	"slices"
	"testing"
)

// The table is shelld-rs's for shlex_split_simple (window.rs, shlex_simple
// and the three tests after it), so the two splitters are pinned by the same
// rows.
func TestSplitCommandLineMatchesShelldRs(t *testing.T) {
	ps := `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	flags := ps + " -NoLogo -NoProfile -NonInteractive"
	body := `if (Test-Path -LiteralPath 'C:\T\My Dir\go') { exit 0 }`
	for _, tc := range []struct {
		line string
		want []string
	}{
		{"/bin/bash", []string{"/bin/bash"}},
		{"pwsh.exe -NoLogo -NoProfile", []string{"pwsh.exe", "-NoLogo", "-NoProfile"}},
		{`pwsh.exe -Command "Set-X -Y SaveNothing"`, []string{"pwsh.exe", "-Command", "Set-X -Y SaveNothing"}},
		// ' is a quote: the body is one word and the quotes are consumed.
		{"/bin/sh -c 'exit 7'", []string{"/bin/sh", "-c", "exit 7"}},
		// A quote of the other form inside a region is a literal character,
		// the only way to write one at all.
		{`/bin/sh -c 'echo "hi"'`, []string{"/bin/sh", "-c", `echo "hi"`}},
		{`/bin/sh -c ": > '/t/r'; exit 0"`, []string{"/bin/sh", "-c", ": > '/t/r'; exit 0"}},
		// A region opened mid-word continues the word.
		{"sh --body='a b' x", []string{"sh", "--body=a b", "x"}},
		// A quoted empty word is a word, and keeps `extra` in its place.
		{`sh -c "" extra`, []string{"sh", "-c", "", "extra"}},
		{"''", []string{""}},
		{"a\tb  c", []string{"a", "b", "c"}},
		{"   ", nil},
		{"", nil},
		// Backslash is not an escape: a Windows path keeps it, and only
		// quoting carries a space through.
		{flags + ` -Command "` + body + `"`, []string{ps, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", body}},
		{flags + ` -File C:\T\My Dir\wait.ps1`, []string{ps, "-NoLogo", "-NoProfile", "-NonInteractive", "-File", `C:\T\My`, `Dir\wait.ps1`}},
	} {
		got, err := splitCommandLine(tc.line)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("splitCommandLine(%q) = %q, %v; want %q", tc.line, got, err, tc.want)
		}
	}
}

// An unterminated quote is refused rather than parsed into an argv of another
// shape, naming the quote left open in shelld-rs's words.
func TestSplitCommandLineRefusesAnUnterminatedQuote(t *testing.T) {
	for _, tc := range []struct {
		line string
		want string
	}{
		{`/bin/sh -c "exit 0`, `spawn shell /bin/sh -c "exit 0: unterminated " quote, so no command line was run`},
		{`/bin/sh -c 'exit 0`, `spawn shell /bin/sh -c 'exit 0: unterminated ' quote, so no command line was run`},
	} {
		got, err := splitCommandLine(tc.line)
		var le *CommandLineError
		if !errors.As(err, &le) || err.Error() != tc.want || got != nil {
			t.Errorf("splitCommandLine(%q) = %q, %v; want the error %q", tc.line, got, err, tc.want)
		}
	}
	// The same line closed is a parse, so a splitter refusing everything fails.
	if got, err := splitCommandLine(`/bin/sh -c "exit 0"`); err != nil || !slices.Equal(got, []string{"/bin/sh", "-c", "exit 0"}) {
		t.Errorf("closed line: %q, %v", got, err)
	}
}

// The wiring the splitter tests cannot see: NewWindow has to return the
// refusal itself, since the create route classifies it by type, and a quoted
// empty program must stay ErrEmptyCommand now that the splitter keeps the
// empty word.
func TestNewWindowRefusesAnUnsplittableShell(t *testing.T) {
	w, err := NewWindow(WindowOpts{Name: "bad-quote", Shell: `sh -c "exit 0`})
	var le *CommandLineError
	if w != nil || !errors.As(err, &le) {
		if w != nil {
			w.Kill()
		}
		t.Fatalf("unterminated quote: %v, want a *CommandLineError", err)
	}
	w, err = NewWindow(WindowOpts{Name: "empty-program", Shell: "''"})
	if w != nil || !errors.Is(err, ErrEmptyCommand) {
		if w != nil {
			w.Kill()
		}
		t.Fatalf("quoted empty program: %v, want ErrEmptyCommand", err)
	}
}
