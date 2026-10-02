package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The two messages bodyUnfaithful writes, as a client sees them, and the phrase
// they share. A row asserting only the shared phrase would pass with the two
// swapped, which is the whole point of having two.
const (
	notUTF8Msg    = "byte that is not valid UTF-8"
	surrogateMsg  = "half a surrogate pair"
	unfaithfulMsg = "would be delivered as U+FFFD"
)

// TestBodyCarryingAStringJSONCannotHoldIsRefused holds every route that takes a
// string to refusing a body whose string would arrive as something else.
//
// encoding/json does not refuse a string it cannot represent. It substitutes
// U+FFFD, returns no error, and the route then does exactly what it was asked
// with bytes the caller did not send: before this, POST .../keys with a raw 0xff
// answered 200 and wrote ef bf bd to the pty, and "literal": true on a paste did
// not help, because the substitution happens in the decode, upstream of anything
// the paste options reach.
//
// Each route is checked on the field that carries its payload, and createSession
// on a name, because the refusal is a property of the body rather than of the
// field: serde_json, which the reference decodes with, fails while parsing the
// string and never reaches the question of which field it belonged to. A row for
// a field no route reads pins that, since it is the case where the two could
// plausibly have diverged.
//
// The accept rows assert that the body was not refused for its bytes, rather than
// asserting a status. They cannot compare against the control's exact answer,
// because some of these routes carry state: inject returns a new queue position
// each time, and createSession answers 409 the second time a name arrives. That
// second one is incidental evidence the check is not too broad -- an escaped
// U+FFFD and the three bytes of U+FFFD collide on the same session name, which
// they only can if both decoded to the same string.
func TestBodyCarryingAStringJSONCannotHoldIsRefused(t *testing.T) {
	for _, route := range []struct {
		name, path, field string
	}{
		{"keys", "/sessions/s/windows/w/keys", "keys"},
		{"raw-key", "/sessions/s/windows/w/raw-key", "key"},
		{"paste", "/sessions/s/windows/w/paste", "text"},
		{"inject", "/sessions/s/windows/w/inject", "payload"},
		{"createSession", "/sessions", "name"},
	} {
		t.Run(route.name, func(t *testing.T) {
			_, h := api(t, map[string][]string{"s": {"w"}})
			body := func(content string) string {
				return `{"` + route.field + `":"` + content + `"}`
			}

			// The control: a plainly valid string. Whatever this route answers
			// for it is what every accept row below has to answer too.
			wantCode, wantBody := call(t, h, "POST", route.path, body("x"))
			if strings.Contains(wantBody, unfaithfulMsg) {
				t.Fatalf("the control body was refused for its bytes (%d %s): every accept row below would pass for the wrong reason", wantCode, wantBody)
			}

			for _, bad := range []struct{ name, content, want string }{
				// A byte that is not part of any valid UTF-8 sequence. This is
				// the case a caller hits by sending terminal bytes.
				{"a raw byte outside valid UTF-8", "A\xffB", notUTF8Msg},
				// Valid UTF-8 as far as it goes, then stops mid-sequence.
				{"a truncated sequence", "A\xc3", notUTF8Msg},
				// An overlong encoding of NUL: decodable by a lax decoder, and
				// refused by both this one and Rust's.
				{"an overlong encoding", "\xc0\x80", notUTF8Msg},
				// Valid UTF-8 in the body, so a check on the body's bytes alone
				// would pass this and the string would still be rewritten. This
				// is the arm a JavaScript client reaches by slicing text that
				// contains an emoji, so it is answered in those terms.
				{"a lone high surrogate escape", `A\ud800B`, surrogateMsg},
				{"a lone low surrogate escape", `A\udc00B`, surrogateMsg},
			} {
				t.Run(bad.name, func(t *testing.T) {
					code, got := call(t, h, "POST", route.path, body(bad.content))
					if code != http.StatusBadRequest || !strings.Contains(got, bad.want) {
						t.Errorf("%s: got %d %s, want 400 naming %q -- encoding/json substitutes U+FFFD here and reports no error, so accepting this writes bytes the caller did not send", bad.name, code, got, bad.want)
					}
					if !strings.Contains(got, unfaithfulMsg) {
						t.Errorf("%s: got %s, want it to say what the caller would get instead", bad.name, got)
					}
				})
			}

			// Strings that do contain U+FFFD, or look as though they might, and
			// must still be accepted: the refusal is about what a string cannot
			// carry, not about which code points appear in it. Without these a
			// check that refused every body would pass every row above.
			for _, good := range []struct{ name, content string }{
				{"an escaped surrogate pair", `A😀B`},
				{"an escaped U+FFFD", `A�B`},
				{"the bytes of U+FFFD", "A\xef\xbf\xbdB"},
				// `\\ud800` is an escaped backslash followed by the text ud800,
				// not a surrogate escape, so the scan has to consume escapes in
				// pairs rather than searching for the letters.
				{"an escaped backslash before ud800", `A\\ud800B`},
				{"an escaped non-ASCII code point", `café`},
				{"the bytes of a non-ASCII code point", "caf\xc3\xa9"},
			} {
				t.Run(good.name, func(t *testing.T) {
					code, got := call(t, h, "POST", route.path, body(good.content))
					if code == http.StatusBadRequest || strings.Contains(got, unfaithfulMsg) {
						t.Errorf("%s: got %d %s, want the route's own answer -- this string carries its bytes faithfully, so refusing it narrows what a caller may send without fixing anything", good.name, code, got)
					}
				})
			}
		})
	}
}

