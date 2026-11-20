package advisor_test

import (
	"regexp"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
)

// EVERY FINDING LEADS WITH ITS NUMBER: each class a detector raises states a
// headline carrying a number, not its class's title, and a finding kept
// before its class wrote one reads the same way.
// Mutation: drop a class's entry from the headlines, and its findings lead
// with their title again.
func TestEveryClassLeadsWithItsNumber(t *testing.T) {
	t.Parallel()
	digit := regexp.MustCompile(`[0-9]`)
	writesItsOwn := []advisor.Class{advisor.ClassWakeReasonWaste, advisor.ClassTurnCostOutlier,
		advisor.ClassScheduleLoad, advisor.ClassApprovalRubberStamp, advisor.ClassAuxiliaryModel,
		advisor.ClassScheduleFrequent, advisor.ClassScheduleOverdue, advisor.ClassModelLimit}
	answer := richAnswer(t)
	raised := map[advisor.Class]bool{}
	for _, f := range answer.Findings {
		raised[f.Class] = true
		if f.Class == advisor.ClassConfigAdvisory || f.Class == advisor.ClassConfigReference ||
			f.Class == advisor.ClassConfigAdmission || f.Class == advisor.ClassDirectBox {
			continue
		}
		if f.Headline == "" || f.Headline == f.Title || !digit.MatchString(f.Headline) {
			t.Errorf("%s (%s) leads with %q, want its number", f.Class, f.Subject.Seat, f.Headline)
		}
		kept := f
		kept.Headline = ""
		if got := advisor.HeadlineOf(kept); slices.Contains(writesItsOwn, f.Class) {
			if got != f.Title {
				t.Errorf("%s writes its own headline, and one kept without it reads %q", f.Class, got)
			}
		} else if got == f.Title || !digit.MatchString(got) {
			t.Errorf("%s kept without a headline reads %q, want its number", f.Class, got)
		}
		if len(f.Evidence) == 0 {
			t.Errorf("%s states no facts to show as its stats", f.Class)
		}
	}
	classes := append(append(advisor.EvidenceClasses(), advisor.AtlasClasses()...), advisor.ClassHealthUnheld)
	for _, class := range classes {
		if slices.Contains(writesItsOwn, class) || (slices.Contains(advisor.AtlasClasses(), class) &&
			class != advisor.ClassAtlasSourceStale) {
			continue // its own headline, or a title that counts its entries
		}
		bare := advisor.Finding{Class: class, Title: "Its title.", Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: "dev"}}
		if advisor.HeadlineOf(bare) == bare.Title {
			t.Errorf("%s has no headline of its own", class)
		}
	}
	for _, class := range []advisor.Class{advisor.ClassHealthUnheld, advisor.ClassBudgetStopped,
		advisor.ClassWorkBottleneck, advisor.ClassApprovalRubberStamp, advisor.ClassAuxiliaryModel} {
		if !raised[class] {
			t.Errorf("%s was not raised to check", class)
		}
	}
	if !slices.ContainsFunc(answer.Findings, func(f advisor.Finding) bool {
		return f.Class == advisor.ClassAuxiliaryShare && f.Headline == "Dev spent 400K of its 1.2M tokens on learning this week (33%)."
	}) {
		t.Errorf("no auxiliary share finding names its seat and numbers: %+v", answer.Findings)
	}
}
