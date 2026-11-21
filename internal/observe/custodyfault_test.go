package observe_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

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

// failingLog is this node's event log with one of its two writes failing as
// a store fails, with an error the store left unmarked.
type failingLog struct {
	observe.CustodyLog
	write, settle error
}

func (f failingLog) WriteCustody(ctx context.Context, b store.CustodyBatch, at time.Time) error {
	if f.write != nil {
		return f.write
	}
	return f.CustodyLog.WriteCustody(ctx, b, at)
}

func (f failingLog) SettleCustody(ctx context.Context, batchID string, kept bool) error {
	if f.settle != nil {
		return f.settle
	}
	return f.CustodyLog.SettleCustody(ctx, batchID, kept)
}

// A FAILURE OF THIS NODE'S IS NEVER THE BATCH'S OWN: a custody batch sent back
// is the batch's only where the store refused it (here one naming no origin),
// and the node's (queue.NakNode) for what the queue tells as the node's, which
// wins over the mark, a claim's unknown answer (wrapped in coord.ErrUnavailable
// or not), and this node's store failing at the write or the settle, closed or
// unmarked (observe's "A custody batch sent back is this node's failure").
//
// Mutation: send back every failure with queue.Nak unless the queue tells it,
// mark the write's transaction failure as the store's refusal, send back the
// settle's failure with queue.Nak, send back a refusal with queue.NakNode, or
// let the store's mark win over the queue's, and the rows that meet it go red.
func TestAFailureOfThisNodesIsNeverTheBatchsOwn(t *testing.T) {
	t.Parallel()
	unmarked := errors.New("disk I/O error")
	for name, tc := range map[string]struct {
		// endFirst ends the context before the delivery is handled,
		// endAtClaim once the claim is made.
		endFirst, endAtClaim bool
		// closeFirst closes this node's store before the delivery is
		// handled, closeAtClaim once the claim is answered.
		closeFirst, closeAtClaim bool
		// write and settle fail the log's two writes.
		write, settle error
		claim         func(ctx context.Context) error
		origin        string
		nodes         bool
	}{
		"a write this node's leaving cut short": {endFirst: true, origin: "seats-1", nodes: true},
		"a claim the coordination store did not answer": {
			claim: func(context.Context) error {
				return fmt.Errorf("%w: claim custody of the batch: nats: timeout", coord.ErrUnavailable)
			},
			origin: "seats-1", nodes: true},
		// The contract's unknown answer is every error but the
		// undecidable record's, whatever a backend wraps it in.
		"a claim answered with an error its store left unmarked": {
			claim: func(context.Context) error {
				return errors.New("the coordination store is unreachable")
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
		// Every record validates, then the transaction cannot run.
		"a write this node's closed store failed": {closeFirst: true, origin: "seats-1", nodes: true},
		"a write its store failed unmarked":       {write: unmarked, origin: "seats-1", nodes: true},
		"a settle this node's closed store failed": {
			closeAtClaim: true, origin: "seats-1", nodes: true},
		"a settle its store failed unmarked": {settle: unmarked, origin: "seats-1", nodes: true},
		"a batch that names no origin":       {},
		// The store's refusal met as the keeper left the group: what the
		// queue tells as the node's wins over the mark.
		"a batch naming no origin this node's leaving cut short": {endFirst: true, nodes: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, end := context.WithCancel(t.Context())
			defer end()
			db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "data-a.db"), store.Options{})
			t.Cleanup(func() { _ = db.Close() })
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
			if tc.closeAtClaim {
				c.claim = func(context.Context) error { return db.Close() }
			}
			k, err := observe.NewKeeper(failingLog{CustodyLog: db.Events(), write: tc.write,
				settle: tc.settle}, c, "data-a")
			if err != nil {
				t.Fatal(err)
			}
			if tc.endFirst {
				end()
			}
			if tc.closeFirst {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
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
