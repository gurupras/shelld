//go:build windows

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The daemon's pipe is owned by its user and grants access only to that user
// and SYSTEM, and a Go client's owner check accepts it.
func TestPipeOwnerOnly(t *testing.T) {
	pipe := `\\.\pipe\shelld-test-` + NewNonce()
	ln, err := listen(pipe, ListenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	conn, err := Dial(context.Background(), pipe, 2*time.Second)
	if err != nil {
		t.Fatalf("Dial with the owner check: %v", err)
	}
	defer conn.Close()
	f := conn.(interface{ Fd() uintptr })
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	me, _, _ := currentUser()
	if owner, _, _ := sd.Owner(); !owner.Equals(me) {
		t.Fatalf("owner %s, want %s", owner, me)
	}
	assertPipeDACLIsOwnerAndSystem(t, sd)
	if _, err := listen(pipe, ListenOptions{}); err == nil || !strings.Contains(err.Error(), "in use by another process") {
		t.Fatalf("second listener: %v", err)
	}
}

// assertPipeDACLIsOwnerAndSystem requires the pipe's DACL to grant exactly two
// trustees: the calling user and SYSTEM.
//
// It replaced a denylist -- `strings.Contains(sd.String(), ";;;WD)")` and the same
// for AU and BA -- which could not fail. pipeSDDL builds the descriptor from a
// fixed template, and with ListenOptions{} the only string it can produce is
// O:<sid>D:P(A;;GA;;;<sid>)(A;;GA;;;SY), optionally followed by S:(ML;;NW;;;HI)
// when elevated. WD and BA appear in no branch of it at all, and AU appears only
// under SharedAccess, which this case does not pass. So all three arms of the
// denylist were assertions about an output space the code controls and cannot
// reach: they would have stayed green if the template had been widened to grant,
// say, Everyone read access under a name they do not list.
//
// The ACEs are enumerated rather than matched against an expected SDDL string,
// because the descriptor being read here came back from the kernel through
// GetSecurityInfo rather than from pipeSDDL, and its spelling is Windows's to
// choose. The trustee set is the property; the text is not.
func assertPipeDACLIsOwnerAndSystem(t *testing.T, sd *windows.SECURITY_DESCRIPTOR) {
	t.Helper()
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("read the pipe's DACL: %v", err)
	}
	// A nil DACL is not an empty one: it grants everyone full access. Absent the
	// check it would read as "no ACE grants too much" and pass.
	if dacl == nil {
		t.Fatalf("the pipe has no DACL, which grants every user full access: %s", sd.String())
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("resolve the SYSTEM SID: %v", err)
	}
	me, _, _ := currentUser()
	want := map[string]bool{me.String(): true, system.String(): true}

	got := make(map[string]bool, dacl.AceCount)
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatalf("read ACE %d of %d: %v", i, dacl.AceCount, err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Errorf("ACE %d has type %d, want an access-allowed ACE: %s", i, ace.Header.AceType, sd.String())
			continue
		}
		// The SID is laid out immediately after the mask, which is what SidStart
		// names -- it is the first uint32 of the SID, not a pointer to it.
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		s := sid.String()
		if !want[s] {
			t.Errorf("the pipe's DACL grants %s, which is neither the owner nor SYSTEM: %s", s, sd.String())
		}
		got[s] = true
	}
	for s := range want {
		if !got[s] {
			t.Errorf("the pipe's DACL does not grant %s: %s", s, sd.String())
		}
	}
	if int(dacl.AceCount) != len(want) {
		t.Errorf("the pipe's DACL has %d ACEs, want %d: %s", dacl.AceCount, len(want), sd.String())
	}
}

// The lockfile can be replaced while another process reads it, both with the
// share-delete reader and, through the retry, with Go's os.Open.
func TestLockfileReplaceWhileRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shelld.pid")
	if err := WriteLockfile(path, LockInfo{Pid: 1, Socket: "s", Version: "v", StartedAt: 1}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(100 * time.Millisecond); f.Close() }()
	if err := WriteLockfile(path, LockInfo{Pid: 2, Socket: "s", Version: "v", StartedAt: 1}); err != nil {
		t.Fatalf("replace while os.Open holds it: %v", err)
	}
	if got, err := ReadLockfile(path); err != nil || got.Pid != 2 {
		t.Fatalf("read back %+v, %v", got, err)
	}
}
