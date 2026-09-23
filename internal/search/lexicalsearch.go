package search

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
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

// maxPostingScan is how many postings a term's FIRST read takes.
//
// IT CHANGES WHAT A QUERY COSTS AND NEVER WHAT IT ANSWERS. [Indexer.Search]
// reads one posting past it to learn the most anything it left out can score,
// resolves exactly every document it reached that could still move, and reads
// a capped term's list WHOLE when a document no read reached could still
// outrank the last hit — so the hits are the exact BM25 top-N at any value of
// this constant.
//
// WHAT IT SAVES IS ROWS, NOT THE SCAN. The read is ordered by an expression
// over a join, which no index serves, so the store reads every posting of the
// term either way; the cap bounds what is handed back and summed.
// Measured on 100 000 documents: a read of a term held by 80 000 of them took
// 165 ms capped against 389 ms whole, and a query of three such terms 0.7 s
// against 1.4 s. Five thousand is therefore a cost trade and nothing else — a
// larger value moves more rows on every query, a smaller one sends more
// queries to the whole read.
//
// A QUERY THE FIRST READS CANNOT SETTLE PAYS TWICE. Six such terms took 4.5 s:
// the first reads and their resolution, then the whole read they could not
// rule out — where reading all six whole from the start took 2.8 s. Three of
// them settled on the first reads. That is what an exact answer costs on the
// queries that need the whole lists, and every other query does not pay it.
//
// WHAT IT LEAVES OUT IS THE BOTTOM OF THE LIST, because the read is ORDERED BY
// THE TERM'S OWN BM25 CONTRIBUTION — see [Indexer.postings] — so every posting
// it did not read scores at most what the first one it left out scores. That
// bound is what lets the first read stop at all.
//
// The order was `ORDER BY p.freq DESC`, which is the same thing only when every
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
//
// THE HITS ARE THE EXACT BM25 TOP-N, whatever [maxPostingScan] is.
//
// # What a capped term costs the ranking, and how it is paid back
//
// A term whose list [maxPostingScan] cut contributes nothing to a document
// past the cut, so read alone the scores are a LOWER bound for exactly those
// documents — and a document past term A's cut that another term did reach
// can lose A's whole contribution and slide below one that kept it. The read
// is ordered by contribution, so each cut also yields an UPPER bound: nothing
// it left out scores more than the first posting it left out.
//
// With both bounds, a document the scan reached is a CONTENDER when its score
// plus the bounds of the capped terms it is missing reaches the last hit's
// score. Contenders are resolved EXACTLY, by one primary-key lookup of the
// missing terms per batch of them, and the top-N is taken again over the
// exact scores. A document the scan reached that is not a contender cannot
// reach the answer, since its bound is below a score that resolution only
// raises. What remains is a document NO read reached, whose score is at most
// the sum of the capped terms' bounds. When that sum reaches the last hit's
// score, the capped terms are read WHOLE and the ranking is taken again over
// complete lists — the cost [maxPostingScan] was saving, paid only by a query
// that needs it.
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

	lists := make([]termList, 0, len(terms))
	for _, term := range terms {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		list, err := x.postings(ctx, term, q, corpus, maxPostingScan)
		if err != nil {
			return nil, err
		}
		lists = append(lists, list)
	}
	ranking := rankLists(lists, corpus)
	if len(ranking.scores) == 0 {
		return nil, nil
	}
	if ranking.capped() {
		for i, ids := range ranking.contenders(limit) {
			//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			found, err := x.lookup(ctx, lists[i].term, ids)
			if err != nil {
				return nil, err
			}
			ranking.resolve(i, ids, found, corpus)
		}
		// AFTER THE RESOLUTION, because it can only raise the last hit's
		// score, and a higher last hit is one fewer query sent to the
		// whole read.
		if ranking.unreached(limit) {
			for i, list := range lists {
				if !list.capped {
					continue
				}
				if lists[i], err = x.postings(ctx, list.term, q, corpus, 0); err != nil {
					return nil, err
				}
			}
			ranking = rankLists(lists, corpus)
		}
	}
	return x.hydrateHits(ctx, q, ranking.scores, terms, limit)
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

