package learning

import (
	"context"
	"fmt"
	"math"
	"slices"
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
// BRUTE FORCE over the seat's rows, deliberately. No ANN index reaches the Go
// driver yet, and the per-seat time index keeps the scan to one seat's
// episodes rather than the whole table. A company's seat has thousands of
// turns, not millions.
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

	rows, err := e.db.SQL().QueryContext(ctx,
		`SELECT `+episodeColumns+` FROM episodes
		 WHERE agent_handle = ? AND embedding IS NOT NULL
		 ORDER BY ended_at DESC`, q.Handle)
	if err != nil {
		return nil, fmt.Errorf("learning: recall for %s: %w", q.Handle, err)
	}
	candidates, err := collectEpisodes(rows)
	if err != nil {
		return nil, err
	}

	var hits []Hit
	for _, ep := range candidates {
		if !slices.Contains(kinds, ep.Kind) {
			continue
		}
		sim, ok := cosine(q.Embedding, ep.Embedding)
		if !ok || sim < floor {
			continue
		}
		hits = append(hits, Hit{Episode: ep, Similarity: sim})
	}
	rank(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
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

// VectorSearchAvailable reports whether the database can do the distance
// arithmetic itself.
//
// Reported rather than relied on: recall above is brute force in Go precisely
// so a build without vector functions still remembers. This exists for the
// operator surface, which should be able to say which mode a deployment is in.
func (e *Episodes) VectorSearchAvailable() bool {
	return e.db.Caps().VectorFunctions
}
