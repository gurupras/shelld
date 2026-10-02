package winexec

import (
	"io/fs"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

// fakeFS is a case-insensitive set of regular files and directories.
type fakeFS struct{ files, dirs map[string]bool }

func newFS(files []string, dirs ...string) fakeFS {
	fs := fakeFS{map[string]bool{}, map[string]bool{}}
	for _, f := range files {
		fs.files[strings.ToLower(f)] = true
	}
	for _, d := range dirs {
		fs.dirs[strings.ToLower(d)] = true
	}
	return fs
}

func (fs fakeFS) isFile(p string) bool { return fs.files[strings.ToLower(p)] }

// legacySearchPath is pty_windows.go's searchPath before this package
// (a port of portable-pty 0.8.1's Windows search_path), with os.Stat replaced
// by the fake file system, where a directory also exists, and filepath's
// Windows Join and Ext written out so it runs here.
func legacySearchPath(exe string, env []string, fs fakeFS) string {
	exists := func(p string) bool { return fs.isFile(p) || fs.dirs[strings.ToLower(p)] }
	path, ok := lookupEnv(env, "PATH")
	if !ok {
		return exe
	}
	exts, ok := lookupEnv(env, "PATHEXT")
	if !ok {
		exts = ".EXE"
	}
	for _, dir := range splitList(path) {
		c := join(dir, exe)
		if exists(c) {
			return c
		}
		for e := range strings.SplitSeq(exts, ";") {
			if e == "" {
				continue
			}
			ce := strings.TrimSuffix(c, ext(c)) + e
			if exists(ce) {
				return ce
			}
		}
	}
	return exe
}

const stdPathext = "PATHEXT=.COM;.EXE;.BAT;.CMD;.VBS;.VBE;.JS;.JSE;.WSF;.WSH;.MSC"

func TestLookPath(t *testing.T) {
	npm := []string{`C:\npm\claude`, `C:\npm\claude.cmd`, `C:\npm\claude.ps1`}
	cases := []struct {
		name    string
		prog    string
		dir     string
		env     []string
		sysDirs []string
		fs      fakeFS
		legacy  string // what searchPath returned
		want    string // "" means an error
	}{
		{name: "npm shim", prog: "claude", env: []string{`PATH=C:\npm`, stdPathext}, fs: newFS(npm),
			legacy: `C:\npm\claude`, want: `C:\npm\claude.CMD`},
		{name: "dotted name", prog: "python3.12", env: []string{`PATH=C:\py`, stdPathext}, fs: newFS([]string{`C:\py\python3.12.exe`}),
			legacy: "python3.12", want: `C:\py\python3.12.EXE`},
		{name: "directory before exe", prog: "node", env: []string{`PATH=C:\a;C:\b`, stdPathext}, fs: newFS([]string{`C:\b\node.exe`}, `C:\a\node`),
			legacy: `C:\a\node`, want: `C:\b\node.EXE`},
		{name: "relative to dir", prog: `.\tool`, dir: `C:\w`, env: []string{`PATH=C:\bin`, stdPathext}, fs: newFS([]string{`C:\w\tool.exe`}),
			legacy: `.\tool`, want: `C:\w\tool.EXE`},
		{name: "subdir relative to dir", prog: `bin/tool.cmd`, dir: `C:\w\`, env: []string{`PATH=C:\bin`}, fs: newFS([]string{`C:\w\bin/tool.cmd`}),
			legacy: `bin/tool.cmd`, want: `C:\w\bin/tool.cmd`},
		{name: "bare name never from dir", prog: "tool", dir: `C:\w`, env: []string{`PATH=C:\bin`, stdPathext}, fs: newFS([]string{`C:\w\tool.exe`}),
			legacy: "tool", want: ""},
		{name: "bare name from PATH, not dir", prog: "git", dir: `C:\repo`, env: []string{`PATH=C:\git\cmd`, stdPathext}, fs: newFS([]string{`C:\repo\git.exe`, `C:\git\cmd\git.exe`}),
			legacy: `C:\git\cmd\git.EXE`, want: `C:\git\cmd\git.EXE`},
		{name: "absolute without extension", prog: `C:\x\app`, env: []string{`PATH=C:\bin`, stdPathext}, fs: newFS([]string{`C:\x\app.exe`}),
			legacy: `C:\x\app`, want: `C:\x\app.EXE`},
		{name: "absolute with extension", prog: `C:\x\app.exe`, env: []string{`PATH=C:\bin`}, fs: newFS([]string{`C:\x\app.exe`}),
			legacy: `C:\x\app.exe`, want: `C:\x\app.exe`},
		{name: "UNC", prog: `\\srv\share\t.cmd`, env: nil, fs: newFS([]string{`\\srv\share\t.cmd`}),
			legacy: `\\srv\share\t.cmd`, want: `\\srv\share\t.cmd`},
		{name: "no PATH: system dirs", prog: "where", env: []string{stdPathext}, sysDirs: []string{`C:\Windows\System32`, `C:\Windows`}, fs: newFS([]string{`C:\Windows\System32\where.exe`}),
			legacy: "where", want: `C:\Windows\System32\where.EXE`},
		{name: "PATHEXT order", prog: "x", env: []string{`PATH=C:\d`, "PATHEXT=.CMD;.EXE"}, fs: newFS([]string{`C:\d\x.exe`, `C:\d\x.cmd`}),
			legacy: `C:\d\x.CMD`, want: `C:\d\x.CMD`},
		{name: "no PATHEXT", prog: "x", env: []string{`PATH=C:\d`}, fs: newFS([]string{`C:\d\x.bat`}),
			legacy: "x", want: `C:\d\x.bat`},
		{name: "script extensions skipped", prog: "run", env: []string{`PATH=C:\d`, stdPathext}, fs: newFS([]string{`C:\d\run.vbs`, `C:\d\run.bat`}),
			legacy: `C:\d\run.BAT`, want: `C:\d\run.BAT`},
		{name: "ps1 only", prog: "claude", env: []string{`PATH=C:\npm`, "PATHEXT=.PS1;.EXE"}, fs: newFS([]string{`C:\npm\claude.ps1`}),
			legacy: `C:\npm\claude.PS1`, want: ""},
		{name: "relative PATH entries skipped", prog: "tool", env: []string{`PATH=.;;bin;C:\bin`, stdPathext}, fs: newFS([]string{`.\tool.exe`, `bin\tool.exe`, `C:\bin\tool.exe`}),
			legacy: `.\tool.EXE`, want: `C:\bin\tool.EXE`},
		{name: "quoted PATH entry", prog: "tool", env: []string{`PATH="C:\a;b";C:\c`, stdPathext}, fs: newFS([]string{`C:\a;b\tool.exe`}),
			legacy: `C:\a;b\tool.EXE`, want: `C:\a;b\tool.EXE`},
		{name: "ADS or trailing dot never match", prog: `C:\x\a.cmd.`, env: nil, fs: newFS([]string{`C:\x\a.cmd.`}),
			legacy: `C:\x\a.cmd.`, want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := legacySearchPath(c.prog, c.env, c.fs); got != c.legacy {
				t.Errorf("legacy searchPath = %q, want %q", got, c.legacy)
			}
			got, err := LookPath(c.prog, c.dir, c.env, c.sysDirs, c.fs.isFile)
			if c.want == "" {
				if err == nil {
					t.Fatalf("LookPath = %q, want an error", got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("LookPath = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestLookPathErrors(t *testing.T) {
	fs := newFS([]string{`C:\s\deploy.ps1`, `C:\bin\x.exe`})
	_, err := LookPath(`C:\s\deploy.ps1`, "", []string{stdPathext}, nil, fs.isFile)
	if err == nil || !strings.Contains(err.Error(), "starts only .exe, .com, .bat and .cmd") {
		t.Errorf(".ps1 path: %v", err)
	}
	_, err = LookPath("nope", "", []string{`PATH=.;C:\bin`}, nil, fs.isFile)
	if err == nil || !strings.Contains(err.Error(), "relative entries are not searched") {
		t.Errorf("relative entry: %v", err)
	}
	_, err = LookPath("nope", "", []string{`PATH=C:\bin`}, nil, fs.isFile)
	if err == nil || !strings.Contains(err.Error(), "not found in PATH") {
		t.Errorf("not found: %v", err)
	}
}

func TestIsBatch(t *testing.T) {
	for p, want := range map[string]bool{
		`C:\x\a.bat`: true, `C:\x\a.CMD`: true, `a.Cmd`: true, `a.cmd.`: true, `a.cmd  `: true,
		`a.cmdx`: false, `a.exe`: false, `C:\x.cmd\a`: false, `a`: false,
	} {
		if got := IsBatch(p); got != want {
			t.Errorf("IsBatch(%q) = %v", p, got)
		}
	}
}

const prefix = `cmd.exe /e:ON /v:OFF /d /c "`

// batchArgs returns the escaped arguments part of BatchCommandLine's result.
func batchArgs(t *testing.T, args ...string) string {
	t.Helper()
	line, err := BatchCommandLine(`C:\t\args.cmd`, args)
	if err != nil {
		t.Fatal(err)
	}
	head := prefix + `"C:\t\args.cmd" `
	if !strings.HasPrefix(line, head) || !strings.HasSuffix(line, `"`) {
		t.Fatalf("line %q", line)
	}
	return line[len(head) : len(line)-1]
}

// Outputs of Rust std's make_bat_command_line (1.89) for each argument. The port
// that produced them was written independently of the implementation under test,
// which is what makes these golden values an oracle rather than a second copy of
// the same reasoning; the run itself was not kept, so the values below are the
// record of it.
func TestBatchCommandLineGolden(t *testing.T) {
	for in, want := range map[string]string{
		"a&b":                   `"a&b"`,
		"a&echo.PWNED":          `"a&echo.PWNED"`,
		`x" & calc & "`:         `"x"" & calc & """`,
		"%PATH%":                `"%%cd:~,%PATH%%cd:~,%"`,
		"%%x%%":                 `"%%cd:~,%%%cd:~,%x%%cd:~,%%%cd:~,%"`,
		`x"y`:                   `"x""y"`,
		`C:\dir\`:               `"C:\dir\\"`,
		`C:\dir\\`:              `"C:\dir\\\\"`,
		"":                      `""`,
		" ":                     `" "`,
		"^&calc":                `"^&calc"`,
		`\"`:                    `"\\"""`,
		`a\"&calc`:              `"a\\""&calc"`,
		"!x!":                   `"!x!"`,
		"a=b":                   `"a=b"`,
		"plain-arg_1.txt":       `plain-arg_1.txt`,
		`C:\no\trailing`:        `C:\no\trailing`,
		"\u00a0":                "\u00a0",
		"日本語 prompt":            `"日本語 prompt"`,
		"--flag=you are %USER%": `"--flag=you are %%cd:~,%USER%%cd:~,%"`,
	} {
		if got := batchArgs(t, in); got != want {
			t.Errorf("%q: got %s, want %s", in, got, want)
		}
	}
}

func TestBatchCommandLineRefuses(t *testing.T) {
	for _, a := range []string{"a\rb", "a\nb", "a\x00b"} {
		if _, err := BatchCommandLine(`C:\t\args.cmd`, []string{"ok", a}); err == nil {
			t.Errorf("%q accepted", a)
		}
	}
	for _, s := range []string{`C:\t\"x".cmd`, `C:\t\`} {
		if _, err := BatchCommandLine(s, nil); err == nil {
			t.Errorf("script %q accepted", s)
		}
	}
	long := strings.Repeat("%", 1100) // each % becomes 8 characters
	if _, err := BatchCommandLine(`C:\t\args.cmd`, []string{long}); err == nil || !strings.Contains(err.Error(), "at most 8191") {
		t.Errorf("long line: %v", err)
	}
	if _, err := BatchCommandLine(`C:\t\args.cmd`, []string{strings.Repeat("a", 8000)}); err != nil {
		t.Errorf("line under the limit: %v", err)
	}
}

func TestBatchScriptPath(t *testing.T) {
	for in, want := range map[string]string{
		`C:/x/app.cmd`:              `"C:\x\app.cmd"`,
		`\\?\C:\x\app.cmd`:          `"C:\x\app.cmd"`,
		`\\?\UNC\srv\share\app.cmd`: `"\\srv\share\app.cmd"`,
		`C:\a%x%b\run.cmd`:          `"C:\a%%cd:~,%x%%cd:~,%b\run.cmd"`,
	} {
		line, err := BatchCommandLine(in, nil)
		if err != nil || line != prefix+want+`"` {
			t.Errorf("%q: %s, %v", in, line, err)
		}
	}
}

// Under the cmd.exe model, every argument reaches the batch file as one
// argument and a UCRT program behind `%*` unchanged, with no operator outside
// quotes and no variable expanded. The quoting used before this package
// (ComposeCommandLine through an implicit cmd /c) fails the same check.
func TestBatchCommandLineAgainstCmdModel(t *testing.T) {
	check := func(args []string) (after, before modelResult, ok bool) {
		line, err := BatchCommandLine(`C:\t\args.cmd`, args)
		if err != nil {
			return modelResult{}, modelResult{}, true
		}
		after = runCmd(line[len(`cmd.exe /e:ON /v:OFF /d /c `):])
		old := `C:\t\args.cmd`
		for _, a := range args {
			old += " " + escapeArg(a)
		}
		before = runCmd(old)
		ok = after.ops == "" && after.exps == 0 && slices.Equal(after.node, args) && len(after.batch) == len(args)
		return after, before, ok
	}
	for _, v := range []string{"a&echo.PWNED", "%PATH%", "a|b", "<x>", "(", `x" & calc & "`} {
		after, before, ok := check([]string{v})
		if !ok {
			t.Errorf("%q: after %+v", v, after)
		}
		if before.ops == "" && before.exps == 0 {
			t.Errorf("%q: the old quoting passes the model too, so this vector proves nothing", v)
		}
	}
	alphabet := []rune("ab \t\"\\%^&|<>()!,;=:~cdPATHx\u00a0\u3000日")
	rng := rand.New(rand.NewSource(1))
	for range 20000 {
		args := make([]string, 1+rng.Intn(3))
		for k := range args {
			rs := make([]rune, rng.Intn(10))
			for q := range rs {
				rs[q] = alphabet[rng.Intn(len(alphabet))]
			}
			args[k] = string(rs)
		}
		if after, _, ok := check(args); !ok {
			t.Fatalf("%q: %+v", args, after)
		}
	}
}

func TestEnvironmentBlock(t *testing.T) {
	decode := func(b []uint16) string { return string(utf16.Decode(b)) }
	if got := decode(EnvironmentBlock(nil)); got != "\x00\x00" {
		t.Errorf("empty: %q", got)
	}
	// Sorted by name with ASCII upper-cased: "_" (0x5F) sorts after letters,
	// and A!=… sorts before AB=… by name, not by the whole entry.
	got := decode(EnvironmentBlock([]string{"path=C:\\x", "_x=1", "AB=2", "A!=3", "Path2=4", "bad=\x00", "é=5"}))
	want := "A!=3\x00AB=2\x00path=C:\\x\x00Path2=4\x00_x=1\x00é=5\x00\x00"
	if got != want {
		t.Errorf("block %q, want %q", got, want)
	}
}

func TestCandidateMode(t *testing.T) {
	for _, c := range []struct {
		mode fs.FileMode
		ok   bool
	}{
		{0o644, true},
		{fs.ModeIrregular, true}, // an App Execution Alias, e.g. WindowsApps\pwsh.exe
		{fs.ModeSymlink, true},
		{fs.ModeDir, false},
		{fs.ModeDir | fs.ModeSymlink, false},
	} {
		if got := CandidateMode(c.mode); got != c.ok {
			t.Errorf("CandidateMode(%v) = %v", c.mode, got)
		}
	}
}
