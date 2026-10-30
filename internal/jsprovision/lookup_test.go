package jsprovision

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// A LOOKUP NOBODY ANSWERED IS ASKED AGAIN UNTIL SOMEBODY DOES. On a cluster
// only an object's leader answers its info, so while its group elects every
// request goes unanswered; the lookup asks again and returns the answer the
// third request got.
//
// Mutation: ask once (the bare request the cluster tests made), and the
// lookup fails on the first silence.
func TestALookupIsAskedAgainUntilItIsAnswered(t *testing.T) {
	t.Parallel()
	calls := 0
	err := fast.lookup(t.Context(), func(context.Context) error {
		calls++
		if calls <= 2 {
			return nats.ErrTimeout
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("lookup = %v after %d call(s), want the third call's answer", err, calls)
	}
}

// A LOOKUP NOBODY EVER ANSWERS GIVES UP, with the last request's own
// silence: when its caller's deadline ends first, that deadline is what ended
// it; with no deadline of the caller's, its own budget is.
//
// JUDGED BY OUTCOME, not by elapsed time, which a loaded runner stretches:
// the error, and which context had run out when it came back. The only clock
// here is a watchdog many budgets long, so an unbounded lookup fails rather
// than hangs.
//
// Mutation: re-ask without a bound, and the case with no caller's deadline
// never returns.
func TestALookupNobodyAnswersGivesUpWithinItsBudget(t *testing.T) {
	t.Parallel()
	silent := func(context.Context) error { return nats.ErrTimeout }
	for name, callerDeadline := range map[string]bool{
		"the caller's deadline": true,
		"its own budget":        false,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			if callerDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, fast.ReAsk)
				defer cancel()
			}
			done := make(chan error, 1)
			go func() { done <- fast.lookup(ctx, silent) }()
			var err error
			select {
			case err = <-done:
			case <-time.After(20 * fast.Lookup):
				t.Fatalf("a lookup nobody answered was still asking after %v", 20*fast.Lookup)
			}
			if !errors.Is(err, nats.ErrTimeout) {
				t.Errorf("lookup = %v, want the last request's silence", err)
			}
			switch {
			case callerDeadline && !errors.Is(ctx.Err(), context.DeadlineExceeded):
				t.Errorf("the lookup gave up while its caller's deadline still stood (%v)", ctx.Err())
			case !callerDeadline && ctx.Err() != nil:
				t.Errorf("the caller's context ended (%v), so the lookup's own budget did not", ctx.Err())
			}
		})
	}
}

// A STREAM'S INFO THAT NAMES NO LEADER IS ASKED AGAIN. A member answers from
// its own copy once its group has been leaderless for a while, and that copy
// can be short of what the quorum committed: it is no answer, and the lookup
// returns the info a leader answered.
//
// Mutation: take the first info that comes back, and the lookup returns a
// member's copy naming no leader.
func TestAStreamInfoNamingNoLeaderIsAskedAgain(t *testing.T) {
	t.Parallel()
	answers := []*jetstream.StreamInfo{
		{Cluster: &jetstream.ClusterInfo{Leader: ""}, State: jetstream.StreamState{LastSeq: 3}},
		{Cluster: &jetstream.ClusterInfo{Leader: ""}, State: jetstream.StreamState{LastSeq: 3}},
		{Cluster: &jetstream.ClusterInfo{Leader: "member-2"}, State: jetstream.StreamState{LastSeq: 5}},
	}
	calls := 0
	var got *jetstream.StreamInfo
	err := fast.lookup(t.Context(), func(context.Context) error {
		got = answers[calls]
		calls++
		return leaderAnswered("KV_x", got)
	})
	if err != nil || calls != 3 || got.Cluster.Leader != "member-2" || got.State.LastSeq != 5 {
		t.Fatalf("lookup = %v after %d call(s), got %+v; want the leader's answer, the third", err, calls, got)
	}
	if err := leaderAnswered("KV_x", &jetstream.StreamInfo{}); err != nil {
		t.Errorf("a stream on one server (no cluster) was refused: %v", err)
	}
}
