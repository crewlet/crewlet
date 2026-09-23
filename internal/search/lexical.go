package search

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
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
// therefore takes about five minutes to build, which is why [Indexer.ReadyFor]
// exists and reports false meanwhile.
const IndexBatch = 20

// ScanBatch is how many ids and versions one lap reads at a time.
//
// FIFTY TIMES [IndexBatch], because it is a different kind of work: the scan
// reads two indexed columns and decides whether anything moved, where a batch
// reads bodies and tokenises them. Sized so a lap over a corpus of any
// realistic size is a handful of statements rather than a walk paced at the
// tokeniser's speed — which is what made a saved page take a full lap to
// become findable.
//
// It is also what bounds the `IN` list the index-side version lookup binds,
// and a thousand is comfortably inside every engine's parameter limit.
const ScanBatch = 1_000

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

	// built is which sources have completed at least one lap since this
	// process started, which is what [Indexer.ReadyFor] answers.
	//
	// IN MEMORY, beside the cursors and for the same reason: it is a fact
	// about THIS process's walk rather than about the index, and a
	// restart honestly re-establishes it on its first lap.
	//
	// ATOMIC, unlike the cursors, and that is the one difference that
	// matters: the cursors are read and written only inside [Indexer.Run],
	// which is one loop, while this is READ BY EVERY SEARCH — the gate is
	// asked on every empty answer, from whichever goroutine is serving a
	// turn. The map itself is built once, at construction, so only the
	// flags move.
	built map[string]*atomic.Bool

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
	built := make(map[string]*atomic.Bool, len(sources))
	for _, source := range sources {
		built[source.Source()] = &atomic.Bool{}
	}
	return &Indexer{
		db: db, sources: sources,
		built:        built,
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
	// THE PARAMETER LIMIT IS READ ONCE PER BATCH, not once per posting: it
	// is a property of the estate this indexer writes to, established when
	// the store opened, and the posting write below is the hottest loop in
	// the tree. Threading it in also keeps [Indexer.upsertOne] free of the
	// handle, so what it writes depends only on its arguments.
	maxVariables := x.db.Caps().MaxVariables
	return x.db.Tx(ctx, func(tx *sql.Tx) error {
		for _, doc := range docs {
			if err := x.upsertOne(ctx, tx, doc, now, maxVariables); err != nil {
				return err
			}
		}
		return nil
	})
}

