package pages

import (
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A WRITTEN REVISION IS THE RECORD'S PACKED POSITION, GENERATION AND ALL.
//
// [Written.Revision] has to be the integer the applier stamps into the row —
// [statelog.Position.Packed], which composes the generation above the
// sequence — or a caller comparing its write against a later read compares
// two number spaces. The round-trip harness cannot tell the two apart: it runs
// in generation zero, where the packed position and the bare sequence are the
// same integer, so a writer that reported the sequence alone would pass every
// case there and be wrong on the first log a reanchor moved on. This holds the
// rule over values, at a generation past zero, for both arms: a write whose
// record landed (applied or pending) answers that record's position, and one
// that published nothing answers the revision its decision read.
func TestAWrittenRevisionIsTheRecordsPackedPosition(t *testing.T) {
	t.Parallel()
	at := statelog.Position{Stream: Domain{}.Stream().Name, Generation: 3, Seq: 7}
	const read = 41
	for name, c := range map[string]struct {
		result statelog.Result
		want   uint64
	}{
		"applied": {statelog.Result{Outcome: statelog.OutcomeApplied, Position: at},
			uint64(at.Packed())},
		"pending": {statelog.Result{Outcome: statelog.OutcomePending, Position: at},
			uint64(at.Packed())},
		"published nothing": {statelog.Result{Outcome: statelog.OutcomeApplied}, read},
	} {
		if got := writtenRevision(c.result, read); got != c.want {
			t.Errorf("%s: the write reports revision %d, want %d", name, got, c.want)
		}
	}
	if uint64(at.Packed()) == at.Seq {
		t.Fatal("the fixture's generation packs to its own sequence, so this " +
			"case cannot tell the two number spaces apart")
	}
}
