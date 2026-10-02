//go:build linux || darwin

package pty

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string, mode os.FileMode) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	work := filepath.Join(root, "work")
	bin := filepath.Join(root, "bin")
	mk("work/claude", 0o755)
	mk("bin/claude", 0o755)
	mk("work/onlyhere", 0o755)
	mk("work/noexec", 0o644)
	mk("bin/tool", 0o755)
	if err := os.MkdirAll(filepath.Join(root, "dirbin", "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	mk("work/rel/tool2", 0o755)
	mk("abs/tool2", 0o755)

	for _, c := range []struct {
		name, prog, path string
		want             string // "" means an error containing wantErr
		wantErr          string
	}{
		{name: "bare name in PATH, not the worktree", prog: "claude", path: bin, want: filepath.Join(bin, "claude")},
		{name: "bare name only in the worktree", prog: "onlyhere", path: bin, wantErr: "not found in PATH"},
		{name: "explicit relative", prog: "./onlyhere", path: bin, want: filepath.Join(work, "onlyhere")},
		{name: "explicit relative, not executable", prog: "./noexec", path: bin, wantErr: "not executable"},
		{name: "directory on PATH is skipped", prog: "tool", path: filepath.Join(root, "dirbin") + ":" + bin, want: filepath.Join(bin, "tool")},
		{name: "dot", prog: ".", path: bin, wantErr: "not a program name"},
		{name: "dotdot", prog: "..", path: bin, wantErr: "not a program name"},
		{name: "empty PATH entry matches in the worktree first", prog: "onlyhere", path: ":" + bin, wantErr: "relative PATH entry"},
		{name: "empty PATH entry after the real one", prog: "claude", path: bin + ":", want: filepath.Join(bin, "claude")},
		{name: "relative PATH entry resolves against the worktree", prog: "tool2", path: "rel:" + filepath.Join(root, "abs"), wantErr: filepath.Join(work, "rel", "tool2")},
		{name: "absolute", prog: filepath.Join(bin, "tool"), path: "", want: filepath.Join(bin, "tool")},
		{name: "absolute directory", prog: filepath.Join(root, "dirbin", "tool"), path: bin, wantErr: "not executable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolve(c.prog, work, []string{"PATH=" + c.path})
			if c.want != "" {
				if err != nil || got != c.want {
					t.Fatalf("resolve = %q, %v; want %q", got, err, c.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("resolve = %q, %v; want an error containing %q", got, err, c.wantErr)
			}
		})
	}
}
