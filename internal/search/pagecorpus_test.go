package search_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
)

// pageRow is one row of pages_heads as this corpus reads it.
type pageRow struct {
	id, container, title, body, status string
	edit, version                      int64
	trashed                            bool
}

// A RENAME MUST NOT RE-EMBED A PAGE, and the column choice is the whole of it.
//
// `pages_heads` carries two version numbers. `version` is the composed LOG
// version and a rename stamps it, because a rename is its own record. Keyed on
// that, every title change in the company would select the page again and buy
// a new vector for text nobody touched — a provider bill for a rename, on a
// corpus where renames are ordinary.
//
// `edit_version` is the page's own monotonic edit number, which a save
// increments and a rename deliberately does not. That is what the vector is
// stored against.
func TestARenamedPageIsNotReEmbedded(t *testing.T) {
	t.Parallel()
	db := pagesStore(t)
	corpus := search.PageCorpus{DB: db}

	writePage(t, db, pageRow{
		id: "p1", container: "eng", title: "Runbook", body: "how to restart",
		status: "published", edit: 3, version: 41,
	})
	writeVector(t, db, "page", "p1", 3)

	stale, _, err := corpus.Stale(t.Context(), "m", 8, 10)
	if err != nil {
		t.Fatalf("Stale: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("a page whose vector matches its edit version was selected "+
			"for embedding: %+v", stale)
	}

	// THE RENAME: the log version moves and the edit version does not.
	writePage(t, db, pageRow{
		id: "p1", container: "eng", title: "The Runbook", body: "how to restart",
		status: "published", edit: 3, version: 87,
	})
	stale, _, err = corpus.Stale(t.Context(), "m", 8, 10)
	if err != nil {
		t.Fatalf("Stale: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("a renamed page was selected for re-embedding, so every title "+
			"change in the company costs a provider call for text nobody "+
			"edited: %+v", stale)
	}

	// THE CONTROL: an actual EDIT does select it, or the assertion above
	// would pass on a corpus that never selects anything.
	writePage(t, db, pageRow{
		id: "p1", container: "eng", title: "The Runbook", body: "how to restart, slowly",
		status: "published", edit: 4, version: 88,
	})
	stale, _, err = corpus.Stale(t.Context(), "m", 8, 10)
	if err != nil {
		t.Fatalf("Stale: %v", err)
	}
	if len(stale) != 1 {
		t.Errorf("an edited page produced %d stale document(s)", len(stale))
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
	corpus := search.PageCorpus{DB: db}

	for _, row := range []pageRow{
		{id: "kept", container: "eng", title: "Kept", body: "b", status: "published", edit: 1},
		{id: "trashed", container: "eng", title: "Gone", body: "b", status: "published", edit: 1, trashed: true},
		{id: "draft", container: "eng", title: "Draft", body: "b", status: "draft", edit: 1},
	} {
		writePage(t, db, row)
		writeVector(t, db, "page", row.id, 1)
	}

	stale, gone, err := corpus.Stale(t.Context(), "m", 8, 10)
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

func pagesStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
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

func writeVector(t *testing.T, db *store.DB, source, id string, rev int64) {
	t.Helper()
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.WithoutCancel(t.Context()), `
			INSERT INTO kb_vectors
				(source, source_id, source_rev, model, dim, search_shard,
				 text_sha, embedding, embedded_at, version)
			VALUES (?, ?, ?, 'm', 8, 0, 'sha', x'', 0, 1)
			ON CONFLICT (source, source_id) DO UPDATE SET
				source_rev = excluded.source_rev`,
			source, id, rev)
		return err
	}); err != nil {
		t.Fatalf("write a vector: %v", err)
	}
}
