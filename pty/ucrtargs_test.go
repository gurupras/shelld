package pty

import (
	"strings"
	"testing"
)

// ucrtArgs splits a Windows command line the way the Universal CRT's startup
// code does (the post-2008 rule). Inside quotes, "" is a literal quote and
// quoting continues. Go's own os/exec_windows.go splitting uses the older
// rule, where "" also ends the quoted part, so os.Args can differ from what a
// UCRT program such as node receives; the Windows batch test parses
// GetCommandLine with this instead.
func ucrtArgs(cmdline string) []string {
	var args []string
	s := cmdline
	// The program name: up to the next quote when quoted, else to white
	// space; no escapes.
	if strings.HasPrefix(s, `"`) {
		end := strings.IndexByte(s[1:], '"')
		if end < 0 {
			return []string{s[1:]}
		}
		args = append(args, s[1:1+end])
		s = s[2+end:]
	} else {
		end := strings.IndexAny(s, " \t")
		if end < 0 {
			return []string{s}
		}
		args = append(args, s[:end])
		s = s[end:]
	}
	var cur strings.Builder
	inQ, have := false, false
	for i := 0; i < len(s); {
		c := s[i]
		if !inQ && (c == ' ' || c == '\t') {
			if have {
				args = append(args, cur.String())
				cur.Reset()
				have = false
			}
			i++
			continue
		}
		have = true
		switch c {
		case '\\':
			n := 0
			for i < len(s) && s[i] == '\\' {
				n++
				i++
			}
			if i < len(s) && s[i] == '"' {
				cur.WriteString(strings.Repeat(`\`, n/2))
				if n%2 == 1 {
					cur.WriteByte('"')
					i++
				}
			} else {
				cur.WriteString(strings.Repeat(`\`, n))
			}
		case '"':
			if inQ && i+1 < len(s) && s[i+1] == '"' {
				cur.WriteByte('"')
				i += 2
				continue
			}
			inQ = !inQ
			i++
		default:
			cur.WriteByte(c)
			i++
		}
	}
	if have {
		args = append(args, cur.String())
	}
	return args
}

func TestUCRTArgs(t *testing.T) {
	for line, want := range map[string][]string{
		`prog a b`:                               {"prog", "a", "b"},
		`"C:\Program Files\x.exe" "a b" c`:       {`C:\Program Files\x.exe`, "a b", "c"},
		`p "x""y" ""`:                            {"p", `x"y`, ""},
		`p "a&echo.PWNED" "%PATH%"`:              {"p", "a&echo.PWNED", "%PATH%"},
		`p "C:\dir\\" next`:                      {"p", `C:\dir\`, "next"},
		`p a\\\"b "c\\"`:                         {"p", `a\"b`, `c\`},
		`p "x""&echo.PWNED&"""`:                  {"p", `x"&echo.PWNED&"`},
		`"C:\t\helper.exe" -test.run=X -- "a b"`: {`C:\t\helper.exe`, "-test.run=X", "--", "a b"},
	} {
		if got := ucrtArgs(line); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("ucrtArgs(%s) = %q, want %q", line, got, want)
		}
	}
}
