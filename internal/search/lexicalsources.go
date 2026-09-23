package search

import (
	"context"
	"database/sql"
)

// WHAT THE LEXICAL INDEX COVERS, as a seam rather than as SQL in the walk.
//
// # Why this exists at all
//
// The index was written for pages and said so three times in three places:
// the walk selected from `pages_heads`, the orphan sweep read `kb_docs` with
// `source = 'page'` spelled into the predicate, and the readiness gate counted
// published pages. Every one of them was correct for pages and silently wrong
// for everything else — and there WAS something else, because the embedding
// duty has carried [TaskCorpus] the whole time. A company paid for a vector on
// every work item, stored it in the replicated estate, replicated it,
// snapshotted it and backed it up, while the lexical half held no task row at
// all and the one ranked reader asked for pages only.
//
// So the three spellings become one list. A source owns its own SQL and
// nothing else; the walk, the cursors, the batching, the estate boundary and
// the posting writes stay where they were.
//
// # Four queries, because the walk asks four questions
//
// The sources and the index are in DIFFERENT ESTATES and no read joins them
// (see [Indexer]), so every comparison is a batch from each side. That makes
// the questions: which ids exist and at what version, what are the bodies of
// these particular ones, which of these indexed ids still exist, and how many
// documents are there.
//
// # Why the scan and the fetch are separate queries
//
// Because they cost three orders of magnitude apart, and a walk that asked
// one question paid the expensive price on every step of a corpus that had
// not changed. Reading id and version is an index-only read; reading the BODY
// is tens of kilobytes per row. Fused, a quiet ten-thousand-document company
// re-read its own bodies to establish that none of them had moved — so the
// walk was paced to twenty documents per idle tick and a page saved just
// behind the cursor waited a full lap, which at that size is about seventeen
// minutes rather than the "idle tick plus one batch" the pacing constant
// claims.
//
// Split, a lap over a quiet corpus is a handful of cheap scans and the bodies
// are read only for the documents that actually moved.
//
// Each takes the replicated estate's own transaction, opened by the indexer:
// a source that opened its own would be a second read at a second instant, and
// the walk's whole correctness argument is that a batch is compared against
// one snapshot of the other side.

// LexicalSource is one corpus the lexical index walks, stated as the four
// questions the walk asks and nothing else.
//
// A source owns its own rows and its own predicates — which of them a reader may
// see, what a version is, what the body is — and owns no part of the walk: the
// cursors, the batching, the estate boundary and the posting writes all stay in
// [Indexer], so adding a corpus is four queries rather than a second indexer.
// [PageSource] and [TaskSource] are the two this build ships
// ([DefaultLexicalSources]); a running node indexes the ones its backends name.
type LexicalSource interface {
	// Source is the value written into `kb_docs.source`, and it is what
	// makes one index table serve corpora whose ids can collide.
	Source() string

	// Versions is the ids and versions after a cursor, ordered by id.
	Versions(ctx context.Context, tx *sql.Tx, after string, limit int) ([]DocVersion, error)

	// Fetch is the full documents for a set of ids, in any order. An id
	// the source no longer has is simply absent.
	Fetch(ctx context.Context, tx *sql.Tx, ids []string) ([]Doc, error)

	// Live is which of these ids this source still has. An id absent from
	// the answer is an orphan the index drops.
	Live(ctx context.Context, tx *sql.Tx, ids []string) (map[string]bool, error)

	// Count is how many documents this source offers, for the reporting
	// number a fleet screen renders.
	Count(ctx context.Context, tx *sql.Tx) (int, error)
}

// DocVersion is one document's identity and version, with no body.
//
// THE SCAN'S OWN SHAPE. It exists so a lap over an unchanged corpus never
// reads a body: what the walk needs to decide "has this moved" is two
// columns, and what it needs to re-index is the whole row — asking one
// question with the other's query is what made freshness a function of
// corpus size.
type DocVersion struct {
	ID      string
	Version uint64
}

// DefaultLexicalSources is every corpus this build can index, which is what an
// indexer covers when nothing narrower is asked for ([NewIndexer]).
//
// NOT WHAT EVERY NODE INDEXES: a running node indexes the corpora its backends
// name, and the engine decides that — a company on Jira has no `tracker_tasks`
// worth walking.
func DefaultLexicalSources() []LexicalSource {
	return []LexicalSource{PageSource{}, TaskSource{}}
}

// PageSource is the knowledge base.
type PageSource struct{}

// Source implements [LexicalSource].
func (PageSource) Source() string { return string(SourcePage) }

// Versions implements [LexicalSource].
//
// PUBLISHED ONLY: a draft is somebody's unfinished thought and a trashed page
// is one somebody put in the trash — still read by its id or its address, and
// out of search — and surfacing either in a knowledge search would put content
// in front of an agent that no person considers current. A page that LEAVES
// the published set is dropped by the orphan pass, which asks
// [PageSource.Live] the same question.
func (PageSource) Versions(ctx context.Context, tx *sql.Tx, after string,
	limit int) ([]DocVersion, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT id, MAX(version, scoped_through)
		  FROM pages_heads
		 WHERE status = 'published' AND id > ?
		 ORDER BY id
		 LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	return scanVersions(rows)
}

