//go:build linux

package daemon

// What /proc says about a process: the input sameDaemon decides on, and the
// only reason a stale lockfile's pid can be told apart from a live daemon's.
//
// processInfo had no test at all, and the two things it can get wrong are
// silent. A creation time computed with the wrong tick unit, or read from a
// neighbouring field of /proc/<pid>/stat, is still a plausible timestamp: it
// verifies or refutes daemons by accident. And alive has to be false for a
// process that has exited but not been reaped, or a daemon killed by a parent
// that never waits would keep its stale socket and lockfile forever.
//
// The malformed-stat arms (no parentheses, fewer than 20 fields, an
// unparseable starttime) are not reachable from the real /proc and the reader
// takes no injectable path, so they are left uncovered rather than tested
// against a fabricated file the kernel would never write.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// statState is the state character from /proc/<pid>/stat, and whether the
// process has an entry at all.
func statState(t *testing.T, pid int) (string, bool) {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data[bytes.LastIndexByte(data, ')')+1:]))
	if len(fields) == 0 {
		t.Fatalf("/proc/%d/stat has no fields after the comm: %q", pid, data)
	}
	return fields[0], true
}

// assertCreatedAround fails unless created falls in the window the process was
// actually started in.
func assertCreatedAround(t *testing.T, created, before, after time.Time, what string) {
	t.Helper()
	// btime is truncated to a second and starttime is in 10ms ticks, so the
	// window is widened by two seconds. Anything that misses it by more is a
	// wrong unit or a wrong field, not rounding.
	if created.Before(before.Add(-2*time.Second)) || created.After(after.Add(2*time.Second)) {
		t.Errorf("%s: created %s, want between %s and %s — the tick unit or the stat field is wrong",
			what, created.Format(time.RFC3339Nano), before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano))
	}
}

