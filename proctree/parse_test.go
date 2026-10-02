//go:build linux

package proctree

// Tests for parseIDs, the one place in this package where a wrong answer is
// plausible rather than an error: /proc/<pid>/stat's comm field is arbitrary bytes
// inside parentheses, so a parser that splits on the FIRST ')' reads the comm's own
// contents as ids and reports success. bytes.LastIndexByte is what makes that
// impossible, and nothing else in the suite can tell the two apart — every fixture
// here is named `sh` or `sleep`.
//
// Two tests, and both are needed. The live one proves the shape is REACHABLE, using
// a real /proc line rather than a fabricated one: a process can put ')' in its own
// comm, and this test does, by writing the name the parser would trip over. The
// fabricated one pins the exact ids, including the values a first-paren parser would
// invent, and it runs even where the live one has to skip.

import (
	"bytes"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"testing"
)

// statWithParenInComm is a real stat line's shape with ')' inside comm. The ids are
// distinctive on purpose: 5678/91/42/987654 are what a last-paren parser reads, and
// 8/9/0 are what a first-paren parser reads out of the comm text " 7 8 9 0 1 2".
const statWithParenInComm = "1234 (sh) 7 8 9 0 1 2) S 5678 91 42 " +
	"0 -1 0 0 0 0 0 0 0 0 0 0 20 0 1 987654 12345678"

func TestParseIDsReadsFieldsAfterTheLastParen(t *testing.T) {
	id, ok := parseIDs([]byte(statWithParenInComm))
	if !ok {
		t.Fatalf("parseIDs refused a well-formed line: %q", statWithParenInComm)
	}
	want := procIDs{ppid: 5678, pgid: 91, sid: 42, start: 987654}
	if id != want {
		t.Errorf("parseIDs = %+v, want %+v. A first-paren split reads the comm's own "+
			"text and gives ppid 8, pgid 9, sid 0 — with ok true, which is why this "+
			"needs an exact assertion and not a nil check", id, want)
	}
}

func TestParseIDsRefusesALineItCannotRead(t *testing.T) {
	// Both of these are shapes /proc does not produce. They are here because the
	// alternative to refusing them is inventing a sid or a start time of 0, and a
	// fabricated id in this package is a pid to SIGKILL.
	cases := []struct {
		name string
		line string
	}{
		{"no_closing_paren", "1234 (sh S 5678 91 42 0 -1 0 0 0 0 0 0 0 0 0 0 20 0 1 987654"},
		{"too_few_fields", "1234 (sh) S 5678 91 42 0 -1"},
		{"nothing_after_the_paren", "1234 (sh)"},
		{"empty", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if id, ok := parseIDs([]byte(c.line)); ok {
				t.Errorf("parseIDs accepted %q and returned %+v", c.line, id)
			}
		})
	}
}

// TestParseIDsOnARealCommWithAParen is the reachability half. Writing to
// /proc/self/comm renames the CALLING THREAD, so the thread is locked for the
// duration and the line read is the thread's own — parseIDs is the same function
// either way, and a thread's stat carries the process's ppid and session.
func TestParseIDsOnARealCommWithAParen(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// /proc/self/task/<tid>/, not /proc/self/: /proc/self is the PROCESS, so
	// /proc/self/comm is the thread-group leader's name. A first version of this test
	// wrote there and read the stat here, which renamed the main thread and then
	// asserted on a different one — the read line still said `(proctree.test)` and the
	// test skipped on its own premise. Both paths now name the same task.
	task := "/proc/self/task/" + strconv.Itoa(syscall.Gettid())
	orig, err := os.ReadFile(task + "/comm")
	if err != nil {
		t.Skipf("cannot read %s/comm: %v", task, err)
	}
	// 15 bytes, which is TASK_COMM_LEN-1: a longer name is truncated by the kernel
	// and the truncation could drop the ')' this test is about.
	const name = "sh) 7 8 9 0 1 2"
	if err := os.WriteFile(task+"/comm", []byte(name), 0); err != nil {
		t.Skipf("cannot set this thread's name: %v", err)
	}
	t.Cleanup(func() { _ = os.WriteFile(task+"/comm", bytes.TrimSpace(orig), 0) })

	stat, err := os.ReadFile(task + "/stat")
	if err != nil {
		t.Fatalf("reading this thread's stat: %v", err)
	}
	// The premise, because the whole test is void if the kernel kept the name we
	// asked for out of the file: the line must carry more than one ')'.
	if n := bytes.Count(stat, []byte(")")); n < 2 {
		t.Skipf("premise: the line has %d ')' — the name did not take: %q", n, stat)
	}
	ppid := os.Getppid()
	// And the premise that the assertion below can distinguish the two parsers: a
	// first-paren split reads ppid 8 out of the comm, so a real ppid of 8 would make
	// the correct and the wrong answer identical.
	if ppid == 8 {
		t.Skip("premise: our ppid really is 8, so this line cannot separate the parsers")
	}

	id, ok := parseIDs(stat)
	if !ok {
		t.Fatalf("parseIDs refused a real stat line: %q", stat)
	}
	if id.ppid != ppid {
		t.Errorf("parseIDs read ppid %d from %q, want %d (8 is what a first-paren "+
			"split reads out of this comm)", id.ppid, stat, ppid)
	}
	if id.start == 0 {
		t.Errorf("parseIDs read start 0 from %q; every task has a nonzero start time", stat)
	}
}
