package executordomain

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// These cases apply an end naming a session that ran out through the shipped applier,
// and read the head the next turn would read.

// capturingSession is a capture of turn naming session, as a harness that keeps
// one id across its resumes names it.
func capturingSession(seat, conversation, turn, session string, launch int64) Capture {
	c := capturing(seat, conversation, turn, launch, 1, 100, budget)
	c.SessionID = session
	return c
}

// exhausted is a failed turn's ending naming the sessions it ran out of context in.
func exhausted(sessions ...string) executorkeep.Ending {
	return executorkeep.Ending{Outcome: executorkeep.OutcomeFailed, Exhausted: sessions}
}

// completed is an ending that says nothing about any session.
var completed = executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}

// The next turn is handed no session that ran out, says why, and is still told what
// came before, since nothing stands in for it.
// Mutation: drop retireExhausted, or let the end absorb, and it goes red.
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
	if why := h.RetiredWhy(); !strings.Contains(why, "s-1") || !strings.Contains(why, "ran out of its model's context") {
		t.Errorf("the read says %q of its head, want the session named and why it went", why)
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

	if h := e.head("ana", "chat:room"); h.Session != nil || h.RetiredWhy() == "" {
		t.Fatalf("the next turn is handed %+v (retired: %q), want none, said", h.Session, h.RetiredWhy())
	}
	if s, _ := e.session("ana", a); s.HasObject() || s.RetiredBy != b {
		t.Errorf("the base's row is %+v, want it retired by b", s)
	}
}

// A hold keeps a session for a relaunch, which here would only meet the same end.
// Mutation: skip a held row in retireExhausted and the base still names it.
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
		Exhausted: []string{"s-1"}}))

	h := e.head("ana", "chat:room")
	if h.Session == nil || h.Session.Turn != b || h.Session.SessionID != "s-2" {
		t.Fatalf("the next turn is handed %+v, want b's session s-2", h.Session)
	}
	if h.RetiredWhy() != "" {
		t.Errorf("a read that names a head says %q of a retirement", h.RetiredWhy())
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
	if h := e.head("ana", "chat:room"); h.Session == nil || h.Session.Turn != turn || h.RetiredWhy() != "" {
		t.Fatalf("the next turn is handed %+v (retired: %q), want the failed turn's session", h.Session, h.RetiredWhy())
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

// An end whose capture goes absorbs nothing, whichever way it goes: nothing holds what it read.
// Mutation: set keepsSession from the retired ids alone and the base is absorbed.
func TestAnEndWhoseCaptureGoesWithItAbsorbsNothing(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	a, b := newTurn(), newTurn()
	e.capture(capturingSession("ana", "chat:room", a, "s-1", 1))
	e.end(ending("ana", "chat:room", a, "", completed))
	read := e.head("ana", "chat:room")
	e.capture(launching(read, capturingSession("ana", "chat:room", b, "s-2", 1)))
	e.end(ending("ana", "chat:room", b, "", executorkeep.Ending{Outcome: executorkeep.OutcomeFailed,
		Exhausted: []string{"s-3"},
		Unkept:    &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1, Reason: "the job ran out"}}))

	if s, _ := e.session("ana", b); s.HasObject() {
		t.Errorf("the turn's row still names its older capture: %+v", s)
	}
	s, found := e.session("ana", a)
	if !found {
		t.Fatal("the end absorbed turn a with no session of its own to stand in for it")
	}
	if !s.HasObject() {
		t.Errorf("turn a's session went, and nothing ran out in it: %+v", s)
	}
	if h := e.head("ana", "chat:room"); h.Session == nil || h.Session.Turn != a {
		t.Errorf("the next turn is handed %+v, want a's session, which never ran out", h.Session)
	}
}

// A retired row says so where its newest attempt was not kept too: the usual way a kept
// session runs out, where a read with no reason would log nothing.
// Mutation: release the stale capture before the retirement and RetiredBy is empty.
func TestARetiredRowSaysSoWhereItsNewestAttemptWasNotKept(t *testing.T) {
	t.Parallel()
	e := newEstate(t)
	turn := newTurn()
	e.capture(capturingSession("ana", "chat:room", turn, "s-1", 1))
	e.end(ending("ana", "chat:room", turn, "", executorkeep.Ending{Outcome: executorkeep.OutcomeFailed,
		Exhausted: []string{"s-1"},
		Unkept:    &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1, Reason: "the job ran out"}}))
	if s, _ := e.session("ana", turn); s.HasObject() || s.RetiredBy != turn {
		t.Errorf("the row is %+v, want it naming no object, retired by its own turn", s)
	}
	if h := e.head("ana", "chat:room"); h.Session != nil {
		t.Errorf("the next turn is handed %+v, want none", h.Session)
	}
}
