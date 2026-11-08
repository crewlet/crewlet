package prompts

import (
	"strings"
	"testing"
	"time"
)

// A NEXT TURN ON A SESSION ITS TURN WENT PAST IS TOLD SO. The lead of a run that
// continues the session its conversation's last turn left says where the
// session ends: through that turn, which ended at its instant, where the
// session is the turn's last word; up to the session, kept while the turn still
// worked at the instant it was kept, what the turn did after it not above, where
// the turn went on past it; and, where the turn was lost, that the session may
// not hold the last of what it did — each saying how that turn ended is told
// below, and a schedule's fire in the schedule's words.
//
// Mutation: tell every base through its turn's end, and the went-on and lost
// leads say so; tell a lost turn the went-on lead, and it is said to have gone
// on past a session it may not have; leave out the instant the session was
// kept, and the run cannot tell where it ends.
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
		{name: "a turn that was lost",
			m: NextTurn{BaseTurn: "t-1", EndedAt: ended, CapturedAt: kept, WentOn: true, Lost: true},
			says: []string{"through turn `t-1`, which was lost with its executor's box or host and ended at " +
				"2026-11-08T10:00:00Z: the session above may not hold the last of what that turn did.",
				"How turn `t-1` ended, among the conversation's earlier turns, is told below."},
			never: []string{"still worked", "which ended at"}},
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
