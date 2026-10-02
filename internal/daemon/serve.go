package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Listen binds addr: a Unix socket path, or a named pipe on Windows. On
// Unix the caller holds the socket's instance lock first (AcquireInstance),
// since binding replaces a stale socket file.
func Listen(addr string, opts ListenOptions) (net.Listener, error) { return listen(addr, opts) }

// CloseListener releases a listener from Listen that will not be served. On Unix
// this is also what removes the socket file, so a caller that gives up after
// binding has nothing to unlink afterwards -- see listen for why the removal has
// to happen while the descriptor is still bound.
func CloseListener(ln net.Listener) {
	_ = ln.Close()
}

// NotifyShutdown installs handlers for the signals Serve stops on -- SIGINT and
// SIGTERM -- and returns a context they cancel plus a stop that removes them
// again. signal.NotifyContext registers at this call, not at the first read of
// the context, so a signal that arrives before Serve is still caught.
//
// It is separate from Serve so that a program can register before it binds its
// listener. Between a bind and the first handler a signal has its default
// disposition, which for both of these is to end the process at once, running
// no deferred cleanup -- while the socket is already bound and accepting
// connect(). Register first and that window instead records the signal for
// Serve to honor.
//
// Registering is the program's call to make, the same as any other
// process-wide decision, which is why this is a separate step and not
// something Serve does for it. What it costs is that the signals stop being
// immediately fatal from here until stop: each one is recorded instead, and
// only the first is even that -- signal.NotifyContext buffers one and reads it
// once, so a second SIGTERM arriving before Serve honors the first does
// nothing at all. So keep the span from here to Serve bounded, and know what
// bounds it rather than assuming it is nothing.
func NotifyShutdown() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// Serve serves the API on ln, bound to addr, until ctx is done or POST
// /shutdown. Pass the context from NotifyShutdown for the signals; one caught
// before this call shuts down without serving a request -- nothing is accepted
// at all, see serveListener -- with the socket file and beforeClose handled
// exactly as for a signal caught while serving. That case announces no
// listening address either, having never listened for anything.
//
// A shutdown while serving drains for at most drainTimeout before Serve returns:
// requests in flight are finished, and every attach is sent a close 1001 "daemon
// shutting down" and given the rest of that time to answer it; see drain.
//
// beforeClose, when set, runs before the listener closes, while this process
// still owns the address; closing the listener removes the socket file, so
// nothing here unlinks it afterwards -- see listen for why the removal has to
// happen while the descriptor is still bound.
func Serve(ctx context.Context, ln net.Listener, addr string, state *State, beforeClose func()) error {
	if ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "shelld: listening on %s (pid %d)\n", addr, os.Getpid())
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	state.Shutdown = cancel
	return serveListener(ctx, ln, state, beforeClose)
}

// ServeListener serves the API on ln until ctx is done. It installs no signal
// handlers and does not set state.Shutdown, so a caller that wants either does
// what Serve does: NotifyShutdown for the signals, its own cancel for the
// route.
func ServeListener(ctx context.Context, ln net.Listener, state *State) error {
	return serveListener(ctx, ln, state, nil)
}

// readHeaderTimeout and idleTimeout bound how long a connection that is not
// making progress can hold a goroutine and a descriptor. Without them a peer
// that connects and sends nothing, or sends half a request line and stops, is
// held until the daemon exits, and nothing in the daemon ever lets go.
//
// The values are this daemon's own and were kept when the bounds below were
// adopted from the reference daemon, whose head bound is 120 s (serve.rs
// HEAD_TIMEOUT). The case for them is that this is a library other programs
// embed: an embedding process cannot put a timeout on connections it does not
// own, so a server with no bound of its own gives its host no way to reclaim one. It is also what
// gosec reports as G112 on any http.Server literal that omits the header bound.
//
// Neither value needs to be precise, because neither is reached by a client that
// is working. A local peer writes its headers in one go, so any finite header
// bound is generous; what matters is that it is finite. The idle bound sits above
// http.Transport's own IdleConnTimeout default of 90s, so a Go client left at the
// standard library's defaults always retires an idle connection before this
// server would, and a request cannot race a close from this end.
//
// A connection the handler hijacked -- which is every WebSocket, once the upgrade
// has answered -- is not reached by either: the deadlines belong to the hijacker
// from that point on. An observer holding an idle socket for hours is unaffected.
const (
	readHeaderTimeout = 30 * time.Second
	idleTimeout       = 120 * time.Second
)

// drainTimeout bounds how long a shutdown waits for what is still in flight,
// shelld-rs's DRAIN_TIMEOUT (serve.rs). The wait is what lets a response that has
// been started be finished -- /shutdown answers on a connection the shutdown is
// taking away, and so does any request in a handler when a signal arrives -- and
// it is bounded because a handler blocked on something that never returns must not
// keep the daemon alive. One deadline covers both the HTTP requests and the
// attaches, so a shutdown takes at most this long in total.
const drainTimeout = 2 * time.Second

