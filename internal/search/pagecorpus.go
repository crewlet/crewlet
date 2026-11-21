package search

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// The knowledge base's own pages, as the embedding duty's second corpus.
//
// `pages_heads` sits in the REPLICATED estate beside `kb_vectors`: the single
// anti-join [TaskCorpus] uses. A vector stands for a page's TITLE and BODY
// opening, filed under its CONTAINER; a save moves the body and `edit_version`,
// a rename or retitle the title (a rename also the container) without stamping
// the edit number. So selection asks all three against the row's `source_rev`,
// `title` and container; on the edit number alone, moved pages answered scoped
// searches from the container they left. NOT THE LOG VERSION: every comment
// stamps it, each publishing a ~17 KB record for an untouched text. A container
// move is RESTAMPED with no provider call ([Embedder.Tick]).

// PageCorpus embeds the knowledge base's published pages.
type PageCorpus struct{ DB store.ReplicatedReader }

// Source implements [Corpus].
func (PageCorpus) Source() Source { return SourcePage }

// pageLive is the population the page corpus embeds: published pages not in
// the trash — one spelling for every statement that reads or counts it, for
// [taskLive]'s reason. It is also, word for word, the predicate of
// `pages_heads_live_updated_idx` (replicated 0065), which the planner reaches
// only for a statement that states it.
const pageLive = `p.status = 'published' AND p.trashed_at IS NULL`

// pageBody is a page's body as a statement reads it, and pageOpening the part
// of it an opening can need — one spelling for the selection and the window
// report, for [taskOpening]'s reason.
const (
	pageBody    = `p.body`
	pageOpening = `substr(` + pageBody + `, 1, ?)`
)

// pageSelectionStatement: the published pages whose vector in the asked space
// is missing or was computed from another body, title or container, oldest
// first, each with its key, its title and the digest of the vector it has
// there — and not its body, which PageOpeningRead reads for the pages the
// selection keeps ([selectStale]). Bound: the space twice, the limit.
//
// WALKED IN ITS ORDER through `pages_heads_live_updated_idx`, the live pages
// by when they last changed (replicated 0065), so the LIMIT stops the walk at
// the first batch of stale pages. With no index in that order every tick read
// every page row in the company, drafts and the trash included, and sorted
// the live ones before it could return one.
const pageSelectionStatement = `
	SELECT p.id, p.container, p.edit_version, p.title,
	       CASE WHEN v.model = ? AND v.dim = ? THEN v.text_sha ELSE '' END
	FROM pages_heads p
	LEFT JOIN kb_vectors v
	  ON v.source = 'page' AND v.source_id = p.id
	WHERE ` + pageLive + `
	  AND (v.source_id IS NULL
	       OR v.source_rev <> p.edit_version
	       OR v.title <> p.title
	       OR v.container <> p.container
	       OR v.model <> ? OR v.dim <> ?)
	ORDER BY p.updated_at
	LIMIT ?`

// pageOpeningStatement: one selected page's opening, by its primary key.
// Bound: the body's read length, the page.
const pageOpeningStatement = `
	SELECT ` + pageOpening + `
	FROM pages_heads p
	WHERE p.id = ?`

// pageWithdrawalsStatement: the vectors of pages trashed, unpublished or
// purged. Bound: the limit.
const pageWithdrawalsStatement = `
	SELECT v.source_id
	FROM kb_vectors v
	LEFT JOIN pages_heads p
	  ON p.id = v.source_id AND ` + pageLive + `
	WHERE v.source = 'page' AND p.id IS NULL
	LIMIT ?`

// pageCoverageStatement: [pageSelectionStatement]'s predicate, inverted and
// counted. Bound: the space.
const pageCoverageStatement = `
	SELECT COUNT(*),
	       COUNT(CASE WHEN v.source_id IS NOT NULL
	                   AND v.source_rev = p.edit_version
	                   AND v.title = p.title
	                   AND v.container = p.container
	                   AND v.model = ? AND v.dim = ?
	                  THEN 1 END)
	FROM pages_heads p
	LEFT JOIN kb_vectors v
	  ON v.source = 'page' AND v.source_id = p.id
	WHERE ` + pageLive

// PageSelection is the statement [PageCorpus.Stale] selects stale pages with,
// and its arguments. It, [PageOpeningRead], [PageWithdrawals] and
// [PageCoverageCount] are the four statements this corpus runs, each the one
// that runs, so a plan gate explains what runs rather than a copy; see the
// package doc's "a read of another domain's table".
func PageSelection(model string, dim, limit int) (string, []any) {
	return pageSelectionStatement, []any{model, dim, model, dim, limit}
}

// PageOpeningRead is the statement [PageCorpus.Stale] reads one selected
// page's opening with, by its primary key, and its arguments: the
// [openingRead] [selectStale] prepares once and binds per page.
func PageOpeningRead(id string) (string, []any) {
	return pageOpeningStatement, []any{embedReadChars, id}
}

// PageWithdrawals is the statement [PageCorpus.Stale] finds the vectors of
// trashed, unpublished or purged pages with, and its arguments.
func PageWithdrawals(limit int) (string, []any) {
	return pageWithdrawalsStatement, []any{limit}
}

// PageCoverageCount is the statement [PageCorpus.Coverage] counts with, and
// its arguments.
func PageCoverageCount(model string, dim int) (string, []any) {
	return pageCoverageStatement, []any{model, dim}
}

// Stale implements [Corpus].
//
// PUBLISHED AND UNTRASHED ONLY. A draft is not searchable, so embedding one
// spends the provider bill on a document no query can return; and a trashed
// page whose vector survived is findable by meaning after it was thrown away,
// which is why both directions are here rather than only the first.
func (c PageCorpus) Stale(ctx context.Context, model string, dim, limit int, held Held) ([]Document, []string, error) {
	var stale []Document
	var gone []string
	err := c.DB.Read(ctx, func(tx *sql.Tx) error {
		statement, args := PageSelection(model, dim, limit+held.Len())
		var err error
		if stale, err = selectStale(ctx, tx, statement, args, limit, held,
			PageOpeningRead); err != nil {
			return err
		}

		// AND THE OTHER DIRECTION: a vector whose page was trashed,
		// unpublished or purged. Without it a page somebody deliberately
		// took down stays findable by meaning for ever — nothing else
		// will ever select it, because the selection above is driven by
		// the rows that remain.
		statement, args = PageWithdrawals(limit)
		gone, err = selectGone(ctx, tx, statement, args)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return stale, gone, nil
}

// Coverage implements [Corpus].
//
// ONE STATEMENT and the same predicate as [PageCorpus.Stale], inverted, for
// the reason [TaskCorpus.Coverage] gives: a coverage number derived from a
// second idea of what "current" means disagrees with the backlog the duty is
// working through — and here a page renamed or moved is NOT covered until its
// vector says so, because until then it is searched by meaning under a title
// or a container it no longer has.
func (c PageCorpus) Coverage(ctx context.Context, model string, dim int) (int, int, error) {
	var current, total int
	err := c.DB.Read(ctx, func(tx *sql.Tx) error {
		statement, args := PageCoverageCount(model, dim)
		return tx.QueryRowContext(ctx, statement, args...).Scan(&total, &current)
	})
	if err != nil {
		return 0, 0, fmt.Errorf("search: count the page corpus's vector "+
			"coverage: %w", err)
	}
	return current, total, nil
}
