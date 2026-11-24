package prompts

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// LEADS TOLD AGAIN ARE ONES THE SESSION MAY ALREADY SHOW. The header says the
// session may already show them, that one shown twice is one message sent once,
// and of several, their count, that they are in sent order and the later of two
// that differ stands. Earlier ones not kept are counted, even with none left to
// tell, never as ones the engine no longer keeps (a failed read counts them
// all). Nothing is no section. NEVER WHY the session may not show them: the
// section before says that, as the record bears out, not "cut off".
//
// Mutation: drop the count, say one message as several, leave the cut out, say
// it as no longer kept, leave a cut with nothing to tell unsaid, or say the run
// may have been cut off, and a case goes red.
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
		if strings.Contains(got, "cut off") {
			t.Errorf("%s: the section says why the session may not show them, that it was cut off:\n%s", name, got)
		}
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

// A TURN TOLD AGAIN KEEPS ITS BOUNDARY AND NOT ITS BOX. A continued turn's lead,
// told again, says where the turn began (after which turn, ended or lost, a fire
// in a schedule's words) and what that makes stale. Every word of the earlier
// turns' session (its box's files, where it was kept, a cut-off call) follows
// the line making it conditional: the session may be another lineage's. NEVER
// a new box, a fresh checkout or the earlier box's paths, stale where a later
// job continues.
//
// Mutation: tell the lead again as BuildNextTurn renders it, keep its new-box
// section, lose the boundary of a turn that went on or was lost, say that the
// session above began as the earlier turns left it, or say a word of that
// session before the line that makes it conditional, and a case goes red.
func TestATurnToldAgainKeepsItsBoundaryAndNotItsBox(t *testing.T) {
	t.Parallel()
	ended := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	newBox := []string{"new box", "FRESH checkout", "code_checkout", "/boxes/4f2a", "$HOME", "pwd",
		"begins here", "Everything above", "The session above began", "the session above may not hold"}
	const where = "Where the session above began as the one your earlier turns left, in full or in a summary:"
	for name, tc := range map[string]struct {
		m             NextTurn
		says, ofThere []string
	}{
		"a turn that ended": {m: NextTurn{BaseTurn: "t-1", EndedAt: ended, Home: "/boxes/4f2a",
			Checkout: "code_checkout"},
			says: []string{"### A new turn of this conversation began here", "This turn began after your earlier " +
				"turns of this conversation, through turn `t-1`, which ended at 2026-11-03T09:00:00Z, and the task " +
				"below is this turn's.",
				"**What was read before this turn began may be stale:**",
				"**Tools activated and skills loaded before this turn began were set up anew for it:**"},
			ofThere: []string{"is what the box of the turn it came from held."}},
		"a turn that went on past its session": {m: NextTurn{BaseTurn: "t-1", WentOn: true, Lost: true,
			CapturedAt: ended},
			says: []string{"through turn `t-1`, which was lost, and the task", "How turn `t-1` ended"},
			ofThere: []string{"**It was kept while turn `t-1` still worked, at 2026-11-03T09:00:00Z:** what that " +
				"turn did after it is not in it."}},
		"a lost turn": {m: NextTurn{BaseTurn: "t-1", Lost: true, EndedAt: ended},
			says:    []string{"which was lost and ended at 2026-11-03T09:00:00Z"},
			ofThere: []string{"**It may not hold the last of what turn `t-1` did,** which was lost."}},
		"a schedule's fire": {m: NextTurn{BaseTurn: "f-9", Fire: true},
			says: []string{"### A new fire of this schedule began here", "This fire began after your earlier " +
				"fires of this schedule, through fire `f-9`"},
			ofThere: []string{"Where the session above began as the one your earlier fires left"}},
		"a session cut off when it was kept": {m: NextTurn{BaseTurn: "t-1", Interrupted: true},
			ofThere: []string{"**It was cut off when it was kept,** so before this turn began it may hold a call " +
				"with no result"}},
	} {
		got := NextTurnAgain(tc.m)
		for _, want := range tc.says {
			if !strings.Contains(got, want) {
				t.Errorf("%s: the turn told again does not say %q:\n%s", name, want, got)
			}
		}
		at := strings.Index(got, where)
		if tc.m.Fire {
			at = strings.Index(got, "Where the session above began as")
		}
		for _, want := range tc.ofThere {
			if said := strings.Index(got, want); said < 0 || at < 0 || said < at {
				t.Errorf("%s: the turn told again does not say %q of a session that began as the earlier "+
					"turns left it alone:\n%s", name, want, got)
			}
		}
		for _, never := range newBox {
			if strings.Contains(got, never) {
				t.Errorf("%s: the turn told again says %q, a claim of a box, or a session, it may not be in:\n%s",
					name, never, got)
			}
		}
	}
}

