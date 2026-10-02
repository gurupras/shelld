//go:build !linux

package cgroup

// The non-Linux stub had no test on any platform. What is worth pinning about it
// is narrower than its surface, so this file says which part is a contract and
// which part is a shape.
//
// Only Probe is reachable in this build. The module's caller (cgroups.go:66)
// probes once, and on an error it logs "cgroup window containment unavailable;
// window kill will use /proc only" and leaves cgManager nil; every other method
// here is called through that manager, so on a platform where Probe declines
// none of them ever runs. pty/place_darwin.go states the same premise from the
// other side. That makes Probe's two returns the only assertions in this file
// with a consequence today:
//
//   - the error must be ErrUnsupported by errors.Is, because that is what tells
//     the caller this is a mode to run in rather than a fault to report; and
//   - the Manager must be nil, because a non-nil one would be stored and then
//     used as if there were a cgroup arm, and every kill would go looking for
//     membership in a cgroup that does not exist.
//
// The rest is pinned as a shape, for the day the "Windows job object" the package
// comment leaves open makes Probe succeed here. Two of the returns are
// deliberately NOT errors -- GC reports nothing collected and Close reports
// success -- because both run on paths where an error is logged as a warning, and
// a platform without cgroups has not failed to collect or release anything. The
// rest return ErrUnsupported so that a caller which somehow holds a Cgroup gets a
// refusal rather than a silent no-op that looks like a kill.
//
// UNTESTED ON WINDOWS AND DARWIN: this file cannot run on the Linux machine it
// was written on. It is vetted and compiled for windows/amd64, windows/arm64,
// darwin/amd64 and darwin/arm64, and it has never been executed.

import (
	"errors"
	"testing"
	"time"
)

// TestProbeDeclinesWithAUsableError is the one reachable contract.
func TestProbeDeclinesWithAUsableError(t *testing.T) {
	m, err := Probe()
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("Probe error = %v, want it to match ErrUnsupported: the caller keys on that to run with the /proc arm alone", err)
	}
	if m != nil {
		t.Errorf("Probe manager = %v, want nil: a non-nil manager would be stored and used as if this platform had cgroups", m)
	}
}

// TestTheStubRefusesEverythingExceptCollectingAndClosing pins the two deliberate
// asymmetries. A stub tidied into returning ErrUnsupported uniformly would turn
// daemon startup and shutdown into warning-logging paths on every non-Linux
// platform; a stub tidied into returning nil uniformly would make a Kill that
// killed nothing indistinguishable from one that worked.
func TestTheStubRefusesEverythingExceptCollectingAndClosing(t *testing.T) {
	m := &Manager{}
	c := &Cgroup{}

	if got := m.Base(); got != "" {
		t.Errorf("Base() = %q, want empty: there is no base directory here", got)
	}
	if got := c.Dir(); got != "" {
		t.Errorf("Dir() = %q, want empty", got)
	}

	// Reports success: nothing was collected and nothing failed to be collected.
	removed, busy, err := m.GC()
	if removed != 0 || busy != 0 || err != nil {
		t.Errorf("GC() = (%d, %d, %v), want (0, 0, nil): startup logs a warning on an error, and there is nothing here to collect", removed, busy, err)
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close() = %v, want nil: shutdown logs a warning on an error, and there is no base to release", err)
	}

	// Refuses, so that nothing reads a no-op as containment.
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"New", second(m.New(1))},
		{"Open", second(c.Open())},
		{"Kill", c.Kill()},
		{"WaitEmpty", c.WaitEmpty(time.Millisecond)},
		{"Release", second(c.Release())},
		{"Remove", c.Remove()},
	} {
		if !errors.Is(tc.err, ErrUnsupported) {
			t.Errorf("%s() error = %v, want ErrUnsupported", tc.name, tc.err)
		}
	}

	// New must not hand back a Cgroup alongside its refusal: a caller that
	// checked the pointer instead of the error would otherwise carry a handle
	// whose every method refuses.
	if got, _ := m.New(1); got != nil {
		t.Errorf("New() cgroup = %v, want nil", got)
	}
	if f, _ := c.Open(); f != nil {
		t.Errorf("Open() file = %v, want nil: pty.Command.PlaceIn would try to place a process in it", f)
	}
}

// second returns the error from a (value, error) pair, so the table above can
// hold calls with different value types.
func second[T any](_ T, err error) error { return err }
