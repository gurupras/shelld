// Package daemon is shelld's HTTP + WebSocket API over a Unix socket (named
// pipe on Windows). The wire format matches shelld-rs, so a client
// written for either daemon works with both.
package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gurupras/shelld/internal/tracing"
)

// Version is the protocol version reported by /health.
const Version = "1"

type params map[string]string

type handlerFunc func(w http.ResponseWriter, r *http.Request, p params)

type route struct {
	segments []string // literal, or ":name" for a parameter
	methods  map[string]handlerFunc
}

// router matches exact segment patterns. Unknown paths get 404 and known
// paths with the wrong method 405, both with empty bodies.
type router struct{ routes []*route }

func (rt *router) handle(method, pattern string, h handlerFunc) {
	segs := strings.Split(strings.Trim(pattern, "/"), "/")
	for _, r := range rt.routes {
		if strings.Join(r.segments, "/") == strings.Join(segs, "/") {
			r.methods[method] = h
			return
		}
	}
	rt.routes = append(rt.routes, &route{segments: segs, methods: map[string]handlerFunc{method: h}})
}

func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if tracing.Enabled {
		id := tracing.NewID()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		span := tracing.Begin(tracing.KindHTTP, id, int64(tracing.Intern(r.Method+" "+rt.pattern(r))))
		defer func() { tracing.End(tracing.KindHTTP, id, span, int64(sw.status), 0) }()
		w = sw
	}
	raw := r.URL.EscapedPath()
	if !strings.HasPrefix(raw, "/") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	parts := strings.Split(raw[1:], "/")
	for _, rte := range rt.routes {
		p, ok := match(rte.segments, parts)
		if !ok {
			continue
		}
		method := r.Method
		h := rte.methods[method]
		if h == nil && method == http.MethodHead {
			h = rte.methods[http.MethodGet]
		}
		if h == nil {
			allow := make([]string, 0, len(rte.methods)+1)
			for m := range rte.methods {
				allow = append(allow, m)
			}
			// HEAD is served wherever GET is, a few lines up, so a list built from the
			// registrations alone understates what the resource accepts.
			if _, ok := rte.methods[http.MethodGet]; ok {
				allow = append(allow, http.MethodHead)
			}
			// Sorted because ranging a map has no order: without this a route with
			// more than one method answers two identical requests with two different
			// header values, and nothing a client or a test reads from it is stable.
			slices.Sort(allow)
			w.Header().Set("Allow", strings.Join(allow, ","))
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		h(w, r, p)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// pattern names the route a request matches, for traces.
func (rt *router) pattern(r *http.Request) string {
	raw := r.URL.EscapedPath()
	if strings.HasPrefix(raw, "/") {
		parts := strings.Split(raw[1:], "/")
		for _, rte := range rt.routes {
			if _, ok := match(rte.segments, parts); ok {
				return "/" + strings.Join(rte.segments, "/")
			}
		}
	}
	return "(unmatched)"
}

// statusWriter records the response status for traces; it passes hijacking
// through for WebSocket upgrades.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Hijack passes a WebSocket upgrade through, and reports rather than panics when
// the writer below cannot be hijacked. The wrapper's own existence is what makes
// the check necessary: wsserver.Upgrade decides whether it can upgrade by asserting
// http.Hijacker on whatever it was handed, and this type has the method, so an
// unchecked assertion here turns "this response cannot be hijacked" — which
// Upgrade answers with an error and attach answers by writing nothing — into a
// panic inside the handler. Flush below is written the same way.
func (s *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	s.status = http.StatusSwitchingProtocols
	return hj.Hijack()
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func match(pattern, parts []string) (params, bool) {
	if len(pattern) != len(parts) {
		return nil, false
	}
	var p params
	for i, seg := range pattern {
		if strings.HasPrefix(seg, ":") {
			if parts[i] == "" {
				return nil, false
			}
			v, err := url.PathUnescape(parts[i])
			if err != nil {
				return nil, false
			}
			if p == nil {
				p = params{}
			}
			p[seg[1:]] = v
		} else if seg != parts[i] {
			return nil, false
		}
	}
	return p, true
}

// writeJSON sends v with keys in sorted order (as serde_json's default map
// does) and without HTML escaping.
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	body := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

var okBody = map[string]any{"ok": true}

// body is a JSON object request body with serde-style typed field access.
type body map[string]json.RawMessage

// decodeBody decodes a JSON object body the way axum's Json extractor accepts
// one: an application/json (or +json) content type and a valid object. It reports
// why a body was rejected; callers reach it through readBounded, which is what
// bounds the read.
//
// A wrong content type is answered by those callers with a 400 carrying the
// route's own message, not the 415 that axum's Json extractor produces. That is
// deliberate and it is the parity-correct answer: the reference declares every
// body as Option<Json<T>>, and on the axum it pins (0.7.9) Option's FromRequest
// turns a rejected inner extraction into Ok(None) rather than propagating the
// rejection, so the 415 never reaches the client -- the handler maps None to a 400
// with its own text. Answering 415 here would create a divergence, not remove one.
// (axum 0.8 changed this; Option there requires OptionalFromRequest. If the
// reference moves, re-derive this from its call sites rather than from the
// extractor's documented default.)
func decodeBody(r *http.Request) (body, error) {
	var b body
	if err := decodeInto(r, &b); err != nil {
		return nil, err
	}
	// A bare `null` body decodes into a nil map without error.
	if b == nil {
		return nil, errNotJSON
	}
	return b, nil
}

// decodeInto decodes a body into v with decodeBody's content type check and
// rejection of trailing data. It exists for a route whose body carries one field
// big enough that decoding it twice is worth avoiding.
//
// body holds each field as a json.RawMessage and unmarshals it on access, so a
// field's bytes are copied once into the map and scanned again on the way out.
// That is free for a mode string or a flag and it is not free for a paste: at
// 256 KB the second pass re-scans and re-unescapes the whole text. A struct
// field decodes straight out of the reader instead.
//
// The semantics have to match the map path exactly, because every way the two
// could differ is a body this daemon rejects today and would start accepting.
// Two rules keep them matched:
//
//   - Declare a required string as *string. A plain string field takes JSON
//     null as a no-op, leaving "" with a nil error, which turns a rejected body
//     into an accepted one. A nil pointer is then rejected by the same check
//     that rejects an absent field, and rejecting both is what the map path
//     already does -- str(key, true) fails on absent and on null alike, so
//     collapsing the two loses no distinction any route reads.
//   - Keep every other field as json.RawMessage and read it with the raw*
//     helpers below. The strictness they encode (a null element, integer
//     syntax, a bounded uint, bool as a literal rather than anything truthy) is
//     then the same code both paths run, not a second copy of it that can drift.
//
// A type error on any field aborts the decode and is returned here, where the
// map path would have accepted the body and failed the field on access. Both end
// at the route's own 400, because readBoundedInto reports ok=false for any error
// it does not turn into a 413, exactly as readBounded does.
//
// Both paths are also held to carrying a string's bytes unchanged, which neither
// did: encoding/json substitutes U+FFFD for a byte no JSON string can hold and
// reports no error. That is checked here, on the body's bytes, because it is the
// one point both paths pass through -- see unfaithful for why a per-field check
// could not cover the struct path at all.
//
// Needing the bytes is why this reads the body whole and calls json.Unmarshal
// rather than streaming a json.Decoder off r.Body. Unmarshal parses the slice in
// place, so the body is copied once; a decoder wrapped round a reader over that
// same slice would buffer it a second time, which measured as 15% on a 256 KB
// paste. Unmarshal also rejects trailing data on its own, and the UseNumber the
// streaming form set is not needed: both paths hold every non-string field as a
// json.RawMessage, so no number is ever decoded here -- the raw* helpers parse
// them from their own bytes. The read is bounded by the MaxBytesReader that
// readBounded and readBoundedInto install, so "whole" is at most their limit.
func decodeInto(r *http.Request, v any) error {
	if !jsonContentType(r) {
		return errNotJSON
	}
	raw, err := readWhole(r)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		// A type error can be a folded key's, which exactKeys would ignore; a
		// syntax error cannot be anything else.
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			return err
		}
		if redone, err2 := exactKeys(raw, v); redone {
			if err2 != nil {
				return err2
			}
		} else {
			return err
		}
	} else if _, err := exactKeys(raw, v); err != nil {
		return err
	}
	// Last, so that a body which is both malformed and unfaithful is reported
	// for its syntax: a caller whose JSON does not parse has a different thing
	// to fix, and this message would send them looking at the wrong byte. Both
	// faults are a 400 either way, so the order decides only which message the
	// caller reads -- TestAMalformedBodyIsReportedForItsSyntax pins it.
	//
	// Checking raw rather than the decoded value is the whole point: by the time
	// a string has been decoded the substitution has already happened and is no
	// longer visible.
	if err := unfaithful(raw); err != nil {
		return err
	}
	return nil
}

// exactKeys holds a struct decode to the map path's exact key match. encoding/json
// matches a key to a field case-insensitively, so {"Text":"hi"} filled paste's
// text, where the map path and shelld-rs's serde struct both ignore the key as
// unknown (H27). When a top-level key of raw folds to one of v's field names
// without being it, v is decoded again from the exactly named members alone, in
// order, so the last one wins as before; redone reports that it was. A body with
// no such key, which is every body a conforming client sends, costs one scan. A
// map target needs none of this.
//
// raw must be syntactically valid JSON, which it is once json.Unmarshal has
// returned nil or a type error.
func exactKeys(raw []byte, v any) (redone bool, err error) {
	rv := reflect.ValueOf(v).Elem()
	if rv.Kind() != reflect.Struct {
		return false, nil // a map's keys are exact already
	}
	rt := rv.Type()
	names := make([]string, rt.NumField())
	for i := range names {
		names[i], _, _ = strings.Cut(rt.Field(i).Tag.Get("json"), ",")
	}
	folded := false
	eachMember(raw, func(key, _ []byte) {
		k := memberKey(key)
		for _, n := range names {
			if k != n && strings.EqualFold(k, n) {
				folded = true
			}
		}
	})
	if !folded {
		return false, nil
	}
	rv.SetZero()
	eachMember(raw, func(key, val []byte) {
		k := memberKey(key)
		if i := slices.Index(names, k); i >= 0 && err == nil {
			err = json.Unmarshal(val, rv.Field(i).Addr().Interface())
		}
	})
	return true, err
}

// memberKey decodes a member's quoted key, unescaping only when it has to.
func memberKey(key []byte) string {
	if bytes.IndexByte(key, '\\') < 0 {
		return string(key[1 : len(key)-1])
	}
	var k string
	_ = json.Unmarshal(key, &k)
	return k
}

// eachMember calls f with the quoted key and the raw value of each member of the
// object b holds, in order. b must be valid JSON; anything but an object has no
// members.
func eachMember(b []byte, f func(key, val []byte)) {
	i := skipSpace(b, 0)
	if i >= len(b) || b[i] != '{' {
		return
	}
	i++
	for {
		i = skipSpace(b, i)
		if i >= len(b) || b[i] != '"' {
			return
		}
		end := skipString(b, i)
		key := b[i:end]
		i = skipSpace(b, end) + 1 // the ':'
		i = skipSpace(b, i)
		vEnd := skipValue(b, i)
		f(key, b[i:vEnd])
		i = skipSpace(b, vEnd)
		if i >= len(b) || b[i] != ',' {
			return
		}
		i++
	}
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// skipString returns the index just past the string opening at b[i].
func skipString(b []byte, i int) int {
	for i++; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return i
}

// skipValue returns the index just past the value starting at b[i].
func skipValue(b []byte, i int) int {
	depth := 0
	for i < len(b) {
		switch b[i] {
		case '"':
			i = skipString(b, i)
			if depth == 0 {
				return i
			}
			continue
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return i
			}
			depth--
			if depth == 0 {
				return i + 1
			}
		case ',':
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return i
}

// readWhole reads a request body into one slice.
//
// It sizes that slice from Content-Length, which is the difference between one
// allocation and ten on a large paste: io.ReadAll grows by doubling, so a 256 KB
// body is allocated and copied through every power of two on the way up. The
// header is a client's claim rather than a fact, so it is used only as a hint and
// only up to the largest body any route accepts -- a client that overstates it
// gets the growth path back, one that understates it is read to the end anyway,
// and either way the MaxBytesReader installed by readBounded and readBoundedInto
// is what actually stops the read.
//
// The spare bytes.MinRead are what bytes.Buffer.ReadFrom reserves before each
// read; without them a body of exactly Content-Length bytes reallocates on the
// final read that returns EOF, which is the allocation this is here to avoid.
func readWhole(r *http.Request) ([]byte, error) {
	if n := r.ContentLength; n > 0 && n <= maxControlBody {
		buf := bytes.NewBuffer(make([]byte, 0, n+bytes.MinRead))
		if _, err := buf.ReadFrom(r.Body); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	return io.ReadAll(r.Body)
}

// unfaithful reports why a JSON body's strings would not decode to the bytes they
// name, or nil when they would. encoding/json does not refuse a string it cannot
// represent; it substitutes U+FFFD and reports no error, so a body that asks to
// write a byte no JSON string can hold is otherwise accepted and written as
// something else.
//
// There are exactly two ways that happens, both established by measurement
// rather than read off the documentation: a byte in the body that is not part of
// valid UTF-8, and a \uXXXX escape naming one half of a surrogate pair. The
// first is why the body is scanned rather than each token -- a token check
// cannot cover the struct path, whose string fields are decoded straight out of
// the reader and never exist as tokens here.
//
// The two are reported separately because they say different things about the
// caller, and a caller who cannot tell which one it hit is left guessing. A byte
// outside valid UTF-8 can only come from a client holding bytes, for which a JSON
// string is the wrong framing. A lone surrogate half arrives from a client holding
// text, and in one particular way: a JavaScript string is a sequence of UTF-16
// code units, so slicing one at a fixed length can cut an astral character (an
// emoji) in half, and JSON.stringify escapes the orphaned half rather than
// refusing it. Naming that in the response is the difference between a client
// fixing its slice and a client wondering which field was wrong.
//
// Both checks are properties of the body, so neither can be fooled by a string
// that legitimately contains U+FFFD: an escaped � and the literal bytes
// ef bf bd are valid UTF-8 and name no surrogate, and both are accepted.
func unfaithful(b []byte) error {
	if !utf8.Valid(b) {
		return errNotUTF8
	}
	if hasLoneSurrogateEscape(b) {
		return errLoneSurrogate
	}
	return nil
}

// hasLoneSurrogateEscape reports whether b contains a \uXXXX escape naming one
// half of a surrogate pair without the other half.
//
// Escapes are consumed two bytes at a time so that a literal backslash cannot
// start one: in `\\ud800` the first backslash escapes the second and the rest is
// the text `ud800`, which is not an escape and is accepted. A malformed escape is
// left alone, because the decode rejects the body for its own reasons and
// reporting this instead would name the wrong fault.
//
// The walk is one byte at a time and so costs about 3 GB/s, against the 64 GB/s
// utf8.Valid manages on the same bytes. That is why it runs behind a search for
// the only two-byte sequence any escape must contain: a body with no `\u` in it
// anywhere cannot name a surrogate, and bytes.Contains settles that in a
// vectorised pass. A paste of ordinary text takes only that pass.
func hasLoneSurrogateEscape(b []byte) bool {
	if !bytes.Contains(b, []byte(`\u`)) {
		return false
	}
	for i := 0; i+1 < len(b); {
		if b[i] != '\\' {
			i++
			continue
		}
		if b[i+1] != 'u' {
			i += 2
			continue
		}
		hi, ok := hex4(b[i+2:])
		if !ok {
			return false
		}
		i += 6
		if hi < 0xD800 || hi > 0xDFFF {
			continue
		}
		if hi >= 0xDC00 {
			return true // a low half with no high half ahead of it
		}
		if i+1 >= len(b) || b[i] != '\\' || b[i+1] != 'u' {
			return true
		}
		lo, ok := hex4(b[i+2:])
		if !ok || lo < 0xDC00 || lo > 0xDFFF {
			return true
		}
		i += 6
	}
	return false
}

// hex4 reads the four hex digits of a \uXXXX escape.
func hex4(b []byte) (rune, bool) {
	if len(b) < 4 {
		return 0, false
	}
	var r rune
	for _, c := range b[:4] {
		var d rune
		switch {
		case c >= '0' && c <= '9':
			d = rune(c - '0')
		case c >= 'a' && c <= 'f':
			d = rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = rune(c-'A') + 10
		default:
			return 0, false
		}
		r = r<<4 | d
	}
	return r, true
}

func jsonContentType(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}
	// A suffix needs a subtype name in front of it (RFC 6838 §4.2.8), so
	// application/+json is not JSON, as shelld-rs's json_content_type has it.
	return mt == "application/json" ||
		(strings.HasPrefix(mt, "application/") && strings.HasSuffix(mt, "+json") && len(mt) > len("application/+json"))
}

var errNotJSON = errors.New("body is not a JSON object")

// errNotUTF8 and errLoneSurrogate are the two ways a body carries a string
// encoding/json would silently rewrite. See unfaithful.
var (
	errNotUTF8       = errors.New("body contains a byte that is not valid UTF-8")
	errLoneSurrogate = errors.New("body contains an escape naming half a surrogate pair")
)

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

// str decodes a String field: required fields must be present; a present
// field must be a JSON string (null is rejected, as serde rejects it).
func (b body) str(key string, required bool) (string, bool) {
	raw, ok := b[key]
	if !ok {
		return "", !required
	}
	var s string
	if isNull(raw) || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// optStr decodes an Option<String>: absent or null is none.
func (b body) optStr(key string) (s string, present, ok bool) { return rawStr(b[key]) }

// rawStr decodes an Option<String> from one field's raw bytes. A nil raw is an
// absent field, which is what a missing map key yields and what a struct field
// left untouched by decodeInto yields, so the two paths share this.
func rawStr(raw json.RawMessage) (s string, present, ok bool) {
	if raw == nil || isNull(raw) {
		return "", false, true
	}
	if json.Unmarshal(raw, &s) != nil {
		return "", false, false
	}
	return s, true, true
}

// strs decodes a Vec<String> field: absent or null is none, and anything that
// is not an array of strings is rejected, as serde rejects it. The elements are
// checked one at a time because encoding/json decodes a null element into the
// empty string without complaining, where serde makes it an error — and a
// silent "" in a list of variable names is a name the caller did not send.
func (b body) strs(key string) ([]string, bool) {
	raw, exists := b[key]
	if !exists || isNull(raw) {
		return nil, true
	}
	var elems []json.RawMessage
	if json.Unmarshal(raw, &elems) != nil {
		return nil, false
	}
	out := make([]string, 0, len(elems))
	for _, e := range elems {
		var s string
		if isNull(e) || json.Unmarshal(e, &s) != nil {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// uint decodes an unsigned integer field no larger than limit.
func (b body) uint(key string, required bool, limit uint64) (uint64, bool) {
	raw, ok := b[key]
	if !ok {
		return 0, !required
	}
	return parseUint(raw, limit)
}

// optUint decodes an Option<unsigned>.
func (b body) optUint(key string, limit uint64) (v uint64, present, ok bool) {
	raw, exists := b[key]
	if !exists || isNull(raw) {
		return 0, false, true
	}
	v, ok = parseUint(raw, limit)
	return v, ok, ok
}

func parseUint(raw json.RawMessage, limit uint64) (uint64, bool) {
	v, err := strconv.ParseUint(string(bytes.TrimSpace(raw)), 10, 64)
	return v, err == nil && v <= limit
}

// int32 decodes an i32 field defaulting to 0.
func (b body) int32(key string) (int32, bool) {
	raw, ok := b[key]
	if !ok {
		return 0, true
	}
	v, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 32)
	return int32(v), err == nil
}

// optBool decodes an Option<bool>.
func (b body) optBool(key string) (v, present, ok bool) { return rawBool(b[key]) }

// rawBool decodes an Option<bool> from one field's raw bytes; see rawStr on the
// nil raw. Compared against the two literals rather than unmarshalled, so 1 and
// "true" are rejected as serde rejects them.
func rawBool(raw json.RawMessage) (v, present, ok bool) {
	if raw == nil || isNull(raw) {
		return false, false, true
	}
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, true, true
	case "false":
		return false, true, true
	}
	return false, false, false
}

// queryBool reads an optional flag query parameter. A bare `?key` (as HTML
// forms send it) and the values "1", "true", "0" and "false" are accepted
// case-insensitively; ok is false for any other value, so a value the daemon
// cannot parse never decides the flag silently. An absent key returns
// (false, true): a client that does not send the parameter at all is not an
// error, which is what keeps the flag optional.
//
// Note that `?key` and `?key=` are indistinguishable here — net/url parses both
// to a single empty value — so the empty string has to mean one of them, and it
// means "on" to make the bare form work.
func queryBool(q url.Values, key string) (v, ok bool) {
	vals, has := q[key]
	if !has || len(vals) == 0 {
		return false, true
	}
	// A repeated key is a contradiction the client never decided. Taking the
	// last value made `?key=1&key=0` and `?key=0&key=1` disagree, so the outcome
	// turned on an ordering nobody chose — and for a flag that grants write
	// access, one of those orderings is the unsafe one. It also left the parser
	// inconsistent with itself: `?key=1&key=yes` was already refused, which is
	// stricter about a contradiction containing garbage than about one between
	// two values it can read perfectly well.
	if len(vals) > 1 {
		return false, false
	}
	switch strings.ToLower(vals[0]) {
	case "", "1", "true":
		return true, true
	case "0", "false":
		return false, true
	}
	return false, false
}

// queryKeyNearMiss reports a query key that is recognisably a botched spelling
// of want rather than a key from some other vocabulary.
//
// An unrecognised query parameter has to be ignored for version skew, but that
// makes a misspelled *known* parameter indistinguishable from a newer client's
// parameter — and for a flag that grants write access, guessing wrong is not
// symmetric. Catching the near misses is the part of that gap we can close
// without giving up the ignore-unknown contract. Two forms qualify:
//
//   - a case variant (`?Observer=1`), and
//   - an encoded separator (`?observer%3d1`), which ParseQuery splits on `=`
//     *before* unescaping, so the whole segment becomes one key named
//     `observer=1` with an empty value. That folds to nothing we know, so the
//     flag read as absent and an over-escaped URL granted write access.
//
// A near miss is refused even when the exact key is also present: a client that
// sends both `observer` and `Observer` has contradicted itself, and the same
// asymmetry that refuses an unreadable value refuses this too.
func queryKeyNearMiss(q url.Values, want string) (string, bool) {
	for k := range q {
		if k == want {
			continue
		}
		if strings.EqualFold(k, want) {
			return k, true
		}
		if name, _, found := strings.Cut(k, "="); found && strings.EqualFold(name, want) {
			return k, true
		}
	}
	return "", false
}

// refuseNearMissKey answers 400 if the query misspells any of wants, and reports
// whether it did. Every route that reads a query parameter goes through this, so
// the message exists once and a new route cannot word it differently.
//
// The asymmetry that justifies the rule on attach is about write access, but it is
// not confined to that. On capture and scroll a misspelled key is answered 200 with
// the parameter at its default, and for `offset` that default is the top of the
// scrollback -- so `?Offset=500` hands back the oldest page as though it were the
// page that was asked for, indistinguishable to the caller from a correct answer.
// That is the same wrong-answer-reported-as-success these routes were made to parse
// strictly to avoid, reached this time through a query net/url reads perfectly well.
//
// Only near misses are refused, so the ignore-unknown contract still holds and a
// parameter from a newer client is still ignored. A plain typo (`?ofset=10`) is not
// a near miss under queryKeyNearMiss's definition and is not caught: it is
// indistinguishable from a name this daemon has never heard of, which is exactly
// what version skew looks like. That gap is not closable without giving up the
// contract, and it is narrower than it sounds -- a case variant is what a client
// written to a camelCase convention produces, and an encoded separator is what an
// over-eager URL builder produces, where a letter left out is a typo its author
// sees the first time they run it.
//
// wants is ranged over in order rather than the query, so a request misspelling two
// keys always names the same one; ranging over the map would name whichever it
// happened to yield first and make the message unreproducible.
func refuseNearMissKey(w http.ResponseWriter, q url.Values, wants ...string) bool {
	for _, want := range wants {
		if k, near := queryKeyNearMiss(q, want); near {
			badQuery(w, "query key `"+k+"` is not `"+want+"`: a near miss of a known key is refused, not ignored")
			return true
		}
	}
	return false
}

// parseQueryStrict parses a request's query, refusing one net/url cannot read.
//
// url.URL.Query discards ParseQuery's error, and ParseQuery does not stop at the
// segment it objects to — it drops that segment and keeps going. Since Go 1.17 a
// `;` is such an objection, and a query is split on `&` before that check, so one
// `;` anywhere makes its whole segment unreadable: `?observer=1;` and
// `?offset=10;lines=1000` each arrive as an *empty* query.
//
// Every parameter in such a query then falls back to its default, under a 200,
// with nothing for the client to notice. That is why each route whose parameters
// have defaults parses through here rather than only attach: the damage is not
// confined to reading a smaller number than the one asked for. `observer` reads
// as absent and a client that asked to watch gets a writable attach; `mode` reads
// as absent and capture returns raw history instead of the visible grid; `offset`
// reads as absent and a pager is handed the top of the scrollback as the page it
// requested. Each is a different answer to a different question, reported as
// success.
//
// A parameter this daemon does not know is still ignored, which is what lets an
// older client degrade gracefully. That survives here because an unknown *name*
// is not a parse error. An unknown parameter whose *encoding* is invalid is a
// different case and is refused, and it is worth being explicit that this refuses
// a request the loose parser would have answered correctly: a bad percent-escape,
// unlike a `;`, costs only its own segment, so `?lines=5&junk=%zz` did reach the
// route with lines=5 intact and now returns a 400.
//
// Refused deliberately, for two reasons. The contract that keeps version skew
// working is about names a future client may add, and a client adding one encodes
// it properly; nothing is made forward-compatible by serving a URL that is not a
// valid URL. And the alternative is to decide case by case whether the unreadable
// segment was one this route happened to read, which is the loose behaviour with
// a guess on top — and guessing wrong in that direction is how `;` came to
// silently grant a writable attach. attach has answered 400 to
// `?observer=1&junk=%zz` since it started parsing strictly, so this is the
// existing behaviour of this daemon rather than a new rule for two routes.
func parseQueryStrict(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		badQuery(w, "malformed query string")
		return nil, false
	}
	return q, true
}

// queryUint parses an optional usize query parameter.
func queryUint(q url.Values, key string, def int) (int, bool) {
	vals, ok := q[key]
	if !ok || len(vals) == 0 {
		return def, true
	}
	v, err := strconv.ParseUint(vals[len(vals)-1], 10, 63)
	return int(v), err == nil
}
