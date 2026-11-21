package sandbox

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/jetstream"
)

// A JOB'S NEXT ANNOUNCEMENT IS OUT BEFORE ITS LAST CAN DEAD-LETTER, at
// production's cadence: the schedule's ceiling, the far end of the poll's
// jitter and that poll's bound fall under the least time the broker carries a
// failing message, so a store's silence never leaves a dead letter as the last
// copy. The broker is internal/queue/jetstream at its defaults (the memory twin
// redelivers at once and is no part of it); the interval is
// [DefaultPollInterval], which only a test changes.
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
