package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
)

// THE SWEEP WRITES WHILE EVERY DECLARED PIN IS HELD.
//
// A pinned connection is DECLARED, which is what makes the pool grow to hold
// it rather than the pin taking a reader's place — and the consequence is
// that a shortfall is not contention that clears under load, it is a refusal
// that never clears at all. Each apply loop takes its pin at boot and keeps
// it for the life of the loop, so every declared pin is gone before the first
// sweep runs: `tracker_notifications` failed on every tick from the first one
// with "declared 3 pinned writer(s) and 3 are held" while a company's inbox
// rows grew for the life of the deployment.
//
// THE ANSWER IS THAT HOUSEKEEPING TAKES NO PIN, which is what this now holds.
// It was one more declared pin, sized from the two jobs that took one — the
// pool in this package sized against a job list in another, on the unenforced
// invariant that a tick runs its jobs in series. Both jobs take a pooled
// write transaction now, which reaches the same write lock through the same
// queue, so `openStore` declares one pin per state-log domain and nothing
// else.
//
// Through a REAL TICK rather than against the constant, which is what keeps
// it falsifiable in the direction that matters: give either housekeeping job
// a pinned writer again and every declared pin is already held by an applier,
// so the tick comes back with that job's refusal. A tick reports every job's
// failure joined together, so it is an error here whichever job it was.
func TestTheSweepGetsAWriterWhileEveryDomainIsApplying(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})

	if _, err := e.Maintenance().Tick(t.Context()); err != nil {
		t.Fatalf("a sweep running beside this node's applying domains: %v\n"+
			"a housekeeping job is asking for a pinned writer: the apply loops "+
			"hold every declared pin for life, so a job that runs once a tick "+
			"has to take a pooled write transaction instead", err)
	}
}