// Fetch implements [LexicalSource].
func (PageSource) Fetch(ctx context.Context, tx *sql.Tx, ids []string) ([]Doc, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, container, title, body, MAX(version, scoped_through)
		  FROM pages_heads
		 WHERE status = 'published' AND id IN (`+binds(len(ids))+`)`,
		anyOf(ids)...)
	if err != nil {
		return nil, err
	}
	return scanDocs(rows, string(SourcePage))
}

// Live implements [LexicalSource].
func (PageSource) Live(ctx context.Context, tx *sql.Tx, ids []string) (map[string]bool, error) {
	return liveIDs(ctx, tx, `SELECT id FROM pages_heads
		 WHERE status = 'published' AND id IN (`+binds(len(ids))+`)`, ids)
}

// Count implements [LexicalSource].
func (PageSource) Count(ctx context.Context, tx *sql.Tx) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pages_heads WHERE status = 'published'`).Scan(&n)
	return n, err
}

// TaskSource is the native work tracker.
//
// # What a work item's text IS
//
// Its title and its description, which is where the words somebody would
// search for live. Deliberately NOT its comment thread: a thread is a
// conversation about the item rather than a statement of it, indexing it would
// make one busy item outrank every concise one on any word said in passing,
// and a comment is already reachable through the item it is on.
//
// REMOVED ITEMS ARE OUT, which is the same rule the page source applies to a
// trashed page: the trash is reachable on purpose, by asking for it, and a
// ranked search that surfaced it would put work nobody is doing in front of
// an agent looking for work to do.
type TaskSource struct{}

// Source implements [LexicalSource].
func (TaskSource) Source() string { return string(SourceTask) }

// Versions implements [LexicalSource].
func (TaskSource) Versions(ctx context.Context, tx *sql.Tx, after string,
	limit int) ([]DocVersion, error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT id, version FROM tracker_tasks
		 WHERE removed_at IS NULL AND id > ?
		 ORDER BY id
		 LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	return scanVersions(rows)
}

// Fetch implements [LexicalSource].
//
// THE PROJECT IS THE CONTAINER, which is what a container-scoped search means
// for a work item and what [TaskCorpus] already uses on the embedding side —
// the two halves of one search have to agree about what scopes a document.
//
// AND THE BODY IS THE EXPENSIVE COLUMN, which is why it is only read here: it
// is a `json_extract` over the whole record blob, per row.
func (TaskSource) Fetch(ctx context.Context, tx *sql.Tx, ids []string) ([]Doc, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, project_key, title,
		       COALESCE(json_extract(document, '$.body'), ''), version
		  FROM tracker_tasks
		 WHERE removed_at IS NULL AND id IN (`+binds(len(ids))+`)`,
		anyOf(ids)...)
	if err != nil {
		return nil, err
	}
	return scanDocs(rows, string(SourceTask))
}

// Live implements [LexicalSource].
func (TaskSource) Live(ctx context.Context, tx *sql.Tx, ids []string) (map[string]bool, error) {
	return liveIDs(ctx, tx, `SELECT id FROM tracker_tasks
		 WHERE removed_at IS NULL AND id IN (`+binds(len(ids))+`)`, ids)
}

// Count implements [LexicalSource].
func (TaskSource) Count(ctx context.Context, tx *sql.Tx) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tracker_tasks WHERE removed_at IS NULL`).Scan(&n)
	return n, err
}

// scanDocs reads the five columns every source's batch query answers, in the
// one order all of them write it.
func scanDocs(rows *sql.Rows, source string) ([]Doc, error) {
	defer func() { _ = rows.Close() }()
	var out []Doc
	for rows.Next() {
		doc := Doc{Source: source}
		var version int64
		if err := rows.Scan(&doc.ID, &doc.Container, &doc.Title, &doc.Body,
			&version); err != nil {
			return nil, err
		}
		doc.Version = uint64(version)
		out = append(out, doc)
	}
	return out, rows.Err()
}

// scanVersions reads the two columns every source's scan query answers.
func scanVersions(rows *sql.Rows) ([]DocVersion, error) {
	defer func() { _ = rows.Close() }()
	var out []DocVersion
	for rows.Next() {
		var at DocVersion
		var version int64
		if err := rows.Scan(&at.ID, &version); err != nil {
			return nil, err
		}
		at.Version = uint64(version)
		out = append(out, at)
	}
	return out, rows.Err()
}

// anyOf is a string slice as bind arguments.
func anyOf(ids []string) []any {
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, id)
	}
	return out
}

// liveIDs runs one source's existence query over a batch of ids.
func liveIDs(ctx context.Context, tx *sql.Tx, query string,
	ids []string) (map[string]bool, error) {

	if len(ids) == 0 {
		return map[string]bool{}, nil
	}
	rows, err := tx.QueryContext(ctx, query, anyOf(ids)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]bool, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
