//go:build linux || darwin

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// TestARelativeSocketIntoTheDefaultDirIsChecked: a relative --socket that
// names a path in the default directory must meet the same private-directory
// check as that path spelled absolutely.
func TestARelativeSocketIntoTheDefaultDirIsChecked(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "shelld-"+strconv.Itoa(os.Getuid()))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	run := func(sock string) (string, int) {
		c := exec.Command(shellctlBin, "--socket", sock, "attach", "s", "w")
		c.Dir = dir
		c.Env = append(os.Environ(), "TMPDIR="+tmp, "SHELLD_SOCKET=")
		var errb bytes.Buffer
		c.Stderr = &errb
		_ = c.Run()
		return errb.String(), c.ProcessState.ExitCode()
	}
	absErr, absCode := run(filepath.Join(dir, "shelld.sock"))
	relErr, relCode := run("shelld.sock")
	if absCode != 1 || absErr == "" {
		t.Fatalf("absolute socket in a world-writable default dir: exit %d, stderr %q; want the check's refusal", absCode, absErr)
	}
	if relErr != absErr || relCode != absCode {
		t.Fatalf("relative socket: exit %d, stderr %q\nabsolute socket: exit %d, stderr %q", relCode, relErr, absCode, absErr)
	}
}
