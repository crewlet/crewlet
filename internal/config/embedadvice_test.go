package config

import "testing"

// THE MODELS A REFUSAL OFFERS ARE THE ONES CARRYING THE LIMIT, and none is
// none: a refusal for a limit no model in the table carries says only "state
// it", because an empty list in parentheses would read as a way out that
// names nothing.
func TestTheModelsOfferedForALimitAreTheOnesCarryingIt(t *testing.T) {
	t.Parallel()
	if got, want := modelsDocumenting(func(m EmbeddingModel) int { return m.BatchInputs }),
		"text-embedding-3-large, text-embedding-3-small"; got != want {
		t.Errorf("the models carrying a request's input count = %q, want %q", got, want)
	}
	if got := modelsDocumenting(func(EmbeddingModel) int { return 0 }); got != "" {
		t.Errorf("a limit no model carries offered %q, want none", got)
	}
	if got := modelsDocumenting(func(m EmbeddingModel) int { return m.InputTokens }); got != knownModels() {
		t.Errorf("every model documents its window, yet the models offered for it are %q, "+
			"not every model (%q)", got, knownModels())
	}
}
