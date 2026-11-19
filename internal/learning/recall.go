package learning

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"

	"github.com/crewlet/crewlet/internal/store"
)

// Hit is one recalled episode with its similarity.
type Hit struct {
	Episode    Episode
	Similarity float64
}

// EpisodeSearch is one similarity search over a seat's turns: what it found,
// and how many turns it could not search at all.
type EpisodeSearch struct {
	Hits []Hit

	// Unsearched is how many of the seat's raw turns have no vector of the
	// query's model at this store's width ([Episodes.Unsearchable]) — not a
	// worse match but no match, since a cosine across two spaces ranks
	// nothing. So no hits over a seat with turns unsearched is "these were
	// not searched", never "nothing like this was ever done".
	Unsearched int
}

// RecallQuery bounds a similarity search.
type RecallQuery struct {
	// Handle is the seat's handle, which is what episodes.agent_handle
	// holds. The diary ignores it: its rows are scoped by the agent id the
	// caller passes.
	Handle    string
	Embedding []float32

	// Model is the model Embedding came from, and REQUIRED: a recall
	// compares only rows of the same model, because two models of one
	// width are two spaces and the width filter alone admits both. A row
	// from another model is not a worse match, it is no match at all.
	Model string

	// Limit is how many hits to return. 0 takes a small default: recall
	// goes into a prompt, and a dozen half-relevant memories crowd out the
	// task they were fetched for.
	Limit int

	// Held is what a diary recall holds its notes to: a note it holds back is
	// never a hit ([DiaryEntry.HeldBack]). Zero holds none back.
	Held Currency

	// MinSimilarity floors what counts as a memory. Cosine similarity over
	// unrelated text still lands well above zero, so with no floor the
	// nearest N rows always come back — a seat with three episodes recalls
	// all three on every turn, however irrelevant.
	MinSimilarity float64
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
// A SCAN: no ANN index is reachable from the Go driver, so every embedded row
// of the one seat is visited, but `vector_distance_cos` in an ORDER BY means
// only rows surviving the LIMIT cross the driver boundary (5 000 x 1 536-dim
// episodes: 34 ms and 35.8 KB, against 144 ms and 35.8 MB scoring in Go).
//
// Rows with no embedding of the query's model are skipped, not scored: a zero
// vector would rank them last, a judgment about their content.
// [Episodes.Unfilled] fills them; [Episodes.Unsearchable] counts the misses.
// RAW TURNS ONLY: a compacted row carries no vector (see [Summary]).
func (e *Episodes) Recall(ctx context.Context, q RecallQuery) ([]Hit, error) {
	if q.Handle == "" {
		return nil, fmt.Errorf("learning: recall needs a seat")
	}
	if len(q.Embedding) == 0 || q.Model == "" {
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
	probe, width, err := vectorProbe(e.db, q.Embedding)
	if err != nil {
		return nil, fmt.Errorf("learning: recall for %s: %w", q.Handle, err)
	}

	// RANK IDS, THEN FETCH THE WINNERS BY PRIMARY KEY.
	//
	// The inner query carries only id, tie-break and distance, so the sort
	// moves dozens of bytes per row, not the ~7 KB an episode weighs (78 ms
	// against 34 ms). The outer statement returns an UNORDERED set: [rank]
	// imposes the total order in Go.
	rows, err := e.db.SQL().QueryContext(ctx,
		`SELECT `+episodeColumns+` FROM episodes WHERE id IN (
		    SELECT id FROM (
		        SELECT id, ended_at,
		               vector_distance_cos(embedding, ?) AS distance
		        FROM episodes
		        WHERE agent_handle = ?
		          AND embedding IS NOT NULL
		          AND embedding_model = ?
		          AND length(embedding) = ?
		          AND kind = ?
		    )
		    WHERE distance <= ?
		    ORDER BY distance ASC, ended_at DESC, id DESC
		    LIMIT ?
		 )`,
		probe, q.Handle, q.Model, width, string(KindRaw), 1-floor, limit)
	if err != nil {
		return nil, fmt.Errorf("learning: recall for %s: %w", q.Handle, err)
	}
	candidates, err := collectEpisodes(rows)
	if err != nil {
		return nil, err
	}

	// SCORED AGAIN IN GO, over the survivors only: `Hit.Similarity` is
	// rendered into prompts, so it stays [cosine]'s float64 value; and
	// vector_distance_cos answers 0, a PERFECT match, for a vector holding a
	// NaN or infinity, which [cosine] rejects and this drops.
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

// vectorProbe packs a query embedding for binding, and reports the byte width
// a stored row must have to be comparable with it.
//
// THE WIDTH FILTER IS NOT AN OPTIMISATION. vector_distance_cos fails the whole
// statement on a width mismatch, and a changed embedding model leaves such
// rows behind; without `length(embedding) = ?` recall would error instead of
// returning what it can. It stays beside the model filter, which does not
// subsume it: one model answers at whatever width is asked (text-embedding-3
// truncates to `dimensions`), so a width change leaves rows of the SAME model
// at the old width.
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
// A TOTAL order, self-contained: SQL leaves the order of equal ORDER BY keys
// unspecified (SQLite happens to give insertion order), so a tie-break
// deferring to the input would silently change with a query plan. Two
// episodes of one recurring task score identically, and a seat recalling a
// different one each turn would be reacting to its storage layout.
//
// Split out so a test can shuffle the slice; through Recall the database's
// incidental determinism masks it.
func rank(hits []Hit) {
	// EVERY KEY DESCENDS — most similar, then newest, then the higher id —
	// so every compare takes its arguments reversed.
	slices.SortFunc(hits, func(a, b Hit) int {
		return cmp.Or(
			cmp.Compare(b.Similarity, a.Similarity),
			b.Episode.EndedAt.Compare(a.Episode.EndedAt),
			cmp.Compare(b.Episode.ID, a.Episode.ID),
		)
	})
}

// cosine returns the cosine similarity of two vectors, and false when it is
// not defined for them.
//
// Undefined in three ways, all of which happen: mismatched widths (embedding
// model changed mid-life), a zero vector (empty text), and a non-finite
// component (a provider returning NaN or an overflow). The last two both end
// in a NaN, so ONE check covers them. They must be caught rather than ranked:
// a NaN compares false against everything and lands wherever the sort's
// pivots put it, making recall order a property of the data layout.
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
// against vector_distance_cos on the one driver that has it,
// so the question has one answer for a given build, and the place it is
// actually reported is the store_opened log line, which prints all three
// capabilities at every start.
