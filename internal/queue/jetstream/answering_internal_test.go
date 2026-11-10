package jetstream

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
)

// A WITHDRAWN REGISTRATION LEAVES NOTHING IN THE RECORD [Queue.Answering]
// READS. The record is this client's own, kept for the conformance suite, and a
// withdrawal that left its gate there would keep every withdrawn registration's
// gate for the queue's life and go on summing it into what the client reports
// it is answering on that subject.
//
// Mutation: drop the unanswer at the top of the withdrawal, and the record
// keeps both gates.
func TestAWithdrawnRegistrationLeavesNoGateBehind(t *testing.T) {
	t.Parallel()
	q := openForTest(t, Config{})
	ctx := t.Context()
	const subject = "crewlet.scatter.record"
	answer := func(context.Context, []byte) ([]byte, error) { return nil, nil }
	gates := func() int {
		q.mu.Lock()
		defer q.mu.Unlock()
		return len(q.answering[subject])
	}

	first, err := q.Serve(ctx, subject, queue.ConcurrentAnswers, answer)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	second, err := q.Serve(ctx, subject, 1, answer)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if n := gates(); n != 2 {
		t.Fatalf("two registrations on %s left %d gates in the record, want 2", subject, n)
	}
	if err := first(ctx); err != nil {
		t.Fatalf("withdraw the first: %v", err)
	}
	if n := gates(); n != 1 {
		t.Fatalf("one of two registrations withdrawn left %d gates in the record, want 1", n)
	}
	if err := second(ctx); err != nil {
		t.Fatalf("withdraw the second: %v", err)
	}
	q.mu.Lock()
	_, kept := q.answering[subject]
	q.mu.Unlock()
	if kept {
		t.Fatalf("both registrations on %s withdrawn, and the record still holds the subject", subject)
	}
}
