package executordomain

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// These cases apply, through the shipped applier, an end naming a session that
// ran out, and read the head the next turn would read.

// capturingSession is a capture of turn naming session, as a harness that keeps
// one id across its resumes names it.
func capturingSession(seat, conversation, turn, session string, launch int64) Capture {
	c := capturing(seat, conversation, turn, launch, 1, 100, budget)
	c.SessionID = session
	return c
}

// exhausted is a failed turn's ending naming the sessions it ran out of context in.
func exhausted(sessions ...string) executorkeep.Ending {
	return spentBy(executorkeep.LossContextExhausted, sessions...)
}

// spentBy is a failed turn's ending naming the sessions loss spent.
func spentBy(loss executorkeep.Loss, sessions ...string) executorkeep.Ending {
	spent := make([]executorkeep.SpentSession, 0, len(sessions))
	for _, session := range sessions {
		spent = append(spent, executorkeep.SpentSession{Session: session, Loss: loss})
	}
	return executorkeep.Ending{Outcome: executorkeep.OutcomeFailed, SpentSessions: spent}
}

// completed is an ending that says nothing about any session.
var completed = executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}

// retiredIn is the turn whose end retired the newest retired session the head's
// endings name, "" where none did: what the next turn reads a retirement by
// (internal/engine's conversationLoss).
func retiredIn(h Head) string {
	for _, ended := range slices.Backward(h.Endings) {
		if ended.RetiredBy != "" {
			return ended.RetiredBy
		}
	}
	return ""
}

// The next turn is handed no session that ran out, says why, and is still told what
// came before, since nothing stands in for it.
// Mutation: drop retireSpent, or let the end absorb, and it goes red.
func TestAnEndThatRanOutOfContextLeavesTheConversationNoHead(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	a, b := newTurn(), newTurn()
	e.capture(capturingSession("ana", "chat:room", a, "s-1", 1))
	e.end(ending("ana", "chat:room", a, "", completed))
	read := e.head("ana", "chat:room")
	e.capture(launching(read, capturingSession("ana", "chat:room", b, "s-1", 1)))
	e.end(ending("ana", "chat:room", b, "", exhausted("s-1")))

	h := e.head("ana", "chat:room")
	if h.Session != nil {
		t.Fatalf("the next turn is handed %+v, want no session: this one ran out of context", h.Session)
	}
	if by := retiredIn(h); by != b {
		t.Errorf("the read's endings name the retirement by %q, want b's, which ran its session out", by)
	}
	if len(h.Endings) != 2 {
		t.Errorf("the next turn is told %d endings, want a's and b's: nothing stands in for them now", len(h.Endings))
	}
	for name, turn := range map[string]string{"a": a, "b": b} {
		s, found := e.session("ana", turn)
		if !found || s.HasObject() || s.RetiredBy != b || s.SessionID != "s-1" {
			t.Errorf("turn %s's row is (%+v, %v), want it naming no object, retired by b", name, s, found)
		}
		if !strings.Contains(s.NotKept(), "ran out of its model's context") {
			t.Errorf("turn %s's row says %q of its session, want why it went", name, s.NotKept())
		}
		if s.Ending == nil {
			t.Errorf("turn %s's ending went with its session", name)
		}
	}
	if s, _ := e.session("ana", b); s.Object.Size != 0 {
		t.Errorf("turn b's row still counts %d bytes of a session it no longer names", s.Object.Size)
	}
}

// A capture that never landed leaves the base, which is the same session.
// Mutation: retire only the ending turn's own row and the head is the base.
func TestARetiredSessionGoesFromTheBaseWhereNoCaptureLanded(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	a, b := newTurn(), newTurn()
	e.capture(capturingSession("ana", "chat:room", a, "s-1", 1))
	e.end(ending("ana", "chat:room", a, "", completed))
	e.begin(beginning(e.head("ana", "chat:room"), "ana", "chat:room", b, ""))
	e.end(ending("ana", "chat:room", b, "", exhausted("s-1")))

	if h := e.head("ana", "chat:room"); h.Session != nil || retiredIn(h) != b {
		t.Fatalf("the next turn is handed %+v (retired by %q), want none, retired by b", h.Session, retiredIn(h))
	}
	if s, _ := e.session("ana", a); s.HasObject() || s.RetiredBy != b {
		t.Errorf("the base's row is %+v, want it retired by b", s)
	}
}

