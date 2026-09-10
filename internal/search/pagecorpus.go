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
// # A page's version is its edit number, not the log's
//
// `pages_heads` carries both, and the one stored beside a vector has to be the
// one that moves when the BODY moves. `version` is the composed log version and
// is stamped by a rename too; `edit_version` is the page's own monotonic edit
// number, which is what a save increments and a rename deliberately does not.
// Keyed on the log version, every rename in the company would re-embed a page
// whose text nobody touched — a provider bill for a title change.

// PageCorpus embeds the knowledge base's published pages.
type PageCorpus struct{ DB *store.DB }

// Source implements [Corpus].
func (PageCorpus) Source() Source { return SourcePage }

// Stale implements [Corpus].
//
// PUBLISHED AND UNTRASHED ONLY. A draft is not searchable, so embedding one
// spends the provider bill on a document no query can return; and a trashed
// page whose vector survived is findable by meaning after it was thrown away,
// which is why both directions are here rather than only the first.
func (c PageCorpus) Stale(ctx context.Context, model string, dim, limit int) ([]Document, []string, error) {
	var stale []Document
	var gone []string
	err := c.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT p.id, p.container, p.edit_version, p.title, p.body
			FROM pages_heads p
			LEFT JOIN kb_vectors v
			  ON v.source = 'page' AND v.source_id = p.id
			WHERE p.status = 'published' AND p.trashed_at IS NULL
			  AND (v.source_id IS NULL
			       OR v.source_rev <> p.edit_version
			       OR v.model <> ? OR v.dim <> ?)
			ORDER BY p.updated_at
			LIMIT ?`, model, dim, limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var doc Document
			var edit int64
			if err := rows.Scan(&doc.ID, &doc.Container, &edit,
				&doc.Title, &doc.Body); err != nil {
				return err
			}
			doc.Version = uint64(edit)
			stale = append(stale, doc)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		// AND THE OTHER DIRECTION: a vector whose page was trashed,
		// unpublished or purged. Without it a page somebody deliberately
		// took down stays findable by meaning for ever — nothing else
		// will ever select it, because the selection above is driven by
		// the rows that remain.
		dead, err := tx.QueryContext(ctx, `
			SELECT v.source_id
			FROM kb_vectors v
			LEFT JOIN pages_heads p
			  ON p.id = v.source_id AND p.status = 'published'
			 AND p.trashed_at IS NULL
			WHERE v.source = 'page' AND p.id IS NULL
			LIMIT ?`, limit)
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
// working through.
func (c PageCorpus) Coverage(ctx context.Context, model string, dim int) (int, int, error) {
	var current, total int
	err := c.DB.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT COUNT(*),
			       COUNT(CASE WHEN v.source_id IS NOT NULL
			                   AND v.source_rev = p.edit_version
			                   AND v.model = ? AND v.dim = ?
			                  THEN 1 END)
			FROM pages_heads p
			LEFT JOIN kb_vectors v
			  ON v.source = 'page' AND v.source_id = p.id
			WHERE p.status = 'published' AND p.trashed_at IS NULL`,
			model, dim).Scan(&total, &current)
	})
	if err != nil {
		return 0, 0, fmt.Errorf("search: count the page corpus's vector "+
			"coverage: %w", err)
	}
	return current, total, nil
}
