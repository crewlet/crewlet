package prompts

import (
	"strings"
	"testing"
	"time"
)

// LEADS TOLD AGAIN ARE ONES THE SESSION MAY ALREADY SHOW. A session that may
// lack its job's brief is told again the messages that brief opened with, under
// a header that says the session may already show them, that a message shown
// both above and here is one message, sent once, and, of several, that they are
// in the order they were sent and the later of two that differ stands; with
// how many of them it says, and how many earlier ones the engine no longer
// keeps, said rather than left out, even where none is left to tell. Nothing at
// all is no section.
//
// Mutation: drop the count, say one message as several, leave the cut out, or
// leave a cut with nothing to tell unsaid, and a case goes red.
func TestLeadsToldAgainAreOnesTheSessionMayAlreadyShow(t *testing.T) {
	t.Parallel()
	answer := "### An answer to a question this work asked\nA run of this work asked: which branch?"
	review := "### Your work was reviewed and sent back"
	if got := LeadsToldAgain(nil, 0); got != "" {
		t.Errorf("nothing to tell again is told as %q, want no section", got)
	}
	for name, tc := range map[string]struct {
		leads       []string
		cut         int
		says, never []string
	}{
		"one message": {leads: []string{answer},
			says:  []string{leadsAgainHeader, "may not show the message below", "ONE message, sent once", answer},
			never: []string{"messages below", "earlier such", "the later one stands"}},
		"two messages, in order": {leads: []string{review, answer},
			says: []string{leadsAgainHeader, "the 2 messages below", "in the order they were sent",
				"the later one stands", review + "\n\n" + answer}},
		"a cut beside them": {leads: []string{answer}, cut: 3,
			says: []string{"(+3 earlier such messages, which are not kept to tell again.)"}},
		"one cut": {leads: []string{answer}, cut: 1,
			says: []string{"(+1 earlier such message, which is not kept to tell again.)"}},
		"a cut and nothing left": {cut: 2,
			says:  []string{leadsAgainHeader, "may not show every message", "(+2 earlier such messages"},
			never: []string{"below"}},
	} {
		got := LeadsToldAgain(tc.leads, tc.cut)
		for _, want := range tc.says {
			if !strings.Contains(got, want) {
				t.Errorf("%s: the section does not say %q:\n%s", name, want, got)
			}
		}
		for _, never := range tc.never {
			if strings.Contains(got, never) {
				t.Errorf("%s: the section says %q:\n%s", name, never, got)
			}
		}
		if cutAt, firstAt := strings.Index(got, "earlier such"), strings.Index(got, "###"); tc.cut > 0 &&
			len(tc.leads) > 0 && (cutAt < 0 || cutAt > firstAt) {
			t.Errorf("%s: the cut is not said before the messages it stands beside:\n%s", name, got)
		}
	}
}

// A TURN TOLD AGAIN KEEPS ITS BOUNDARY AND NOT ITS BOX. The lead of a run that
// continued the session its conversation's last turn left, told again to a
// later session of the work that may lack it, says where this turn began (the
// turn it began after, when that turn ended, that it went on past the session
// or was lost, in a schedule's words for a fire), what that boundary makes
// stale, and that the session's files from before it were the earlier box's;
// and, of a session cut off when it was kept, that it may hold a call with no
// result from before the turn began. NEVER that the run is in a new box, on a
// fresh checkout, to check a repository out again, or where the earlier box's
// paths were: those are true of the box that lead's run started in, and stale
// or false of the box a later job continues the session in.
//
// Mutation: tell the lead again as BuildNextTurn renders it, keep its new-box
// section, or lose the boundary of a turn that went on or was lost, and a case
// goes red.
func TestATurnToldAgainKeepsItsBoundaryAndNotItsBox(t *testing.T) {
	t.Parallel()
	ended := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	newBox := []string{"new box", "FRESH checkout", "code_checkout", "/boxes/4f2a", "$HOME", "pwd",
		"begins here", "Everything above"}
	for name, tc := range map[string]struct {
		m    NextTurn
		says []string
	}{
		"a turn that ended": {m: NextTurn{BaseTurn: "t-1", EndedAt: ended, Home: "/boxes/4f2a",
			Checkout: "code_checkout"},
			says: []string{"### A new turn of this conversation began here", "through turn `t-1`",
				"which ended at 2026-11-03T09:00:00Z", "This turn began after that",
				"**What was read before this turn began may be stale:**",
				"**Tools activated and skills loaded before this turn began were set up anew for it:**",
				"is what the box of the turn it came from held."}},
		"a turn that went on past its session": {m: NextTurn{BaseTurn: "t-1", WentOn: true, Lost: true,
			CapturedAt: ended},
			says: []string{"up to a session kept while turn `t-1` still worked", "at 2026-11-03T09:00:00Z",
				"and that turn was lost", "How turn `t-1` ended"}},
		"a lost turn": {m: NextTurn{BaseTurn: "t-1", Lost: true, EndedAt: ended},
			says: []string{"which was lost and ended at 2026-11-03T09:00:00Z", "may not hold the last of"}},
		"a schedule's fire": {m: NextTurn{BaseTurn: "f-9", Fire: true},
			says: []string{"### A new fire of this schedule began here", "This fire began after that"}},
		"a session cut off when it was kept": {m: NextTurn{BaseTurn: "t-1", Interrupted: true},
			says: []string{"was cut off when it was kept", "before this turn began it may hold a call with no result"}},
	} {
		got := NextTurnAgain(tc.m)
		for _, want := range tc.says {
			if !strings.Contains(got, want) {
				t.Errorf("%s: the turn told again does not say %q:\n%s", name, want, got)
			}
		}
		for _, never := range newBox {
			if strings.Contains(got, never) {
				t.Errorf("%s: the turn told again says %q, a claim of a box it may not be in:\n%s", name, never, got)
			}
		}
	}
}
