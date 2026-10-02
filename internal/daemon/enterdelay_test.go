package daemon

import (
	"fmt"
	"math"
	"testing"
)

// H13: an enter_delay_ms past a minute is refused with a 400 naming the bound,
// on both daemons, rather than clamped and answered 200 for a delay that was
// never performed. 60000 itself passes the check and reaches the window, which
// is a fake one with no writer here, hence its 500.
func TestKeysRefusesAnEnterDelayPastAMinute(t *testing.T) {
	_, h := api(t, map[string][]string{"s": {"w"}})
	const path = "/sessions/s/windows/w/keys"
	const want = `{"error":"enter_delay_ms must be at most 60000"}`
	for _, ms := range []uint64{60001, 600000, math.MaxUint64} {
		st, body := call(t, h, "POST", path, fmt.Sprintf(`{"keys":"x","enter_delay_ms":%d}`, ms))
		if st != 400 || body != want {
			t.Errorf("enter_delay_ms %d: %d %s, want 400 %s", ms, st, body, want)
		}
	}
	for _, ms := range []uint64{0, 60000} {
		if st, body := call(t, h, "POST", path, fmt.Sprintf(`{"keys":"x","enter_delay_ms":%d}`, ms)); st != 500 {
			t.Errorf("enter_delay_ms %d: %d %s, want it past the bound check (500 from the fake window)", ms, st, body)
		}
	}
}
