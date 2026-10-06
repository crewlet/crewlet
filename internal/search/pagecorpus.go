package search

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// The knowledge base's own pages, as the embedding duty's second corpus.
//
// # Why it is one anti-join now and could not have been before
//
// When the pages were this node's own projection they lived in a different
// estate from `kb_vectors`, and no statement may name a table in both: the
// arm would have been a bounded cursor sweep whose whole shape existed to work
// around a boundary. The pages domain removed the boundary — `pages_heads` is
// written by an applier into the REPLICATED estate, beside the vectors — so
// the selection is the same single anti-join [TaskCorpus] uses, and the seam
// the duty was built around costs nothing to fill.
//
// # A page is selected when what its vector was computed from moved — exactly
//
// A vector stands for a page's TITLE and the opening of its BODY, filed under
// its CONTAINER, and those three move on three different paths: a save moves
// the body and the page's own edit number, `edit_version`; a rename moves the
// title and — across containers — the container, and a retitle moves the
// displayed title, and neither stamps the edit number. So the selection asks
// all three, each against what the vector row says it was computed from: the
// edit number it stores as `source_rev`, the title it stores as `title`, and
// the container it is filed under.
//
// Keyed on the edit number alone it was blind to the other two: a renamed
// page kept a vector of its old title, and a page moved to another container
// kept answering scoped semantic searches from the one it left — returned
// inside a knowledge scope it had been moved out of, missing from the one it
// had been moved into — while the lexical half, which keys on the log
// version, had already moved it.
//
// NOT THE LOG VERSION, MAX(version, scoped_through), which the lexical index
// keys on and which would also catch all three: every comment, watcher change
// and child re-parent stamps it too, so every one of those would select the
// page and publish a ~17 KB vector record for a text nobody touched. Exact
// costs one column on the vector row; the broad key costs a record per
// comment, for ever.
//
// AND WHAT IS SELECTED WITHOUT A NEW TEXT IS RESTAMPED: a move to another
// container keeps the title and the body, so the digest the stored vector
// carries is still the text's, and the vector is republished under the new
// container with no provider call ([Embedder.Tick]). A rename to another title
// is a new text, and is embedded.

// PageCorpus embeds the knowledge base's published pages.
type PageCorpus struct{ DB store.ReplicatedReader }

// Source implements [Corpus].
func (PageCorpus) Source() Source { return SourcePage }

// pageLive is the population the page corpus embeds: published pages not in
// the trash — one spelling for every statement that reads or counts it, for
// [taskLive]'s reason.
const pageLive = `p.status = 'published' AND p.trashed_at IS NULL`

// pageSelectionStatement: the published pages whose vector in the asked space
// is missing or was computed from another body, title or container, oldest
// first, each with the opening of its body and the digest of the vector it has
// there. Bound: the body's read length, the space twice, the limit.
const pageSelectionStatement = `
	SELECT p.id, p.container, p.edit_version, p.title,
	       substr(p.body, 1, ?),
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

// pageSelection, pageWithdrawals and pageCoverageCount are the three
// statements this corpus runs, each with its arguments — one place, so the
// plan gate explains what runs rather than a copy.
func pageSelection(model string, dim, limit int) (string, []any) {
	return pageSelectionStatement, []any{embedReadChars, model, dim, model, dim, limit}
}

func pageWithdrawals(limit int) (string, []any) {
	return pageWithdrawalsStatement, []any{limit}
}

func pageCoverageCount(model string, dim int) (string, []any) {
	return pageCoverageStatement, []any{model, dim}
}

// Stale implements [Corpus].
//
// PUBLISHED AND UNTRASHED ONLY. A draft is not searchable, so embedding one
// spends the provider bill on a document no query can return; and a trashed
// page whose vector survived is findable by meaning after it was thrown away,
// which is why both directions are here rather than only the first.
func (c PageCorpus) Stale(ctx context.Context, model string, dim, limit int) ([]Document, []string, error) {
	var stale []Document
	var gone []string
	err := c.DB.Read(ctx, func(tx *sql.Tx) error {
		statement, args := pageSelection(model, dim, limit)
		rows, err := tx.QueryContext(ctx, statement, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var doc Document
			var edit int64
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := rows.Scan(&doc.ID, &doc.Container, &edit,
				&doc.Title, &doc.Body, &doc.StoredSHA); err != nil {
				return err
			}
			doc.Version = uint64(edit)
			stale = append(stale, doc)
		}
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := rows.Err(); err != nil {
			return err
		}

		// AND THE OTHER DIRECTION: a vector whose page was trashed,
		// unpublished or purged. Without it a page somebody deliberately
		// took down stays findable by meaning for ever — nothing else
		// will ever select it, because the selection above is driven by
		// the rows that remain.
		statement, args = pageWithdrawals(limit)
		dead, err := tx.QueryContext(ctx, statement, args...)
		if err != nil {
			return err
		}
		defer func() { _ = dead.Close() }()
		for dead.Next() {
			var id string
			if err := dead.Scan(&id); err != nil {
				return err
			}
			gone = append(gone, id)
		}
		return dead.Err()
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
		statement, args := pageCoverageCount(model, dim)
		return tx.QueryRowContext(ctx, statement, args...).Scan(&total, &current)
	})
	if err != nil {
		return 0, 0, fmt.Errorf("search: count the page corpus's vector "+
			"coverage: %w", err)
	}
	return current, total, nil
}
