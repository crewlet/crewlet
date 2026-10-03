package search

import (
	"container/heap"
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Evaluating the semantic half against a COMPANY'S OWN VECTORS.
//
// # Why the deterministic gate cannot answer this
//
// The gate in this package's tests measures the ARITHMETIC — that an exact
// rerank over a 1-bit candidate pool recovers the exact ranking at sufficient
// depth — on a seeded fixture. It cannot measure recall on a particular
// company's documents, and no figure anywhere in this engine does: a sign code
// keeps only each vector's orthant, and how much an orthant says about cosine
// rank is a property of the corpus's own distribution and of nothing else.
// Over a family of embedding-shaped generators at one corpus size the same
// arithmetic spans 0.29 to 0.98.
//
// So the operator's own vectors are the only thing that settles it, and this
// is what asks them.
//
// # Nobody authors a judgement
//
// The ground truth is the EXACT f32 scan's own top-K over the same filtered
// set. That is the answer the two-stage search is approximating, so it is the
// only honest reference — and it removes the step where an evaluation depends
// on somebody writing down what the right answer was, which is the step that
// makes an evaluation stop being run.
//
// The queries are DOCUMENTS from the corpus itself, each measured against the
// rest of it (see below). A synthetic query vector would be a draw from a
// distribution nobody measured; a document is a point the corpus actually
// contains, which is the same shape a real query lands in after the embedding
// model has read it.
//
// # An index is measured TWICE: what searches run, and the scan it replaces
//
// When the partition has an index (ADR-0028), every query runs through it —
// that is the recall the company actually gets, and the one [EvalReport.Passed]
// judges — and again with the full scan as its first stage, so the report says
// what the index COSTS in recall against the exact first stage it
// approximates. A report of the index alone could not tell a corpus the 1-bit
// codes fail from an index that fails a corpus the codes serve, and the two
// have different remedies.
//
// # And in every SHAPE a search is issued in
//
// Every search the engine issues is narrowed — to a source, and often to the
// containers a knowledge scope names — and a narrowed search reads other lists
// than an unfiltered one ([ProbeCount]). So each query document is measured
// unfiltered, narrowed to each source, and narrowed to its own source and
// container ([QueryShape]), each against the exact top-K of the rows that shape
// searches and the floor for how many there are; the evaluation passes only
// if every shape does.
//
// # The query document is never its own answer
//
// A query is a document the corpus holds, and every first stage finds a
// document from its own code at distance zero — so counting it would add a
// free hit to every query's recall and hide one of the top ten from the
// head-miss count. The ground truth is the exact top-K of the REST of the
// shape's rows, which is what a search for anything but that document is
// answered from, and what the floor curve was fitted on: queries the corpus
// does not contain.

// EvalOptions is one evaluation run.
type EvalOptions struct {
	// Model and Dim are the embedding space to evaluate. Empty takes the
	// most populated pair in the table, which is what a company that has
	// never changed model has exactly one of.
	Model string
	Dim   int

	// Queries is how many query documents to use. Zero takes
	// [EvalQueries].
	Queries int

	// Limit is the depth recall is measured at. Zero takes [ReturnDepth],
	// which is the depth the shipped configuration returns.
	Limit int

	// Candidates is the stage-1 depth. Zero takes [Stage1Depth].
	Candidates int

	// Probes overrides how many lists the index is probed at, to measure
	// what another count would buy. Zero is the index's own.
	Probes int
}

// EvalQueries is how many documents an evaluation samples by default.
//
// TWENTY-FIVE, which puts the standard error of a recall near 0.97 at about
// 0.003 — an order of magnitude under the margin between a healthy curve and
// the declared floor, so a run is a measurement rather than a coin flip. The
// ground truth for all of them is ONE pass over the wide table ([exactTops]):
// every 12 KB row read once with a distance per query, rather than twenty-five
// passes that each read every row. The index's own training measures on the
// same count, so an index it installs is judged on the evidence an evaluation
// uses.
const EvalQueries = 25

// EvalReport is what one evaluation found.
type EvalReport struct {
	// Model, Dim and Sources describe the corpus that was measured.
	Model   string
	Dim     int
	Sources int

	// Queries is how many query documents were used.
	Queries int

	// Limit and Candidates are the depths measured at.
	Limit, Candidates int

	// Stage1 is the first stage the unfiltered searches ran: the
	// partition's index, or the full scan and why.
	Stage1 Stage1Report

	// Index is the partition's index as its head records it, nil when it
	// has never had one.
	Index *IndexSummary

	// Recall is the mean fraction of the exact top-K the unfiltered
	// two-stage search returned, through the first stage searches actually
	// run.
	Recall float64

	// Floor is what this corpus size is expected to clear.
	Floor float64

	// HeadMisses is how many documents in the exact top TEN the candidate
	// pool dropped, over every unfiltered query.
	//
	// REPORTED BESIDE THE AGGREGATE and never folded into it: a recall of
	// 0.98 is compatible with losing exactly the documents that matter,
	// and a semantic-only document the first stage drops leaves the fused
	// answer entirely rather than sliding down it.
	HeadMisses int

	// WorstQuery is the lowest recall any single query scored, which is
	// what a mean of 0.97 can hide.
	WorstQuery float64

	// ScanRecall and ScanHeadMisses are the same measurement with the FULL
	// SCAN as the first stage — the exact first stage an index
	// approximates. Equal to Recall and HeadMisses when the searches
	// scanned anyway.
	ScanRecall     float64
	ScanHeadMisses int

	// Shapes are the NARROWED searches' measurements, each judged against
	// its own floor ([ShapeReport]).
	Shapes []ShapeReport

	// MeanPairCos is the corpus's own mean pairwise cosine — the one
	// parameter the seeded fixture's floor is most sensitive to, and what
	// a fitting run re-derives the pinned constant from.
	MeanPairCos float64
}

// ShapeReport is one narrowed shape's measurement.
type ShapeReport struct {
	Shape  QueryShape
	Source Source

	// Queries is how many queries measured it, and Scanned how many of
	// those ran the full scan because their filter matched too few rows
	// for the index within its ceiling.
	Queries, Scanned int

	// Recall, WorstQuery and HeadMisses are measured through the first
	// stage searches run, and ScanRecall and ScanHeadMisses with the full
	// scan as the first stage. Floor is the mean of each query's floor for
	// how many rows its filter keeps.
	Recall, Floor, WorstQuery float64
	HeadMisses                int
	ScanRecall                float64
	ScanHeadMisses            int
}

// Passed reports whether the shape met its floor with no head miss.
func (s ShapeReport) Passed() bool { return s.Recall >= s.Floor && s.HeadMisses == 0 }

// IndexSummary is the partition's index as an evaluation reports it.
type IndexSummary struct {
	// Generation is the packed position of the record that installed it.
	Generation int64
	Model      string
	Dim        int
	// Lists is zero for a verdict that the partition has none; Why says
	// which verdict.
	Lists, Probes int
	TrainedOn     int
	// Measurement is the latest the head records — the training's or a
	// later one — nil when nothing was measured.
	Measurement *Measurement
	Why         IndexVerdict
}

// InSpace reports whether the index describes the evaluated space.
func (x IndexSummary) InSpace(r EvalReport) bool {
	return x.Model == r.Model && x.Dim == r.Dim
}

// Passed reports whether the measured recall clears the floor for this corpus
// size, with no head misses, unfiltered and in every narrowed shape.
//
// BOTH, because either alone is passable while the search is not: an aggregate
// above the floor with documents missing from the first ten ranks is exactly
// the failure the head-miss count exists to name. And it judges what searches
// RUN — through the index when there is one — because that is the recall the
// company gets; the scan's figure beside it is the diagnosis, not the verdict.
func (r EvalReport) Passed() bool {
	if r.Recall < r.Floor || r.HeadMisses != 0 {
		return false
	}
	for _, shape := range r.Shapes {
		if !shape.Passed() {
			return false
		}
	}
	return true
}

// Eval measures the two-stage search against the exact scan, on the vectors
// this store actually holds.
func Eval(ctx context.Context, tx *sql.Tx, opts EvalOptions) (EvalReport, error) {
	rep := EvalReport{
		Model: opts.Model, Dim: opts.Dim,
		Queries:    cmpOr(opts.Queries, EvalQueries),
		Limit:      cmpOr(opts.Limit, ReturnDepth),
		Candidates: cmpOr(opts.Candidates, Stage1Depth),
	}
	if rep.Model == "" || rep.Dim <= 0 {
		model, dim, n, err := largestSpace(ctx, tx)
		if err != nil {
			return EvalReport{}, err
		}
		rep.Model, rep.Dim, rep.Sources = model, dim, n
	}
	if rep.Sources == 0 {
		n, err := countSpace(ctx, tx, rep.Model, rep.Dim)
		if err != nil {
			return EvalReport{}, err
		}
		rep.Sources = n
	}
	if rep.Sources == 0 {
		return EvalReport{}, fmt.Errorf("search: this store holds no vectors "+
			"for %s at %d dimensions — an evaluation needs the corpus it is "+
			"about", rep.Model, rep.Dim)
	}
	head, indexed, err := readIndexHead(ctx, tx)
	if err != nil {
		return EvalReport{}, err
	}
	if indexed {
		rep.Index = &IndexSummary{
			Generation: head.Generation, Model: head.Model, Dim: head.Dim,
			Lists: head.Lists, Probes: head.Probes, TrainedOn: head.TrainedOn,
			Measurement: head.Measurement, Why: head.Why,
		}
	}

	docs, err := sampleDocuments(ctx, tx, rep.Model, rep.Dim, rep.Queries)
	if err != nil {
		return EvalReport{}, err
	}
	if len(docs) == 0 {
		return EvalReport{}, fmt.Errorf("search: no query documents could be " +
			"sampled from the corpus")
	}
	rep.Queries = len(docs)
	shapes := make([][]ShapeQuery, len(docs))
	for i, doc := range docs {
		shapes[i] = shapesFor(doc, Sources)
	}
	tops, err := exactTops(ctx, tx, docs, shapes, rep.Model, rep.Dim, rep.Limit, unwatched)
	if err != nil {
		return EvalReport{}, err
	}

	// ONE PASS PER FIRST STAGE over every (query, shape): what searches
	// run, and — when that is an index — the full scan beside it.
	type tally struct {
		report   ShapeReport
		measured int
	}
	tallies := map[ShapeFilter]*tally{}
	var order []ShapeFilter
	run := func(fullScan bool) error {
		for i, doc := range docs {
			for j, shape := range shapes[i] {
				want := tops[i][j]
				if len(want.Keys) == 0 {
					continue
				}
				// ONE MORE AT EACH STAGE, and the query document dropped
				// from what comes back: it is found at distance zero by
				// every first stage and takes a slot a search for anything
				// but itself would fill with another document.
				q := SemanticQuery{
					Vector: doc.Vector, Model: rep.Model, Dim: rep.Dim,
					Limit: rep.Limit + 1, Candidates: rep.Candidates + 1,
					Probes: opts.Probes, FullScan: fullScan,
				}
				if shape.Shape != ShapeAll {
					q.Sources = []Source{shape.Source}
				}
				if shape.Shape == ShapeContainer {
					q.Containers = []string{shape.Container}
				}
				hits, report, searchErr := Semantic(ctx, tx, q)
				if searchErr != nil {
					return searchErr
				}
				self := Key(doc.Source, doc.ID)
				got := make(map[string]bool, len(hits))
				for _, h := range hits {
					if key := Key(h.Source, h.ID); key != self && len(got) < rep.Limit {
						got[key] = true
					}
				}
				found, head := 0, 0
				for rank, key := range want.Keys {
					if got[key] {
						found++
					} else if rank < 10 {
						head++
					}
				}
				recall := float64(found) / float64(len(want.Keys))
				key := ShapeFilter{Shape: shape.Shape, Source: shape.Source}
				t := tallies[key]
				if t == nil {
					t = &tally{report: ShapeReport{Shape: shape.Shape,
						Source: shape.Source, WorstQuery: 1}}
					tallies[key] = t
					order = append(order, key)
				}
				r := &t.report
				if fullScan {
					r.ScanRecall += recall
					r.ScanHeadMisses += head
					continue
				}
				if shape.Shape == ShapeAll && rep.Stage1.Method == "" {
					rep.Stage1 = report
				}
				if report.Method == Stage1Scan && report.Why == ScanFiltered {
					r.Scanned++
				}
				t.measured++
				r.Recall += recall
				r.WorstQuery = min(r.WorstQuery, recall)
				r.HeadMisses += head
				r.Floor += FloorAt(want.Matching)
			}
		}
		return nil
	}
	if err = run(false); err != nil {
		return EvalReport{}, err
	}
	indexed = rep.Stage1.Method == Stage1IVF
	if indexed {
		if err = run(true); err != nil {
			return EvalReport{}, err
		}
	}
	for _, key := range order {
		t := tallies[key]
		r := t.report
		if t.measured > 0 {
			r.Queries = t.measured
			r.Recall /= float64(t.measured)
			r.Floor /= float64(t.measured)
			r.ScanRecall /= float64(t.measured)
		}
		if !indexed {
			r.ScanRecall, r.ScanHeadMisses = r.Recall, r.HeadMisses
		}
		if key.Shape == ShapeAll {
			rep.Recall, rep.Floor, rep.HeadMisses = r.Recall, r.Floor, r.HeadMisses
			rep.WorstQuery = r.WorstQuery
			rep.ScanRecall, rep.ScanHeadMisses = r.ScanRecall, r.ScanHeadMisses
			continue
		}
		rep.Shapes = append(rep.Shapes, r)
	}
	if rep.Floor == 0 {
		rep.Floor = FloorAt(rep.Sources)
	}

	vectors := make([][]byte, len(docs))
	for i, doc := range docs {
		vectors[i] = doc.Vector
	}
	rep.MeanPairCos, err = meanPairCosine(ctx, tx, rep.Model, rep.Dim, vectors)
	if err != nil {
		return EvalReport{}, err
	}
	return rep, nil
}

func cmpOr(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}

// largestSpace is the (model, width) pair the corpus mostly holds.
//
// THE MOST POPULATED PAIR rather than an arbitrary one, because a corpus
// mid-refill holds two and evaluating the one with nine rows in it would
// report a number about nothing.
func largestSpace(ctx context.Context, tx *sql.Tx) (string, int, int, error) {
	var model string
	var dim, n int
	err := tx.QueryRowContext(ctx, `
		SELECT model, dim, COUNT(*) AS n
		FROM kb_vectors
		WHERE embedding IS NOT NULL
		GROUP BY model, dim
		ORDER BY n DESC, model
		LIMIT 1`).Scan(&model, &dim, &n)
	if err == sql.ErrNoRows {
		return "", 0, 0, fmt.Errorf("search: this store holds no vectors at all")
	}
	if err != nil {
		return "", 0, 0, fmt.Errorf("search: find the corpus's embedding space: %w", err)
	}
	return model, dim, n, nil
}

func countSpace(ctx context.Context, tx *sql.Tx, model string, dim int) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM kb_vectors
		WHERE model = ? AND dim = ? AND embedding IS NOT NULL`,
		model, dim).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("search: count the corpus: %w", err)
	}
	return n, nil
}

// sampledDoc is one query document: its key, its container and its vector.
type sampledDoc struct {
	Source    Source
	ID        string
	Container string
	Vector    []byte
}

// sampleDocuments takes n documents SPREAD ACROSS the corpus.
//
// Deterministically, by taking every (total/n)th row in primary-key order
// rather than by a random sample: two runs on one store must be comparable, and
// a random sample makes a difference between two runs indistinguishable from a
// difference between two samples.
//
// THE ROWS ARE NUMBERED ON THE NARROW TABLE and only the chosen ones read from
// the wide one. Numbered over kb_vectors itself, the window carried every
// row's 12 KB vector through the sort to keep twenty-five of them — measured
// at about 230 µs a source, three times the exact pass it feeds, and the
// largest term of a training's reading. The narrow table holds the same keys
// in the same space, written by the same apply.
func sampleDocuments(ctx context.Context, tx *sql.Tx, model string, dim, n int) ([]sampledDoc, error) {
	rows, err := tx.QueryContext(ctx, sampleStatement, model, dim, n, n)
	if err != nil {
		return nil, fmt.Errorf("search: sample query documents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []sampledDoc
	for rows.Next() {
		var d sampledDoc
		var source string
		if err := rows.Scan(&source, &d.ID, &d.Container, &d.Vector); err != nil {
			return nil, err
		}
		d.Source = Source(source)
		out = append(out, d)
	}
	return out, rows.Err()
}

// sampleStatement numbers the space's keys on the narrow table and reads the
// chosen documents' vectors from the wide one by primary key. Bound: the
// model, the width, and the sample size twice.
const sampleStatement = `
	SELECT v.source, v.source_id, v.container, v.embedding
	FROM (
		SELECT source, source_id,
		       ROW_NUMBER() OVER (ORDER BY source, source_id) AS rn,
		       COUNT(*) OVER () AS total
		FROM kb_vectors_bin
		WHERE model = ? AND dim = ?
	) AS s
	JOIN kb_vectors v ON v.source = s.source AND v.source_id = s.source_id
	WHERE s.rn % MAX(s.total / ?, 1) = 0 AND v.embedding IS NOT NULL
	ORDER BY s.rn
	LIMIT ?`

// ShapeQuery is one query shape as SQL filters it: the shape, and the source
// and container it narrows to when it does.
type ShapeQuery struct {
	Shape     QueryShape
	Source    Source
	Container string
}

// matches reports whether a row of the given source and container is one the
// shape's search can return.
func (q ShapeQuery) matches(source Source, container string) bool {
	switch q.Shape {
	case ShapeSource:
		return source == q.Source
	case ShapeContainer:
		return source == q.Source && container == q.Container
	}
	return true
}

// shapesFor is every shape a query document is measured in: unfiltered,
// narrowed to each source, and narrowed to its own source and container — the
// last only when it has one, since no search is scoped to "no container".
func shapesFor(doc sampledDoc, sources []Source) []ShapeQuery {
	out := []ShapeQuery{{Shape: ShapeAll}}
	for _, source := range sources {
		out = append(out, ShapeQuery{Shape: ShapeSource, Source: source})
	}
	if doc.Container != "" {
		out = append(out, ShapeQuery{Shape: ShapeContainer, Source: doc.Source,
			Container: doc.Container})
	}
	return out
}

// ExactTop is one (query, shape)'s ground truth: the exact top keys, best
// first, and how many rows the shape searches — neither counting the query
// document itself.
type ExactTop struct {
	Keys     []string
	Matching int
}

// exactTops is the ground truth for every (query, shape) at once: the exact
// f32 top limit of each over the rows its shape searches, as [Key]s, best
// first, never counting the query document itself.
//
// # One pass, not one per query
//
// The wide table's rows are 12 KB, and reading them is the whole cost of an
// exact scan — the distance is a few microseconds beside the overflow pages.
// So every row is read ONCE with a distance per query, and a bounded heap per
// (query, shape) keeps its top. The order is the one the per-query statement
// this replaces declared — distance, then source, then source id, compared as
// the database compares text — so the answer is the same answer.
//
// It is the one ground truth in the package: the evaluation and the index's
// own training both measure against it, so an index a training installs is
// judged by the reference an operator's evaluation uses. It tells advanced
// every [progressStride] rows it reads, which a training's bound measures it
// by ([Budget]).
func exactTops(ctx context.Context, tx *sql.Tx, queries []sampledDoc, shapes [][]ShapeQuery, model string, dim, limit int, advanced func()) ([][]ExactTop, error) {
	if len(queries) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(queries)+3)
	for _, q := range queries {
		args = append(args, q.Vector)
	}
	args = append(args, model, dim, 4*dim)
	rows, err := tx.QueryContext(ctx, exactTopsStatement(len(queries)), args...)
	if err != nil {
		return nil, fmt.Errorf("search: the exact scan: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tops := make([][]*exactHeap, len(queries))
	out := make([][]ExactTop, len(queries))
	for i := range queries {
		tops[i] = make([]*exactHeap, len(shapes[i]))
		out[i] = make([]ExactTop, len(shapes[i]))
		for j := range tops[i] {
			tops[i][j] = &exactHeap{}
		}
	}
	distances := make([]float64, len(queries))
	dest := make([]any, 3+len(queries))
	var source, id, container string
	dest[0], dest[1], dest[2] = &source, &id, &container
	for i := range distances {
		dest[3+i] = &distances[i]
	}
	for scanned := 1; rows.Next(); scanned++ {
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		if scanned%progressStride == 0 {
			advanced()
		}
		for i, d := range distances {
			if queries[i].Source == Source(source) && queries[i].ID == id {
				continue
			}
			for j, shape := range shapes[i] {
				if !shape.matches(Source(source), container) {
					continue
				}
				out[i][j].Matching++
				tops[i][j].offer(exactRow{distance: d, source: source, id: id}, limit)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: the exact scan: %w", err)
	}
	for i := range tops {
		for j, top := range tops[i] {
			ranked := append([]exactRow(nil), *top...)
			sort.Slice(ranked, func(a, b int) bool { return ranked[a].before(ranked[b]) })
			keys := make([]string, len(ranked))
			for k, r := range ranked {
				keys[k] = Key(Source(r.source), r.id)
			}
			out[i][j].Keys = keys
		}
	}
	return out, nil
}

// exactTopsStatement is [exactTops]' one pass for n queries: every row of the
// space once, with what a shape filters on and a distance per query. Bound:
// the n query vectors, then the model, the width and the byte length.
func exactTopsStatement(n int) string {
	columns := make([]string, n)
	for i := range columns {
		columns[i] = "vector_distance_cos(embedding, ?)"
	}
	return `
		SELECT source, source_id, container, ` + strings.Join(columns, ", ") + `
		FROM kb_vectors
		WHERE model = ? AND dim = ? AND length(embedding) = ?`
}

// exactRow is one document's exact distance from one query.
type exactRow struct {
	distance   float64
	source, id string
}

// before is the exact scan's declared order: distance, source, source id.
func (r exactRow) before(o exactRow) bool {
	if r.distance != o.distance {
		return r.distance < o.distance
	}
	if r.source != o.source {
		return r.source < o.source
	}
	return r.id < o.id
}

// exactHeap keeps the best limit rows seen, with the WORST at the root so the
// next better row displaces it.
type exactHeap []exactRow

func (h exactHeap) Len() int           { return len(h) }
func (h exactHeap) Less(a, b int) bool { return h[b].before(h[a]) }
func (h exactHeap) Swap(a, b int)      { h[a], h[b] = h[b], h[a] }
func (h *exactHeap) Push(x any)        { *h = append(*h, x.(exactRow)) }
func (h *exactHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

func (h *exactHeap) offer(r exactRow, limit int) {
	if limit <= 0 {
		return
	}
	if h.Len() < limit {
		heap.Push(h, r)
		return
	}
	if !r.before((*h)[0]) {
		return
	}
	(*h)[0] = r
	heap.Fix(h, 0)
}

// meanPairCosine is the corpus's own mean pairwise cosine, over the sampled
// documents against a bounded slice of the corpus.
//
// IT IS THE ONE PARAMETER THE FIXTURE'S FLOOR IS MOST SENSITIVE TO: at a fixed
// spectrum, recall at the shipped oversample rises 0.6360 → 0.9832 as this
// value rises 0 → 0.50. A generator pinned at a mean the real corpus is
// nowhere near is a gate whose floor describes a different corpus, which is
// what `-fit` re-derives it from.
func meanPairCosine(ctx context.Context, tx *sql.Tx, model string, dim int, queries [][]byte) (float64, error) {
	// A BOUNDED SLICE rather than every pair: the quantity is a mean, and
	// N × 400 pairs estimate it to well inside the precision anything reads
	// it at, where every pair is O(N²) full-vector distances.
	const against = 400
	var sum float64
	var pairs int
	for _, query := range queries {
		rows, err := tx.QueryContext(ctx, `
			SELECT vector_distance_cos(embedding, ?)
			FROM kb_vectors
			WHERE model = ? AND dim = ? AND length(embedding) = ?
			ORDER BY source, source_id
			LIMIT ?`, query, model, dim, 4*dim, against)
		if err != nil {
			return 0, fmt.Errorf("search: measure the corpus's mean cosine: %w", err)
		}
		for rows.Next() {
			var distance float64
			if err := rows.Scan(&distance); err != nil {
				_ = rows.Close()
				return 0, err
			}
			// A DISTANCE OF ZERO IS THE DOCUMENT ITSELF, which every
			// sample meets exactly once and which would pull the mean
			// towards a similarity nothing else in the corpus has.
			if distance == 0 {
				continue
			}
			sum += 1 - distance
			pairs++
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return 0, err
		}
		_ = rows.Close()
	}
	if pairs == 0 {
		return 0, nil
	}
	return sum / float64(pairs), nil
}

// SpacesIn is every (model, width) pair the corpus holds, largest first — what
// an operator reads to see a refill in progress.
func SpacesIn(ctx context.Context, tx *sql.Tx) ([]Space, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT model, dim, COUNT(*) FROM kb_vectors
		WHERE embedding IS NOT NULL
		GROUP BY model, dim`)
	if err != nil {
		return nil, fmt.Errorf("search: list the corpus's embedding spaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Space
	for rows.Next() {
		var s Space
		if err := rows.Scan(&s.Model, &s.Dim, &s.Sources); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sources != out[j].Sources {
			return out[i].Sources > out[j].Sources
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}

// Space is one embedding space the corpus holds.
type Space struct {
	Model   string
	Dim     int
	Sources int
}
