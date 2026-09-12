package search

import (
	"context"
	"database/sql"
	"fmt"
)

// NodeScanner answers a bucket range out of the tables this process holds.
//
// It is what every participant in a fan-out runs — this node's own assignment
// and a peer's alike — so a slice computed locally and a slice that arrived
// over the broker are the same function of the same corpus. A second
// implementation for the local case would be a second ranking to keep in step
// with the first.
type NodeScanner struct{ Index *Indexer }

// Scan runs both halves over one bucket range.
//
// THE SEMANTIC HALF IS BEST EFFORT AND SAYS SO. A company with no embeddings
// provider has no vectors, a fresh node has not applied them yet, and a
// replicated store can be slow — none of which may fail a search that the
// lexical half can already answer. What it may NOT do is degrade silently, so
// the slice carries [Slice.SemanticSkipped] and the answer above it reports
// the fraction.
func (n NodeScanner) Scan(ctx context.Context, q FanQuery, shards Assignment) (Slice, error) {
	if n.Index == nil {
		return Slice{}, fmt.Errorf("search: scan a bucket range with no index")
	}
	out := Slice{Shards: shards}

	hits, err := n.Index.Search(ctx, SearchQuery{
		Text:       q.Text,
		Containers: q.Containers,
		Sources:    q.Sources,
		// FuseN RATHER THAN THE QUERY'S LIMIT, because this list is an
		// input to a merge rather than an answer: the global top-N is
		// contained in the union of the per-slice top-N only if each
		// slice returns N of them. See [MergeByScore].
		Limit:  FuseN,
		Shards: shards,
	})
	if err != nil {
		// THE LEXICAL HALF IS THE ONE THAT MAY FAIL THE SCAN. It reads
		// this node's own database, so its failure is the store under
		// the caller's feet rather than a degradation to report.
		return Slice{}, err
	}
	for _, hit := range hits {
		out.Lexical = append(out.Lexical, Scored{
			Key: Key(Source(hit.Source), hit.ID), Score: hit.Score,
		})
	}

	if len(q.Vector) == 0 || q.Model == "" || q.Dim == 0 {
		// A LEXICAL QUERY IS NOT A DEGRADED ONE. The caller asked for
		// one half, so the other did not fail — and reporting a skip
		// here would fire [statelog.KindSearchDegraded] on every search
		// a company without an embeddings provider ever runs, which is
		// an alarm that is red for the life of the deployment and
		// therefore an alarm nobody reads.
		return out, nil
	}
	var vectors []SemanticHit
	if err := n.Index.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var err error
		vectors, err = Semantic(ctx, tx, SemanticQuery{
			Vector: q.Vector, Model: q.Model, Dim: q.Dim,
			Containers: q.Containers,
			Sources:    sourcesOf(q.Sources),
			Limit:      FuseN,
			Shards:     shards,
		})
		return err
	}); err != nil {
		// THIS one is a degradation and says so: a vector was supplied,
		// the scan was meant to run, and it did not. Best effort by
		// design — a turn must not die because the replicated store was
		// slow — but never silent, because what was lost is exactly the
		// class the semantic half exists for.
		out.SemanticSkipped = true
		return out, nil
	}
	for _, hit := range vectors {
		out.Semantic = append(out.Semantic, Scored{
			Key: Key(hit.Source, hit.ID), Score: ScoreOfDistance(hit.Distance),
		})
	}
	return out, nil
}

// sourcesOf converts the wire form of a source filter to the typed one.
//
// AN UNKNOWN NAME IS DROPPED rather than refused, because this value crosses
// the broker: a peer running a build that knows a source this one does not
// must narrow to what it can answer instead of failing the whole slice, which
// would turn a rolling upgrade into a fleet of partial answers.
func sourcesOf(names []string) []Source {
	out := make([]Source, 0, len(names))
	for _, name := range names {
		for _, known := range Sources {
			if Source(name) == known {
				out = append(out, known)
			}
		}
	}
	return out
}

// ScoreOfDistance turns a semantic scan's distance into a merge score.
//
// THE NEGATION, and it is exact rather than pretty. [Scored.Score] is
// higher-is-better so that one merge serves both methods, and the obvious
// conversion for a cosine distance is `1 - d`, which is the cosine similarity
// itself and reads far better in a log.
//
// It is also LOSSY exactly where it matters. Floating-point subtraction near 1
// rounds away anything below the ulp of 1 (≈ 2.2e-16), so two documents at
// distances 1e-17 and 2e-17 — a real pair on near-duplicate pages — both map
// to 1.0, become a tie, and are then ordered by their ids instead of by how
// close they actually are. Negation has no rounding at all: it flips one sign
// bit, so every distance the scan could distinguish stays distinguishable
// after the merge.
func ScoreOfDistance(distance float64) float64 { return -distance }
