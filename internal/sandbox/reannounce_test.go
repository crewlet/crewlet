package sandbox

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/jetstream"
)

// A JOB'S NEXT ANNOUNCEMENT IS OUT BEFORE ITS LAST CAN DEAD-LETTER, at
// production's cadence. A completion whose claim this node's own store does
// not answer is handed back NAK'd and nothing renews it
// ([Coordinator.collectFailed]): that is safe because the waiter announces a
// running, unheld job's completion again on its schedule, each announcement a
// fresh message with a fresh budget, and because the latest the next one goes
// out after the last is under the least time the broker carries a failing
// message before its dead letter. That latest is the schedule's ceiling, the
// first poll at or past it at the far end of the poll's jitter, and that
// poll's own bound, which the announcement follows. So while the waiter's
// store lists the run's row, the completion always has a delivery in flight,
// and no dead letter a store's silence leaves is its last copy.
//
// The broker is the one production runs (internal/queue/jetstream at its
// defaults); the memory twin redelivers at once by design and is no part of
// it. The poll interval is [DefaultPollInterval], which production never
// changes (only a test sets the engine's SandboxPollInterval).
//
// Mutation: jetstream's NAK ceiling at 5 s, or reannounceCeilingPolls at 60,
// and the case is red.
func TestAJobsNextAnnouncementIsOutBeforeItsLastCanDeadLetter(t *testing.T) {
	t.Parallel()
	ceiling := reannounceCeilingPolls * DefaultPollInterval
	nextPoll := time.Duration(float64(DefaultPollInterval) * (1 + jitterFraction))
	latest := ceiling + nextPoll + pollBound
	if least := jetstream.LeastTimeToDeadLetter(); latest >= least {
		t.Fatalf("a job's next announcement can go out %v after its last (the schedule's %v ceiling, "+
			"a poll %v on and its %v bound), and the broker can dead-letter a failing message %v "+
			"after its first delivery: a store silent that long leaves a completion whose only copy "+
			"is its dead letter", latest, ceiling, nextPoll, pollBound, least)
	}
}
