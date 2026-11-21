package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/statelog"
)

// growthSpendClass is the lease an allowed spend's apply holds, one per ad
// account (internal/spendguard): the whole company must agree on who applies
// next, so two raises read on one capacity cannot both pass the daily cap.
const growthSpendClass coord.Class = "growth_spend"

const (
	// growthSpendLeaseTTL outlasts one apply: the account and its capacity
	// read again and the one vendor call, each bounded by googleauth's client
	// timeout; a holder that dies delays the account's next apply that long.
	growthSpendLeaseTTL = 3 * time.Minute

	// growthSpendPoll is how often a waiting apply asks for the lease.
	growthSpendPoll = 250 * time.Millisecond

	// growthSpendRelease bounds the release after the apply's own context
	// may have ended.
	growthSpendRelease = 10 * time.Second
)

// spendLeases holds growthSpendClass's leases on the coordination store.
type spendLeases struct{ engine *Engine }

// Hold takes resource's lease, waiting while another node's apply holds it
// or until ctx ends, and answers its release.
func (l spendLeases) Hold(ctx context.Context, resource string) (func(), error) {
	coordResource := growthSpendClass.Resource(resource)
	owner := l.engine.selfNode() + "/" + uuid.NewString()
	for {
		lease, _, err := l.engine.backends.Coord.TryAcquire(ctx, coordResource, coord.AcquireOptions{
			Owner: owner, TTL: growthSpendLeaseTTL, Ungated: true})
		if err != nil {
			return nil, fmt.Errorf("engine: take the spend lease on %s: %w", resource, err)
		}
		if lease != nil {
			return func() {
				releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), growthSpendRelease)
				defer cancel()
				if _, err := l.engine.backends.Coord.Release(releaseCtx, coordResource, owner, lease.Epoch); err != nil {
					log.WarnContext(ctx, "growth_spend_lease_kept", "resource", resource, "error", err,
						"detail", "it lapses on its own")
				}
			}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(growthSpendPoll):
		}
	}
}

// stagedPages bounds how far back a batch job's staging is looked for: a job
// staged further back than this many pages of changes is refused, never run
// unclassed.
const stagedPages = 10

// StagedClass is the highest class the operations staged into batchJob of
// account came to, from the access log's records of their staging; false
// where it holds none.
func (a GrowthAudit) StagedClass(ctx context.Context, kind, account, batchJob string) (growth.Class, bool, error) {
	target := account + "/batchJobs/" + batchJob + ":addOperations"
	var out growth.Class
	q := accessdomain.GrowthQuery{Product: kind, Limit: 500}
	for range stagedPages {
		page, err := a.Changes(ctx, q, statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceSeat)})
		if err != nil {
			return "", false, err
		}
		for _, row := range page.Rows {
			if row.Target == target && (row.Step == growth.StepMade || row.Step == growth.StepApplied) &&
				row.Class.Rank() > out.Rank() {
				out = row.Class
			}
		}
		if page.Complete || page.Next == "" {
			break
		}
		q.Cursor = page.Next
	}
	return out, out != "", nil
}
