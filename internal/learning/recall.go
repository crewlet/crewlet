package learning

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/store"
)

// Hit is one recalled episode with its similarity.
type Hit struct {
	Episode    Episode
	Similarity float64
}

// RecallQuery bounds a similarity search.
type RecallQuery struct {
	Handle    string
	Embedding []float32

	// Limit is how many hits to return. 0 takes a small default: recall
	// goes into a prompt, and a dozen half-relevant memories crowd out the
	// task they were fetched for.
	Limit int

	// MinSimilarity floors what counts as a memory. Cosine similarity over
	// unrelated text still lands well above zero, so with no floor the
	// nearest N rows always come back — a seat with three episodes recalls
	// all three on every turn, however irrelevant.
	MinSimilarity float64

	// Kinds filters row shapes. Empty means raw episodes only: a compacted
	// cluster summarises many turns and reads in a prompt like one turn
	// that did all of them.
	Kinds []Kind
}

const (
	defaultRecallLimit = 5
	// defaultMinSimilarity is the floor when a caller states none.
	//
	// 0.3 rather than 0: two unrelated sentences from one embedding model
	// routinely score 0.1–0.25, so a zero floor returns the nearest rows
	// whatever they are. It is deliberately generous — the cost of a
	// marginal memory is prompt tokens, and the cost of missing the
	// relevant one is the seat repeating work it has already done.
	defaultMinSimilarity = 0.3
)

// Recall returns a seat's most similar past episodes.
//
// A SCAN, and the database does the arithmetic. There is still no ANN index
// reachable from the Go driver (decisions/002, re-measured at the pin), so
// every embedded row for the seat is visited — what the per-seat time index
// buys is that it is one seat's episodes rather than the whole table. A
// company's seat has thousands of turns, not millions.
//
// It used to visit them in Go: select every row, decode every vector, cosine
// each one. That was written when a second driver with no vector functions
// had to be served and it then ran on BOTH drivers unconditionally, because
// nothing ever called the other path. With one driver (decisions/003) the
// ordering is `vector_distance_cos` in an ORDER BY, and only the rows that
// survive the LIMIT cross the driver boundary. Measured at 5 000 rows of 1 536
// dimensions: 30.7 MB and 81 ms became one row and 26 ms.
//
// Rows with no embedding are skipped rather than scored: they were written
// during an embeddings outage, and treating a missing vector as a zero vector
// would score them as maximally dissimilar to everything and rank them
// consistently last — which reads as a judgment about their content.
func (e *Episodes) Recall(ctx context.Context, q RecallQuery) ([]Hit, error) {
	if q.Handle == "" {
		return nil, fmt.Errorf("learning: recall needs a seat")
	}
	if len(q.Embedding) == 0 {
		return nil, ErrNoEmbedding
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultRecallLimit
	}
	floor := q.MinSimilarity
	if floor == 0 {
		floor = defaultMinSimilarity
	}
	kinds := q.Kinds
	if len(kinds) == 0 {
		kinds = []Kind{KindRaw}
	}
	probe, width, err := vectorProbe(e.db, q.Embedding)
	if err != nil {
		return nil, fmt.Errorf("learning: recall for %s: %w", q.Handle, err)
	}

	// The kind filter is a bound list of short literals rather than
	// placeholders because it comes from a typed enum this package owns —
	// see kindList.
	rows, err := e.db.SQL().QueryContext(ctx,
		`SELECT `+episodeColumns+` FROM (
		    SELECT `+episodeColumns+`,
		           vector_distance_cos(embedding, ?) AS distance
		    FROM episodes
		    WHERE agent_handle = ?
		      AND embedding IS NOT NULL
		      AND length(embedding) = ?
		      AND kind IN (`+kindList(kinds)+`)
		 )
		 WHERE distance <= ?
		 ORDER BY distance ASC, ended_at DESC, id DESC
		 LIMIT ?`,
		probe, q.Handle, width, 1-floor, limit)
	if err != nil {
		return nil, fmt.Errorf("learning: recall for %s: %w", q.Handle, err)
	}
	candidates, err := collectEpisodes(rows)
	if err != nil {
		return nil, err
	}

	// SCORED AGAIN IN GO, over the rows that survived rather than over all
	// of them. Two reasons, and neither is distrust of the ordering — the
	// two agree to eight decimal places (measured).
	//
	// The first is that `Hit.Similarity` is read by callers and rendered
	// into prompts, so it has to keep meaning exactly what it meant: the
	// value [cosine] returns, computed in float64 from the same vectors.
	//
	// The second is a guard the SQL cannot express. vector_distance_cos
	// answers 0 — a PERFECT match — for a vector holding a NaN or an
	// infinity, so a single poisoned embedding would sort itself to the top
	// of every recall this seat ever ran. [cosine] rejects those, and the
	// rows it rejects are dropped here. That costs at most `limit`
	// computations, against the thousands the loop used to do.
	var hits []Hit
	for _, ep := range candidates {
		sim, ok := cosine(q.Embedding, ep.Embedding)
		if !ok || sim < floor {
			continue
		}
		hits = append(hits, Hit{Episode: ep, Similarity: sim})
	}
	rank(hits)
	return hits, nil
}

