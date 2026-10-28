package cliagent

import (
	"slices"
	"testing"
	"time"
)

// THE EXECUTOR'S RUN AND A RUN ON THIS HOST ON ONE ENTRY NAME ONE MODEL.
//
// ModelArgument is what the executor's run of the entry's own CLI is handed,
// and argv is what every run on this host passes; both render the
// profile's model_args, an override's provider prefix included. A run that
// rebuilt the value its own way addressed OpenCode's box as
// `anthropic/openrouter/…` while every run on this host said `openrouter/…`. A box's
// runner writes its own flag, so the run is handed the flag's VALUE: a joined
// `--model={model}` returned whole reached the box as `--model
// '--model=openrouter/…'`.
func TestTheRunsModelIsTheValueEveryRunOnThisHostPasses(t *testing.T) {
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
		// A run on this host carries the same value in its argv, in the
		// element the profile spells it in.
		if argv := p.argv(false); !slices.Contains(argv, tc.textArg) {
			t.Errorf("%s: a run on this host passes %v, the executor's run %q", tc.name, argv, p.ModelArgument())
		}
	}
}
