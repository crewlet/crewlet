package prompts

import (
	"strings"
	"testing"
)

// THE READ HEADER IS NO LONGER THAN THE HEADER IT REPLACES ([calledSinceHeader]),
// so the exact telling never makes a brief outgrow the cut-off one's.
//
// Mutation: lengthen the read header past 471 bytes, and this goes red.
func TestTheReadHeaderIsNoLongerThanTheHeaderItReplaces(t *testing.T) {
	t.Parallel()
	if len(readHeader) > len(calledSinceHeader) {
		t.Errorf("the read header is %d bytes, longer than the %d of the header it replaces", len(readHeader),
			len(calledSinceHeader))
	}
}

// A LIST A READING THINNED SAYS THE SESSION HOLDS SOME LEFT OUT: the session
// may show some, a call shown both is one call, the list leaves out what the
// reading found held or lists below, and never that they are every call since
// the point, which the cut-off header says and is false of it. A cut-off list
// no reading thinned keeps the cut-off header.
//
// Mutation: head a thinned list with the cut-off header, and this goes red.
func TestAListAReadingThinnedSaysTheSessionHoldsSomeLeftOut(t *testing.T) {
	t.Parallel()
	const listed = "- slack_post({\"text\":\"hi\"}) → success"
	if got, want := CalledAfter(AfterSave{Interrupted: true, Read: true, Calls: listed}),
		readHeader+"\n"+listed; got != want {
		t.Errorf("a list a reading thinned is told:\n%s\nwant:\n%s", got, want)
	}
	if got, want := CalledAfter(AfterSave{Interrupted: true, Calls: listed}),
		calledSinceHeader+"\n"+listed; got != want {
		t.Errorf("a cut-off list no reading thinned is told:\n%s\nwant:\n%s", got, want)
	}
	for _, said := range []string{"## Calls this run made that the session above may not show",
		"may show some of these", "less any it found the session holds or lists below",
		"is ONE call, made once, not two", "ALREADY RAN"} {
		if !strings.Contains(readHeader, said) {
			t.Errorf("the read header does not say %q:\n%s", said, readHeader)
		}
	}
	for _, unsaid := range []string{"every call", "they are", "not in it", "\u2014"} {
		if strings.Contains(readHeader, unsaid) {
			t.Errorf("the read header says %q:\n%s", unsaid, readHeader)
		}
	}
}

// A SUBAGENT'S CALLS ARE TOLD UNDER ITS NAME, in their own section after the
// calls after the point and before those shown again and the late ones: per
// subagent a heading (its agent id alone where nameless), its calls, and, after
// a successful `submit_work`, that it does not stand. No call listed, no
// heading; none at all, no section. The header claims nothing of the session,
// never that a call is shown twice.
//
// Mutation: drop the section, place it anywhere else, head a nameless subagent
// with an empty name, or drop a subagent's submission line, and this goes red.
func TestASubagentsCallsAreToldUnderItsName(t *testing.T) {
	t.Parallel()
	const (
		owed   = "- slack_history({\"channel\":\"C0\"}) → success (read)"
		listed = "- slack_post({\"text\":\"hi\"}) → success"
		first  = "- jira_create({\"summary\":\"one\"}) → success"
		second = "- submit_work({\"outcome\":\"delivered\"}) → success"
		shown  = "- slack_post({\"text\":\"held\"}) → success"
		late   = "- jira_create({\"summary\":\"the late one\"}) → success"
	)
	got := CalledAfter(AfterSave{HeldSubmission: true, Owed: owed, Calls: listed, Interrupted: true, Read: true,
		Subagents: []SubagentCalls{{Agent: "a1", Name: "survey the repo", Calls: first},
			{Agent: "b2", Calls: ""}, {Agent: "c3", Calls: second, Submission: true}},
		Shown: shown, Late: late})
	want := strings.Join([]string{heldSubmission, owedHeader + "\n" + owed, readHeader + "\n" + listed,
		subagentsHeader + "\n\n### Subagent a1: survey the repo\n" + first + "\n\n### Subagent c3\n" + second +
			"\n" + earlierSubmission,
		shownAgainHeader + "\n" + shown, lateCallsHeader + "\n" + late}, "\n\n")
	if got != want {
		t.Errorf("a brief with subagents' calls is told:\n%s\nwant:\n%s", got, want)
	}
	if got := CalledAfter(AfterSave{Late: late, Subagents: []SubagentCalls{{Agent: "b2"}}}); strings.Contains(got,
		subagentsHeader) {
		t.Errorf("a brief with no subagent's call listed is told the section:\n%s", got)
	}
	for _, unsaid := range []string{"ONE call", "shown both", "\u2014"} {
		if strings.Contains(subagentsHeader, unsaid) {
			t.Errorf("the subagents' header says %q:\n%s", unsaid, subagentsHeader)
		}
	}
}
