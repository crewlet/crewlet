package store

import (
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

// THE IN-FLIGHT ROWS A NODE WROTE BEFORE THE KEYED TABLE ARE CARRIED ACROSS, and
// an entry that names no row is not.
//
// Node migration 0048 moves each batch's identities out of the JSON list 0038
// kept on the batch's row and into `custody_unsettled_rows`, one row each. A
// node that upgrades with batches in flight has to come out of it with exactly
// those rows still out of every count — and an entry missing either half of
// its identity, which no writer ever made, names no row: it is dropped rather
// than carried, so it can neither take a row out of a count nor refuse the
// migration over a key the table cannot hold. Each half of the guard is held
// here by an entry sharing the OTHER half with a row this node keeps.
//
// The case puts the estate back as 0047 left it — the batch table in 0038's
// shape with its list, the keyed tables absent, 0048 unrecorded — and runs the
// migrator, which is the only thing that ever applies the file.
//
// Mutation: drop either half of the back-fill's `IS NOT NULL` guard, and the
// migration refuses the entry; carry nothing across, and the in-flight row is
// counted.
func TestAnUpgradeCarriesTheRowsInFlightAcross(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := OpenNode(ctx, filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	day := now().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	own, flight := day.Add(time.Hour), day.Add(2*time.Hour)
	for _, rec := range []EventRecord{
		{ID: "own", Type: phaseCompleted, Time: own, Category: "agent", Payload: []byte(`{}`),
			Tags: map[string]string{"agent_id": "seat-1"}},
		{ID: "flight", Type: phaseCompleted, Time: flight, Category: "agent", Payload: []byte(`{}`),
			Tags: map[string]string{"agent_id": "seat-1"}},
	} {
		if err := db.Events().Append(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		`DROP TABLE custody_unsettled_rows`,
		`DROP TABLE custody_settled`,
		`DROP TABLE custody_unsettled`,
		`CREATE TABLE custody_unsettled (batch_id TEXT NOT NULL PRIMARY KEY,
			origin TEXT NOT NULL, written_at INTEGER NOT NULL, events TEXT NOT NULL)`,
		`INSERT INTO custody_unsettled (batch_id, origin, written_at, events) VALUES
			('batch', 'stateless-1', 1, json_array(
				json_object('t', ` + strconv.FormatInt(EncodeTime(flight), 10) + `, 'id', 'flight'),
				json_object('id', 'own'),
				json_object('t', ` + strconv.FormatInt(EncodeTime(own), 10) + `),
				json_object()))`,
		`DELETE FROM schema_migrations WHERE version = '0048_a_custody_row_is_found_by_its_identity.sql'`,
	} {
		if _, err := db.sql.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("put the estate back as 0047 left it: %v\n%s", err, stmt)
		}
	}
	done, err := db.migrate(ctx)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !slices.Contains(done, "0048_a_custody_row_is_found_by_its_identity.sql") {
		t.Fatalf("the migrator applied %v, not 0048", done)
	}

	keys, err := func() ([]RowKey, error) {
		rows, err := db.sql.QueryContext(ctx,
			`SELECT row_time, row_id FROM custody_unsettled_rows WHERE batch_id = 'batch'`)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		var out []RowKey
		for rows.Next() {
			var k RowKey
			if err := rows.Scan(&k.At, &k.ID); err != nil {
				return nil, err
			}
			out = append(out, k)
		}
		return out, rows.Err()
	}()
	if err != nil {
		t.Fatal(err)
	}
	if want := []RowKey{{At: EncodeTime(flight), ID: "flight"}}; !slices.Equal(keys, want) {
		t.Errorf("the batch's in-flight rows are %v after the upgrade, want %v", keys, want)
	}
	mark, err := db.UsageMark(ctx, UsageWindow{Start: day, End: day.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if mark.Events != 1 || mark.LastEvent != EncodeTime(own) {
		t.Errorf("after the upgrade the day counts %d row(s), newest %d — want this node's "+
			"own row alone: the batch's row is still in flight, and an entry naming half "+
			"of the own row's identity names no row", mark.Events, mark.LastEvent)
	}
	if left, err := db.Events().UnsettledCustody(ctx, time.Now().Add(time.Hour), UnsettledBatch{}, 10); err != nil ||
		len(left) != 1 || left[0].ID != "batch" {
		t.Errorf("after the upgrade the unsettled batches are (%v, %v), want the one written", left, err)
	}
}