// kindList renders a kind filter as SQL literals.
//
// The values are this package's own typed enum — [KindRaw] and
// [KindCompacted], both fixed identifiers — so there is no caller input in the
// statement. Placeholders would be safer against a future where that stops
// being true, and would also make the statement text vary with the number of
// kinds, which costs a prepared-statement entry per shape; the enum being
// closed is what makes the trade honest. A value outside it renders as a
// quoted string that matches no row, which is the same answer a placeholder
// would give.
func kindList(kinds []Kind) string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, "'"+strings.ReplaceAll(string(k), "'", "''")+"'")
	}
	return strings.Join(out, ", ")
}

// vectorProbe packs a query embedding for binding, and reports the byte width
// a stored row must have to be comparable with it.
//
// THE WIDTH FILTER IS NOT AN OPTIMISATION. vector_distance_cos fails the whole
// statement on a width mismatch — "Vectors must have the same dimensions",
// raised during iteration, after the query has already succeeded — and a
// company that changes its embedding model leaves exactly those rows behind.
// The Go loop skipped them silently (cosine returns false on a shape
// mismatch); without `length(embedding) = ?` the SQL would turn that same
// history into a recall that errors instead of one that returns what it can.
func vectorProbe(db *store.DB, embedding []float32) ([]byte, int, error) {
	blob, err := db.EncodeVector(embedding)
	if err != nil {
		return nil, 0, err
	}
	return blob, len(blob), nil
}

// rank orders hits: most similar first, then most recent, then by id
// descending.
//
// A TOTAL order, and self-contained: it does not lean on the order the
// database returned. SQL leaves the order of equal ORDER BY keys unspecified,
// and SQLite happens to give insertion order — which means a tie-break that
// deferred to the input would be correct today and silently change with a
// query plan. Two episodes of one recurring task score identically, and a seat
// that recalled a different one on every turn would be reacting to its own
// storage layout.
//
// Split out so it can be exercised on a shuffled slice. Through Recall the
// database's incidental determinism masks it completely.
func rank(hits []Hit) {
	slices.SortFunc(hits, func(a, b Hit) int {
		switch {
		case a.Similarity > b.Similarity:
			return -1
		case a.Similarity < b.Similarity:
			return 1
		case a.Episode.EndedAt.After(b.Episode.EndedAt):
			return -1
		case a.Episode.EndedAt.Before(b.Episode.EndedAt):
			return 1
		default:
			return -1 * compareStrings(a.Episode.ID, b.Episode.ID)
		}
	})
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// cosine returns the cosine similarity of two vectors, and false when it is
// not defined for them.
//
// Undefined in three ways, all of which happen: mismatched widths (a company
// that changed embedding model mid-life), a zero vector (an embedding of empty
// text), and a non-finite component (a provider returning NaN or an
// overflowed value). The first is caught by shape; the other two both arrive
// at the same place — a NaN result — which is why there is ONE check for them
// rather than one each.
//
// It matters that they are caught at all rather than ranked: a NaN compares
// false against everything, so a single one lands wherever the sort's pivot
// choices put it, and a seat's recall order becomes a property of its data
// layout.
func cosine(a, b []float32) (float64, bool) {
	if len(a) == 0 || len(a) != len(b) {
		return 0, false
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	sim := dot / (math.Sqrt(na) * math.Sqrt(nb))
	// A zero vector divides by zero; a non-finite component propagates.
	// Both land on NaN, and Cauchy-Schwarz bounds a finite result to
	// [-1, 1], so an infinity here could only come from the same place.
	if math.IsNaN(sim) || math.IsInf(sim, 0) {
		return 0, false
	}
	return sim, true
}

// There is no VectorSearchAvailable here any more.
//
// It reported db.Caps().VectorFunctions "for the operator surface", and no
// operator surface ever called it — while recall, the one thing the answer was
// about, ignored it and ran the Go loop either way. Recall is now written
// against vector_distance_cos on the one driver that has it (decisions/003),
// so the question has one answer for a given build, and the place it is
// actually reported is the store_opened log line, which prints all three
// capabilities at every start.
