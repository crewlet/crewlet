package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// EVERY USAGE COUNT SEEKS ITS WINDOW, AND SEEKS THE IN-FLIGHT KEY FROM EACH ROW
// IT COUNTS.
//
// The statements a usage day is derived from read a day of the busiest rows
// the log holds, two of them on every fifteen-second tick, and each must leave
// out a custody batch in flight ([keptRowSQL]). Two plans make that cheap, and
// neither is the planner's default:
//
//   - THE LOG IS SOUGHT BY TYPE AND TIME. Over `event_type IN (…)` — which
//     is what these statements were before the per-type branches — the
//     planner seeks only the type and walks every row of it the log holds —
//     thirty days of spend for one day's — so every step that reaches the log
//     must carry both bounds of the window.
//   - THE IN-FLIGHT IDENTITIES ARE SOUGHT, ONE PROBE PER ROW COUNTED. Read as
//     a list (`NOT IN` over the batches' JSON, a `LIST SUBQUERY`), every row
//     is checked against the whole list one entry at a time: a fingerprint of
//     a five-thousand-row day took 2.4 s beside ten thousand identities in
//     flight (measured). So every log step has its own probe of the custody
//     key, by the row's `(event_time, event_id)`, and the custody tables are
//     never scanned.
//
// Each statement must also still READ the custody key — one that lost the
// predicate counts a batch two nodes hold twice — once per log step, so a
// branch of a UNION that lost it fails too.
//
// The kept settle's own read is held here as well: driven from the batch's
// identities, by the batch index, each a seek of the log.
//
// Mutation: drop [keptRowSQL] from any statement or any branch, write it as
// `NOT IN`, put a statement back on `event_type IN (…)`, compare a turn id to
// the bare JSON value in [usageLegacyTurnsSQL], or join the log first in
// [markUsageRederiveSQL], and this names the statement.
func TestEveryUsageCountSeeksItsWindow(t *testing.T) {
	t.Parallel()
	db, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	at := now()
	w := UsageWindow{Start: at.Add(-24 * time.Hour), End: at}

	for _, s := range []struct {
		name string
		sql  func(UsageWindow) (string, []any)
	}{
		{"the fingerprint", usageMarkSQL},
		{"the spend", usageTokensSQL},
		{"a person's spend", usagePeopleSQL},
		{"the ended turns", usageEndedTurnsSQL},
		{"the reads", usageReadsSQL},
		{"the repository reads", usageRepoReadsSQL},
		{"the changes and reviews", usageCodeSQL},
	} {
		query, args := s.sql(w)
		plan := planOf(t, db, query, args)
		logSteps, probes := 0, 0
		for _, step := range strings.Split(plan, "\n") {
			switch {
			case scansLog(step), strings.HasPrefix(step, "SCAN custody_"),
				strings.HasPrefix(step, "SCAN u"), strings.Contains(step, "LIST SUBQUERY"),
				strings.Contains(step, "custody_unsettled_rows") && !strings.HasPrefix(step, "SEARCH u "):
				t.Errorf("%s: the plan is %q — step %q walks what it should seek", s.name, plan, step)
			case strings.HasPrefix(step, "SEARCH e ") || strings.HasPrefix(step, "SEARCH crewlet_events"):
				logSteps++
				if !strings.Contains(step, "event_time>") || !strings.Contains(step, "event_time<") {
					t.Errorf("%s: the plan is %q — step %q does not seek the window, so it "+
						"walks every row of its kind the log holds", s.name, plan, step)
				}
			case strings.HasPrefix(step, "SEARCH u "):
				probes++
				if !strings.Contains(step, "(row_time=? AND row_id=?)") {
					t.Errorf("%s: the plan is %q — step %q does not probe the in-flight "+
						"key by the row's identity", s.name, plan, step)
				}
			}
		}
		if logSteps == 0 {
			t.Errorf("%s: the plan is %q — it never seeks the log", s.name, plan)
		}
		if probes != logSteps {
			t.Errorf("%s: the plan is %q — %d step(s) read the log and %d probe the "+
				"batches in flight, so a batch two nodes hold is in both their days",
				s.name, plan, logSteps, probes)
		}
	}

	// THE RECORDS OF A TURN WHOSE END STATES NO TALLY are found by the turn
	// rather than by the day — they can fall on the day before — so that
	// read seeks the turn index once per turn named, and still probes the
	// in-flight key from each row it folds.
	legacy, legacyArgs, err := usageLegacyTurnsSQL(UsageWindow{Start: w.Start, End: w.End,
		Accepted: "done", SentBack: "self_iterate"}, []string{"t-1", "t-2"})
	if err != nil {
		t.Fatal(err)
	}
	plan := planOf(t, db, legacy, legacyArgs)
	turnSeeks, turnProbes := 0, 0
	for _, step := range strings.Split(plan, "\n") {
		switch {
		case scansLog(step), strings.HasPrefix(step, "SCAN u"),
			strings.Contains(step, "LIST SUBQUERY"):
			t.Errorf("an untallied turn's records: the plan is %q — step %q walks what it "+
				"should seek", plan, step)
		case strings.HasPrefix(step, "SEARCH e "):
			turnSeeks++
			if !strings.Contains(step, "crewlet_events_turn_idx (turn_id=?)") {
				t.Errorf("an untallied turn's records: the plan is %q — step %q does not "+
					"seek the turn index", plan, step)
			}
		case strings.HasPrefix(step, "SEARCH u "):
			turnProbes++
		}
	}
	if turnSeeks != 1 || turnProbes != 1 {
		t.Errorf("an untallied turn's records: the plan is %q — want one seek of the turn "+
			"index and one probe of the batches in flight", plan)
	}

	// A REPOSITORY'S NAME is a label read once per repository, off the row of
	// its newest read — found by that read's INSTANT, a seek of the log's key
	// rather than of the window — and the row it reads is one this node keeps.
	name, nameArgs := usageRepoNameSQL("repo-1", EncodeTime(at))
	plan = planOf(t, db, name, nameArgs)
	nameSeeks, nameProbes := 0, 0
	for _, step := range strings.Split(plan, "\n") {
		switch {
		case scansLog(step), strings.HasPrefix(step, "SCAN u"), strings.Contains(step, "LIST SUBQUERY"):
			t.Errorf("a repository's name: the plan is %q — step %q walks what it should seek", plan, step)
		case strings.HasPrefix(step, "SEARCH e "):
			nameSeeks++
			if !strings.Contains(step, "event_time=?") {
				t.Errorf("a repository's name: the plan is %q — step %q does not seek the read's "+
					"instant", plan, step)
			}
		case strings.HasPrefix(step, "SEARCH u "):
			nameProbes++
		}
	}
	if nameSeeks != 1 || nameProbes != 1 {
		t.Errorf("a repository's name: the plan is %q — want one seek of the log by the read's "+
			"instant and one probe of the batches in flight", plan)
	}

	query, args := markUsageRederiveSQL("batch-1")
	if plan := planOf(t, db, query, args); !strings.HasPrefix(plan,
		"SEARCH u USING INDEX custody_unsettled_rows_batch_idx") ||
		!strings.Contains(plan, "SEARCH e ") || scansAnyLog(plan) {
		t.Errorf("a kept settle's mark: the plan is %q — it must start from the batch's "+
			"identities and seek the log for each", plan)
	}
}

