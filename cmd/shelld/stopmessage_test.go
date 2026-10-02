//go:build unix

package main

import (
	"net"
	"os"
	"strings"
	"testing"
)

// "graceful shutdown failed" is true of only one of the two ways stop gets here,
// and it was printed for both. When the socket could not be dialled at all
// nothing was listening, so no shutdown was ever requested and none failed --
// the reader is then told a daemon ignored a request that was never sent, right
// before a sentence about signalling a process.
//
// The two cases differ in one factor, whether something is holding the socket,
// and each is the other's control: a fix that says "nothing answered" for both
// is as wrong as the one it replaces, and only the pair can tell them apart.
// Everything else is held equal -- same unreadable lockfile, so LocateDaemon
// answers unverified for the same reason both times and the message comes off
// the same line of cmdStop.
//
// The lockfile is a directory because that is the cheapest thing at that path
// that cannot be read as one. It reaches the unverified lane without a live
// process to name, so neither case signals anything.
func TestStopSaysWhetherAnythingAnsweredOnTheSocket(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listen bool
		want   string
		reject string
	}{
		{"nothing is listening", false, "nothing answered on", "graceful shutdown failed"},
		{"something holds the socket", true, "graceful shutdown failed", "nothing answered on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPaths(t)
			if err := os.Mkdir(p.pidfile, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.listen {
				// Bound and answering, but not with HTTP: the round trip dials,
				// writes, and then fails reading the response. That is the whole of
				// what separates this case from the one above.
				ln, err := net.Listen("unix", p.socket)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { ln.Close() })
				go func() {
					for {
						conn, err := ln.Accept()
						if err != nil {
							return
						}
						conn.Close()
					}
				}()
			}

			code, out := p.shelld(t, nil, "stop")
			if code != 1 {
				t.Errorf("exit %d, want 1: %s", code, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output does not say %q: %s", tc.want, out)
			}
			if strings.Contains(out, tc.reject) {
				t.Errorf("output says %q, which is the other case: %s", tc.reject, out)
			}
			// The sentence this one introduces, so the pair is pinned to the line
			// that prints both and not to whichever one happens to be reached.
			if !strings.Contains(out, "not signalled") {
				t.Errorf("output is not the unverified message: %s", out)
			}
		})
	}
}
