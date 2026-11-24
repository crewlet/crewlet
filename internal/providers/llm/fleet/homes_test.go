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