func (x *Indexer) upsertOne(ctx context.Context, tx *sql.Tx, doc Doc, now int64,
	maxVariables int) error {

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

	// THE POSTINGS ARE COLLECTED INTO A SLICE rather than written straight
	// out of the map, because the multi-row insert below binds them by
	// index and a map has no indexes. The ORDER within that slice is
	// whatever the map yielded and is deliberately not sorted: the rows are
	// a set, the key is (term, doc_id) and the terms of one document are
	// unique by construction, so no two rows of one statement can collide
	// and there is no conflict clause whose last-write-wins would make the
	// order observable. See the insert for what that would cost if a
	// conflict clause were ever added.
	postings := make([]posting, 0, len(terms))
	length := 0
	for term, n := range terms {
		postings = append(postings, posting{term: term, freq: n})
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
	// ONE STATEMENT PER CHUNK, not one per term. This was the hottest
	// per-row loop in the tree: the inverted list takes a row per UNIQUE
	// TERM per document, so a 600-word page was several hundred round trips
	// and several hundred plans inside the index transaction, which is the
	// shape BenchmarkLogApplyDrain names "unprepared" and measures as the
	// slowest of the three.
	//
	// NO CONFLICT CLAUSE, exactly as before: the DELETE above cleared this
	// document's list, and (term, doc_id) is unique within it because
	// `terms` is keyed on the term. A duplicate here would be the index
	// contradicting itself rather than something to absorb, and the primary
	// key says so.
	if _, err := store.InsertRows(ctx, tx, maxVariables,
		`INSERT INTO kb_postings (term, doc_id, freq) VALUES`, `(?, ?, ?)`, "",
		len(postings), func(i int) []any {
			return []any{postings[i].term, id, postings[i].freq}
		}); err != nil {
		// THE DOCUMENT RATHER THAN THE TERM. A chunk carries hundreds of
		// terms and the engine names the offending one in its own error,
		// which %w carries; naming the first term of the chunk here would
		// be a guess that reads as a fact.
		return fmt.Errorf("search: write %d postings for %s: %w",
			len(postings), id, err)
	}
	return nil
}

// posting is one row of the inverted list, held while a document's terms are
// turned from a map into something a multi-row insert can index into.
type posting struct {
	term string
	freq int
}

// excerptLimit bounds the stored excerpt, in bytes.
//
// Six hundred: enough for a snippet window wherever the query terms fall in
// the opening, and small enough that the index does not become a second copy
// of every body.
//
// IT IS THE FALLBACK RATHER THAN THE SOURCE. A snippet is cut from the
// document's real body, read per query for the top hits alone — see
// [Indexer.resnippet] — because a window over the opening cannot centre on a
// match that is not in the opening, and a snippet without the search term in
// it reads as a wrong result. What this value covers is the case where that
// read gives nothing: a source that no longer holds the row, an estate that
// would not answer, or a body that is now empty. Three times
// [knowledge.SnippetLimit], so the fallback still has a window to choose
// within.
const excerptLimit = 600

// excerptOf keeps the opening of a body for the fallback snippet, MARKED where
// it was cut.
//
// Whitespace is collapsed first, so a markdown body's blank lines do not
// spend the budget, and the cut goes through [textcut.Within] — a plain slice
// splits a multi-byte rune, and the invalid UTF-8 that produces is
// substituted by the JSON encoder and read by a model as a replacement
// character.
//
// THE MARKER IS STORED IN THE VALUE, because the snippet cannot supply it.
// [textindex.Snippet] marks an edge it cut itself and leaves unmarked an edge
// that reached the end of the text it was handed — and handed this, that is
// the end of the excerpt rather than of the document. Unmarked, a match near
// the excerpt's end would give a snippet stopping mid-word on text the page
// goes on past, with nothing to say so. Marked here, that snippet ends in this
// marker; a window that stops earlier cuts the marker away with the rest of
// the tail and adds its own.
//
// [textcut.Within] rather than [textcut.Ellipsis], so the stored value stays
// inside [excerptLimit] marker and all.
//
// NOT THE ONLY COPY. The whole body is the source row's own —
// `pages_heads.body` for a page, the body inside `tracker_tasks.document` for a
// work item — which a hit's source and id address, and which
// [Indexer.resnippet] reads through [LexicalSource.Fetch] whenever it can.
func excerptOf(body string) string {
	return textcut.Within(strings.Join(strings.Fields(body), " "), excerptLimit)
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
	// ONE SOURCE PER CALL, in order, and the first with work wins. A pass
	// that read every corpus would make a batch mean "twenty pages AND
	// twenty items", which is two transactions of work behind one
	// [IndexBatch] — and the loop calls this until it finds nothing, so
	// nothing is starved by taking them in turn.
	for _, source := range x.sources {
		docs, err := x.staleIn(ctx, source, limit)
		if err != nil {
			return nil, err
		}
		if len(docs) > 0 {
			return docs, nil
		}
	}
	return nil, nil
}

// staleIn laps one source until it finds documents to re-index, or wraps.
//
// # The lap is the unit, not the batch
//
// It SCANS at [ScanBatch] and only reads bodies for what actually moved, so a
// pass over an unchanged corpus is a handful of two-column statements rather
// than a walk paced at the tokeniser's speed. That is the whole difference
// between "a saved page is findable after an idle tick" and "after a full lap
// of the corpus", which at ten thousand documents was about seventeen minutes.
//
// Returning inside the lap is what keeps the write side bounded: the scan is
// cheap and unbounded in reach, the fetch is [IndexBatch] documents, and the
// caller's loop comes straight back for the next one.
func (x *Indexer) staleIn(ctx context.Context, source LexicalSource,
	limit int) ([]Doc, error) {

	name := source.Source()
	for {
		var moved []string
		var wrapped bool
		// THE CURSOR IS STAGED, NEVER ADVANCED INSIDE THE TRANSACTION.
		// It is in-memory state and the transaction below can fail after
		// it has been set — the index read and the body fetch are both
		// store reads — and an in-memory write is not rolled back with
		// the transaction that produced it. Advanced in place, one
		// transient read failure SKIPPED every document in that scan
		// window until the walk wrapped, which is exactly the staleness
		// this walk's whole design is measured against: at ten thousand
		// documents a lap was about seventeen minutes. Staged here and
		// committed only on success, a failed step is retried over the
		// same window on the next call.
		next, advance := x.cursor[name], false
		// ONE TRANSACTION PER SCAN STEP, holding the scan and the fetch
		// together: the walk's correctness argument is that a batch is
		// compared against one snapshot of the other side, and reading
		// the bodies in a second transaction would fetch a version the
		// scan never saw.
		//
		// THROUGH THE HANDLE, NOT ITS POOL. `DB.SQL()` answers a NIL
		// pool on a replicated estate that is not open — a legitimate,
		// documented state of that peer — and a statement issued on it
		// panics inside database/sql. [store.DB.Read] answers
		// [store.ErrNoEstate] instead, which every caller here already
		// reads as an empty index pass.
		var out []Doc
		if err := x.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
			scan, err := source.Versions(ctx, tx, x.cursor[name], ScanBatch)
			if err != nil {
				return err
			}
			if len(scan) == 0 {
				// THE WALK WRAPS. It is a cursor over ids rather
				// than a watermark over versions, so reaching the
				// end is the ordinary case rather than a failure
				// — and it is the moment this source is BUILT.
				wrapped = true
				return nil
			}
			// A SCAN THAT DID NOT ADVANCE IS A SOURCE BREAKING ITS
			// CONTRACT, and it has to be an error rather than another
			// turn of this loop. [LexicalSource.Versions] promises ids
			// AFTER the cursor, ordered by id; a source that ignores
			// the cursor — or whose ordering disagrees with the
			// comparison the cursor is carried through, which is a
			// collation question rather than a hypothetical — hands
			// back the same batch for ever. The loop's three exits are
			// "it wrapped", "it found work" and "the context ended",
			// and none of them is reachable from there: the indexer
			// spins on one batch, never indexes again, never marks the
			// source built, and the only symptom is search that is
			// permanently scoped with nothing in the log to say why.
			if last := scan[len(scan)-1].ID; last <= x.cursor[name] {
				return fmt.Errorf("search: source %s answered a scan after %q "+
					"with a batch ending at %q, which is not after it — the "+
					"source's Versions must return ids strictly after the "+
					"cursor, ordered by id, or this walk cannot terminate",
					name, x.cursor[name], last)
			}
			next, advance = scan[len(scan)-1].ID, true
			indexed, err := x.versions(ctx, name, scan)
			if err != nil {
				return err
			}
			for _, at := range scan {
				if held, ok := indexed[at.ID]; ok && held == at.Version {
					continue
				}
				moved = append(moved, at.ID)
				if len(moved) == limit {
					// THE CURSOR STOPS WHERE THE BATCH DOES,
					// so the ids past it are read again on
					// the next call rather than skipped:
					// this scan reached further than one
					// batch of bodies, and everything
					// after this id is still to do.
					next = at.ID
					break
				}
			}
			if len(moved) == 0 {
				return nil
			}
			out, err = source.Fetch(ctx, tx, moved)
			return err
		}); err != nil {
			return nil, fmt.Errorf("search: read the next %s documents to "+
				"index: %w", name, err)
		}
		if advance {
			x.cursor[name] = next
		}
		if wrapped {
			x.cursor[name] = ""
			if flag := x.built[name]; flag != nil {
				flag.Store(true)
			}
			return nil, nil
		}
		if len(out) > 0 {
			return out, nil
		}
		if err := ctx.Err(); err != nil {
			// A CANCELLED LAP ESTABLISHED NOTHING, so it is reported
			// as the failure it is rather than as an empty pass. The
			// two honest exits from this loop are "it wrapped" and
			// "it found work"; answering (nil, nil) for a third makes
			// a stop look identical to a source with nothing stale in
			// it, which is precisely the licence [Indexer.Sweep] hands
			// its caller to stop asking.
			return nil, err
		}
	}
}

