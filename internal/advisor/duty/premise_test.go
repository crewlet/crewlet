package duty_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
)

// A FINDING WHOSE PREMISE NO LONGER HOLDS IS RESOLVED on the next rules run,
// and one whose premise holds stays; the run records that it asked them.
// Mutation: leave a model finding until a later pass omits it, and a finding
// about an item that was done stays open for a day.
func TestAFindingWhosePremiseNoLongerHoldsIsResolved(t *testing.T) {
	t.Parallel()
	gone, kept := openFinding(t, "gone", advisor.SeverityCosts, "dev", start), openFinding(t, "kept", advisor.SeverityCosts, "qa", start)
	// AN INVESTIGATION'S FINDINGS, which no rule run re-derives.
	for _, s := range []*advisor.Stored{&gone, &kept} {
		s.Observation.Finding.Class, s.Observation.Finding.Source = advisor.ClassModel, advisor.SourceModel
	}
	gone.Observation.Finding.Premise = &advisor.Premise{Read: "work_items", Path: "items[0].open", Op: advisor.PremiseGe, Value: 1}
	kept.Observation.Finding.Premise = &advisor.Premise{Read: "seat_activity", Path: "turns", Op: advisor.PremiseGe, Value: 1}
	l := &fakeLog{live: []advisor.Stored{gone, kept}}
	r := &fakeReader{signature: "r1", premises: map[string]bool{"work_items": false, "seat_activity": true}}
	step(t, newDuty(l, r, nil, &clock{now: start}))
	var closed []string
	for _, tr := range l.recorded {
		if tr.Step == advisor.StepClose && tr.Reason == advisor.ReasonConditionCleared {
			closed = append(closed, tr.Stored.ID)
		}
	}
	if len(closed) != 1 || closed[0] != "gone" {
		t.Errorf("closed %v, want the finding whose premise no longer holds", closed)
	}
	if len(l.runs) != 1 || l.runs[0].Scopes[len(l.runs[0].Scopes)-1].Scope != duty.PremiseScope {
		t.Errorf("the run's scopes = %+v, want the premises' check recorded", l.runs[0].Scopes)
	}
}
