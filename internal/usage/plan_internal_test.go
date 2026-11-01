package usage

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// EVERY STATEMENT THAT READS OR CLEARS ONE OBJECT SEEKS THAT OBJECT'S KEY.
//
// The publisher reads what the fleet holds of an object before every publish
// ([Applied]) and the applier reads its guard and clears its rows on every
// record, so each runs once per changed object per tick on every data node.
// With no statistics the planner prefers the one-column day index over a
// primary key that does not cover what a statement reads, and a statement that
// takes it walks every data node's day — every seat's spend cells, every
// person's — to read one object. Each statement here is the package's own text,
// so the plan read is the plan run, and it is read through storetest's one
// reading of a plan, which also asks whether the planner unnested a correlated
// aggregate ([storetest.Decorrelated]): a guard rewritten that way reads every
// row of its table however many objects it is asked about.
//
// Mutation: drop either `INDEXED BY` from the spend reads, or from the person
// guard, and its plan names `_day_idx (day=?)`; drop it from a repository's
// readers, and its plan seeks the covering reader index by the repository
// alone — its every reader on every node and day; write the schedule guard as
// an unfiltered correlated MAX and the decorrelation refusal names it.
func TestEveryObjectStatementSeeksItsKey(t *testing.T) {
	t.Parallel()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	const (
		seat     = "(day=? AND node=? AND agent_id=?)"
		person   = "(day=? AND node=? AND person=?)"
		schedule = "(day=? AND node=? AND scope_type=? AND scope_id=? AND name=?)"
		repo     = "(day=? AND node=? AND repo_id=?)"
	)
	for _, c := range []struct {
		name, query string
		// seeks is every key prefix a plan step may name: the object's
		// whole key, or — for a seat's head row alone — the seat's own
		// index, which holds one row per node for that seat-day.
		seeks []string
	}{
		{"what the fleet holds: a seat's head", appliedSeatHeadSQL,
			[]string{seat, "usage_turns_agent_idx (agent_id=? AND day=?)"}},
		{"what the fleet holds: a seat's spend", appliedSeatTokensSQL, []string{seat}},
		{"what the fleet holds: a seat's reads", appliedSeatReadsSQL, []string{seat}},
		{"what the fleet holds: a person's spend", appliedPersonSQL, []string{person}},
		{"what the fleet holds: a schedule's fires", appliedScheduleSQL, []string{schedule}},
		{"the apply's guard: a seat's head", applySeatHeadSQL,
			[]string{seat, "usage_turns_agent_idx (agent_id=? AND day=?)"}},
		{"the apply's guard: a schedule's newest", applyScheduleNewestSQL, []string{schedule}},
		{"the apply's guard: a person's newest", applyPersonNewestSQL, []string{person}},
		{"the apply's clear: a seat's spend", applyClearSeatTokensSQL, []string{seat}},
		{"the apply's clear: a seat's reads", applyClearSeatReadsSQL, []string{seat}},
		{"the apply's clear: a schedule's fires", applyClearScheduleSQL, []string{schedule}},
		{"the apply's clear: a person's spend", applyClearPersonSQL, []string{person}},
		{"what the fleet holds: a repository's day", appliedRepoReadHeadSQL, []string{repo}},
		{"what the fleet holds: a repository's readers", appliedRepoReadsSQL, []string{repo}},
		{"the apply's guard: a repository's day", applyRepoReadHeadSQL, []string{repo}},
		{"the apply's clear: a repository's readers", applyClearRepoReadsSQL, []string{repo}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			args := make([]any, strings.Count(c.query, "?"))
			for i := range args {
				args[i] = "x"
			}
			// A WRITE IS PLANNED IN A WRITE TRANSACTION, by Plan itself:
			// the clears are writes and the estate refuses one in a read.
			steps := storetest.Plan(t, db.Replicated(), c.query, args)
			if line, ok := storetest.Decorrelated(steps); ok {
				t.Errorf("the planner unnested a correlated aggregate into a "+
					"read of the whole table: %s", line)
			}
			for _, step := range steps {
				if !strings.HasPrefix(step, "SEARCH ") {
					t.Errorf("a step does not seek: %s", step)
					continue
				}
				if !containsAny(step, c.seeks) || strings.Contains(step, "_day_idx") {
					t.Errorf("the plan seeks %q, want one of %q — it walks the "+
						"whole day of every node", step, c.seeks)
				}
			}
		})
	}
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
