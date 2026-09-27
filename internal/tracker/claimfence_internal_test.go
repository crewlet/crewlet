package tracker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// A WALK'S FENCE FOLLOWS WHAT THE STORE LAST ANSWERED, IN ALL THREE VALUES.
//
// A renewal the store answers "held" moves the lease on; one it cannot answer
// changes nothing, so the walk goes on under what its last confirmed renewal
// bought and stops once less than [ClaimFenceMargin] of that is left; and one
// it answers "not held" stops the walk at once, whatever time is left, because
// that tenure is over and only a new epoch holds the resource again.
//
// Mutation: treat an unanswered renewal as a loss and the fourth case stops a
// walk with most of its lease left; count it as confirmed and the fifth
// appends past its margin; ignore a "not held" and the last appends under a
// lease somebody else may hold.
func TestAWalksFenceFollowsWhatTheStoreLastAnswered(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// held and err are the store's answer to the renewal.
		held bool
		err  error
		// after is how long after the renewal was sent the fence is read.
		after time.Duration
		lost  bool
	}{
		{name: "held, a heartbeat later", held: true, after: ClaimHeartbeat},
		{name: "held, a margin short of the renewed lease", held: true,
			after: ClaimTTL - ClaimFenceMargin},
		{name: "held, past the margin of the renewed lease", held: true,
			after: ClaimTTL - ClaimFenceMargin + time.Second, lost: true},
		{name: "unanswered, a heartbeat later", err: coord.ErrUnavailable,
			after: ClaimHeartbeat},
		{name: "unanswered, past the margin of the lease it did not renew",
			err: coord.ErrUnavailable, after: ClaimTTL - ClaimFenceMargin -
				ClaimHeartbeat + time.Second, lost: true},
		{name: "not held, at once", after: 0, lost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			at := time.Unix(1_700_000_000, 0)
			h := &held{
				claims:   answering{held: tc.held, err: tc.err},
				resource: "merge:dup", owner: "node-a", epoch: 7,
				clock:     func() time.Time { return at },
				confirmed: at,
			}
			// THE RENEWAL IS SENT A HEARTBEAT INTO THE LEASE, as the
			// heartbeat sends it.
			at = at.Add(ClaimHeartbeat)
			h.renew(t.Context())
			at = at.Add(tc.after)
			err := h.holding()
			switch {
			case tc.lost && !errors.Is(err, ErrClaimLost):
				t.Fatalf("the fence answered %v, want the walk stopped", err)
			case !tc.lost && err != nil:
				t.Fatalf("the fence stopped a walk that still holds its claim: %v", err)
			}
		})
	}
}

// answering is a coordination store whose every renewal gets one answer.
type answering struct {
	Claims
	held bool
	err  error
}

func (a answering) Renew(context.Context, string, string, int64, time.Duration) (bool, error) {
	return a.held, a.err
}
