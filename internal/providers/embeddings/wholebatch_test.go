package embeddings_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// MANY TEXTS, EACH WHOLE, IN ONE CALL: a short text is the vector EmbedBatch
// gives it, a long one is the pool EmbedWhole gives it, an empty one keeps its
// slot — and all of them went in one call packed by the model's limits rather
// than one round trip a text.
func TestEmbeddingManyTextsWholeIsOneCallAndEachTextsOwnVector(t *testing.T) {
	t.Parallel()
	limits := embeddings.Limits{InputBytes: 32, BatchInputs: 64, BatchBytes: 4096}
	f := embeddings.NewFake(64)
	f.SetLimits(limits)
	short := "the release train is thursdays"
	long := strings.Repeat("deploy failing staging runbook rollback ", 6)
	got, err := embeddings.EmbedWholeBatch(t.Context(), f, []string{short, " \n ", long})
	if err != nil {
		t.Fatalf("EmbedWholeBatch: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("%d vectors for 3 texts", len(got))
	}
	if got[1] != nil {
		t.Errorf("a text of nothing was given a vector: %v", got[1])
	}

	reference := embeddings.NewFake(64)
	reference.SetLimits(limits)
	alone, err := reference.EmbedBatch(t.Context(), []string{short})
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if !slices.Equal(got[0], alone[0]) {
		t.Error("a short text is not the vector EmbedBatch gives it")
	}
	whole, err := embeddings.EmbedWhole(t.Context(), reference, long)
	if err != nil {
		t.Fatalf("EmbedWhole: %v", err)
	}
	if !slices.Equal(got[2], whole) {
		t.Error("a long text is not the pool EmbedWhole gives it")
	}

	pieces := 1 + len(embeddings.Chunks(long, limits.InputBytes))
	if requests := f.Requests(); len(requests) != 1 || len(requests[0]) != pieces {
		t.Fatalf("requests = %d (%v), want ONE carrying all %d pieces",
			len(requests), requests, pieces)
	}
}

// A FAILURE IS THE CALL'S, as it is EmbedBatch's: no text's vector is handed
// back beside the error, so a caller cannot store half a batch believing it
// whole.
func TestEmbeddingManyTextsWholeFailsAsOne(t *testing.T) {
	t.Parallel()
	f := embeddings.NewFake(16)
	f.Refuse("poison")
	got, err := embeddings.EmbedWholeBatch(t.Context(), f, []string{"fine", "poison"})
	if !errors.Is(err, embeddings.ErrRefused) || got != nil {
		t.Fatalf("EmbedWholeBatch = %v, %v; want nothing and ErrRefused", got, err)
	}
}

// NOTHING TO EMBED IS NO CALL: a batch of empty texts answers its empty slots
// without asking the provider anything.
func TestEmbeddingManyEmptyTextsWholeAsksNothing(t *testing.T) {
	t.Parallel()
	f := embeddings.NewFake(16)
	got, err := embeddings.EmbedWholeBatch(t.Context(), f, []string{"", "  "})
	if err != nil || len(got) != 2 || got[0] != nil || got[1] != nil {
		t.Fatalf("EmbedWholeBatch = %v, %v; want two empty slots", got, err)
	}
	if requests := f.Requests(); len(requests) != 0 {
		t.Fatalf("nothing to embed sent %d requests", len(requests))
	}
}
