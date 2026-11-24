package sandbox

import (
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
	loss, why := takeRefusal(t.Context(), spooled)
	if loss != executorkeep.LossTooLarge || !strings.Contains(why, "256 MiB of plaintext") {
		t.Errorf("the plaintext bound came to %s: %q, want too_large naming 256 MiB of plaintext", loss, why)
	}
	loss, why = takeRefusal(t.Context(), fmt.Errorf("sealing: %w", executorkeep.ErrSessionTooLarge))
	if loss != executorkeep.LossTooLarge || !strings.Contains(why, fmt.Sprint(executorkeep.SessionMaxBytes)) {
		t.Errorf("the archive's bound came to %s: %q, want too_large naming its bytes", loss, why)
	}
}
