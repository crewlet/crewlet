package jsprovision

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
	got, err := lookupWith(t.Context(), fast, func(context.Context) (int, error) {
		calls++
		if calls <= 2 {
			return 0, nats.ErrTimeout
		}
		return calls, nil
	})
	if err != nil || calls != 3 || got != 3 {
		t.Fatalf("lookup = %d, %v after %d call(s), want the third call's answer", got, err, calls)
	}
}

// A LOOKUP RETURNS THE ANSWER THAT CAME, even when a request it presumed lost
// ends after it. The read keeps such a request outstanding while it asks
// again, and once the next one is answered it cancels the first and waits for
// it to end. So the cut-off request is the LAST to return, with no value.
// A lookup that took its answer from a variable each request assigned
// returned that request's nil instead. On a partitioned cluster that handed
// observeBucket a nil bucket, and the nil dereference stopped the test binary.
//
// Mutation: have the lookup keep the value each request returns in a
// variable they share, as LookupBucket once did, and this returns nil.
func TestALookupAnsweredLateKeepsTheAnswerNotTheRequestCutOff(t *testing.T) {
	t.Parallel()
	answer := &jetstream.StreamInfo{Config: jetstream.StreamConfig{Name: "KV_x"}}
	var asks atomic.Int32
	got, err := lookupWith(t.Context(), fast, func(ctx context.Context) (*jetstream.StreamInfo, error) {
		if asks.Add(1) == 1 {
			// LOST: nobody answers it until the read cuts it off.
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return answer, nil
	})
	if err != nil || got != answer {
		t.Fatalf("lookup = %+v, %v after %d request(s), want the second request's answer",
			got, err, asks.Load())
	}
	if n := asks.Load(); n != 2 {
		t.Fatalf("the lookup sent %d request(s), want 2 — the case needs one presumed "+
			"lost and outstanding when the next is answered", n)
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
	silent := func(context.Context) (struct{}, error) { return struct{}{}, nats.ErrTimeout }
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
			go func() { _, err := lookupWith(ctx, fast, silent); done <- err }()
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
	got, err := lookupWith(t.Context(), fast, func(context.Context) (*jetstream.StreamInfo, error) {
		info := answers[calls]
		calls++
		if err := leaderAnswered("KV_x", info); err != nil {
			return nil, err
		}
		return info, nil
	})
	if err != nil || calls != 3 || got.Cluster.Leader != "member-2" || got.State.LastSeq != 5 {
		t.Fatalf("lookup = %v after %d call(s), got %+v; want the leader's answer, the third", err, calls, got)
	}
	if err := leaderAnswered("KV_x", &jetstream.StreamInfo{}); err != nil {
		t.Errorf("a stream on one server (no cluster) was refused: %v", err)
	}
}

// A READ WHOSE ANSWER IS STILL ARRIVING IS NEITHER ASKED AGAIN NOR CUT OFF AT
// THE CEILING.
//
// A stream leader's reply that carries a value of megabytes is built by the
// leader, crosses whatever link stands between, and is decoded, none of it
// heard until its last byte is in. Presumed lost at the bare read term, such a
// read was asked again while its first reply was still on its way — each
// re-ask another whole reply built and sent — and held to the bare ceiling,
// one whose reply takes longer than the ceiling to arrive was never taken
// (crewlet/crewlet#1149, crewlet/crewlet#1153). Given its arrival, the read is
// asked once and takes the reply that lands past the bare ceiling, inside the
// arrival; the control, given none, is asked again at the term and cut off at
// the ceiling. Exact, in a bubble.
//
// Mutation: grow the term alone, or the ceiling alone, by the arrival, and the
// read is cut off at the bare ceiling, or asked again while its reply is still
// on its way.
func TestAReadWhoseAnswerIsStillArrivingIsNeitherAskedAgainNorCutOff(t *testing.T) {
	t.Parallel()
	// THE REPLY TAKES THREE CEILINGS TO ARRIVE, and lands half of that past
	// the bare ceiling.
	arrival := 3 * fast.Lookup
	lands := fast.Lookup + arrival/2
	for _, c := range []struct {
		name   string
		timing Timing
		taken  bool
		asked  func(int32) bool
	}{
		{"given its arrival", fast.Arriving(arrival), true, func(n int32) bool { return n == 1 }},
		{"control", fast, false, func(n int32) bool { return n > 1 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				var asks atomic.Int32
				got, err := ReadWith(context.Background(), c.timing, func(ctx context.Context) (int, error) {
					asks.Add(1)
					landed := time.NewTimer(lands)
					defer landed.Stop()
					select {
					case <-landed.C:
						return 7, nil
					case <-ctx.Done():
						return 0, ctx.Err()
					}
				}, nil)
				if taken := err == nil && got == 7; taken != c.taken {
					t.Errorf("a reply landing %v in, past the ceiling of %v, read = %d, %v: taken %t, "+
						"want %t", lands, fast.Lookup, got, err, taken, c.taken)
				}
				if n := asks.Load(); !c.asked(n) {
					t.Errorf("a read whose reply lands %v in was asked %d times", lands, n)
				}
			})
		})
	}
}
