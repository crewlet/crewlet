package search

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// The semantic half, as one statement over two tables in one file.
//
// # The shape, and why it is not an index
//
// There is no approximate-nearest-neighbour index on this driver — `caps.go`
// probes for one on every open and it is absent at the pin — so the honest
// choices were a full exact scan, an embedded search library with its own
// index format, file and backup story, or two-stage retrieval. Two-stage is
// what every production vector engine does anyway (DiskANN's rerank, pgvector's
// documented rerank subquery), and here it needs no index at all: a narrow
// table of sign codes is scanned first and the survivors are reranked exactly
// against the vectors they came from, by primary key.
//
// Measured on this container at 3 072 dimensions, mean of twelve probes at a
// stage-1 depth of 400 — the shipped depth is 1 200, which is a further +7.6 %
// on the first stage and is DERIVED rather than measured:
//
//	N        stage 1 alone   two-stage        the exact f32 scan
//	 10 000   11.1 ms         14.5 ms          75.7 ms
//	 40 000   43.8 ms         55.0 ms         306.8 ms
//	120 000  148.3 ms        150.6 ms         979.2 ms
//
// The rerank is essentially free — 2.3 ms of the 150.6 at the largest size —
// because it is O(candidates) and not O(N).
//
// # Why the query code is computed by a one-row CTE
//
// The first stage orders by `vector_distance_cos(bits, <the query's own sign
// code>)`, and that code has to be in the driver's own encoding or the two
// values are different lengths and the function is undefined. Computing it in
// Go would mean this package reproducing an encoding it can only observe;
// computing it inline in the ORDER BY risks re-evaluating `vector1bit` once
// per row, since nothing marks it constant. A one-row CTE cross-joined into the
// scan is evaluated once and keeps the whole thing one statement.
//
// # Why the container filter is on the NARROW table
//
// It is the only place it does any good. Pushing it onto the wide table would
// filter after the scan that the filter exists to shrink, and joining
// `kb_vectors` inside the stage-1 subquery to reach it turns a sequential scan
// of a narrow table into an indexed lookup per candidate row.

// SemanticQuery is one semantic search.
type SemanticQuery struct {
	// Vector is the query embedding, PACKED LITTLE-ENDIAN FLOAT32 — the
	// same layout the column holds, which is what makes the width filter
	// a byte-length comparison rather than a declared-type claim.
	Vector []byte

	// Model and Dim are the embedding space. BOTH are required: a model
	// change at the same width leaves two incompatible spaces in one
	// candidate pool, and `vector_distance_cos` over two widths is
	// undefined and fails the whole statement rather than the row.
	Model string
	Dim   int

	// Sources narrows to "page", "task", or both. Empty is both.
	Sources []Source

	// Containers narrows to these containers. EMPTY IS EVERY ONE, which
	// on the native backend means the whole company — the engine IS the
	// boundary here, and there is no second account to launder a read
	// through.
	Containers []string

	// Limit caps the hits. Zero takes [ReturnDepth], which is the depth
	// the fusion below it is written against.
	Limit int

	// Candidates caps stage one. Zero takes [Stage1Depth]; anything above
	// [BinaryCandidateCeiling] is clamped to it.
	Candidates int
}

// SemanticHit is one ranked document, with the EXACT distance rather than the
// quantized one.
//
// The rerank's whole purpose is that the score a caller sees is computed from
// the full vector: a sign code decides which documents are looked at and never
// how they are ordered.
type SemanticHit struct {
	Source    Source
	ID        string
	Container string
	Distance  float64
}

// SemanticScanBudget bounds one semantic search.
//
// ONE SECOND, which is the interactive target the supported corpus is declared
// against: at the measured coefficient a node holds ≈ 740 000 embeddable
// sources inside it. It is a CEILING on the statement rather than a promise
// about it — the point is that a query over a corpus somebody grew past the
// projection fails a search instead of holding this store's reader for as long
// as it takes.
const SemanticScanBudget = time.Second

