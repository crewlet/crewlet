package worklet

import (
	"testing"
	"time"
)

// A FLOW NOTICE IS ONE SENTENCE IN ITS READER'S WORDS (ADR-0162): no state
// label beside the reason's own, the person told called "you", and a stuck
// item's window said in hours rather than named.
func TestAFlowNoticeReadsAsOneSentenceToWhomeverItTells(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		notice   FlowNotice
		assignee string
		window   time.Duration
		want     string
	}{
		{"due, to its person", FlowNotice{State: FlowDue, Role: FlowToldAssignee}, "riley-ops",
			8 * time.Hour, "Its start passed and you have not picked it up since."},
		{"due, to the lead", FlowNotice{State: FlowDue, Role: FlowToldLead}, "agent-swe",
			8 * time.Hour, "Its start passed and agent-swe has not picked it up since."},
		{"due, nobody assigned", FlowNotice{State: FlowDue, Role: FlowToldLead}, "",
			8 * time.Hour, "Its start passed and nobody is assigned to it."},
		{"stuck, a one-hour window", FlowNotice{State: FlowStuck, Role: FlowToldAssignee},
			"riley-ops", time.Hour, "Nothing changed on it for 1 hour, its project's stuck window."},
		{"stuck, the default window", FlowNotice{State: FlowStuck, Role: FlowToldLead},
			"agent-swe", 8 * time.Hour, "Nothing changed on it for 8 hours, its project's stuck window."},
		{"held, paused", FlowNotice{State: FlowHeld, Cause: FlowCausePaused, Role: FlowToldLead},
			"agent-swe", 8 * time.Hour, "agent-swe is paused, so nobody is moving it."},
		{"held, nobody", FlowNotice{State: FlowHeld, Cause: FlowCauseNobody, Role: FlowToldLead},
			"", 8 * time.Hour, "Nobody is assigned to it."},
	} {
		if got := flowExcerpt(c.notice, c.assignee, c.window); got != c.want {
			t.Errorf("%s: the notice reads %q, want %q", c.name, got, c.want)
		}
	}
}