// TestABadByteInAFieldNoRouteReadsIsStillRefused pins that the refusal is a
// property of the body and not of the fields a route happens to look at.
//
// This is where a per-field check and a body check part company, so it is worth
// its own test: an unknown field is otherwise ignored, by this daemon and by
// serde alike, and it would be reasonable to expect a byte inside one to be
// ignored with it. It is not, because serde_json fails while parsing the string
// and never gets as far as deciding the field is unknown -- so refusing here is
// what keeps the two ports answering the same thing.
func TestABadByteInAFieldNoRouteReadsIsStillRefused(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})

	const path = "/sessions/s/windows/w/keys"
	wantCode, wantBody := call(t, h, "POST", path, `{"keys":"x","somethingElse":"plain"}`)
	if strings.Contains(wantBody, unfaithfulMsg) {
		t.Fatalf("the control was refused for its bytes (%d %s)", wantCode, wantBody)
	}

	code, got := call(t, h, "POST", path, "{\"keys\":\"x\",\"somethingElse\":\"A\xffB\"}")
	if code != http.StatusBadRequest || !strings.Contains(got, notUTF8Msg) {
		t.Errorf("a bad byte in an unread field: got %d %s, want 400 naming UTF-8 -- serde_json fails parsing the string before it decides the field is unknown, so ignoring it here would diverge from the reference", code, got)
	}
}

// TestAMalformedBodyIsReportedForItsSyntax pins the order of the two checks in
// decodeInto: a body that both fails to parse and carries an unholdable byte is
// reported for the parse failure.
//
// Both faults answer 400, so nothing here is about the status -- it is about the
// caller reading a message that names something they can act on. Someone whose
// JSON is truncated has to fix the truncation first, and being told about UTF-8
// would point them at a byte that is not their problem yet.
func TestAMalformedBodyIsReportedForItsSyntax(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})

	// Unterminated, and with a byte no JSON string can hold, so whichever check
	// runs first decides the message.
	code, got := call(t, h, "POST", "/sessions/s/windows/w/keys", "{\"keys\":\"A\xffB")
	if code != http.StatusBadRequest {
		t.Fatalf("got %d %s, want 400: the body is malformed on any reading", code, got)
	}
	if strings.Contains(got, unfaithfulMsg) {
		t.Errorf("got %s, want the syntax fault named instead -- the UTF-8 check runs last for exactly this body", got)
	}
}

