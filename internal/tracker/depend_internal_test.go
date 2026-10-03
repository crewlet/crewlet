package tracker

import (
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A DEPENDENCY RESULT FOLDS EVERY COMMIT the sequence made into one answer:
// the latest position, the least certain outcome, and the version of the
// call's OWN task.
//
// The answer used to be whichever commit landed last, whole. That handed a
// caller the BLOCKER's version whenever a mirror landed after the authored
// edge — an If-Match with it refuses the caller's next edit of its own item —
// and reported an authored edge whose outcome was `unknown` as `applied`
// because a mirror after it was confirmed.
func TestADependencyResultFoldsEveryCommit(t *testing.T) {
	t.Parallel()
	at := func(seq uint64) statelog.Position {
		return statelog.Position{Stream: "S", Generation: 1, Seq: seq}
	}
	commit := func(o statelog.Outcome, seq uint64) WriteResult {
		r := WriteResult{Result: statelog.Result{Outcome: o}}
		if seq > 0 {
			r.Position, r.Version = at(seq), at(seq).Packed()
		}
		return r
	}

	var out DependencyResult
	out.fold(commit(statelog.OutcomeApplied, 10), true)  // the authored edge
	out.fold(commit(statelog.OutcomeApplied, 12), false) // the blocker's mirror
	if out.Version != at(10).Packed() {
		t.Errorf("version = %d, want the call's own task's %d rather than "+
			"the counterparty's", out.Version, at(10).Packed())
	}
	if out.Position != at(12) {
		t.Errorf("position = %s, want the later commit's", out.Position)
	}

	var masked DependencyResult
	masked.fold(commit(statelog.OutcomeUnknown, 0), true)
	masked.fold(commit(statelog.OutcomeApplied, 12), false)
	if masked.Outcome != statelog.OutcomeUnknown {
		t.Errorf("outcome = %q, want unknown: a confirmed mirror says "+
			"nothing about the authored edge before it", masked.Outcome)
	}

	var nothing DependencyResult
	nothing.fold(commit(statelog.OutcomePending, 12), false)
	nothing.fold(commit(statelog.OutcomeApplied, 0), true)
	if nothing.Outcome != statelog.OutcomePending || nothing.Version != 0 {
		t.Errorf("a commit that appended nothing moved the answer to %q "+
			"at version %d", nothing.Outcome, nothing.Version)
	}
}
