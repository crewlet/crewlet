package observe_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// claimedThen is the fleet's custody record as a keeper reaches it while
// something of this node's fails: claim answers in the store's place when set,
// and end, when set, ends the delivery's context once the claim is made.
type claimedThen struct {
	inner *coordmem.Fleet
	claim func(ctx context.Context) error
	end   func()
}

func (c *claimedThen) ClaimCustody(ctx context.Context, batch, node string) (string, error) {
	if c.end != nil {
		defer c.end()
	}
	if c.claim != nil {
		if err := c.claim(ctx); err != nil {
			return "", err
		}
	}
	return c.inner.ClaimCustody(ctx, batch, node)
}

// A FAILURE OF THIS NODE'S IS NEVER THE BATCH'S OWN. A custody batch that
// could not be written or settled is sent back for a retry; where the failure
// is this node's, it is sent back as the node's (queue.NakNode), so a delivery
// whose budget it spends is kept with the custody group's unhandled
// deliveries, never with its poison. This node's are what the queue tells as
// its node's (queue.NodeFault: here a context this node's leaving the group
// ended, at the write, the claim or the settle) and a claim the coordination
// store did not answer (coord.ErrUnavailable). A batch no node can write, one
// that names no origin, stays the batch's own failure.
//
// Mutation: send back the write's failure, or the settle's, with queue.Nak,
// and its rows count the failure against the batch; drop the coordination
// store's silence from the settle's test and its row does.
func TestAFailureOfThisNodesIsNeverTheBatchsOwn(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		// endFirst ends the context before the delivery is handled,
		// endAtClaim once the claim is made.
		endFirst, endAtClaim bool
		claim                func(ctx context.Context) error
		origin               string
		nodes                bool
	}{
		"a write this node's leaving cut short": {endFirst: true, origin: "seats-1", nodes: true},
		"a claim the coordination store did not answer": {
			claim: func(context.Context) error {
				return fmt.Errorf("%w: claim custody of the batch: nats: timeout", coord.ErrUnavailable)
			},
			origin: "seats-1", nodes: true},
		"a claim this node's leaving cut short": {
			endAtClaim: true,
			claim: func(ctx context.Context) error {
				<-ctx.Done()
				return fmt.Errorf("claim custody of the batch: %w", ctx.Err())
			},
			origin: "seats-1", nodes: true},
		"a settle this node's leaving cut short": {endAtClaim: true, origin: "seats-1", nodes: true},
		"a batch that names no origin":           {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, end := context.WithCancel(t.Context())
			defer end()
			c := &claimedThen{inner: coordmem.NewFleet(), claim: tc.claim}
			if tc.endAtClaim {
				c.end = end
				if tc.claim != nil {
					// The claim sees its context end, as a client
					// waiting on the store does when the keeper leaves.
					claim := tc.claim
					c.end, c.claim = nil, func(ctx context.Context) error { end(); return claim(ctx) }
				}
			}
			if tc.endFirst {
				end()
			}
			db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "data-a.db"), store.Options{})
			t.Cleanup(func() { _ = db.Close() })
			k, err := observe.NewKeeper(db.Events(), c, "data-a")
			if err != nil {
				t.Fatal(err)
			}
			batch, _ := carrier(t, 2)
			batch.Node = tc.origin
			got := k.Handle(ctx, batch)
			if got.Outcome != queue.OutcomeNak {
				t.Fatalf("Handle = %+v, want the delivery sent back for a retry", got)
			}
			if got.FailsMessage() == tc.nodes {
				t.Errorf("FailsMessage() = %v for %v; want %v", got.FailsMessage(), got.Err, !tc.nodes)
			}
		})
	}
}
