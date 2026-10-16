package shelld

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Session is a named set of windows, keyed by window name. It owns the windows
// in it: Kill and KillWindow kill the process behind the window, and a window
// removed from a session is gone unless a Manager moved it to another one.
//
// All of its methods are safe to call from several goroutines.
type Session struct {
	// Name identifies the session within its Manager. It is set once, at
	// construction, and is not guarded by the session's lock.
	Name string

	mu      sync.RWMutex
	windows map[string]*Window
	// index holds each window's position in the session, keyed like windows.
	// A window takes the lowest index no other window holds, as a tmux window
	// does with base-index 0, and the lists are in index order.
	index map[string]int
	// killed is set by Kill. A create that spawned before the kill took the
	// lock must not add its window afterwards: the Manager has already dropped
	// this session, so nothing could reach or kill that window again.
	killed bool
}

// ErrSessionKilled is CreateWindow's error when the session was killed while
// the window was being spawned. The window it spawned is killed.
var ErrSessionKilled = errors.New("session was killed")

// NewSession returns an empty session with that name. A Manager makes these
// itself; construct one directly only to use a session without a Manager.
func NewSession(name string) *Session {
	return &Session{Name: name, windows: make(map[string]*Window), index: make(map[string]int)}
}

// add puts w in the session under name at the lowest free index. The caller
// holds s.mu and has checked that name is free.
func (s *Session) add(name string, w *Window) {
	used := make(map[int]bool, len(s.index))
	for _, i := range s.index {
		used[i] = true
	}
	i := 0
	for used[i] {
		i++
	}
	s.windows[name] = w
	s.index[name] = i
}

// remove takes name out of the session. The caller holds s.mu.
func (s *Session) remove(name string) {
	delete(s.windows, name)
	delete(s.index, name)
}

// sortedNames returns the window names in index order. The caller holds s.mu.
func (s *Session) sortedNames() []string {
	out := make([]string, 0, len(s.windows))
	for name := range s.windows {
		out = append(out, name)
	}
	sort.Slice(out, func(a, b int) bool { return s.index[out[a]] < s.index[out[b]] })
	return out
}

// CreateWindow starts a window and adds it under opts.Name, which must not
// already be taken. On a session that has been killed it returns
// ErrSessionKilled.
//
// The process is spawned before the name is claimed, because spawning forks and
// opens a PTY and the session's lock should not be held across that. Two
// concurrent creates of the same name therefore both spawn, and the one that
// loses the second check kills its window asynchronously and returns the same
// "already exists" error it would have returned had it lost the first. The cost
// is one short-lived process on a race a caller could avoid; the alternative is
// serialising every create in the session behind the slowest one.
func (s *Session) CreateWindow(opts WindowOpts) (*Window, error) {
	s.mu.RLock()
	_, exists := s.windows[opts.Name]
	killed := s.killed
	s.mu.RUnlock()
	if killed {
		return nil, ErrSessionKilled
	}
	if exists {
		return nil, &NameInUseError{Name: opts.Name}
	}
	w, err := NewWindow(opts)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.killed {
		go w.Kill()
		return nil, ErrSessionKilled
	}
	if _, exists := s.windows[opts.Name]; exists {
		go w.Kill()
		return nil, &NameInUseError{Name: opts.Name}
	}
	s.add(opts.Name, w)
	return w, nil
}

// NameInUseError is CreateWindow's error for a name the session already has a
// window under. Its text says "already exists", which is what
// clients match to treat the create as "attach to the live one".
type NameInUseError struct{ Name string }

func (e *NameInUseError) Error() string { return fmt.Sprintf("window %q already exists", e.Name) }

// Window returns the window with that name, or nil if there is none. The window
// it returns may already have exited; read Window.Done or Window.ExitStatus to
// tell.
func (s *Session) Window(name string) *Window {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.windows[name]
}

// KillWindow removes the window and kills it, waiting for the kill to finish. It
// reports whether the session had a window by that name; tmux's kill-window
// fails for a window it cannot find, and the daemon answers 404 for it.
func (s *Session) KillWindow(name string) bool {
	s.mu.Lock()
	w := s.windows[name]
	s.remove(name)
	s.mu.Unlock()
	if w != nil {
		w.Kill()
	}
	return w != nil
}

// ListWindows returns every window in the session in index order, which is the
// order tmux's list-windows shows. The slice is a snapshot; the windows in it
// are live.
func (s *Session) ListWindows() []*Window {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := s.sortedNames()
	out := make([]*Window, len(names))
	for i, name := range names {
		out[i] = s.windows[name]
	}
	return out
}

// WindowNames returns the names of every window in the session, in index order.
func (s *Session) WindowNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sortedNames()
}

// Kill empties the session and kills every window in it, in parallel, and
// returns once they are all down. The session takes no windows afterwards:
// CreateWindow returns ErrSessionKilled, including for a create whose spawn was
// already under way when Kill started.
func (s *Session) Kill() {
	s.mu.Lock()
	s.killed = true
	windows := s.windows
	s.windows = make(map[string]*Window)
	s.index = make(map[string]int)
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, w := range windows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Kill()
		}()
	}
	wg.Wait()
}

// InsertFakeWindow adds a window with no PTY and no process behind it, under
// that name, replacing any window already there without killing it.
//
// It exists for tests that need a session to contain something without paying
// for a fork. See NewFakeWindow for what such a window can and cannot do. It is
// exported because the daemon's tests are in another package and need it; it is
// not part of the API a program should build on.
func (s *Session) InsertFakeWindow(name string) {
	s.mu.Lock()
	s.remove(name)
	s.add(name, NewFakeWindow(name))
	s.mu.Unlock()
}
