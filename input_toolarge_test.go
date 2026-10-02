package shelld

// The two ways an input queue says no, at the level of the queue rather than of
// a window: the caller that has to tell them apart is the injection drain loop
// (injectnoroom_pty_test.go), and it can only do so if push distinguishes them in
// the first place. No PTY here, so this runs on every platform.

import (
	"errors"
	"testing"
)

func TestPushTellsAWriteThatWillNeverFitFromOneThatDoesNotFitYet(t *testing.T) {
	// Nothing drains these queues -- there is no writer goroutine -- so bytes
	// pushed stay counted, which is the wedged program this bound exists for.
	const limit = 1024

	t.Run("the window's own limit", func(t *testing.T) {
		q := newInputQueue(limit, NewInputBudget(1<<20))
		if err := q.push(inputChunk{data: make([]byte, limit+1)}); !errors.Is(err, ErrInputTooLarge) {
			t.Errorf("a write one byte past the limit: %v, want ErrInputTooLarge", err)
		}
		// Exactly the limit fits, so the boundary is not off by one in the
		// direction that would refuse a legitimate write for ever.
		if err := q.push(inputChunk{data: make([]byte, limit)}); err != nil {
			t.Fatalf("a write of exactly the limit into an empty queue: %v", err)
		}
		err := q.push(inputChunk{data: make([]byte, 1)})
		if !errors.Is(err, ErrInputQueueFull) {
			t.Fatalf("one byte into a full queue: %v, want ErrInputQueueFull", err)
		}
		if errors.Is(err, ErrInputTooLarge) {
			t.Error("one byte into a full queue read as too large: the program reading its input would make room for it, so a caller told this gives up on a write it could have retried")
		}
	})

	t.Run("the shared budget, which can be smaller than the window's limit", func(t *testing.T) {
		// WindowOpts sets InputQueueLimit and InputBudget independently, so a
		// window can be configured with a per-window limit the shared budget could
		// never fund. Without this half of the test the budget term in push is
		// unconstrained: the per-window term alone passes every case above.
		q := newInputQueue(1<<20, NewInputBudget(limit))
		if err := q.push(inputChunk{data: make([]byte, limit+1)}); !errors.Is(err, ErrInputTooLarge) {
			t.Errorf("a write past the whole budget but well inside the window's own limit: %v, want ErrInputTooLarge", err)
		}
	})

	t.Run("both still read as a full queue", func(t *testing.T) {
		// The wrapping relation, in both directions, which is what let the new error
		// be introduced without revisiting every caller: a caller outside the drain
		// loop only wants to know that the input was not accepted.
		//
		// What the relation costs is that it makes the ORDER of a caller's errors.Is
		// calls decide the answer, since a too-large error satisfies both tests. The
		// two callers that care ask the specific one first -- writeInputError, which
		// answers 413 and not 429 so a client is not told to retry a write no state
		// of the queue accepts, and drainLoop, which drops the payload instead of
		// requeueing it. internal/daemon/writeinputerror_test.go pins that ordering.
		//
		// The second assertion below is what keeps the ordering meaningful: if the
		// relation were made symmetric, a plain full queue would match too and every
		// full queue would take the specific arm.
		if !errors.Is(ErrInputTooLarge, ErrInputQueueFull) {
			t.Error("ErrInputTooLarge no longer matches ErrInputQueueFull, so callers that test only for a full queue stop recognising it")
		}
		if errors.Is(ErrInputQueueFull, ErrInputTooLarge) {
			t.Error("a plain full queue matches ErrInputTooLarge, which would send the drain loop to drop a payload it should retry")
		}
	})
}
