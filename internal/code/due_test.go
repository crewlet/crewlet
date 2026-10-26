package code_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
)

// A GIT READ IS SERVED UNTIL ITS ANSWER IS DUE. The context a git read is
// served under ends a reply's margin before its asker's deadline, its cause
// code.ErrAnswerDue, so the read answers while the asker still listens and
// can tell that end (AnswerDue) from every other: a caller gone, or an
// earlier deadline of the context's own, is no answer falling due, and a
// context still live is none either. A zero deadline is no asker on a wire,
// and cuts nothing.
//
// Mutation: cut at the deadline itself rather than a margin before it — red.
func TestAReadIsServedUntilItsAnswerIsDue(t *testing.T) {
	t.Parallel()
	deadline := time.Now().Add(code.ReplyMargin + 50*time.Millisecond)
	ctx, cancel := code.AnswerBy(t.Context(), deadline)
	defer cancel()
	if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline.Add(-code.ReplyMargin)) {
		t.Fatalf("a read served for an asker waiting until %v ends at %v (%v), want a reply's margin before it",
			deadline, got, ok)
	}
	if code.AnswerDue(ctx) {
		t.Error("a read whose answer is not due yet says it is")
	}
	<-ctx.Done()
	if !code.AnswerDue(ctx) {
		t.Errorf("a read cut at its answer's due instant ended %v (cause %v), want it due", ctx.Err(),
			context.Cause(ctx))
	}
	if !time.Now().Before(deadline) {
		t.Errorf("the read's answer fell due at %v, past its asker's deadline %v", time.Now(), deadline)
	}

	parent, leave := context.WithCancel(t.Context())
	gone, stop := code.AnswerBy(parent, time.Now().Add(time.Hour))
	defer stop()
	leave()
	<-gone.Done()
	if code.AnswerDue(gone) {
		t.Error("a read whose caller went away says its answer fell due: nobody listens for that answer")
	}

	early, stopEarly := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer stopEarly()
	sooner, stopSooner := code.AnswerBy(early, time.Now().Add(time.Hour))
	defer stopSooner()
	<-sooner.Done()
	if code.AnswerDue(sooner) {
		t.Error("a read its caller's own earlier deadline ended says its answer fell due")
	}

	uncut, stopUncut := code.AnswerBy(t.Context(), time.Time{})
	defer stopUncut()
	if _, ok := uncut.Deadline(); ok {
		t.Error("a read served with no asker's deadline was given one")
	}
}
