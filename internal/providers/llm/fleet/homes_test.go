package fleet_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
)

// TestLedgerHomesAreEachJobsLastTakeNewestFirst holds what a node publishes of
// its homes: one per seat job, the member that last took its work and when,
// the most recently taken first, so a bounded record keeps the active ones.
func TestLedgerHomesAreEachJobsLastTakeNewestFirst(t *testing.T) {
	now := t0
	l := fleet.NewLedger(func() time.Time { return now })
	f := fleet.Fleet{Key: "opus", Strategy: fleet.Balanced, Threshold: 5,
		Members: []fleet.Member{plan("a", 10, 1), plan("b", 30, 1)}}
	engineer := fleet.Job{Fleet: "opus", Subject: "engineer", Job: "execute"}
	writer := fleet.Job{Fleet: "opus", Subject: "writer", Job: "review"}
	l.Walk(f, engineer)[0].Take()
	now = t0.Add(time.Minute)
	l.Walk(f, writer)[1].Take()
	now = t0.Add(2 * time.Minute)
	l.Walk(f, engineer)[1].Take()
	got := l.Homes()
	want := []fleet.Home{
		{Job: engineer, Member: "b", At: t0.Add(2 * time.Minute)},
		{Job: writer, Member: "b", At: t0.Add(time.Minute)},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Homes = %+v, want %+v", got, want)
	}
	if l.Home(engineer) != "b" {
		t.Errorf("Home(engineer) = %q, want b", l.Home(engineer))
	}
}

// THE LEDGER'S WORK IN FLIGHT IS WHAT RUNS NOW: each begin is counted until
// its end, a job running twice on a member stays from its first begin until
// both end, an end is idempotent, and the node is told each change.
func TestLedgerRunningIsTheWorkInFlight(t *testing.T) {
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	l := fleet.NewLedger(func() time.Time { return now })
	changes := 0
	l.OnRunning(func() { changes++ })
	review := fleet.Job{Fleet: "opus", Subject: "engineer", Job: "review"}
	first := l.Begin(review, "a")
	now = now.Add(time.Minute)
	second := l.Begin(review, "a")
	pinned := l.Begin(fleet.Job{Subject: "writer", Job: "triage"}, "solo")
	got := l.Running()
	if len(got) != 2 || got[0].Member != "a" || !got[0].Since.Equal(now.Add(-time.Minute)) ||
		got[1].Member != "solo" || got[1].Fleet != "" {
		t.Fatalf("running = %+v, want engineer's review on a since its first begin, then writer's pinned triage", got)
	}
	first()
	first()
	if got := l.Running(); len(got) != 2 {
		t.Errorf("one of two calls ended and the job left: %+v", got)
	}
	second()
	pinned()
	if got := l.Running(); len(got) != 0 {
		t.Errorf("running after every end = %+v, want none", got)
	}
	if changes != 6 {
		t.Errorf("told %d changes, want one per begin and per first end (6)", changes)
	}
}
