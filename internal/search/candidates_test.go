package search_test

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
)

// THE CANDIDATES FUSE BY THE FAN-OUT'S OWN RULE, so the order a caller walks
// past what it cannot show is the order the fan-out answered: each method by
// score, cut at FuseN, then reciprocal rank fusion once over the two. A list
// that arrived in another order — a slice's, before the merge — is ranked by
// its scores, never by where it happened to be.
func TestTheCandidatesFuseByTheFanOutsOwnRule(t *testing.T) {
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
	byScore := func(list []search.Scored) []string {
		var keys []string
		for _, s := range search.MergeByScore([][]search.Scored{list}, search.FuseN) {
			keys = append(keys, s.Key)
		}
		return keys
	}
	want := search.Fuse(byScore(lexical), byScore(semantic))
	if len(want) == 0 {
		t.Fatal("the corpus fused to nothing")
	}
	// UNSORTED, as the documents were generated: the rule sorts them.
	got := search.Candidates{Lexical: lexical, Semantic: semantic}.Fused()
	if !slices.Equal(got, want) {
		t.Errorf("the candidates fused to a different order:\n got %v\nwant %v",
			head(got), head(want))
	}
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
