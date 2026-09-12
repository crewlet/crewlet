package search

import (
	"context"
	"database/sql"
	"fmt"
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
// # Three queries, because the walk asks three questions
//
// The sources and the index are in DIFFERENT ESTATES and no read joins them
// (see [Indexer]), so every comparison is a batch from each side. That makes
// the questions: what is the next batch of documents, which of these indexed
// ids still exist, and how many documents are there — the last being the
// readiness gate's, which is a count rather than a walk because "not indexed
// yet" and "nothing written down" are answers a person acts on differently.
//
// Each takes the replicated estate's own transaction, opened by the indexer:
// a source that opened its own would be a second read at a second instant, and
// the walk's whole correctness argument is that a batch is compared against
// one snapshot of the other side.
type LexicalSource interface {
	// Source is the value written into `kb_docs.source`, and it is what
	// makes one index table serve corpora whose ids can collide.
	Source() string

	// Next is the batch of documents after a cursor, ordered by id.
	Next(ctx context.Context, tx *sql.Tx, after string, limit int) ([]Doc, error)

	// Live is which of these ids this source still has. An id absent from
	// the answer is an orphan the index drops.
	Live(ctx context.Context, tx *sql.Tx, ids []string) (map[string]bool, error)

	// Count is how many documents this source offers, for the gate.
	Count(ctx context.Context, tx *sql.Tx) (int, error)
}

// DefaultLexicalSources is what a node indexes.
//
// BOTH CORPORA, and the tracker's is not an addition so much as a repair: the
// engine already embeds every task and already declares [SourceTask] beside
// [SourcePage] in [Sources], so the lexical half was the one place that had
// never heard of them.
func DefaultLexicalSources() []LexicalSource {
	return []LexicalSource{PageSource{}, TaskSource{}}
}

// PageSource is the knowledge base.
type PageSource struct{}

// Source implements [LexicalSource].
func (PageSource) Source() string { return string(SourcePage) }

// Next implements [LexicalSource].
//
// PUBLISHED ONLY: a draft is somebody's unfinished thought and a trashed page
// is deleted as far as a reader is concerned, and surfacing either in a
// knowledge search would put content in front of an agent that no person
// considers current.
func (PageSource) Next(ctx context.Context, tx *sql.Tx, after string, limit int) ([]Doc, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, container, title, body, MAX(version, scoped_through)
		  FROM pages_heads
		 WHERE status = 'published' AND id > ?
		 ORDER BY id
		 LIMIT ?`, after, limit)
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

// Next implements [LexicalSource].
//
// THE PROJECT IS THE CONTAINER, which is what a container-scoped search means
// for a work item and what [TaskCorpus] already uses on the embedding side —
// the two halves of one search have to agree about what scopes a document.
func (TaskSource) Next(ctx context.Context, tx *sql.Tx, after string, limit int) ([]Doc, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, project_key, title,
		       COALESCE(json_extract(document, '$.body'), ''), version
		  FROM tracker_tasks
		 WHERE removed_at IS NULL AND id > ?
		 ORDER BY id
		 LIMIT ?`, after, limit)
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

// liveIDs runs one source's existence query over a batch of ids.
func liveIDs(ctx context.Context, tx *sql.Tx, query string,
	ids []string) (map[string]bool, error) {

	if len(ids) == 0 {
		return map[string]bool{}, nil
	}
	bound := make([]any, 0, len(ids))
	for _, id := range ids {
		bound = append(bound, id)
	}
	rows, err := tx.QueryContext(ctx, query, bound...)
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

// sourceNames is every source name this indexer covers, for a message.
func sourceNames(sources []LexicalSource) string {
	names := make([]string, 0, len(sources))
	for _, s := range sources {
		names = append(names, s.Source())
	}
	return fmt.Sprint(names)
}
