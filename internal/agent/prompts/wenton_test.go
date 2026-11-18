package prompts

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/provenance"
)

// A NEXT TURN ON A SESSION ITS TURN WENT PAST IS TOLD SO. The lead says where
// the session ends: through the turn, at its end, when the session is its last
// word; up to the session kept while the turn still worked, at that instant,
// its later work not above, when the turn went on past it (and lost, if so);
// through a lost turn that said nothing of going on, the session perhaps
// lacking its last work. Each says how the turn ended is told below, a fire in
// the schedule's words.
//
// Mutation: tell every base through its turn's end; check lost before went-on;
// tell a lost turn the went-on lead; or drop the instant kept: a check goes
// red.
func TestANextTurnOnASessionItsTurnWentPastIsToldSo(t *testing.T) {
	t.Parallel()
	ended := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	kept := ended.Add(-40 * time.Minute)
	for _, c := range []struct {
		name        string
		m           NextTurn
		says, never []string
	}{
		{name: "a turn that went on past its session",
			m: NextTurn{BaseTurn: "t-1", EndedAt: ended, CapturedAt: kept, WentOn: true},
			says: []string{"up to a session kept while turn `t-1` still worked, at 2026-11-08T09:20:00Z: what " +
				"that turn did after it is not above.",
				"How turn `t-1` ended, among the conversation's earlier turns, is told below.",
				"A new message has woken you"},
			never: []string{"through turn `t-1`", "may not hold"}},
		{name: "a fire that went on past its session",
			m: NextTurn{BaseTurn: "f-1", EndedAt: ended, CapturedAt: kept, WentOn: true, Fire: true},
			says: []string{"up to a session kept while fire `f-1` still worked",
				"what that fire did after it is not above",
				"How fire `f-1` ended, among the schedule's earlier fires, is told below.",
				"The schedule has fired again"},
			never: []string{"through fire `f-1`", "conversation"}},
		{name: "a turn that went on past its session and was then lost",
			m: NextTurn{BaseTurn: "t-1", EndedAt: ended, CapturedAt: kept, WentOn: true, Lost: true},
			says: []string{"up to a session kept while turn `t-1` still worked, at 2026-11-08T09:20:00Z: what " +
				"that turn did after it is not above, and that turn was lost.",
				"How turn `t-1` ended, among the conversation's earlier turns, is told below."},
			never: []string{"through turn `t-1`", "may not hold"}},
		{name: "a turn that was lost",
			m: NextTurn{BaseTurn: "t-1", EndedAt: ended, CapturedAt: kept, Lost: true},
			says: []string{"through turn `t-1`, which was lost and ended at 2026-11-08T10:00:00Z: the session " +
				"above may not hold the last of what that turn did.",
				"How turn `t-1` ended, among the conversation's earlier turns, is told below."},
			never: []string{"still worked", "which ended at", "box or host"}},
		{name: "a turn that ended on its last word",
			m: NextTurn{BaseTurn: "t-1", EndedAt: ended, CapturedAt: kept},
			says: []string{"through turn `t-1`, which ended at 2026-11-08T10:00:00Z.",
				"How the conversation's earlier turns ended is told below."},
			never: []string{"still worked", "may not hold", "is not above"}},
	} {
		lead := BuildNextTurn(c.m)
		for _, want := range c.says {
			if !strings.Contains(lead, want) {
				t.Errorf("%s: the lead does not say %q:\n%s", c.name, want, lead)
			}
		}
		for _, not := range c.never {
			if strings.Contains(lead, not) {
				t.Errorf("%s: the lead says %q:\n%s", c.name, not, lead)
			}
		}
	}
}

// A CUT-OFF SESSION IS SAID TRUE OF A SAVE AND OF A STOP. A cut-off session
// was taken while its harness still worked (a box that would not end, packed
// as it stood) or as or after it stopped; a run continuing either is told it
// may end mid-call in words true of both, never that its harness was stopped.
//
// Mutation: say it was saved only as or after a stop, or leave the section off
// a next turn's cut-off base, and a check goes red.
func TestACutOffSessionIsSaidTrueOfASaveAndOfAStop(t *testing.T) {
	t.Parallel()
	for _, said := range []string{"saved while its harness was still working", "or as or after it was stopped",
		"may end in the middle of a call"} {
		if !strings.Contains(InterruptedSession, said) {
			t.Errorf("the cut-off section does not say %q:\n%s", said, InterruptedSession)
		}
	}
	if lead := BuildNextTurn(NextTurn{BaseTurn: "t-1", Interrupted: true}); !strings.Contains(lead,
		InterruptedSession) {
		t.Errorf("a next turn on a cut-off session is not told so:\n%s", lead)
	}
	if lead := BuildNextTurn(NextTurn{BaseTurn: "t-1"}); strings.Contains(lead, "cut off") {
		t.Errorf("a next turn on a session nothing cut off is told it was:\n%s", lead)
	}
}

