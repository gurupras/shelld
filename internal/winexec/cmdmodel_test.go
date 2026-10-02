package winexec

// A model of how cmd.exe parses `cmd /c <line>`, how a batch file splits %n,
// and how a UCRT program (node, through npm's `%*` shim) splits the
// forwarded text. It was written for a pre-review of the Windows proposal, from
// recalled parser rules and not observed on Windows -- which is the part that
// matters about where it came from: the model is a statement of what cmd.exe is
// believed to do, so the tests here check the escaping against a belief, and only
// the Windows tests in pty check it against Windows.

import "strings"

var modelEnv = map[string]string{"path": `C:\Windows`, "cd": `C:\w`, "x": "EXPANDED"}

// percentExpand expands %name% on a command line. An undefined %name% keeps
// "%name" and rescans from the closing %.
func percentExpand(s string) (string, int) {
	var out strings.Builder
	expansions := 0
	for i := 0; i < len(s); {
		if s[i] != '%' {
			out.WriteByte(s[i])
			i++
			continue
		}
		j := strings.IndexByte(s[i+1:], '%')
		if j < 0 {
			out.WriteString(s[i:])
			break
		}
		name := s[i+1 : i+1+j]
		base, sub, hasSub := strings.Cut(name, ":~")
		if v, ok := modelEnv[strings.ToLower(base)]; ok && name != "" {
			if hasSub {
				if sub == "," {
					v = ""
				} else {
					v = "<SUBSTR>"
				}
			} else {
				expansions++
			}
			out.WriteString(v)
			i = i + 1 + j + 1
			continue
		}
		out.WriteString(s[i : i+1+j])
		i = i + 1 + j
	}
	return out.String(), expansions
}

// unquotedOps returns the operators cmd sees outside quotes.
func unquotedOps(s string) string {
	inQ := false
	var ops []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQ = !inQ
		case inQ:
		case c == '^':
			i++
		case strings.IndexByte("&|<>()", c) >= 0:
			ops = append(ops, c)
		}
	}
	return string(ops)
}

// batchSplit splits a batch file's arguments (%0, %1, ...).
func batchSplit(s string) []string {
	var toks []string
	var cur strings.Builder
	inQ, have := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			inQ = !inQ
		}
		if !inQ && strings.IndexByte(" \t,;=", c) >= 0 {
			if have {
				toks = append(toks, cur.String())
				cur.Reset()
				have = false
			}
			continue
		}
		cur.WriteByte(c)
		have = true
	}
	if have {
		toks = append(toks, cur.String())
	}
	return toks
}

// ucrtArgs splits a command line tail as the UCRT's argv parser does.
func ucrtArgs(s string) []string {
	var args []string
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
		if c == '\\' {
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
			continue
		}
		if c == '"' {
			if inQ && i+1 < len(s) && s[i+1] == '"' {
				cur.WriteByte('"')
				i += 2
				continue
			}
			inQ = !inQ
			i++
			continue
		}
		cur.WriteByte(c)
		i++
	}
	if have {
		args = append(args, cur.String())
	}
	return args
}

type modelResult struct {
	ops   string
	exps  int
	batch []string // %1, %2, ...
	node  []string // argv a UCRT program gets from `prog %*`
}

// runCmd models `cmd /c <afterC>` running a batch file that forwards %* to a
// UCRT program.
func runCmd(afterC string) modelResult {
	s := afterC
	if strings.HasPrefix(s, `"`) && strings.Count(s, `"`) > 2 {
		k := strings.LastIndexByte(s, '"')
		s = s[1:k] + s[k+1:]
	}
	exp, n := percentExpand(s)
	r := modelResult{exps: n, ops: unquotedOps(exp)}
	if toks := batchSplit(exp); len(toks) > 0 {
		r.batch = toks[1:]
		rest := strings.TrimLeft(exp[strings.Index(exp, toks[0])+len(toks[0]):], " \t,;=")
		r.node = ucrtArgs(rest)
	}
	return r
}

// escapeArg is x/sys/windows v0.46.0 EscapeArg, which ComposeCommandLine
// applies to each argument: the quoting a batch file got before the fix.
func escapeArg(s string) string {
	if len(s) == 0 {
		return `""`
	}
	n := len(s)
	hasSpace := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			n++
		case ' ', '\t':
			hasSpace = true
		}
	}
	if hasSpace {
		n += 2
	}
	if n == len(s) {
		return s
	}
	qs := make([]byte, n)
	j := 0
	if hasSpace {
		qs[j] = '"'
		j++
	}
	slashes := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		default:
			slashes = 0
			qs[j] = s[i]
		case '\\':
			slashes++
			qs[j] = s[i]
		case '"':
			for ; slashes > 0; slashes-- {
				qs[j] = '\\'
				j++
			}
			qs[j] = '\\'
			j++
			qs[j] = s[i]
		}
		j++
	}
	if hasSpace {
		for ; slashes > 0; slashes-- {
			qs[j] = '\\'
			j++
		}
		qs[j] = '"'
		j++
	}
	return string(qs[:j])
}
