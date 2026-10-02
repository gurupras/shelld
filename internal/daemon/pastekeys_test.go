package daemon

import "testing"

// H27: paste's keys match exactly, as every other route's do. encoding/json
// folds case into a struct field, so {"Text":"hi"} was pasted; shelld-rs's serde
// struct ignores the key as unknown and refuses the body for its missing text.
func TestPasteKeysMatchExactly(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})
	const path = "/sessions/s/windows/w/paste"
	for _, body := range []string{`{"Text":"hi"}`, `{"TEXT":"hi"}`, `{"text":"hi","Mode":"bogus","mode":"nope"}`} {
		want := `{"error":"invalid request"}`
		if body == `{"text":"hi","Mode":"bogus","mode":"nope"}` {
			want = `{"error":"invalid paste mode"}`
		}
		if st, got := call(t, h, "POST", path, body); st != 400 || got != want {
			t.Errorf("paste %s: %d %s, want 400 %s", body, st, got, want)
		}
	}
	// The exact key still reaches the window, a fake one with no writer: 500.
	if st, got := call(t, h, "POST", path, `{"text":"hi","Mode":"bogus"}`); st != 500 {
		t.Errorf(`paste {"text":"hi","Mode":"bogus"}: %d %s, want the unknown Mode ignored and 500 from the fake window`, st, got)
	}
}
