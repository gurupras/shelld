package shelld

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func lookPath(prog string) bool {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(dir, prog)); err == nil {
			return true
		}
	}
	return false
}

// DefaultShell picks the shell command for a window that names none, as a
// command line for display. A window spawns DefaultShellArgv instead: the
// line is not safe to split, since $SHELL and COMSPEC are paths and a path
// may hold a space.
func DefaultShell() string {
	line, _ := defaultShell()
	return line
}

// DefaultShellArgv is DefaultShell as the argv a window spawns.
func DefaultShellArgv() []string {
	_, argv := defaultShell()
	return argv
}

func defaultShell() (line string, argv []string) {
	if runtime.GOOS == "windows" {
		const noHistory = "Set-PSReadLineOption -HistorySaveStyle SaveNothing"
		for _, ps := range []string{"pwsh.exe", "powershell.exe"} {
			if lookPath(ps) {
				return ps + ` -NoLogo -NoProfile -NoExit -Command "` + noHistory + `"`,
					[]string{ps, "-NoLogo", "-NoProfile", "-NoExit", "-Command", noHistory}
			}
		}
		c := os.Getenv("COMSPEC")
		if c == "" {
			c = "cmd.exe"
		}
		return c + " /K", []string{c, "/K"}
	}
	// loginShell rather than a $PATH probe for bash. The probe asked whether
	// bash was anywhere on $PATH and then returned the unrelated literal
	// "/bin/bash", so on a host whose bash is under /usr/local -- or any host
	// without usrmerge, where /bin and /usr/bin are different directories --
	// it answered with a path that need not exist, and the /bin/sh floor below
	// it was unreachable precisely because the probe had succeeded. loginShell
	// validates what it returns and already backs $SHELL with the passwd entry,
	// which is where a host that has bash somewhere unusual records it, and is
	// the chain the reference implements (tmux getshell/checkshell).
	//
	// On darwin the passwd step is a no-op -- /etc/passwd there holds only the
	// system accounts, and a normal uid (501) has no entry, its shell being in
	// OpenDirectory -- so an unset $SHELL yields /bin/sh where this used to yield
	// /bin/bash. Both exist on macOS, so both are spawnable; what changes is
	// which one, and only in the case where nothing told us. --shell is the way
	// to say.
	sh := loginShell()
	return sh, []string{sh}
}

// IdlePatternForShell returns the prompt regex (Rust syntax) used to decide a
// shell is idle, for a command line: its program is the first word, with
// split as splitCommandLine splits it.
func IdlePatternForShell(shell string) string {
	if parts, _ := splitCommandLine(shell); len(parts) > 0 {
		return IdlePatternForProgram(parts[0])
	}
	return IdlePatternForProgram("")
}

// IdlePatternForProgram is IdlePatternForShell for a program path that is
// already split out, which may contain spaces.
func IdlePatternForProgram(prog string) string {
	s := basenameLower(prog)
	switch {
	case strings.Contains(s, "pwsh") || strings.Contains(s, "powershell"):
		return `(?m)^PS [^>]+>\s*$`
	case s == "cmd.exe" || s == "cmd":
		return `(?m)^[A-Za-z]:\\.*>\s*$`
	case strings.Contains(s, "fish"):
		return `(?m)^\s*[\w@/~\-.]+\s*[$❯>]\s*$`
	}
	return `(?m)^\s*(?:❯|[$%#>])\s*$`
}

// basenameLower is the lowercased final segment of a path, splitting on both
// / and \.
func basenameLower(s string) string {
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	return strings.ToLower(s)
}

// CommandLineError is NewWindow's error for a shell command line that cannot
// be split into words: it ended inside a quoted region, so where its words end
// is not determined by the line. Line is the whole line rather than a first
// word, because which word is first is exactly what the line leaves open.
type CommandLineError struct {
	Line string
	// Quote is the quote left open, so the message names the character the
	// caller has to close.
	Quote rune
}

// Error matches shelld-rs's ShellError::malformed_command_line as its create
// route renders it.
func (e *CommandLineError) Error() string {
	return fmt.Sprintf("spawn shell %s: unterminated %c quote, so no command line was run", e.Line, e.Quote)
}

// splitCommandLine splits a client's command line on spaces and tabs, with
// both ' and " opening a quoted region, as shelld-rs's shlex_split_simple does.
//
// A quoted empty region is a word, so `sh -c ""` keeps its empty body rather
// than handing the interpreter the next word as one. An unterminated quote is a
// *CommandLineError rather than a parse: the line spawned some other argv
// before. Backslash is not an escape, deliberately: these lines reach
// CreateProcess on Windows, where `C:\T\My Dir` is a path, and quoting is the
// only thing that carries its space through. So a quote of one form can be
// written only inside a region of the other.
func splitCommandLine(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	var quote rune
	// A word from a quoted region is a word even when empty, which cur.Len()
	// alone cannot tell from "no word here yet".
	quoted := false
	for _, c := range s {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, quoted = c, true
		case c == ' ' || c == '\t':
			if cur.Len() > 0 || quoted {
				out = append(out, cur.String())
				cur.Reset()
				quoted = false
			}
		default:
			cur.WriteRune(c)
		}
	}
	if quote != 0 {
		return nil, &CommandLineError{Line: s, Quote: quote}
	}
	if cur.Len() > 0 || quoted {
		out = append(out, cur.String())
	}
	return out, nil
}
