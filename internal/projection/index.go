package projection

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/textindex"
)

// Doc is one document offered to the index — a page or a work item.
//
// The SOURCE ROWS ARE NOT INDEXED IN PLACE, and that separation is the point:
// the index is a different lifecycle from the projection. Rows land in
// seconds; the index is built asynchronously behind them, can be dropped and
// rebuilt wholesale when the analyzer changes, and is what a search reads so
// that a query is one scan over postings rather than a UNION over two
// schemas.
type Doc struct {
	// Source is "page" or "item".
	Source string

	// ID is the source record's own id.
	ID string

	// Container scopes the document — a page's space, an item's project —
	// so a scoped search filters without joining back to the source table.
	Container string

	Title string
	Body  string

	// Version is the source's own version, so the indexer can tell a stale
	// row from a current one without re-tokenising the body.
	Version uint64
}

// docKey is the index's own primary key for a document.
//
// SOURCE-QUALIFIED, because a page and a work item can share an id-shaped
// string and the index is one table over both. It is deterministic so an
// upsert finds the existing row rather than accumulating one per re-index.
func docKey(source, id string) string { return source + ":" + id }

// IndexBatch is how many documents one index transaction carries.
//
// Twenty. Tokenising a 20 KB page and writing its postings measured around
// 60 ms, so a batch is a little over a second of work — long enough to
// amortise the transaction, short enough that the writer's own applies are
// never behind an index batch for a noticeable time. A 5,000-page company
// therefore takes about five minutes to index, which is why [Indexer.Ready]
// exists and reports false meanwhile.
const IndexBatch = 20

// Indexer maintains the lexical index behind the projection.
//
// SEPARATE FROM THE PROJECTOR, and it must be: an apply has to be fast and
// synchronous with the change feed, while indexing a page is tokenising tens
// of kilobytes and writing hundreds of posting rows. Doing it inline would
// put the whole index build inside the change feed's own transaction, and a
// node catching up on a large company would stop applying changes entirely
// while it worked.
type Indexer struct {
	db *store.DB
}

// NewIndexer builds an indexer over a node's store.
func NewIndexer(db *store.DB) *Indexer { return &Indexer{db: db} }

// Upsert indexes documents, replacing whatever each had before.
//
// REPLACE RATHER THAN MERGE. A page edit that removed a paragraph must remove
// its terms, and a merge would leave the document matching a word it no
// longer contains — which reads to a person as the search inventing a result.
func (x *Indexer) Upsert(ctx context.Context, docs []Doc) error {
	if len(docs) == 0 {
		return nil
	}
	now := store.EncodeTime(time.Now().UTC())
	return x.db.Tx(ctx, func(tx *sql.Tx) error {
		for _, doc := range docs {
			if err := x.upsertOne(ctx, tx, doc, now); err != nil {
				return err
			}
		}
		return nil
	})
}

