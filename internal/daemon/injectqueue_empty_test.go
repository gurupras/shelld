//go:build linux || darwin

package daemon

import (
	"strings"
	"testing"
)

// Both arrays in this response come off the same line, and only one of them was
// normalized: an empty expired marshalled as [] because the handler replaces a nil
// slice, and an empty pending marshalled as null because nothing replaced that one.
// So a client reading the queue of a window with nothing in it is handed a null it
// has to special-case, from a field whose neighbour in the same object is an empty
// array. The reference daemon answers [] for pending -- it hands serde a Vec -- so
// this is a divergence and not a choice.
//
// Asserted on the raw body rather than through a decoder, because that is where
// the difference is: null and [] both decode to a nil slice in Go, which is why
// the existing tests that unmarshal this response could not see it.
func TestAnEmptyInjectQueueIsAnEmptyArrayAndNotNull(t *testing.T) {
	sock := boot(t)
	openWindow(t, sock, "s")

	st, _, raw := request(t, sock, "GET", "/sessions/s/windows/w/inject/queue", "")
	if st != 200 {
		t.Fatalf("status %d: %s", st, raw)
	}
	if strings.Contains(raw, "null") {
		t.Errorf("a queue with nothing in it answers with a null: %s", raw)
	}
	for _, want := range []string{`"pending":[]`, `"expired":[]`} {
		if !strings.Contains(raw, want) {
			t.Errorf("response does not contain %s: %s", want, raw)
		}
	}
}
