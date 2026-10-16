package shelld

import (
	"fmt"
	"sort"
	"sync"
)

// Manager is the registry of sessions. It is the top of the ownership chain: a
// Manager owns its sessions, a Session owns its windows, and a Window owns the
// PTY and the process on the far end of it. Killing at any level kills
// everything below.
//
// All of its methods are safe to call from several goroutines. Its lock covers
// the session map only; the sessions themselves lock separately, and the one
// method that needs both is MoveWindow, which takes the Manager's first.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
}

// NewManager returns an empty Manager.
func NewManager() *Manager { return &Manager{sessions: make(map[string]*Session)} }

// CreateSession adds an empty session, and fails if one already has that name.
func (m *Manager) CreateSession(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[name]; ok {
		return fmt.Errorf("session %q already exists", name)
	}
	m.sessions[name] = NewSession(name)
	return nil
}

// Session returns the session with that name, or nil if there is none.
func (m *Manager) Session(name string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[name]
}

// SessionExists reports whether a session has that name.
func (m *Manager) SessionExists(name string) bool { return m.Session(name) != nil }

// KillSession removes the session and kills every window in it. It reports
// whether there was a session by that name; tmux's kill-session fails for a
// session it cannot find, and the daemon answers 404 for it.
func (m *Manager) KillSession(name string) bool {
	m.mu.Lock()
	s := m.sessions[name]
	delete(m.sessions, name)
	m.mu.Unlock()
	if s != nil {
		s.Kill()
	}
	return s != nil
}

// ListSessions returns every session sorted by name, the order tmux's
// list-sessions shows.
func (m *Manager) ListSessions() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// MoveWindow re-parents a window between sessions without touching its
// process. An empty dstName keeps the name.
//
// Two things must happen after the source window has been found, and that is the
// contract rather than an implementation detail, because each of the other orders
// is wrong in a way a caller can see. A missing destination session created before
// the lookup leaves an empty session behind whenever the move then fails,
// reachable from the daemon's move endpoint with nothing but a typo in srcName.
// The no-op returning before the lookup answers nil — which that endpoint maps to
// 200 — for a window the session does not hold.
//
// Both of those are about position relative to the LOOKUP. The create's position
// relative to the no-op is not part of the contract and is not observable: the
// no-op requires srcSession == dstSession, so on that path the destination is the
// source, which was found above, and nothing is created whichever side of the
// no-op the create sits on. Swapping just those two is behaviour-preserving. The
// other ordering in this method that a caller can see is the collision check
// before the detach, which is the last paragraph below.
//
// The reference daemon's move_window has both of the defects above, so this is a
// deliberate divergence and not a port of it. Restoring parity by reverting this
// would reintroduce them.
//
// Both sessions are locked for the whole swap, and the destination name is
// checked before the window is detached, so there is no rollback and no interval
// in which the window is in neither session.
//
// An earlier version detached first and undid the detach on a collision, and the
// undo was a write of src.windows[srcName] performed after src.mu had been
// released. m.mu does not cover that: Session.CreateWindow takes only s.mu (the
// daemon's create endpoint calls it on a *Session directly and never goes through
// this Manager), so a create of the same name could land in that interval and the
// undo would overwrite it — dropping a live window, with its shell, its PTY, its
// cgroup and its four goroutines, out of every map that can reach it, while the
// client that created it held a name that answered 404. Holding both locks is what
// makes that unrepresentable rather than unlikely.
//
// Deadlock is not a concern from taking two session locks: m.mu is held for the
// whole move so no second move can interleave, no Session method takes any lock
// but its own, and a Window cannot reach the session that holds it. src == dst is
// locked once.
func (m *Manager) MoveWindow(srcSession, srcName, dstSession, dstName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	src, ok := m.sessions[srcSession]
	if !ok {
		return fmt.Errorf("source session %q not found", srcSession)
	}

	// The source window is resolved first, before anything is created and before
	// the no-op below, because each of those used to run for a window nobody had
	// checked was there -- and they went wrong differently. The no-op answered:
	// it returned nil for a window that was not there. The create did not answer,
	// it acted: it allocated a session and then fell through to the error, leaving
	// the session behind. A move naming a window the session does not hold is a
	// caller error whatever the destination says, so it is decided first.
	src.mu.Lock()
	defer src.mu.Unlock()
	w, ok := src.windows[srcName]
	if !ok {
		return fmt.Errorf("window %q not found in session %q", srcName, srcSession)
	}

	// Moving a window onto its own session under its own name is a no-op and
	// reporting success for it is right -- but only now that the window is known
	// to exist. Reached before the lookup, this returned nil for any srcName at
	// all, and the route maps nil to 200: a caller who misspelled the window got
	// a success for a window the daemon did not have, and nothing else in the
	// response could tell them otherwise.
	if srcSession == dstSession && (dstName == "" || dstName == srcName) {
		return nil
	}

	// The destination is created only once the move is known to be possible.
	// Created before the checks above, a source that did not resolve left an
	// empty session behind that nothing removes and no caller asked for. The
	// only error remaining below is the collision, and a session created here
	// holds no windows, so it cannot be the one that collides -- which is what
	// makes "created" and "returned an error" mutually exclusive rather than
	// merely unlikely.
	dst, ok := m.sessions[dstSession]
	if !ok {
		dst = NewSession(dstSession)
		m.sessions[dstSession] = dst
	}
	if dst != src {
		dst.mu.Lock()
		defer dst.mu.Unlock()
	}

	finalName := dstName
	if finalName == "" {
		finalName = srcName
	}
	if _, exists := dst.windows[finalName]; exists {
		return fmt.Errorf("window %q already exists in destination session %q", finalName, dstSession)
	}
	// A rename within the session keeps the window's index; a move to another
	// session takes the lowest free index there, as tmux's move-window does
	// without -t index.
	idx, keep := src.index[srcName]
	src.remove(srcName)
	w.setName(finalName)
	dst.add(finalName, w)
	if dst == src && keep {
		dst.index[finalName] = idx
	}
	return nil
}

// KillAll empties the registry and kills every window in every session. It
// clears the map before doing any killing, so a session created afterwards is
// not caught by a kill that was already under way.
//
// It is for a program that embeds this package and owns the lifetime of its
// windows. The daemon deliberately does not call it when it shuts down, and the
// absence is the decision rather than an omission: a window's own process is the
// session leader of its pty, so closing the master hangs it up without shelld
// killing anything, and what a kill here would additionally reach is a job that
// has setsid'd out of that session — which shutdown cannot tell apart from one
// someone detached on purpose. tmux's kill-server leaves that job running too.
// cmd/shelld's TestStopLeavesADetachedJobRunningAsTmuxDoes holds the measurement,
// and CloseCgroups says the same thing about the cgroup the job is still in.
func (m *Manager) KillAll() {
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()
	for _, s := range sessions {
		s.Kill()
	}
}