// versions is what the index already holds for one scan batch.
func (x *Indexer) versions(ctx context.Context, source string,
	scan []DocVersion) (map[string]uint64, error) {

	ids := make([]any, 0, len(scan))
	for _, at := range scan {
		ids = append(ids, docKey(source, at.ID))
	}
	rows, err := x.db.SQL().QueryContext(ctx,
		`SELECT source_id, source_rev FROM kb_docs WHERE id IN (`+
			binds(len(ids))+`)`, ids...)
	if err != nil {
		return nil, fmt.Errorf("search: read what the index holds: %w", err)
	}
	defer rows.Close()
	out := make(map[string]uint64, len(scan))
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

// orphanPass reads index rows whose source is gone, WITH the source they
// belong to, so the caller can drop them under the right name.
//
// Its own pass rather than a foreign key, for the reason [Indexer.Remove]
// gives: a cascade from the source table would delete a posting list while the
// indexer is writing it.
//
// IT CARRIES THE SOURCE because the index is ONE table over every corpus and a
// page and a work item can share an id-shaped string — removing by id alone
// would delete whichever row sorted first. An exported form that answered bare
// ids stood here after the walk became source-plural, with no caller and
// exactly the signature the new code could not use safely.
//
// LAP says whether each source's walk runs until it finds orphans or wraps, or
// takes one step; see [Indexer.Sweep] for which it is asked for, and when.
func (x *Indexer) orphanPass(ctx context.Context, limit int, lap bool) (string, []string, error) {
	if limit <= 0 {
		limit = IndexBatch
	}
	for _, source := range x.sources {
		gone, err := x.orphansOf(ctx, source, limit, lap)
		if err != nil {
			return source.Source(), nil, err
		}
		if len(gone) > 0 {
			return source.Source(), gone, nil
		}
	}
	return "", nil, nil
}

// orphansOf is one source's share of that walk, and it answers up to limit ids
// to drop from the index under that source's own name.
//
// # The lap is the unit, as it is for [Indexer.staleIn]
//
// A step that found nothing is not a lap that found nothing, and [Indexer.Sweep]
// hands its caller a licence to sleep only on the second. So a step reads
// [ScanBatch] index rows — ids alone, then one existence check on the source
// for all of them — and with lap set the walk keeps stepping until it has
// orphans to report or reaches the end. A walk paced at [IndexBatch] rows per
// [indexIdle] tick instead would leave a page somebody trashed findable by
// keyword until it came round — over half an hour on an index of twenty
// thousand documents.
//
// THE CURSOR IS STAGED, NEVER ADVANCED BEFORE THE CHECK SUCCEEDS, for
// [Indexer.staleIn]'s reason: it is in-memory state, and a check that failed
// after the cursor moved would skip every row in that window until the walk
// next wrapped. And it STOPS WHERE THE ANSWER DOES, so the rows after the
// limit-th orphan are checked again on the next call rather than passed over.
func (x *Indexer) orphansOf(ctx context.Context, source LexicalSource,
	limit int, lap bool) ([]string, error) {

	name := source.Source()
	for {
		// THE SAME ESTATE BOUNDARY as [Indexer.Stale], and the same
		// answer: a batch of index rows read here, and their sources
		// checked against the replicated estate in a second read.
		candidates, err := x.indexedAfter(ctx, name, x.orphanCursor[name])
		if err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			x.orphanCursor[name] = ""
			return nil, nil
		}

		var live map[string]bool
		// THROUGH THE HANDLE, for [Indexer.staleIn]'s reason: a nil pool
		// from a closed replicated estate panics where the handle
		// answers [store.ErrNoEstate].
		if err := x.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
			var err error
			live, err = source.Live(ctx, tx, candidates)
			return err
		}); err != nil {
			return nil, fmt.Errorf("search: check which indexed %s documents "+
				"still exist: %w", name, err)
		}
		next := candidates[len(candidates)-1]
		var out []string
		for _, id := range candidates {
			if live[id] {
				continue
			}
			out = append(out, id)
			if len(out) == limit {
				next = id
				break
			}
		}
		x.orphanCursor[name] = next
		if len(out) > 0 || !lap {
			return out, nil
		}
		if err := ctx.Err(); err != nil {
			// A CANCELLED LAP ESTABLISHED NOTHING, on [Indexer.staleIn]'s
			// terms: answering (nil, nil) would read to [Indexer.Sweep]
			// as a lap that found nothing.
			return nil, err
		}
	}
}

