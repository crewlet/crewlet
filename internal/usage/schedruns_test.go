package usage_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// EVERY NODE'S FIRES, NEWEST FIRST, AND A SCROLL NEITHER REPEATS NOR SKIPS —
// including two fires at one instant on two nodes, which only the key orders.
func TestScheduleRunsPageEveryNodesFiresNewestFirst(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	base := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	type fire struct {
		node, target string
		at           time.Time
	}
	fires := []fire{
		{"n1", "pm", base}, {"n2", "pm", base}, // one instant, two nodes
		{"n1", "cto", base.Add(-time.Hour)},
		{"n2", "pm", base.Add(-2 * time.Hour)},
		{"n1", "pm", base.Add(-3 * time.Hour)},
	}
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		for i, f := range fires {
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO usage_schedule_runs
					(day, node, scope_type, scope_id, name, fired_at, target,
					 outcome, trace_id, turn_id, version)
				VALUES (?, ?, 'role', 'pm', 'standup', ?, ?, 'dispatched', '', ?, 1)`,
				f.at.Format("2006-01-02"), f.node, store.EncodeTime(f.at), f.target,
				fmt.Sprintf("turn-%d", i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var seen []string
	q := usage.ScheduleRunsQuery{Limit: 2}
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("the scroll does not end")
		}
		runs, more, err := usage.ScheduleRuns(t.Context(), db.Replicated(), q)
		if err != nil {
			t.Fatalf("ScheduleRuns: %v", err)
		}
		for _, run := range runs {
			seen = append(seen, run.TurnID)
		}
		if !more {
			break
		}
		last := runs[len(runs)-1]
		q.BeforeAt, q.BeforeKey = last.FiredAt, last.Key()
	}
	want := fmt.Sprint([]string{"turn-1", "turn-0", "turn-2", "turn-3", "turn-4"})
	if got := fmt.Sprint(seen); got != want {
		t.Errorf("the scroll read %s, want %s", got, want)
	}

	runs, _, err := usage.ScheduleRuns(t.Context(), db.Replicated(),
		usage.ScheduleRunsQuery{Target: "cto", Limit: 10})
	if err != nil || len(runs) != 1 || runs[0].TurnID != "turn-2" {
		t.Errorf("the fires addressed to cto are %+v (%v), want the one", runs, err)
	}
}

// A TICK THE SCHEDULER DID NOT RUN IS NOT A RUN: narrowed to one outcome, the
// page holds only the fires recorded under it, and the scroll still ends.
func TestScheduleRunsNarrowToOneOutcome(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	base := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	outcomes := []string{"fired", "skipped_catchup", "fired", "skipped_paused", "fired"}
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		for i, outcome := range outcomes {
			at := base.Add(-time.Duration(i) * time.Minute)
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO usage_schedule_runs
					(day, node, scope_type, scope_id, name, fired_at, target,
					 outcome, trace_id, turn_id, version)
				VALUES (?, 'n1', 'role', 'pm', 'sweep', ?, 'pm', ?, '', ?, 1)`,
				at.Format("2006-01-02"), store.EncodeTime(at), outcome,
				fmt.Sprintf("turn-%d", i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var seen []string
	q := usage.ScheduleRunsQuery{Outcome: "fired", Limit: 2}
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("the scroll does not end")
		}
		runs, more, err := usage.ScheduleRuns(t.Context(), db.Replicated(), q)
		if err != nil {
			t.Fatalf("ScheduleRuns: %v", err)
		}
		for _, run := range runs {
			if run.Outcome != "fired" {
				t.Errorf("a %s tick came back narrowed to fired", run.Outcome)
			}
			seen = append(seen, run.TurnID)
		}
		if !more {
			break
		}
		last := runs[len(runs)-1]
		q.BeforeAt, q.BeforeKey = last.FiredAt, last.Key()
	}
	if got, want := fmt.Sprint(seen), fmt.Sprint([]string{"turn-0", "turn-2", "turn-4"}); got != want {
		t.Errorf("the fired runs read %s, want %s", got, want)
	}
}

// ONE SCHEDULE'S FIRES ARE ALL THREE PARTS OF ITS IDENTITY: two units can each
// declare a `standup`, and a role and a unit can both, so each part narrows on
// its own. A read naming only some of them is refused rather than read as a
// filter on what it named.
//
// Mutation: drop any one of the three predicates from scheduleRunsSQL, and the
// page holds another schedule's fire.
func TestScheduleRunsNarrowToOneSchedule(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	at := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		for i, s := range [][3]string{
			{"role", "pm", "standup"},
			{"unit", "pm", "standup"},  // another scope type
			{"role", "cto", "standup"}, // another scope
			{"role", "pm", "retro"},    // another name
		} {
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO usage_schedule_runs
					(day, node, scope_type, scope_id, name, fired_at, target,
					 outcome, trace_id, turn_id, version)
				VALUES (?, 'n1', ?, ?, ?, ?, 'pm', 'fired', ?, '', 1)`,
				at.Format("2006-01-02"), s[0], s[1], s[2], store.EncodeTime(at),
				fmt.Sprintf("trace-%d", i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runs, more, err := usage.ScheduleRuns(t.Context(), db.Replicated(), usage.ScheduleRunsQuery{
		ScopeType: "role", ScopeID: "pm", Name: "standup", Limit: 10})
	if err != nil {
		t.Fatalf("ScheduleRuns: %v", err)
	}
	if more || len(runs) != 1 || runs[0].TraceID != "trace-0" {
		t.Errorf("one schedule's fires are %+v (more %v), want trace-0 alone", runs, more)
	}
	for _, q := range []usage.ScheduleRunsQuery{
		{ScopeType: "role", ScopeID: "pm", Limit: 10},
		{ScopeType: "role", Name: "standup", Limit: 10},
		{ScopeID: "pm", Name: "standup", Limit: 10},
	} {
		if _, _, err := usage.ScheduleRuns(t.Context(), db.Replicated(), q); err == nil {
			t.Errorf("%+v was read, want it refused for naming part of a schedule", q)
		}
	}
}
