package statelog_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A WRITE NEVER DECIDES FROM A STATE BELOW THE CALLER'S OWN PREVIOUS WRITE.
//
// [statelog.Request.Session] is the one input the framework cannot derive:
// only the caller knows that the record which last moved this subject is its
// own and is still in flight. Until a caller filled it the field had no
// producer at all — a wait, a refusal reason and a histogram, over a value
// nothing set — so what the framework did instead was open a snapshot, run the
// decide, ask the broker for the subject's last sequence, be refused, and only
// then wait for exactly the position the caller had been holding all along.
//
// The two arms below are the whole contract: the wait is OUTSIDE the round
// loop, so a mark the node has not reached stops the decide from running at
// all, and a mark it has already passed costs nothing.
func TestASessionMarkIsWaitedForBeforeAnySnapshotOpens(t *testing.T) {
	t.Parallel()

	t.Run("behind refuses before the decide runs", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		// AN APPLIER THAT DOES NOT ANSWER, which is what a node that
		// cannot catch up looks like from here: the mark below is a
		// position it never reaches and the wait spends its whole
		// budget.
		h.applier.mu.Lock()
		h.applier.stalled = true
		h.applier.mu.Unlock()

		mark := statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_000}
		decided := 0
		res, err := h.pub.Publish(t.Context(), sessionWrite(probeSubject("a"),
			"op-1", mark, func() { decided++ }))
		var refusal *statelog.Unavailable
		switch {
		case err == nil:
			t.Fatal("a write below the caller's own previous write was accepted")
		case !errors.As(err, &refusal):
			t.Fatalf("the refusal is %v, want a named one a caller can act on", err)
		case refusal.Reason != statelog.ReasonBehind:
			t.Fatalf("the reason is %q, want %q", refusal.Reason, statelog.ReasonBehind)
		case refusal.Position != mark:
			t.Fatalf("the refusal names %s and the caller's own write is at %s "+
				"— the position is what the caller retries against",
				refusal.Position, mark)
		}
		// THE DECIDE NEVER RAN. That is the whole of what the mark buys
		// over the framework's own recovery: no expectation is formed
		// from rows the caller's own record is about to move, and no
		// decision is computed and thrown away.
		if decided != 0 {
			t.Errorf("the decide ran %d time(s) against a state below the "+
				"caller's own write; the session wait is outside the round "+
				"loop precisely so it cannot", decided)
		}
		if res.Rounds != 0 {
			t.Errorf("the write reports %d round(s) and opened no snapshot",
				res.Rounds)
		}
		// AND IT SAYS WHOSE WRITE IT IS. "A colleague is editing this"
		// and "my own applier is lagging" are the same refusal reason
		// and different things to do about it.
		if !strings.Contains(refusal.Detail, "own") {
			t.Errorf("the detail is %q and does not say the write it is "+
				"waiting for is the caller's own", refusal.Detail)
		}
	})

	t.Run("a mark already reached costs nothing", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		first, err := h.write(probeSubject("a"), "op-1", "hello")
		if err != nil {
			t.Fatalf("the first write: %v", err)
		}
		decided := 0
		if _, err := h.pub.Publish(t.Context(), sessionWrite(probeSubject("a"),
			"op-2", first.Position, func() { decided++ })); err != nil {
			t.Fatalf("a write marked with a position this node has already "+
				"applied was refused: %v", err)
		}
		if decided == 0 {
			t.Error("the decide never ran, so the wait did not fall through " +
				"for a mark the node is already past")
		}
	})
}

// sessionWrite is one arbitrated write carrying a session mark, counting every
// time its decide is entered.
func sessionWrite(subject statelog.Subject, opID string, session statelog.Position,
	decided func()) statelog.Request {

	return statelog.Request{
		Subject:  subject,
		Scope:    statelog.ScopeSet{Paths: []string{subject.String()}},
		OpID:     opID,
		MintedAt: time.Now(),
		Pattern:  statelog.PatternArbitrated,
		Session:  session,
		Decide: func(*sql.Tx) (statelog.Decision, error) {
			decided()
			return statelog.Decision{Payload: []byte("body"), Version: 1}, nil
		},
	}
}
