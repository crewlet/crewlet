package usage

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

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
// alone — its every reader on every node and day; drop it from a schedule's
// fires or their guard, and the plan names `usage_schedule_runs_schedule_idx`;
// write the schedule guard as an unfiltered correlated MAX and the
// decorrelation refusal names it.
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
		{"what the fleet holds: a seat's tool calls", appliedSeatToolsSQL, []string{seat}},
		{"what the fleet holds: a seat's turn ledger", appliedSeatLedgerSQL, []string{seat}},
		{"what the fleet holds: a person's spend", appliedPersonSQL, []string{person}},
		{"what the fleet holds: a schedule's fires", appliedScheduleSQL, []string{schedule}},
		{"what the fleet holds: a schedule's superseded fires", appliedSupersededSQL,
			[]string{schedule}},
		{"the apply's guard: a seat's head", applySeatHeadSQL,
			[]string{seat, "usage_turns_agent_idx (agent_id=? AND day=?)"}},
		{"the apply's guard: a schedule's newest", applyScheduleNewestSQL, []string{schedule}},
		{"the apply's guard: a schedule's newest superseded", applySupersededNewestSQL,
			[]string{schedule}},
		{"the apply's guard: a person's newest", applyPersonNewestSQL, []string{person}},
		{"the apply's clear: a seat's spend", applyClearSeatTokensSQL, []string{seat}},
		{"the apply's clear: a seat's reads", applyClearSeatReadsSQL, []string{seat}},
		{"the apply's clear: a seat's tool calls", applyClearSeatToolsSQL, []string{seat}},
		{"the apply's clear: a seat's turn ledger", applyClearSeatLedgerSQL, []string{seat}},
		{"the apply's clear: a schedule's fires", applyClearScheduleSQL, []string{schedule}},
		{"the apply's clear: a schedule's superseded fires", applyClearSupersededSQL,
			[]string{schedule}},
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

// THE TURN LEDGER IS READ BY ITS KEY'S LEADING DAY: a day range of every node
// and every seat, never a walk of the whole table.
//
// Mutation: lead the key with anything but the day, and the plan walks the table.
func TestTheTurnLedgerSeeksItsDayIndex(t *testing.T) {
	t.Parallel()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	steps := storetest.Plan(t, db.Replicated(), turnLedgerSQL, []any{"2026-09-17", "2026-09-23"})
	sought := false
	for _, step := range steps {
		if strings.Contains(step, "sqlite_autoindex_usage_turn_ledger_1 (day>=? AND day<=?)") {
			sought = true
		}
		if strings.HasPrefix(step, "SCAN l") || strings.HasPrefix(step, "SCAN usage_turn_ledger") {
			t.Errorf("the plan walks the table: %s", step)
		}
	}
	if !sought {
		t.Errorf("the plan %q does not seek the ledger by day", steps)
	}
}

// A SERVER'S CALLS ARE READ BY ITS OWN INDEX: the read is a range of days of
// one server, every node and every seat, and walking the whole range of every
// server to find one is what `usage_tool_calls_server_idx` exists to stop.
//
// Mutation: drop the index from the migration, and the plan names the primary
// key's day range instead.
func TestAServersToolCallsSeekItsIndex(t *testing.T) {
	t.Parallel()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	steps := storetest.Plan(t, db.Replicated(), serverToolCallsSQL,
		[]any{"github", "2026-09-17", "2026-09-23"})
	if line, ok := storetest.Decorrelated(steps); ok {
		t.Errorf("the planner unnested a correlated aggregate: %s", line)
	}
	sought := false
	for _, step := range steps {
		if strings.Contains(step, "usage_tool_calls_server_idx (server=?") {
			sought = true
		}
		if strings.HasPrefix(step, "SCAN t") || strings.HasPrefix(step, "SCAN usage_tool_calls") {
			t.Errorf("the plan walks the table: %s", step)
		}
	}
	if !sought {
		t.Errorf("the plan %q does not seek the server's index", steps)
	}
}

// A SCHEDULE'S HISTORY IS READ IN ITS OWN ORDER, and so is what became of each
// fire: the company's newest fires walk `usage_schedule_runs_newest_idx` from
// its newest end, one schedule's seek `usage_schedule_runs_schedule_idx`, both
// stopped at the page with nothing sorted, and every fire's outcome probes
// `usage_schedule_superseded` by its trace. Sorted instead, a page read every
// fire of the 181-day horizon on every poll of the Schedules page; and without
// the trace index every fire on a page walks every superseded fire the fleet
// holds.
//
// Mutation: drop or narrow either index in replicated 0076, or order one
// schedule's page by the columns it fixes, and the plan sorts or walks.
func TestAScheduleHistoryIsReadInItsOwnOrder(t *testing.T) {
	t.Parallel()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	page := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name  string
		q     ScheduleRunsQuery
		index string
	}{
		{"the company's", ScheduleRunsQuery{Limit: 50}, "usage_schedule_runs_newest_idx"},
		{"the company's runs, scrolled", ScheduleRunsQuery{Outcome: scheduleOutcomeFired,
			BeforeAt: page, BeforeKey: "n1", Limit: 50}, "usage_schedule_runs_newest_idx"},
		{"one seat's runs", ScheduleRunsQuery{Target: "pm", Outcome: scheduleOutcomeFired,
			Limit: 50}, "usage_schedule_runs_newest_idx"},
		{"one schedule's", ScheduleRunsQuery{ScopeType: "role", ScopeID: "pm",
			Name: "standup", Limit: 50},
			"usage_schedule_runs_schedule_idx (scope_type=? AND scope_id=? AND name=?"},
		{"one schedule's, scrolled", ScheduleRunsQuery{ScopeType: "role", ScopeID: "pm",
			Name: "standup", BeforeAt: page, BeforeKey: "n1", Limit: 50},
			"usage_schedule_runs_schedule_idx (scope_type=? AND scope_id=? AND name=?"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			query, args, err := scheduleRunsSQL(c.q)
			if err != nil {
				t.Fatal(err)
			}
			steps := storetest.Plan(t, db.Replicated(), query, args)
			if line, ok := storetest.Decorrelated(steps); ok {
				t.Errorf("the planner unnested the outcome's probe: %s", line)
			}
			var paged, probed bool
			for _, step := range steps {
				switch {
				case strings.Contains(step, "SORTER"), strings.Contains(step, "TEMP B-TREE"):
					t.Errorf("the plan %q sorts the fires rather than reading them in order", steps)
				case strings.Contains(step, c.index):
					paged = true
				case strings.Contains(step, "usage_schedule_superseded_trace_idx (trace_id=?)"):
					probed = true
				case strings.HasPrefix(step, "SCAN "):
					t.Errorf("the plan %q walks a table: %s", steps, step)
				}
			}
			if !paged {
				t.Errorf("the plan %q does not read %s", steps, c.index)
			}
			if !probed {
				t.Errorf("the plan %q does not probe a fire's trace", steps)
			}
		})
	}
}
