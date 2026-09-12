package search

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/textindex"
)

var log = logging.Get("search")

// Doc is one document offered to the index — a page or a work item.
//
// The SOURCE ROWS ARE NOT INDEXED IN PLACE, and that separation is the point:
// the index is a different lifecycle from the rows it covers. They land with
// their record's own commit; the index is built asynchronously behind them,
// can be dropped and rebuilt wholesale when the analyzer changes, and is what
// a search reads so that a query is one scan over postings rather than a UNION
// over two schemas.
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

// Indexer maintains the lexical index behind this node's own applied rows.
//
// SEPARATE FROM THE APPLIER, and it must be: an apply carries the checkpoint
// in its own transaction and has to be fast, while indexing a page is
// tokenising tens of kilobytes and writing hundreds of posting rows. Doing it
// inline would put the whole index build inside the apply transaction — which
// holds this store's only writer — and a node catching up on a large company
// would stop applying records entirely while it worked.
//
// It also lives in a DIFFERENT ESTATE from what it indexes: the sources are
// replicated rows derived from a log, and the index is this node's own. No
// read joins the two, which is why every comparison here is a batch from each
// side rather than a JOIN.
type Indexer struct {
	db *store.DB

	// cursor and orphanCursor are where the two reconciliation walks are.
	//
	// IN MEMORY rather than in a table, and that is deliberate: losing
	// them costs one extra cycle over rows that are already correct, which
	// is exactly what the walk is for. A durable cursor would be a second
	// piece of state to keep in step with an index that is itself
	// rebuildable.
	//
	// UNGUARDED because [Indexer.Run] is ONE loop, and every walk runs
	// inside it. A second caller would be a second indexer over one node's
	// tables, which is a race about the postings long before it is a race
	// about these.
	cursor       map[string]string
	orphanCursor map[string]string

	// sources is what this index covers. See [LexicalSource] for why the
	// SQL is theirs and the walk is this type's.
	sources []LexicalSource
}

// NewIndexer builds an indexer over a node's store, covering every corpus in
// [DefaultLexicalSources].
func NewIndexer(db *store.DB) *Indexer { return NewIndexerOver(db, DefaultLexicalSources()) }

