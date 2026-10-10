package cliagent

import (
	"slices"
	"testing"
	"time"
)

// A CODING RUN AND A TEXT CALL ON ONE ENTRY NAME ONE MODEL.
//
// ModelArgument is what an agent-mode or code-sandbox run of the entry's own
// CLI is handed, and argv is what every text call passes; both render the
// profile's model_args, an override's provider prefix included. A run that
// rebuilt the value its own way addressed OpenCode's box as
// `anthropic/openrouter/…` while every text call said `openrouter/…`. A box's
// runner writes its own flag, so the run is handed the flag's VALUE: a joined
// `--model={model}` returned whole reached the box as `--model
// '--model=openrouter/…'`.
func TestTheRunsModelIsTheValueEveryTextCallPasses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		model     string
		overrides map[string]any
		want      string
		textArg   string
	}{
		{"as written", "openrouter/anthropic/claude-sonnet-5", nil,
			"openrouter/anthropic/claude-sonnet-5", "openrouter/anthropic/claude-sonnet-5"},
		{"an override's prefix", "claude-sonnet-5",
			map[string]any{"model_args": []any{"--model", "anthropic/{model}"}},
			"anthropic/claude-sonnet-5", "anthropic/claude-sonnet-5"},
		{"a joined flag", "openrouter/anthropic/claude-sonnet-5",
			map[string]any{"model_args": []any{"--model={model}"}},
			"openrouter/anthropic/claude-sonnet-5", "--model=openrouter/anthropic/claude-sonnet-5"},
		{"a joined flag with a prefix", "claude-sonnet-5",
			map[string]any{"model_args": []any{"-m=anthropic/{model}"}},
			"anthropic/claude-sonnet-5", "-m=anthropic/claude-sonnet-5"},
	} {
		p, err := New(Config{
			Key: "oc", Agent: "opencode", Model: tc.model, Overrides: tc.overrides,
			StateDir: t.TempDir(), Timeout: time.Minute,
		})
		if err != nil {
			t.Fatalf("%s: New: %v", tc.name, err)
		}
		if got := p.ModelArgument(); got != tc.want {
			t.Errorf("%s: ModelArgument = %q, want %q", tc.name, got, tc.want)
		}
		// The text call's own argv carries the same value, in the
		// element the profile spells it in.
		if argv := p.argv(); !slices.Contains(argv, tc.textArg) {
			t.Errorf("%s: a text call passes %v, the run %q", tc.name, argv, p.ModelArgument())
		}
	}
}
