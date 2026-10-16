//go:build linux || darwin

package daemon

// What the API answers when a caller names something that is not there, sends a
// body the route cannot read, or asks for something the window cannot do.
//
// These are the answers a client acts on, and each one is a decision it cannot
// make for itself: 400 means "my request was wrong, retrying is pointless", 429
// means "retry once the program reads", 500 means "this window is gone". A route
// that returns the wrong one of those turns a recoverable condition into a stuck
// client, or a permanent failure into an infinite retry. The bodies are pinned
// word for word where a client parses them, because shelld-rs answers the same
// routes and clients switch on the text.
//
// One arm of routes.go is left uncovered and named here rather than left to be
// noticed: resize's 500. Window.Resize succeeds on a window with no PTY —
// setEffectiveSize has nothing to tell the kernel — and the hook that makes the
// ioctl fail is unexported and lives next to the window, so from this package
// there is no way to make the handler take that path.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/gurupras/shelld"
)

// windowRoute is one route under /sessions/:session/windows/:window, with a body
// that would be accepted if the window existed.
type windowRoute struct {
	method, suffix, body string
	// upgrade sends the WebSocket handshake headers, which attach requires
	// before it looks the window up at all.
	upgrade bool
}

var windowRoutes = []windowRoute{
	{method: "POST", suffix: "/keys", body: `{"keys":"x"}`},
	{method: "POST", suffix: "/raw-key", body: `{"key":"x"}`},
	{method: "POST", suffix: "/key-names", body: `{"names":["Up"]}`},
	{method: "POST", suffix: "/paste", body: `{"text":"x"}`},
	{method: "GET", suffix: "/capture"},
	{method: "GET", suffix: "/scroll"},
	{method: "POST", suffix: "/resize", body: `{"cols":80,"rows":24}`},
	{method: "POST", suffix: "/colors", body: `{"background":"#000000"}`},
	{method: "GET", suffix: "/attach", upgrade: true},
	{method: "GET", suffix: "/frame"},
	{method: "POST", suffix: "/inject", body: `{"payload":"x"}`},
	{method: "GET", suffix: "/inject/queue"},
}

func (wr windowRoute) call(t *testing.T, h http.Handler, session, window string) (int, string) {
	t.Helper()
	path := "/sessions/" + session + "/windows/" + window + wr.suffix
	if !wr.upgrade {
		return call(t, h, wr.method, path, wr.body)
	}
	r := httptest.NewRequest(wr.method, path, nil)
	setUpgradeHeaders(r)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

func setUpgradeHeaders(r *http.Request) {
	r.Header.Set("Connection", "upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
}

// TestEveryWindowRouteRefusesANameItDoesNotHave is the one test that has to
// cover every route rather than an interesting few. A window handler receives
// *shelld.Window from a lookup that returns nil, and every one of them is a
// separate hand-written nil check; a route that forgets it does not return a
// wrong status, it dereferences nil and panics. A panic in a handler kills the
// connection and leaves the other sessions' clients holding a socket that the
// daemon will never answer on, so the blast radius of one missing check is not
// the one request that hit it.
//
// A session name is client-supplied — a client addresses a session it
// believes exists and races its own DELETE — so a nonexistent name is an
// ordinary condition, not a malformed request.
//
// The bodies are asserted exactly, and that is what keeps this table honest: the
// router answers an unmatched path with 404 and an EMPTY body, so a typo in one
// of these paths would 404 for the wrong reason and pass a status-only check.
// The final loop is the other half of that guard: the same paths against names
// that do exist must not 404 (nor 405 — also an empty body).
func TestEveryWindowRouteRefusesANameItDoesNotHave(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})

	const noSession = `{"error":"session not found"}`
	const noWindow = `{"error":"window not found"}`

	for _, wr := range windowRoutes {
		if st, body := wr.call(t, h, "nope", "w"); st != 404 || body != noSession {
			t.Errorf("%s %s with an unknown session: %d %q, want 404 %s", wr.method, wr.suffix, st, body, noSession)
		}
		if st, body := wr.call(t, h, "s", "nope"); st != 404 || body != noWindow {
			t.Errorf("%s %s with an unknown window: %d %q, want 404 %s", wr.method, wr.suffix, st, body, noWindow)
		}
	}

	// The routes that take a session but no window, and killWindow with an
	// unknown session (TestDeleteOfWhatIsGoneIs404 has its unknown window).
	for _, r := range []struct{ method, path, body string }{
		{"GET", "/sessions/nope/windows", ""},
		{"POST", "/sessions/nope/windows", `{"name":"x","cols":80,"rows":24}`},
		{"DELETE", "/sessions/nope/windows/w", ""},
	} {
		if st, body := call(t, h, r.method, r.path, r.body); st != 404 || body != noSession {
			t.Errorf("%s %s: %d %q, want 404 %s", r.method, r.path, st, body, noSession)
		}
	}

	// Every path above is a real route reached with real names: without this, a
	// mistyped path would 404 from the router and every case would pass.
	for _, wr := range windowRoutes {
		if st, body := wr.call(t, h, "s", "w"); st == 404 || st == 405 {
			t.Errorf("%s %s with names that exist: %d %q — the path is not the route it is meant to be",
				wr.method, wr.suffix, st, body)
		}
	}
}

// TestDeleteOfWhatIsGoneIs404 pins that a DELETE names something that exists,
// as tmux's kill-session and kill-window do ("can't find session" / "can't find
// window", exit 1). A delete that succeeds is answered once; repeating it, or
// naming a window or session that never existed, is a 404 with the same body as
// every other route that misses.
func TestDeleteOfWhatIsGoneIs404(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})
	const ok = `{"ok":true}`
	const noSession = `{"error":"session not found"}`
	const noWindow = `{"error":"window not found"}`
	for _, c := range []struct{ path, want string }{
		{"/sessions/s/windows/w", ok},          // there
		{"/sessions/s/windows/w", noWindow},    // deleted a moment ago by this very test
		{"/sessions/s/windows/nope", noWindow}, // never existed
		{"/sessions/nope/windows/w", noSession},
		{"/sessions/s", ok},
		{"/sessions/s", noSession},
		{"/sessions/nope", noSession},
	} {
		wantSt := 404
		if c.want == ok {
			wantSt = 200
		}
		if st, body := call(t, h, "DELETE", c.path, ""); st != wantSt || body != c.want {
			t.Errorf("DELETE %s: %d %q, want %d %s", c.path, st, body, wantSt, c.want)
		}
	}
}

// TestEscapedNamesAddressTheRightWindow covers why the router splits
// URL.EscapedPath and unescapes each segment itself, rather than using the
// already-decoded URL.Path. Window names come from whoever is driving the
// daemon, and a client may name windows after tasks, files or branches. A
// name holding '/' is the case that decides the design: decoded first, "a/b"
// becomes two path segments and the request matches no route at all, so the
// window would be unreachable by the name the client just created it under.
func TestEscapedNamesAddressTheRightWindow(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"feat/x", "a b", "w#1", "100%"}})
	for _, name := range []string{"feat/x", "a b", "w#1", "100%"} {
		path := "/sessions/s/windows/" + url.PathEscape(name) + "/frame"
		st, body := call(t, h, "GET", path, "")
		if st != 200 {
			t.Errorf("GET %s (window %q): %d %q, want 200", path, name, st, body)
		}
	}
	// The same name sent undecoded is a different path, and must not resolve to
	// the window by accident.
	if st, body := call(t, h, "GET", "/sessions/s/windows/feat/x/frame", ""); st != 404 || body != "" {
		t.Errorf("an unescaped '/' in a window name: %d %q, want 404 and no body", st, body)
	}
}

