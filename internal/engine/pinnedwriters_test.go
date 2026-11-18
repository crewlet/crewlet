package engine_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
)

// THE SWEEP WRITES WHILE EVERY DECLARED PIN IS HELD. A pinned connection is
// declared, so the pool grows to hold it, and a shortfall is a refusal that
// never clears rather than contention: each apply loop takes its pin at boot
// and keeps it for life, so every declared pin is held before the first sweep.
// So housekeeping takes no pin (one sized from its job list would rest on the
// unenforced rule that a tick runs jobs in series): its jobs take a pooled
// write transaction, which reaches the same write lock, and `openStore`
// declares one pin per state-log domain and nothing else.
//
// Through a REAL TICK rather than against the constant: give either
// housekeeping job a pinned writer again and the tick comes back with that
// job's refusal (a tick joins every job's failure, so it errors either way).
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