// The connection bounds shelld-rs serves under (serve.rs MAX_CONNECTIONS,
// PERMIT_TIMEOUT, BODY_TIMEOUT), as variables so a test can shorten them; the
// server and its listener read them when they are built.
//
// maxConnections bounds the connections served at once, and with them the
// goroutines and read buffers a client can make the daemon hold. A connection
// gives its slot back when it closes or when it is hijacked: an upgraded attach
// is bounded by MaxAttaches instead, since one that held a slot would let enough
// viewers shut out /health.
//
// permitTimeout is how long the accept loop waits for a free slot before it
// accepts one connection anyway and tells it the daemon is full, in place of
// leaving it queued with no bound. rs derives its 45 s from the bounds around it,
// above bodyTimeout in particular, so a shed is caused by connections that were
// actively in use and not by one that merely stalled and has yet to be reaped.
//
// bodyTimeout bounds the silence of a request body, not its duration: it is
// re-armed by every read, so a body that keeps arriving is read however long it
// takes, while one that stops is answered as a body that could not be read.
// ReadHeaderTimeout does not cover a body, so without this a complete head with
// a Content-Length and then nothing held its handler for ever.
var (
	maxConnections = 256
	permitTimeout  = 45 * time.Second
	bodyTimeout    = 30 * time.Second
)

// refuseTimeout bounds the write of the 503 a refused connection is sent, rs's
// REFUSE_TIMEOUT: a client that does not read it is not waited on.
const refuseTimeout = 2 * time.Second

// connLimitBody is the refused connection's body, rs's text.
const connLimitBody = `{"error":"daemon is at its connection limit, retry in a moment"}`

// newServer builds the server serveListener runs. It exists so a test can hold
// the bounds below to being set at all, and can run a server built exactly like
// the real one with the two durations shortened to something a test can wait for.
func newServer(state *State) *http.Server {
	return &http.Server{
		Handler:           boundBodies(Handler(state), bodyTimeout),
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ConnState:         releaseOnHijack,
	}
}

// boundBodies puts a silence bound of d on every request body h reads.
func boundBodies(h http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = &silentBody{ReadCloser: r.Body, rc: http.NewResponseController(w), d: d}
		}
		h.ServeHTTP(w, r)
	})
}

// silentBody re-arms the connection's read deadline before every read of the
// body, and clears it at the end of the body, so that what is bounded is a gap
// between reads. Cleared rather than left: the server reads on in the
// background once a body is done, and a deadline still set there would fire
// mid-handler and cancel the request's context.
type silentBody struct {
	io.ReadCloser
	rc *http.ResponseController
	d  time.Duration
}

func (b *silentBody) Read(p []byte) (int, error) {
	_ = b.rc.SetReadDeadline(time.Now().Add(b.d))
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

// releaseOnHijack gives a hijacked connection's slot back at the hijack; see
// maxConnections. A connection that closes gives it back in its own Close.
func releaseOnHijack(c net.Conn, s http.ConnState) {
	if pc, ok := c.(*permitConn); ok && (s == http.StateHijacked || s == http.StateClosed) {
		pc.release()
	}
}

// limitListener holds the connections it hands out to a fixed number of slots;
// see maxConnections and permitTimeout.
type limitListener struct {
	net.Listener
	slots   chan struct{}
	wait    time.Duration
	closed  chan struct{}
	closeMu sync.Once
}

func newLimitListener(ln net.Listener, n int, wait time.Duration) *limitListener {
	return &limitListener{Listener: ln, slots: make(chan struct{}, n), wait: wait, closed: make(chan struct{})}
}

// Accept takes a slot before it accepts, so a full daemon leaves arrivals in the
// listen backlog rather than holding them. Once wait goes by without a slot it
// accepts one connection and sheds it with a 503, unless a slot freed meanwhile,
// and then waits again: one shed per wait, which is what the Retry-After says.
func (l *limitListener) Accept() (net.Conn, error) {
	timer := time.NewTimer(l.wait)
	defer timer.Stop()
	for {
		select {
		case l.slots <- struct{}{}:
			return l.accept()
		case <-l.closed:
			return nil, net.ErrClosed
		case <-timer.C:
		}
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &permitConn{Conn: c, slots: l.slots}, nil
		default:
		}
		go refuseSaturated(c, l.wait)
		timer.Reset(l.wait)
	}
}

func (l *limitListener) accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &permitConn{Conn: c, slots: l.slots}, nil
}

