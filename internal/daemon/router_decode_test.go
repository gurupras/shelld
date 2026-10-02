package daemon

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The paste route decodes into a struct rather than a body map, so that a 256 KB
// text is not copied into a json.RawMessage and then scanned again on the way
// out (see decodeInto). Every way the two paths could disagree is a body this
// daemon rejects today and would start accepting, which is the direction a test
// is the only thing guarding.
//
// So this runs both paths over the same bodies and compares them to each other,
// rather than comparing the struct path against a column of expected values: a
// hand-written table can only encode what I already believed the map path did,
// and it is the map path's actual behaviour that is the contract. The bodies
// below are chosen to hit each place encoding/json's struct decoding is laxer
// than the accessors — null into a required field above all, which a plain
// string field takes as a no-op and leaves as "" with a nil error.
func TestPasteStructDecodeMatchesTheBodyMap(t *testing.T) {
	bodies := []string{
		// Accepted shapes.
		`{"text":"x"}`,
		`{"text":""}`,
		`{"text":"x","mode":"raw"}`,
		`{"text":"x","mode":""}`,
		`{"text":"x","mode":null}`,
		`{"text":"x","literal":true}`,
		`{"text":"x","literal":false}`,
		`{"text":"x","literal":null}`,
		`{"text":"x","mode":"bracketed","literal":true}`,

		// A required field that is absent, null, or the wrong type. The null case
		// is the one that motivates *string.
		`{}`,
		`{"text":null}`,
		`{"text":42}`,
		`{"text":true}`,
		`{"text":["x"]}`,
		`{"text":{"a":1}}`,

		// Optional fields of the wrong type. literal is compared against the two
		// literals, so nothing merely truthy may pass.
		`{"text":"x","mode":42}`,
		`{"text":"x","mode":["raw"]}`,
		`{"text":"x","literal":1}`,
		`{"text":"x","literal":0}`,
		`{"text":"x","literal":"true"}`,
		`{"text":"x","literal":[]}`,

		// Unknown fields are ignored and a duplicate key keeps the last value,
		// for a struct as for a map. Neither is a behaviour to change quietly
		// while changing how the body is read.
		`{"text":"x","nosuchfield":1}`,
		`{"nosuchfield":1,"text":"x"}`,
		`{"text":"a","text":"b"}`,
		`{"mode":"raw","text":"x","mode":"auto"}`,

		// Keys match exactly, as they do in the map (H27). encoding/json folds case
		// into a struct field, so each of these reached a field it does not name;
		// the map ignores them as unknown, as shelld-rs's serde struct does.
		`{"Text":"hi"}`,
		`{"TEXT":"x"}`,
		`{"text":"a","Text":"b"}`,
		`{"Text":"b","text":"a"}`,
		`{"text":"x","Mode":"bogus"}`,
		`{"text":"x","MODE":42}`,
		`{"text":"x","Literal":true}`,
		`{"text":"x","LITERAL":1}`,
		`{"\u0054ext":"x"}`,
		`{"\u0074ext":"x"}`,
		`{"Text":"b","text":"a","Mode":1,"mode":"raw"}`,
		`{"Text":{"nested":["Text"]},"text":"a"}`,

		// Not a JSON object, or more than one.
		`null`,
		`[]`,
		`["text"]`,
		`42`,
		`"text"`,
		``,
		`   `,
		`{"text":"x"}{"text":"y"}`,
		`{"text":"x"} `,
		`{"text":"x"} null`,
		`{`,
		`{"text":"x"`,

		// Escapes have to come out of the two paths identically, since only one
		// of them unescapes twice. The lone surrogate is the interesting one:
		// both decode strings through encoding/json, so both must produce its
		// U+FFFD substitution rather than one preserving the bytes.
		`{"text":"a\nb"}`,
		`{"text":"a\\b"}`,
		`{"text":"a\"b"}`,
		`{"text":"é"}`,
		`{"text":"😀"}`,
		`{"text":"\ud800"}`,
		`{"text":"\u0000"}`,
		`{"text":"tab\there"}`,
	}
	for _, in := range bodies {
		mOK, mText, mMode, mLit := pasteViaBodyMap(decodeRequest(in))
		sOK, sText, sMode, sLit := pasteViaStruct(decodeRequest(in))
		if mOK != sOK {
			// The failure that matters reads sOK=true, mOK=false: the struct path
			// accepting a body the map path rejected.
			t.Errorf("body %s: map accepted=%v, struct accepted=%v", in, mOK, sOK)
			continue
		}
		if mText != sText || mMode != sMode || mLit != sLit {
			t.Errorf("body %s: map gave (%q, %q, %v), struct gave (%q, %q, %v)",
				in, mText, mMode, mLit, sText, sMode, sLit)
		}
	}
}

