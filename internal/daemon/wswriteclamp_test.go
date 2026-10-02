package daemon

import (
	"math"
	"strconv"
	"testing"
	"time"
)

// No build tag, unlike wswrite_deadline_test.go: that file needs a booted daemon
// over a unix socket, and this needs neither, so pinning it to linux and darwin
// would leave the clamp untested on the platforms State is also built for.

// The bound arrives from the environment as a decimal string and envUint accepts
// anything ParseUint accepts at 64 bits, so a mistyped extra digit reaches the
// multiply. Past the range it produces an arbitrary duration rather than a large
// one, and the two arbitrary results are wrong in opposite directions: a negative
// is no deadline at all, because ws.Conn applies the timeout under
// `writeTimeout > 0`, which restores the wedge the setting exists to prevent; a
// positive sub-millisecond one drops every attached client on its first write that
// is not instant.
//
// Both are measured. Which of the two an input produces follows from the low bits
// of the product, not from the input's size, so the table carries a row for each
// and the positivity check at the bottom stands in for neither.
func TestWSWriteTimeoutClampsInsteadOfWrapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		ms   uint64
		want time.Duration
	}{
		// Zero stays the documented way to ask for no deadline: the clamp must not
		// turn the opt-out into the longest possible bound.
		{"zero is the opt-out", 0, 0},
		{"an ordinary bound", 1000, time.Second},
		{"the largest value that converts", maxWSWriteTimeoutMS, time.Duration(maxWSWriteTimeoutMS) * time.Millisecond},
		// Unclamped: about -2562047h, which ws.Conn reads as no deadline.
		{"just past the limit", maxWSWriteTimeoutMS + 1, math.MaxInt64},
		// Unclamped: 448.384µs, a deadline short enough to drop healthy clients.
		{"a value that wraps to a usable-looking bound", 18_446_744_073_710, math.MaxInt64},
		// Unclamped: -1ms.
		{"the largest uint64", math.MaxUint64, math.MaxInt64},
	} {
		ms := tc.ms
		st := &State{WSWriteTimeoutMS: &ms}
		if got := st.wsWriteTimeout(); got != tc.want {
			t.Errorf("%s: wsWriteTimeout() = %v for %d ms, want %v", tc.name, got, tc.ms, tc.want)
		}
	}

	// The side of zero ws.Conn reads, which is the one the negative wrap crossed.
	// Checked separately from the table because it is the consumer's test and not
	// the conversion's: a clamp that landed on any positive value would satisfy it.
	ms := uint64(math.MaxUint64)
	if got := (&State{WSWriteTimeoutMS: &ms}).wsWriteTimeout(); got <= 0 {
		t.Errorf("the clamped bound is %v, so ws.Conn applies no deadline and one unread client wedges every other", got)
	}
}

// The clamp is only worth having if the environment can reach it, and that is a
// fact about envUint rather than about wsWriteTimeout: a parser that rejected
// values above some ceiling would make the table above unreachable in production
// while leaving it green. So the value goes in as the string a caller would type.
func TestAMistypedWriteTimeoutReachesTheClampThroughTheEnvironment(t *testing.T) {
	for _, ms := range []uint64{maxWSWriteTimeoutMS + 1, 18_446_744_073_710, math.MaxUint64} {
		t.Setenv("SHELLD_WS_WRITE_TIMEOUT_MS", strconv.FormatUint(ms, 10))
		st := NewState("/tmp/does-not-need-to-exist.sock")
		if st.WSWriteTimeoutMS == nil {
			t.Fatalf("envUint rejected %d, so nothing in production reaches the clamp and the table's rows are unreachable rows", ms)
		}
		if *st.WSWriteTimeoutMS != ms {
			t.Fatalf("the environment's %d arrived as %d", ms, *st.WSWriteTimeoutMS)
		}
		if got := st.wsWriteTimeout(); got != time.Duration(math.MaxInt64) {
			t.Errorf("%d from the environment became %v, want the clamp", ms, got)
		}
	}
}

// The same question for the inject stall deadline, which goes through the same
// envUint and is the one whose wrap is worse: a negative duration is
// ExpireOlderThan's documented opt-out, so an operator asking for the longest
// possible deadline would silently get none.
//
// stallTimeout is unexported in the library, so what this can assert is the half
// of the path that lives here: envUint accepts a value past the range and hands it
// on unaltered, with nothing between the environment and the library clamping or
// rejecting it. The link after this one is WindowOpts.StallTimeoutMS reaching
// injectCfg, which TestStallTimeoutZeroKeepsThePayloadQueued pins for the value
// zero and for no other, and the conversion at the end of it is the stallTimeout
// table's. So the path is covered by three tests in two packages and by no single
// one, which is worth saying because the middle link is the weakest: it is
// asserted at one value.
func TestAMistypedStallTimeoutReachesTheLibraryThroughTheEnvironment(t *testing.T) {
	for _, ms := range []uint64{18_446_744_073_710, math.MaxUint64} {
		t.Setenv("SHELLD_INJECT_STALL_TIMEOUT_MS", strconv.FormatUint(ms, 10))
		st := NewState("/tmp/does-not-need-to-exist.sock")
		if st.StallTimeoutMS == nil {
			t.Fatalf("envUint rejected %d, so no operator can reach the clamp and its rows are unreachable rows", ms)
		}
		if *st.StallTimeoutMS != ms {
			t.Fatalf("the environment's %d arrived as %d", ms, *st.StallTimeoutMS)
		}
	}
	// And the parser really does refuse something, or the assertions above hold for
	// every string and say nothing about these two.
	t.Setenv("SHELLD_INJECT_STALL_TIMEOUT_MS", "18446744073709551616") // MaxUint64 + 1
	if st := NewState("/tmp/does-not-need-to-exist.sock"); st.StallTimeoutMS != nil {
		t.Errorf("envUint accepted a value past uint64 as %d", *st.StallTimeoutMS)
	}
}
