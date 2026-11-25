package statelog_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ownMeasuresAlarming is a reading holding only what a node measures of itself
// (its store's volume and write-ahead log, its pool's waits, its objective
// meter's burns), each past its alarm.
func ownMeasuresAlarming() statelog.Reading {
	fast := objectives.BurnWindows()[0]
	return statelog.Reading{
		StoreVolume: "/var/lib/crewlet", StoreBytes: 10 << 30, FreeBytes: 1 << 30,
		WALBytes: 2 << 30, WALFile: "node.db",
		PoolWaitP95: time.Second,
		ObjectiveBurns: []objectives.Burn{{Objective: objectives.SessionCarry, Window: fast,
			LongRate: 100, ShortRate: 100, LongBad: 10, LongEvents: 10}},
	}
}

// EVERY LOCAL RULE FIRES FROM THE NODE'S OWN MEASURES ALONE, both ways: what a
// node running no state log raises from its own measures is exactly the local
// kinds, the four a seat-only node can meet, and the whole table raises no
// other kind from them. A rule marked local that read a log's or the fleet's
// inputs would never fire on such a node; a rule left unmarked would never be
// judged there.
//
// Mutation: clear the flag on volume_low, or mark apply_lag, and a check fails.
func TestEveryLocalRuleFiresFromTheNodesOwnMeasuresAlone(t *testing.T) {
	t.Parallel()
	want := []statelog.Kind{statelog.KindVolumeLow, statelog.KindWALLarge, statelog.KindPoolStarved,
		statelog.KindObjectiveBurn}
	slices.Sort(want)
	local := slices.Sorted(slices.Values(statelog.LocalKinds()))
	if !slices.Equal(local, want) {
		t.Fatalf("the local kinds are %v, want %v", local, want)
	}
	r := ownMeasuresAlarming()
	raised := slices.Sorted(slices.Values(kindsOf(statelog.EvaluateLocal(r))))
	if !slices.Equal(raised, want) {
		t.Errorf("the node's own measures raise %v on its own table, want %v", raised, want)
	}
	whole := slices.Sorted(slices.Values(kindsOf(statelog.Evaluate(r))))
	if !slices.Equal(whole, want) {
		t.Errorf("the whole table raises %v from the node's own measures, want only %v", whole, want)
	}
	if got := statelog.EvaluateLocal(statelog.Reading{}); len(got) != 0 {
		t.Errorf("a node measuring nothing amiss raises %v", kindsOf(got))
	}
}

// A NODE'S OWN TABLE JUDGES NO OTHER RULE, however its reading reads: a node
// running no state log leaves every log's and the fleet's inputs at zero, and
// were one set, judging it there would be a second node raising what its own
// node raises.
//
// Mutation: evaluate the whole table in EvaluateLocal, and a check fails.
func TestEvaluateLocalJudgesNoOtherRule(t *testing.T) {
	t.Parallel()
	r := statelog.Reading{
		ApplyLag: time.Hour, ObjectsMissing: 3, ExecutorResumesBlocked: 1,
		ExecutorResumeBlockedSeat: "swe", ExecutorResumeBlockedNode: "sat-1",
		ExecutorResumeBlockedKind: "disk_full", ExecutorResumeBlockedFor: time.Hour,
	}
	others := []statelog.Kind{statelog.KindApplyLag, statelog.KindObjectsMissing,
		statelog.KindExecutorResumeBlocked}
	whole := kindsOf(statelog.Evaluate(r))
	for _, kind := range others {
		if !slices.Contains(whole, kind) {
			t.Fatalf("premise: the whole table raises %v, want %s among them", whole, kind)
		}
	}
	if got := statelog.EvaluateLocal(r); len(got) != 0 {
		t.Errorf("a node's own table raised %v from inputs only a data node holds", kindsOf(got))
	}
}
