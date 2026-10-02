package daemon

// The HTTP fixture the route tests share: an API over fake windows, driven in
// process.
//
// It is here, in a file with no build constraint, because nothing in it is
// platform-specific and several of the files that use it are not constrained
// either. While it lived in routes_errors_test.go, which is, the package's tests
// did not compile on Windows at all -- `go vet` and `go test` for internal/daemon
// failed on an undefined identifier -- so every one of those unconstrained files
// was unreachable on a platform the package otherwise supports.

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// api returns the API over a manager holding the named sessions, each holding
// the named windows — fake windows, with a real grid, scrollback and inject
// queue but no PTY and no child (see shelld.NewFakeWindow).
//
// Nothing it builds forks, binds a socket or serves, which is what makes it the
// right fixture for a case settled before a handler reaches a live program: a real
// window would cost a fork and give the same answer. Driving Handler in process
// also reaches one arm a served request cannot, because httptest's recorder is not
// an http.Hijacker, so an attach upgrade over it fails on demand --
// routes_errors_test.go is where that case lives.
func api(t *testing.T, sessions map[string][]string) (*State, http.Handler) {
	t.Helper()
	st := NewState("/nonexistent/shelld-routes-test.sock")
	st.Exit = func(int) {}
	// No debug.FreeOSMemory a second after a DELETE: a fake window holds nothing
	// to release and the collection is process-wide.
	st.release = nil
	for _, name := range slices.Sorted(maps.Keys(sessions)) {
		if err := st.Mgr.CreateSession(name); err != nil {
			t.Fatal(err)
		}
		sess := st.Mgr.Session(name)
		for _, win := range sessions[name] {
			sess.InsertFakeWindow(win)
		}
	}
	return st, Handler(st)
}

// call sends a JSON request and returns the status and the body verbatim.
func call(t *testing.T, h http.Handler, method, path, body string) (int, string) {
	t.Helper()
	return callAs(t, h, method, path, "application/json", body)
}

func callAs(t *testing.T, h http.Handler, method, path, ctype, body string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}