// TestInputRoutesRejectABodyTheyCannotRead covers the fields where silently
// accepting a value the caller did not write would send different bytes to the
// program than the caller asked for. "enter" is the clearest: it defaults to
// true, so a route that shrugged at `"enter":"no"` would press Enter on a
// command the client explicitly asked it not to submit — a partially typed
// command run against a live shell. The same reasoning covers the content type
// and the trailing-garbage cases: a body that was not the JSON object the client
// believes it sent must not be half-applied.
func TestInputRoutesRejectABodyTheyCannotRead(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})
	const base = "/sessions/s/windows/w"
	// ctype is the Content-Type header; empty means application/json and "-"
	// means no header at all.
	cases := []struct {
		path, ctype, body, want string
	}{
		{path: "/keys", body: `{}`, want: "invalid request"},
		{path: "/keys", body: `{"keys":null}`, want: "invalid request"},
		{path: "/keys", body: `{"keys":42}`, want: "invalid request"},
		{path: "/keys", body: `{"keys":"x","enter":"no"}`, want: "invalid request"},
		{path: "/keys", body: `{"keys":"x","enter":0}`, want: "invalid request"},
		{path: "/keys", body: `{"keys":"x","enter_delay_ms":-1}`, want: "invalid request"},
		{path: "/keys", body: `{"keys":"x","enter_delay_ms":"5"}`, want: "invalid request"},
		{path: "/raw-key", body: `{}`, want: "invalid request"},
		{path: "/raw-key", body: `{"key":null}`, want: "invalid request"},
		{path: "/paste", body: `{}`, want: "invalid request"},
		{path: "/paste", body: `{"text":["x"]}`, want: "invalid request"},
		{path: "/inject", body: `{}`, want: "payload required"},
		{path: "/inject", body: `{"payload":""}`, want: "payload required"},
		{path: "/inject", body: `{"payload":"x","priority":"high"}`, want: "payload required"},
		{path: "/inject", body: `{"payload":"x","priority":2147483648}`, want: "payload required"},

		// Not a JSON object at all. A body the decoder stopped early on is not a
		// request: the fields it did read are an arbitrary prefix.
		{path: "/keys", body: `null`, want: "invalid request"},
		{path: "/keys", body: `["keys"]`, want: "invalid request"},
		{path: "/keys", body: `{"keys":"x"}{"keys":"y"}`, want: "invalid request"},
		{path: "/keys", body: ``, want: "invalid request"},
		{path: "/keys", ctype: "text/plain", body: `{"keys":"x"}`, want: "invalid request"},
		{path: "/keys", ctype: "application/x-www-form-urlencoded", body: `keys=x`, want: "invalid request"},
		{path: "/keys", ctype: "-", body: `{"keys":"x"}`, want: "invalid request"},
	}
	for _, tc := range cases {
		ctype := tc.ctype
		switch ctype {
		case "":
			ctype = "application/json"
		case "-":
			ctype = ""
		}
		st, body := callAs(t, h, "POST", base+tc.path, ctype, tc.body)
		want := `{"error":"` + tc.want + `"}`
		if st != 400 || body != want {
			t.Errorf("POST %s %s (content type %q): %d %q, want 400 %s", tc.path, tc.body, ctype, st, body, want)
		}
	}

	// A content type of application/…+json is accepted, as axum accepts it: the
	// rejections above must be about the body, not about a client that labelled
	// its JSON precisely.
	if st, body := callAs(t, h, "POST", base+"/keys", "application/vnd.shelld+json", `{"keys":"x"}`); st == 400 {
		t.Errorf("a +json content type was rejected: %d %q", st, body)
	}
}

// TestInputRoutesAgreeOnADeadWindow: every input route maps a window with no
// writer to 500, and none of them reports success. A window whose program has
// exited still exists and still answers — capture and frame return its final
// screen, which is the point of keeping it — so "the window is there" and "input
// will reach a program" are different questions, and only the status code
// distinguishes them. A route answering 200 here would tell a client its
// keystrokes were delivered to a shell that is not running.
//
// 500 and not 429 because the two are the client's retry decision: 429 is "the
// program is not reading, retry"; this window will never read again.
func TestInputRoutesAgreeOnADeadWindow(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})
	const base = "/sessions/s/windows/w"
	for _, tc := range []struct{ path, body string }{
		{"/keys", `{"keys":"x"}`},
		{"/keys", `{"keys":"x","enter":false}`},
		{"/keys", `{"keys":"x","enter_delay_ms":10}`},
		{"/raw-key", `{"key":"\u0003"}`},
		{"/key-names", `{"names":["Up"]}`},
		{"/key-names", `{"names":["Up"],"strict":true}`},
		{"/paste", `{"text":"x"}`},
	} {
		st, body := call(t, h, "POST", base+tc.path, tc.body)
		var v struct{ Error string }
		_ = json.Unmarshal([]byte(body), &v)
		if st != 500 || v.Error == "" {
			t.Errorf("POST %s %s on a window with no writer: %d %q, want 500 naming the failure", tc.path, tc.body, st, body)
		}
	}
	// The window is still readable, which is what makes the 500s above a
	// statement about input rather than about the window being gone.
	if st, _ := call(t, h, "GET", base+"/frame", ""); st != 200 {
		t.Errorf("frame on a window with no writer: %d, want 200", st)
	}
}

// TestKeyNamesReportsEmptyListsAsArrays covers the shape of the strict-mode
// error, not its status. The two lists are what a caller displays to say which
// names it got wrong, and a caller reads them as arrays — `data.lossy.length`,
// or a loop over them. JSON null is not an array: encoded as null, the field
// that happens to be empty crashes the client that is handling the error, which
// is the worst moment for a second failure. One list is empty in almost every
// real failure, because a typo and a modifier that cannot be encoded are
// different mistakes and a caller rarely makes both at once.
func TestKeyNamesReportsEmptyListsAsArrays(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})
	const path = "/sessions/s/windows/w/key-names"
	for _, tc := range []struct{ body, want string }{
		{`{"names":["Entr"],"strict":true}`,
			`{"error":"unknown key names [\"Entr\"]","lossy":[],"unknown":["Entr"]}`},
		{`{"names":["S-Enter"],"strict":true}`,
			`{"error":"key names that lose modifiers [\"S-Enter\"]","lossy":["S-Enter"],"unknown":[]}`},
	} {
		st, body := call(t, h, "POST", path, tc.body)
		if st != 400 || body != tc.want {
			t.Errorf("POST %s: %d %q\nwant 400 %s", tc.body, st, body, tc.want)
		}
	}
}