// postings reads one term's list, filtered to the query's scope, with the
// term's IDF. A depth above zero reads that many postings and — when the list
// holds more — the bound on what the read left out; zero reads the whole list.
//
// THE DOCUMENT COUNT BEHIND THE IDF IS UNFILTERED, deliberately. It is the term's rarity
// across the whole corpus, which is what makes it a weight; counting only
// within a scope would make the same word rare in a small space and common in
// a large one, so a hit's rank would depend on which container it happened to
// be in rather than on how well it matched.
//
// ORDERED BY THE TERM'S OWN BM25 CONTRIBUTION, which is what makes a read
// that stops at a depth leave out the bottom of the list rather than an
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
// everything and fill the first read's whole depth.
func (x *Indexer) postings(ctx context.Context, term string, q LexicalQuery,
	corpus textindex.Corpus, depth int) (termList, error) {
	list := termList{term: term}
	var total int
	if err := x.db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM kb_postings WHERE term = ?`, term).Scan(&total); err != nil {
		return termList{}, fmt.Errorf("search: count postings for %q: %w", term, err)
	}
	if total == 0 {
		return list, nil
	}
	list.idf = textindex.IDF(corpus.Docs, total)

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
	args = append(args, textindex.B, avg, textindex.B, avg)
	limit := ""
	if depth > 0 {
		// ONE PAST THE DEPTH, which is the evidence it was reached: a
		// list that fills exactly depth was read whole, and one that
		// returns the extra row was not — and that row, being the best
		// posting the read left out, is also the bound on every other
		// one it left out.
		limit = "LIMIT ?"
		args = append(args, depth+1)
	}

	rows, err := x.db.SQL().QueryContext(ctx, `
		SELECT p.doc_id, p.freq, d.length
		  FROM kb_postings p
		  JOIN kb_docs d ON d.id = p.doc_id
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY p.freq /
		          ((1 - ?) * ? + ? * (CASE WHEN d.length > 0 THEN d.length ELSE ? END))
		          DESC, p.doc_id
		 `+limit, args...)
	if err != nil {
		return termList{}, fmt.Errorf("search: read postings for %q: %w", term, err)
	}
	defer rows.Close()
	var out []textindex.Posting
	for rows.Next() {
		var p textindex.Posting
		if err := rows.Scan(&p.DocID, &p.Freq, &p.Length); err != nil {
			return termList{}, fmt.Errorf("search: scan posting for %q: %w", term, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return termList{}, fmt.Errorf("search: read postings for %q: %w", term, err)
	}
	if depth > 0 && len(out) > depth {
		probe := out[depth]
		out = out[:depth]
		list.capped = true
		list.ceiling = textindex.Score(list.idf, probe, corpus) * (1 + ceilingSlack)
	}
	list.postings = out
	return list, nil
}

// lookup reads one term's postings for a set of documents, by primary key.
//
// It is how a capped term's contribution is resolved for the documents that
// could still move: `kb_postings` is keyed (term, doc_id), so each id is one
// seek however long the term's list is — which keeps a resolution's cost in
// proportion to the capped reads that produced its contenders rather than to
// the lists they cut.
//
// THE PLAN IS STATED, because the planner does not find it. Asked for
// `term = ? AND doc_id = ?`, as a join or one id at a time, it seeks
// `kb_postings_doc_idx` on the id and filters on the term, which walks every
// term the document holds: measured over documents of two hundred terms,
// 1 999 ids took 160 ms joined and 350 ms one at a time, against 14 ms
// through the primary key. So the ids drive the join from a VALUES list, and
// INDEXED BY names the primary key's own index — the name SQLite gives a
// table's first automatic index, and one whose absence fails the statement
// when it is prepared rather than slowing it down. An IN list is not used
// either: the plan seeks the term and tests each of its postings against the
// list, which took 330 ms for 1 999 ids against a six-thousand-posting term.
//
// BATCHED to the estate's own parameter limit, less the term's own bind. An id
// with no row does not hold the term.
func (x *Indexer) lookup(ctx context.Context, term string,
	ids []string) ([]textindex.Posting, error) {

	batch := x.db.Caps().MaxVariables - 1
	var out []textindex.Posting
	for len(ids) > 0 {
		chunk := ids[:min(batch, len(ids))]
		ids = ids[len(chunk):]
		args := make([]any, 0, len(chunk)+1)
		for _, id := range chunk {
			args = append(args, id)
		}
		args = append(args, term)
		err := func() error {
			rows, err := x.db.SQL().QueryContext(ctx, `
				WITH wanted(id) AS (VALUES `+valueRows(len(chunk))+`)
				SELECT p.doc_id, p.freq, d.length
				  FROM wanted w
				 CROSS JOIN kb_postings p INDEXED BY sqlite_autoindex_kb_postings_1
				  JOIN kb_docs d ON d.id = p.doc_id
				 WHERE p.term = ? AND p.doc_id = w.id`, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var p textindex.Posting
				if err := rows.Scan(&p.DocID, &p.Freq, &p.Length); err != nil {
					return err
				}
				out = append(out, p)
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, fmt.Errorf("search: resolve %q for %d document(s) its "+
				"capped read left out: %w", term, len(chunk), err)
		}
	}
	return out, nil
}

// valueRows renders n one-column VALUES rows of placeholders.
func valueRows(n int) string {
	return strings.TrimSuffix(strings.Repeat("(?),", n), ",")
}

// termList is one query term's postings as [Indexer.postings] read them.
type termList struct {
	term string
	idf  float64

	// postings are the ones read, in descending contribution.
	postings []textindex.Posting

	// capped says the term has more postings in scope than were read.
	capped bool

	// ceiling bounds the contribution of every posting NOT read: the score
	// of the first one the read left out, widened by [ceilingSlack]. Zero
	// when nothing was left out.
	ceiling float64
}

// ceilingSlack widens a capped term's ceiling, relatively.
//
// The read is ORDERED in SQL and SCORED in Go, by two float computations of
// one monotone function, so two postings whose true order is a near tie can
// come back in the other order and score an ulp or two apart the wrong way. A
// ceiling taken as the probe's own score could then sit below a posting it is
// meant to bound, and [Indexer.Search] would skip the whole read on a bound
// that is not one. One part in a billion is over a million times that
// rounding, and all a wider ceiling can do is resolve a few more documents and
// send a query to the whole read a little sooner.
const ceilingSlack = 1e-9

// ranking is a query's scores, kept per term so a contribution resolved later
// is summed in the same order as the ones read at first.
type ranking struct {
	lists []termList

	// contrib[i] is term i's contribution to each document it is known to
	// hold. A document absent from contrib[i] either was not reached by a
	// capped read of term i, or does not hold it.
	contrib []map[string]float64

	// resolved[i] is the documents whose term-i contribution is now exact
	// whether or not they hold the term.
	resolved []map[string]bool

	scores map[string]float64
}

// rankLists sums the lists' contributions into per-document scores.
func rankLists(lists []termList, corpus textindex.Corpus) *ranking {
	r := &ranking{
		lists:    lists,
		contrib:  make([]map[string]float64, len(lists)),
		resolved: make([]map[string]bool, len(lists)),
		scores:   map[string]float64{},
	}
	for i, list := range lists {
		r.contrib[i] = make(map[string]float64, len(list.postings))
		r.resolved[i] = map[string]bool{}
		for _, p := range list.postings {
			r.contrib[i][p.DocID] = textindex.Score(list.idf, p, corpus)
			r.scores[p.DocID] = 0
		}
	}
	for id := range r.scores {
		r.scores[id] = r.sum(id, false)
	}
	return r
}

// sum is one document's score, added up in TERM ORDER whenever its parts
// became known, so a resolved score is the very number a read with no cap
// would have summed and not one that differs from it in the last place —
// float addition is not associative. With bound set, a capped term the
// document is missing contributes its ceiling instead of nothing.
func (r *ranking) sum(id string, bound bool) float64 {
	total := 0.0
	for i, list := range r.lists {
		if c, ok := r.contrib[i][id]; ok {
			total += c
			continue
		}
		if bound && list.capped && !r.resolved[i][id] {
			total += list.ceiling
		}
	}
	return total
}

func (r *ranking) capped() bool {
	for _, list := range r.lists {
		if list.capped {
			return true
		}
	}
	return false
}

// last is the score of the limit-th document, or zero when fewer are scored.
func (r *ranking) last(limit int) float64 {
	top := topN(r.scores, limit)
	if len(top) < limit {
		return 0
	}
	return r.scores[top[limit-1]]
}

// contenders is, per capped term, the scored documents missing it whose upper
// bound reaches the last hit's score — the ones a resolution can move.
func (r *ranking) contenders(limit int) map[int][]string {
	floor := r.last(limit)
	out := map[int][]string{}
	for id, score := range r.scores {
		upper := r.sum(id, true)
		if upper == score || upper < floor {
			continue
		}
		for i, list := range r.lists {
			if _, ok := r.contrib[i][id]; !ok && list.capped {
				out[i] = append(out[i], id)
			}
		}
	}
	for i := range out {
		sort.Strings(out[i])
	}
	return out
}

// resolve records term i's exact contribution for ids, from the postings the
// lookup found: an id with no posting does not hold the term.
func (r *ranking) resolve(i int, ids []string, found []textindex.Posting,
	corpus textindex.Corpus) {

	for _, p := range found {
		r.contrib[i][p.DocID] = textindex.Score(r.lists[i].idf, p, corpus)
	}
	for _, id := range ids {
		r.resolved[i][id] = true
	}
	for _, id := range ids {
		r.scores[id] = r.sum(id, false)
	}
}

// unreached reports whether a document no read reached could still outrank the
// limit-th scored one: its score is at most the capped terms' ceilings summed,
// and ties go against the answer, since a tie's order is decided by id and the
// unread document's id is unknown. With a term capped and fewer than limit
// documents scored it is always true, since then every document holding a
// term belongs in the answer.
func (r *ranking) unreached(limit int) bool {
	bound := r.unread()
	return bound > 0 && bound >= r.last(limit)
}

// unread is the most a document no read reached can score: the capped terms'
// ceilings, summed. Zero when no term was capped.
func (r *ranking) unread() float64 {
	bound := 0.0
	for _, list := range r.lists {
		if list.capped {
			bound += list.ceiling
		}
	}
	return bound
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
// from this node's own index, which is what makes hydrating a PEER's hit an
// ordinary local read rather than a second round trip.
//
// A key this node's index holds no row for is SKIPPED rather than rendered
// blank, on [Indexer.hydrateHits]'s terms. Two things leave one: the indexer
// removed a document between the scan and this read, and a document that no
// longer exists is not an answer; or this node's index is still on its first
// build and has not reached a document a peer ranked — which is why a search
// on such a node reports itself as building rather than as a whole answer
// ([Indexer.ReadyFor]).
//
// # IT RETURNS [FusedHit], WHICH HAS NO SCORE, AND THAT IS THE POINT
//
// A [LexicalHit] carries the BM25 number one ranker gave one document, which is
// comparable within that ranker and is what the fan-out merges its own slices
// on. What comes back HERE has been through reciprocal rank fusion across two
// rankers and across disjoint slices, so the only thing left is an ORDER — the
// fused number is a sum of reciprocal placements and means nothing beside a
// BM25 score. A score field here could only ever carry a zero that is not a
// value, which is the shape a type has to refuse rather than document: a
// caller cannot read a score that does not exist.
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
