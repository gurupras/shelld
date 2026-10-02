//go:build linux || darwin

package daemon

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	shelld "github.com/gurupras/shelld"
)

// The listing publishes two facts about two different things — alive is the
// terminal, exitStatus is the window's own process — and the combination that
// looks like a bug is not one: a background process can hold the terminal open
// after the shell has exited and been reaped, so alive=true beside a recorded
// exitStatus is a state the daemon must report as it is.
//
// This test exists because that combination was once suppressed here, on the
// theory that a recorded status means the window is dead. A unit test over
// (bool, map) pairs cannot catch that — it has no way to tell a scheduling skew
// of 109µs from a holder that lives for seconds — so the assertion has to run
// against a real window through the real route, which is what this does.
//
// The shape that produces it: the shell ignores SIGHUP, then forks a holder
// that keeps the PTY slave open, then exits.
//
// The ignore goes in the PARENT, before the fork, and that order is the whole
// point. Written the other way, with the trap inside the subshell:
//
//	(trap '' HUP; sleep 3) &        # RACES. Keep indented; see the root
//	                                # package's copy for why gofmt requires it.
//
// this test raced: exit makes the session leader leave, the kernel
// sends SIGHUP to the foreground process group, and a subshell that has not yet
// reached its `trap` builtin takes the default disposition and dies with the
// shell. With a 50ms delay before that trap, 5 in 6. SIG_IGN is inherited across
// fork and preserved across exec, so a holder forked after this line is immune
// before it runs its first instruction — checked with a variant whose child never
// traps at all and survived 6 for 6.
//
// The 3-in-6 first measured here was taken pinned to two CPUs, which is this
// repo's correctness-run configuration and happens to be the configuration that
// SUPPRESSES this race: unpinned the same shape fails 8 of 8. See the affinity
// numbers on TestHolderKeepsTerminalOpenAfterExit in the root package, which is
// where the sweep ran; do not read 3-in-6 as the rate.
//
// What made the race worth fixing rather than tolerating is not the red: a
// holder that dies at once produces alive=false, which is the TRUTH, with the
// same message and the same 0.02s as suppressing alive deliberately. A real
// regression in the pair this test guards would have read as a known flake.
func TestListingReportsAHeldTerminalAsAlive(t *testing.T) {
	// The state this test is about does not exist on darwin, so it is skipped rather
	// than weakened into something that passes there. BSD revokes a controlling
	// terminal when its controlling process exits: every descriptor for the tty is
	// invalidated, the holder's included, so the master sees EOF at once and
	// alive=false is the truth rather than a suppression. Measured with the same
	// script this test uses -- trap '' HUP, fork a 3s holder, exit -- reading the
	// master until it ends: linux 3.001s, macOS 26.4 arm64 14ms. That is a platform
	// difference in what a window can outlive, not a difference in the daemon, and
	// it is recorded on livenessJSON's matrix in routes.go as well, because it is a
	// caller-visible guarantee that does not hold everywhere.
	if runtime.GOOS != "linux" {
		t.Skipf("a process cannot hold a window's terminal open past the shell's exit on %s; see the comment here", runtime.GOOS)
	}
	const holderLife = 3 * time.Second
	sock := boot(t)
	if st, _, raw := request(t, sock, "POST", "/sessions", `{"name":"s"}`); st != 200 {
		t.Fatalf("create session: %d %s", st, raw)
	}
	// %g on Seconds() rather than an integer conversion: int(holderLife/time.Second)
	// truncates, so holderLife = 900ms would write `sleep 0`, and the stability loop
	// below would then report "the held state did not last" — the fixture's premise
	// failing while the message blames the daemon, which is the class this commit
	// exists to close. coreutils sleep takes fractional seconds.
	scriptWindow(t, sock, "s", "held",
		fmt.Sprintf("trap '' HUP\n(sleep %g) &\nexit 0\n", holderLife.Seconds()))

	// Wait for the status to be published, then assert on the same body that
	// carried it: reading the pair from two separate listings would let the
	// holder expire between them.
	var heldAt time.Time
	deadline := time.Now().Add(10 * time.Second)
	for {
		wins, raw := windowsByName(t, sock, "s")
		win := wins["held"]
		if win == nil {
			t.Fatalf("window went missing from the listing: %s", raw)
		}
		if win["exitStatus"] != nil {
			heldAt = time.Now()
			if win["alive"] != true {
				t.Fatalf("alive=%v beside exitStatus=%v: the shell exited but a holder keeps the "+
					"terminal open, and the listing must not suppress either fact: %s",
					win["alive"], win["exitStatus"], raw)
			}
			if !equalJSON(win["exitStatus"], map[string]any{"code": 0.0}) {
				t.Errorf("exitStatus = %v, want code 0", win["exitStatus"])
			}
			// Both keys present with real values, on one line of JSON.
			if !strings.Contains(raw, `"alive":true`) {
				t.Errorf("alive is not true in the raw body: %s", raw)
			}
			break
		}
		if !time.Now().After(deadline) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.Fatalf("the shell's exit status never appeared while the holder ran; either the holder "+
			"died with the shell or the status is not being recorded: %s", raw)
	}

	// The state has to LAST, and one listing cannot show that. Under the trap race
	// described above, one run passed in 9ms: the holder was alive for the single
	// poll that observed it and dead before the next, so the loop below returned
	// immediately and the test reported success without ever seeing the state it is
	// named for. A pass has to mean the pair was stable, not that it was caught in
	// flight, so sample a third of the holder's life. Anything less than the full
	// holderLife leaves the closing assertion below something to observe.
	//
	// A third, not more, because the window is measured from heldAt while the
	// holder's clock started at the create: this tolerates ~2s of latency between
	// them before it fails for load rather than for cause. That is not the tight
	// constraint in the suite — TestHolderKeepsTerminalOpenAfterExit runs the same
	// shape with holderLife/2 and additionally requires output emitted at
	// holderStart+1s to be captured inside it. Whoever retunes holderLife should
	// check that one first; it fails earlier.
	for stable := heldAt.Add(holderLife / 3); time.Now().Before(stable); {
		wins, raw := windowsByName(t, sock, "s")
		win := wins["held"]
		if win == nil || win["alive"] != true || win["exitStatus"] == nil {
			t.Fatalf("the held state did not last: at +%v of the holder's %v the listing says "+
				"alive=%v exitStatus=%v, so the holder is gone and this test's premise never "+
				"held: %s", time.Since(heldAt).Round(time.Millisecond), holderLife,
				win["alive"], win["exitStatus"], raw)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The holder's own exit closes the terminal, and does not disturb the status.
	deadline = time.Now().Add(15 * time.Second)
	for {
		wins, raw := windowsByName(t, sock, "s")
		win := wins["held"]
		if win != nil && win["alive"] == false {
			if win["exitStatus"] == nil {
				t.Errorf("the terminal closed and the exit status was lost: %s", raw)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the terminal never closed after the holder exited: %s", raw)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// exitStatusJSON's mapping, which is the part that is genuinely a pure
// function: a signal exit carries the name beside code -1, a plain exit carries
// only the code, and an unreaped window is a JSON null rather than an absent
// key. The alive half is deliberately not modelled here — see the test above for
// why a (bool, map) table is the wrong instrument for it.
func TestExitStatusJSONMapping(t *testing.T) {
	for _, c := range []struct {
		name string
		code int
		sig  string
		ok   bool
		want map[string]any
	}{
		{"not reaped", 0, "", false, nil},
		{"clean exit", 0, "", true, map[string]any{"code": 0}},
		{"non-zero exit", 3, "", true, map[string]any{"code": 3}},
		{"signal", -1, "SIGTERM", true, map[string]any{"code": -1, "signal": "SIGTERM"}},
	} {
		got := exitStatusJSON(shelld.ExitStatus{Code: c.code, Signal: c.sig}, c.ok)
		if !equalJSON(got, c.want) {
			t.Errorf("%s: exitStatusJSON = %v, want %v", c.name, got, c.want)
		}
	}
}