// TestScrollRefusesAQueryItCannotRead covers the one failure mode in this file
// that is not a status code but a wrong answer. offset and lines are parsed from
// the query with a default, and a parse this route ignored would fall back to
// that default: `?offset=abc` would return the FIRST page of history with a 200,
// and a pager walking a long scrollback would be handed the top of the buffer as
// if it were the page it asked for. Nothing in the response says which page it
// is, so the client cannot detect it.
//
// The text is byte for byte what shelld-rs's serde-based extractor produces, and
// the content type is plain text rather than JSON for the same reason: a client
// parsing one daemon's error must not have to special-case the other.
func TestScrollRefusesAQueryItCannotRead(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})
	const want = "Failed to deserialize query string: invalid digit found in string"
	for _, q := range []string{
		"/scroll?offset=abc",
		"/scroll?offset=-1",
		"/scroll?lines=abc",
		"/scroll?lines=1.5",
		"/scroll?max_bytes=abc",
		"/scroll?max_bytes=99999999999999999999",
		"/capture?lines=abc",
		"/capture?max_bytes=abc",
		"/capture?mode=visible&lines=abc",
	} {
		r := httptest.NewRequest("GET", "/sessions/s/windows/w"+q, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 || w.Body.String() != want {
			t.Errorf("GET %s: %d %q, want 400 %q", q, w.Code, w.Body.String(), want)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("GET %s: content type %q, want text/plain", q, ct)
		}
	}
	// An absent parameter takes its default; a parameter present with an empty
	// value does not, it is refused like any other unreadable value. The two are
	// distinguishable here and a client that builds its query from an unset
	// variable will hit the second: `?lines=` is a 400, not lines=50.
	if st, body := call(t, h, "GET", "/sessions/s/windows/w/scroll", ""); st != 200 {
		t.Errorf("GET /scroll with no parameters: %d %q, want 200 and the defaults", st, body)
	}
	for _, q := range []string{"?lines=", "?offset=", "?max_bytes="} {
		if st, got := call(t, h, "GET", "/sessions/s/windows/w/scroll"+q, ""); st != 400 || got != want {
			t.Errorf("GET /scroll%s: %d %q, want 400 %q", q, st, got, want)
		}
	}
	// UNVERIFIED against shelld-rs, for the empty value only: Rust's integer
	// parse reports an empty string through a different error variant than a bad
	// digit ("cannot parse integer from empty string"), so the two daemons may
	// answer this one input with the same 400 and different text. Every other row
	// above is a bad digit, where the text is the one asserted here. A client
	// should switch on the status.
}

// TestCaptureAndScrollRefuseAQueryNetURLCannotParse covers the case the test above
// cannot reach, because it is not a bad value but a query net/url gives up on
// wholesale. url.URL.Query throws the error away and returns what it salvaged,
// and since Go 1.17 a `;` is an objection that costs the entire segment — so a
// route reading the query loosely sees *no* parameters and takes every default at
// once. The status would be 200.
//
// Each row below is checked twice: once as sent, which must be refused, and once
// with the `;` removed, which must be accepted and must come back carrying the
// parameter. Without that second half the test would pass against a route that
// ignores its query entirely, and would not show that the rejected request was
// asking for anything in particular.
//
// What that second half can and cannot show is worth being exact about, because
// it is weaker than it looks. api uses InsertFakeWindow, so the window holds no
// output: every reply here has output "" and totalLines 0 whatever was asked for.
// So wantInGood can only assert an ECHOED parameter, never an effect on content.
// `"offset":10` shows scroll received the offset, not that it read from there --
// a scroll that echoed the offset and paged from 0 would satisfy it. The effect
// is covered by TestCaptureByteCap, which seeds real history and compares the
// pages; this row's job is only to show that the refused request above was
// carrying a value, which an all-defaults reply would not have.
//
// The `mode` row is strengthened past that limit by a negative control below,
// since `"mode":"visible"` on its own only shows the field is present.
//
// wantInGood is empty for a parameter with no visible echo either, and the row
// then checks the rejection only. That is not a gap to be filled with any field
// that happens to be in the response: `truncated` is in every scroll reply
// whatever max_bytes was, so asserting it would look like a control and test
// nothing. max_bytes is pinned on the parser instead, by
// TestMaxBytesQueryClampsWhatAClientAsksFor, for the reason given there -- only a
// window already holding more than the cap answers differently because of it.
func TestCaptureAndScrollRefuseAQueryNetURLCannotParse(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})
	const want = "Failed to deserialize query string: malformed query string"
	for _, tc := range []struct {
		bad, good, wantInGood, why string
	}{
		{
			bad: "/scroll?offset=10;lines=5", good: "/scroll?offset=10&lines=5",
			wantInGood: `"offset":10`,
			why:        "a pager asking for a page inside the buffer gets offset 0 -- the top -- as if it were that page",
		},
		{
			bad: "/capture?lines=1000;mode=visible", good: "/capture?lines=1000&mode=visible",
			wantInGood: `"mode":"visible"`,
			why:        "not a shorter capture but a different one: raw history in place of the visible grid",
		},
		{
			bad: "/capture?mode=visible;", good: "/capture?mode=visible",
			wantInGood: `"mode":"visible"`,
			why:        "a trailing `;` is enough, so a client that joins parameters with the wrong separator loses all of them",
		},
		{
			bad: "/scroll?max_bytes=1;", good: "/scroll?max_bytes=1",
			why: "max_bytes defaulting to 0 means no cap, the opposite of the bound that was asked for",
		},
		{
			// On /scroll rather than /capture so this row gets the positive control
			// the others have: capture's reply never names lines (it reports
			// linesReturned, which is 0 here whatever was asked), while scroll
			// echoes `"lines":5`. The parameter and the parse error are the same
			// either way.
			bad: "/scroll?lines=%zz", good: "/scroll?lines=5",
			wantInGood: `"lines":5`,
			why:        "a `;` is not the only thing ParseQuery objects to -- a bad percent-escape is another, and here it hits the parameter the route reads",
		},
	} {
		if st, got := call(t, h, "GET", "/sessions/s/windows/w"+tc.bad, ""); st != 400 || got != want {
			t.Errorf("GET %s: %d %q, want 400 %q\n  %s", tc.bad, st, got, want, tc.why)
		}
		st, got := call(t, h, "GET", "/sessions/s/windows/w"+tc.good, "")
		if st != 200 {
			t.Errorf("GET %s: %d %q, want 200 -- the same query minus the `;` must still be served", tc.good, st, got)
			continue
		}
		if tc.wantInGood == "" {
			continue
		}
		if !strings.Contains(got, tc.wantInGood) {
			t.Errorf("GET %s: body %q does not contain %s; the refused request above was asking for something this route does not otherwise return, and without that this test proves nothing about what was lost", tc.good, got, tc.wantInGood)
		}
	}
	// The negative half of the `mode` row. Asserting that `?mode=visible` comes
	// back saying "visible" shows the field is present, not that it followed the
	// query: a raw capture that emitted `"mode":"visible"` unconditionally passes
	// every assertion above. So the default must be shown NOT to claim it. This is
	// the strongest control the fixture allows, because the two modes return
	// different content and this window has none.
	if st, got := call(t, h, "GET", "/sessions/s/windows/w/capture", ""); st != 200 || strings.Contains(got, `"mode":"visible"`) {
		t.Errorf("GET /capture with no mode: %d %q, want 200 without a visible mode -- a reply that says `visible` whatever was asked makes the mode row above vacuous", st, got)
	}
	// The order of the two refusals, which is not otherwise pinned anywhere. The
	// strict parse needs no window, so it can be hoisted above the lookup without
	// anything failing to compile or any test going red -- and that would turn this
	// whole class from 404 into 400, telling a client its query was malformed when
	// the real answer is that the window is gone. attach settled the order first
	// (upgrade check, then window, then query), and capture and scroll match it.
	for _, tc := range []struct{ path, want string }{
		{"/sessions/nope/windows/nope/capture?lines=10;mode=visible", "session not found"},
		{"/sessions/s/windows/nope/scroll?offset=10;lines=5", "window not found"},
	} {
		if st, got := call(t, h, "GET", tc.path, ""); st != 404 || !strings.Contains(got, tc.want) {
			t.Errorf("GET %s: %d %q, want 404 mentioning %q -- the missing window is the client's actual problem and must be reported ahead of the query", tc.path, st, got, tc.want)
		}
	}
	// The strict parse must not turn a query the route has always accepted into an
	// error. An unknown parameter NAME is not a parse error, so it stays ignored,
	// which is what lets an older client talk to a newer daemon.
	//
	// This loop is what bounds the strictness from above, and it has to name the
	// cases a stricter parser would plausibly reject, not just obviously fine ones.
	// The rejection rows above only establish a floor: every one of them still
	// passes if the parser grows an extra rule and starts refusing queries it used
	// to serve. The last two rows are those extra rules, both of which attach does
	// enforce and neither of which belongs here. A repeated key is a contradiction
	// for attach because its one parameter decides whether the client may type,
	// where lines=5&lines=7 asks for two page sizes and the first-value rule answers
	// one of them -- a client that does that gets a page, not an error. An empty key
	// is a stray `&` or `=`, which ParseQuery reads as a nameless parameter; it is an
	// unknown name like any other and is ignored.
	for _, q := range []string{
		"", "?lines=1", "?somethingelse=1", "?lines=1&somethingelse=1", "?lines=%35",
		"?lines=5&lines=7", "?=1&lines=5",
	} {
		if st, got := call(t, h, "GET", "/sessions/s/windows/w/capture"+q, ""); st != 200 {
			t.Errorf("GET /capture%s: %d %q, want 200 -- a name this daemon does not know, and a value that is merely escaped, are both readable and must still be served", q, st, got)
		}
	}
	// This one IS a behaviour change, and is asserted so that it is a decision
	// rather than a side effect. A bad percent-escape costs only its own segment,
	// so each query below reached the route with the parameter it reads intact and
	// was answered correctly before; now it is a 400. The reasoning is in
	// parseQueryStrict's doc. What pins it as consistent rather than arbitrary is
	// the attach row: attach has answered 400 here since it started parsing
	// strictly, so the refusal is this daemon's existing behaviour and these two
	// routes were the ones out of step.
	for _, q := range []string{"/capture?lines=5&junk=%zz", "/scroll?offset=1&junk=100%", "/capture?lines=5&%zz=1"} {
		if st, got := call(t, h, "GET", "/sessions/s/windows/w"+q, ""); st != 400 || got != want {
			t.Errorf("GET %s: %d %q, want 400 %q", q, st, got, want)
		}
	}
	r := httptest.NewRequest("GET", "/sessions/s/windows/w/attach?observer=1&junk=%zz", nil)
	for k, v := range map[string]string{
		"Connection": "Upgrade", "Upgrade": "websocket",
		"Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==",
	} {
		r.Header.Set(k, v)
	}
	wrec := httptest.NewRecorder()
	h.ServeHTTP(wrec, r)
	if wrec.Code != 400 || wrec.Body.String() != want {
		t.Errorf("GET /attach?observer=1&junk=%%zz: %d %q, want 400 %q -- if attach does NOT refuse this, then capture and scroll refusing it is a new rule for two routes and not the consistency this change claims", wrec.Code, wrec.Body.String(), want)
	}
}