// indexedAfter is the next [ScanBatch] ids this node's index holds for one
// source, after a cursor and in id order.
func (x *Indexer) indexedAfter(ctx context.Context, source, after string) ([]string, error) {
	rows, err := x.db.SQL().QueryContext(ctx,
		`SELECT source_id FROM kb_docs WHERE source = ? AND source_id > ?
		  ORDER BY source_id LIMIT ?`, source, after, ScanBatch)
	if err != nil {
		return nil, fmt.Errorf("search: read the next index rows to check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("search: scan an index row: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: read the next index rows to check: %w", err)
	}
	return out, nil
}

// Pending is how many documents the sources offer that the index holds no row
// for. No names means every source this index covers, and a name it does not
// cover counts nothing.
//
// A REPORTING NUMBER rather than the gate — see [Indexer.ReadyFor] for why the
// gate is the first lap instead. [Indexer.Run] logs it beside each corpus's
// first lap.
//
// TWO COUNTS AND A SUBTRACTION PER SOURCE, because the sources and the index
// are in different estates and no read joins them. It counts documents with no
// index row at all, and deliberately not STALE ones: a page whose body moved
// is already searchable, just by its previous text, where a page with no row
// is one a search reports as not existing.
//
// IT CAN ONLY UNDERCOUNT. The index can legitimately hold rows its source no
// longer offers — a page trashed between the two reads, an orphan the next
// sweep removes — and each one offsets a missing document of the same source.
// So the subtraction is clamped PER SOURCE: summed first, one corpus's orphans
// would hide another corpus's missing documents, and a negative total would
// read as an index that is more than caught up.
func (x *Indexer) Pending(ctx context.Context, sources ...string) (int, error) {
	covered := x.covering(sources)
	offered := make([]int, len(covered))
	// THROUGH THE HANDLE, for [Indexer.staleIn]'s reason.
	if err := x.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		for i, source := range covered {
			n, err := source.Count(ctx, tx)
			if err != nil {
				return fmt.Errorf("count the documents %s offers: %w",
					source.Source(), err)
			}
			offered[i] = n
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("search: %w", err)
	}
	pending := 0
	for i, source := range covered {
		var indexed int
		if err := x.db.SQL().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM kb_docs WHERE source = ?`,
			source.Source()).Scan(&indexed); err != nil {
			return 0, fmt.Errorf("search: count the indexed %s documents: %w",
				source.Source(), err)
		}
		pending += max(offered[i]-indexed, 0)
	}
	return pending, nil
}

// ReadyFor reports whether this node's FIRST INDEX BUILD has finished for the
// sources a query asks for. No names means every source this index covers.
//
// THE SEARCH GATE, and it exists because "no results" and "not indexed yet"
// are different answers a person acts on differently. A seat on a freshly
// joined node would otherwise be told the company has written nothing down
// for as long as the first build takes — so a search over a corpus that has
// not finished its first lap reports itself as building instead: the scan
// counts its range missing ([NodeScanner.Scan]), and the knowledge and work
// searches say "still building" where they would have said "nothing matched".
//
// # Why it is the first LAP and not "nothing is pending"
//
// Because "nothing is pending" is a state a company with people in it is
// almost never in. Read from [Indexer.Pending], a single page saved a moment
// ago would make it false, and every empty search on the node would answer
// "the index is still building, try again" rather than "nothing matched" — on
// a company writing continuously, for ever. After the first lap an index a few
// documents behind is ordinary staleness: a search over it is a true answer
// about slightly older rows, which is strictly better than refusing, and a
// document not indexed yet is exactly the one the caller would not have found
// anyway.
//
// # Why per source
//
// The corpora build independently, and a query that names one of them must not
// be held back by the other's lap. A name this index does not cover is ignored
// rather than refused: the source filter crosses the broker, and a peer running
// a build that knows a corpus this one does not must narrow to what it can
// answer — the reasoning [sourcesOf] states for the same value.
//
// NO I/O: every scan asks it, so it reads the flags this node's own walk sets.
func (x *Indexer) ReadyFor(sources ...string) bool {
	if x == nil {
		return false
	}
	for _, source := range x.covering(sources) {
		flag := x.built[source.Source()]
		if flag == nil || !flag.Load() {
			return false
		}
	}
	return true
}

// covering is the sources a filter names, in this index's order, or every
// source for an empty filter. A name this index does not cover selects nothing
// — see [Indexer.ReadyFor] for why that is not an error.
func (x *Indexer) covering(names []string) []LexicalSource {
	if len(names) == 0 {
		return x.sources
	}
	var out []LexicalSource
	for _, source := range x.sources {
		if slices.Contains(names, source.Source()) {
			out = append(out, source)
		}
	}
	return out
}

// Run indexes in batches until the context ends.
//
// It sleeps only when it finds nothing, so a catch-up runs flat out and a
// steady state costs one indexed count per idle tick.
func (x *Indexer) Run(ctx context.Context) {
	announced := make(map[string]bool, len(x.sources))
	for {
		worked, err := x.Sweep(ctx)
		if ctx.Err() != nil {
			return
		}
		x.announceBuilt(ctx, announced)
		switch {
		case err != nil:
			log.WarnContext(ctx, "lexical_index_step_failed",
				"error", err.Error(),
				"detail", "the lexical index is behind this node's own rows; "+
					"a search over a corpus reports itself as building until "+
					"that corpus's first lap finishes, and answers over "+
					"slightly older rows after that")
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

// announceBuilt logs each corpus's first lap, once, as it finishes.
//
// PER CORPUS, because that is the moment a search over it stops saying "still
// building": a knowledge search waits on the pages' lap and a work search on
// the work items', each alone ([Indexer.ReadyFor]) — so one line for the whole
// index would name a moment neither search observes. Without the line, that
// moment is a state an operator can only observe by asking a seat and reading
// its answer.
//
// The corpus's pending count rides it: zero is the healthy reading, and
// anything else names how far behind that corpus started serving. A count that
// could not be read says so rather than reporting zero, which is the healthy
// reading and would be a claim nobody made.
func (x *Indexer) announceBuilt(ctx context.Context, announced map[string]bool) {
	for _, name := range x.newlyBuilt(announced) {
		attrs := []any{"source", name}
		if pending, err := x.Pending(ctx, name); err != nil {
			attrs = append(attrs, "pending_error", err.Error())
		} else {
			attrs = append(attrs, "pending", pending)
		}
		log.InfoContext(ctx, "lexical_index_built", attrs...)
	}
}

// newlyBuilt is each source whose first lap has finished and that announced
// does not name yet, in this index's order — and it marks them, so each
// source is named once.
func (x *Indexer) newlyBuilt(announced map[string]bool) []string {
	var out []string
	for _, source := range x.sources {
		name := source.Source()
		if announced[name] || !x.ReadyFor(name) {
			continue
		}
		announced[name] = true
		out = append(out, name)
	}
	return out
}

// indexIdle is how long the indexer waits when it found nothing to do.
//
// Two seconds, and "nothing to do" now means a whole LAP found nothing — so
// the index is behind this node's own rows by at most this plus one lap,
// which is a handful of two-column scans plus whatever actually moved.
//
// That was not true while the lap was paced at [IndexBatch] per idle tick: a
// ten-thousand-document company took about seventeen minutes to come round,
// so a page saved just behind the cursor was unfindable for that long and
// every empty search on the node reported itself as still building. The
// constant did not change; what changed is that a lap no longer reads bodies
// to establish that nothing moved.
const indexIdle = 2 * time.Second

// Sweep does ONE unit of index work, reporting whether it found any.
//
// A `false` means a whole LAP over every source found nothing, IN BOTH
// DIRECTIONS — nothing to index and nothing to drop — so it is the caller's
// licence to sleep rather than merely the end of a batch. The stale walk laps
// inside [Indexer.staleIn]; the orphan walk laps inside [Indexer.orphansOf],
// and only when the stale walk came back empty.
//
// # Why the orphan walk laps only on a quiet sweep
//
// Because that is the only sweep whose `false` it has to make true. A sweep
// that indexed something reports work whatever the orphans say, and its
// caller comes straight back — so there the orphan walk takes ONE step, and a
// cold build pays one index read and one existence check per batch rather
// than a lap of an index that is growing under it. Only a sweep about to
// report nothing has to have looked at every row, and that one laps.
//
// EXPORTED because two callers need exactly this and neither should reach past
// it: [Indexer.Run] is the loop, and a test drives the index to a fixed point
// by calling this until it stops finding work. The alternative — a test that
// waited on [Indexer.ReadyFor] — waits on the FIRST-BUILD gate, which closes
// once per process and is deliberately blind to a row that is merely stale.
func (x *Indexer) Sweep(ctx context.Context) (bool, error) {
	stale, err := x.Stale(ctx, IndexBatch)
	if err != nil {
		return false, err
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := x.Upsert(ctx, stale); err != nil {
		return false, err
	}
	// THE ORPHAN PASS NAMES ITS SOURCE, because the index is one table over
	// every corpus and a page and a work item can share an id-shaped
	// string: removing by id alone would delete whichever row sorted first.
	source, orphans, err := x.orphanPass(ctx, IndexBatch, len(stale) == 0)
	if err != nil {
		return len(stale) > 0, err
	}
	for i, id := range orphans {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := x.Remove(ctx, source, id); err != nil {
			return len(stale) > 0 || i > 0, err
		}
	}
	return len(stale) > 0 || len(orphans) > 0, nil
}
