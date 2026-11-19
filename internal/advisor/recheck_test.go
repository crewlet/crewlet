package advisor_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
)

// capFix is a round-cap finding whose fix is one change to the seat.
func capFix(seat string, runs int64, cap int) advisor.Finding {
	f := roundCap(seat, runs)
	f.Fix = advisor.Fix{Kind: advisor.FixConfigChange, Sentence: "Raise its cap.", Options: []advisor.Option{{
		Label: "Raise the cap", Change: advisor.Change{Entity: advisor.EntityRoles, ID: seat, Set: map[string]any{},
			Merge: map[string]any{"sandbox": map[string]any{"max_turns": cap}}, Summary: "raise the cap"}}}}
	return f
}

// AN APPLY RE-CHECKS FIRST and answers one of four outcomes from what a run
// observes now: the same verdict and change holds; another change is shown
// before anything is sent; a condition that ended is resolved and a subject
// that left is not applicable, neither sent; a source that could not be read
// says nothing. Mutation: answer holds without comparing the fix, and the
// changed case reads as holds.
func TestApplyRechecksAndAnswersHoldsChangedResolvedOrNotApplicable(t *testing.T) {
	t.Parallel()
	s := storedAs(t, observed(capFix("dev", 3, 60)), advisor.StateOpen)
	run := evidenceRun()
	for _, c := range []struct {
		name     string
		observed []advisor.Observation
		held     func(advisor.Subject) bool
		want     advisor.RecheckOutcome
	}{
		{"the same finding and change", []advisor.Observation{observed(capFix("dev", 3, 60))}, nil, advisor.RecheckHolds},
		{"only its numbers moved", []advisor.Observation{observed(capFix("dev", 5, 60))}, nil, advisor.RecheckHolds},
		{"another change", []advisor.Observation{observed(capFix("dev", 3, 90))}, nil, advisor.RecheckChanged},
		{"the condition ended", nil, nil, advisor.RecheckResolved},
		{"another finding's observation", []advisor.Observation{observed(capFix("qa", 3, 60))}, nil, advisor.RecheckResolved},
		{"the seat left", nil, func(advisor.Subject) bool { return false }, advisor.RecheckNotApplicable},
	} {
		r := run
		r.Held = c.held
		got, step, err := advisor.Recheck(s, c.observed, r)
		if err != nil || got != c.want {
			t.Errorf("%s: %s (%v), want %s", c.name, got, err, c.want)
		}
		if c.want != advisor.RecheckHolds && step == nil {
			t.Errorf("%s: no step records it", c.name)
		}
	}
	unread := run
	unread.Unread = []string{advisor.UnreadRuns}
	if _, _, err := advisor.Recheck(s, nil, unread); !errors.Is(err, advisor.ErrUnconcluded) {
		t.Errorf("a re-check whose source was unread answered %v, want ErrUnconcluded", err)
	}
	applied := s
	applied.State = advisor.StateApplied
	if _, _, err := advisor.Recheck(applied, nil, run); err == nil {
		t.Error("an applied finding was re-checked for applying again")
	}
}

// THE SEND IS BOUND TO THE REVISION THE RE-CHECK READ: a finding revised
// since, or another option, is another digest.
func TestTheApplyDigestNamesTheRevisionAndTheOption(t *testing.T) {
	t.Parallel()
	o := capFix("dev", 3, 60).Fix.Options[0]
	other := capFix("dev", 3, 90).Fix.Options[0]
	d := advisor.ApplyDigest("f-1", 1, advisor.OptionDigest(o))
	if d == advisor.ApplyDigest("f-1", 2, advisor.OptionDigest(o)) || d == advisor.ApplyDigest("f-1", 1, advisor.OptionDigest(other)) ||
		d == advisor.ApplyDigest("f-2", 1, advisor.OptionDigest(o)) || d != advisor.ApplyDigest("f-1", 1, advisor.OptionDigest(o)) {
		t.Error("the apply digest does not bind the finding, its revision and the option")
	}
}

// AN APPLIED FIX THAT DID NOT CLEAR THE CONDITION IS NEVER OFFERED AGAIN: its
// option leaves the fix the next run observes, and a fix left with none is
// handed to Ask Crewlet. Mutation: keep the options, and the same change is
// offered again.
func TestAnAppliedFixThatDidNotClearTheConditionIsNeverOfferedAgain(t *testing.T) {
	t.Parallel()
	f := capFix("dev", 3, 60)
	tried := advisor.OptionDigest(f.Fix.Options[0])
	s := storedAs(t, observed(f), advisor.StateOpen)
	s.Ineffective = []string{tried}
	steps := advisor.Reconcile([]advisor.Stored{s}, []advisor.Observation{observed(f)}, evidenceRun())
	if len(steps) != 1 || steps[0].Step != advisor.StepRevise {
		t.Fatalf("steps %v, want the fix revised", stepsOf(steps))
	}
	if fix := steps[0].Observed.Finding.Fix; fix.Kind != advisor.FixAskCrewlet || len(fix.Options) != 0 || fix.Prompt == "" {
		t.Errorf("the fix after its only option did not take is %+v, want Ask Crewlet's", fix)
	}
	two := capFix("dev", 3, 60)
	two.Fix.Options = append(two.Fix.Options, capFix("dev", 3, 90).Fix.Options[0])
	if kept := advisor.WithoutIneffective(two, []string{tried}); len(kept.Fix.Options) != 1 ||
		advisor.OptionDigest(kept.Fix.Options[0]) == tried {
		t.Errorf("the options kept are %+v, want the one not tried", kept.Fix.Options)
	}
}

func stepsOf(ts []advisor.Transition) []advisor.Step {
	out := []advisor.Step{}
	for _, t := range ts {
		out = append(out, t.Step)
	}
	return out
}

// A CHANGE IS IN THE CONFIGURATION ONCE THE ENTITY HOLDS EVERY FIELD IT WRITES,
// a nested field merged beside the rest.
func TestAnEntityHoldsAChangeOnceItWritesNothingNew(t *testing.T) {
	t.Parallel()
	ch := capFix("dev", 3, 60).Fix.Options[0].Change
	before := map[string]any{"handle": "dev", "sandbox": map[string]any{"max_turns": 30, "memory": "4g"}}
	if holds, err := advisor.Holds(before, ch); err != nil || holds {
		t.Errorf("an entity at 30 rounds holds the change to 60: %v (%v)", holds, err)
	}
	after, err := advisor.Changed(before, ch)
	if err != nil {
		t.Fatal(err)
	}
	if sandbox, _ := after["sandbox"].(map[string]any); sandbox["memory"] != "4g" {
		t.Errorf("the change lost a field beside the one it writes: %v", after)
	}
	if holds, err := advisor.Holds(after, ch); err != nil || !holds {
		t.Errorf("the changed entity does not hold the change: %v (%v)", holds, err)
	}
}
