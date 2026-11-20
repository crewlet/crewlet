package duty_test

import (
	"slices"
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

// A MODEL FINDING A RULE'S FINDING SAYS IS CLOSED AS A DUPLICATE on the rules
// run that observes the rule's finding. Mutation: leave it, and the list says
// one cause twice, once without numbers.
func TestAModelFindingARuleFindingSaysIsClosedAsADuplicate(t *testing.T) {
	t.Parallel()
	said := openFinding(t, "said", advisor.SeverityCosts, "pm", start)
	said.Observation.Finding.Class, said.Observation.Finding.Source = advisor.ClassModel, advisor.SourceModel
	said.Observation.Finding.Title = "pm's lead-fallback wakes did nothing."
	rule := advisor.Finding{Class: advisor.ClassWakeReasonWaste, Severity: advisor.SeverityCosts,
		Source: advisor.SourceEvidence, Subject: advisor.Subject{Kind: advisor.SubjectWake, Name: "lead_fallback"},
		Seats: []advisor.SeatShare{{Seat: "pm"}}, Fix: advisor.Fix{Kind: advisor.FixNone}}
	l := &fakeLog{live: []advisor.Stored{said}}
	r := &fakeReader{signature: "r1", observed: []advisor.Observation{
		advisor.Observe(rule, advisor.Span{From: start.Add(-advisor.Window), To: start}, "r1")}}
	step(t, newDuty(l, r, nil, &clock{now: start}))
	for _, tr := range l.recorded {
		if tr.Stored != nil && tr.Stored.ID == "said" {
			if tr.Step != advisor.StepClose || tr.State != advisor.StateRetired || tr.Reason != advisor.ReasonDuplicate {
				t.Errorf("the model finding took %+v, want closed as a duplicate", tr)
			}
			return
		}
	}
	t.Error("the model finding the rule's finding says was left open")
}

// A MOVED PREMISE ON WORK STILL BLOCKED IS REPLACED, NOT RESOLVED: an investigation's finding that a
// person holds the blockers of GP-233 and GP-234 loses its premise when the blockers change hands,
// while a rule's finding of the same run says both items are still blocked on nothing anyone holds.
// The model finding closes as replaced by it, never as cleared. Mutation: resolve on the premise
// alone, and the list says the blocked items are fine.
func TestAMovedPremiseOnWorkStillBlockedIsReplacedNotResolved(t *testing.T) {
	t.Parallel()
	held := openFinding(t, "held", advisor.SeverityCosts, "owner", start)
	held.Observation.Finding.Class, held.Observation.Finding.Source = advisor.ClassModel, advisor.SourceModel
	held.Observation.Finding.Title = "A person holds 3 open blockers on GP-233 and GP-234."
	held.Observation.Finding.Records = []advisor.Record{{Kind: advisor.RecordItem, ID: "GP-233"},
		{Kind: advisor.RecordItem, ID: "GP-234"}}
	held.Observation.Finding.Premise = &advisor.Premise{Read: "work_items", Path: "total", Op: advisor.PremiseGe, Value: 3}
	blocked := advisor.Finding{Class: advisor.ClassBlockedNoCause, Severity: advisor.SeverityCosts,
		Source: advisor.SourceEvidence, Subject: advisor.Subject{Kind: advisor.SubjectCompany},
		Records: []advisor.Record{{Kind: advisor.RecordItem, ID: "GP-233"}, {Kind: advisor.RecordItem, ID: "GP-234"}},
		Fix:     advisor.Fix{Kind: advisor.FixAskCrewlet, Options: []advisor.Option{}}}
	l := &fakeLog{live: []advisor.Stored{held}}
	r := &fakeReader{signature: "r1", premises: map[string]bool{"work_items": false}, observed: []advisor.Observation{
		advisor.Observe(blocked, advisor.Span{From: start.Add(-advisor.Window), To: start}, "r1")}}
	step(t, newDuty(l, r, nil, &clock{now: start}))
	var opened bool
	for _, tr := range l.recorded {
		if tr.Step == advisor.StepOpen && tr.Observed != nil && tr.Observed.Finding.Class == advisor.ClassBlockedNoCause {
			opened = true
		}
		if tr.Stored != nil && tr.Stored.ID == "held" && (tr.State != advisor.StateRetired || tr.Reason != advisor.ReasonReplaced) {
			t.Errorf("the model finding took %+v, want closed as replaced", tr)
		}
	}
	if !opened || !slices.ContainsFunc(l.recorded, func(tr advisor.Transition) bool {
		return tr.Stored != nil && tr.Stored.ID == "held"
	}) {
		t.Errorf("recorded %+v, want the blocked items' finding opened and the model finding closed", l.recorded)
	}
}
