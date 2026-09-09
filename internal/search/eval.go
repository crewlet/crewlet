package search

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
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
// The queries are held-out DOCUMENTS from the corpus itself. A synthetic query
// vector would be a draw from a distribution nobody measured; a document is a
// point the corpus actually contains, which is the same shape a real query
// lands in after the embedding model has read it.

// EvalOptions is one evaluation run.
type EvalOptions struct {
	// Model and Dim are the embedding space to evaluate. Empty takes the
	// most populated pair in the table, which is what a company that has
	// never changed model has exactly one of.
	Model string
	Dim   int

	// Queries is how many held-out documents to use. Zero takes
	// [EvalQueries].
	Queries int

	// Limit is the depth recall is measured at. Zero takes [ReturnDepth],
	// which is the depth the shipped configuration returns.
	Limit int

	// Candidates is the stage-1 depth. Zero takes [Stage1Depth].
	Candidates int
}

// EvalQueries is how many documents an evaluation samples by default.
//
// TWENTY-FIVE, which puts the standard error of a recall near 0.97 at about
// 0.003 — an order of magnitude under the margin between a healthy curve and
// the declared floor, so a run is a measurement rather than a coin flip. It is
// also 25 full scans of the corpus, which is what bounds how long this takes:
// at the measured coefficient that is ≈ 25 seconds at 740 000 sources.
const EvalQueries = 25

// EvalReport is what one evaluation found.
type EvalReport struct {
	// Model, Dim and Sources describe the corpus that was measured.
	Model   string
	Dim     int
	Sources int

	// Queries is how many held-out documents were used.
	Queries int

	// Limit and Candidates are the depths measured at.
	Limit, Candidates int

	// Recall is the mean fraction of the exact top-K the two-stage search
	// returned.
	Recall float64

	// Floor is what this corpus size is expected to clear.
	Floor float64

	// HeadMisses is how many documents in the exact top TEN the candidate
	// pool dropped, over every query.
	//
	// REPORTED BESIDE THE AGGREGATE and never folded into it: a recall of
	// 0.98 is compatible with losing exactly the documents that matter,
	// and a semantic-only document the first stage drops leaves the fused
	// answer entirely rather than sliding down it.
	HeadMisses int

	// WorstQuery is the lowest recall any single query scored, which is
	// what a mean of 0.97 can hide.
	WorstQuery float64

	// MeanPairCos is the corpus's own mean pairwise cosine — the one
	// parameter the seeded fixture's floor is most sensitive to, and what
	// a fitting run re-derives the pinned constant from.
	MeanPairCos float64
}

// Passed reports whether the measured recall clears the floor for this corpus
// size, with no head misses.
//
// BOTH, because either alone is passable while the search is not: an aggregate
// above the floor with documents missing from the first ten ranks is exactly
// the failure the head-miss count exists to name.
func (r EvalReport) Passed() bool { return r.Recall >= r.Floor && r.HeadMisses == 0 }

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
	rep.Floor = FloorAt(rep.Sources)

	queries, err := sampleVectors(ctx, tx, rep.Model, rep.Dim, rep.Queries)
	if err != nil {
		return EvalReport{}, err
	}
	if len(queries) == 0 {
		return EvalReport{}, fmt.Errorf("search: no query documents could be " +
			"sampled from the corpus")
	}
	rep.Queries = len(queries)
	rep.WorstQuery = 1

	for _, query := range queries {
		want, err := exactTop(ctx, tx, query, rep.Model, rep.Dim, rep.Limit)
		if err != nil {
			return EvalReport{}, err
		}
		hits, err := Semantic(ctx, tx, SemanticQuery{
			Vector: query, Model: rep.Model, Dim: rep.Dim,
			Limit: rep.Limit, Candidates: rep.Candidates,
		})
		if err != nil {
			return EvalReport{}, err
		}
		got := map[string]bool{}
		for _, h := range hits {
			got[Key(h.Source, h.ID)] = true
		}
		found := 0
		for rank, key := range want {
			if got[key] {
				found++
				continue
			}
			if rank < 10 {
				rep.HeadMisses++
			}
		}
		if len(want) == 0 {
			continue
		}
		recall := float64(found) / float64(len(want))
		rep.Recall += recall
		rep.WorstQuery = math.Min(rep.WorstQuery, recall)
	}
	rep.Recall /= float64(rep.Queries)

	rep.MeanPairCos, err = meanPairCosine(ctx, tx, rep.Model, rep.Dim, queries)
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

// sampleVectors takes n documents SPREAD ACROSS the corpus.
//
// Deterministically, by taking every (total/n)th row in primary-key order
// rather than by a random sample: two runs on one store must be comparable, and
// a random sample makes a difference between two runs indistinguishable from a
// difference between two samples.
func sampleVectors(ctx context.Context, tx *sql.Tx, model string, dim, n int) ([][]byte, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT embedding FROM (
			SELECT embedding, ROW_NUMBER() OVER (ORDER BY source, source_id) AS rn,
			       COUNT(*) OVER () AS total
			FROM kb_vectors
			WHERE model = ? AND dim = ? AND embedding IS NOT NULL
		)
		WHERE rn % MAX(total / ?, 1) = 0
		LIMIT ?`, model, dim, n, n)
	if err != nil {
		return nil, fmt.Errorf("search: sample query documents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out [][]byte
	for rows.Next() {
		var vector []byte
		if err := rows.Scan(&vector); err != nil {
			return nil, err
		}
		out = append(out, vector)
	}
	return out, rows.Err()
}

// exactTop is the ground truth: one full scan of the wide table, no candidate
// pool at all.
func exactTop(ctx context.Context, tx *sql.Tx, query []byte, model string, dim, limit int) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT source, source_id
		FROM kb_vectors
		WHERE model = ? AND dim = ? AND length(embedding) = ?
		ORDER BY vector_distance_cos(embedding, ?), source, source_id
		LIMIT ?`, model, dim, 4*dim, query, limit)
	if err != nil {
		return nil, fmt.Errorf("search: the exact scan: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var source, id string
		if err := rows.Scan(&source, &id); err != nil {
			return nil, err
		}
		out = append(out, Key(Source(source), id))
	}
	return out, rows.Err()
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
