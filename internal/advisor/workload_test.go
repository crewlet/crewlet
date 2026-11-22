package advisor_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/worklet"
)

// twinShape is the stall shape with a second engineer doing Dev's job.
func twinShape(t *testing.T) string {
	t.Helper()
	return replace(t, stallShape, "  - name: QA\n", "  - name: Dev 2\n    handle: dev-2\n    llm: p\n  - name: QA\n")
}

// ONE SEAT HOLDING ITS JOB'S WORK WHILE ITS TWIN IDLES is one finding, the twins inside, its fix
// moving the busy seat's work not yet started to the idle one; seats a few items apart, or busy
// alike, are none, and the same seat is not also said to hold the company's work.
// Mutation: drop the gap or the times bar, and the even pair raises a finding.
func TestASeatHoldingItsTwinsShareOfTheWorkIsMovedToIt(t *testing.T) {
	t.Parallel()
	held := func(handle string, open int, started []string) worklet.HeldOpen {
		return worklet.HeldOpen{Handle: handle, Open: open, Keys: started, NotStarted: len(started),
			NotStartedKeys: started}
	}
	todo := []string{"ENG-1", "ENG-2", "ENG-3", "ENG-4", "ENG-5"}
	ev := advisor.Evidence{Work: worklet.WorkPatterns{Open: worklet.OpenWork{
		Holders: []worklet.HeldOpen{held("dev", 27, todo), held("dev-2", 2, nil), held("qa", 1, nil)}}}}
	got := stalls(t, twinShape(t), ev, advisor.ClassWorkImbalance)
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want one", got)
	}
	f := got[0]
	if f.Subject.Seat != "dev" || f.Headline != "Dev holds 27 open items while Dev 2, on the same job, holds 2." ||
		len(f.Seats) != 2 || f.Seats[0].Seat != "dev" || f.Seats[1].Seat != "dev-2" || f.Seats[1].Value != 2 {
		t.Errorf("finding = %+v, want Dev's 27 against Dev 2's 2, both seats inside", f)
	}
	if r := f.Fix.Reassign; r == nil || r.To != "dev-2" || !slices.Equal(r.Items, todo) {
		t.Errorf("reassign = %+v, want Dev's five items not yet started moved to Dev 2", f.Fix.Reassign)
	}
	if len(stalls(t, twinShape(t), ev, advisor.ClassWorkConcentrated)) != 0 {
		t.Errorf("Dev is also said to hold the company's work")
	}
	even := advisor.Evidence{Work: worklet.WorkPatterns{Open: worklet.OpenWork{
		Holders: []worklet.HeldOpen{held("dev", 12, todo), held("dev-2", 6, nil)}}}}
	if got := stalls(t, twinShape(t), even, advisor.ClassWorkImbalance); len(got) != 0 {
		t.Errorf("12 against 6 raised %+v", got)
	}
}

// WORK BLOCKED A DAY ON NOTHING ANYONE HOLDS is one finding for the company, the holders inside,
// its fix asking Crewlet to find why each waits; a wait set within the day is none.
// Mutation: drop the day's bar, and the fresh wait is counted.
func TestWorkBlockedOnNothingAnyoneHoldsIsOneFindingThatAsksWhy(t *testing.T) {
	t.Parallel()
	ev := advisor.Evidence{Work: worklet.WorkPatterns{Open: worklet.OpenWork{Unheld: []worklet.UnheldWait{
		{Key: "GP-233", Holder: "dev", Since: monday.Add(-3 * 24 * time.Hour), On: []string{"GP-200"}},
		{Key: "GP-234", Holder: "dev", Since: monday.Add(-2 * 24 * time.Hour), On: []string{"GP-201"}},
		{Key: "GP-240", Holder: "qa", Since: monday.Add(-2 * time.Hour), On: []string{"GP-202"}},
	}}}}
	got := stalls(t, stallShape, ev, advisor.ClassBlockedNoCause)
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want one", got)
	}
	f := got[0]
	if f.Subject.Kind != advisor.SubjectCompany || f.Headline != "2 items are blocked on nothing anyone holds, "+
		"the oldest unchanged for 3 days." || len(f.Seats) != 1 || f.Seats[0].Seat != "dev" || f.Seats[0].Value != 2 {
		t.Errorf("finding = %+v, want GP-233 and GP-234, Dev's, the oldest 3 days", f)
	}
	if f.Fix.Kind != advisor.FixAskCrewlet || !strings.Contains(f.Fix.Prompt, "GP-233 (held by Dev, waits on GP-200)") ||
		strings.Contains(f.Fix.Prompt, "GP-240") {
		t.Errorf("fix = %+v, want Ask Crewlet about GP-233 and GP-234 only", f.Fix)
	}
}