// AN IN-FLIGHT BATCH COSTS A COUNT A PROBE PER ROW, HOWEVER LARGE IT IS.
//
// What the plan above promises, measured: the fingerprint of a busy day beside
// a backlog a coordination outage leaves — fifty thousand identities — takes
// about what it takes beside none, where a `NOT IN` list over the batches'
// JSON took seconds (measured).
// The bound is generous on purpose (a loaded runner is not a benchmark): the
// list's cost was two orders of magnitude over it.
func TestAnInFlightBacklogDoesNotSlowAUsageCount(t *testing.T) {
	t.Parallel()
	db, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	day := now().Add(-24 * time.Hour)
	bulk := func(n int, insert string, row func(i int) []any) {
		t.Helper()
		tx, err := db.sql.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := range n {
			if _, err := tx.ExecContext(t.Context(), insert, row(i)...); err != nil {
				_ = tx.Rollback()
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	bulk(5000, `INSERT INTO crewlet_events (event_time, event_id, event_type, source,
		category, actor, payload, tags) VALUES (?, ?, 'agent_phase_completed', 's', 'agent',
		'a', '{}', '{}')`, func(i int) []any {
		return []any{EncodeTime(day.Add(time.Duration(i) * time.Second)), fmt.Sprintf("ev-%d", i)}
	})
	bulk(50_000, `INSERT INTO custody_unsettled_rows (row_time, row_id, batch_id)
		VALUES (?, ?, 'backlog')`, func(i int) []any {
		return []any{EncodeTime(day.Add(-48*time.Hour + time.Duration(i)*time.Millisecond)),
			fmt.Sprintf("x-%d", i)}
	})
	query, args := usageMarkSQL(UsageWindow{Start: day.Add(-time.Hour), End: now()})
	start := time.Now()
	var count, newest int64
	if err := db.sql.QueryRowContext(t.Context(), query, args...).Scan(&count, &newest); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 500*time.Millisecond || count != 5000 {
		t.Fatalf("the fingerprint beside fifty thousand identities in flight took %v and "+
			"counted %d rows — want the day's 5000 in a probe per row", took, count)
	}
}

// AND A FLEET READ'S KEPT ROWS COST THE ROWS NAMED, NOT THE BACKLOG.
//
// Every fleet read that names unsettled rows asks every node which of them it
// keeps ([EventLog.KeptRows]). Read whole into a set beside the rows named, the
// in-flight table cost each such read about 170 ms beside the fifty thousand
// identities a coordination outage leaves; probed from each row named, the
// answer is the same for a millisecond. The bound is generous for the reason
// the one above is.
//
// Mutation: read the in-flight table whole again, and this goes red on time.
func TestAnInFlightBacklogDoesNotSlowTheKeptRows(t *testing.T) {
	t.Parallel()
	db, err := OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	day := now().Add(-24 * time.Hour)
	tx, err := db.sql.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 50_000 {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO custody_unsettled_rows
			(row_time, row_id, batch_id) VALUES (?, ?, 'backlog')`,
			EncodeTime(day.Add(time.Duration(i)*time.Millisecond)), fmt.Sprintf("x-%d", i)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	named := []UnsettledRow{{Time: day, ID: "kept-1"}, {Time: day.Add(time.Minute), ID: "kept-2"}}
	for _, r := range named {
		if err := db.Events().Append(t.Context(), EventRecord{ID: r.ID, Type: "x", Time: r.Time,
			Category: "agent", Payload: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	kept, err := db.Events().KeptRows(t.Context(), named)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 50*time.Millisecond || len(kept) != 2 {
		t.Fatalf("the kept rows of two named beside fifty thousand identities in flight "+
			"took %v and answered %d — want both in a probe per row named", took, len(kept))
	}
}

// scansLog reports a plan step that walks the event log rather than seeking it.
func scansLog(step string) bool {
	return step == "SCAN e" || strings.HasPrefix(step, "SCAN e ") ||
		strings.HasPrefix(step, "SCAN crewlet_events") || strings.HasPrefix(step, "SCAN scheduled_runs")
}

// scansAnyLog is [scansLog] over every step of a plan.
func scansAnyLog(plan string) bool {
	for _, step := range strings.Split(plan, "\n") {
		if scansLog(step) {
			return true
		}
	}
	return false
}