// NewIndexerOver builds one over the sources given, which is what a test that
// is about ONE corpus uses.
func NewIndexerOver(db *store.DB, sources []LexicalSource) *Indexer {
	return &Indexer{
		db: db, sources: sources,
		cursor:       map[string]string{},
		orphanCursor: map[string]string{},
	}
}

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
		INSERT INTO kb_docs (id, source, source_id, search_shard, container,
		                     title, excerpt, length, source_rev, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		-- THE SHARD IS NOT RE-STAMPED, and that is the invariant rather
		-- than an omission: the id column is derived from the same two
		-- source columns the bucket is, so a row's bucket cannot change
		-- while its key stays the same. Re-stamping it would do something
		-- only if the scheme itself changed -- and then it would
		-- re-bucket exactly the documents that happened to be re-indexed,
		-- leaving the corpus half in each scheme with searches silently
		-- missing whatever is on the other side. A scheme change costs a
		-- full index rebuild; repair-on-touch is not a cheaper version of
		-- one, it is a corpus nobody can reason about.
		ON CONFLICT (id) DO UPDATE SET
			container  = excluded.container,
			title      = excluded.title,
			excerpt    = excluded.excerpt,
			length     = excluded.length,
			source_rev = excluded.source_rev,
			indexed_at = excluded.indexed_at`,
		id, doc.Source, doc.ID, ShardOf(doc.Source, doc.ID), doc.Container,
		doc.Title, excerptOf(doc.Body), length, int64(doc.Version), now); err != nil {
		return fmt.Errorf("search: index %s: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM kb_postings WHERE doc_id = ?`, id); err != nil {
		return fmt.Errorf("search: clear postings for %s: %w", id, err)
	}
	for term, freq := range terms {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kb_postings (term, doc_id, freq) VALUES (?, ?, ?)`,
			term, id, freq); err != nil {
			return fmt.Errorf("search: write posting %q for %s: %w", term, id, err)
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
	// ONE SOURCE PER CALL, in order, and the first with work wins. A pass
	// that read every corpus would make a batch mean "twenty pages AND
	// twenty items", which is two transactions of work behind one
	// [IndexBatch] — and the loop calls this until it finds nothing, so
	// nothing is starved by taking them in turn.
	for _, source := range x.sources {
		batch, err := x.nextBatch(ctx, source, limit)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			// THE WALK WRAPS. It is a cursor over ids rather than
			// a watermark over versions, so reaching the end is
			// the ordinary case rather than a failure.
			x.cursor[source.Source()] = ""
			continue
		}
		x.cursor[source.Source()] = batch[len(batch)-1].ID

		indexed, err := x.versions(ctx, batch)
		if err != nil {
			return nil, err
		}
		out := make([]Doc, 0, len(batch))
		for _, doc := range batch {
			if held, ok := indexed[doc.ID]; ok && held == doc.Version {
				continue
			}
			out = append(out, doc)
		}
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, nil
}

// sources reads the next batch of indexable documents from the REPLICATED
// estate.
//
// # Why this is a walk rather than a join
//
// The source rows and the index rows are in DIFFERENT ESTATES — a page is
// replicated state derived from a log, and the index is this node's own
// derived copy — and no read joins across the two. So the comparison the old
// LEFT JOIN made in SQL is made here in Go, over a batch bounded by an id
// cursor.
//
// What that costs is a full walk per cycle even on a quiet corpus: at
// [IndexBatch] a ten-thousand-page company re-reads its own ids every five
// hundred steps. What it buys is a repair the version compare never had — an
// index row that drifted for any reason at all is rebuilt on the next pass,
// where a watermark over versions would only ever notice a source that moved.
func (x *Indexer) nextBatch(ctx context.Context, source LexicalSource,
	limit int) ([]Doc, error) {

	var out []Doc
	// THROUGH THE HANDLE, NOT ITS POOL. `DB.SQL()` answers a NIL pool on a
	// replicated estate that is not open — which is a legitimate,
	// documented state of that peer, not a fault — and a statement issued
	// on it panics inside database/sql. [store.DB.Read] answers
	// [store.ErrNoEstate] instead, which every caller here already reads
	// as an empty index pass.
	err := x.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		batch, err := source.Next(ctx, tx, x.cursor[source.Source()], limit)
		out = batch
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("search: read the next documents to index: %w", err)
	}
	return out, nil
}

// versions is what the index already holds for one batch of sources.
func (x *Indexer) versions(ctx context.Context, batch []Doc) (map[string]uint64, error) {
	ids := make([]any, 0, len(batch))
	for _, doc := range batch {
		ids = append(ids, docKey(doc.Source, doc.ID))
	}
	rows, err := x.db.SQL().QueryContext(ctx,
		`SELECT source_id, source_rev FROM kb_docs WHERE id IN (`+
			binds(len(ids))+`)`, ids...)
	if err != nil {
		return nil, fmt.Errorf("search: read what the index holds: %w", err)
	}
	defer rows.Close()
	out := make(map[string]uint64, len(batch))
	for rows.Next() {
		var id string
		var version int64
		if err := rows.Scan(&id, &version); err != nil {
			return nil, fmt.Errorf("search: scan an index version: %w", err)
		}
		out[id] = uint64(version)
	}
	return out, rows.Err()
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
	// ONE SOURCE PER CALL, exactly as [Indexer.Stale] takes them: the
	// orphan check is a batch from each estate, and mixing two sources in
	// one batch would ask each source's existence query about the other's
	// ids. The caller removes what comes back UNDER the source named
	// beside it, which is why both travel together.
	_, gone, err := x.orphanPass(ctx, limit)
	return gone, err
}

// orphanPass is [Indexer.Orphans] with the SOURCE the ids belong to, which is
// what a removal needs and what an id on its own cannot say.
func (x *Indexer) orphanPass(ctx context.Context, limit int) (string, []string, error) {
	if limit <= 0 {
		limit = IndexBatch
	}
	for _, source := range x.sources {
		gone, err := x.orphansOf(ctx, source, limit)
		if err != nil {
			return source.Source(), nil, err
		}
		if len(gone) > 0 {
			return source.Source(), gone, nil
		}
	}
	return "", nil, nil
}

// orphansOf is one source's share of that walk, and it answers the ids to
// drop from the index under that source's own name.
func (x *Indexer) orphansOf(ctx context.Context, source LexicalSource,
	limit int) ([]string, error) {

	// THE SAME ESTATE BOUNDARY as [Indexer.Stale], and the same answer: a
	// batch of index rows read here, and their sources checked against the
	// replicated estate in a second read.
	rows, err := x.db.SQL().QueryContext(ctx,
		`SELECT source_id FROM kb_docs WHERE source = ? AND source_id > ?
		  ORDER BY source_id LIMIT ?`,
		source.Source(), x.orphanCursor[source.Source()], limit)
	if err != nil {
		return nil, fmt.Errorf("search: read the next index rows to check: %w", err)
	}
	var candidates []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("search: scan an index row: %w", err)
		}
		candidates = append(candidates, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("search: read the next index rows to check: %w", err)
	}
	_ = rows.Close()
	if len(candidates) == 0 {
		x.orphanCursor[source.Source()] = ""
		return nil, nil
	}
	x.orphanCursor[source.Source()] = candidates[len(candidates)-1]

	live := map[string]bool{}
	// THROUGH THE HANDLE, for [Indexer.nextBatch]' reason: a nil pool from
	// a closed replicated estate panics where the handle answers
	// [store.ErrNoEstate].
	if err := x.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		held, err := source.Live(ctx, tx, candidates)
		live = held
		return err
	}); err != nil {
		return nil, fmt.Errorf("search: check which indexed %s documents "+
			"still exist: %w", source.Source(), err)
	}
	var out []string
	for _, id := range candidates {
		if !live[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// Pending is how many documents are waiting to be indexed.
// TWO COUNTS AND A SUBTRACTION, because the sources and the index are in
// different estates and no read joins them. It answers how many published
// pages are NOT represented in the index — which is what the gate below needs
// — and deliberately not how many are STALE: a page whose body moved is
// already searchable, just by its previous text, where a page with no row at
// all is a page a search reports as not existing.
func (x *Indexer) Pending(ctx context.Context) (int, error) {
	var published, indexed int
	// SUMMED ACROSS SOURCES, because the gate is about the whole index: a
	// seat whose company has pages indexed and items not is one that would
	// be told its own tracker holds nothing.
	// THROUGH THE HANDLE, for [Indexer.nextBatch]' reason.
	if err := x.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		for _, source := range x.sources {
			n, err := source.Count(ctx, tx)
			if err != nil {
				return err
			}
			published += n
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("search: count the documents %s offers: %w",
			sourceNames(x.sources), err)
	}
	for _, source := range x.sources {
		var n int
		if err := x.db.SQL().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM kb_docs WHERE source = ?`,
			source.Source()).Scan(&n); err != nil {
			return 0, fmt.Errorf("search: count the indexed %s documents: %w",
				source.Source(), err)
		}
		indexed += n
	}
	// NEVER NEGATIVE. The index can legitimately hold rows the sources no
	// longer have — a page trashed between the two reads, an orphan the
	// next sweep removes — and a negative "pending" would read as a gate
	// that is more than ready.
	return max(published-indexed, 0), nil
}