// TestProcessInfoOnThisProcess reads the one process whose identity the test
// already knows. The creation time cannot be known exactly, so what is
// asserted is the pair of bounds that must hold of any process: it started
// after the machine booted and it is not in the future.
func TestProcessInfoOnThisProcess(t *testing.T) {
	info, alive, err := processInfo(os.Getpid())
	if err != nil || !alive {
		t.Fatalf("processInfo(self) = %+v, alive=%v, %v", info, alive, err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if info.image != exe {
		t.Errorf("image %q, want this test binary %q", info.image, exe)
	}
	boot, err := bootTime()
	if err != nil {
		t.Fatal(err)
	}
	if info.created.Before(boot) || info.created.After(time.Now()) {
		t.Errorf("created %s is outside [boot %s, now]", info.created, boot)
	}
}

// TestProcessInfoDatesAFreshlyStartedProcess is the assertion that pins the
// tick unit. A process started a moment ago must be dated a moment ago:
// dividing by the wrong userHZ, or reading starttime from the field next to it,
// moves the answer by hours on a machine that has been up for hours, and
// sameDaemon then verifies or refutes daemons for the wrong reason.
func TestProcessInfoDatesAFreshlyStartedProcess(t *testing.T) {
	dir := shortDir(t)
	before := time.Now()
	p := shelldNamed(t, dir)
	after := time.Now()
	info, alive, err := processInfo(p.pid)
	if err != nil || !alive {
		t.Fatalf("processInfo(%d) = %+v, alive=%v, %v", p.pid, info, alive, err)
	}
	assertCreatedAround(t, info.created, before, after, fmt.Sprintf("pid %d", p.pid))
	if want := filepath.Join(dir, "shelld-fixture"); info.image != want {
		t.Errorf("image %q, want %q from /proc/%d/exe", info.image, want, p.pid)
	}
}

// TestProcessInfoParsesACommWithParentheses covers why the reader looks for the
// last ')' rather than the first. A process's comm is its executable's name and
// can hold anything, brackets and spaces included; a reader that splits on the
// first ')' reads the fields at the wrong offsets and dates the process from
// whatever number lands in field 22. So this is not a hypothetical input: a
// user only has to name a binary with a bracket in it.
func TestProcessInfoParsesACommWithParentheses(t *testing.T) {
	dir := shortDir(t)
	const name = "sl) (eep" // comm is the basename, up to 15 characters
	before := time.Now()
	p := sleepNamed(t, dir, name)
	after := time.Now()
	comm, err := os.ReadFile("/proc/" + strconv.Itoa(p.pid) + "/comm")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSuffix(string(comm), "\n"); got != name {
		t.Fatalf("comm %q, want %q: the test is not exercising the case it means to", got, name)
	}
	info, alive, err := processInfo(p.pid)
	if err != nil || !alive {
		t.Fatalf("processInfo(%d) = %+v, alive=%v, %v", p.pid, info, alive, err)
	}
	assertCreatedAround(t, info.created, before, after, "a process whose comm holds brackets")
	if want := filepath.Join(dir, name); info.image != want {
		t.Errorf("image %q, want %q", info.image, want)
	}
}

// TestProcessInfoFallsBackToComm covers the other image branch: a process whose
// /proc/<pid>/exe cannot be read, where the name between the parentheses is all
// there is. A kernel thread is the case that exists on every Linux box. The
// fallback matters because sameDaemon reads the image: returning an empty
// string instead would refuse to verify a daemon whose exe link is unreadable.
func TestProcessInfoFallsBackToComm(t *testing.T) {
	const pid = 2 // kthreadd, whose exe link resolves to nothing
	if state, ok := statState(t, pid); !ok {
		t.Skip("no pid 2 on this machine")
	} else if state == "Z" || state == "X" {
		t.Skipf("pid 2 is in state %s", state)
	}
	if exe, err := os.Readlink("/proc/2/exe"); err == nil {
		t.Skipf("pid 2 has a readable exe link (%s); this is not a kernel thread", exe)
	}
	comm, err := os.ReadFile("/proc/2/comm")
	if err != nil {
		t.Skipf("cannot read /proc/2/comm: %v", err)
	}
	want := strings.TrimSuffix(string(comm), "\n")
	info, alive, err := processInfo(pid)
	if err != nil || !alive {
		t.Fatalf("processInfo(2) = %+v, alive=%v, %v", info, alive, err)
	}
	if info.image != want {
		t.Errorf("image %q, want the comm %q", info.image, want)
	}
}

// TestProcessInfoOnAnExitedPid pins the contract the callers depend on: a pid
// that is not there is not an error. LocateDaemon reads an error as "I cannot
// tell" and holds the state at unverified, which would leave a stale socket
// from a crashed daemon undeletable.
func TestProcessInfoOnAnExitedPid(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh: %v", err)
	}
	cmd := exec.Command(sh, "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.ProcessState.Pid()
	// Reaped, so the pid is free — unless the kernel has already handed it out.
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Skipf("pid %d is in use again: %v", pid, err)
	}
	info, alive, err := processInfo(pid)
	if err != nil {
		t.Fatalf("processInfo on a pid that is gone returned an error: %v", err)
	}
	if alive {
		t.Error("alive is true for a pid that no longer exists")
	}
	if info != (procInfo{}) {
		t.Errorf("info %+v, want the zero value when there is no process", info)
	}
}

// TestProcessInfoOnAZombie covers the state check. A process that has exited
// but whose parent has not reaped it still has a /proc/<pid>/stat, so reading
// the file is not enough to call it alive; only its state says so. A daemon
// left as a zombie by a parent that never waits would otherwise keep its socket
// and lockfile reserved for as long as that parent runs.
func TestProcessInfoOnAZombie(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh: %v", err)
	}
	cmd := exec.Command(sh, "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Wait()
		}
	})
	pid := cmd.Process.Pid
	deadline := time.Now().Add(10 * time.Second)
	for {
		state, ok := statState(t, pid)
		if !ok {
			t.Fatalf("pid %d disappeared without being reaped", pid)
		}
		if state == "Z" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d is in state %s, not a zombie", pid, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, alive, err := processInfo(pid)
	if err != nil {
		t.Fatalf("processInfo on a zombie returned an error: %v", err)
	}
	if alive {
		t.Error("alive is true for a zombie: its /proc entry outlives the process")
	}
	if info != (procInfo{}) {
		t.Errorf("info %+v, want the zero value for a zombie", info)
	}
	_ = cmd.Wait()
	reaped = true
	if _, ok := statState(t, pid); ok {
		t.Log("the pid still has a /proc entry after being reaped; it was reused")
	}
}

// TestBootTimeMatchesUptime checks the value every creation time is measured
// from, against the other thing the kernel publishes about the same instant.
// The two are computed from the same offsets, so they agree to well under a
// second on a quiet box; the tolerance here is for btime's truncation to
// seconds, and the assertion is about the field and its unit.
func TestBootTimeMatchesUptime(t *testing.T) {
	boot, err := bootTime()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		t.Fatal(err)
	}
	secs, err := strconv.ParseFloat(strings.Fields(string(data))[0], 64)
	if err != nil {
		t.Fatal(err)
	}
	derived := time.Now().Add(-time.Duration(secs * float64(time.Second)))
	if d := boot.Sub(derived); d > 10*time.Second || d < -10*time.Second {
		t.Errorf("bootTime %s, but /proc/uptime puts boot at %s (%s apart)", boot, derived, d)
	}
	if boot.Before(time.Unix(1_400_000_000, 0)) || boot.After(time.Now()) {
		t.Errorf("bootTime %s is not a plausible boot instant", boot)
	}
}
