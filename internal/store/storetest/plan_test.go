package storetest_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE DECORRELATION CHECK SEES THE DRIVER'S UNNEST, AND ONLY IT.
//
// Every gate that refuses a per-row aggregate read as a grouped scan of its
// whole child table asks [storetest.Decorrelated], so the predicate is held to
// the driver here, on statements whose plan is the point: a `MIN` correlated
// to an unfiltered outer table, which the planner rewrites; the same subquery
// over an outer read bound to one key, which it keeps correlated; the same
// minimum spelled `ORDER BY … LIMIT 1`, which it never rewrites; and a
// subquery written in a FROM clause and joined, which is materialised behind
// an ephemeral index of the same family without anything having been
// decorrelated. If a driver bump renames the rewrite or stops making it, the
// first case fails here instead of every gate going quietly blind; if it starts
// rewriting the limit, the third does.
//
// NEVER ANALYZEd, because no node is: a plan taken over statistics is a plan
// no deployment runs.
//
// Mutation: change the operand the predicate matches (`scalar_sub_query_`)
// and the first case goes red; match the ephemeral index instead
// (`ephemeral_subquery_`) and the last one does.
func TestTheDecorrelationCheckSeesTheDriverUnnest(t *testing.T) {
	t.Parallel()
	db, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })

	for name, c := range map[string]struct {
		statement    string
		args         []any
		decorrelated bool
	}{
		"a minimum per row of an unfiltered table": {
			statement: `SELECT p.key, (SELECT MIN(t.rank) FROM tracker_tasks t
			              WHERE t.project_key = p.key)
			              FROM tracker_projects p`,
			decorrelated: true,
		},
		"a count for one row read by its key": {
			statement: `SELECT p.key, (SELECT COUNT(*) FROM tracker_tasks t
			              WHERE t.project_key = p.key)
			              FROM tracker_projects p WHERE p.key = ?`,
			args: []any{"P"},
		},
		"the minimum spelled as the first row": {
			statement: `SELECT p.key, (SELECT t.rank FROM tracker_tasks t
			              WHERE t.project_key = p.key ORDER BY t.rank LIMIT 1)
			              FROM tracker_projects p`,
		},
		"a grouped subquery the statement joins": {
			statement: `SELECT p.key, s.n FROM tracker_projects p
			              JOIN (SELECT project_key, COUNT(*) n FROM tracker_tasks
			                     GROUP BY project_key) s ON s.project_key = p.key`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan := storetest.Plan(t, estate, c.statement, c.args)
			line, decorrelated := storetest.Decorrelated(plan)
			switch {
			case c.decorrelated && !decorrelated:
				t.Errorf("the planner unnests this subquery and the check does not "+
					"see it, so every gate asking it is blind to the form it "+
					"exists to refuse — the plan is %q", plan)
			case !c.decorrelated && decorrelated:
				t.Errorf("the check reads %q as a decorrelated subquery, and the "+
					"statement keeps its subquery a seek per row (or has none) — "+
					"the plan is %q", line, plan)
			}
		})
	}
}

// A PLAN THAT SAYS NOTHING FAILS, EXCEPT AN INSERT'S.
//
// A gate that passed on an empty plan would certify nothing, so [storetest.Plan]
// refuses a statement the planner says nothing about — but an insert of values
// reads no table, its conflict probe included, and the planner says nothing
// about one, so a gate registering such a write reads it as the empty plan it
// is. A write is planned in a write transaction, because the read path runs
// query_only and refuses even the EXPLAIN of one.
//
// Mutation: refuse the insert's empty plan too, or plan every statement in a
// read transaction, and this goes red.
func TestAPlanThatSaysNothingFailsExceptAnInserts(t *testing.T) {
	t.Parallel()
	db, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })

	insert := &recordingTB{TB: t}
	if plan := storetest.Plan(insert, estate, `INSERT INTO tracker_projects (key, name)
		VALUES (?, ?)`, []any{"P", "Project"}); len(plan) != 0 || insert.failed != "" {
		t.Errorf("an insert of values planned %q and failed with %q, want the "+
			"empty plan it is", plan, insert.failed)
	}
	write := &recordingTB{TB: t}
	if storetest.Plan(write, estate, `DELETE FROM tracker_projects WHERE key = ?`,
		[]any{"P"}); write.failed != "" {
		t.Errorf("a delete was not planned in a write transaction: %s", write.failed)
	}
	// A ROW OF VALUES IS NOT AN INSERT, and the planner says nothing about
	// it: the one statement here a gate could register and read nothing of.
	silent := &recordingTB{TB: t}
	storetest.Plan(silent, estate, `VALUES (1)`, nil)
	if silent.failed == "" {
		t.Error("a statement the planner said nothing about passed, so a gate " +
			"over it would certify nothing")
	}
}

// recordingTB is a test that records its first fatal failure rather than
// stopping, so a case can hold what [storetest.Plan] refuses.
type recordingTB struct {
	testing.TB
	failed string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Fatalf(format string, args ...any) {
	if r.failed == "" {
		r.failed = fmt.Sprintf(format, args...)
	}
}
