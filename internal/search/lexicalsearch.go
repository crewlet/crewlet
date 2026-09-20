package search

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/textindex"
)

// LexicalQuery is one BM25 search over this node's own inverted list.
//
// NAMED FOR ITS RANKER, like [SemanticQuery] beside it: a hybrid answer is two
// rankings fused, so "the query" is never one thing here and a type that
// claimed the bare word would have to be read twice to know which half it is.
type LexicalQuery struct {
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

	// Shards is the bucket range this scan may read.
	//
	// THE ZERO VALUE IS EVERY BUCKET, on [SemanticQuery.Shards]'s terms:
	// one meaning "no buckets" would answer every search with nothing, and
	// an empty answer is indistinguishable from an empty corpus.
	Shards Assignment

	// MergeInput says this answer feeds [MergeByScore] rather than a
	// reader, which is the fan-out participant and nothing else.
	//
	// IT SKIPS THE BODY READ A SNIPPET NEEDS. A [Slice] carries keys and
	// scores and no text at all, so the snippet a participant cut would be
	// fifty documents read and discarded per query per node; the
	// coordinator cuts them once over the fused list, in [Indexer.Hydrate].
	// The excerpt-cut snippet is still filled, because it costs a column
	// of a row the hydration already reads.
	MergeInput bool
}

// LexicalHit is one document [Indexer.Search] ranked, and the BM25 score it
// ranked it on.
//
// THE SCORE IS COMPARABLE ACROSS PARTICIPANTS, which is what the fan-out merges
// its slices on: every node holds the whole corpus, so the statistics behind
// this number are global however few buckets the scan read — see [FanOut].
type LexicalHit struct {
	Source    string
	ID        string
	Container string
	Title     string
	Snippet   string
	Score     float64
}

// FusedHit is one document from a FUSED answer, in the order the fusion put it
// in and with no score.
//
// SEPARATE FROM [LexicalHit] BY ONE FIELD, deliberately — see [Indexer.Hydrate].
// Carrying the field and leaving it zero is what this pair of types exists to
// make impossible.
type FusedHit struct {
	Source    string
	ID        string
	Container string
	Title     string
	Snippet   string
}

