package harness

import (
	"slices"
	"testing"
	"time"
)

// EVERY HARNESS WITH AN EFFORT SETTING IS HANDED THE ENTRY'S LEVEL its own way,
// on a call with tools and without: a flag after the model's, the model's
// variant, or a variable.
func TestAnEntrysEffortReachesItsHarnessItsOwnWay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		harness, model, level string
		args                  []string
		env                   string
	}{
		{"claude-code", "sonnet", "xhigh", []string{"--model", "sonnet", "--effort", "xhigh"}, ""},
		{"codex", "gpt-5.5", "high", []string{"--model", "gpt-5.5", "-c", `model_reasoning_effort="high"`}, ""},
		{"opencode", "anthropic/claude-sonnet-5", "max", []string{"--model", "anthropic/claude-sonnet-5#max"}, ""},
		{"copilot", "gpt-5.5", "low", []string{"--model", "gpt-5.5", "--reasoning-effort", "low"}, ""},
		{"grok", "grok-4.6", "high", []string{"--model", "grok-4.6", "--reasoning-effort", "high"}, ""},
		{"muse-code", "muse-1", "xhigh", []string{"--model", "muse-1", "--reasoning-effort", "xhigh"}, ""},
		{"hermes", "openrouter/auto", "medium", []string{"--model", "openrouter/auto", "--reasoning", "medium"}, ""},
		{"pi", "anthropic/claude-sonnet-5", "off", []string{"--model", "anthropic/claude-sonnet-5", "--thinking", "off"}, ""},
		{"kimi-code", "kimi-k3", "max", []string{"--model", "kimi-k3"}, "KIMI_MODEL_THINKING_EFFORT=max"},
	} {
		p, err := New(Config{Key: "e", Harness: tc.harness, Model: tc.model, Effort: tc.level,
			StateDir: t.TempDir(), Timeout: time.Minute})
		if err != nil {
			t.Fatalf("%s: New: %v", tc.harness, err)
		}
		for _, tools := range []bool{false, true} {
			if argv := p.argv(tools); !slices.Equal(argv[len(argv)-len(tc.args):], tc.args) {
				t.Errorf("%s (tools %v): argv ends %q, want %q", tc.harness, tools,
					argv[max(0, len(argv)-len(tc.args)):], tc.args)
			}
		}
		env := buildEnv(p.profile, &Checkout{Home: t.TempDir(), Cache: t.TempDir(), Work: t.TempDir()},
			p.callEnv(), p.auth)
		if tc.env != "" && !slices.Contains(env, tc.env) {
			t.Errorf("%s: the call's environment lacks %s", tc.harness, tc.env)
		}
		if p.Effort() != tc.level {
			t.Errorf("%s: Effort = %q, want %q", tc.harness, p.Effort(), tc.level)
		}
	}
}

// NO EFFORT, NO SETTING: an entry that names none passes nothing, so the harness
// runs at its own default exactly as before.
func TestAnEntryWithNoEffortPassesNone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ harness, model string }{
		{"claude-code", "sonnet"}, {"codex", "gpt-5.5"}, {"opencode", "anthropic/claude-sonnet-5"},
		{"kimi-code", "kimi-k3"}, {"pi", "anthropic/claude-sonnet-5"},
	} {
		p, err := New(Config{Key: "e", Harness: tc.harness, Model: tc.model,
			StateDir: t.TempDir(), Timeout: time.Minute})
		if err != nil {
			t.Fatalf("%s: New: %v", tc.harness, err)
		}
		argv := p.argv(false)
		for _, flag := range []string{"--effort", "--reasoning-effort", "--reasoning", "--thinking"} {
			if slices.Contains(argv, flag) {
				t.Errorf("%s: argv carries %s with no effort: %q", tc.harness, flag, argv)
			}
		}
		if slices.ContainsFunc(argv, func(a string) bool { return a == tc.model+"#" || len(a) > 0 && a[len(a)-1] == '#' }) {
			t.Errorf("%s: the model carries an empty variant: %q", tc.harness, argv)
		}
		if _, set := p.callEnv()["KIMI_MODEL_THINKING_EFFORT"]; set {
			t.Errorf("%s: the call's environment sets the effort variable with no effort", tc.harness)
		}
	}
}
