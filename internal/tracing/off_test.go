//go:build !shelld_trace

package tracing

import "testing"

func TestDisabledByDefault(t *testing.T) {
	if Enabled {
		t.Fatal("tracing must be compiled out without the shelld_trace tag")
	}
	if Begin(KindRender, 1, 0) != 0 || NewID() != 0 || Intern("x") != 0 {
		t.Fatal("no-op build returned ids")
	}
}