// TestCaptureAndScrollRefuseANearMissOfAKeyTheyRead covers the gap that strict
// parsing left open. Parsing strictly fixed the queries net/url cannot read; these
// are queries it reads perfectly well, where the key is a recognisable misspelling
// of one the route knows. `?Offset=500` was answered 200 with offset at its default
// of 0 -- the top of the scrollback returned as the page that was asked for, with
// nothing in the response to say so. A client paging backwards through history got
// the oldest page every time and could not tell.
//
// Each row asserts the exact message, not just the status, because the status alone
// does not say the near miss is why: capture and scroll answer 400 for a malformed
// query and for an unparsable value too, so a row asserting only 400 would pass if
// the request were refused for either of those instead.
//
// The correctly-spelled query is then required to be served AND to take effect
// where the route echoes it. Without that second half every row would pass against
// a route that refused its parameter outright, which is a worse bug than the one
// being fixed. max_bytes has no echo, so its row checks only the refusal, for the
// reason given in TestMaxBytesQueryClampsWhatAClientAsksFor.
//
// The two forms here are the two queryKeyNearMiss recognises. A plain typo
// (`?ofset=10`) is deliberately NOT refused and is asserted below as still served:
// it cannot be told apart from a parameter a newer client added, and refusing it
// would give up the ignore-unknown contract that lets an old daemon serve a new
// client. That is a real remaining gap, and pinning it here keeps it a decision.
func TestCaptureAndScrollRefuseANearMissOfAKeyTheyRead(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})
	msg := func(got, want string) string {
		return "Failed to deserialize query string: query key `" + got + "` is not `" + want + "`: a near miss of a known key is refused, not ignored"
	}
	for _, tc := range []struct{ bad, got, want, good, wantInGood, why string }{
		{
			bad: "/scroll?Offset=10", got: "Offset", want: "offset",
			good: "/scroll?offset=10", wantInGood: `"offset":10`,
			why: "a case variant is what a client written to a camelCase convention sends, and the default it fell back to is the top of the scrollback",
		},
		{
			bad: "/scroll?offset%3d10", got: "offset=10", want: "offset",
			good: "/scroll?offset=10", wantInGood: `"offset":10`,
			why: "ParseQuery splits on `=` before unescaping, so an over-escaped URL becomes one key NAMED `offset=10` and the offset reads as absent",
		},
		{
			bad: "/capture?Mode=visible", got: "Mode", want: "mode",
			good: "/capture?mode=visible", wantInGood: `"mode":"visible"`,
			why: "raw scrollback in place of the visible grid -- a different answer, not a smaller one",
		},
		{
			bad: "/capture?Lines=5", got: "Lines", want: "lines",
			good: "/scroll?lines=5", wantInGood: `"lines":5`,
			why: "the same key is read by both routes, and the good half uses scroll because capture does not echo lines",
		},
		{
			bad: "/scroll?Max_Bytes=1", got: "Max_Bytes", want: "max_bytes",
			good: "/scroll?max_bytes=1",
			why:  "max_bytes defaulting to 0 means no cap, the opposite of the bound that was asked for",
		},
	} {
		t.Run(tc.got, func(t *testing.T) {
			want := msg(tc.got, tc.want)
			if st, got := call(t, h, "GET", "/sessions/s/windows/w"+tc.bad, ""); st != 400 || got != want {
				t.Errorf("GET %s: %d %q, want 400 %q\n  %s", tc.bad, st, got, want, tc.why)
			}
			st, got := call(t, h, "GET", "/sessions/s/windows/w"+tc.good, "")
			if st != 200 {
				t.Fatalf("GET %s: %d %q, want 200 -- the correctly-spelled key must still be served, or this rule refuses the parameter rather than the misspelling", tc.good, st, got)
			}
			if tc.wantInGood != "" && !strings.Contains(got, tc.wantInGood) {
				t.Errorf("GET %s: body %q does not contain %s -- the refused spelling above was asking for something this route acts on, and without that the row shows nothing was lost", tc.good, got, tc.wantInGood)
			}
		})
	}
	// The near-miss rule must not swallow the ignore-unknown contract. A name from
	// another vocabulary is still ignored, including one that merely starts the same
	// way or differs by more than case, and including the plain typo this rule cannot
	// catch.
	for _, q := range []string{
		"?ofset=10", "?offsets=10", "?offset_=10", "?lineshere=1", "?xmode=visible",
	} {
		if st, got := call(t, h, "GET", "/sessions/s/windows/w/scroll"+q, ""); st != 200 {
			t.Errorf("GET /scroll%s: %d %q, want 200 -- only a case variant or an encoded separator is a near miss; anything else is indistinguishable from a newer client's parameter and must be ignored", q, st, got)
		}
	}
	// The order relative to the window lookup, as for the strict parse: a request for
	// a window that is gone is told the window is gone.
	if st, got := call(t, h, "GET", "/sessions/s/windows/nope/scroll?Offset=10", ""); st != 404 || !strings.Contains(got, "window not found") {
		t.Errorf("GET a missing window with a near-miss key: %d %q, want 404 mentioning the window -- the missing window is the client's actual problem", st, got)
	}
}

