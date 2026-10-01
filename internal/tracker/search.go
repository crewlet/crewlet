package tracker

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
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
	ID       string `json:"id"`
	Key      string `json:"key"`
	Title    string `json:"title"`
	Project  string `json:"project"`
	Type     string `json:"type"`
	Status   Status `json:"status"`
	Assignee string `json:"assignee,omitempty"`
	// Rank is this hit's 1-based place in the answer.
	//
	// A PLACE AND NOT A SCORE, because a place is what the fan-out
	// actually produces: [search.Answer] carries fused KEYS, best first,
	// and the arithmetic that ordered them — score within a method, RRF
	// across methods, per slice — is finished before a coordinator sees
	// them. A `score` field here could only ever have serialised zero,
	// which is the same defect as every other value in this tree with no
	// writer, wearing a number's clothes.
	Rank int `json:"rank"`

	// Snippet is the index's own excerpt, which is what makes a ranked
	// answer readable without opening every hit.
	Snippet string `json:"snippet,omitempty"`
}

// RankedDoc is one candidate as the INDEX knows it, before this package says
// what it is a hit ON.
//
// NO SCORE, for [Ranked.Rank]'s reason: the scores travel in the candidates
// beside it ([search.Candidates]), and what a hit carries out of a fusion is
// its place.
type RankedDoc struct {
	ID      string
	Snippet string
}

// Ranker is the index seam, declared here because this is the caller.
//
// THE INDEX IS THIS NODE'S OWN and the rows are the fleet's, which is why the
// two halves of this search are two reads rather than a join — the same estate
// boundary every other reader here crosses the same way.
type Ranker interface {
	// Candidates is each method's top candidates for text, with scores, by
	// the index's own document key — and the work item behind each key, with
	// the index's excerpt of it.
	Candidates(ctx context.Context, text string) (search.Candidates, map[string]RankedDoc, error)

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

// Searcher answers a ranked item search over this node's corpus.
type Searcher struct {
	db   store.PartitionReader
	rank Ranker
}

// NewSearcher builds one over this node's store and its index.
func NewSearcher(db store.PartitionReader, rank Ranker) *Searcher {
	return &Searcher{db: db, rank: rank}
}

// ErrIndexBuilding reports an index that has not caught up.
//
// ITS OWN SENTINEL, because the caller's answer differs from every other
// refusal: nothing is wrong, the company's work is simply not all searchable
// yet, and the honest thing to tell a seat is "ask again" rather than "there
// is nothing" — which it would otherwise act on by filing a duplicate.
var ErrIndexBuilding = fmt.Errorf("tracker: the search index is still building")

// SearchAnswer is a ranked search's hits, best first, and what it covered.
type SearchAnswer struct {
	Hits []Ranked `json:"hits"`

	// Coverage is what a search answered PARTITION BY PARTITION covered —
	// see [statelog.Coverage]. A partition that did not answer is work
	// this search never ranked, and a caller renders a non-empty Missing as
	// its Notice rather than as the shorter list.
	Coverage statelog.Coverage `json:"coverage,omitzero"`
}

// SearchSlice is what one corpus answers a ranked search with BEFORE it is
// fused: its candidates, and every work item it can show, by the index's key.
//
// THE CANDIDATES RATHER THAN A RANKED LIST, for [search.Candidates]' reason:
// a list's order means nothing beside another corpus's, so each corpus sends
// what the fusion ranks it by, and the item behind every key it might win.
type SearchSlice struct {
	Candidates search.Candidates `json:"candidates"`

	// Items is the work item behind each candidate key, ABSENT for one
	// indexed and removed since — the index is behind this node's rows by
	// design — so the fusion walks past it rather than reporting an item
	// nobody can open.
	Items map[string]Ranked `json:"items,omitempty"`
}

// Slice answers a ranked search from this node's corpus, before fusion.
func (s *Searcher) Slice(ctx context.Context, text string) (SearchSlice, error) {
	if s == nil || s.rank == nil || s.db.IsZero() {
		return SearchSlice{}, fmt.Errorf("tracker: this node has no search index")
	}
	cands, docs, err := s.rank.Candidates(ctx, text)
	if err != nil {
		return SearchSlice{}, err
	}
	if len(docs) == 0 {
		// THE GATE IS ASKED ONLY ON AN EMPTY ANSWER, because that is the
		// only answer it changes: a search that found something has
		// found it whether or not the index is still catching up, and
		// asking every time would put one indexed count on every call.
		if s.rank.Building(ctx) {
			return SearchSlice{}, ErrIndexBuilding
		}
		return SearchSlice{Candidates: cands}, nil
	}
	rows, err := s.itemsByID(ctx, docs)
	if err != nil {
		return SearchSlice{}, err
	}
	out := SearchSlice{Candidates: cands, Items: make(map[string]Ranked, len(docs))}
	for key, doc := range docs {
		row, held := rows[doc.ID]
		if !held {
			// INDEXED AND GONE. The index is behind this node's own
			// rows by design — see the indexer's own orphan sweep —
			// so a hit whose item has been removed since is dropped
			// rather than reported as an item nobody can open.
			continue
		}
		row.Snippet = doc.Snippet
		out.Items[key] = row
	}
	return out, nil
}

// MergeSearch fuses the slices of DISJOINT corpora into one ranked answer of up
// to limit items, best first ([search.FuseCandidates]).
//
// THE PLACE IS COUNTED OVER WHAT SURVIVES, so an item dropped as gone leaves no
// gap in the numbering a reader would take for a result that went missing —
// and the fused order is walked past it rather than cut first, so a removed
// item costs the answer that item and never a place.
func MergeSearch(slices []SearchSlice, limit int) []Ranked {
	switch {
	case limit <= 0:
		limit = SearchLimit
	case limit > MaxSearchLimit:
		limit = MaxSearchLimit
	}
	cands := make([]search.Candidates, 0, len(slices))
	for _, slice := range slices {
		cands = append(cands, slice.Candidates)
	}
	out := make([]Ranked, 0, limit)
	for _, key := range search.FuseCandidates(cands) {
		row, ok := itemFor(slices, key)
		if !ok {
			continue
		}
		row.Rank = len(out) + 1
		out = append(out, row)
		if len(out) == limit {
			break
		}
	}
	return out
}

// itemFor is the item behind key in whichever slice named it — exactly one,
// since the corpora are disjoint.
func itemFor(slices []SearchSlice, key string) (Ranked, bool) {
	for _, slice := range slices {
		if row, ok := slice.Items[key]; ok {
			return row, true
		}
	}
	return Ranked{}, false
}

// itemsByID reads what a ranked hit has to carry, for one batch of candidates.
//
// ONE QUERY over the whole batch rather than a read per hit: the candidates
// are bounded by twice [search.FuseN], and a read per hit would answer each
// from a different instant.
func (s *Searcher) itemsByID(ctx context.Context, docs map[string]RankedDoc) (map[string]Ranked, error) {
	ids := make([]any, 0, len(docs))
	for _, doc := range docs {
		ids = append(ids, doc.ID)
	}
	out := make(map[string]Ranked, len(ids))
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
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
