package evals

import (
	"testing"

	"github.com/crewlet/crewlet/internal/assistant"
)

// A TURN IS WAITED FOR AS LONG AS THE SERVICE LETS IT RUN, a scripted one as
// much as a model's: the eval reports a stream that never ended only once the
// service has broken its own bound on a turn, never because the machine was
// slow. A scripted turn's tools do real work (the first docs search builds the
// docs index, measured past 10 s under -race on a loaded two-core runner), so
// any wait shorter than the turn's own timeout fails a turn that is working.
// Mutation: wait a scripted turn a fixed 10 s, or exactly the turn's timeout
// with no room for the records of its end, and this fails.
func TestATurnIsWaitedForAsLongAsTheServiceLetsItRun(t *testing.T) {
	t.Parallel()
	if turnPatience <= assistant.TurnTimeout {
		t.Fatalf("a turn's stream is waited for %s, no longer than the %s the service "+
			"lets the turn itself run", turnPatience, assistant.TurnTimeout)
	}
}
