package search

import (
	"context"
	"database/sql"
	"encoding/binary"
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

	// Candidates is the stage-1 depth. Zero takes [Stage1Depth], and one
	// below Limit is raised to it, as [Semantic] does.
	Candidates int
}

// EvalQueries is how many documents an evaluation samples by default.
//
// TWENTY-FIVE, which puts the standard error of a recall near 0.97 at about
// 0.003 — an order of magnitude under the margin between a healthy curve and
// the declared floor, so a run is a measurement rather than a coin flip. It is
// also what bounds how long a run takes: each query is one exact scan of the
// wide table and one two-stage search.
const EvalQueries = 25

// EvalReport is what one evaluation found.
type EvalReport struct {
	// Model, Dim and Sources describe the corpus that was measured.
	Model   string
	Dim     int
	Sources int

	// Queries is how many held-out documents were used.
	Queries int

	// Limit and Candidates are the depths measured at — the ones the
	// search ran at, which is not always what [EvalOptions] asked for.
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
	//
	// AN ESTIMATE, over every pair of a held-out query and a document from
	// a second sample of the corpus, both spread across its whole key order
	// — see meanPairCosine for the sample and why its size is enough.
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
		Queries: cmpOr(opts.Queries, EvalQueries),
	}
	rep.Limit, rep.Candidates = semanticDepths(opts.Limit, opts.Candidates)
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

	queries, err := strided(ctx, tx, rep.Model, rep.Dim, rep.Queries)
	if err != nil {
		return EvalReport{}, fmt.Errorf("search: sample query documents: %w", err)
	}
	if len(queries) == 0 {
		return EvalReport{}, fmt.Errorf("search: no query documents could be " +
			"sampled from the corpus")
	}
	rep.Queries = len(queries)
	rep.WorstQuery = 1

	for _, query := range queries {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		want, err := exactTop(ctx, tx, query.vector, rep.Model, rep.Dim, rep.Limit)
		if err != nil {
			return EvalReport{}, err
		}
		hits, err := Semantic(ctx, tx, SemanticQuery{
			Vector: query.vector, Model: rep.Model, Dim: rep.Dim,
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

	against, err := strided(ctx, tx, rep.Model, rep.Dim, meanCosineAgainst)
	if err != nil {
		return EvalReport{}, fmt.Errorf("search: sample the corpus's mean "+
			"cosine: %w", err)
	}
	rep.MeanPairCos, err = meanPairCosine(queries, against, rep.Dim)
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
		-- CHUNK 0 counts DOCUMENTS, which is what "the corpus" means to
		-- everything downstream: the recall floor is a curve per corpus
		-- SIZE, and counting windows would read a corpus of long pages
		-- as several times larger than it is and hold it to a floor
		-- meant for a corpus it is not.
		WHERE embedding IS NOT NULL AND chunk = 0
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
		WHERE model = ? AND dim = ? AND embedding IS NOT NULL AND chunk = 0`,
		model, dim).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("search: count the corpus: %w", err)
	}
	return n, nil
}

// sampled is one document a [strided] sample took: its key, which is what
// tells it apart from the documents it is compared with, and its first
// window's vector.
type sampled struct {
	key    string
	vector []byte
}

// strided takes n documents SPREAD EVENLY across the corpus, or every one when
// it holds fewer.
//
// Deterministically, in primary-key order rather than by a random sample: two
// runs on one store must be comparable, and a random sample makes a difference
// between two runs indistinguishable from a difference between two samples.
//
// EVENLY rather than every (total/n)th row. An integer stride truncates: at
// 799 documents and a sample of 400 it is 1, which answers the first 400 in key
// order — and the key leads with the source, so that is every page before any
// task. Row r (from 1) is taken exactly when ⌊r·n/total⌋ steps past
// ⌊(r−1)·n/total⌋, which it does n times at a spacing of total/n however the
// two divide, and at every row when total is below n.
//
// THE SPACING IS DECIDED OVER KEYS, and only the n vectors it picks are read,
// by primary key. The window numbers every chunk-0 row of the space before the
// spacing filter keeps any of them, so a vector carried through it is a
// vector copied for every document the corpus holds and then thrown away.
// Measured at 20 000 documents of 1 024 dimensions: a sample of 25 took
// 0.64–0.74 s with the vector in the window and 0.09 s without it, where one
// exact scan of the same table took 0.08 s.
func strided(ctx context.Context, tx *sql.Tx, model string, dim, n int) ([]sampled, error) {
	type key struct{ source, id string }
	var keys []key
	err := func() error {
		rows, err := tx.QueryContext(ctx, `
			SELECT source, source_id FROM (
				SELECT source, source_id,
				       ROW_NUMBER() OVER (ORDER BY source, source_id) AS rn,
				       COUNT(*) OVER () AS total
				FROM kb_vectors
				-- ONE WINDOW PER DOCUMENT, which is both halves of
				-- this sample: a vector taken from a document's
				-- fourth window is about that section rather than
				-- about the document, and the total this spacing is
				-- computed against has to be the one countSpace
				-- reports.
				WHERE model = ? AND dim = ? AND embedding IS NOT NULL
				  AND chunk = 0
			)
			WHERE (rn * ?) / total > ((rn - 1) * ?) / total`, model, dim, n, n)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var k key
			if err := rows.Scan(&k.source, &k.id); err != nil {
				return err
			}
			keys = append(keys, k)
		}
		return rows.Err()
	}()
	if err != nil {
		return nil, err
	}

	// THE SAME TRANSACTION as the spacing, so every key it chose still has
	// the non-null vector it was chosen for.
	stmt, err := tx.PrepareContext(ctx, `
		SELECT embedding FROM kb_vectors
		WHERE source = ? AND source_id = ? AND chunk = 0`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stmt.Close() }()
	out := make([]sampled, 0, len(keys))
	for _, k := range keys {
		s := sampled{key: Key(Source(k.source), k.id)}
		if err := stmt.QueryRowContext(ctx, k.source, k.id).Scan(&s.vector); err != nil {
			return nil, fmt.Errorf("read the vector of %s: %w", s.key, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// exactTop is the ground truth: one full scan of the wide table, no candidate
// pool at all.
func exactTop(ctx context.Context, tx *sql.Tx, query []byte, model string, dim, limit int) ([]string, error) {
	// A DOCUMENT SCORES AS ITS BEST WINDOW, exactly as the two-stage scan
	// it is the ground truth FOR does. Left ungrouped this would rank
	// windows, so a recall measured against it would compare a list of
	// documents to a list of sections and report a number about neither.
	rows, err := tx.QueryContext(ctx, `
		SELECT source, source_id
		FROM kb_vectors
		WHERE model = ? AND dim = ? AND length(embedding) = ?
		GROUP BY source, source_id
		ORDER BY MIN(vector_distance_cos(embedding, ?)), source, source_id
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

// meanCosineAgainst is how many documents the mean pairwise cosine compares
// each held-out query with.
//
// A SAMPLE rather than every pair, because every pair is O(N²) full-vector
// products for a quantity that is a mean. It is taken by [strided] exactly as
// the queries are, spread across the whole key order, so it is a sample of the
// corpus rather than of whichever source sorts first.
//
// FOUR HUNDRED, sixteen times [EvalQueries], and the ratio is the reason. The
// estimate is a mean over a grid of queries by documents drawn from one corpus
// by one rule, so how close a document tends to sit to the rest enters its row
// of the grid and its column alike: that variance is divided by the query
// count on one axis and by this on the other. At sixteen times the queries
// this axis carries a sixteenth of what the queries do, so a larger sample
// would shrink only that sixteenth.
//
// Its price is a second pass of [strided]'s window over the space's keys, which
// grows with the corpus, and this many vectors read by primary key, which does
// not.
const meanCosineAgainst = 400

// meanPairCosine is the corpus's own mean pairwise cosine, over every pair of a
// query and a sampled document that are not the same document.
//
// IT IS THE ONE PARAMETER THE FIXTURE'S FLOOR IS MOST SENSITIVE TO: at a fixed
// spectrum, recall at the shipped oversample rises 0.6360 → 0.9832 as this
// value rises 0 → 0.50. A generator pinned at a mean the real corpus is
// nowhere near is a gate whose floor describes a different corpus, which is
// what `-fit` re-derives it from.
//
// A DOCUMENT IS NOT PAIRED WITH ITSELF, and it is told apart by its KEY rather
// than by a distance of zero. The two samples are taken by one rule, so at the
// default sizes every query is also in the sample it is compared with; a
// self-pair at cosine one would pull the mean towards a similarity nothing else
// in the corpus has. Two DIFFERENT documents with identical vectors are a
// genuine pair at cosine one, and a zero-distance test would drop them too.
//
// A VECTOR WITH NO LENGTH HAS NO DIRECTION, so it is in no pair: a cosine
// against it is a division by zero. Nothing upstream refuses a provider that
// answers a zero vector — only a non-finite one — so this is reachable.
//
// PURE OVER VALUES, like the rest of this package's arithmetic, so it is
// tested without a store.
func meanPairCosine(queries, against []sampled, dim int) (float64, error) {
	decode := func(s sampled) ([]float64, float64, error) {
		if len(s.vector) != 4*dim {
			return nil, 0, fmt.Errorf("search: %s holds a %d-byte vector in a "+
				"%d-dimension space, which is %d bytes", s.key, len(s.vector),
				dim, 4*dim)
		}
		out := make([]float64, dim)
		var norm float64
		for i := range out {
			out[i] = float64(math.Float32frombits(
				binary.LittleEndian.Uint32(s.vector[4*i:])))
			norm += out[i] * out[i]
		}
		return out, math.Sqrt(norm), nil
	}
	type unpacked struct {
		key    string
		vector []float64
		norm   float64
	}
	unpack := func(in []sampled) ([]unpacked, error) {
		out := make([]unpacked, 0, len(in))
		for _, s := range in {
			vector, norm, err := decode(s)
			if err != nil {
				return nil, err
			}
			if norm == 0 {
				continue
			}
			out = append(out, unpacked{key: s.key, vector: vector, norm: norm})
		}
		return out, nil
	}
	qs, err := unpack(queries)
	if err != nil {
		return 0, err
	}
	as, err := unpack(against)
	if err != nil {
		return 0, err
	}
	var sum float64
	var pairs int
	for _, q := range qs {
		for _, a := range as {
			if q.key == a.key {
				continue
			}
			var dot float64
			for i := range q.vector {
				dot += q.vector[i] * a.vector[i]
			}
			sum += dot / (q.norm * a.norm)
			pairs++
		}
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
		WHERE embedding IS NOT NULL AND chunk = 0
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
