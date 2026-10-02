package shelld

import (
	"math"
	"regexp"
	"strings"
	"testing"
	"time"
)

// enter_delay_ms is the same unsigned-millisecond-to-signed-Duration conversion as
// StallTimeoutMS, so it has the same two silent failures past the range, and the
// table is deliberately the same shape as TestStallTimeoutClampsInsteadOfWrapping.
// What makes the wrap worse here than a wrong number is the direction: both
// arbitrary results are SHORTER gaps, and the caller passing enter_delay_ms is
// asking for a gap because something downstream needs the text to land before the
// Enter does.
func TestEnterDelayClampsInsteadOfWrapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		ms   uint64
		want time.Duration
	}{
		// An ordinary delay and the no-delay default are untouched.
		{"zero is no gap", 0, 0},
		{"an ordinary delay", 250, 250 * time.Millisecond},
		// MaxInputGap is the longest gap a window holds (shelld-rs's MAX_INPUT_GAP),
		// so every row past it lands on it rather than on the conversion's maximum.
		{"just under MaxInputGap", 59_999, 59_999 * time.Millisecond},
		{"MaxInputGap itself", 60_000, MaxInputGap},
		{"just past MaxInputGap", 60_001, MaxInputGap},
		{"the largest value that converts", maxStallMS, MaxInputGap},
		// Unclamped, about -2562047h, and writeInput's `c.gap > 0` reads a negative
		// gap as no gap at all, so the Enter lands immediately behind the text.
		{"just past the limit", maxStallMS + 1, MaxInputGap},
		// Unclamped, 448.384µs: a caller asking for a very long gap gets one shorter
		// than any it could have asked for directly. Which input lands here rather
		// than on the negative above follows from the low bits of the multiply and
		// not from the input's size, so both are measured and both need a row — and
		// only this one survives the positivity check below.
		{"a value that wraps to a usable-looking gap", 18_446_744_073_710, MaxInputGap},
		// Unclamped, -1ms: no gap again.
		{"the largest uint64", math.MaxUint64, MaxInputGap},
	} {
		if got := enterDelay(tc.ms); got != tc.want {
			t.Errorf("%s: enterDelay(%d) = %v, want %v", tc.name, tc.ms, got, tc.want)
		}
	}
	// The clamp has to stay on the side of the comparison writeInput reads as a
	// gap, which is what the negative wrap crossed.
	if enterDelay(math.MaxUint64) <= 0 {
		t.Error("the clamped gap is not positive, so writeInput skips the wait entirely")
	}
	// Monotone over the interesting boundary: bigger asks must not get shorter
	// waits, which is the whole property the wrap broke.
	if enterDelay(maxStallMS+1) < enterDelay(maxStallMS) {
		t.Error("asking for a longer gap produced a shorter one")
	}
}

// hasControl decides whether a scanned line can be trusted as rendering to itself,
// and renderCR gives up on more than the C0 controls: every rune uniwidth gives no
// width. A line that hasControl passes is read verbatim by IsIdleScan, so a rune
// misjudged here does not render wrong, it gets believed.
func TestHasControlCoversEveryRuneRenderCRGivesUp(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    string
		want bool
	}{
		// U+0085 (NEL) is the sharp case: the one rune unicode.IsSpace calls a
		// space while uniwidth gives it no width, so a line holding it was both
		// trusted and treated as blank, and the idle scan walked back past a
		// running program's output to the previous prompt.
		{"NEL alone", "\u0085", true},
		{"NEL in a prompt-shaped line", "user@host:~$ \u0085", true},
		// The rest of the zero-width and C1 population renderCR also refuses.
		{"DEL", "a\x7fb", true},
		{"a C1 control", "a\u0090b", true},
		{"a combining mark", "áb", true},
		{"a zero-width space", "a​b", true},
		{"a zero-width joiner", "a‍b", true},
		// C0, which the old predicate did catch, must keep being caught.
		{"a C0 control", "a\x01b", true},
		{"a tab", "a\tb", true},
		{"a carriage return", "a\rb", true},
		// And the printables must stay trusted, or every line becomes untrusted
		// and idle detection stops working at all. The wide runes matter here:
		// they have width 2, not 0, and renderCR renders them exactly.
		{"ASCII", "user@host:~/src$ ", false},
		{"a space", " ", false},
		{"empty", "", false},
		{"a wide CJK rune", "cd 日本語", false},
		{"an emoji", "ok 👍", false},
		{"latin-1 with accents", "café", false},
		{"a box-drawing rune", "├─ tree", false},
	} {
		if got := hasControl(tc.s); got != tc.want {
			t.Errorf("%s: hasControl(%q) = %v, want %v", tc.name, tc.s, got, tc.want)
		}
	}
}