// TestMaxBytesQueryClampsWhatAClientAsksFor pins a ceiling on an allocation the
// caller chooses. max_bytes reaches CaptureBytes, which sizes the response from
// it, and the daemon shares a box with the program it is recording: a caller
// asking for 2^62 must not make it try to build a response no client could
// receive. The clamp cannot be seen in a response body — only a window already
// holding more than 64 MiB would return fewer bytes because of it — so it is
// asserted on the parser, which is where the number stops being the client's.
func TestMaxBytesQueryClampsWhatAClientAsksFor(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  int
		ok    bool
	}{
		{query: "", want: 0, ok: true}, // absent: the window's own default
		{query: "max_bytes=1", want: 1, ok: true},
		{query: "max_bytes=67108864", want: maxReadBytes, ok: true}, // exactly the ceiling
		{query: "max_bytes=67108865", want: maxReadBytes, ok: true}, // one byte past it
		{query: "max_bytes=4611686018427387904", want: maxReadBytes, ok: true},
		{query: "max_bytes="},
		{query: "max_bytes=abc"},
		{query: "max_bytes=-1"},
	} {
		q, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatalf("ParseQuery(%q): %v", tc.query, err)
		}
		got, ok := maxBytesQuery(q)
		if ok != tc.ok || got != tc.want {
			t.Errorf("maxBytesQuery(%q) = %d, %v; want %d, %v", tc.query, got, ok, tc.want, tc.ok)
		}
	}
}

// TestAnEmptyNameSegmentMatchesNoRoute covers match's refusal to bind a
// parameter to an empty segment. A client that builds a URL by concatenation
// produces "/sessions/s/windows/" + name whenever name is unset, and DELETE is
// the case that decides this: it is idempotent by design and answers 200 for a
// window that is already gone, so an empty :window would report {"ok":true} for
// a request that named nothing and killed nothing, and the caller would go on
// believing its window was cleaned up. Refusing the match makes it a 404, which
// is the same answer the client would get for a typo.
func TestAnEmptyNameSegmentMatchesNoRoute(t *testing.T) {
	st, h := api(t, map[string][]string{"s": {"w"}})
	for _, tc := range []struct{ method, path string }{
		{"DELETE", "/sessions/s/windows/"},
		{"DELETE", "/sessions//windows/w"},
		{"GET", "/sessions/s/windows//frame"},
		{"GET", "/sessions//windows/w/frame"},
		{"GET", "/sessions//windows"},
		{"GET", "/sessions/"},
		{"POST", "/sessions/s/windows//keys"},
	} {
		if code, body := call(t, h, tc.method, tc.path, `{"keys":"x"}`); code != 404 || body != "" {
			t.Errorf("%s %s: %d %q, want 404 and no body", tc.method, tc.path, code, body)
		}
	}
	// The DELETEs above must not have taken anything with them.
	if sess := st.Mgr.Session("s"); sess == nil {
		t.Fatal("session s is gone after a DELETE that named no window")
	} else if sess.Window("w") == nil {
		t.Fatal("window w is gone after a DELETE that named no window")
	}
}

// TestSetColorsAppliesWhatItCanParse pins the contract that makes this route
// unlike the input routes: it is partial. A palette is sent as a whole — a
// theme, 16 or 256 entries at once — and an entry this daemon cannot read is
// dropped while the rest are applied, so one unreadable colour does not leave
// the window with no theme at all. That makes the 200 the interesting answer,
// and the two 400s have to stay distinguishable from it and from each other:
// "colors required" means the body was not a colours request, while "no valid
// colors provided" means it was and nothing in it survived parsing — the second
// is a theme the client should look at, the first is a bug in the client.
//
// The index bound is asserted at both sides because a palette index is a byte:
// 255 is the last valid entry, and accepting 256 would write an entry no
// terminal sequence can ever name.
func TestSetColorsAppliesWhatItCanParse(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})
	const path = "/sessions/s/windows/w/colors"
	for _, tc := range []struct {
		body       string
		wantStatus int
		wantBody   string
	}{
		{`{"background":"#1e1e2e"}`, 200, `{"ok":true}`},
		{`{"foreground":"ffffff"}`, 200, `{"ok":true}`},
		{`{"cursor":"+f+f+f"}`, 200, `{"ok":true}`}, // Rust's from_str_radix takes a leading '+'
		{`{"palette":{"255":"000000"}}`, 200, `{"ok":true}`},
		// A palette KEY takes a leading '+' for the same reason a channel does:
		// the reference parses it with usize::from_str. Go's ParseUint accepts no
		// sign at all, so these were dropped here while applying in shelld-rs —
		// the divergence this table now pins on both sides of the '+'.
		{`{"palette":{"+7":"ff0000"}}`, 200, `{"ok":true}`},
		{`{"palette":{"+255":"000000"}}`, 200, `{"ok":true}`},
		// One entry survives, so the theme is applied and the bad entries are dropped.
		{`{"background":"nope","palette":{"0":"000000","1":"zzz","256":"000000"}}`, 200, `{"ok":true}`},

		// Not a colours request: a field of the wrong type is a client that is
		// sending something other than what this route reads.
		{`{"background":1}`, 400, `{"error":"colors required"}`},
		{`{"palette":null}`, 400, `{"error":"colors required"}`},
		{`{"palette":[]}`, 400, `{"error":"colors required"}`},
		{`{"palette":{"1":2}}`, 400, `{"error":"colors required"}`},

		// A colours request in which nothing parsed.
		{`{}`, 400, `{"error":"no valid colors provided"}`},
		{`{"background":null}`, 400, `{"error":"no valid colors provided"}`},
		{`{"background":"nope"}`, 400, `{"error":"no valid colors provided"}`},
		{`{"palette":{}}`, 400, `{"error":"no valid colors provided"}`},
		{`{"palette":{"256":"000000"}}`, 400, `{"error":"no valid colors provided"}`},
		{`{"palette":{"-1":"000000"}}`, 400, `{"error":"no valid colors provided"}`},
		{`{"palette":{"one":"000000"}}`, 400, `{"error":"no valid colors provided"}`},
		// The other side of the '+' allowance, and what keeps it from being "strip
		// any sign". from_str takes exactly one '+' and nothing else, so a second
		// one, a bare sign, and a '-' before a zero are all rejected — and the 255
		// bound still applies to what the trim leaves behind.
		{`{"palette":{"++7":"000000"}}`, 400, `{"error":"no valid colors provided"}`},
		{`{"palette":{"+":"000000"}}`, 400, `{"error":"no valid colors provided"}`},
		{`{"palette":{"-0":"000000"}}`, 400, `{"error":"no valid colors provided"}`},
		{`{"palette":{"+256":"000000"}}`, 400, `{"error":"no valid colors provided"}`},
	} {
		st, body := call(t, h, "POST", path, tc.body)
		if st != tc.wantStatus || body != tc.wantBody {
			t.Errorf("POST %s: %d %q, want %d %s", tc.body, st, body, tc.wantStatus, tc.wantBody)
		}
	}
}

