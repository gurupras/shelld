//go:build linux || darwin

package daemon

import (
	"encoding/json"
	"fmt"
	"testing"
)

// TestANameWithAControlCharacterIsRefusedWhereItWouldBeCreated pins checkName
// at each of the four fields that create a name, word for word, since
// shelld-rs answers the same routes with the same body. Each refusal is also
// checked to have created nothing, and each field is also sent names that must
// pass, so a check that refused everything would red here as well.
func TestANameWithAControlCharacterIsRefusedWhereItWouldBeCreated(t *testing.T) {
	refused := []struct {
		name, code string
	}{
		{"x\r\ny", "U+000D"},
		{"tab\tx", "U+0009"},
		{"nul\x00", "U+0000"},
		{"del\x7f", "U+007F"},
		{"csi\u009b2J", "U+009B"}, // C1: one character that is a CSI
		{"nel\u0085", "U+0085"},
	}
	// Allowed as tmux allows them, and U+00A0, which is a space, not a control.
	allowed := []string{"has space", "has?q", "has#h", "has/slash", "nb\u00a0sp"}

	type route struct {
		field, method, path string
		body                func(name string) string
		created             func(st *State, name string) bool
	}
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	routes := []route{
		{"name", "POST", "/sessions",
			func(n string) string { return `{"name":` + q(n) + `}` },
			func(st *State, n string) bool { return st.Mgr.Session(n) != nil }},
		{"dstName", "POST", "/move-window",
			func(n string) string {
				return `{"srcSession":"s","srcName":"w","dstSession":"s","dstName":` + q(n) + `}`
			},
			func(st *State, n string) bool { return st.Mgr.Session("s").Window(n) != nil }},
		{"dstSession", "POST", "/move-window",
			func(n string) string { return `{"srcSession":"s","srcName":"w","dstSession":` + q(n) + `}` },
			func(st *State, n string) bool { return st.Mgr.Session(n) != nil }},
	}
	for _, r := range routes {
		for _, c := range refused {
			st, h := api(t, map[string][]string{"s": {"w"}})
			want := fmt.Sprintf(`{"error":"%s must not contain a control character, and it contains %s"}`, r.field, c.code)
			if code, body := call(t, h, r.method, r.path, r.body(c.name)); code != 400 || body != want {
				t.Errorf("%s %q: %d %s, want 400 %s", r.field, c.name, code, body, want)
			}
			if r.created(st, c.name) {
				t.Errorf("%s %q was created", r.field, c.name)
			}
		}
		for _, n := range allowed {
			st, h := api(t, map[string][]string{"s": {"w"}})
			if code, body := call(t, h, r.method, r.path, r.body(n)); code != 200 {
				t.Errorf("%s %q: %d %s, want 200", r.field, n, code, body)
			}
			if !r.created(st, n) {
				t.Errorf("%s %q answered 200 and created nothing", r.field, n)
			}
		}
	}

	// create-window refuses before it spawns anything, so it needs no shell.
	for _, c := range refused {
		st, h := api(t, map[string][]string{"s": nil})
		want := fmt.Sprintf(`{"error":"name must not contain a control character, and it contains %s"}`, c.code)
		if code, body := call(t, h, "POST", "/sessions/s/windows", `{"name":`+q(c.name)+`}`); code != 400 || body != want {
			t.Errorf("window name %q: %d %s, want 400 %s", c.name, code, body, want)
		}
		if st.Mgr.Session("s").Window(c.name) != nil {
			t.Errorf("window name %q was created", c.name)
		}
	}

	// Both dst fields bad: dstName is the one named, as shelld-rs orders them.
	_, h := api(t, map[string][]string{"s": {"w"}})
	want := `{"error":"dstName must not contain a control character, and it contains U+0009"}`
	if code, body := call(t, h, "POST", "/move-window", `{"srcSession":"s","srcName":"w","dstSession":"a\nb","dstName":"c\td"}`); code != 400 || body != want {
		t.Errorf("both dst fields bad: %d %s, want 400 %s", code, body, want)
	}
	// The empty name is still "name required", checked first.
	if code, body := call(t, h, "POST", "/sessions", `{"name":""}`); code != 400 || body != `{"error":"name required"}` {
		t.Errorf("empty name: %d %s", code, body)
	}
	// A lookup is not checked: a control character in srcName misses, 409.
	if code, body := call(t, h, "POST", "/move-window", `{"srcSession":"s","srcName":"w\tx","dstSession":"s"}`); code != 409 {
		t.Errorf("srcName with a tab: %d %s, want the 409 a missing window gets", code, body)
	}
}
