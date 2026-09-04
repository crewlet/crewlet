package projection

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/textindex"
)

// SearchQuery is one lexical search over the index.
type SearchQuery struct {
	// Text is plain language. Never a query grammar: see [textindex] for
	// why this package deliberately offers none.
	Text string

	// Containers narrows to these containers. Empty is every one, which on
	// the native backend means the whole company — the engine IS the
	// boundary here, and there is no second account to launder a read
	// through.
	Containers []string

	// Sources narrows to "page", "item", or both. Empty is both.
	Sources []string

	// Limit caps the hits.
	Limit int
}

// SearchHit is one ranked document.
type SearchHit struct {
	Source    string
	ID        string
	Container string
	Title     string
	Snippet   string
	Score     float64
}

// maxPostingScan bounds how many postings one term contributes to a query.
//
// Five thousand. A term in nearly every document — a company's own name, or
// "the" — has a posting list the size of the corpus, and scanning all of it
// buys nothing: its IDF is near zero, so every one of those documents scores
// almost the same and the ranking is decided by the query's OTHER terms. The
// cap turns the pathological query into a bounded one, at the cost of an
// arbitrary subset of a term that was not discriminating anyway.
const maxPostingScan = 5000

// defaultSearchLimit is how many hits a query returns when it says nothing.
const defaultSearchLimit = 10

// Search ranks documents against a query, BM25 over the inverted list.
//
// IT RAISES rather than answering empty, unlike [knowledge.Searcher], and the
// two are reconciled at the seam: this is the storage layer, where "the store
// would not answer" and "nothing matched" are different facts, and the
// knowledge adapter above it is what turns a failure into the empty block a
// turn tolerates. Collapsing them here would make a broken index look exactly
// like a company that has written nothing down.
func (x *Indexer) Search(ctx context.Context, q SearchQuery) ([]SearchHit, error) {
	terms := textindex.Terms(q.Text)
	if len(terms) == 0 {
		return nil, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}

	corpus, err := x.corpus(ctx)
	if err != nil {
		return nil, err
	}
	if corpus.Docs == 0 {
		return nil, nil
	}

	scores := map[string]float64{}
	for _, term := range terms {
		postings, docs, err := x.postings(ctx, term, q)
		if err != nil {
			return nil, err
		}
		idf := textindex.IDF(corpus.Docs, docs)
		for _, p := range postings {
			scores[p.DocID] += textindex.Score(idf, p, corpus)
		}
	}
	if len(scores) == 0 {
		return nil, nil
	}
	return x.hydrateHits(ctx, scores, terms, limit)
}

