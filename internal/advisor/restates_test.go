package advisor_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
)

// A MODEL FINDING THAT RESTATES A RULE'S FINDING IS A DUPLICATE: about a seat
// the rule's finding holds, and naming what it is about (the kind of wake),
// however the words are spelled; another seat, or words about something else,
// are not. Mutation: match on the seat alone, and every model finding about a
// busy seat is closed.
func TestAModelFindingThatRestatesARuleFindingIsADuplicate(t *testing.T) {
	t.Parallel()
	rule := advisor.Finding{Class: advisor.ClassWakeReasonWaste, Source: advisor.SourceEvidence,
		Subject: advisor.Subject{Kind: advisor.SubjectWake, Name: "lead_fallback"},
		Seats:   []advisor.SeatShare{{Seat: "pm"}, {Seat: "dev"}}}
	model := func(seat, title string) advisor.Finding {
		return advisor.Finding{Class: advisor.ClassModel, Source: advisor.SourceModel,
			Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: seat}, Title: title}
	}
	for _, c := range []struct {
		f    advisor.Finding
		want bool
	}{
		{model("pm", "pm's lead-fallback wakes did nothing."), true},
		{model("dev", "Lead fallback wakes cost dev 40 turns."), true},
		{model("qa", "qa's lead-fallback wakes did nothing."), false},
		{model("pm", "pm's reviews wait a day."), false},
	} {
		if got := advisor.Restates(c.f, rule); got != c.want {
			t.Errorf("%q about %s restates the rule's finding: %v, want %v", c.f.Title, c.f.Subject.Seat, got, c.want)
		}
	}
	if advisor.Restates(rule, rule) {
		t.Error("a rule's finding restates itself")
	}
}
