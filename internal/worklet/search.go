package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
)

// RANKED SEARCH OVER WORK ITEMS: the half of "find the thing I half remember"
// the board's query language cannot answer.
//
// It is the tracker's own verb over the same index, not a widening of
// [knowledge.Searcher]: that seam is backend-neutral (Confluence, with no work
// items, may answer it), and its page-shaped [knowledge.Hit] fits no work item.
//
// The board's `q` stays beside it: a SUBSTRING of the key or title that pages,
// sorts and groups with every other filter, but cannot see a description or
// rank. One narrows a list, the other orders a corpus.

// Ranked is one work item as a ranked search answers it.
type Ranked struct {
	ID  string `json:"id"`
	Key string `json:"key"`

	// KeyCollision is [TaskRow.KeyCollision] for a hit: the key is one
	// another task claimed first, so it opens THAT task and this hit is
	// reached by its id ([ItemAddress]). A list a person opens items from
	// must tell a hit that opens its neighbour from one that does not.
	KeyCollision bool `json:"key_collision,omitempty"`

	Title    string `json:"title" untrusted:"name"`
	Project  string `json:"project"`
	Type     string `json:"type"`
	Status   Status `json:"status"`
	Assignee string `json:"assignee,omitempty"`
	// Priority is the item's own, so a hit says how much it matters beside
	// what it is, as the Board draws it.
	Priority Priority `json:"priority"`
	// Rank is this hit's 1-based place in the answer: a place, not a
	// score, because [search.Answer] carries fused KEYS, best first, the
	// scoring (per method, RRF across methods) finished before a
	// coordinator sees them, so a `score` here could only serialise zero.
	Rank int `json:"rank"`

	// Snippet is the index's own excerpt, which is what makes a ranked
	// answer readable without opening every hit.
	Snippet string `json:"snippet,omitempty" untrusted:"fence"`
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

// SearchQuery is one ranked item search.
type SearchQuery struct {
	// Text is plain language.
	Text string

	// Limit caps the answer. Zero takes [SearchLimit]; past
	// [MaxSearchLimit] is served the ceiling.
	Limit int

	// Mode is how to rank, in the knowledge search's own vocabulary —
	// one set of three modes for both of the engine's ranked searches.
	// The zero value is hybrid.
	Mode knowledge.Mode
}

// SearchAnswer is a ranked item search's hits and what the search did — the
// mode it served, what it covered and why it served less than was asked
// ([knowledge.Outcome]).
type SearchAnswer struct {
	Hits []Ranked `json:"hits"`
	knowledge.Outcome
}

// RankedCandidates is the index's answer over one corpus: each method's top
// candidates with their scores, the item behind every key, and what the
// ranking did.
type RankedCandidates struct {
	Candidates search.Candidates
	Docs       map[string]RankedDoc
	knowledge.Outcome
}

// Ranker is the index seam, declared here because this is the caller. The
// index is this node's own and the rows are the fleet's, so the two halves of
// a search are two reads rather than a join.
type Ranker interface {
	// Candidates is each method the query's mode runs, its top candidates
	// for the text with their scores, by the index's own document key —
	// the work item behind each key with the index's excerpt of it — and
	// what the ranking did.
	Candidates(ctx context.Context, q SearchQuery) (RankedCandidates, error)

	// Building reports an index that has not caught up with this node's
	// own rows, so a caller can tell "nothing matched" from "not indexed
	// yet" — which are different answers a person acts on differently.
	Building(ctx context.Context) bool
}

// SearchLimit is how many ranked items one search answers with: twenty, a
// screen and what a model can weigh in one read. The caller opens one or two,
// so the twenty-first is worth near zero yet costs the answer's budget as much
// as the first.
const SearchLimit = 20

// MaxSearchLimit caps what a caller may ask for.
const MaxSearchLimit = 50

// Searcher answers a ranked item search over this node's corpus.
type Searcher struct {
	db   store.ReplicatedReader
	rank Ranker
}

// NewSearcher builds one over this node's store and its index.
func NewSearcher(db store.ReplicatedReader, rank Ranker) *Searcher {
	return &Searcher{db: db, rank: rank}
}

// ErrIndexBuilding reports an index that has not caught up. Its own sentinel:
// nothing is wrong, the work is not all searchable yet, and a seat told "there
// is nothing" rather than "ask again" would file a duplicate.
var ErrIndexBuilding = fmt.Errorf("worklet: the search index is still building")

// Search ranks the company's work items against plain text, up to q's limit
// items, best first. The fused order is walked past an item removed since it
// was indexed rather than cut first and filtered after (the index lags this
// node's rows by design), so a removed leader costs that item, never a place;
// and the place is counted over what survives, so the numbering has no gap.
func (s *Searcher) Search(ctx context.Context, q SearchQuery) (SearchAnswer, error) {
	switch {
	case s == nil || s.rank == nil || s.db.IsZero():
		return SearchAnswer{}, fmt.Errorf("worklet: this node has no search index")
	case !q.Mode.Valid():
		return SearchAnswer{}, fmt.Errorf("worklet: unknown search mode %q; "+
			"the modes are hybrid, keyword and semantic", q.Mode)
	}
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = SearchLimit
	case limit > MaxSearchLimit:
		limit = MaxSearchLimit
	}
	keyed, err := s.keyed(ctx, q.Text)
	if err != nil {
		return SearchAnswer{}, err
	}
	ranked, err := s.rank.Candidates(ctx, q)
	if err != nil {
		return SearchAnswer{}, err
	}
	answer := SearchAnswer{Hits: make([]Ranked, 0, limit), Outcome: ranked.Outcome}
	// AN EXACT KEY IS ITS TASK, AND IT LEADS: the index ranks words, and
	// "ENG-2" splits into "eng" and "2", ranking every body that mentions it
	// above the task itself, if it ranks the task at all. So the task the key
	// directory resolves it to is placed first, and the ranking follows
	// without it.
	listed := make(map[string]bool, len(keyed))
	for _, row := range keyed {
		if len(answer.Hits) == limit {
			break
		}
		row.Rank = len(answer.Hits) + 1
		answer.Hits = append(answer.Hits, row)
		listed[row.ID] = true
	}
	if len(ranked.Docs) == 0 {
		if len(answer.Hits) > 0 {
			return answer, nil
		}
		// THE GATE IS ASKED ONLY ON AN EMPTY ANSWER, the only one it changes
		// (asking every time costs an indexed count per call), AND ONLY WHEN
		// THE WORDS WERE RANKED: the gate is the LEXICAL index's state. A
		// semantic search never read it, and one that ran nothing already
		// carries the degradation that explains its empty answer, which "still
		// building" would hide.
		if lexicalRan(ranked.ServedMode) && s.rank.Building(ctx) {
			return SearchAnswer{}, ErrIndexBuilding
		}
		return answer, nil
	}
	rows, err := s.itemsByID(ctx, ranked.Docs)
	if err != nil {
		return SearchAnswer{}, err
	}
	for _, key := range ranked.Candidates.Fused() {
		doc, named := ranked.Docs[key]
		if !named {
			continue
		}
		row, held := rows[doc.ID]
		if listed[doc.ID] {
			continue
		}
		if !held {
			// INDEXED AND GONE: the index lags this node's rows by design (the
			// indexer's orphan sweep), so a hit whose item was removed since is
			// walked past, not reported as an item nobody can open.
			continue
		}
		row.Snippet, row.Rank = doc.Snippet, len(answer.Hits)+1
		answer.Hits = append(answer.Hits, row)
		if len(answer.Hits) == limit {
			break
		}
	}
	return answer, nil
}

