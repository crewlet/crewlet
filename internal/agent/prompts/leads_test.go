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
// how many of them it says, and how many earlier ones are not kept to tell
// again, said rather than left out, even where none is left to tell, and never
// as messages the engine no longer keeps: a read of them that failed counts
// every one, though the store may keep them still. Nothing at all is no
// section.
//
// Mutation: drop the count, say one message as several, leave the cut out, say
// it as no longer kept, or leave a cut with nothing to tell unsaid, and a case
// goes red.
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

// EARLIER ANSWERS ARE TOLD IN THE ORDER THEY CAME. A run that starts a session
// of its own is told the answers that carried the turn's work on before it,
// under a header of their own: how many, that they are in the order they came,
// each in the round its heading names, that the later of two that differ
// stands, and that its session holds none of them; and how many earlier
// messages that carried the work on are not kept to tell, said rather than left
// out, even where no answer is left to tell, and never as messages the engine
// no longer keeps: a read of them that failed counts every one, though the
// store may keep them still. Told again inside a task, the same answers are
// said in words true of any session of the work, which may show them, with no
// cut, which the section around the task says once. Nothing to tell is no
// section. And the section nests a level down by its own headings alone.
//
// Mutation: drop the count, the order, the rule that the later stands, or the
// word that the fresh session holds none of them; say it of a session told them
// again; leave a cut unsaid, or a cut alone, or say one as no longer kept; or
// nest a line no builder wrote, and a case goes red.
func TestEarlierAnswersAreToldInTheOrderTheyCame(t *testing.T) {
	t.Parallel()
	first := "### An answer to a question this work asked (in round 1, which a review sent back)\nfirst"
	second := "### An answer to a question this work asked (in this round)\n# not a heading of ours\nsecond"
	for _, empty := range []Prompt{EarlierAnswers(nil, 0, true), EarlierAnswers(nil, 3, false)} {
		if empty.Text != "" || len(empty.Sections) != 0 {
			t.Errorf("nothing to tell is told as %q, want no section", empty.Text)
		}
	}
	for name, tc := range map[string]struct {
		answers     []string
		cut         int
		fresh       bool
		says, never []string
	}{
		"one, to a fresh run": {answers: []string{first}, fresh: true,
			says: []string{answersBeforeHeader, "A run before you on this task stopped to ask, and was carried on by " +
				"the answer below, in the round its heading names. Your session does not hold it.", first},
			never: []string{"answers below", "the later one stands", "earlier message"}},
		"two, to a fresh run": {answers: []string{first, second}, fresh: true,
			says: []string{"the 2 answers below, in the order they came",
				"Your session holds none of them: where two of them differ, the later one stands.",
				first + "\n\n" + second}},
		"two, told again": {answers: []string{first, second},
			says: []string{"Runs of this work stopped to ask",
				"the 2 answers below, in the order they came, each in the round its heading names: where two of " +
					"them differ, the later one stands.", first + "\n\n" + second},
			never: []string{"Your session", "before you"}},
		"a cut beside them": {answers: []string{first}, cut: 2, fresh: true,
			says: []string{"(+2 earlier messages that carried this work on, which are not kept to tell: any answer " +
				"among them is not told here.)"}},
		"one cut beside them": {answers: []string{first}, cut: 1, fresh: true,
			says: []string{"(+1 earlier message that carried this work on, which is not kept to tell"}},
		"a cut and nothing left": {cut: 3, fresh: true,
			says: []string{answersBeforeHeader, "the 3 earliest messages that carried this work on are not kept to " +
				"tell, so any answer among them is not told."},
			never: []string{"below", "no longer keeps"}},
		"one cut and nothing left": {cut: 1, fresh: true,
			says:  []string{"the 1 earliest message that carried this work on is not kept to tell"},
			never: []string{"below", "no longer keeps"}},
	} {
		got := EarlierAnswers(tc.answers, tc.cut, tc.fresh)
		for _, want := range tc.says {
			if !strings.Contains(got.Text, want) {
				t.Errorf("%s: the section does not say %q:\n%s", name, want, got.Text)
			}
		}
		for _, never := range tc.never {
			if strings.Contains(got.Text, never) {
				t.Errorf("%s: the section says %q:\n%s", name, never, got.Text)
			}
		}
		if !got.Valid() {
			t.Errorf("%s: the section's outline does not tile it: %+v", name, got.Sections)
		}
		if cutAt, firstAt := strings.Index(got.Text, "earlier message"), strings.Index(got.Text, "###"); tc.cut > 0 &&
			len(tc.answers) > 0 && (cutAt < 0 || cutAt > firstAt) {
			t.Errorf("%s: the cut is not said before the answers it stands beside:\n%s", name, got.Text)
		}
	}
	nested := EarlierAnswers([]string{first, second}, 0, false).Nested().Text
	for _, want := range []string{"### Earlier answers that carried this work on",
		"#### An answer to a question this work asked (in round 1", "#### An answer to a question this work asked " +
			"(in this round)\n# not a heading of ours\nsecond"} {
		if !strings.Contains(nested, want) {
			t.Errorf("nested, the section does not say %q:\n%s", want, nested)
		}
	}
}

// AN ANSWER NAMES THE ROUND IT CAME IN. Told to a run working in a round, an
// answer of that round came in this round; one of an earlier round came in a
// round a review sent back, since nothing else begins a later round, and is
// named by its number, as the work already done numbers its rounds; and the
// held call it approved, made in that round, is among that round's calls there,
// which say how it ended. A round no record names is named not at all.
//
// Mutation: name an earlier round without its review, or this round by its
// number, or point a held call anywhere but its round's calls, and a case goes
// red.
func TestAnAnswerNamesTheRoundItCameIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		round, current int
		want           string
	}{
		{round: 2, current: 2, want: " (in this round)"},
		{round: 1, current: 3, want: " (in round 1, which a review sent back)"},
		{round: 4, current: 3, want: " (in round 4)"},
		{round: 0, current: 3, want: ""},
	} {
		if got := AnswerRound(tc.round, tc.current); got != tc.want {
			t.Errorf("AnswerRound(%d, %d) = %q, want %q", tc.round, tc.current, got, tc.want)
		}
	}
	listed := "among round 2's calls, under Iteration 2 of the work already done earlier in this turn"
	if got := HeldCallListed(2); !strings.Contains(got, "made once") || !strings.Contains(got, listed) ||
		!strings.Contains(PriorWorkHeader, "Already done earlier in this turn") {
		t.Errorf("a held call of round 2 is pointed to as %q, not to its round's calls in the work already done", got)
	}
}