func (x *Indexer) upsertOne(ctx context.Context, tx *sql.Tx, doc Doc, now int64) error {
	id := docKey(doc.Source, doc.ID)

	// THE TITLE IS INDEXED WITH THE BODY, and weighted by repetition rather
	// than by a field boost: a separate title field would need its own
	// posting table, its own IDF and a blending constant nothing could
	// justify, where repeating the title makes a title match worth about
	// three body mentions through the ordinary term-frequency path. That is
	// the effect a boost was reaching for, expressed in the arithmetic that
	// is already there.
	text := doc.Title + "\n" + doc.Title + "\n" + doc.Title + "\n" + doc.Body
	terms := textindex.Analyze(text)

	length := 0
	for _, n := range terms {
		length += n
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO kb_docs (id, source, source_id, container, title, excerpt,
		                     length, source_rev, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			container  = excluded.container,
			title      = excluded.title,
			excerpt    = excluded.excerpt,
			length     = excluded.length,
			source_rev = excluded.source_rev,
			indexed_at = excluded.indexed_at`,
		id, doc.Source, doc.ID, doc.Container, doc.Title,
		excerptOf(doc.Body), length, int64(doc.Version), now); err != nil {
		return fmt.Errorf("projection: index %s: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM kb_postings WHERE doc_id = ?`, id); err != nil {
		return fmt.Errorf("projection: clear postings for %s: %w", id, err)
	}
	for term, freq := range terms {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kb_postings (term, doc_id, freq) VALUES (?, ?, ?)`,
			term, id, freq); err != nil {
			return fmt.Errorf("projection: write posting %q for %s: %w", term, id, err)
		}
	}
	return nil
}

// excerptLimit bounds the stored excerpt, in bytes.
//
// Six hundred: enough for a snippet window wherever the query terms fall in
// the opening, and small enough that the index does not become a second copy
// of every body. A hit whose terms are deeper in the document gets its
// snippet cut from the excerpt, which is why this is not [knowledge.SnippetLimit].
const excerptLimit = 600

// excerptOf keeps the opening of a body for snippet rendering.
//
// Whitespace is collapsed first, so a markdown body's blank lines do not
// spend the budget, and the cut goes through [textcut.Bytes] — a plain slice
// splits a multi-byte rune, and the invalid UTF-8 that produces is
// substituted by the JSON encoder and read by a model as a replacement
// character. No marker: this value is snippet INPUT, and an ellipsis inside
// it would be cut again by the snippet.
func excerptOf(body string) string {
	return textcut.Bytes(strings.Join(strings.Fields(body), " "), excerptLimit)
}

// Remove drops a document from the index.
func (x *Indexer) Remove(ctx context.Context, source, id string) error {
	return x.db.Tx(ctx, func(tx *sql.Tx) error {
		// The postings cascade on kb_docs, so one delete is the whole
		// removal — and it is the reason kb_docs has no foreign key back
		// to the source tables: the projector deletes a source row and the
		// indexer deletes the index row, in that order, and a cascade from
		// the source would delete a posting list mid-write.
		_, err := tx.ExecContext(ctx,
			`DELETE FROM kb_docs WHERE source = ? AND source_id = ?`, source, id)
		return err
	})
}

// Stale returns up to limit documents whose index row is missing or behind
// the source's version.
//
// THE INDEXER'S WHOLE INPUT. It polls rather than being notified, because the
// two sides are deliberately decoupled: an apply must never wait on an index
// batch, and a notification queue between them would be a second buffer with
// its own overflow rule for exactly no benefit — the poll is one indexed scan
// and the work it finds is the same work either way.
func (x *Indexer) Stale(ctx context.Context, limit int) ([]Doc, error) {
	if limit <= 0 {
		limit = IndexBatch
	}
	// A page is indexed only when it is PUBLISHED: a draft is somebody's
	// unfinished thought and a trashed page is deleted as far as a reader
	// is concerned, and surfacing either in a knowledge search would put
	// content in front of an agent that no person considers current.
	rows, err := x.db.SQL().QueryContext(ctx, `
		SELECT 'page', p.id, p.container, p.title, p.body, p.version
		  FROM pages p
		  LEFT JOIN kb_docs d ON d.source = 'page' AND d.source_id = p.id
		 WHERE p.status = 'published'
		   AND (d.id IS NULL OR d.source_rev <> p.version)
		 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("projection: find stale index rows: %w", err)
	}
	defer rows.Close()
	var out []Doc
	for rows.Next() {
		var doc Doc
		var version int64
		if err := rows.Scan(&doc.Source, &doc.ID, &doc.Container,
			&doc.Title, &doc.Body, &version); err != nil {
			return nil, fmt.Errorf("projection: scan stale index row: %w", err)
		}
		doc.Version = uint64(version)
		out = append(out, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projection: find stale index rows: %w", err)
	}
	return out, nil
}

// Orphans returns index rows whose source is gone, so the indexer can drop
// them.
//
// Its own pass rather than a foreign key, for the reason [Indexer.Remove]
// gives: a cascade from the source table would delete a posting list while
// the indexer is writing it.
func (x *Indexer) Orphans(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 {
		limit = IndexBatch
	}
	rows, err := x.db.SQL().QueryContext(ctx, `
		SELECT d.source_id FROM kb_docs d
		 WHERE d.source = 'page'
		   AND NOT EXISTS (
		       SELECT 1 FROM pages p WHERE p.id = d.source_id AND p.status = 'published')
		 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("projection: find orphan index rows: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("projection: scan orphan index row: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Pending is how many documents are waiting to be indexed.
func (x *Indexer) Pending(ctx context.Context) (int, error) {
	var n int
	err := x.db.SQL().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pages p
		  LEFT JOIN kb_docs d ON d.source = 'page' AND d.source_id = p.id
		 WHERE p.status = 'published'
		   AND (d.id IS NULL OR d.source_rev <> p.version)`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("projection: count pending index rows: %w", err)
	}
	return n, nil
}

// Ready reports whether the index has caught up with the projection.
//
// THE SEARCH GATE, and it exists because "no results" and "not indexed yet"
// are different answers a person acts on differently. A seat on a freshly
// joined node would otherwise be told the company has written nothing down,
// for the five minutes the first index build takes — so the knowledge block
// renders "index building" instead, and the searcher declines rather than
// answering empty.
func (x *Indexer) Ready(ctx context.Context) (bool, error) {
	pending, err := x.Pending(ctx)
	if err != nil {
		return false, err
	}
	return pending == 0, nil
}

// Run indexes in batches until the context ends.
//
// It sleeps only when it finds nothing, so a catch-up runs flat out and a
// steady state costs one indexed count per idle tick.
func (x *Indexer) Run(ctx context.Context) {
	for {
		worked, err := x.step(ctx)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil:
			log.WarnContext(ctx, "projection_index_step_failed",
				"error", err.Error(),
				"detail", "the lexical index is behind the projection; search "+
					"reports itself as building until it catches up")
		case worked:
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(indexIdle):
		}
	}
}

// indexIdle is how long the indexer waits when it found nothing to do.
//
// Two seconds. The index is behind the projection by at most this plus one
// batch, which is the latency between saving a page and finding it in search
// — short enough that a person who saves and immediately searches finds their
// own page, long enough that an idle node runs one cheap count every two
// seconds rather than spinning.
const indexIdle = 2 * time.Second

// step does one unit of index work, reporting whether it found any.
func (x *Indexer) step(ctx context.Context) (bool, error) {
	orphans, err := x.Orphans(ctx, IndexBatch)
	if err != nil {
		return false, err
	}
	for _, id := range orphans {
		if err := x.Remove(ctx, "page", id); err != nil {
			return false, err
		}
	}
	stale, err := x.Stale(ctx, IndexBatch)
	if err != nil {
		return len(orphans) > 0, err
	}
	if err := x.Upsert(ctx, stale); err != nil {
		return len(orphans) > 0, err
	}
	return len(orphans) > 0 || len(stale) > 0, nil
}