// corpus reads the collection statistics BM25 needs.
func (x *Indexer) corpus(ctx context.Context) (textindex.Corpus, error) {
	var (
		docs int
		avg  float64
	)
	err := x.db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(AVG(length), 0) FROM kb_docs`).Scan(&docs, &avg)
	if err != nil {
		return textindex.Corpus{}, fmt.Errorf("projection: read index statistics: %w", err)
	}
	return textindex.Corpus{Docs: docs, AvgLength: avg}, nil
}

// postings reads one term's list, filtered to the query's scope, and the
// number of documents holding the term.
//
// THE DOCUMENT COUNT IS UNFILTERED, deliberately. It is the term's rarity
// across the whole corpus, which is what makes it a weight; counting only
// within a scope would make the same word rare in a small space and common in
// a large one, so a hit's rank would depend on which container it happened to
// be in rather than on how well it matched.
func (x *Indexer) postings(ctx context.Context, term string, q SearchQuery) ([]textindex.Posting, int, error) {
	var total int
	if err := x.db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM kb_postings WHERE term = ?`, term).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("projection: count postings for %q: %w", term, err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	where := []string{"p.term = ?"}
	args := []any{term}
	if len(q.Containers) > 0 {
		where = append(where, "d.container IN ("+binds(len(q.Containers))+")")
		for _, c := range q.Containers {
			args = append(args, c)
		}
	}
	if len(q.Sources) > 0 {
		where = append(where, "d.source IN ("+binds(len(q.Sources))+")")
		for _, s := range q.Sources {
			args = append(args, s)
		}
	}
	args = append(args, maxPostingScan)

	rows, err := x.db.SQL().QueryContext(ctx, `
		SELECT p.doc_id, p.freq, d.length
		  FROM kb_postings p
		  JOIN kb_docs d ON d.id = p.doc_id
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY p.freq DESC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("projection: read postings for %q: %w", term, err)
	}
	defer rows.Close()
	var out []textindex.Posting
	for rows.Next() {
		var p textindex.Posting
		if err := rows.Scan(&p.DocID, &p.Freq, &p.Length); err != nil {
			return nil, 0, fmt.Errorf("projection: scan posting for %q: %w", term, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("projection: read postings for %q: %w", term, err)
	}
	return out, total, nil
}

// hydrateHits turns scores into ranked hits, reading only the top ones.
//
// The ordering is done in Go over the score map rather than in SQL, because
// the scores exist only here: pushing them into a temporary table to sort
// them would cost a write transaction per query on a single-writer store.
func (x *Indexer) hydrateHits(ctx context.Context, scores map[string]float64, terms []string, limit int) ([]SearchHit, error) {
	top := topN(scores, limit)
	if len(top) == 0 {
		return nil, nil
	}
	ids := make([]any, 0, len(top))
	for _, id := range top {
		ids = append(ids, id)
	}
	rows, err := x.db.SQL().QueryContext(ctx, `
		SELECT id, source, source_id, container, title, excerpt
		  FROM kb_docs WHERE id IN (`+binds(len(ids))+`)`, ids...)
	if err != nil {
		return nil, fmt.Errorf("projection: read index hits: %w", err)
	}
	defer rows.Close()
	byID := map[string]SearchHit{}
	for rows.Next() {
		var id, excerpt string
		var hit SearchHit
		if err := rows.Scan(&id, &hit.Source, &hit.ID, &hit.Container,
			&hit.Title, &excerpt); err != nil {
			return nil, fmt.Errorf("projection: scan index hit: %w", err)
		}
		hit.Score = scores[id]
		hit.Snippet = textindex.Snippet(excerpt, terms, snippetBytes)
		byID[id] = hit
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projection: read index hits: %w", err)
	}
	out := make([]SearchHit, 0, len(top))
	for _, id := range top {
		// A hit whose row vanished between the posting scan and this read
		// is SKIPPED rather than rendered blank: the indexer removed it,
		// so it is a document that no longer exists.
		if hit, ok := byID[id]; ok {
			out = append(out, hit)
		}
	}
	return out, nil
}

// snippetBytes is the window a hit's snippet is cut to.
//
// Two hundred, matching [knowledge.SnippetLimit]: the block exists to tell a
// planner WHICH page to read, not to be the page, and it is re-sent on every
// round of the phase.
const snippetBytes = 200

// topN is the highest-scoring limit ids, best first.
//
// A partial selection rather than a full sort: a broad query touches every
// document holding a common term, and sorting the whole map to take ten is
// work proportional to the corpus on every search.
func topN(scores map[string]float64, limit int) []string {
	if limit <= 0 || len(scores) == 0 {
		return nil
	}
	out := make([]string, 0, limit)
	for id := range scores {
		out = insertRanked(out, scores, id, limit)
	}
	return out
}

// insertRanked places id into a descending-by-score list capped at limit.
//
// Ties break on the id, so two documents with identical scores rank in a
// stable order rather than in map-iteration order — a search that returned
// different results for the same query on the same data would look broken
// long before anyone suspected the ranking.
func insertRanked(out []string, scores map[string]float64, id string, limit int) []string {
	better := func(a, b string) bool {
		if scores[a] != scores[b] {
			return scores[a] > scores[b]
		}
		return a < b
	}
	at := len(out)
	for at > 0 && better(id, out[at-1]) {
		at--
	}
	if at >= limit {
		return out
	}
	if len(out) < limit {
		out = append(out, "")
	}
	copy(out[at+1:], out[at:])
	out[at] = id
	return out
}

// binds renders an IN list of n placeholders.
func binds(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
