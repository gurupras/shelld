package daemon

import "testing"

// H26: application/+json names no subtype in front of its suffix (RFC 6838
// §4.2.8), so shelld-rs's json_content_type refuses it (shelld-daemon body.rs),
// and Go refuses it with whatever the route answers any other non-JSON type.
// Both decode paths are here: /keys reads a body map and /paste a struct.
func TestAPlusJSONSuffixNeedsASubtype(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})
	for _, tc := range []struct{ path, body string }{
		{"/sessions/s/windows/w/keys", `{"keys":"x"}`},
		{"/sessions/s/windows/w/paste", `{"text":"x"}`},
		{"/sessions", `{"name":"n"}`},
	} {
		wantSt, want := callAs(t, h, "POST", tc.path, "text/plain", tc.body)
		if wantSt != 400 {
			t.Fatalf("premise: POST %s as text/plain answered %d %s, not a 400", tc.path, wantSt, want)
		}
		if st, got := callAs(t, h, "POST", tc.path, "application/+json", tc.body); st != wantSt || got != want {
			t.Errorf("POST %s as application/+json: %d %s, want %d %s as for text/plain", tc.path, st, got, wantSt, want)
		}
		// A suffix with a subtype in front of it is still JSON.
		if st, got := callAs(t, h, "POST", tc.path, "application/x+json", tc.body); st == 400 {
			t.Errorf("POST %s as application/x+json: %d %s, want it accepted", tc.path, st, got)
		}
	}
}
