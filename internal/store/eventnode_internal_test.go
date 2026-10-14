package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// THE BACKFILL NAMES AN ORIGIN ONLY WHERE THE PAYLOAD IS THE ENVELOPE.
//
// schema/0048 promotes the envelope's `node` out of the rows an upgrade already
// holds. A stored payload is the whole event — except an inbound delivery's,
// which is filed with the vendor's own body, and a vendor body may carry a
// `node` that names nothing here. The envelope's `id` is the row's own
// `event_id`, which is what tells the two apart. Run against a database that
// has every migration, its rows written as an older build left them — the
// column at its default — by the migration's own statement, read out of the
// binary rather than restated here.
//
// Mutation: drop the `id = event_id` term and the delivery row is stamped with
// the vendor's field; drop the `json_type` term and a row with no node is
// stamped NULL, which the column refuses.
func TestTheBackfillNamesAnOriginOnlyWhereThePayloadIsTheEnvelope(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, err := OpenNode(ctx, filepath.Join(t.TempDir(), "node.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	at := time.Now().UTC().Add(-time.Hour)
	for i, r := range []struct{ id, payload string }{
		{"e-env", `{"id":"e-env","type":"turn_completed","node":"node-a"}`},
		{"e-delivery", `{"id":"vendor-7","node":"vendor-host","action":"opened"}`},
		{"e-bare", `{"id":"e-bare","type":"turn_completed"}`},
	} {
		// Node left empty: the row as a build before 0048 wrote it.
		if err := db.Events().Append(ctx, EventRecord{ID: r.id, Type: "x", Category: "task",
			Time: at.Add(time.Duration(i) * time.Second), Payload: json.RawMessage(r.payload)}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := schemaFS.ReadFile("schema/node/0048_an_event_names_the_node_that_published_it.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, backfill, ok := strings.Cut(string(raw), "UPDATE crewlet_events")
	if !ok {
		t.Fatal("schema/0048 holds no backfill statement")
	}
	if _, err := db.sql.ExecContext(ctx, "UPDATE crewlet_events"+backfill); err != nil {
		t.Fatalf("the backfill: %v", err)
	}
	for id, want := range map[string]string{"e-env": "node-a", "e-delivery": "", "e-bare": ""} {
		var got string
		if err := db.sql.QueryRowContext(ctx,
			`SELECT node FROM crewlet_events WHERE event_id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s was backfilled as published by %q, want %q", id, got, want)
		}
	}
}
