package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// THE PLAINTEXT BOUND IS TOO LARGE, NEVER A TAKE THAT FAILED: a take refused for
// expanding past what its scrub reads, however its error is wrapped on the way
// to the stamp, is stamped `too_large`, its reason naming the 256 MiB of
// plaintext; the archive's own bound keeps its sentence.
//
// Mutation: drop the plaintext case, or order it after the archive's, and the
// sentence names the wrong bound or the loss is keep_failed.
func TestThePlaintextBoundIsTooLarge(t *testing.T) {
	t.Parallel()
	scrubbed := fmt.Errorf("%w: executor: the session is past the bounds a kept session is scrubbed within",
		executorkeep.ErrSessionPlainTooLarge)
	spooled := errors.Join(fmt.Errorf("sealing: %w", scrubbed), nil)
	loss, why := takeRefusal(t.Context(), spooled, textBound.take)
	if loss != executorkeep.LossTooLarge || !strings.Contains(why, "256 MiB of plaintext") {
		t.Errorf("the plaintext bound came to %s: %q, want too_large naming 256 MiB of plaintext", loss, why)
	}
	loss, why = takeRefusal(t.Context(), fmt.Errorf("sealing: %w", executorkeep.ErrSessionTooLarge), textBound.take)
	if loss != executorkeep.LossTooLarge || !strings.Contains(why, "48 MiB of text and 94 MiB in all") ||
		!strings.Contains(why, "sealing") {
		t.Errorf("the archive's bound came to %s: %q, want too_large naming both bounds and what refused it",
			loss, why)
	}
}

// A TAKE REFUSED FOR ITS TIME NAMES ITS OWN BUDGET: a take whose time ran out
// says the budget its own bound gave it, eight minutes at the text bound, ten
// at the kept bound, never the kept bound's for every take.
//
// Mutation: name SessionTakeBudget whatever the take's bound, and the text
// case goes red.
func TestATakeRefusedForItsTimeNamesItsOwnBudget(t *testing.T) {
	t.Parallel()
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	for _, bound := range []takeBound{textBound, keptBound} {
		loss, why := takeRefusal(ended, errors.New("the box's stream stalled"), bound.take)
		if loss != executorkeep.LossKeepFailed || !strings.Contains(why, "("+bound.take.String()+" at its bound)") {
			t.Errorf("a take whose time ran out at %v came to %s: %q, want keep_failed naming its budget", bound.take,
				loss, why)
		}
	}
}
