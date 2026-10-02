package main

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"testing/iotest"
)

// TestDetachSendsWhatWasTypedBeforeTheKey: the bytes read ahead of Ctrl-], in
// the same read, are sent before detaching; the key and what follows it are not.
func TestDetachSendsWhatWasTypedBeforeTheKey(t *testing.T) {
	for _, c := range []struct {
		name string
		r    io.Reader
		want []string
	}{
		{"same read", bytes.NewReader([]byte("ls\x1dafter")), []string{"ls"}},
		{"key first", bytes.NewReader([]byte("\x1dafter")), nil},
		{"earlier reads", iotest.OneByteReader(bytes.NewReader([]byte("ab\x1dc"))), []string{"a", "b"}},
		{"no key", bytes.NewReader([]byte("abc")), []string{"abc"}},
	} {
		var sent []string
		forwardInput(c.r, func(b []byte) error { sent = append(sent, string(b)); return nil })
		if len(sent) != len(c.want) {
			t.Errorf("%s: sent %q, want %q", c.name, sent, c.want)
			continue
		}
		for i := range sent {
			if sent[i] != c.want[i] {
				t.Errorf("%s: sent %q, want %q", c.name, sent, c.want)
			}
		}
	}
}

// A failed send ends forwarding, as it ended the attach before.
func TestForwardingStopsAtAFailedSend(t *testing.T) {
	calls := 0
	forwardInput(iotest.OneByteReader(bytes.NewReader([]byte("abc"))), func([]byte) error {
		calls++
		return errors.New("closed")
	})
	if calls != 1 {
		t.Errorf("%d sends after the first failed, want none", calls-1)
	}
}