// lexicalRan reports whether an answer served in this mode read the lexical
// index. The EMPTY mode is a search that ranked nothing, so it read nothing —
// which [knowledge.Mode.Resolved] alone would call hybrid.
func lexicalRan(served knowledge.Mode) bool {
	return served != "" && served.Lexical()
}

// rankedItemsStatement is [Searcher.itemsByID]'s read of n ranked hits, named
// for the plan test.
func rankedItemsStatement(n int) string {
	return `
		SELECT id, key, key_collision, project_key, type, title, status,
		       assignee, priority
		  FROM tracker_tasks
		 WHERE removed_at IS NULL AND id IN (` + placeholders(n) + `)`
}

// keyedItemsStatement is [Searcher.keyed]'s read, named for the plan test:
// the task the key directory opens a key on, then any other live task whose
// own key it is (a duplicate flagged `key_collision`). The directory leads, in
// [taskReadScopeStatement]'s order: it holds every key a task answers to (and
// those a move or rename left it) and opens a shared key on the task that
// claimed it, so the leading hit is what a point read of the key returns.
const keyedItemsStatement = `
		SELECT t.id, t.key, t.key_collision, t.project_key, t.type, t.title,
		       t.status, t.assignee, t.priority
		  FROM tracker_task_keys k
		  JOIN tracker_tasks t ON t.id = k.task_id
		 WHERE k.key = ? AND t.removed_at IS NULL
		UNION ALL
		SELECT id, key, key_collision, project_key, type, title, status,
		       assignee, priority
		  FROM tracker_tasks
		 WHERE key = ? AND removed_at IS NULL`

// keyed is the live tasks a search text names by key, the one the key opens
// first, or none when the text is not exactly one task key.
//
// EXACTLY ONE KEY, case aside: a key inside a sentence is words, which the
// ranking is for, and "eng-2" typed in lower case is still the key.
func (s *Searcher) keyed(ctx context.Context, text string) ([]Ranked, error) {
	key := strings.ToUpper(strings.TrimSpace(text))
	if key == "" || taskKeyPattern().FindString(key) != key {
		return nil, nil
	}
	var out []Ranked
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		out = out[:0]
		rows, err := tx.QueryContext(ctx, keyedItemsStatement, key, key)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var row Ranked
			var collision int
			if err := rows.Scan(&row.ID, &row.Key, &collision, &row.Project,
				&row.Type, &row.Title, &row.Status, &row.Assignee,
				&row.Priority); err != nil {
				return err
			}
			if slices.ContainsFunc(out, func(r Ranked) bool { return r.ID == row.ID }) {
				continue
			}
			row.KeyCollision = collision == 1
			out = append(out, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("worklet: read the task %s names: %w", key, err)
	}
	return out, nil
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
		rows, err := tx.QueryContext(ctx, rankedItemsStatement(len(ids)), ids...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var row Ranked
			var collision int
			if err := rows.Scan(&row.ID, &row.Key, &collision, &row.Project,
				&row.Type, &row.Title, &row.Status, &row.Assignee,
				&row.Priority); err != nil {
				return err
			}
			row.KeyCollision = collision == 1
			out[row.ID] = row
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("worklet: read the ranked items: %w", err)
	}
	return out, nil
}