// TestParseHexRGBMatchesRustsRadixParsing is a parity test, and the only reason
// to write it is that the two daemons must agree. A colour this one accepts and
// the other rejects is a theme that renders in one and errors in the other from
// the same client and the same config file; the reverse is a colour silently
// dropped. The accepted oddities ('+' before a channel, mixed case) are not
// anyone's intent — they are what u8::from_str_radix accepts, so they are the
// contract.
//
// Two of parseHexRGB's guards are belt-and-braces that no input can reach, and
// this table deliberately does not pretend otherwise: `pair == "+"` cannot fire
// because len(h) == 6 makes every pair exactly two bytes, and the explicit
// `pair[0] == '-'` rejection is already what ParseUint does with any sign.
// Deleting either one leaves behaviour unchanged on every input, verified by
// mutation: both survive this table by construction, not for want of a case.
func TestParseHexRGBMatchesRustsRadixParsing(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string // "" for rejected
	}{
		{"#1e1e2e", "30 30 46"},
		{"1e1e2e", "30 30 46"},
		{"FFAA00", "255 170 0"},
		{"ffAA00", "255 170 0"},
		{"000000", "0 0 0"},
		{"#000000", "0 0 0"},
		{"+1+2+3", "1 2 3"}, // from_str_radix takes one leading '+' per channel
		{"+f+f+f", "15 15 15"},

		{"", ""},
		{"#", ""},
		{"12345", ""},   // five digits
		{"1234567", ""}, // seven
		{"#12345", ""},
		{"1e1e2g", ""},
		{"0x0102", ""},
		{"12 456", ""},
		{"++++++", ""},
		{"-10000", ""}, // a '-' leads a channel: rejected before from_str_radix,
		{"00-100", ""}, // which would read "-1" as a negative and wrap
		{"1-1-1-", ""},
		{"#1e1e2e ", ""},
		{" 1e1e2e", ""},
	} {
		rgb, ok := parseHexRGB(tc.in)
		got := ""
		if ok {
			got = fmt.Sprintf("%d %d %d", rgb.R, rgb.G, rgb.B)
		}
		if got != tc.want {
			switch {
			case tc.want == "":
				t.Errorf("parseHexRGB(%q) = %s, want it rejected", tc.in, got)
			default:
				t.Errorf("parseHexRGB(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	}
}

// TestInjectQueueBoundIsAnHTTPStatus covers the boundary itself. The queue is
// what stops a producer that injects faster than the window consumes from
// growing the daemon's memory without limit, and the only way a producer learns
// it has reached that point is this status code. 202 at the last slot and 429 at
// the next one is the difference between a client that backs off and a client
// that believes every message it sent is queued.
//
// position and queueLength are asserted because a producer uses them to decide
// whether to keep sending; both are the length after the enqueue, so the
// message just accepted is counted.
func TestInjectQueueBoundIsAnHTTPStatus(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})
	const path = "/sessions/s/windows/w/inject"
	// The window's queue holds shelld.DefaultInjectConfig().MaxQueueSize; a fake
	// window runs no delivery goroutine, so nothing drains it underneath us.
	limit := shelld.DefaultInjectConfig().MaxQueueSize
	for i := 1; i <= limit; i++ {
		st, body := call(t, h, "POST", path, fmt.Sprintf(`{"payload":"m%d"}`, i))
		var v struct {
			ID          string `json:"id"`
			Position    int    `json:"position"`
			QueueLength int    `json:"queueLength"`
		}
		_ = json.Unmarshal([]byte(body), &v)
		if st != 202 || v.Position != i || v.QueueLength != i || v.ID == "" {
			t.Fatalf("inject %d of %d: %d %q, want 202 at position %d", i, limit, st, body, i)
		}
	}
	if st, body := call(t, h, "POST", path, `{"payload":"one too many"}`); st != 429 || body != `{"error":"queue full"}` {
		t.Errorf("inject past the queue's size: %d %q, want 429 queue full", st, body)
	}
	// The refusal did not consume a slot, and the queue is readable.
	st, body := call(t, h, "GET", "/sessions/s/windows/w/inject/queue", "")
	var q struct {
		Length  int `json:"length"`
		Pending []struct {
			ID      string `json:"id"`
			Payload string `json:"payload"`
		} `json:"pending"`
	}
	_ = json.Unmarshal([]byte(body), &q)
	if st != 200 || q.Length != limit || len(q.Pending) != limit {
		t.Fatalf("queue after a refusal: %d length %d, want 200 and %d", st, q.Length, limit)
	}
	if q.Pending[0].Payload != "m1" || q.Pending[limit-1].Payload != fmt.Sprintf("m%d", limit) {
		t.Errorf("queue holds %q first and %q last, want m1 and m%d",
			q.Pending[0].Payload, q.Pending[limit-1].Payload, limit)
	}
}

// TestMoveWindowSeparatesABadRequestFromARefusal. 400 and 409 are the client's
// decision about whether to retry elsewhere: 400 says the request was malformed
// and no move is possible, 409 says the move was understood and refused, which a
// client answers by choosing another destination name. MoveWindow discovers the
// collision before it detaches the window from the source, so a refusal has to
// leave the window reachable under its original name; the last check pins that
// order and reds if it ever goes back to detach-then-check, which would strand
// the window under neither name.
func TestMoveWindowSeparatesABadRequestFromARefusal(t *testing.T) {
	_, h := api(t, map[string][]string{"src": {"w"}, "dst": {"taken"}})
	const path = "/move-window"
	const required = `{"error":"srcSession, srcName, dstSession required"}`
	for _, body := range []string{
		`{}`,
		`{"srcSession":"src"}`,
		`{"srcSession":"src","srcName":"w"}`,
		`{"srcSession":"","srcName":"w","dstSession":"dst"}`,
		`{"srcSession":"src","srcName":"","dstSession":"dst"}`,
		`{"srcSession":"src","srcName":"w","dstSession":""}`,
		`{"srcSession":"src","srcName":"w","dstSession":"dst","dstName":null}`,
		`{"srcSession":"src","srcName":"w","dstSession":"dst","dstName":7}`,
	} {
		if st, got := call(t, h, "POST", path, body); st != 400 || got != required {
			t.Errorf("POST %s: %d %q, want 400 %s", body, st, got, required)
		}
	}
	for _, tc := range []struct{ body, want string }{
		{`{"srcSession":"nope","srcName":"w","dstSession":"dst"}`,
			`{"error":"source session \"nope\" not found"}`},
		{`{"srcSession":"src","srcName":"nope","dstSession":"dst"}`,
			`{"error":"window \"nope\" not found in session \"src\""}`},
		{`{"srcSession":"src","srcName":"w","dstSession":"dst","dstName":"taken"}`,
			`{"error":"window \"taken\" already exists in destination session \"dst\""}`},
	} {
		if st, got := call(t, h, "POST", path, tc.body); st != 409 || got != tc.want {
			t.Errorf("POST %s: %d %q, want 409 %s", tc.body, st, got, tc.want)
		}
	}
	// A refused move left the window where it was, under the name it had.
	if st, body := call(t, h, "GET", "/sessions/src/windows/w/frame", ""); st != 200 {
		t.Errorf("the source window after a refused move: %d %q, want 200", st, body)
	}
	// And a move that is accepted moves it.
	if st, body := call(t, h, "POST", path, `{"srcSession":"src","srcName":"w","dstSession":"dst","dstName":"moved"}`); st != 200 {
		t.Fatalf("move: %d %q", st, body)
	}
	if st, _ := call(t, h, "GET", "/sessions/dst/windows/moved/frame", ""); st != 200 {
		t.Errorf("the window after the move: %d, want 200 under its new name", st)
	}
	if st, _ := call(t, h, "GET", "/sessions/src/windows/w/frame", ""); st != 404 {
		t.Errorf("the old name after the move: %d, want 404", st)
	}
}

// TestAttachAnswersBeforeItUpgrades. Everything attach can refuse, it has to
// refuse while the connection is still HTTP: after the 101 there is no status
// code left to send, and a client that has switched protocols can only see the
// socket close — indistinguishable from the daemon dying, from the window
// exiting, and from a network fault. So a client that asked to attach to a
// window that does not exist has to be told 404, not disconnected.
//
// The handshake messages are axum's, word for word, because a client library
// written against shelld-rs reports them to its user.
func TestAttachAnswersBeforeItUpgrades(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})
	const path = "/sessions/s/windows/w/attach"
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"no handshake at all", nil, "Connection header did not include 'upgrade'"},
		{"connection only", map[string]string{
			"Connection": "upgrade",
		}, "`Upgrade` header did not include 'websocket'"},
		{"no version", map[string]string{
			"Connection": "upgrade", "Upgrade": "websocket",
		}, "`Sec-WebSocket-Version` header did not include '13'"},
		{"wrong version", map[string]string{
			"Connection": "upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "8",
		}, "`Sec-WebSocket-Version` header did not include '13'"},
		{"no key", map[string]string{
			"Connection": "upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13",
		}, "`Sec-WebSocket-Key` header missing"},
	} {
		r := httptest.NewRequest("GET", path, nil)
		for k, v := range tc.headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 || w.Body.String() != tc.want {
			t.Errorf("%s: %d %q, want 400 %q", tc.name, w.Code, w.Body.String(), tc.want)
		}
	}

	// A complete handshake for a window that is not there is a 404 with the same
	// body every other route uses, not a closed connection.
	r := httptest.NewRequest("GET", "/sessions/s/windows/nope/attach", nil)
	setUpgradeHeaders(r)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 404 || w.Body.String() != `{"error":"window not found"}` {
		t.Errorf("attach to an unknown window: %d %q, want 404", w.Code, w.Body.String())
	}

	// Both wrong at once: the handshake is answered first. The order is what a
	// client can act on — a library with a broken handshake fails against every
	// window and every session, and being told "window not found" sends its
	// author looking for a window that is in fact there.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/sessions/s/windows/nope/attach", nil))
	if w.Code != 400 || w.Body.String() != "Connection header did not include 'upgrade'" {
		t.Errorf("no handshake and no such window: %d %q, want the handshake error", w.Code, w.Body.String())
	}

	// A handshake that cannot be completed — here because the recorder cannot be
	// hijacked, in production because the connection went away between the
	// request and the response — writes nothing at all. There is no status that
	// means "I accepted your upgrade and then could not": sending one after the
	// client has stopped speaking HTTP would leave bytes in the stream that a
	// WebSocket client would read as a frame.
	r = httptest.NewRequest("GET", path, nil)
	setUpgradeHeaders(r)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if body := w.Body.String(); body != "" {
		t.Errorf("a failed upgrade wrote %q, want nothing", body)
	}
}