// A body at the size the struct path exists for must still decode to exactly the
// bytes the caller sent. The escape is in there because an unescaping bug is
// invisible at 3 bytes of ASCII and this is the only test carrying a text long
// enough for a second pass to be worth avoiding.
func TestPasteStructDecodeCarriesALargeText(t *testing.T) {
	const size = 256 << 10
	want := strings.Repeat("line of pasted text\n", size/20)
	in := `{"text":"` + strings.ReplaceAll(want, "\n", `\n`) + `"}`

	ok, text, _, _ := pasteViaStruct(decodeRequest(in))
	if !ok {
		t.Fatalf("a %d-byte text was rejected", len(want))
	}
	if text != want {
		t.Errorf("text round-tripped wrong: got %d bytes, want %d", len(text), len(want))
	}
	if mOK, mText, _, _ := pasteViaBodyMap(decodeRequest(in)); !mOK || mText != text {
		t.Errorf("map path disagreed at %d bytes: ok=%v, equal=%v", len(want), mOK, mText == text)
	}
}

// BenchmarkPasteDecode is the reason the struct path exists, so the saving is
// measured here rather than asserted in a commit message. The two sub-benchmarks
// decode the same body: "map" holds each field as a json.RawMessage and
// unmarshals text on access, "struct" decodes it once out of the reader.
//
// Sizes span the range to locate the saving, and the measured answer is not the
// one this comment first predicted. The struct path wins by a near-constant ~1.5x
// at all three sizes, with the alloc delta exactly 9 at each, so the win is not
// large-body-only: nine allocations and the map itself are charged per request
// regardless of size, and the second unescaping pass adds to that in proportion.
// What changes with size is only the absolute saving -- about 1 us at 64 B against
// about 0.7 ms at 256 KB.
//
// So /keys stays on the map path on magnitude, not because the paths are
// equivalent there: 1 us per keystroke is four orders of magnitude below the
// frame interval, and the conversion's cost is a real risk of accepting a body
// this daemon rejects today (see the differential test above).
//
// The request is built once per sub-benchmark and only its Body is replaced per
// iteration. httptest.NewRequest costs about 4 us and ~7 KB, which is four times
// the work of decoding the 64 B body: leaving it inside the timed region put a
// large fixed cost in both arms, which cancels in the difference but deflates the
// ratio and makes the small sizes unreadable. A control belongs outside the
// interval it is controlling for, or it is not a control.
//
// The loop is the classic form on purpose: go1.26.1 ICEs on some b.Loop() bodies
// in this tree, and a benchmark that cannot be compiled measures nothing.
func BenchmarkPasteDecode(b *testing.B) {
	for _, size := range []int{64, 4 << 10, 256 << 10} {
		// Text with escapes in it, since a body of plain ASCII lets the second
		// pass skip the work the first pass already did.
		text := strings.Repeat("line of pasted text\n", size/20+1)
		body := `{"text":"` + strings.ReplaceAll(text, "\n", `\n`) + `","mode":"auto","literal":false}`

		run := func(name string, decode func(*http.Request) (bool, string, string, bool)) {
			b.Run(fmt.Sprintf("%s/%d", name, size), func(b *testing.B) {
				r := decodeRequest("")
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				b.ResetTimer()
				// Content-Length as well as Body: a real request carries one, and
				// decodeInto sizes its read from it, so leaving the request's
				// stale zero here would measure the chunked path on every row.
				r.ContentLength = int64(len(body))
				for range b.N {
					r.Body = io.NopCloser(strings.NewReader(body))
					if ok, _, _, _ := decode(r); !ok {
						b.Fatal("body rejected")
					}
				}
			})
		}
		run("map", pasteViaBodyMap)
		run("struct", pasteViaStruct)
	}
}

func decodeRequest(body string) *http.Request {
	r := httptest.NewRequest("POST", "/paste", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// pasteViaBodyMap is what paste did before decodeInto: a body map and its typed
// accessors. Kept here as the reference the struct path is compared against.
// Both helpers return zero values on rejection so a rejected body cannot appear
// to differ in a field neither path would have used.
func pasteViaBodyMap(r *http.Request) (ok bool, text, mode string, literal bool) {
	b, err := decodeBody(r)
	if err != nil {
		return false, "", "", false
	}
	text, v1 := b.str("text", true)
	mode, _, v2 := b.optStr("mode")
	literal, _, v3 := b.optBool("literal")
	if !v1 || !v2 || !v3 {
		return false, "", "", false
	}
	return true, text, mode, literal
}

// pasteViaStruct mirrors the paste handler's own reads.
func pasteViaStruct(r *http.Request) (ok bool, text, mode string, literal bool) {
	var req pasteReq
	if err := decodeInto(r, &req); err != nil {
		return false, "", "", false
	}
	mode, _, v2 := rawStr(req.Mode)
	literal, _, v3 := rawBool(req.Literal)
	if req.Text == nil || !v2 || !v3 {
		return false, "", "", false
	}
	return true, *req.Text, mode, literal
}