// A hold keeps a session for a relaunch, which here would only meet the same end.
// Mutation: skip a held row in retireSpent and the base still names it.
func TestARetiredSessionGoesFromARowALiveTurnHolds(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	a, b, c := newTurn(), newTurn(), newTurn()
	e.capture(capturingSession("ana", "chat:room", a, "s-1", 1))
	e.end(ending("ana", "chat:room", a, "", completed))
	read := e.head("ana", "chat:room")
	e.begin(beginning(read, "ana", "chat:room", b, ""))
	e.begin(beginning(read, "ana", "chat:room", c, ""))
	if !e.holds("ana", a) {
		t.Fatal("the premise: no live turn holds the base")
	}
	e.end(ending("ana", "chat:room", b, "", exhausted("s-1")))
	if s, _ := e.session("ana", a); s.HasObject() {
		t.Errorf("the row a live turn holds still names the retired session: %+v", s)
	}
	if h, _ := e.session("ana", c); h.Ended() || h.BaseTurn != a {
		t.Errorf("the retirement reached the live turn's own row: %+v", h)
	}
}

// A run that succeeds after a fresh start is the new head, and absorbs as any does.
// Mutation: retire the ending turn's row whatever it names and the session that worked goes.
func TestASessionStartedAfterRunningOutIsTheNextHead(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	a, b := newTurn(), newTurn()
	e.capture(capturingSession("ana", "chat:room", a, "s-1", 1))
	e.end(ending("ana", "chat:room", a, "", completed))
	read := e.head("ana", "chat:room")
	e.capture(launching(read, capturingSession("ana", "chat:room", b, "s-2", 2)))
	e.end(ending("ana", "chat:room", b, "", executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted,
		SpentSessions: exhausted("s-1").SpentSessions}))

	h := e.head("ana", "chat:room")
	if h.Session == nil || h.Session.Turn != b || h.Session.SessionID != "s-2" {
		t.Fatalf("the next turn is handed %+v, want b's session s-2", h.Session)
	}
	if _, found := e.session("ana", a); found {
		t.Error("b's end absorbed nothing, want a absorbed: its session holds what a's ending said")
	}
}

// A run that failed any other way keeps its session: another launch may well work.
// Mutation: retire a failed end's own row and the head is gone.
func TestAnEndThatFailedAnyOtherWayKeepsItsSession(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	turn := newTurn()
	e.capture(capturingSession("ana", "chat:room", turn, "s-1", 1))
	e.end(ending("ana", "chat:room", turn, "", executorkeep.Ending{Outcome: executorkeep.OutcomeFailed,
		Failure: "the harness exited with status 1"}))
	if h := e.head("ana", "chat:room"); h.Session == nil || h.Session.Turn != turn || retiredIn(h) != "" {
		t.Fatalf("the next turn is handed %+v (retired by %q), want the failed turn's session", h.Session,
			retiredIn(h))
	}
}

// Only that session, that conversation and that seat: a sibling on another session is the head.
// Mutation: drop either key from retireStatement, or the empty-conversation guard, and a row goes.
func TestARetirementReachesNoOtherSessionOrConversationOrSeat(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	a, sibling, elsewhere, otherSeat, keyless, keylessOther, b := newTurn(), newTurn(), newTurn(), newTurn(),
		newTurn(), newTurn(), newTurn()
	e.capture(capturingSession("ana", "chat:room", a, "s-1", 1))
	e.end(ending("ana", "chat:room", a, "", completed))
	e.capture(capturingSession("ana", "chat:room", sibling, "s-9", 1))
	e.end(ending("ana", "chat:room", sibling, "", completed))
	e.capture(capturingSession("ana", "chat:other", elsewhere, "s-1", 1))
	e.end(ending("ana", "chat:other", elsewhere, "", completed))
	e.capture(capturingSession("bea", "chat:room", otherSeat, "s-1", 1))
	e.end(ending("bea", "chat:room", otherSeat, "", completed))
	e.capture(capturingSession("ana", "", keylessOther, "s-1", 1))
	e.end(ending("ana", "", keylessOther, "", completed))
	e.capture(capturingSession("ana", "", keyless, "s-1", 1))
	e.end(ending("ana", "", keyless, "", exhausted("s-1")))
	if s, _ := e.session("ana", keylessOther); !s.HasObject() {
		t.Errorf("a turn with no conversation retired another such turn's session: %+v", s)
	}

	e.begin(beginning(e.head("ana", "chat:room"), "ana", "chat:room", b, ""))
	e.end(ending("ana", "chat:room", b, "", exhausted("s-1")))

	if s, _ := e.session("ana", a); s.HasObject() {
		t.Errorf("the row naming the session that ran out still names it: %+v", s)
	}
	h := e.head("ana", "chat:room")
	if h.Session == nil || h.Session.Turn != sibling {
		t.Errorf("the next turn is handed %+v, want the sibling, which ran on another session", h.Session)
	}
	for name, row := range map[string]struct{ seat, turn string }{
		"another conversation": {"ana", elsewhere}, "another seat": {"bea", otherSeat},
	} {
		if s, _ := e.session(row.seat, row.turn); !s.HasObject() {
			t.Errorf("the retirement reached %s: %+v", name, s)
		}
	}
}