// TestMaxControlBodyMatchesTheReferenceLimit pins the constant to its value rather
// than to the reasoning that produced it, because the reasoning lives in two places
// this test cannot reach.
//
// The binding reason is parity: the reference daemon extracts every body through
// axum and never overrides DefaultBodyLimit, whose default is this many bytes
// (axum-core 0.4.5, ext_traits/request.rs: `const DEFAULT_LIMIT: usize =
// 2_097_152`). A smaller value here refuses a body the reference accepts, which is a
// divergence no other test in this package would show, since the routes' own
// assertions are all written relative to this constant.
//
// Corroborating it, ARG_MAX on Linux is the same number, so a control body larger
// than this cannot describe a window the daemon could actually exec. That one is
// deliberately not read from the host at run time: it would make this test's verdict
// depend on the machine, when the number that matters is fixed by the reference.
func TestMaxControlBodyMatchesTheReferenceLimit(t *testing.T) {
	if maxControlBody != 2097152 {
		t.Errorf("maxControlBody = %d, want 2097152 -- axum's DefaultBodyLimit, which the reference leaves at its default; a different value here diverges from the reference on every configuration route", maxControlBody)
	}
}

// TestEveryConfigurationRouteBoundsItsBody. The input routes have been bounded
// since they were written; these five read their body through the same decoder
// with no bound at all, so a local client could make the daemon allocate whatever
// it chose to send. A 3MB POST /sessions was answered 200, and the session it
// created was named by 3MB of the client's bytes.
//
// Each route is called three times with the same body, padded differently. The
// unpadded call establishes what that route answers in this fixture, and the other
// two must differ from it only when the bound is crossed:
//
//   - padded to exactly maxControlBody: must answer the same as unpadded, so the
//     bound is not somewhere below the constant, and padding by itself does not
//     change the answer
//   - padded to maxControlBody+1: must be 413
//
// Without the middle call the test would pass against a bound set anywhere at or
// under the limit -- including one so small it rejected ordinary bodies -- and
// without the first it would not show that these routes still work.
//
// Both padded sizes are computed from maxControlBody, so this pins the bound to
// that constant but says nothing about the constant's own value: moving it to 1MB
// or 4MB keeps every assertion here true. What the value has to be is a separate
// claim, asserted separately in TestMaxControlBodyMatchesTheReferenceLimit.
//
// Every body names something the fixture already has, which makes each route's own
// answer repeatable rather than uniform: 409 for the two create routes, which
// refuse a duplicate, and 200 for resize, setColors and moveWindow. Resize and
// setColors get their 200 by acting on the window that is there. moveWindow's row
// names one session for both ends and no dstName, which is the no-op -- it touches
// nothing, and its 200 depends on the window existing rather than being produced by
// anything done to it.
//
// Repeatable is the property the test needs -- a created-then-duplicate sequence
// would answer differently on the second call, and the claim here is only that the
// status does not move with the padding. Which of the two answers a row gets is not
// asserted, and no row's status is compared to any other row's.
//
// The padding goes in a field no route reads. A route's own fields cannot carry it:
// a 2MB hex colour or a 2MB row count is rejected for being malformed long before
// its size matters, which would test the parser rather than the bound.
func TestEveryConfigurationRouteBoundsItsBody(t *testing.T) {
	const want = `{"error":"request body too large"}`
	// pad returns fields with a "pad" entry bringing the whole body to n bytes.
	pad := func(fields string, n int) string {
		body := `{` + fields + `,"pad":""}`
		return `{` + fields + `,"pad":"` + strings.Repeat("x", n-len(body)) + `"}`
	}
	for _, tc := range []struct{ name, method, path, fields string }{
		{"createSession", "POST", "/sessions", `"name":"s"`},
		{"createWindow", "POST", "/sessions/s/windows", `"name":"w"`},
		{"resize", "POST", "/sessions/s/windows/w/resize", `"cols":80,"rows":24`},
		{"setColors", "POST", "/sessions/s/windows/w/colors", `"background":"#112233"`},
		{"moveWindow", "POST", "/move-window", `"srcSession":"s","srcName":"w","dstSession":"s"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, h := api(t, map[string][]string{"s": {"w"}})
			base, body := call(t, h, tc.method, tc.path, `{`+tc.fields+`}`)
			if base == 413 {
				t.Fatalf("unpadded body: %d %q, want anything but 413 -- an ordinary body must not be refused for its size", base, body)
			}
			if st, got := call(t, h, tc.method, tc.path, pad(tc.fields, maxControlBody)); st != base {
				t.Errorf("body of exactly maxControlBody (%d): %d %q, want %d as unpadded -- the bound must be at that byte, and padding alone must not change the answer", maxControlBody, st, got, base)
			}
			if st, got := call(t, h, tc.method, tc.path, pad(tc.fields, maxControlBody+1)); st != 413 || got != want {
				t.Errorf("body of maxControlBody+1 (%d): %d %q, want 413 %s", maxControlBody+1, st, got, want)
			}
		})
	}
}

// TestInputLimitFallsBackWhenUnset. MaxInputBody is a bound on a request body,
// and State is a struct a caller fills in — the daemon's own tests do, and so
// would an embedder. Zero is what an unset field holds, and read literally it is
// a limit of zero bytes: every input request 413, on a daemon whose
// configuration says nothing about limits. A limit has to be absent or a number,
// never accidentally the strictest possible value.
func TestInputLimitFallsBackWhenUnset(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		st, h := api(t, map[string][]string{"s": {"w"}})
		st.MaxInputBody = limit
		// 500, not 413: the body was read and the request reached the window.
		code, body := call(t, h, "POST", "/sessions/s/windows/w/keys", `{"keys":"x"}`)
		if code != 500 {
			t.Errorf("MaxInputBody %d: %d %q, want the body to be read (500 from the dead window)", limit, code, body)
		}
	}
	// And a limit that is set is applied.
	st, h := api(t, map[string][]string{"s": {"w"}})
	st.MaxInputBody = 16
	code, body := call(t, h, "POST", "/sessions/s/windows/w/keys", `{"keys":"`+strings.Repeat("x", 64)+`"}`)
	if code != 413 || body != `{"error":"request body too large"}` {
		t.Errorf("MaxInputBody 16 with a 70-byte body: %d %q, want 413", code, body)
	}
}

// The same two properties for paste, which reads its body through inputBodyInto
// rather than inputBody. They are asserted separately because that is a second
// implementation of "an input route is bounded", and the test above cannot see
// it: every assertion there runs over /keys, so readBoundedInto could omit its
// MaxBytesReader altogether and the suite would stay green. Paste is also the
// route where an unbounded read would cost the most, since it is the one whose
// body is meant to be large.
func TestPasteIsBoundedByTheInputLimit(t *testing.T) {
	const tooLarge = `{"error":"request body too large"}`

	st, h := api(t, map[string][]string{"s": {"w"}})
	st.MaxInputBody = 16
	code, body := call(t, h, "POST", "/sessions/s/windows/w/paste", `{"text":"`+strings.Repeat("x", 64)+`"}`)
	if code != 413 || body != tooLarge {
		t.Errorf("MaxInputBody 16 with a 74-byte paste: %d %q, want 413 %s", code, body, tooLarge)
	}

	// An unset limit falls back rather than reading as zero bytes; 500 because
	// the body was read and reached a window with no writer.
	for _, limit := range []int64{0, -1} {
		st, h := api(t, map[string][]string{"s": {"w"}})
		st.MaxInputBody = limit
		code, body := call(t, h, "POST", "/sessions/s/windows/w/paste", `{"text":"x"}`)
		if code != 500 {
			t.Errorf("MaxInputBody %d: paste answered %d %q, want the body to be read (500 from the dead window)", limit, code, body)
		}
	}
}

// TestEnvBytesFallsBackToItsDefault. These variables bound how much input the
// daemon will hold, and they are set by whatever launches it — a shell profile,
// a container spec, a CI file. Read literally, SHELLD_INPUT_BUDGET=0 is a budget
// of nothing and every keystroke is refused; a value with a typo in it is the
// same. A misconfigured bound has to leave the daemon working with its default,
// because the alternative is a daemon that starts, serves, and refuses all
// input, which looks like a bug in the daemon rather than in the environment.
func TestEnvBytesFallsBackToItsDefault(t *testing.T) {
	const def = int64(4242)
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"", def}, // unset
		{"0", def},
		{"-1", def},
		{"abc", def},
		{" 100", def},
		{"100 ", def},
		{"1_000", def},
		{"1e6", def},
		{"9223372036854775808", def}, // one past int64
		{"1", 1},
		{"65536", 65536},
		{"9223372036854775807", 1<<63 - 1},
	} {
		// t.Setenv first, so its cleanup restores whatever the host had.
		t.Setenv("SHELLD_TEST_BYTES", tc.value)
		if tc.value == "" {
			if err := os.Unsetenv("SHELLD_TEST_BYTES"); err != nil {
				t.Fatal(err)
			}
		}
		if got := envBytes("SHELLD_TEST_BYTES", def); got != tc.want {
			t.Errorf("envBytes(%q) = %d, want %d", tc.value, got, tc.want)
		}
	}
}

// TestParseEnvAcceptsAnObject. Both forms are part of the API, and the object
// form is the one a JSON-speaking client naturally sends. It works only because
// the array decode is attempted first and fails: reverse the two, or make the
// array decode tolerant, and an object body silently contributes no variables at
// all — the window starts, the client sees 200, and the program runs without the
// environment it was configured with. That failure is invisible at the API, and
// what it breaks (a PATH, a token, a TERM) surfaces much later as the program
// misbehaving.
//
// Key order is asserted because the object form is documented to apply in sorted
// order, matching serde_json's BTreeMap on the other daemon: a duplicate name
// across the two forms has to resolve the same way in both.
func TestParseEnvAcceptsAnObject(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{``, ""},
		{`null`, ""},
		{`[]`, ""},
		{`{}`, ""},
		{`["A=1","B=2"]`, "A=1 B=2"},
		{`["B=2","A=1"]`, "B=2 A=1"}, // an array applies in the order it was sent
		{`["A=1=2"]`, "A=1=2"},       // only the first '=' separates
		{`["A="]`, "A="},
		{`["noequals","A=1"]`, "A=1"}, // an entry that is not KEY=VALUE is dropped
		{`[1,"A=1",null]`, "A=1"},
		// An object applies in sorted key order, whatever order it was written in.
		// Four keys rather than two: with two, an implementation that iterated the
		// map directly would pass half the time.
		{`{"D":"4","B":"2","C":"3","A":"1"}`, "A=1 B=2 C=3 D=4"},
		{`{"A":1,"B":"2"}`, "B=2"}, // a non-string value is dropped
		{`{"A":null}`, ""},
		{`"A=1"`, ""}, // neither form
		{`42`, ""},
		{`{`, ""},
	} {
		var parts []string
		for _, kv := range parseEnv([]byte(tc.raw)) {
			parts = append(parts, kv[0]+"="+kv[1])
		}
		if got := strings.Join(parts, " "); got != tc.want {
			t.Errorf("parseEnv(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestHeadIsAnswered. A HEAD on a GET route is what a liveness probe sends — the
// daemon's own health check does not need the body — and the router answers it
// with the GET handler rather than 405. A probe that got 405 would report the
// daemon as unhealthy while it was serving normally.
//
// The second case is a request line with no path at all (an absolute-form URI
// with an empty path, which a proxy can send). The router splits the path after
// its leading slash, so a path that has none has to be rejected before the slice
// — it is a 404 either way, but one of the two ways is a panic.
func TestHeadIsAnswered(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})
	if st, _ := call(t, h, "HEAD", "/health", ""); st != 200 {
		t.Errorf("HEAD /health: %d, want 200", st)
	}
	if st, _ := call(t, h, "HEAD", "/sessions/s/windows/w/frame", ""); st != 200 {
		t.Errorf("HEAD a window route: %d, want 200", st)
	}
	// POST /shutdown exists, so this is a route with no GET to fall back to.
	if st, _ := call(t, h, "HEAD", "/shutdown", ""); st != 405 {
		t.Errorf("HEAD /shutdown: %d, want 405", st)
	}

	r := httptest.NewRequest("GET", "/health", nil)
	r.URL = &url.URL{Path: ""}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Errorf("a request with no path: %d, want 404", w.Code)
	}
}
