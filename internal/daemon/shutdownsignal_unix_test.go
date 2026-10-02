//go:build linux || darwin

package daemon

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The set of signals lives in NotifyShutdown so that the program registering
// them before its bind and the Serve that honors the result cannot disagree
// about which ones they are. Sending each to this process is what holds the set
// to what Serve documents: a signal missing from it keeps its default
// disposition, and the test binary dies the way the daemon would.
//
// Registering is the whole subject, so there is no listener here. That is the
// point of the split from Serve -- this is what a program gets to do before it
// has one.
//
// Killing the test binary is only a usable oracle while nothing else in package
// daemon sends SIGINT or SIGTERM to its own process, and while no test here runs
// in parallel with this one: stop() puts both back to their default disposition,
// so a stray one landing outside the window below kills the run instead of
// failing a test. Nothing does today -- the package's other signals go to child
// processes, or are Kill(pid, 0) liveness probes -- and the package has no
// t.Parallel() anywhere. Both are invariants this test depends on.
func TestNotifyShutdownCatchesTheSignalsServeDocuments(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, stop := NotifyShutdown()
			defer stop()
			// signal.NotifyContext registers before it looks at the parent, so a
			// context that comes back already canceled would absorb the kill below
			// and satisfy the Done() read without the signal reaching it at all.
			// A daemon wired to one of those never serves a request in its life.
			if err := ctx.Err(); err != nil {
				t.Fatalf("context already done before any signal: %v", err)
			}
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatalf("kill: %v", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
				t.Fatalf("context not canceled by %v", sig)
			}
		})
	}
}

// The test above bounds the set from below; this bounds it from above. Handling
// a signal the daemon has other plans for is as wrong as missing one: SIGHUP is
// the case in the tree, where a daemon that took it as a shutdown would exit on
// the hangup its own clients survive (see cmd/shelld's hangup test), and
// swallowing it is just as bad as exiting, since it would then reach neither
// Serve nor the default disposition.
//
// The kill has to be observed by something, or SIGHUP's default disposition ends
// the run before any assertion, and a test that only proves SIGHUP never
// arrived proves nothing about the set. A second registration is that observer.
func TestNotifyShutdownLeavesOtherSignalsAlone(t *testing.T) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	ctx, stop := NotifyShutdown()
	defer stop()

	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("kill: %v", err)
	}
	select {
	case <-hup:
	case <-time.After(10 * time.Second):
		t.Fatal("SIGHUP was not delivered, so this proves nothing about the set")
	}
	// Waited on rather than read once, because signal.NotifyContext cancels from
	// its own goroutine: the delivery above and that cancel are not ordered
	// against each other, so a set that does include SIGHUP passes a
	// non-blocking read here just by being the slower of the two. It did.
	select {
	case <-ctx.Done():
		t.Error("SIGHUP canceled the shutdown context, so the daemon would exit on a hangup")
	case <-time.After(2 * time.Second):
	}
}

// What the bind opens up is a client, not just a signal: the kernel queues a
// connect from the moment the address exists, so a shutdown decided during
// startup has a connection already waiting on it. Serving that request would
// answer a daemon that is going away and whose lockfile is being removed in the
// same breath, so Serve must accept nothing at all on this path.
//
// Unix-only because it dials the socket -- the portable half of this path is in
// TestServeHonorsAContextDoneBeforeItIsCalled.
func TestServeAnswersNothingQueuedBeforeAShutdown(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	ln, err := listen(sock, ListenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st := NewState(sock)
	st.Exit = func(code int) { t.Errorf("a queued request reached POST /shutdown: Exit(%d)", code) }

	// Queued before Serve is ever called, which is the ordering the window
	// creates: bound and connectable while the program is still starting up.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET /health HTTP/1.1\r\nHost: shelld\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	// The wait is what makes this an assertion about accepting rather than a
	// scheduling race. beforeClose runs while the address is still owned and does
	// real work there -- removing the lockfile -- so a queued request has that
	// whole span to be answered in; without the wait an accept loop launched here
	// loses to the shutdown almost every time and the test is a 1-in-70 detector.
	// Nothing can be served across it if nothing was ever accepted.
	go func() {
		done <- Serve(ctx, ln, sock, st, func() { time.Sleep(250 * time.Millisecond) })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return for a context that was already done")
	}

	// A read rather than a sleep: closing the listener resets whatever the
	// backlog still holds, so the client learns the request went unanswered.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err == nil {
		resp.Body.Close()
		t.Errorf("a request queued before the shutdown was answered: %s", resp.Status)
	}
}
