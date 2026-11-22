package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/statelog"
)

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
