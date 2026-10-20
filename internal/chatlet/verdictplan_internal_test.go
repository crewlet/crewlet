package chatlet

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE VERDICTS ARE READ AND PRUNED BY SEEK, and every message record runs two
// of the four statements inside the transaction that holds the estate's one
// writer: whether an earlier copy decided its operation, and the prune past the
// horizon. A scan of either is a cost every post pays, growing with the month
// of verdicts the table holds; the writer's read-back and the feed's run on the
// request path and the wake path.
//
// NEVER ANALYZEd, because no node is: a plan taken over statistics is a plan
// no deployment runs.
func TestTheVerdictsAreReadAndPrunedBySeek(t *testing.T) {
	t.Parallel()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})

	for _, c := range []struct {
		name, statement string
		want            []string
	}{
		// AN OPERATION HOLDS ONE VERDICT, two where a copy landed, so
		// the position term is filtered off the index's own entries.
		{"an earlier copy's verdict", verdictDecidedStatement, []string{
			"SEARCH chat_verdicts USING COVERING INDEX chat_verdicts_op_idx (op_id=?)",
		}},
		{"an operation's first verdict", verdictReadStatement, []string{
			"SEARCH chat_verdicts USING INDEX chat_verdicts_op_idx (op_id=?)",
		}},
		{"the prune past the horizon", verdictPruneStatement, []string{
			"LIST SUBQUERY 1",
			"SEARCH chat_verdicts USING COVERING INDEX chat_verdicts_at_idx (at<?)",
			"SEARCH chat_verdicts USING INTEGER PRIMARY KEY (rowid=?)",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := make([]any, strings.Count(c.statement, "?"))
			for i := range args {
				args[i] = 1
			}
			// storetest.Plan plans the prune, a delete, in a write
			// transaction, since a read transaction refuses to plan it.
			plan := storetest.Plan(t, db, c.statement, args)
			if strings.Join(plan, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("the plan is\n\t%s\nwant\n\t%s",
					strings.Join(plan, "\n\t"), strings.Join(c.want, "\n\t"))
			}
		})
	}
}