// A SEND-BACK TOLD AGAIN SAYS NOTHING OF THE SESSION ABOVE. Told again, it may
// reach a session it was not sent to, so it says the work was sent back and the
// rules for the earlier rounds' calls whether or not the session shows them,
// never that this is the same session or that those rounds are in it. The
// original send-back says both, which is true of it.
//
// Mutation: tell a send-back again in its own words, or lose the rules or the
// review, and a check goes red.
func TestASendBackToldAgainSaysNothingOfTheSessionAbove(t *testing.T) {
	t.Parallel()
	sent := BuildContinuedRound(ContinuedRound{Task: "post the summary", Review: "add a label"})
	again := BuildContinuedRound(ContinuedRound{Task: "post the summary", Review: "add a label", Again: true})
	for _, says := range []string{"the same session", "in this session above"} {
		if !strings.Contains(sent.Text, says) {
			t.Errorf("the send-back to the session that did the round does not say %q:\n%s", says, sent.Text)
		}
		if strings.Contains(again.Text, says) {
			t.Errorf("the send-back told again says %q, true only of the session it was sent to:\n%s", says,
				again.Text)
		}
	}
	for _, says := range []string{"## Your work was reviewed and sent back\nA review of this turn's work sent it " +
		"back.", "Every call your earlier rounds of this turn made that succeeded ALREADY RAN, whether or not the " +
		"session above shows it:", "**Everything else** changed something outside the engine: do NOT issue it " +
		"again", "post the summary", "add a label"} {
		if !strings.Contains(again.Text, says) {
			t.Errorf("the send-back told again does not say %q:\n%s", says, again.Text)
		}
	}
	if !again.Valid() {
		t.Errorf("the send-back told again's outline does not tile it: %+v", again.Sections)
	}
}

