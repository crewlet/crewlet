package search_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// pageRow is one row of pages_heads as this corpus reads it.
type pageRow struct {
	id, container, title, body, status string
	edit, version                      int64
	trashed                            bool
}

// vectorOf is what a page's vector row says it was computed from.
type vectorOf struct {
	rev              int64
	title, container string
}

// A PAGE IS SELECTED EXACTLY WHEN WHAT ITS VECTOR WAS COMPUTED FROM MOVED.
//
// A vector stands for a page's title and the opening of its body, filed under
// its container, and those move on three paths: a save moves the edit number,
// a rename the title and perhaps the container, a retitle the title. Keyed on
// the edit number alone a renamed page kept its old title's vector and a moved
// one answered scoped searches from the container it left; keyed on the log
// version, which every comment and watcher change stamps too, each of those
// would cost a vector record for a text nobody touched. So the selection asks
// the three things the vector row records — and nothing else.
func TestAPageIsSelectedExactlyWhenWhatItsVectorWasComputedFromMoved(t *testing.T) {
	t.Parallel()
	base := pageRow{id: "p1", container: "eng", title: "Runbook", body: "how to restart",
		status: "published", edit: 3, version: 41}
	current := vectorOf{rev: 3, title: "Runbook", container: "eng"}
	for _, tc := range []struct {
		name     string
		page     func(pageRow) pageRow
		selected bool
	}{
		{name: "nothing moved", page: func(p pageRow) pageRow { return p }},
		{name: "a comment or a watcher stamps the log version", page: func(p pageRow) pageRow {
			p.version = 87
			return p
		}},
		{name: "a save moves the edit number", selected: true, page: func(p pageRow) pageRow {
			p.edit, p.version, p.body = 4, 88, "how to restart, slowly"
			return p
		}},
		{name: "a rename moves the title", selected: true, page: func(p pageRow) pageRow {
			p.title, p.version = "The Runbook", 89
			return p
		}},
		{name: "a rename moves the container", selected: true, page: func(p pageRow) pageRow {
			p.container, p.version = "ops", 90
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := pagesStore(t)
			corpus := search.PageCorpus{DB: db.Replicated().Reader()}
			writePage(t, db, tc.page(base))
			writeVector(t, db, "page", "p1", current)

			stale, _, err := corpus.Stale(t.Context(), "m", 8, 10, search.Held{})
			if err != nil {
				t.Fatalf("Stale: %v", err)
			}
			if got := len(stale) == 1; got != tc.selected {
				t.Fatalf("the page was selected: %v, want %v (%+v)", got, tc.selected, stale)
			}
			// AND THE COVERAGE COUNTS THE SAME POPULATION: a page the
			// selection would take next is not covered.
			have, total, err := corpus.Coverage(t.Context(), "m", 8)
			if err != nil {
				t.Fatalf("Coverage: %v", err)
			}
			if covered := have == 1; total != 1 || covered == tc.selected {
				t.Fatalf("coverage is %d of %d where the page is selected: %v — the "+
					"two must be one predicate", have, total, tc.selected)
			}
		})
	}
}

