package observe

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE LOOP RECONCILES EVERY INTERVAL AND STOPS WHEN TOLD, which is what makes
// a lost completion a half-minute's ghost rather than a permanent one.
func TestTheReconcileRunsEveryIntervalUntilStopped(t *testing.T) {
	t.Parallel()
	sink := &countingSink{}
	r := NewSandboxReconciler(emptyRuns{}, sink)
	r.interval = 5 * time.Millisecond
	r.Start(t.Context())
	deadline := time.Now().Add(5 * time.Second)
	for sink.n.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("reconciled %d times in five seconds at a 5ms interval", sink.n.Load())
		}
		time.Sleep(time.Millisecond)
	}
	r.Stop()
	after := sink.n.Load()
	time.Sleep(30 * time.Millisecond)
	if sink.n.Load() != after {
		t.Error("the loop reconciled after it was stopped")
	}
	r.Stop() // a second stop is a no-op, not a hang

	// A reconciler started again after a stop runs again, and stops again
	// — the shape a test harness that builds two surfaces in one process
	// takes, and the one under which the loop's own channel must not be
	// read off a field the stop has cleared.
	r.Start(t.Context())
	r.Stop()
}

type emptyRuns struct{}

func (emptyRuns) ListActive(context.Context) ([]sandbox.PendingRun, error) { return nil, nil }

type countingSink struct{ n atomic.Int64 }

func (s *countingSink) ReconcileSandboxes([]livestate.SandboxRecord, time.Time) { s.n.Add(1) }