// Ready reports whether the index has caught up with this node's own rows.
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
		worked, err := x.Sweep(ctx)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil:
			log.WarnContext(ctx, "lexical_index_step_failed",
				"error", err.Error(),
				"detail", "the lexical index is behind this node's own rows; "+
					"search reports itself as building until it catches up")
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
// Two seconds. The index is behind this node's own rows by at most this plus
// one batch, which is the latency between saving a page and finding it in
// search
// — short enough that a person who saves and immediately searches finds their
// own page, long enough that an idle node runs one cheap count every two
// seconds rather than spinning.
const indexIdle = 2 * time.Second

// Sweep does ONE unit of index work, reporting whether it found any.
//
// EXPORTED because two callers need exactly this and neither should reach past
// it: [Indexer.Run] is the loop, and a test drives the index to a fixed point
// by calling this until it stops finding work. The alternative — a test that
// waited on [Indexer.Ready] — waits on the FIRST-BUILD gate, which counts rows
// the index is missing and is deliberately blind to a row that is merely
// stale.
func (x *Indexer) Sweep(ctx context.Context) (bool, error) {
	// THE ORPHAN PASS NAMES ITS SOURCE, because the index is one table over
	// every corpus and a page and a work item can share an id-shaped
	// string: removing by id alone would delete whichever row sorted first.
	source, orphans, err := x.orphanPass(ctx, IndexBatch)
	if err != nil {
		return false, err
	}
	for _, id := range orphans {
		if err := x.Remove(ctx, source, id); err != nil {
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
