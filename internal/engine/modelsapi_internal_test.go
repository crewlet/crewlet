package engine

import (
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
)

// sonnetEntry is an api-key entry whose key is read from its own variable,
// on a CLI binary that is not there, so nothing in a test calls a vendor.
func sonnetEntry(t *testing.T) config.LLMProvider {
	t.Helper()
	return config.LLMProvider{Harness: "claude-code", Model: "sonnet", StateDir: t.TempDir(),
		Auth:      config.HarnessAuth{Mode: config.AuthAPIKey, APIKey: "${ANTHROPIC_API_KEY_SONNET}"},
		Overrides: map[string]any{"binary": filepath.Join(t.TempDir(), "no-such-cli")}}
}

// modelsEngine is a bare engine running one entry, resolving through env.
func modelsEngine(t *testing.T, entry config.LLMProvider, env config.MapSource) *Engine {
	t.Helper()
	built, err := BuildCLIAgent("sonnet", entry, config.NewResolver(env), nil)
	if err != nil {
		t.Fatalf("BuildCLIAgent: %v", err)
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "sonnet", Provider: built}})
	if err != nil {
		t.Fatal(err)
	}
	e := epochEngine(&Company{Models: models, Config: &config.Company{Providers: config.Providers{
		LLM: map[string]config.LLMProvider{"sonnet": entry}, Sandbox: &config.SandboxProvider{Fake: true}}}})
	e.environ = env
	return e
}

// WHAT THIS NODE RESOLVED IS ANSWERED, AND NEVER THE VALUE.
//
// Settings › Models names the variable an entry reads, whether it resolved
// here and the value's hint; the heartbeat carries none of the three.
func TestLocalModelsNameTheVariableAndHintTheValue(t *testing.T) {
	t.Parallel()
	e := modelsEngine(t, sonnetEntry(t), config.MapSource{
		"ANTHROPIC_API_KEY_SONNET": "sk-ant-api03-the-resolved-key-7d2K"})
	rows := e.LocalModels()
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	got := rows[0]
	if got.Route != "api-key" || !got.Resolves || got.Variable != "ANTHROPIC_API_KEY_SONNET" {
		t.Errorf("row = %+v, want an api-key read from its own variable that resolves", got)
	}
	if got.Hint != "sk-ant-api03-…7d2K" {
		t.Errorf("hint = %q, want the prefix and the last four", got.Hint)
	}
	if got.TokenLifetime != 0 {
		t.Errorf("an API key carries a token lifetime %v", got.TokenLifetime)
	}
}
