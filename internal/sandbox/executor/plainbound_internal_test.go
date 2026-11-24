package executor

import (
	"errors"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// A TAKE PAST THE SCRUB'S PLAINTEXT IS A SESSION TOO LARGE: its error is the
// plaintext bound's, a size bound's like the archive's, still the scrub's own;
// any other passes as it is.
//
// Mutation: leave the scrub's error unwrapped, or wrap every error, and a
// check goes red.
func TestThePlaintextBoundIsTooLarge(t *testing.T) {
	t.Parallel()
	err := plainTooLarge(fmt.Errorf("%w: it expands past %d bytes", errScrubBound, scrubPlainMax))
	if !errors.Is(err, executorkeep.ErrSessionPlainTooLarge) || !errors.Is(err, executorkeep.ErrSessionTooLarge) ||
		!errors.Is(err, errScrubBound) {
		t.Errorf("the scrub's bound came to %v, want the plaintext bound's, a session too large", err)
	}
	other := errors.New("the box went away")
	if got := plainTooLarge(other); !errors.Is(got, other) || errors.Is(got, executorkeep.ErrSessionTooLarge) ||
		plainTooLarge(nil) != nil {
		t.Errorf("another error came to %v, want it as it was", got)
	}
}