// RustRegex's bracket tracking decides which form a \s becomes, so losing track of
// it changes the language the compiled pattern matches. Nothing downstream can
// notice: the output still compiles, and an idle pattern that matches the wrong
// thing either fires on output lines or never fires.
func TestRustRegexKeepsAPOSIXClassInsideItsBrackets(t *testing.T) {
	// A POSIX class holds three '[' and ']' between the outer brackets, and a flag
	// toggled by each of them read as "outside the class" by the time it reached
	// the \s. The bracketed form then nested inside the class it thought it had
	// left, and Go closes the class at the first unescaped ']' — so the pattern
	// silently grew a trailing literal ']'.
	t.Run("the class and the shorthand are one class", func(t *testing.T) {
		got := RustRegex(`^[[:alpha:]\s]+$`)
		if want := "^[[:alpha:]" + rustSpaceClass + "]+$"; got != want {
			t.Errorf("RustRegex = %q, want %q", got, want)
		}
		re, err := regexp.Compile(got)
		if err != nil {
			t.Fatalf("compiling %q: %v", got, err)
		}
		// The language, not the text: letters and spaces only, and nothing with
		// the stray ']' the old rewrite required.
		for _, s := range []string{"ab cd", "a", " ", "a b"} {
			if !re.MatchString(s) {
				t.Errorf("%q should match %q", got, s)
			}
		}
		for _, s := range []string{"ab]", "a]", "a1", "]"} {
			if re.MatchString(s) {
				t.Errorf("%q should not match %q", got, s)
			}
		}
	})

	// A negated class is the same shape with one more character before the name.
	t.Run("a negated POSIX class", func(t *testing.T) {
		got := RustRegex(`^[[:^alpha:]\s]+$`)
		if want := "^[[:^alpha:]" + rustSpaceClass + "]+$"; got != want {
			t.Errorf("RustRegex = %q, want %q", got, want)
		}
		if _, err := regexp.Compile(got); err != nil {
			t.Fatalf("compiling %q: %v", got, err)
		}
	})

	// An unknown name is passed through so the error Go reports is the user's
	// pattern rather than this rewrite's output.
	t.Run("an unknown class name is not rewritten", func(t *testing.T) {
		in := `[[:bogus:]]`
		if got := RustRegex(in); got != in {
			t.Errorf("RustRegex(%q) = %q, want it unchanged", in, got)
		}
	})

	// The controls: a '[' that opens no class is an ordinary character inside one
	// and must not be treated as nesting, and a \s outside every class still takes
	// the bracketed form. Both of these worked before and the fix must not cost
	// them.
	t.Run("the forms either side of a class are unchanged", func(t *testing.T) {
		for in, want := range map[string]string{
			`[\s]`:       "[" + rustSpaceClass + "]",
			`\s`:         "[" + rustSpaceClass + "]",
			`\w+`:        "[" + rustWordClass + "]+",
			`[[\s]`:      "[[" + rustSpaceClass + "]",
			`[a-z\w]`:    "[a-z" + rustWordClass + "]",
			`\[\s`:       `\[` + "[" + rustSpaceClass + "]",
			`^PS [^>]`:   `^PS [^>]`,
			`[[:digit:]`: `[[:digit:]`,
		} {
			if got := RustRegex(in); got != want {
				t.Errorf("RustRegex(%q) = %q, want %q", in, got, want)
			}
		}
	})

	// The patterns actually shipped still compile and still match a prompt, which
	// is the only reason any of this exists.
	//
	// The match is what makes this more than a compile check, and it was missing:
	// a translation that DELETED a shorthand instead of rewriting it leaves no \s
	// or \w behind and still compiles, so the two checks above pass on a pattern
	// that matches nothing. The non-prompt line is the other direction, against a
	// translation broad enough to match a prompt with a command typed after it —
	// the state shelld reads as idle, so a pattern that matched it would report a
	// busy shell as idle rather than merely failing to detect one.
	t.Run("the shipped idle patterns", func(t *testing.T) {
		for _, tc := range []struct {
			shell, prompt, notPrompt string
		}{
			{"/bin/bash", "$ ", "$ ls -la"},
			{"/bin/zsh", "  % ", "% cd /tmp"},
			{"/usr/bin/fish", "~/work> ", "~/work> make"},
			{"pwsh.exe", `PS C:\Users\me> `, `PS C:\Users\me> dir`},
		} {
			p := RustRegex(IdlePatternForShell(tc.shell))
			if strings.Contains(p, `\s`) || strings.Contains(p, `\w`) {
				t.Errorf("%s: %q still holds a Rust shorthand", tc.shell, p)
			}
			re, err := regexp.Compile(p)
			if err != nil {
				t.Errorf("%s: compiling %q: %v", tc.shell, p, err)
				continue
			}
			if !re.MatchString(tc.prompt) {
				t.Errorf("%s: %q does not match the prompt %q", tc.shell, p, tc.prompt)
			}
			if re.MatchString(tc.notPrompt) {
				t.Errorf("%s: %q matches %q, which is that prompt with a command typed "+
					"after it", tc.shell, p, tc.notPrompt)
			}
		}
	})
}

