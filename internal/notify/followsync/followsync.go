// Package followsync carries a node's own chat thread-follows onto the fleet,
// once, and removes them.
//
// # The bug this exists for
//
// `chat_thread_follows` was a company-wide fact in a file one process owns
// exclusively, which is ADR-0003's failure and what migration 0028 repairs.
// The repair moves the STATE; it does not move the ROWS, and a `.sql` file
// cannot — it has no KV client, and it runs before any Go code on every boot.
//
// Dropping them instead is not the ninety-day horizon arriving early, which is
// how it reads. That horizon expires follows after ninety days of INACTIVITY,
// so the population it drops is the threads nobody has touched in a quarter
// and the documented cost — "at most one missed non-mention reply" — is true
// of them. A drop at upgrade takes the opposite population: every follow,
// ordered by recency, the busiest ones included. For a live thread the cost is
// every subsequent reply until a fresh mention, and it is self-locking, because
// the reply that would prompt the seat to post is the one it no longer
// receives. For a follow recorded as `explicit` there is no mention coming at
// all, so the bound fails outright: those are gone for good.
//
// # The shape, and where it comes from
//
// [internal/fleetsecrets] is this tree's precedent for a one-way handoff out of
// a node table into coordination, and this follows it: copy, then remove, per
// row; a row that could not be copied is NOT removed; and the pass is armed at
// boot where both stores are reachable and nothing is routing yet.
//
// One rule is STRICTER here than in the precedent. fleetsecrets lists the
// destination once and skips the names it already holds, which is a read and
// then a write with a window between them. This pass runs while the engine is
// up and inbound chat is flowing, so the window is live: it writes through
// [coord.Follows.FollowIfAbsent], which collapses the check and the write into
// one operation, and never lists the fleet at all.
//
// That is also what makes two nodes handing off at once correct with nothing
// agreed between them. Each holds its own local table, the keys overlap,
// exactly one create wins, and the loser removes its own row having learned
// the fleet already has the record.
//
// # Why removing is not optional
//
// Nothing reads this table any more, so a row left behind cannot resurface as
// a stale answer the way a secret could. What it does instead is never end:
// every boot of every node would re-read N rows and re-attempt N creates for
// the life of the deployment. The delete is what makes the steady state one
// SELECT against an empty table.
package followsync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/store"
)

var log = logging.Get("notify.followsync")

// Source is the node-local table this package drains.
//
// Declared here rather than imported whole, in [fleetsecrets.LocalStore]'s
// idiom and for its reason: two methods is all a handoff needs, and a store
// handle that could reach the rest of the database would eventually be used
// for something else.
type Source interface {
	List(ctx context.Context) ([]store.Follow, error)
	Drop(ctx context.Context, one store.Follow) (bool, error)
}

// Destination is the fleet's follow store, CREATE-ONLY.
//
// The narrow half of [coord.Follows] on purpose: this package must not be able
// to overwrite a record the fleet already holds, and the way to guarantee that
// is to make the overwriting verb unreachable from here rather than to
// remember not to call it.
type Destination interface {
	FollowIfAbsent(ctx context.Context, backend, handle, channel, thread, reason string, at time.Time) (bool, error)
}

// Parallelism is how many rows the pass carries at once.
//
// EIGHT. Sequential is wrong at any real size: the write is a round trip, and
// against an external NATS cluster at 10 ms that is 100 seconds of blocked boot
// for ten thousand rows. Unbounded is worse — a burst of ten thousand
// concurrent creates at a broker that is also serving every other node's
// bring-up, which is the storm jsprovision's budgets exist to keep out of the
// boot path. Eight hides the round trip (ten thousand rows in about twelve
// seconds) and stays far inside one node's share of a broker that is starting.
const Parallelism = 8

// Migrate carries this node's own follow rows onto the fleet and removes them,
// reporting how many it created there.
//
// IT IS NOT BEST EFFORT. A row whose create failed is left in the table and
// the error is returned, so the next boot retries it — the alternative would
// delete a subscription this node is the only holder of, and the first symptom
// would be a seat silently not answering a thread, which is the very failure
// this exists to prevent.
//
// The count is what LANDED, which is not the number of rows: a row the fleet
// already held is removed locally and counted as nothing, because this call
// did not put it there.
func Migrate(ctx context.Context, from Source, to Destination) (int, error) {
	if from == nil || to == nil {
		return 0, nil
	}
	rows, err := from.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("followsync: list the follows to hand off: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}

	var (
		mu      sync.Mutex
		created int
		failed  []error
	)
	work := make(chan store.Follow)
	var wg sync.WaitGroup
	for range min(Parallelism, len(rows)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for one := range work {
				// COPY, THEN REMOVE, and only ever in that order.
				made, err := to.FollowIfAbsent(ctx, one.Backend, one.Handle,
					one.Channel, one.Thread, one.Reason, one.UpdatedAt)
				if err != nil {
					mu.Lock()
					failed = append(failed, fmt.Errorf(
						"followsync: carry the follow on %s thread %s for %s: %w",
						one.Backend, one.Thread, one.Handle, err))
					mu.Unlock()
					continue
				}
				if _, err := from.Drop(ctx, one); err != nil {
					// THE ROW IS ON THE FLEET and this node could not
					// forget it. Reported, because the pass will run
					// again next boot and re-attempt a create that now
					// answers "already there" — harmless, and it will
					// keep happening until the local delete succeeds.
					mu.Lock()
					failed = append(failed, err)
					mu.Unlock()
					continue
				}
				if made {
					mu.Lock()
					created++
					mu.Unlock()
				}
			}
		}()
	}
	for _, one := range rows {
		select {
		case work <- one:
		case <-ctx.Done():
			close(work)
			wg.Wait()
			return created, fmt.Errorf("followsync: hand off %d follow(s): %w",
				len(rows), ctx.Err())
		}
	}
	close(work)
	wg.Wait()

	if len(failed) > 0 {
		return created, fmt.Errorf("followsync: %d of %d follow(s) did not move: %w",
			len(failed), len(rows), errors.Join(failed...))
	}
	log.InfoContext(ctx, "follows_handed_off", "rows", len(rows), "created", created,
		"detail", "these were written to this node's own database before the follows "+
			"moved to coordination; every peer can see them now")
	return created, nil
}
