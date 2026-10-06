package search_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
)

// THE WINDOW REPORT MEASURES WHAT THE EMBEDDED OPENING LEAVES OUT, AS THE DUTY
// PREPARES IT.
//
// It is the measurement that decides whether a company needs more than one
// vector a source, so it must count what the semantic half actually cannot
// see: each source the corpora embed — never a trashed page or a draft — as
// its title, one space and its whole body with the whitespace collapsed, the
// way the window is measured when a vector is computed. A whitespace-heavy body
// is short once prepared and lies inside the window; counted raw, it would
// report text past the window that the model would never have been sent.
func TestTheWindowReportMeasuresWhatTheEmbeddedOpeningLeavesOut(t *testing.T) {
	t.Parallel()
	db := pagesStore(t)
	writeTask(t, db, "t-short", "Short", "a short body")
	writeTask(t, db, "t-long", "Long", strings.Repeat("abcdefghi ", 1000))
	writeTask(t, db, "t-spaces", "Ws", "x"+strings.Repeat(" ", 20_000)+"y")
	writePage(t, db, pageRow{id: "p-long", container: "eng", title: "Page",
		body: strings.Repeat("é", 5000), status: "published", edit: 1, version: 1})
	writePage(t, db, pageRow{id: "p-trashed", container: "eng", title: "Gone",
		body: strings.Repeat("z", 20_000), status: "published", edit: 1, version: 1,
		trashed: true})
	writePage(t, db, pageRow{id: "p-draft", container: "eng", title: "Draft",
		body: strings.Repeat("z", 20_000), status: "draft", edit: 1, version: 1})

	var reports []search.WindowReport
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		var err error
		reports, err = search.Window(t.Context(), tx, search.EmbedInputBytes)
		return err
	}); err != nil {
		t.Fatalf("Window: %v", err)
	}
	want := map[search.Source]search.WindowReport{
		// "Short a short body" (18), "Long abcdefghi …" (10 004, of which the
		// 8 192-byte opening leaves 1 812), "Ws x y" (6).
		search.SourceTask: {Source: search.SourceTask, Sources: 3, Beyond: 1,
			Bytes: 18 + 10_004 + 6, BeyondBytes: 1_812},
		// "Page " and 5 000 two-byte characters (10 005), cut on a character
		// boundary at 8 191.
		search.SourcePage: {Source: search.SourcePage, Sources: 1, Beyond: 1,
			Bytes: 10_005, BeyondBytes: 1_814},
	}
	if len(reports) != len(want) {
		t.Fatalf("the report covers %d corpora: %+v", len(reports), reports)
	}
	for _, got := range reports {
		if got != want[got.Source] {
			t.Errorf("the %s corpus measured %+v, want %+v", got.Source, got, want[got.Source])
		}
	}
}

// writeTask writes one live tracker row directly, with its body in the encoded
// document where the tracker keeps it.
func writeTask(t *testing.T, db *store.DB, id, title, body string) {
	t.Helper()
	document, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.WithoutCancel(t.Context()), `
			INSERT INTO tracker_tasks
				(id, key, project_key, root_id, type, title, status,
				 status_group, rank, version, created_at, updated_at, document)
			VALUES (?, ?, 'ENG', ?, 'task', ?, 'todo', 'not_started', 'm0',
			        1, 0, 1, ?)`,
			id, strings.ToUpper(id), id, title, document)
		return err
	}); err != nil {
		t.Fatalf("write a task: %v", err)
	}
}