// A HELD PAGE TAKES NONE OF THE SELECTION'S PLACES.
//
// A page the provider refused alone is never embedded, so it keeps its place at
// the front of every oldest-first selection after it. Read only to the limit,
// the selection filled with held pages and passed them over, so a wiki with as
// many refused pages as the limit — one tick's sources — embedded no page
// written or edited afterwards, ever. The task corpus is held to the same rule
// through the duty; this holds the page corpus's own statement to it.
func TestHeldPagesNeverTakeThePageSelectionsPlaces(t *testing.T) {
	t.Parallel()
	db := pagesStore(t)
	corpus := search.PageCorpus{DB: db.Replicated().Reader()}
	const limit = 4
	var held []search.Document
	at := int64(1)
	for i := range limit + 1 {
		row := pageRow{id: fmt.Sprintf("held-%d", i), container: "eng",
			title: fmt.Sprintf("Refused %d", i), body: "a page the provider refuses",
			status: "published", edit: 2, version: at}
		writePage(t, db, row)
		held = append(held, search.Document{ID: row.id, Version: uint64(row.edit), Title: row.title})
		at++
	}
	for i := range 2 * limit {
		writePage(t, db, pageRow{id: fmt.Sprintf("fresh-%d", i), container: "eng",
			title: fmt.Sprintf("Written afterwards %d", i), body: "a page to embed",
			status: "published", edit: 1, version: at})
		at++
	}

	stale, _, err := corpus.Stale(t.Context(), "m", 8, limit, search.HeldOf(held...))
	if err != nil {
		t.Fatalf("Stale: %v", err)
	}
	if len(stale) != limit {
		t.Fatalf("with %d pages held the selection returned %d page(s), want the %d "+
			"written afterwards that fit the limit", len(held), len(stale), limit)
	}
	for i, doc := range stale {
		if want := fmt.Sprintf("fresh-%d", i); doc.ID != want || doc.Body == "" {
			t.Fatalf("the selection's page %d is %q with body %q, want %s, oldest "+
				"first and read", i, doc.ID, doc.Body, want)
		}
	}
}

