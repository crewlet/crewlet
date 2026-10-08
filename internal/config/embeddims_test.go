package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// EVERY MODEL SAYS WHAT ITS ENDPOINT DOES WITH A WIDTH ASKED FOR, and where
// that was read. Whether a request carries `dimensions`, and whether any width
// but the model's own can be configured, both turn on it — so an entry with no
// answer, or an answer with no source, is a decision nobody can check.
func TestEveryModelSaysWhatItsEndpointDoesWithAWidth(t *testing.T) {
	t.Parallel()
	if len(config.EmbeddingModels) == 0 {
		t.Fatal("the model table is empty — this guard was certifying nothing")
	}
	for name, model := range config.EmbeddingModels {
		if !model.Dimensions.Valid() {
			t.Errorf("%s: Dimensions %q is not one of the documented answers", name, model.Dimensions)
		}
		if strings.TrimSpace(model.DimensionsSource) == "" {
			t.Errorf("%s: says what its endpoint does with `dimensions` and not where that was read", name)
		}
	}
	if config.DimensionsSupport("").Valid() || config.DimensionsSupport("maybe").Valid() {
		t.Error("an answer nobody documented reads as a valid one")
	}
}

// A WIDTH IS ASKED FOR WHERE THE ENDPOINT TAKES ONE, and nowhere else.
//
// OpenAI's third-generation models shorten to the width asked; Google's
// compatible endpoint documents nothing, so it is asked as it always was;
// a model this build does not know is asked too. Cohere's Compatibility API
// lists `dimensions` as a parameter it does not support, so embed-v4.0 is
// never sent it at the width it emits — and no other width can be configured,
// because the endpoint answers at 1536 whatever the store was sized for.
func TestAWidthIsAskedForOnlyWhereTheEndpointTakesOne(t *testing.T) {
	t.Parallel()
	cohere := func(width int) *config.EmbeddingProvider {
		return &config.EmbeddingProvider{
			Type: config.EmbeddingOpenAICompatible, Model: "embed-v4.0",
			BaseURL: "https://api.cohere.ai/compatibility/v1", Dimensions: width,
			MaxBatchInputs: 96, MaxBatchTokens: 128_000,
		}
	}
	for name, tc := range map[string]struct {
		provider *config.EmbeddingProvider
		sends    bool
	}{
		"OpenAI at its own width":     {&config.EmbeddingProvider{Model: "text-embedding-3-large"}, true},
		"OpenAI shortened":            {&config.EmbeddingProvider{Model: "text-embedding-3-large", Dimensions: 1024}, true},
		"an endpoint that is silent":  {&config.EmbeddingProvider{Model: "gemini-embedding-001"}, true},
		"a model this build lacks":    {&config.EmbeddingProvider{Model: "bge-m3", Dimensions: 1024}, true},
		"Cohere's endpoint, unset":    {cohere(0), false},
		"Cohere's endpoint, its own":  {cohere(1536), false},
		"Cohere's endpoint, narrower": {cohere(1024), true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.provider.SendsDimensions(); got != tc.sends {
				t.Errorf("SendsDimensions() = %v, want %v", got, tc.sends)
			}
		})
	}

	for _, width := range []int{0, 1536} {
		if err := embeddingCompany(t, cohere(width)).Validate(); err != nil {
			t.Errorf("embed-v4.0 at dimensions %d was refused: %v", width, err)
		}
	}
	err := embeddingCompany(t, cohere(1024)).Validate()
	if err == nil {
		t.Fatal("embed-v4.0 at a width its endpoint cannot produce was accepted")
	}
	if !errors.Is(err, config.ErrConflict) {
		t.Errorf("refusal %v is not ErrConflict", err)
	}
	refusal, refused := limitRefusals(err)["dimensions"]
	if !refused {
		t.Fatalf("refusal %q does not name `dimensions`", err)
	}
	for _, want := range []string{"embed-v4.0", "1536", "docs.cohere.com/docs/compatibility-api"} {
		if !strings.Contains(refusal, want) {
			t.Errorf("refusal = %q, want it to say %q", refusal, want)
		}
	}
	// A WIDTH OUT OF RANGE IS ONE REFUSAL, not two: the range is what is
	// wrong with it.
	if got := len(config.Problems(embeddingCompany(t, cohere(32)).Validate())); got != 1 {
		t.Errorf("a width below the floor for embed-v4.0 drew %d problems, want 1", got)
	}
}
