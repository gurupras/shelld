//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNetgoGraphHasNoCgo guards the release build (make build: -tags netgo):
// no package the binaries depend on under netgo may have cgo files, or they
// would link libc dynamically again and pay for it at every start. It checks
// the dependency graph, not that a given binary was built with the tag; a
// plain `go build` still links net's libc resolver.
//
// CGO_ENABLED=1 is forced for the listing: with cgo off, or without a C
// compiler, go list reports no cgo files whatever the imports are.
func TestNetgoGraphHasNoCgo(t *testing.T) {
	gotool := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOROOT() == "" { // built with -trimpath and GOROOT unset
		var err error
		if gotool, err = exec.LookPath("go"); err != nil {
			t.Skipf("go tool not available: %v", err)
		}
	}
	if _, err := os.Stat(gotool); err != nil {
		t.Skipf("go tool not available: %v", err)
	}
	cmd := exec.Command(gotool, "list", "-deps", "-tags", "netgo",
		"-f", "{{if .CgoFiles}}{{.ImportPath}}{{end}}",
		"github.com/gurupras/shelld/cmd/shelld", "github.com/gurupras/shelld/cmd/shellctl")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		t.Fatalf("packages with cgo files under -tags netgo:\n%s", s)
	}
}