// A PAGE SOMEBODY TOOK DOWN IS NOT SEARCHABLE BY MEANING EITHER.
//
// The selection above is driven by the rows that REMAIN, so nothing in it will
// ever revisit a page that was trashed or unpublished — its vector would
// survive for the life of the deployment and keep answering searches for a
// document the company deliberately withdrew.
func TestAWithdrawnPageLosesItsVector(t *testing.T) {
	t.Parallel()
	db := pagesStore(t)
	corpus := search.PageCorpus{DB: db.Replicated().Reader()}

	for _, row := range []pageRow{
		{id: "kept", container: "eng", title: "Kept", body: "b", status: "published", edit: 1},
		{id: "trashed", container: "eng", title: "Gone", body: "b", status: "published", edit: 1, trashed: true},
		{id: "draft", container: "eng", title: "Draft", body: "b", status: "draft", edit: 1},
	} {
		writePage(t, db, row)
		writeVector(t, db, "page", row.id, vectorOf{rev: 1, title: row.title, container: row.container})
	}

	stale, gone, err := corpus.Stale(t.Context(), "m", 8, 10, search.Held{})
	if err != nil {
		t.Fatalf("Stale: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("a fully embedded corpus produced %d stale document(s)", len(stale))
	}
	if len(gone) != 2 {
		t.Errorf("a trashed page and a draft left %d vector(s) to forget; both "+
			"are documents no search may return and nothing else will ever "+
			"select them", len(gone))
	}

	// AND THE COVERAGE COUNTS THE SAME POPULATION. A denominator that
	// included the withdrawn pages would report a fully embedded company
	// as a third short, for ever.
	fraction, known, err := search.Coverage(t.Context(),
		[]search.Corpus{corpus}, "m", 8)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if !known || fraction != 1 {
		t.Errorf("coverage over one published page is %.3f (known=%v)",
			fraction, known)
	}
}

// A MOVED PAGE IS RESTAMPED AND A RENAMED ONE EMBEDDED, through the duty.
//
// A move to another container keeps the title and the body, so its stored
// vector is still its text's: the duty republishes it under the new container
// with no provider call, and a scoped semantic search finds the page where it
// now lives. A rename to another title is a new text, and costs one request.
// And a comment costs nothing at all.
func TestAMovedPageIsRestampedAndARenamedOneEmbedded(t *testing.T) {
	t.Parallel()
	h := newEmbedHarness(t)
	page := pageRow{id: "p1", container: "eng", title: "Runbook",
		body: "how to restart the worker", status: "published", edit: 1, version: 1}
	writePage(t, h.db, page)
	duty := h.dutyOver(search.PageCorpus{DB: h.db.Replicated().Reader()})
	if published, err := duty.Tick(t.Context()); err != nil || published != 1 {
		t.Fatalf("the first tick published %d (%v)", published, err)
	}
	h.drain()
	sent := len(h.embedder.sent())

	page.version = 2 // a comment
	writePage(t, h.db, page)
	if published, err := duty.Tick(t.Context()); err != nil || published != 0 {
		t.Fatalf("a comment published %d record(s) (%v)", published, err)
	}

	page.container, page.version = "ops", 3 // a move
	writePage(t, h.db, page)
	if published, err := duty.Tick(t.Context()); err != nil || published != 1 {
		t.Fatalf("a move published %d (%v), want the restamp", published, err)
	}
	if again := len(h.embedder.sent()); again != sent {
		t.Fatalf("a move sent %d request(s) for a text that did not change", again-sent)
	}
	h.drain()
	if row := h.storedRow(search.SourcePage, "p1"); row.container != "ops" || row.binContainer != "ops" {
		t.Fatalf("after the move the vector rows are filed under %q and %q, want ops",
			row.container, row.binContainer)
	}

	page.title, page.version = "The Runbook", 4 // a rename
	writePage(t, h.db, page)
	if published, err := duty.Tick(t.Context()); err != nil || published != 1 {
		t.Fatalf("a rename published %d (%v)", published, err)
	}
	if again := len(h.embedder.sent()); again != sent+1 {
		t.Fatalf("a rename went in %d request(s), want one — the title is the "+
			"first thing the vector embeds", again-sent)
	}
	h.drain()
	if covered, total, err := (search.PageCorpus{DB: h.db.Replicated().Reader()}).Coverage(
		t.Context(), embedModel, h.embedder.Width()); err != nil || covered != 1 || total != 1 {
		t.Fatalf("after the rename coverage is %d of %d (%v)", covered, total, err)
	}
}

func pagesStore(t *testing.T) *store.DB {
	t.Helper()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// writePage writes one pages_heads row DIRECTLY: this suite is about the
// corpus's selection, not about the pages applier that normally writes it.
func writePage(t *testing.T, db *store.DB, row pageRow) {
	t.Helper()
	var trashed any
	if row.trashed {
		trashed = int64(1)
	}
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.WithoutCancel(t.Context()), `
			INSERT INTO pages_heads
				(id, container, title, title_norm, body, status, edit_version,
				 created_at, updated_at, trashed_at, version, document)
			VALUES (?, ?, ?, LOWER(?), ?, ?, ?, 0, ?, ?, ?, x'')
			ON CONFLICT (id) DO UPDATE SET
				container = excluded.container,
				title = excluded.title, title_norm = excluded.title_norm,
				body = excluded.body, status = excluded.status,
				edit_version = excluded.edit_version,
				updated_at = excluded.updated_at,
				trashed_at = excluded.trashed_at, version = excluded.version`,
			row.id, row.container, row.title, row.title, row.body, row.status,
			row.edit, row.version, trashed, row.version)
		return err
	}); err != nil {
		t.Fatalf("write a page: %v", err)
	}
}

// writeVector writes one vector row directly, saying what it was computed
// from.
func writeVector(t *testing.T, db *store.DB, source, id string, of vectorOf) {
	t.Helper()
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.WithoutCancel(t.Context()), `
			INSERT INTO kb_vectors
				(source, source_id, source_rev, title, container, model, dim,
				 search_shard, text_sha, embedding, embedded_at, version)
			VALUES (?, ?, ?, ?, ?, 'm', 8, 0, 'sha', x'', 0, 1)
			ON CONFLICT (source, source_id) DO UPDATE SET
				source_rev = excluded.source_rev, title = excluded.title,
				container = excluded.container`,
			source, id, of.rev, of.title, of.container)
		return err
	}); err != nil {
		t.Fatalf("write a vector: %v", err)
	}
}