// TestHasLoneSurrogateEscape covers the scan directly, because the cases that
// distinguish it from a substring search are awkward to reach through a route
// and easy to get wrong: a backslash that escapes another backslash, an escape
// cut short by the end of the body, and a pair split so that each half is well
// formed on its own.
func TestHasLoneSurrogateEscape(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"no escapes", `"hello"`, false},
		{"a non-surrogate escape", `"A"`, false},
		{"a complete pair", `"😀"`, false},
		{"a complete pair, upper case hex", `"😀"`, false},
		{"two complete pairs", `"😀😁"`, false},
		{"a high half alone", `"\ud800"`, true},
		{"a low half alone", `"\udc00"`, true},
		{"a high half followed by a plain escape", `"\ud800A"`, true},
		{"a high half followed by text", `"\ud800A"`, true},
		{"two high halves", `"\ud800\ud801"`, true},
		{"a low half before a high half", `"\udc00\ud800"`, true},
		// Two low halves in a row: the branch that completes a pair would take the
		// second as the other half of the first, if the low-half case did not return
		// before reaching it.
		{"two low halves", `"\udc00\udc01"`, true},
		// Each end of each half's range, because every one of the four bounds is an
		// off-by-one away from letting a half through or refusing a character that
		// is not one.
		{"the code point below the high halves", `"\ud7ff"`, false},
		{"the last high half alone", `"\udbff"`, true},
		{"the last low half alone", `"\udfff"`, true},
		{"the code point above the low halves", `"\ue000"`, false},
		{"the lowest pair", `"\ud800\udc00"`, false},
		{"the highest pair", `"\udbff\udfff"`, false},
		// The escaped backslash cases: the letters ud800 appear, and none of
		// them is an escape.
		{"an escaped backslash then ud800", `"\\ud800"`, false},
		{"two escaped backslashes then ud800", `"\\\\ud800"`, false},
		// Three backslashes: the first two are an escaped backslash, the third
		// starts a real escape, so this one is a lone half.
		{"an escaped backslash then a real escape", `"\\\ud800"`, true},
		// Malformed rather than lone: reported false, because the decode refuses
		// the body for its own reason and this would name the wrong fault.
		{"a truncated escape at the end", `"\ud80`, false},
		{"a non-hex escape", `"\uzzzz"`, false},
		{"a bare backslash at the end", `"\`, false},
		{"a high half with a truncated second escape", `"\ud800\ud`, true},
	} {
		if got := hasLoneSurrogateEscape([]byte(tc.in)); got != tc.want {
			t.Errorf("hasLoneSurrogateEscape(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestReadWholeReadsTheBodyWhateverContentLengthClaims covers the header-sized
// read, whose whole input is a number the client chose.
//
// Content-Length is only ever a hint here: the body is read to its end either
// way, so an understated or absent header costs a reallocation and nothing else.
// The case worth pinning is the overstated one. Sizing a buffer from an
// unchecked header hands a local client a way to make the daemon allocate
// whatever it is willing to type, so the hint is capped at the largest body any
// route accepts -- without that cap the last row below panics in make rather
// than returning a body.
func TestReadWholeReadsTheBodyWhateverContentLengthClaims(t *testing.T) {
	const payload = `{"keys":"hello"}`
	for _, tc := range []struct {
		name   string
		length int64
	}{
		{"the length the body actually is", int64(len(payload))},
		{"no length at all", -1},
		{"a length below the body's size", 4},
		{"a length above the body's size", 4096},
		{"a length no body could have", 1 << 62},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/x", strings.NewReader(payload))
			r.ContentLength = tc.length
			got, err := readWhole(r)
			if err != nil {
				t.Fatalf("readWhole: %v", err)
			}
			if string(got) != payload {
				t.Errorf("got %q, want %q -- the header is a hint about size, not about how much to read", got, payload)
			}
		})
	}
}
