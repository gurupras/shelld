package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gurupras/shelld"
)

// writeInputError picks a status from an error, and the interesting part is that
// ErrInputTooLarge WRAPS ErrInputQueueFull, so one error satisfies both tests and
// the ORDER of the two arms decides the answer. That makes the two rows below each
// other's control rather than two independent cases: a table with only the
// queue-full row passes whichever order the arms are in, and a table with only the
// too-large row passes an implementation that answers 413 for every full queue.
// Together they pin the order, which is the whole of the function.
//
// The status matters because it is what a conforming client does next. 429 names a
// transient condition and invites a retry; for a write larger than the whole queue
// no state of the queue accepts it, so the retry cannot succeed at any later time
// and the client and the daemon are left in a loop. 413 says the request is the
// problem.
func TestWriteInputErrorDistinguishesAWriteThatWillNeverFit(t *testing.T) {
	// Premise: the wrapping this test is about is still in place. Without it the
	// too-large row below would pass for the wrong reason -- any single-arm
	// implementation would get it right -- so the ordering would stop being tested
	// and nothing here would say so.
	if !errors.Is(shelld.ErrInputTooLarge, shelld.ErrInputQueueFull) {
		t.Fatal("premise: ErrInputTooLarge no longer wraps ErrInputQueueFull, so the arm order this test exists to pin no longer decides anything")
	}

	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{
			// The specific case. Fails if the arms are swapped, since the general
			// test matches this error too.
			name: "a write larger than the whole queue",
			err:  shelld.ErrInputTooLarge,
			want: http.StatusRequestEntityTooLarge,
		},
		{
			// The general case, which must NOT be dragged to 413 by the fix. Fails if
			// the too-large arm is widened to every full queue.
			name: "a queue that is merely full right now",
			err:  shelld.ErrInputQueueFull,
			want: http.StatusTooManyRequests,
		},
		{
			// Wrapped once more, as a caller adding context would leave it: errors.Is
			// unwraps, so a route that compared with == instead would answer 500 here
			// and tell the client the daemon is broken.
			name: "a too-large error wrapped by a caller",
			err:  fmt.Errorf("sendKeys: %w", shelld.ErrInputTooLarge),
			want: http.StatusRequestEntityTooLarge,
		},
		{
			name: "a dead window",
			err:  errors.New(`window "w" is dead`),
			want: http.StatusInternalServerError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeInputError(rec, tc.err)
			if rec.Code != tc.want {
				t.Errorf("writeInputError(%v) = %d, want %d", tc.err, rec.Code, tc.want)
			}
			// The message is the only place a client learns which limit it hit, since
			// bodyTooLarge answers 413 for a body over the read limit as well.
			if body := rec.Body.String(); body == "" {
				t.Error("no message: the status alone does not say which limit was hit")
			}
		})
	}
}
