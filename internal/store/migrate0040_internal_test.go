package store

import (
	"path"
	"path/filepath"
	"slices"
	"testing"
)

// migration0040 is the file under test, named once.
const migration0040 = "0040_a_spend_row_counts_its_calls.sql"

// 0040 GIVES EVERY PHASE ROW ALREADY STORED ITS PROVIDER CALLS, by the rule the
// writer now applies.
//
// A rollup's "N calls" counted rows, and from this migration it sums `calls`:
// a stored phase row left at the column's default of zero would count as no
// call at all, and a month of history would read as a company that made none.
// Each phase gets its rounds where it recorded them, its rounds_used where it
// predates that list, and one where neither says — and a row that is no call
// (any other type) stays zero.
func TestNode0040CountsTheCallsOfEveryPhaseAnUpgradeHolds(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	pool, err := openPrepared(ctx, filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("openPrepared: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	db := &DB{sql: pool, estate: EstateNode}

	if _, err := pool.ExecContext(ctx, `
		CREATE TABLE schema_migrations (
			version    TEXT    NOT NULL PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	files, err := schemaVersions(EstateNode)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(files, migration0040) {
		t.Fatalf("%s is not among the node migrations %v", migration0040, files)
	}
	for _, name := range files {
		if name >= migration0040 {
			break
		}
		body, err := schemaFS.ReadFile(path.Join("schema", string(EstateNode), name))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.applyOne(ctx, name, string(body)); err != nil {
			t.Fatalf("bring the file to 0039: %v", err)
		}
	}

	rows := []struct {
		id, eventType, payload string
		want                   int
	}{
		{"rounds", "agent_phase_completed",
			`{"rounds":[{"round":1},{"round":2},{"round":3}],"rounds_used":9}`, 3},
		{"older", "agent_phase_completed", `{"rounds_used":5}`, 5},
		{"judge", "agent_phase_completed", `{"phase":"judge"}`, 1},
		{"empty-list", "agent_phase_completed", `{"rounds":[],"rounds_used":2}`, 2},
		{"no-call", "turn_completed", `{"rounds_used":7}`, 0},
	}
	for i, row := range rows {
		if _, err := pool.ExecContext(ctx, `
			INSERT INTO crewlet_events (event_time, event_id, event_type, source,
				category, summary, actor, tags, payload)
			VALUES (?, ?, ?, 'engine', 'system', '', '', '{}', ?)`,
			i+1, row.id, row.eventType, row.payload); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}

	applied, err := db.migrate(ctx)
	if err != nil {
		t.Fatalf("migrate a populated 0039 database: %v", err)
	}
	if !slices.Contains(applied, migration0040) {
		t.Fatalf("applied %v, want %s among them", applied, migration0040)
	}
	for _, row := range rows {
		var calls int
		var stage string
		if err := pool.QueryRowContext(ctx,
			`SELECT calls, spend_stage FROM crewlet_events WHERE event_id = ?`,
			row.id).Scan(&calls, &stage); err != nil {
			t.Fatal(err)
		}
		if calls != row.want || stage != "" {
			t.Errorf("%s: calls = %d, stage = %q, want %d and no stage", row.id, calls, stage, row.want)
		}
	}
}

// THE WRITER APPLIES THE MIGRATION'S RULE, so a phase row written after the
// upgrade and one backfilled by it count their calls alike — and an auxiliary
// record counts the calls it coalesced, filed as the auxiliary phase with its
// purpose as the worker and its stage beside it.
func TestASpendRowCountsItsProviderCalls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, eventType, payload string
		want                     Spend
	}{{
		name: "a phase's rounds", eventType: "agent_phase_completed",
		payload: `{"phase":"execute","rounds":[{},{},{}],"rounds_used":9,"total_tokens":10}`,
		want:    Spend{Phase: "execute", TotalTokens: 10, Calls: 3},
	}, {
		name: "an older phase's rounds_used", eventType: "agent_phase_completed",
		payload: `{"phase":"execute","rounds_used":5}`,
		want:    Spend{Phase: "execute", Calls: 5},
	}, {
		name: "a judge's single call", eventType: "agent_phase_completed",
		payload: `{"phase":"judge","host_phase":"execute"}`,
		want:    Spend{Phase: "judge", HostPhase: "execute", Calls: 1},
	}, {
		name: "an auxiliary record", eventType: "auxiliary_spend",
		payload: `{"stage":"reflection","purpose":"persist_decider","calls":7,` +
			`"turn_id":"t-1","model":"haiku","provider_key":"cheap","total_tokens":900,` +
			`"input_tokens":800,"output_tokens":100,"cache_read_tokens":300}`,
		want: Spend{Phase: "auxiliary", Worker: "persist_decider", Stage: "reflection",
			TurnID: "t-1", Model: "haiku", ProviderKey: "cheap", TotalTokens: 900,
			InputTokens: 800, OutputTokens: 100, CacheReadTokens: 300, Calls: 7},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := SpendFor(tc.eventType, []byte(tc.payload))
			if got == nil || *got != tc.want {
				t.Errorf("spend = %+v, want %+v", got, tc.want)
			}
		})
	}
	if got := SpendFor("turn_completed", []byte(`{"total_tokens":5}`)); got != nil {
		t.Errorf("a record that is no call has spend %+v", got)
	}
}