// EARLIER ANSWERS ARE TOLD IN THE ORDER THEY CAME. To a fresh session the header
// says how many, in order, each in its heading's round, the later of two that
// differ stands, and its session holds none; earlier messages not kept are
// counted, even with no answer left, never as ones the engine no longer keeps.
// Told again inside a task, the words hold for any session and carry no cut.
// Nothing is no section, and it nests by its own headings alone.
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
		answers     []EarlierLead
		cut         int
		fresh       bool
		says, never []string
	}{
		"one, to a fresh run": {answers: answered(first), fresh: true,
			says: []string{answersBeforeHeader, "A run before you on this task stopped to ask, and was carried on by " +
				"the answer below, in the round its heading names. Your session does not hold it.", first},
			never: []string{"answers below", "the later one stands", "earlier message"}},
		"two, to a fresh run": {answers: answered(first, second), fresh: true,
			says: []string{"the 2 answers below, in the order they came",
				"Your session holds none of them: where two of them differ, the later one stands.",
				first + "\n\n" + second}},
		"two, told again": {answers: answered(first, second),
			says: []string{"Runs of this work stopped to ask",
				"the 2 answers below, in the order they came, each in the round its heading names: where two of " +
					"them differ, the later one stands.", first + "\n\n" + second},
			never: []string{"Your session", "before you"}},
		"a cut beside them": {answers: answered(first), cut: 2, fresh: true,
			says: []string{"(+2 earlier messages that carried this work on, which are not kept to tell: any answer " +
				"among them is not told here.)"}},
		"one cut beside them": {answers: answered(first), cut: 1, fresh: true,
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
	nested := EarlierAnswers(answered(first, second), 0, false).Nested().Text
	for _, want := range []string{"### Earlier answers that carried this work on",
		"#### An answer to a question this work asked (in round 1", "#### An answer to a question this work asked " +
			"(in this round)\n# not a heading of ours\nsecond"} {
		if !strings.Contains(nested, want) {
			t.Errorf("nested, the section does not say %q:\n%s", want, nested)
		}
	}
}

// answered is texts as answers to questions the work asked, for [EarlierAnswers].
func answered(texts ...string) []EarlierLead {
	out := make([]EarlierLead, len(texts))
	for i, text := range texts {
		out[i] = EarlierLead{Text: text}
	}
	return out
}

// AN ACCOUNT OF WHAT A RUN WAITED FOR IS NEVER INTRODUCED AS AN ANSWER. The
// engine carries a run on with the calls it parked on or the run it waited for,
// and nobody was asked anything: the section says what was waited for, under a
// heading of messages, and only where answers sit beside such accounts that the
// later of two answers stands, fresh or told again.
//
// Mutation: introduce every lead as an answer to a question the run asked (the
// old intro, or answersBeforeHeader whatever the kinds), and a case goes red.
func TestAnAccountOfWhatARunWaitedForIsNeverIntroducedAsAnAnswer(t *testing.T) {
	t.Parallel()
	calls := EarlierLead{Text: "### Calls this work waited for (in this round)\nx", Kind: EarlierCalls}
	wait := EarlierLead{Text: "### A run this work waited for (in this round)\ny", Kind: EarlierWait}
	answer := EarlierLead{Text: "### An answer to a question this work asked (in this round)\nz"}
	for _, fresh := range []bool{true, false} {
		for name, tc := range map[string]struct {
			leads []EarlierLead
			says  []string
		}{
			"calls alone":  {leads: []EarlierLead{calls}, says: []string{"waited for calls of its own"}},
			"a wait alone": {leads: []EarlierLead{wait}, says: []string{"waited for another run"}},
			"both":         {leads: []EarlierLead{calls, wait}, says: []string{"calls of their own or other runs"}},
			"beside an answer": {leads: []EarlierLead{calls, answer},
				says: []string{"answers to questions a run asked", "Where two answers differ, the later one stands."}},
		} {
			got := EarlierAnswers(tc.leads, 0, fresh).Text
			intro := got[:strings.Index(got, "###")]
			for _, want := range append(tc.says, messagesBeforeHeader) {
				if !strings.Contains(intro, want) {
					t.Errorf("%s, fresh %v: the intro does not say %q:\n%s", name, fresh, want, intro)
				}
			}
			if strings.Contains(intro, "stopped to ask") ||
				(!slices.Contains(tc.leads, answer) && strings.Contains(intro, "answer")) {
				t.Errorf("%s, fresh %v: the intro says the run was answered:\n%s", name, fresh, intro)
			}
		}
	}
}

// AN ANSWER NAMES THE ROUND IT CAME IN: "this round", or an earlier round by
// number as one a review sent back (nothing else begins a later round), and the
// held call it approved is among that round's calls in the work already done.
// A round no record names is named not at all.
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

// A COUNTED HELD CALL IS ONE OF ITS ENTRY'S CALLS, TOLD HOW IT ENDED. Where an
// entry may count the approved call rather than list it, the answer names the
// entry (a round by number, a parked span by first "to" last, never a dash) and
// says what became of the call is told here, since the entry may not say it.
//
// Mutation: name a span by its first round alone, say a round alone as a span,
// name another round's calls, or leave out that the entry may count it, and a
// case goes red.
func TestACountedHeldCallIsOneOfItsEntrysCalls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		round, first, last int
		says, never        []string
	}{
		{round: 2, first: 2, last: 0, says: []string{"one of round 2's calls, which Iteration 2 of the work already " +
			"done earlier in this turn may count rather than list", "what became of it is told here"},
			never: []string{"Iterations"}},
		{round: 2, first: 2, last: 2, says: []string{"which Iteration 2 of the work already done"},
			never: []string{"Iterations"}},
		{round: 3, first: 2, last: 5, says: []string{"one of round 3's calls, which Iterations 2 to 5 of the work " +
			"already done earlier in this turn may count rather than list"}, never: []string{"\u2013", "\u2014"}},
	} {
		got := HeldCallCounted(tc.round, tc.first, tc.last)
		for _, want := range tc.says {
			if !strings.Contains(got, want) {
				t.Errorf("HeldCallCounted(%d, %d, %d) does not say %q: %q", tc.round, tc.first, tc.last, want, got)
			}
		}
		for _, never := range tc.never {
			if strings.Contains(got, never) {
				t.Errorf("HeldCallCounted(%d, %d, %d) says %q: %q", tc.round, tc.first, tc.last, never, got)
			}
		}
	}
}

// A REVIEW NOT KEPT IS SAID AS GONE, NEVER AS A COPY THE ENGINE HOLDS. A review
// whose round a park dropped is said in its place, under its own lead-level
// heading: it sent the work back, its round is not kept on the parked turn, and
// what it said is not told again. Never as an unreadable copy, which claims the
// engine holds one.
//
// Mutation: say it in LeadUnread's words, or as a send-back, or leave out that
// its round is not kept, and this goes red.
func TestAReviewNotKeptIsSaidAsGoneNeverAsACopyTheEngineHolds(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"### A review that sent this work back, which is not kept\n",
		"is not kept on the parked turn", "what its reviewer said is gone", "not told again here"} {
		if !strings.Contains(ReviewNotKept, want) {
			t.Errorf("a review not kept does not say %q: %q", want, ReviewNotKept)
		}
	}
	for _, never := range []string{"copy", "cannot be read", "does not open", "Your work was reviewed"} {
		if strings.Contains(ReviewNotKept, never) {
			t.Errorf("a review not kept says %q: %q", never, ReviewNotKept)
		}
	}
}
