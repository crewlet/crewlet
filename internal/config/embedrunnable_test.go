package config_test

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// AN EMBEDDER THIS BUILD CANNOT RUN IS REFUSED WHERE A REVISION IS APPLIED, not
// only where a document is submitted.
//
// The embedding rules — a width the endpoint cannot produce, a limit nobody
// documented and nobody stated, a limit raised past the model's — are RUNNABLE
// rules, and the configuration guide promises an operator that a stored
// revision breaking one is refused at boot as well as at an apply. Both of
// those decode the stored form and hold it to ValidateRunnable alone
// (cmd/crewlet's boot, the engine's reconcile), so that is the path taken
// here: the company goes into the store's JSON, comes back out through
// DecodeCompany, and is judged as a boot judges it. Every other embedding
// test calls Validate, which runs the admission rules too — so a case moved
// to the admission class would keep all of them green while a node booted on
// the revision and sent what the model refuses, or asked a width its
// endpoint cannot give.
//
// And none of them is an admission rule as well, which Validate would report
// twice.
func TestAnEmbedderThisBuildCannotRunIsRefusedWhereARevisionIsApplied(t *testing.T) {
	t.Parallel()
	cohere := func(width int) *config.EmbeddingProvider {
		return &config.EmbeddingProvider{
			Type: config.EmbeddingOpenAICompatible, Model: "embed-v4.0",
			BaseURL: "https://api.cohere.ai/compatibility/v1", Dimensions: width,
			MaxBatchInputs: 96, MaxBatchTokens: 128_000,
		}
	}
	gemini := &config.EmbeddingProvider{
		Type: config.EmbeddingOpenAICompatible, Model: "gemini-embedding-001",
		BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai/",
	}
	unknown := &config.EmbeddingProvider{
		Type: config.EmbeddingOpenAICompatible, Model: "bge-m3",
		BaseURL: "http://embed.example.com/v1",
	}
	noRequestLimits := cohere(0)
	noRequestLimits.MaxBatchInputs, noRequestLimits.MaxBatchTokens = 0, 0

	for name, tc := range map[string]struct {
		provider *config.EmbeddingProvider
		refused  []string // the embedding fields refused, exactly
	}{
		// The CONTROL: a provider every rule admits, so a test that
		// refused everything could not pass.
		"embed-v4.0 at its own width with its limits stated": {cohere(1536), nil},
		"embed-v4.0 at a width its endpoint cannot produce":  {cohere(1024), []string{"dimensions"}},
		"embed-v4.0 with no request limits": {noRequestLimits,
			[]string{"max_batch_inputs", "max_batch_tokens"}},
		"gemini-embedding-001 with no request limits": {gemini,
			[]string{"max_batch_inputs", "max_batch_tokens"}},
		"a model this build does not know, stating nothing": {unknown,
			[]string{"dimensions", "max_batch_inputs", "max_batch_tokens", "max_input_tokens"}},
		"an OpenAI limit raised past the model's": {
			&config.EmbeddingProvider{Model: "text-embedding-3-small", MaxBatchInputs: 4096},
			[]string{"max_batch_inputs"}},
	} {
		t.Run(name, func(t *testing.T) {
			payload, err := json.Marshal(embeddingCompany(t, tc.provider))
			if err != nil {
				t.Fatalf("marshal the stored form: %v", err)
			}
			stored, err := config.DecodeCompany(payload)
			if err != nil {
				t.Fatalf("decode the stored form: %v", err)
			}

			got := slices.Sorted(maps.Keys(limitRefusals(stored.ValidateRunnable())))
			if !slices.Equal(got, tc.refused) {
				t.Errorf("ValidateRunnable refused %v of the stored revision's embedding "+
					"fields, want %v", got, tc.refused)
			}
			if admitted := limitRefusals(stored.ValidateAdmission()); len(admitted) != 0 {
				t.Errorf("ValidateAdmission refused %v: an embedding rule belongs to the "+
					"runnable class alone", slices.Sorted(maps.Keys(admitted)))
			}
		})
	}
}
