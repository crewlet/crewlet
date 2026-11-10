package prompts

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/provenance"
)

// A NEXT TURN ON A SESSION ITS TURN WENT PAST IS TOLD SO. The lead of a run that
// continues the session its conversation's last turn left says where the
// session ends: through that turn, which ended at its instant, where the
// session is the turn's last word; up to the session, kept while the turn still
// worked at the instant it was kept, what the turn did after it not above,
// where the turn's end said it went on past it — saying too where that turn was
// then lost; and, where the turn was lost and its end said nothing of going
// on, through it, the session perhaps not holding the last of what it did —
// each saying how that turn ended is told below, and a schedule's fire in the
// schedule's words.
//
// Mutation: tell every base through its turn's end, and the went-on leads say
// so; let a lost turn's lead come before the went-on one, and a turn that went
// on past its session and was then lost is told through it; tell a lost turn
// the went-on lead, and it is said to have gone on past a session it may not
// have; leave out the instant the session was kept, and the run cannot tell
// where it ends.
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

// A CUT-OFF SESSION IS SAID TRUE OF A SAVE AND OF A STOP. A session its
// harness was cut off in is one taken while the harness still worked — a job its
// box would not end, packed as it stood — or one taken as or after it was
// stopped; a run that continues either, a relaunch of its turn or its
// conversation's next turn, is told it may end mid-call in words true of both,
// never that its harness was stopped when it may never have been.
//
// Mutation: say it was saved only as or after its harness was stopped, as it
// once did, or leave the section off a next turn's cut-off base, and a check
// goes red.
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
// IS NOT TOLD IT WAS. The copy of a session its job's own box holds is continued
// in place with no capture behind it, so where its job's record says its
// harness was cut off it is told what a cut-off session is — it may end in the
// middle of a call, and may not show the last of what its run did — and nothing
// of a save, which that copy never had: a kept capture's section says it was
// saved while its harness worked or as it stopped. Where the record says nothing
// of how its harness ended — an older build collected the job, or the record did
// not land — it is told the same of what it may then lack, but only that it may
// have been cut off: never that it was stopped, killed or ended on an error,
// which nobody saw. And the header the calls follow under says why the session
// may show only some of them in no words of its own, the section before it
// saying it, so it asserts nothing of a copy nothing was recorded of.
//
// Mutation: tell the copy the kept capture's section, drop either clause, tell a
// copy nothing was recorded of that it was cut off, or say in the calls' header
// that the session was cut off, and a check goes red.
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
// ALREADY SHOW, AND COUNTED. Each note says it stands for the rest of the turn,
// so a session that holds one and reads it again could do twice what it asks:
// the notes told again are headed as notes the session may not show, a note
// shown both above and there being one note, sent once; and they are counted,
// since the notes the brief is the first to read follow them in the same form.
// Those are said to be ones the turn had not read before the message, never new
// to the session: which notes the turn has read is its record's word, and a
// harness can be handed a note before the record says so. None told again says
// nothing.
//
// Mutation: drop the header, its one-note sentence or its count, call the notes
// after them new, or say something where there is no note, and a check goes
// red.
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
		if header := strings.TrimSuffix(got, strings.Join(told, "\n\n")); strings.Contains(header, " new") ||
			!strings.Contains(header, "A note after them is one the turn had not read before this message.") {
			t.Errorf("%d notes told again say the notes after them are new rather than ones the turn had not "+
				"read:\n%s", notes, got)
		}
		if !strings.HasSuffix(got, "\n\n"+strings.Join(told, "\n\n")) {
			t.Errorf("%d notes told again are not told after their header, in order:\n%s", notes, got)
		}
	}
}

// A LOST TURN'S ENDING NAMES NO BOX IT MAY STILL HAVE. A turn ends lost wherever
// no frame of it saw it end — a job lost with its box, a resume that broke, a
// seat removed, an end never recorded and ended by the retention duty a day
// after — and only the first of those lost a box: the account of how the
// conversation's earlier turns ended says the engine ended it, and leaves what
// ended it to the engine's own failure beside it, never that its executor's box
// or host was lost, which the lead of the next turn on its session and that
// failure would both contradict.
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