// The other half of the bracket language, and the half RustRegex's first fix left
// wrong: a ']' in the first position of a class body is a literal ']', not the
// terminator, so the class it appears to close is still open. Rust says so at
// regex-syntax 0.8.11 ast/parse.rs:2059 ("If `]` is the *first* char in a set, then
// interpret it as a literal `]`") and Go agrees, which is what makes treating it as
// a terminator a translation error rather than a difference of dialect.
//
// The manifestation needs a \s, a \w or a '[' after the false close and before the
// real end of the class; before that the output is the input and nothing is wrong.
// After it, the shorthand takes the outside-a-class bracketed form nested in the
// class it believed it had left, Go closes at the next ']' and the pattern silently
// grows a trailing literal ']' — a two-character language where the user wrote a
// one-character one. It compiles either way.
func TestRustRegexTreatsALeadingBracketCloseAsALiteral(t *testing.T) {
	// The four shapes, each with what the user asked for spelled out as a Go class
	// that needs no rewriting. A leading '-' is deliberately not here: Rust accepts
	// any run of '-' as literals *before* this rule (parse.rs:2046) and pushes them,
	// so union.items is no longer empty and '[-]' really does close at the ']'.
	for _, tc := range []struct {
		in, want string
	}{
		{`[]\s]`, `[]` + rustSpaceClass + `]`},
		{`[^]\s]`, `[^]` + rustSpaceClass + `]`},
		{`[]\w]`, `[]` + rustWordClass + `]`},
		{`[][:alpha:]\s]`, `[][:alpha:]` + rustSpaceClass + `]`},
	} {
		got := RustRegex(tc.in)
		if got != tc.want {
			t.Errorf("RustRegex(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if _, err := regexp.Compile(got); err != nil {
			t.Errorf("compiling %q: %v", got, err)
		}
	}

	// The language, which is the thing that was wrong: one character, either a ']'
	// or a space. The old output required two.
	t.Run("the matched language", func(t *testing.T) {
		re, err := regexp.Compile(RustRegex(`^[]\s]$`))
		if err != nil {
			t.Fatalf("compiling: %v", err)
		}
		for _, s := range []string{"]", " ", "\t"} {
			if !re.MatchString(s) {
				t.Errorf("%q should match %q", re, s)
			}
		}
		for _, s := range []string{"]]", "] ", "a", "", "[["} {
			if re.MatchString(s) {
				t.Errorf("%q should not match %q", re, s)
			}
		}
	})

	// Controls. A ']' that is not in the first position still closes, so the rule
	// must not swallow every ']' after a '['; and a class whose leading ']' is
	// followed by nothing to rewrite comes back unchanged, which is why the defect
	// was invisible.
	t.Run("a ']' elsewhere still closes", func(t *testing.T) {
		for in, want := range map[string]string{
			`[a]\s`:   `[a]` + "[" + rustSpaceClass + "]",
			`[-]\s`:   `[-]` + "[" + rustSpaceClass + "]",
			`[^a]\s`:  `[^a]` + "[" + rustSpaceClass + "]",
			`[]]`:     `[]]`,
			`[^]]`:    `[^]]`,
			`[]a]`:    `[]a]`,
			`[]`:      `[]`,
			`[^]`:     `[^]`,
			`\[]\s`:   `\[]` + "[" + rustSpaceClass + "]",
			`[]\s]\s`: `[]` + rustSpaceClass + `]` + "[" + rustSpaceClass + "]",
		} {
			if got := RustRegex(in); got != want {
				t.Errorf("RustRegex(%q) = %q, want %q", in, got, want)
			}
		}
	})
}
