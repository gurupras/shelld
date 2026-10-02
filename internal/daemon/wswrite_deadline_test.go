//go:build linux || darwin

package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/gurupras/shelld"
	"github.com/gurupras/shelld/ws"
)

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s", what, d)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A client that stops reading is not a client that disconnects. Its socket buffer
// fills, the daemon's next write to it blocks, and it blocks while holding that
// connection's write mutex -- so nothing above can recover, because the goroutine
// that would notice is the one parked in the write.
//
// What makes it more than a leaked goroutine is the deferred Attacher.Release
// that never runs. The effective size is min over voting attachers, so the wedged
// client keeps voting for as long as the daemon lives, and every other client's
// grid is held at the wedged client's size. That is the consequence asserted
// here, rather than the attacher count, because it is the one a user sees: the
// second client asked for 1024 columns and gets 500 because someone else stopped
// reading.
//
// The two arms differ in one factor, the write bound, and each is the other's
// control. A pointer to zero is not an absent setting -- it is how a caller asks
// for no deadline -- so the second arm is the pre-fix daemon, reached through the
// variable rather than through an old binary. Without it a green first arm would
// only say the wedger went away, not that the bound is what removed it.
//
// Which is why the shell must not be allowed to exit. When the tick loop returns
// it sends a close and gives the client ten seconds to answer, after which the
// read loop errors and releases the attacher -- so a shell that ran out of work
// releases a wedged client too, and does it on a timescale a test would mistake
// for the write bound. An earlier draft of this test looped a fixed number of
// times and released at 11.1s against a bound of 1s; the loop here never ends,
// and the window is killed by bootWith's cleanup.
//
// The second arm on its own would be weak -- "still voting" is also true of a
// daemon that never blocked -- but it is not on its own: the first arm releases
// from the same setup, so the release belongs to the bound and not to anything
// else here.
func TestAClientThatStopsReadingIsDroppedRatherThanPinningEveryOtherClient(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout uint64
		bounded bool
	}{
		{"write bounded", 1000, true},
		{"write unbounded", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var st *State
			ms := tc.timeout
			sock := bootWith(t, func(s *State) {
				st = s
				s.WSWriteTimeoutMS = &ms
			})
			openWindow(t, sock, "s")
			win := st.Mgr.Session("s").Window("w")

			// Drained for the whole test: attachAt's channel is bounded, so a test
			// that stopped taking from it would turn its own control client into a
			// second wedged one and there would be nothing left to compare against.
			healthy, hch := attachAt(t, sock, "s", shelld.MaxCols, shelld.MaxRows)
			go func() {
				for range hch {
				}
			}()

			// Votes in its opening frame and then never reads another byte. Its 500
			// columns against the healthy client's 1024 is the harm, and the grid being
			// this large is how the wedge is reached in a dozen frames rather than a few
			// thousand: a repaint of 500x1024 dense cells is hundreds of kilobytes, and
			// a Unix socket takes about 180 before a write blocks. A small grid does not
			// fail to wedge, it just takes minutes of output to get there -- which is a
			// fact about how long the test would have to run, not about the defect.
			wedged := openWS(t, sock, "s")
			if err := wedged.WriteMessage(ws.OpText, resizeCtl(500, shelld.MaxRows)); err != nil {
				t.Fatal(err)
			}
			// The size is what says the wedger registered and is voting; without it a
			// later 1024 could mean it never arrived rather than that it left.
			waitSize(t, sock, "s", [2]int{500, shelld.MaxRows})
			if n := win.AttacherCount(); n != 2 {
				t.Fatalf("setup: %d attachers, want 2", n)
			}

			// Dense output, and unending: every line fills a row of the grid, so each
			// frame is a repaint rather than a diff of a few cells, and the shell is
			// never done. Sent through the healthy client, so the shell's input does
			// not depend on the connection under test. Measured at about 520 kB
			// reaching an attacher in eight seconds, against the ~180 kB that fits in
			// the buffer.
			if err := healthy.WriteMessage(ws.OpBinary, []byte(
				"while :; do echo \""+strings.Repeat("abcdefghij", 50)+"\"; done\n",
			)); err != nil {
				t.Fatal(err)
			}

			if tc.bounded {
				// Generous: the release comes at the bound, since a failed write stops
				// the read loop at once, and this test does not own the timings.
				waitFor(t, 40*time.Second, "the wedged client being dropped", func() bool {
					return win.AttacherCount() == 1
				})
				waitSize(t, sock, "s", [2]int{shelld.MaxCols, shelld.MaxRows})
				return
			}

			// The negative arm: with no bound the write never returns, so the vote is
			// still there. It has to outlast the other arm's release, or a reader can
			// answer it with "you did not wait long enough". A failed write ends the
			// tick loop and stops the read loop at once, which releases the attacher;
			// before that stop it was released only after the ten seconds a client is
			// given to answer a close, 11.04s for a bound of one second, and 20s still
			// outlasts that.
			time.Sleep(20 * time.Second)
			if n := win.AttacherCount(); n != 2 {
				t.Fatalf("the unbounded client was released after all (%d attachers), so the arm above measures something other than the write bound", n)
			}
			if got := effectiveSize(t, sock, "s"); got != [2]int{500, shelld.MaxRows} {
				t.Fatalf("effective size %v, want 500x1024: the unbounded wedger stopped voting without the write returning", got)
			}
			// Nothing here reads from wedged to show the buffer is over-full, which
			// would be the direct evidence that a write had to wait: draining it
			// unblocks the write being measured, after which any number of bytes can be
			// counted and the count says nothing. What carries that weight instead is
			// the arm above -- the same setup, differing only in the bound, releases the
			// attacher -- so the release cannot be a property of the setup.
		})
	}
}

// The bound the daemon ships with, which no other test in this file can see: they
// all set the field, so the default is free to be zero -- and a zero default is
// the fix switched off for every real client while the suite stays green.
func TestTheShippedWriteBoundIsNotZero(t *testing.T) {
	st := NewState("/tmp/does-not-need-to-exist.sock")
	if st.WSWriteTimeoutMS != nil {
		t.Fatalf("SHELLD_WS_WRITE_TIMEOUT_MS is set in this environment (%d), so this says nothing about the default", *st.WSWriteTimeoutMS)
	}
	if got := st.wsWriteTimeout(); got <= 0 {
		t.Errorf("the default write bound is %v, so an unconfigured daemon blocks for ever on a client that stops reading", got)
	}

	t.Setenv("SHELLD_WS_WRITE_TIMEOUT_MS", "250")
	if got := NewState("/tmp/does-not-need-to-exist.sock").wsWriteTimeout(); got != 250*time.Millisecond {
		t.Errorf("the environment asked for 250ms and the state answers %v", got)
	}
}