// Semantic runs the two-stage search and returns the exactly-reranked hits.
//
// IT RAISES rather than answering empty, and the seam above it is what turns a
// failure into the empty block a turn tolerates: this is the storage layer,
// where "the store would not answer" and "nothing matched" are different
// facts, and collapsing them here would make a broken index look exactly like
// a company that has written nothing down.
func Semantic(ctx context.Context, tx *sql.Tx, q SemanticQuery) ([]SemanticHit, error) {
	if len(q.Vector) == 0 {
		return nil, fmt.Errorf("search: the semantic half needs a query vector")
	}
	if q.Model == "" || q.Dim <= 0 {
		return nil, fmt.Errorf("search: the semantic half needs the model and " +
			"the width the corpus was embedded at — without both, one pool " +
			"holds two embedding spaces and the ranking between them is " +
			"arithmetic on incompatible vectors")
	}
	if want := 4 * q.Dim; len(q.Vector) != want {
		return nil, fmt.Errorf("search: the query vector is %d bytes and the "+
			"width says %d — vector_distance_cos over mismatched lengths is "+
			"undefined and fails the whole statement", len(q.Vector), want)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = ReturnDepth
	}
	candidates := q.Candidates
	if candidates <= 0 {
		candidates = Stage1Depth
	}
	candidates = min(candidates, BinaryCandidateCeiling)
	// A CANDIDATE POOL SMALLER THAN THE ANSWER is a rerank that cannot
	// fill the page it was asked for, whatever it finds.
	candidates = max(candidates, limit)

	// THE ARGUMENTS ARE BUILT IN STATEMENT ORDER, which is the only order
	// a positional bind has: the query vector twice — once for the sign
	// code the first stage orders by and once for the exact distance the
	// second computes — then the predicate, then the two limits with the
	// width filter between them.
	args := []any{q.Vector, q.Vector}
	where := []string{"b.model = ?", "b.dim = ?"}
	args = append(args, q.Model, q.Dim)
	if len(q.Sources) > 0 {
		where = append(where, "b.source IN ("+placeholders(len(q.Sources))+")")
		for _, s := range q.Sources {
			args = append(args, string(s))
		}
	}
	if len(q.Containers) > 0 {
		where = append(where, "b.container IN ("+placeholders(len(q.Containers))+")")
		for _, c := range q.Containers {
			args = append(args, c)
		}
	}
	args = append(args, candidates, len(q.Vector), limit)

	// THE TIE BREAK IS DECLARED AT BOTH STAGES. Hamming distance over a
	// 3 072-bit code takes at most 3 073 distinct values whatever the
	// corpus size, so ties are the common case rather than the corner —
	// and without a declared tie break the candidate pool depends on the
	// scan's own row order, so two nodes rerank different pools and a
	// paged answer can repeat or skip a document.
	statement := `
		WITH q(code) AS (SELECT vector1bit(?))
		SELECT c.source, c.source_id, c.container,
		       vector_distance_cos(v.embedding, ?) AS distance
		FROM (
			SELECT b.source, b.source_id, b.container
			FROM kb_vectors_bin b, q
			WHERE ` + strings.Join(where, " AND ") + `
			ORDER BY vector_distance_cos(b.bits, q.code), b.source, b.source_id
			LIMIT ?
		) AS c
		JOIN kb_vectors v ON v.source = c.source AND v.source_id = c.source_id
		WHERE length(v.embedding) = ?
		ORDER BY distance, c.source, c.source_id
		LIMIT ?`

	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("search: the semantic scan: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SemanticHit
	for rows.Next() {
		var hit SemanticHit
		var source string
		if err := rows.Scan(&source, &hit.ID, &hit.Container, &hit.Distance); err != nil {
			return nil, fmt.Errorf("search: read a semantic hit: %w", err)
		}
		hit.Source = Source(source)
		out = append(out, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: the semantic scan: %w", err)
	}
	return out, nil
}

// placeholders builds `?, ?, …`.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// Key is how a hit is named to the fusion, which joins two ranked lists of
// documents that came from two different tables in two different estates.
//
// ONE SPELLING, here, because the lexical half and the semantic half each
// produce it and the fusion compares them: written twice, a page would fuse
// against itself under two names and every hybrid answer would be wrong in a
// way no single-half test could see.
func Key(source Source, id string) string { return string(source) + ":" + id }

// Hybrid fuses a keyword list and a semantic list into one answer.
//
// # Why the two halves are fused in Go rather than joined in SQL
//
// They are in DIFFERENT DATABASE FILES. The lexical index is this node's own —
// every node tokenises its own rows with no network — and the vectors are
// replicated, applied from a committed record. No transaction spans the two
// estates and no read joins across them, so a `UNION` was never available
// here; reciprocal rank fusion consumes RANKS, so nothing is lost by doing it
// over two lists of ids.
//
// What fusion absorbs is the loss of a document the keyword half also found,
// which merely slides down. What it does NOT absorb is the loss of a
// semantic-only document, which leaves the answer entirely — and that is
// exactly the class the semantic half exists for. See
// TestFuseIsNotMonotoneInTheSemanticList.
func Hybrid(keyword, semantic []string, limit int) []string {
	fused := Fuse(keyword, semantic)
	if limit > 0 && len(fused) > limit {
		fused = fused[:limit]
	}
	return fused
}
