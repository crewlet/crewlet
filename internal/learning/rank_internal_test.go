package learning

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

func TestRankIsATotalOrderIndependentOfInputOrder(t *testing.T) {
	t.Parallel()
	// Exercised directly rather than through Recall, because the database's
	// incidental determinism masks it completely: SQLite returns rows with
	// equal ORDER BY keys in insertion order, so every shuffle a test could
	// arrange through the store arrives already sorted.
	//
	// Found by mutation — deleting the id tie-break, and reversing it,
	// changed nothing observable through Recall.
	at := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	newer := at.Add(time.Minute)

	hits := []Hit{
		{Similarity: 0.9, Episode: Episode{ID: "a", EndedAt: at}},
		{Similarity: 0.9, Episode: Episode{ID: "z", EndedAt: at}},
		{Similarity: 0.9, Episode: Episode{ID: "m", EndedAt: at}},
		{Similarity: 0.9, Episode: Episode{ID: "recent", EndedAt: newer}},
		{Similarity: 0.5, Episode: Episode{ID: "weak", EndedAt: newer}},
	}
	// Similarity first, then recency, then id DESCENDING.
	want := []string{"recent", "z", "m", "a", "weak"}

	shuffled := slices.Clone(hits)
	rank(shuffled)
	if got := rankIDs(shuffled); !slices.Equal(got, want) {
		t.Fatalf("rank = %v, want %v", got, want)
	}

	// Every input order produces the same output order. A comparator that
	// deferred to the input for full ties would pass a single arrangement
	// and fail here.
	rng := rand.New(rand.NewPCG(1, 2))
	for range 50 {
		perm := slices.Clone(hits)
		rng.Shuffle(len(perm), func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
		rank(perm)
		if got := rankIDs(perm); !slices.Equal(got, want) {
			t.Fatalf("a shuffled input ranked %v, want %v", got, want)
		}
	}
}

func rankIDs(hits []Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Episode.ID)
	}
	return out
}
