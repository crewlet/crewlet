package search_test

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
)

// CORPORA FUSED FROM THEIR CANDIDATES RANK EXACTLY AS ONE CORPUS DOES.
//
// A gather asks each partition for its candidates — each method's top-FuseN
// with scores — and fuses them where it holds them all. That is only worth
// doing if it is the SAME ranking a single scan of every document would have
// produced: the per-method global top-N lies inside the union of the
// per-corpus top-N because the corpora are disjoint, so merging by score and
// fusing once is identical to fusing the whole. Split a corpus every way and
// the order must not move.
func TestDisjointCorporaFuseExactlyAsOneCorpus(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(7, 11))
	const docs = 400
	lexical := make([]search.Scored, 0, docs)
	semantic := make([]search.Scored, 0, docs)
	for i := range docs {
		key := fmt.Sprintf("page:%04d", i)
		// NOT EVERY DOCUMENT IN BOTH LISTS, which is the shape a real
		// query has: a page that shares no word with it is semantic-only.
		if i%3 != 0 {
			lexical = append(lexical, search.Scored{Key: key, Score: rng.Float64() * 20})
		}
		semantic = append(semantic, search.Scored{Key: key, Score: -rng.Float64()})
	}
	whole := search.FuseCandidates([]search.Candidates{topOf(lexical, semantic, nil)})
	if len(whole) == 0 {
		t.Fatal("the whole corpus fused to nothing")
	}
	for _, partitions := range []int{2, 4, 64} {
		parts := make([]search.Candidates, partitions)
		for p := range parts {
			parts[p] = topOf(lexical, semantic, func(key string) bool {
				var n int
				_, _ = fmt.Sscanf(key, "page:%d", &n)
				return n%partitions == p
			})
		}
		if got := search.FuseCandidates(parts); !slices.Equal(got, whole) {
			t.Errorf("split %d ways the corpus fused to a different order:\n got %v\nwant %v",
				partitions, head(got), head(whole))
		}
	}
}

// topOf is one corpus's candidates: the documents keep admits, each method's
// best FuseN by score.
func topOf(lexical, semantic []search.Scored, keep func(string) bool) search.Candidates {
	pick := func(all []search.Scored) []search.Scored {
		var mine []search.Scored
		for _, s := range all {
			if keep == nil || keep(s.Key) {
				mine = append(mine, s)
			}
		}
		return search.MergeByScore([][]search.Scored{mine}, search.FuseN)
	}
	return search.Candidates{Lexical: pick(lexical), Semantic: pick(semantic)}
}

func head(keys []string) []string { return keys[:min(len(keys), 8)] }

// A CANDIDATE SET NAMES EACH DOCUMENT ONCE, whichever methods found it — so a
// caller hydrating what it may have to show reads each page once.
func TestACandidateSetNamesEachDocumentOnce(t *testing.T) {
	t.Parallel()
	c := search.Candidates{
		Lexical:  []search.Scored{{Key: "a", Score: 3}, {Key: "b", Score: 2}},
		Semantic: []search.Scored{{Key: "b", Score: -0.1}, {Key: "c", Score: -0.2}},
	}
	if got, want := c.Keys(), []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Errorf("Keys() = %v, want %v", got, want)
	}
}
