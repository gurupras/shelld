package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

// GET …/inject/queue carries expired alongside pending, because the enqueue
// already returned 202: a payload that the window gave up on is otherwise
// indistinguishable from one still waiting, and a client polling this route is
// the only thing that would notice either way.
//
// The field is always present and always an array. A client iterating it must not
// have to distinguish "nothing expired" from "this daemon does not report
// expiries" — Go marshals a nil slice as null, so the empty case is the one worth
// pinning.
func TestInjectQueueRouteAlwaysCarriesAnExpiredArray(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})
	if st, body := call(t, h, "POST", "/sessions/s/windows/w/inject", `{"payload":"hi"}`); st != 202 {
		t.Fatalf("inject: %d %q", st, body)
	}

	st, body := call(t, h, "GET", "/sessions/s/windows/w/inject/queue", "")
	if st != 200 {
		t.Fatalf("queue: %d %q", st, body)
	}
	if !strings.Contains(body, `"expired":[]`) {
		t.Errorf("body %q does not carry an empty expired array", body)
	}

	// pending and length still read as they did before expired was added: this
	// route's existing clients are the reason the field is additive.
	var v struct {
		Expired []json.RawMessage `json:"expired"`
		Length  int               `json:"length"`
		Pending []struct {
			ID      string `json:"id"`
			Payload string `json:"payload"`
		} `json:"pending"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	if v.Expired == nil {
		t.Errorf("expired decoded as nil, so it was null on the wire: %q", body)
	}
	if v.Length != 1 || len(v.Pending) != 1 || v.Pending[0].Payload != "hi" {
		t.Errorf("queue reads length %d pending %+v, want the one injected payload",
			v.Length, v.Pending)
	}
}
