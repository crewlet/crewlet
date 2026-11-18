package gitlet_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// A GIT READ IS SERVED UNTIL ITS ANSWER IS DUE. Its context ends a reply's
// margin before the asker's deadline with cause gitlet.ErrAnswerDue, which
// AnswerDue tells from every other end: a caller gone, an earlier deadline of
// its own, or a live context is no answer falling due. A zero deadline cuts
// nothing. In a bubble the context is seen ended at exactly the deadline less
// the margin, to the nanosecond, however slow the runner.
//
// Mutation: cut at the deadline itself rather than a margin before it — red.
func TestAReadIsServedUntilItsAnswerIsDue(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		deadline := time.Now().Add(gitlet.ReplyMargin + 50*time.Millisecond)
		cut := deadline.Add(-gitlet.ReplyMargin)
		ctx, cancel := gitlet.AnswerBy(t.Context(), deadline)
		defer cancel()
		if got, ok := ctx.Deadline(); !ok || !got.Equal(cut) {
			t.Fatalf("a read served for an asker waiting until %v ends at %v (%v), want a reply's margin before it",
				deadline, got, ok)
		}
		if gitlet.AnswerDue(ctx) {
			t.Error("a read whose answer is not due yet says it is")
		}
		<-ctx.Done()
		if !gitlet.AnswerDue(ctx) {
			t.Errorf("a read cut at its answer's due instant ended %v (cause %v), want it due", ctx.Err(),
				context.Cause(ctx))
		}
		if at := time.Now(); !at.Equal(cut) {
			t.Errorf("the read's answer fell due at %v, want its cut %v: a reply's margin before its asker's "+
				"deadline %v", at, cut, deadline)
		}

		parent, leave := context.WithCancel(t.Context())
		gone, stop := gitlet.AnswerBy(parent, time.Now().Add(time.Hour))
		defer stop()
		leave()
		<-gone.Done()
		if gitlet.AnswerDue(gone) {
			t.Error("a read whose caller went away says its answer fell due: nobody listens for that answer")
		}

		early, stopEarly := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer stopEarly()
		sooner, stopSooner := gitlet.AnswerBy(early, time.Now().Add(time.Hour))
		defer stopSooner()
		<-sooner.Done()
		if gitlet.AnswerDue(sooner) {
			t.Error("a read its caller's own earlier deadline ended says its answer fell due")
		}

		uncut, stopUncut := gitlet.AnswerBy(t.Context(), time.Time{})
		defer stopUncut()
		if _, ok := uncut.Deadline(); ok {
			t.Error("a read served with no asker's deadline was given one")
		}
	})
}