// A COPY CUT OFF IN ITS BOX IS TOLD SO WITHOUT A SAVE, AND ONE NOTHING SAYS OF
// IS NOT TOLD IT WAS. A copy in its job's own box continues with no capture, so
// one whose record says it was cut off is told it may end mid-call and lack its
// run's last work, and nothing of a save. One whose record says nothing (an
// older build, a record that did not land) is told the same lack, but only
// that it may have been cut off, never stopped, killed or failed. The calls'
// header asserts no cut-off of its own.
//
// Mutation: tell the copy the kept capture's section, drop either clause, tell
// an unrecorded copy it was cut off, or say so in the calls' header, and a
// check goes red.
func TestACopyCutOffInItsBoxIsToldSoWithoutASave(t *testing.T) {
	t.Parallel()
	for section, said := range map[string][]string{
		InterruptedCopy: {"## The session above was cut off", "did not finish cleanly by itself",
			"may end in the middle of a call", "may not show the last of what it did"},
		MayBeInterruptedCopy: {"## The session above may have been cut off", "no record that the run",
			"may end in the middle of a call", "may not show the last of what it did"},
	} {
		for _, want := range said {
			if !strings.Contains(section, want) {
				t.Errorf("the cut-off section of a copy in its box does not say %q:\n%s", want, section)
			}
		}
		if strings.Contains(section, "saved") {
			t.Errorf("the cut-off section of a copy in its box speaks of a save:\n%s", section)
		}
	}
	for _, unseen := range []string{"was cut off", "did not finish cleanly", "stopped, killed"} {
		if strings.Contains(MayBeInterruptedCopy, unseen) {
			t.Errorf("a copy nothing was recorded of is told %q:\n%s", unseen, MayBeInterruptedCopy)
		}
	}
	if strings.Contains(calledSinceHeader, "was cut off") {
		t.Errorf("the header of the calls a copy may not show says it was cut off:\n%s", calledSinceHeader)
	}
}

// THE NOTES A SESSION MAY HAVE BEEN CUT OFF FROM ARE TOLD AGAIN AS NOTES IT MAY
// ALREADY SHOW, AND COUNTED, so a note held twice is done once: one shown above
// and below is ONE note; the count separates them from the later notes, which
// are ones the turn had not read, never "new" (a harness can hold a note before
// the record says so). Nor are they "every note" the turn read: a harness that
// ended its own run lacks only the last read's. None says nothing.
//
// Mutation: drop the header, its one-note sentence or its count, call the
// later notes new, call these every note the turn read, or say something where
// there is no note, and a check goes red.
func TestNotesToldAgainAreOnesTheSessionMayAlreadyShow(t *testing.T) {
	t.Parallel()
	if got := NotesReadAgain(nil); got != "" {
		t.Errorf("no note told again says %q, want nothing", got)
	}
	first, second := SteerMessage("founder", "use the staging database"), SteerMessage("", "and tag it")
	for notes, count := range map[int]string{1: "the note below", 2: "the 2 notes below"} {
		told := []string{first, second}[:notes]
		got := NotesReadAgain(told)
		if !strings.HasPrefix(got, notesAgainHeader+"\n") || !strings.Contains(got, count) ||
			!strings.Contains(got, "ONE note, sent once") {
			t.Errorf("%d notes told again are not headed as ones the session may already show, counted as "+
				"%q:\n%s", notes, count, got)
		}
		header := strings.TrimSuffix(got, strings.Join(told, "\n\n"))
		if strings.Contains(header, " new") ||
			!strings.Contains(header, "A note after them is one the turn had not read before this message.") {
			t.Errorf("%d notes told again say the notes after them are new rather than ones the turn had not "+
				"read:\n%s", notes, got)
		}
		if strings.Contains(strings.ToLower(header), "every note") {
			t.Errorf("%d notes told again are said to be every note the turn has read:\n%s", notes, got)
		}
		if !strings.HasSuffix(got, "\n\n"+strings.Join(told, "\n\n")) {
			t.Errorf("%d notes told again are not told after their header, in order:\n%s", notes, got)
		}
	}
}

// A LOST TURN'S ENDING NAMES NO BOX IT MAY STILL HAVE. A turn is lost
// wherever nothing saw it end (a lost box, a broken resume, a removed seat, a
// retention duty ending an unrecorded end), and only the first lost a box: the
// ending says the engine ended it and leaves the cause to its failure, never
// a lost box or host.
//
// Mutation: say a lost turn was lost with its executor's box or host, and the
// account of a turn the retention duty ended says it.
func TestALostTurnsEndingNamesNoBoxItMayStillHave(t *testing.T) {
	t.Parallel()
	b := provenance.NewBoundary()
	block, _ := BuildConversationEndings(b, ConversationEndings{Head: "t-1", Turns: []TurnEnding{{Turn: "t-1",
		Outcome: "lost", Failure: "no end was recorded for this turn, and no run of it was live a day after " +
			"its last record: its end was lost"}}})
	if !strings.Contains(block, "It was lost: the engine ended it, not the turn itself, and it is not carried on.") {
		t.Errorf("the lost turn's ending does not say the engine ended it:\n%s", block)
	}
	if strings.Contains(block, "box or host") {
		t.Errorf("the lost turn's ending blames a box or host it may still have:\n%s", block)
	}
}

// A SESSION THAT MAY COMPACT IS TOLD WHY, AND NEVER THAT IT WAS CUT OFF. A
// continuation Claude Code may summarize is told everything again, after a
// section saying why, that the session may still show it all, and that each is
// one message, call or note; nothing of a cut-off, since it may be whole.
//
// Mutation: tell it the cut-off copy's words, or drop the clause that it may
// still show them, and a check goes red.
func TestAMayCompactSessionIsToldWhyAndNeverThatItWasCutOff(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"## The session above may reach you as a summary", "compacted",
		"a summary of it", "may still show some or all of them", "each is ONE message, call or note"} {
		if !strings.Contains(MayCompactSession, want) {
			t.Errorf("the section of a session that may compact does not say %q:\n%s", want, MayCompactSession)
		}
	}
	for _, unsaid := range []string{"cut off", "did not finish cleanly", "stopped, killed", "saved"} {
		if strings.Contains(MayCompactSession, unsaid) {
			t.Errorf("the section of a session that may compact says %q:\n%s", unsaid, MayCompactSession)
		}
	}
}
