package main

// Untagged, like args_test.go and for the same reason: everything here is
// parsing and argv construction that reaches no daemon, and the rest of this
// package's tests are linux-or-darwin only, so a tagged file would leave these
// uncovered on Windows -- where `shelld --shell` is just as parseable.

import (
	"slices"
	"strings"
	"testing"
)

// --shell takes a value in either form, on either side of the subcommand, and
// is absent from the default cli. The value is kept verbatim, spaces included:
// splitting it is shelld.splitCommandLine's job at window-create time, not the
// parser's, and a parser that split it here would lose the quoting rules that
// function honours.
func TestParseArgsReadsTheShellOverride(t *testing.T) {
	clearDefaults(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"absent", []string{"run"}, ""},
		{"separate value", []string{"run", "--shell", "/bin/zsh"}, "/bin/zsh"},
		{"joined value", []string{"run", "--shell=/bin/zsh"}, "/bin/zsh"},
		{"before the subcommand", []string{"--shell", "/bin/zsh", "run"}, "/bin/zsh"},
		{"with arguments", []string{"run", "--shell", "bash -l"}, "bash -l"},
		{"empty value is the same as absent", []string{"run", "--shell="}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseArgs(tc.args).shell; got != tc.want {
				t.Errorf("parseArgs(%q).shell = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// The commands that can act on --shell accept it and the rest reject it, rather
// than ignoring it. `shelld stop --shell X` cannot change any shell -- the
// daemon it talks to is already serving -- so accepting it silently would exit
// 0 for a request that did nothing.
//
// restart is in the accepting set because it runs cmdDaemonize with this same
// cli, so it really does carry the flag through. (--shared-access rejects
// restart even though the same is true of it; that is its own inconsistency,
// not a rule this follows.)
func TestShellOverrideIsRejectedWhereItCannotBeHonoured(t *testing.T) {
	for _, cmd := range []string{"run", "daemonize", "restart"} {
		t.Run(cmd+" accepts it", func(t *testing.T) {
			clearDefaults(t)
			// parseArgs exits the process on a usage error, so reaching the
			// return at all is the assertion; the value confirms which flag was
			// parsed rather than merely tolerated.
			if got := parseArgs([]string{cmd, "--shell", "/bin/zsh"}).shell; got != "/bin/zsh" {
				t.Errorf("shelld %s --shell: shell = %q, want /bin/zsh", cmd, got)
			}
		})
	}
	for _, cmd := range []string{"stop", "status", "kill", "logs", "sessions"} {
		t.Run(cmd+" rejects it", func(t *testing.T) {
			args := []string{cmd, "--shell", "/bin/zsh"}
			if cmd == "kill" {
				args = append(args, "--force")
			}
			_, stderr, code := runShelld(t, args...)
			if code != 2 {
				t.Errorf("shelld %q exited %d, want 2: a flag this command cannot honour must not be accepted\nstderr: %s",
					args, code, stderr)
			}
			// The message has to name the flag, or it sends the reader looking
			// at the subcommand instead.
			if !strings.Contains(stderr, "--shell") {
				t.Errorf("shelld %q stderr does not mention --shell: %s", args, stderr)
			}
		})
	}
}

// cmdDaemonize spawns `shelld run` as a fresh process, so --shell has to be
// repeated in that argv. Left out, `shelld daemonize --shell X` reports a
// successful start and the daemon serves $SHELL -- a silent success, which is
// why this is asserted directly rather than left to the end-to-end tests.
//
// Asserted as two argv entries, because that is what makes a shell carrying
// arguments survive an exec with no shell to re-split it.
func TestDaemonizeForwardsTheShellOverride(t *testing.T) {
	base := cli{socket: "s", pidfile: "p", logfile: "l"}

	got := runArgs(base)
	if i := slices.Index(got, "--shell"); i >= 0 {
		t.Errorf("runArgs with no shell passed --shell: %q", got)
	}

	withShell := base
	withShell.shell = "bash -l"
	got = runArgs(withShell)
	i := slices.Index(got, "--shell")
	if i < 0 {
		t.Fatalf("runArgs dropped --shell: %q", got)
	}
	if i+1 >= len(got) {
		t.Fatalf("runArgs passed --shell with no value: %q", got)
	}
	if got[i+1] != "bash -l" {
		t.Errorf("runArgs passed --shell %q, want the value whole in one argv entry: %q", got[i+1], got)
	}
	// The child must be able to parse back exactly what was forwarded, which is
	// the property that matters and the one a hand-written expectation can get
	// wrong: it closes the loop rather than restating the producer.
	if back := parseArgs(got).shell; back != withShell.shell {
		t.Errorf("the forwarded argv %q parses back to shell %q, want %q", got, back, withShell.shell)
	}
}
