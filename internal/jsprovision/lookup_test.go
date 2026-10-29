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

// A LOOKUP NOBODY EVER ANSWERS GIVES UP: within its caller's deadline when
// that is the shorter, and within its own budget otherwise, with the last
// request's own error.
//
// Mutation: re-ask without a bound, and neither case returns.
func TestALookupNobodyAnswersGivesUpWithinItsBudget(t *testing.T) {
	t.Parallel()
	silent := func(context.Context) error { return nats.ErrTimeout }
	for name, tc := range map[string]struct {
		parent time.Duration
		within time.Duration
	}{
		"the caller's deadline": {parent: 3 * fast.ReAsk / 2, within: fast.Lookup},
		"its own budget":        {parent: 0, within: 2 * fast.Lookup},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			if tc.parent > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.parent)
				defer cancel()
			}
			done := make(chan error, 1)
			go func() { done <- fast.lookup(ctx, silent) }()
			select {
			case err := <-done:
				if !errors.Is(err, nats.ErrTimeout) {
					t.Errorf("lookup = %v, want the last request's silence", err)
				}
			case <-time.After(tc.within):
				t.Fatalf("a lookup nobody answered was still asking after %v", tc.within)
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
