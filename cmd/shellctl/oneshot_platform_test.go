package main

import (
	"strings"
	"testing"
)

// The one-shot commands run only where their tests have: elsewhere they refuse,
// naming the platform, rather than run a client nobody has seen work there.
func TestOneShotRunsOnlyWhereTested(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		if err := oneShotUnsupported("capture", goos); err != nil {
			t.Fatalf("%s refused: %v", goos, err)
		}
	}
	err := oneShotUnsupported("capture", "windows")
	if err == nil || !strings.Contains(err.Error(), "windows") || !strings.Contains(err.Error(), "capture") {
		t.Errorf("windows: %v; want a refusal naming the command and the platform", err)
	}
}