func (l *limitListener) Close() error {
	l.closeMu.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

// permitConn is a connection holding one of a limitListener's slots.
type permitConn struct {
	net.Conn
	slots chan struct{}
	once  sync.Once
}

func (c *permitConn) release() { c.once.Do(func() { <-c.slots }) }

func (c *permitConn) Close() error {
	c.release()
	return c.Conn.Close()
}

// refuseSaturated tells a connection the daemon is full and closes it, without
// reading its request: the answer does not depend on it. Retry-After is one
// shedding cadence in whole seconds, rounded up, which is the soonest a retry can
// find a slot; rs's retry_after_secs.
func refuseSaturated(c net.Conn, cadence time.Duration) {
	defer c.Close()
	retry := max(int64((cadence+time.Second-1)/time.Second), 1)
	_ = c.SetWriteDeadline(time.Now().Add(refuseTimeout))
	_, _ = fmt.Fprintf(c, "HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/json\r\n"+
		"Content-Length: %d\r\nRetry-After: %d\r\nConnection: close\r\n\r\n%s", len(connLimitBody), retry, connLimitBody)
}

func serveListener(ctx context.Context, ln net.Listener, state *State, beforeClose func()) error {
	// Checked here and not only in the select below, because the accept loop
	// starts the moment it is launched: the kernel queues a connect as soon as
	// the address is bound, so a request can arrive during startup and be served
	// in full before ctx.Done() is noticed -- answered by a daemon that has
	// already been told to stop, and whose lockfile beforeClose is about to
	// remove. A shutdown decided before the first accept accepts nothing, and
	// whatever the backlog holds is closed unanswered.
	if ctx.Err() != nil {
		if beforeClose != nil {
			beforeClose()
		}
		_ = ln.Close()
		return nil
	}

	srv := newServer(state)
	lim := newLimitListener(ln, maxConnections, permitTimeout)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(lim) }()
	select {
	case <-ctx.Done():
		if beforeClose != nil {
			beforeClose()
		}
		_ = lim.Close()
		drain(srv, state)
		<-done
		return nil
	case err := <-done:
		if beforeClose != nil {
			beforeClose()
		}
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}

// drain finishes what is in flight once the listener is closed, for at most
// drainTimeout: the HTTP requests through srv.Shutdown, and the attaches, which
// Shutdown cannot see because their connections were hijacked at the upgrade, by
// telling each one the daemon is going away (see upgraded). Whatever is left at the
// deadline is cut: the daemon does not wait on a client that does not answer.
func drain(srv *http.Server, state *State) {
	deadline := time.Now().Add(drainTimeout)
	// Before either wait, so an attach spends the whole drain knowing it is ending.
	state.upgraded.stop(deadline)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if err := srv.Shutdown(ctx); errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("shutting down with connections still in flight")
		_ = srv.Close()
	}
	if n := state.upgraded.wait(deadline); n > 0 {
		slog.Warn("shutting down with upgraded connections still in flight", "upgraded", n)
	}
}

// upgraded tracks the attaches being served, whose connections the handler has
// hijacked from the http.Server and which Shutdown therefore never sees. The zero
// value is ready to use.
//
// An attach joins before its upgrade, while the server still counts its
// connection, so there is no moment at which a shutdown could find it in neither.
type upgraded struct {
	mu   sync.Mutex
	live int
	// stopCh is closed by stop; deadline is the drain's, set before the close.
	stopCh   chan struct{}
	deadline time.Time
	stopped  bool
	// left is signalled on every leave, for wait.
	left chan struct{}
}

func (u *upgraded) init() {
	if u.stopCh == nil {
		u.stopCh = make(chan struct{})
		u.left = make(chan struct{}, 1)
	}
}

// join counts an attach in and returns the channel stop closes. An attach that
// joins after the stop finds it already closed, and is told at once.
func (u *upgraded) join() <-chan struct{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.init()
	u.live++
	return u.stopCh
}

// leave counts an attach out.
func (u *upgraded) leave() {
	u.mu.Lock()
	u.live--
	u.mu.Unlock()
	select {
	case u.left <- struct{}{}:
	default:
	}
}

// stop tells every attach the daemon is going away, and that it has until
// deadline to finish.
func (u *upgraded) stop(deadline time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.init()
	if !u.stopped {
		u.stopped, u.deadline = true, deadline
		close(u.stopCh)
	}
}

// stopDeadline is the deadline stop set. Read it only once the channel join
// returned is closed.
func (u *upgraded) stopDeadline() time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.deadline
}

// wait waits for every attach to leave, until deadline, and returns how many
// were still there.
func (u *upgraded) wait(deadline time.Time) int {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		u.mu.Lock()
		u.init()
		n, left := u.live, u.left
		u.mu.Unlock()
		if n == 0 {
			return 0
		}
		select {
		case <-left:
		case <-timer.C:
			u.mu.Lock()
			defer u.mu.Unlock()
			return u.live
		}
	}
}
