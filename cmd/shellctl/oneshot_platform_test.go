package main

import (
	"strings"
	"testing"
)

// The one-shot commands run only where their tests have: elsewhere they refuse,
// naming the platform, rather than run a client nobody has seen work there.
func TestOneShotRunsOnlyWhereTested(t *testing.T) {
	if err := oneShotUnsupported("capture", "linux"); err != nil {
		t.Fatalf("linux refused: %v", err)
	}
	for _, goos := range []string{"darwin", "windows", "freebsd"} {
		err := oneShotUnsupported("capture", goos)
		if err == nil || !strings.Contains(err.Error(), goos) || !strings.Contains(err.Error(), "capture") {
			t.Errorf("%s: %v; want a refusal naming the command and the platform", goos, err)
		}
	}
}
