package shelld

import (
	"slices"
	"testing"
)

// Sessions list by name and windows by index, as tmux's list-sessions and
// list-windows do. A window takes the lowest free index, so one created after a
// kill fills the gap the kill left (tmux's winlink_next_index from base-index
// 0), and the order is not the creation order.
func TestListsAreInTmuxOrder(t *testing.T) {
	m := NewManager()
	for _, name := range []string{"m", "b", "z", "a"} {
		if err := m.CreateSession(name); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, s := range m.ListSessions() {
		got = append(got, s.Name)
	}
	if want := []string{"a", "b", "m", "z"}; !slices.Equal(got, want) {
		t.Fatalf("sessions %q, want %q", got, want)
	}

	s := m.Session("a")
	for _, name := range []string{"w0", "w1", "w2", "w3"} {
		s.InsertFakeWindow(name)
	}
	if want := []string{"w0", "w1", "w2", "w3"}; !slices.Equal(s.WindowNames(), want) {
		t.Fatalf("windows %q, want %q", s.WindowNames(), want)
	}
	if !s.KillWindow("w1") {
		t.Fatal("KillWindow of a window the session has reported false")
	}
	if s.KillWindow("w1") {
		t.Fatal("KillWindow of a window already killed reported true")
	}
	s.InsertFakeWindow("new")
	if want := []string{"w0", "new", "w2", "w3"}; !slices.Equal(s.WindowNames(), want) {
		t.Fatalf("after a kill and a create: %q, want %q", s.WindowNames(), want)
	}
	var wins []string
	for _, w := range s.ListWindows() {
		wins = append(wins, w.Name())
	}
	if want := []string{"w0", "new", "w2", "w3"}; !slices.Equal(wins, want) {
		t.Fatalf("ListWindows %q, want %q", wins, want)
	}

	// A rename keeps the window's index even with a lower one free, so the
	// window created after it fills the gap below it; a move takes the lowest
	// free index in the destination.
	s.KillWindow("new")
	if err := m.MoveWindow("a", "w2", "a", "renamed"); err != nil {
		t.Fatal(err)
	}
	s.InsertFakeWindow("fill")
	if want := []string{"w0", "fill", "renamed", "w3"}; !slices.Equal(s.WindowNames(), want) {
		t.Fatalf("after a rename and a create: %q, want %q", s.WindowNames(), want)
	}
	b := m.Session("b")
	b.InsertFakeWindow("x0")
	b.InsertFakeWindow("x1")
	b.KillWindow("x0")
	if err := m.MoveWindow("a", "w0", "b", ""); err != nil {
		t.Fatal(err)
	}
	if want := []string{"w0", "x1"}; !slices.Equal(b.WindowNames(), want) {
		t.Fatalf("destination after a move: %q, want %q", b.WindowNames(), want)
	}
	s.InsertFakeWindow("again")
	if want := []string{"again", "fill", "renamed", "w3"}; !slices.Equal(s.WindowNames(), want) {
		t.Fatalf("source after a move and a create: %q, want %q", s.WindowNames(), want)
	}

	if !m.KillSession("z") || m.KillSession("z") || m.KillSession("nope") {
		t.Fatal("KillSession must report true once for a session it has, then false")
	}
}