// maxPostingScan bounds how many postings one term contributes to a query.
//
// Five thousand. A term in nearly every document — a company's own name, or
// "the" — has a posting list the size of the corpus, and scanning all of it
// buys nothing: its IDF is near zero, so every one of those documents scores
// almost the same and the ranking is decided by the query's OTHER terms. The
// cap turns the pathological query into a bounded one.
//
// WHAT IT DROPS IS THE BOTTOM OF THE LIST, which is the only thing that makes
// an absolute cap defensible at all. The paragraph above justifies cutting a
// term that appears in nearly every document, and 5 000 is not that on a
// large corpus: a term in 5 001 of 100 000 documents has an IDF near 3 and
// decides the ranking. So the cut cannot be arbitrary, and the read is
// ORDERED BY THE TERM'S OWN BM25 CONTRIBUTION — see [Indexer.postings] — so
// what a capped term loses are postings that would have ranked below five
// thousand others of its own.
//
// It was `ORDER BY p.freq DESC`, which is the same thing only when every
// document is the same length. BM25 divides by length, so raw frequency keeps
// the LONGEST documents — a 10 000-term runbook mentioning the term five
// times displaced a 50-term page mentioning it five times, though the page
// scores an order of magnitude higher. That is ranking by verbosity rather
// than by coverage, which is the exact failure [textindex.K1] is pinned at
// 1.2 to avoid, reintroduced underneath it by a LIMIT.
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
func (x *Indexer) Search(ctx context.Context, q LexicalQuery) ([]LexicalHit, error) {
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
		postings, docs, err := x.postings(ctx, term, q, corpus)
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
	return x.hydrateHits(ctx, q, scores, terms, limit)
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
		return textindex.Corpus{}, fmt.Errorf("search: read index statistics: %w", err)
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
//
// ORDERED BY THE TERM'S OWN BM25 CONTRIBUTION, which is what makes
// [maxPostingScan] a cut of the bottom of the list rather than of an
// arbitrary slice of it. The expression is the ORDER of [textindex.Score] and
// not the score: `idf` and `K1+1` are constants within one term, and
// `tf/(tf+K)` is increasing in `tf/K`, so ordering by `tf/K` gives exactly
// the same sequence with one division instead of three. K is
// `K1*(1-B+B*len/avg)`, and dropping the constant K1 and the constant avg
// leaves `freq / ((1-B)*avg + B*length)` — the whole of it, with B bound from
// [textindex.B] rather than written into the SQL, so the two cannot drift.
//
// A DOCUMENT WITH NO RECORDED LENGTH scores as AVERAGE here, exactly as
// [textindex.Score] treats it: as infinitely short it would sort above
// everything and take the cap's whole budget.
func (x *Indexer) postings(ctx context.Context, term string, q LexicalQuery,
	corpus textindex.Corpus) ([]textindex.Posting, int, error) {
	var total int
	if err := x.db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM kb_postings WHERE term = ?`, term).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("search: count postings for %q: %w", term, err)
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
	if q.Shards.Covers() {
		// THE SHARD IS ON THE DOCUMENT, not the posting. A posting
		// belongs to its document, so a term's list is filtered through
		// the join rather than by a column of its own — which is also
		// why kb_postings has no shard: it would be the document's own
		// value written once per term it contains.
		where = append(where, "d.search_shard >= ? AND d.search_shard < ?")
		args = append(args, q.Shards.From, q.Shards.To)
	}
	// The same guard [textindex.Score] applies, for the same reason: an
	// index whose lengths are all zero would otherwise divide by zero here
	// and order by nothing at all.
	avg := corpus.AvgLength
	if avg <= 0 {
		avg = 1
	}
	args = append(args, textindex.B, avg, textindex.B, avg, maxPostingScan)

	rows, err := x.db.SQL().QueryContext(ctx, `
		SELECT p.doc_id, p.freq, d.length
		  FROM kb_postings p
		  JOIN kb_docs d ON d.id = p.doc_id
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY p.freq /
		          ((1 - ?) * ? + ? * (CASE WHEN d.length > 0 THEN d.length ELSE ? END))
		          DESC, p.doc_id
		 LIMIT ?`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("search: read postings for %q: %w", term, err)
	}
	defer rows.Close()
	var out []textindex.Posting
	for rows.Next() {
		var p textindex.Posting
		if err := rows.Scan(&p.DocID, &p.Freq, &p.Length); err != nil {
			return nil, 0, fmt.Errorf("search: scan posting for %q: %w", term, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("search: read postings for %q: %w", term, err)
	}
	return out, total, nil
}

// hydrateHits turns scores into ranked hits, reading only the top ones.
//
// The ordering is done in Go over the score map rather than in SQL, because
// the scores exist only here: pushing them into a temporary table to sort
// them would cost a write transaction per query on a single-writer store.
func (x *Indexer) hydrateHits(ctx context.Context, q LexicalQuery,
	scores map[string]float64, terms []string, limit int) ([]LexicalHit, error) {
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
		return nil, fmt.Errorf("search: read index hits: %w", err)
	}
	defer rows.Close()
	byID := map[string]LexicalHit{}
	for rows.Next() {
		var id, excerpt string
		var hit LexicalHit
		if err := rows.Scan(&id, &hit.Source, &hit.ID, &hit.Container,
			&hit.Title, &excerpt); err != nil {
			return nil, fmt.Errorf("search: scan index hit: %w", err)
		}
		hit.Score = scores[id]
		hit.Snippet = textindex.Snippet(excerpt, terms, snippetBytes)
		byID[id] = hit
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: read index hits: %w", err)
	}
	// THE SNIPPET IS RECUT FROM THE BODY, unless this answer is a merge
	// input nobody will read one from.
	if !q.MergeInput {
		for key, snippet := range x.resnippet(ctx,
			bodyRefs(byID, func(h LexicalHit) (string, string) {
				return h.Source, h.ID
			}), terms) {
			hit := byID[key]
			hit.Snippet = snippet
			byID[key] = hit
		}
	}
	out := make([]LexicalHit, 0, len(top))
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

// resnippet recuts the top hits' snippets FROM THEIR BODIES.
//
// A SNIPPET THAT DOES NOT CONTAIN THE SEARCH TERM READS AS A WRONG RESULT,
// which is [textindex.Snippet]'s own reason for centring on the match — and
// the excerpt it was handed guarantees the failure for every hit that matched
// deeper than [excerptLimit] into its body. The stored excerpt is the
// document's OPENING, so `firstTermIndex` found nothing, the window stayed at
// zero, and the answer was the page's preamble under an ellipsis that says
// text was cut but not that the match is in it. The Confluence backend behind
// the same [knowledge] seam snippets from the whole body, so which searcher a
// company ran decided whether its hits showed why they were hits.
//
// THROUGH THE SOURCE'S OWN [LexicalSource.Fetch], batched per source, for the
// TOP N ONLY — twenty rows by primary key, never the corpus — and in the
// replicated estate's own transaction, because a body lives beside the
// document and the index is this node's. It is the seam the indexer already
// uses to read a body it is about to tokenise; nothing new crosses the
// boundary.
//
// IT DEGRADES TO WHAT IT WAS. Every failure here — a read that could not be
// taken, a source that no longer has the row, a body that is now empty —
// leaves the excerpt-cut snippet in place and logs. A hit is still a hit, and
// a search that died because one body was unreadable is strictly worse than
// one whose snippet is a preamble.
// IT RETURNS RATHER THAN MUTATES, because the two hydrations it serves
// answer in different types — [LexicalHit] carries a score and [FusedHit]
// deliberately does not — and a shared step that wrote into both would need
// the field they differ by.
func (x *Indexer) resnippet(ctx context.Context, want map[string][2]string,
	terms []string) map[string]string {

	// BY SOURCE, because an id is only unique within one: `kb_docs.id` is
	// source-qualified for exactly that reason, and the source's Fetch
	// takes its own unqualified ids.
	wanted := map[string][]string{}
	for _, ref := range want {
		wanted[ref[0]] = append(wanted[ref[0]], ref[1])
	}
	out := make(map[string]string, len(want))
	for _, source := range x.sources {
		ids := wanted[source.Source()]
		if len(ids) == 0 {
			continue
		}
		if err := x.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
			docs, err := source.Fetch(ctx, tx, ids)
			if err != nil {
				return err
			}
			for _, d := range docs {
				// AN EMPTY BODY IS NOT AN IMPROVEMENT: a page
				// whose text is only its title keeps the
				// excerpt's snippet rather than getting none.
				if strings.TrimSpace(d.Body) == "" {
					continue
				}
				out[docKey(d.Source, d.ID)] =
					textindex.Snippet(d.Body, terms, snippetBytes)
			}
			return nil
		}); err != nil {
			log.WarnContext(ctx, "search_snippet_body_unavailable",
				"source", source.Source(), "documents", len(ids), "error", err)
		}
	}
	return out
}

// bodyRefs is the (source, id) pair each key needs fetching by.
func bodyRefs[T any](byKey map[string]T, of func(T) (string, string)) map[string][2]string {
	out := make(map[string][2]string, len(byKey))
	for key, v := range byKey {
		source, id := of(v)
		out[key] = [2]string{source, id}
	}
	return out
}

// snippetBytes is the window a hit's snippet is cut to.
//
// Two hundred, matching [knowledge.SnippetLimit]: the block exists to tell an
// agent WHICH page to read, not to be the page, and it is re-sent on every
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

// Hydrate reads the documents behind a fused answer, in the order given.
//
// THE FAN-OUT RETURNS KEYS, not rows, and it has to: a slice that carried
// titles and snippets across the broker would move a kilobyte per candidate to
// render ten. So the coordinator fuses ids and reads the rows here — locally,
// from tables that hold the whole corpus, which is what makes hydrating a
// PEER's hit an ordinary local read rather than a second round trip.
//
// A key whose row is gone is SKIPPED rather than rendered blank, on
// [Indexer.hydrateHits]'s terms: between the scan and this read the indexer
// may have removed a document, and a document that no longer exists is not an
// answer.
//
// # IT RETURNS [FusedHit], WHICH HAS NO SCORE, AND THAT IS THE POINT
//
// A [LexicalHit] carries the BM25 number one ranker gave one document, which is
// comparable within that ranker and is what the fan-out merges its own slices
// on. What comes back HERE has been through reciprocal rank fusion across two
// rankers and across disjoint slices, so the only thing left is an ORDER — the
// fused number is a sum of reciprocal placements and means nothing beside a
// BM25 score.
//
// It used to return [LexicalHit] and set no score at all, so every hit the two
// ranked readers in this tree render carried a confident `Score: 0`. A zero
// that is not a value is exactly the shape a type has to refuse rather than
// document, so this one does: a caller cannot read a score that does not
// exist.
func (x *Indexer) Hydrate(ctx context.Context, keys []string, text string) ([]FusedHit, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	ids := make([]any, 0, len(keys))
	for _, key := range keys {
		ids = append(ids, key)
	}
	rows, err := x.db.SQL().QueryContext(ctx, `
		SELECT id, source, source_id, container, title, excerpt
		  FROM kb_docs WHERE id IN (`+binds(len(ids))+`)`, ids...)
	if err != nil {
		return nil, fmt.Errorf("search: read fused hits: %w", err)
	}
	defer rows.Close()
	terms := textindex.Terms(text)
	byKey := make(map[string]FusedHit, len(keys))
	for rows.Next() {
		var id, excerpt string
		var hit FusedHit
		if err := rows.Scan(&id, &hit.Source, &hit.ID, &hit.Container,
			&hit.Title, &excerpt); err != nil {
			return nil, fmt.Errorf("search: scan a fused hit: %w", err)
		}
		hit.Snippet = textindex.Snippet(excerpt, terms, snippetBytes)
		byKey[id] = hit
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: read fused hits: %w", err)
	}
	// THE SAME RECUT, because this is where a FAN-OUT's snippets are made:
	// a participant answers with keys and scores alone, so the coordinator
	// is the only place in that path a snippet exists at all.
	for key, snippet := range x.resnippet(ctx,
		bodyRefs(byKey, func(h FusedHit) (string, string) {
			return h.Source, h.ID
		}), terms) {
		hit := byKey[key]
		hit.Snippet = snippet
		byKey[key] = hit
	}
	out := make([]FusedHit, 0, len(keys))
	for _, key := range keys {
		if hit, ok := byKey[key]; ok {
			out = append(out, hit)
		}
	}
	return out, nil
}

// Corpus reports how many documents this node's index holds, which is what the
// fan-out floor is decided against.
func (x *Indexer) Corpus(ctx context.Context) (int, error) {
	stats, err := x.corpus(ctx)
	if err != nil {
		return 0, err
	}
	return stats.Docs, nil
}
