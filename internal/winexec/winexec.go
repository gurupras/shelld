// Package winexec resolves program names and builds command lines the way
// Windows needs them for CreateProcess. It is string logic with the file
// system behind a function, so it builds and is tested on every platform;
// only the Windows PTY uses it.
package winexec

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"
)

// runnable are the extensions CreateProcess can start: PE images directly,
// batch files through cmd.exe.
var runnable = []string{".com", ".exe", ".bat", ".cmd"}

func isRunnable(ext string) bool {
	for _, r := range runnable {
		if strings.EqualFold(ext, r) {
			return true
		}
	}
	return false
}

// LookPath resolves prog for CreateProcess:
//
//   - A name with a drive, a slash or a backslash is not searched for. An
//     absolute name is used as is; a relative one is taken relative to dir
//     (the child's working directory) when dir is set.
//   - A bare name is searched for in the absolute entries of env's PATH, in
//     order, never in dir: a program in the window's working directory must
//     not shadow one on PATH. Relative PATH entries would resolve against
//     the daemon's working directory, so they are skipped, as Go's
//     exec.ErrDot refuses them. Without a PATH variable, sysDirs (System32
//     and the Windows directory) are searched.
//
// In each place the name is tried as given only if its extension is one
// CreateProcess can start (.com, .exe, .bat, .cmd), then with each PATHEXT
// extension that is. So "claude" finds claude.cmd, never npm's extensionless
// sh shim beside it, and "python3.12" finds python3.12.exe. isFile reports
// whether a path exists and CandidateMode accepts it.
func LookPath(prog, dir string, env []string, sysDirs []string, isFile func(string) bool) (string, error) {
	if prog == "" {
		return "", errors.New("empty program name")
	}
	exts := pathExts(env)
	unstartable := "" // a file that exists under the exact name but cannot be started
	find := func(file string) string {
		if e := ext(file); isRunnable(e) && isFile(file) {
			return file
		} else if unstartable == "" && e != "" && isFile(file) {
			unstartable = file
		}
		for _, e := range exts {
			if isFile(file + e) {
				return file + e
			}
		}
		return ""
	}
	skipped := false
	if strings.ContainsAny(prog, `:\/`) {
		file := prog
		if dir != "" && !strings.ContainsRune(prog, ':') && !isSep(prog[0]) {
			rel := prog
			for strings.HasPrefix(rel, `.\`) || strings.HasPrefix(rel, "./") {
				rel = rel[2:]
			}
			file = join(dir, rel)
		}
		if f := find(file); f != "" {
			return f, nil
		}
	} else {
		dirs := sysDirs
		if path, ok := lookupEnv(env, "PATH"); ok {
			dirs = nil
			for _, d := range splitList(path) {
				if isAbs(d) {
					dirs = append(dirs, d)
				} else if d != "" {
					skipped = true
				}
			}
		}
		for _, d := range dirs {
			if f := find(join(d, prog)); f != "" {
				return f, nil
			}
		}
	}
	switch {
	case unstartable != "":
		return "", fmt.Errorf("unable to spawn %s: found %s, but CreateProcess starts only .exe, .com, .bat and .cmd files", prog, unstartable)
	case skipped:
		return "", fmt.Errorf("unable to spawn %s because it was not found in PATH's absolute entries (relative entries are not searched)", prog)
	}
	return "", fmt.Errorf("unable to spawn %s because it doesn't exist on the filesystem and was not found in PATH", prog)
}

// CandidateMode reports whether a file with this mode can be a program
// LookPath returns: anything that exists and is not a directory, as os/exec
// decides. It must not require a regular file: Go stats App Execution
// Aliases (the Microsoft Store's pwsh.exe and python.exe in WindowsApps) as
// irregular, and they start like any executable.
func CandidateMode(m fs.FileMode) bool { return !m.IsDir() }

// IsBatch reports whether path names a batch file, which CreateProcess runs
// through cmd.exe. Trailing dots and spaces are ignored, as Win32 ignores
// them in file names.
func IsBatch(path string) bool {
	e := ext(strings.TrimRight(path, ". "))
	return strings.EqualFold(e, ".bat") || strings.EqualFold(e, ".cmd")
}

// MaxBatchCommandLine is cmd.exe's limit on a command line, in UTF-16 code
// units.
const MaxBatchCommandLine = 8191

// BatchCommandLine returns the command line that runs script with args
// through cmd.exe without cmd reinterpreting any argument:
//
//	cmd.exe /e:ON /v:OFF /d /c ""<script>" <args>"
//
// /d skips AutoRun commands, /v:OFF disables !var! expansion, and /e:ON
// enables the %var:~,% syntax the % escape below needs. Each argument that
// contains anything but letters, digits and #$*+-./:?@\_ is quoted; inside
// it " is doubled and % becomes %%cd:~,% (a zero-length substring of %cd%),
// so no operator is seen outside quotes and no variable expands. This is
// Rust std's make_bat_command_line (library/std/src/sys/args/windows.rs,
// 1.89), which also escapes % in the script path.
//
// Arguments containing CR, LF or NUL are refused, since cmd ends the command
// there, as is a script path containing " or ending in \ and a result longer
// than MaxBatchCommandLine. A batch file that re-expands its arguments with
// delayed expansion enabled can still expand ! itself, and a program the
// batch file hands them to through an ANSI argv may map lookalike characters
// such as a fullwidth quote to ASCII ("WorstFit"); neither is cmd.exe's
// parsing, which is all this escapes. The arguments are escaped for programs
// that split their command line with the UCRT's rule (node, behind npm's
// shims); a Go program behind a batch file receives "" inside quotes
// differently, because Go's os.Args uses the pre-2008 rule. cmd.exe cannot
// use a UNC working directory: it warns and runs in the Windows directory.
func BatchCommandLine(script string, args []string) (string, error) {
	script = userPath(script)
	if strings.ContainsRune(script, '"') || strings.HasSuffix(script, `\`) || strings.ContainsRune(script, 0) {
		return "", fmt.Errorf("batch file path %q may not contain \" or NUL or end with \\", script)
	}
	var b strings.Builder
	b.WriteString(`cmd.exe /e:ON /v:OFF /d /c "`)
	b.WriteByte('"')
	appendPercentEscaped(&b, script)
	b.WriteByte('"')
	for i, a := range args {
		if strings.ContainsAny(a, "\r\n\x00") {
			return "", fmt.Errorf("argument %d for batch file %s contains CR, LF or NUL, which cmd.exe cannot pass", i+1, script)
		}
		b.WriteByte(' ')
		appendBatchArg(&b, a)
	}
	b.WriteByte('"')
	line := b.String()
	if n := len(utf16.Encode([]rune(line))); n > MaxBatchCommandLine {
		return "", fmt.Errorf("command line for batch file %s is %d characters; cmd.exe accepts at most %d", script, n, MaxBatchCommandLine)
	}
	return line, nil
}

func appendBatchArg(b *strings.Builder, arg string) {
	quote := arg == "" || strings.HasSuffix(arg, `\`)
	for _, r := range arg {
		if r < 0x80 && !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(`#$*+-./:?@\_`, r)) {
			quote = true
		}
		if unicode.IsControl(r) {
			quote = true
		}
	}
	if quote {
		b.WriteByte('"')
	}
	backslashes := 0
	for _, r := range arg {
		if r == '\\' {
			backslashes++
			b.WriteRune(r)
			continue
		}
		switch r {
		case '"':
			// n backslashes become 2n before the quote, and the quote is
			// doubled.
			b.WriteString(strings.Repeat(`\`, backslashes))
			b.WriteByte('"')
		case '%':
			b.WriteString("%%cd:~,")
		}
		backslashes = 0
		b.WriteRune(r)
	}
	if quote {
		b.WriteString(strings.Repeat(`\`, backslashes))
		b.WriteByte('"')
	}
}

func appendPercentEscaped(b *strings.Builder, s string) {
	for _, r := range s {
		if r == '%' {
			b.WriteString("%%cd:~,")
		}
		b.WriteRune(r)
	}
}

// userPath turns a path into the form cmd.exe accepts: backslashes, and no
// \\?\ prefix.
func userPath(p string) string {
	p = strings.ReplaceAll(p, "/", `\`)
	switch {
	case strings.HasPrefix(p, `\\?\UNC\`):
		return `\\` + p[len(`\\?\UNC\`):]
	case strings.HasPrefix(p, `\\?\`):
		return p[len(`\\?\`):]
	}
	return p
}

// pathExts is PATHEXT's runnable extensions in order, spelled as PATHEXT
// spells them, or .com, .exe, .bat, .cmd when PATHEXT names none.
func pathExts(env []string) []string {
	var exts []string
	if v, ok := lookupEnv(env, "PATHEXT"); ok {
		for e := range strings.SplitSeq(v, ";") {
			if e == "" {
				continue
			}
			if e[0] != '.' {
				e = "." + e
			}
			if isRunnable(e) {
				exts = append(exts, e)
			}
		}
	}
	if len(exts) == 0 {
		exts = runnable
	}
	return exts
}

// ext is the extension of the last path element, including the dot.
func ext(p string) string {
	i := strings.LastIndexByte(p, '.')
	if i < 0 || strings.LastIndexAny(p, `:\/`) > i {
		return ""
	}
	return p[i:]
}

func isSep(c byte) bool { return c == '\\' || c == '/' }

// isAbs reports a drive-absolute (C:\x) or UNC (\\server\share) path.
func isAbs(p string) bool {
	if len(p) >= 3 && p[1] == ':' && isSep(p[2]) && isLetter(p[0]) {
		return true
	}
	return len(p) >= 2 && isSep(p[0]) && isSep(p[1])
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// join joins a directory and a name with a backslash.
func join(dir, name string) string {
	switch {
	case dir == "":
		return name
	case isSep(dir[len(dir)-1]), len(dir) == 2 && dir[1] == ':':
		return dir + name
	}
	return dir + `\` + name
}

// splitList splits a PATH value on ';', treating double-quoted parts as
// literal, as Go's filepath.SplitList does on Windows.
func splitList(path string) []string {
	if path == "" {
		return nil
	}
	var list []string
	start, quoted := 0, false
	for i := 0; i < len(path); i++ {
		switch c := path[i]; {
		case c == '"':
			quoted = !quoted
		case c == ';' && !quoted:
			list = append(list, path[start:i])
			start = i + 1
		}
	}
	list = append(list, path[start:])
	for i, s := range list {
		list[i] = strings.ReplaceAll(s, `"`, "")
	}
	return list
}

// lookupEnv finds key case-insensitively, the last entry winning.
func lookupEnv(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

// EnvironmentBlock builds CreateProcess's Unicode environment block from
// KEY=value entries: sorted by name as CreateProcess documents and Go's
// syscall.createEnvBlock does (ASCII letters upper-cased, stable for equal
// names), each entry NUL-terminated, then a final NUL. An empty environment
// is two NULs. Entries containing NUL are dropped.
func EnvironmentBlock(env []string) []uint16 {
	upperKey := func(kv string) string {
		k, _, _ := strings.Cut(kv, "=")
		b := []byte(k)
		for i, c := range b {
			if 'a' <= c && c <= 'z' {
				b[i] -= 'a' - 'A'
			}
		}
		return string(b)
	}
	env = slices.Clone(env)
	slices.SortStableFunc(env, func(a, b string) int { return strings.Compare(upperKey(a), upperKey(b)) })
	var block []uint16
	for _, kv := range env {
		if strings.ContainsRune(kv, 0) {
			continue
		}
		block = append(block, utf16.Encode([]rune(kv))...)
		block = append(block, 0)
	}
	if len(block) == 0 {
		block = append(block, 0)
	}
	return append(block, 0)
}
