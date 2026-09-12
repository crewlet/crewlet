package tracker

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// RANKED SEARCH OVER WORK ITEMS, which is the half of "find the thing I half
// remember" the board's query language cannot answer.
//
// # Why this is the tracker's and not the knowledge seam's
//
// `search_knowledge` reads through [knowledge.Searcher], which is
// BACKEND-NEUTRAL by design and has Confluence behind it on a company that
// runs one — and Confluence has no work items. Widening that seam to carry
// tasks would make "what do we already know about this" depend on which
// backend answered, which is the one thing that package exists to prevent. Its
// [knowledge.Hit] is page-shaped for the same reason: a space key, a page id
// and an ancestor chain, none of which a work item has.
//
// So the tracker gets its own verb, over the same index, exactly as it has its
// own board beside the wiki's own list.
//
// # What the board's `q` is, and why it is not this
//
// The grammar's `q` is a SUBSTRING of the key or the title, composed into the
// board query with every other filter — a cheap predicate that pages, sorts
// and groups with the rest, and the right tool for "the item whose key I am
// half sure of". It cannot see a description, it cannot rank, and no amount of
// filtering turns it into either. Both stay: one narrows a list, the other
// orders a corpus.

// Ranked is one work item as a ranked search answers it.
type Ranked struct {
	ID       string  `json:"id"`
	Key      string  `json:"key"`
	Title    string  `json:"title"`
	Project  string  `json:"project"`
	Type     string  `json:"type"`
	Status   Status  `json:"status"`
	Assignee string  `json:"assignee,omitempty"`
	Score    float64 `json:"score"`

	// Snippet is the index's own excerpt, which is what makes a ranked
	// answer readable without opening every hit.
	Snippet string `json:"snippet,omitempty"`
}

// RankedDoc is one hit as the INDEX knows it, before this package says what it
// is a hit ON.
type RankedDoc struct {
	ID      string
	Snippet string
	Score   float64
}

// Ranker is the index seam, declared here because this is the caller.
//
// THE INDEX IS THIS NODE'S OWN and the rows are the fleet's, which is why the
// two halves of this search are two reads rather than a join — the same estate
// boundary every other reader here crosses the same way.
type Ranker interface {
	// RankItems returns work-item ids in rank order, best first.
	RankItems(ctx context.Context, text string, limit int) ([]RankedDoc, error)

	// Building reports an index that has not caught up with this node's
	// own rows, so a caller can tell "nothing matched" from "not indexed
	// yet" — which are different answers a person acts on differently.
	Building(ctx context.Context) bool
}

// SearchLimit is how many ranked items one search answers with.
//
// TWENTY, which is a screen and is also what a model can weigh in one read. A
// ranked answer is not a board: the caller's next move is to open one or two
// of them, so the value of the twenty-first is near zero while its cost in an
// answer's budget is the same as the first's.
const SearchLimit = 20

// MaxSearchLimit caps what a caller may ask for.
const MaxSearchLimit = 50

// Searcher answers a ranked item search.
type Searcher struct {
	db   *store.DB
	rank Ranker
}

// NewSearcher builds one over this node's store and its index.
func NewSearcher(db *store.DB, rank Ranker) *Searcher {
	return &Searcher{db: db, rank: rank}
}

// ErrIndexBuilding reports an index that has not caught up.
//
// ITS OWN SENTINEL, because the caller's answer differs from every other
// refusal: nothing is wrong, the company's work is simply not all searchable
// yet, and the honest thing to tell a seat is "ask again" rather than "there
// is nothing" — which it would otherwise act on by filing a duplicate.
var ErrIndexBuilding = fmt.Errorf("tracker: the search index is still building")

// Search ranks the company's work items against plain text.
func (s *Searcher) Search(ctx context.Context, text string, limit int) ([]Ranked, error) {
	switch {
	case s == nil || s.rank == nil || s.db == nil:
		return nil, fmt.Errorf("tracker: this node has no search index")
	case limit <= 0:
		limit = SearchLimit
	case limit > MaxSearchLimit:
		limit = MaxSearchLimit
	}
	docs, err := s.rank.RankItems(ctx, text, limit)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		// THE GATE IS ASKED ONLY ON AN EMPTY ANSWER, because that is the
		// only answer it changes: a search that found something has
		// found it whether or not the index is still catching up, and
		// asking every time would put one indexed count on every call.
		if s.rank.Building(ctx) {
			return nil, ErrIndexBuilding
		}
		return nil, nil
	}
	rows, err := s.itemsByID(ctx, docs)
	if err != nil {
		return nil, err
	}
	// IN THE INDEX'S ORDER, not the database's. The rank is the whole
	// answer here, and a SQL read returns rows in whatever order suits it.
	out := make([]Ranked, 0, len(docs))
	for _, doc := range docs {
		row, held := rows[doc.ID]
		if !held {
			// INDEXED AND GONE. The index is behind this node's own
			// rows by design — see the indexer's own orphan sweep —
			// so a hit whose item has been removed since is dropped
			// rather than reported as an item nobody can open.
			continue
		}
		row.Score, row.Snippet = doc.Score, doc.Snippet
		out = append(out, row)
	}
	return out, nil
}

// itemsByID reads what a ranked hit has to carry, for one batch of ids.
//
// ONE QUERY over the whole batch rather than a read per hit: the ids come from
// a ranking that is already bounded by [MaxSearchLimit], and a read per hit
// would answer each from a different instant.
func (s *Searcher) itemsByID(ctx context.Context, docs []RankedDoc) (map[string]Ranked, error) {
	ids := make([]any, 0, len(docs))
	for _, doc := range docs {
		ids = append(ids, doc.ID)
	}
	out := make(map[string]Ranked, len(ids))
	err := s.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, key, project_key, type, title, status, assignee
			  FROM tracker_tasks
			 WHERE removed_at IS NULL AND id IN (`+placeholders(len(ids))+`)`,
			ids...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var row Ranked
			if err := rows.Scan(&row.ID, &row.Key, &row.Project, &row.Type,
				&row.Title, &row.Status, &row.Assignee); err != nil {
				return err
			}
			out[row.ID] = row
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("tracker: read the ranked items: %w", err)
	}
	return out, nil
}
