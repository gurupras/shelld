package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A shutdown decided before Serve was reached is the state that registering the
// signal handlers above the bind creates, and it is reachable only because of
// that ordering: the listener is bound and accepting connect() while the
// program is still setting itself up, so Serve has to shut down without ever
// serving a request and still leave nothing behind.
//
// The cleanup is not an unlink of Serve's own -- closing the listener is what
// removes the socket file -- so this reads the file as well as beforeClose, to
// hold that both halves of a signal's cleanup happen on a path where no request
// ever arrived.
func TestServeHonorsAContextDoneBeforeItIsCalled(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	if runtime.GOOS == "windows" {
		// A pipe name, not a path: the pipe listener refuses a filesystem path
		// with "Incorrect function".
		sock = `\\.\pipe\shelld-test-` + NewNonce()
	}
	ln, err := listen(sock, ListenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st := NewState(sock)
	// Not a no-op: leaving the real os.Exit in place would take the test binary
	// down, but a no-op would silently absorb a request this path is supposed
	// never to serve. The one route that calls Exit is POST /shutdown.
	st.Exit = func(code int) { t.Errorf("a request was served on the shutdown path: Exit(%d)", code) }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	closed := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, sock, st, func() { close(closed) }) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return for a context that was already done")
	}
	select {
	case <-closed:
	default:
		t.Error("beforeClose did not run, so the lockfile would outlive the daemon")
	}
	if runtime.GOOS != "windows" {
		if _, err := os.Lstat(sock); !os.IsNotExist(err) {
			t.Errorf("socket file still present after Serve returned: Lstat = %v", err)
		}
	}
	// Serve owns this wiring on every path, not just the one that serves: a
	// daemon that came up while a shutdown was pending and then did not exit
	// would otherwise answer POST /shutdown by doing nothing.
	if st.Shutdown == nil {
		t.Error("state.Shutdown not set")
	}
}