// An end whose capture goes absorbs nothing, whichever way it goes: nothing holds what it
// read. It goes spent (stamped spentSessionsVersion), or released for a newer unkept capture
// by an end stamped below wentOnVersion; the two never meet in one end.
// Mutation: drop the spent check or !stale from keepsSession and a case absorbs the base.
func TestAnEndWhoseCaptureGoesWithItAbsorbsNothing(t *testing.T) {
	t.Parallel()
	unkept := &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1, Reason: "the object store refused it"}
	for name, end := range map[string]func(e *estate, b string){
		"spent": func(e *estate, b string) {
			e.end(ending("ana", "chat:room", b, "", exhausted("s-2")))
		},
		"released below four": func(e *estate, b string) {
			e.applyEncoded(OpEnd, e.endStamped(ending("ana", "chat:room", b, "",
				executorkeep.Ending{Outcome: executorkeep.OutcomeFailed, Unkept: unkept}), 3))
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEstate(t)
			a, b := newTurn(), newTurn()
			e.capture(capturingSession("ana", "chat:room", a, "s-1", 1))
			e.end(ending("ana", "chat:room", a, "", completed))
			read := e.head("ana", "chat:room")
			e.capture(launching(read, capturingSession("ana", "chat:room", b, "s-2", 1)))
			end(e, b)

			if s, _ := e.session("ana", b); s.HasObject() {
				t.Errorf("the turn's row still names its capture: %+v", s)
			}
			s, found := e.session("ana", a)
			if !found {
				t.Fatal("the end absorbed turn a with no session of its own to stand in for it")
			}
			if !s.HasObject() {
				t.Errorf("turn a's session went, and nothing spent it: %+v", s)
			}
			if h := e.head("ana", "chat:room"); h.Session == nil || h.Session.Turn != a {
				t.Errorf("the next turn is handed %+v, want a's session, which nothing spent", h.Session)
			}
		})
	}
}

// A RETIRED ROW SAYS HOW ITS SESSION WAS SPENT, as the end that spent it named
// it, on every row the session goes from, the spending turn's own among them
// though its newest attempt was not kept: never read off the end's own unkept
// capture, which here is another loss of another session (a fresh one the store
// refused after the provider refused the kept one's request).
//
// Mutation: retire with no loss, or with the end's unkept capture's, and a case
// goes red.
func TestARetiredRowSaysHowItsSessionWasSpent(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	a, b := newTurn(), newTurn()
	e.capture(capturingSession("ana", "chat:room", a, "s-1", 1))
	e.end(ending("ana", "chat:room", a, "", completed))
	e.capture(launching(e.head("ana", "chat:room"), capturingSession("ana", "chat:room", b, "s-1", 1)))
	refused := spentBy(executorkeep.LossRequestRefused, "s-1")
	refused.Unkept = &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1, Loss: executorkeep.LossStoreRefused,
		Reason: "the object store refused the fresh session"}
	refused.WentOn = true
	e.end(ending("ana", "chat:room", b, "", refused))

	for name, turn := range map[string]string{"a": a, "b": b} {
		s, _ := e.session("ana", turn)
		if s.HasObject() || s.RetiredBy != b || s.RetiredLoss != executorkeep.LossRequestRefused {
			t.Errorf("turn %s's row is retired by %q for %q, want by b for %q", name, s.RetiredBy, s.RetiredLoss,
				executorkeep.LossRequestRefused)
		}
		if why := s.NotKept(); !strings.Contains(why, executorkeep.LossRequestRefused.Words()) ||
			strings.Contains(why, executorkeep.LossContextExhausted.Words()) {
			t.Errorf("turn %s's row says %q of its session, want that its provider refused it, and only that",
				name, why)
		}
	}
}

// AN END THAT SPENDS A SESSION IS STAMPED FOR THE BUILD THAT RETIRES IT: one
// naming its spent sessions is stamped at spentSessionsVersion, so a build
// reading less retains it rather than apply it and leave the session on its
// rows alone; an end naming none is stamped below it.
//
// Mutation: drop the spent sessions' versioned field row and it goes red.
func TestAnEndsSpentSessionsAreStampedForTheBuildThatRetiresThem(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	turn := newTurn()
	subject := ConversationSubject(servedSeat, servedChat, turn)
	stampOf := func(end executorkeep.Ending) int {
		env, err := DecodeEnvelope(e.encode(subject, OpEnd, seatScope(servedSeat),
			ending(servedSeat, servedChat, turn, "", end)))
		if err != nil {
			t.Fatalf("decode the end: %v", err)
		}
		return env.V
	}
	if got := stampOf(exhausted("s-1")); got != spentSessionsVersion {
		t.Errorf("an end naming a spent session is stamped %d, want %d", got, spentSessionsVersion)
	}
	if got := stampOf(completed); got >= spentSessionsVersion {
		t.Errorf("an end naming none is stamped %d, which a build reading %d retains", got,
			spentSessionsVersion-1)
	}
}
