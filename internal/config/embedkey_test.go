package config_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// AN EMBEDDER RUNS ON THE KEY IT NAMES: an empty field reads OPENAI_API_KEY,
// and an unresolved reference stays empty rather than borrowing a key the
// entry never named — an entry whose every reference resolved to nothing must
// not be told apart from one that named none only by the bill.
func TestAnEmbedderRunsOnTheKeyItNames(t *testing.T) {
	t.Parallel()
	r := config.NewResolver(config.MapSource{"OPENAI_API_KEY": "sk-openai", "EMBED_KEY": " sk-embed "})
	for _, tc := range []struct {
		field, want string
	}{
		{"", "sk-openai"},
		{"${EMBED_KEY}", "sk-embed"},
		{"${UNSET_KEY}", ""},
	} {
		e := config.EmbeddingProvider{APIKey: tc.field}
		if got := e.ResolvedKey(r); got != tc.want {
			t.Errorf("api_key %q resolved to %q, want %q", tc.field, got, tc.want)
		}
	}
}
