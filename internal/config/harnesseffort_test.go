package config

import (
	"errors"
	"strings"
	"testing"
)

// effortEntry is a company whose one entry drives harness on model at effort.
func effortEntry(harness, model, effort string, lines ...string) string {
	var b strings.Builder
	b.WriteString("name: Acme\nproviders:\n  llm:\n    m:\n")
	b.WriteString("      harness: " + harness + "\n      model: " + model + "\n")
	if effort != "" {
		b.WriteString("      effort: " + effort + "\n")
	}
	for _, line := range lines {
		b.WriteString("      " + line + "\n")
	}
	return b.String()
}

// AN EFFORT THE HARNESS TAKES IS ADMITTED, in its own words, and none at all is
// the harness's default as before.
func TestAnEffortTheHarnessTakesIsAdmitted(t *testing.T) {
	t.Parallel()
	for _, tc := range [][3]string{
		{"claude-code", "sonnet", ""},
		{"claude-code", "opus", "max"},
		{"claude-code", "claude-opus-4-6", "max"},
		{"codex", "gpt-5.5", "minimal"},
		{"opencode", "anthropic/claude-sonnet-5", "high"},
		{"pi", "anthropic/claude-sonnet-5", "off"},
		{"kimi-code", "kimi-k3", "high"},
		{"claude-code", "${CLAUDE_MODEL}", "xhigh"},
	} {
		mustCompany(t, effortEntry(tc[0], tc[1], tc[2]))
	}
}

// AN EFFORT THAT WOULD REACH NOTHING IS REFUSED, NEVER DROPPED: a harness with
// no setting the engine passes, a level the harness does not take, and a level
// the harness does not take on the entry's model. Each refusal says why, and is
// an admission rule: the levels are a vendor fact a newer peer may know more of.
func TestAnEffortThatWouldReachNothingIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		harness, model, effort string
		kind                   error
		says                   string
	}{
		{"gemini-cli", "gemini-3-pro", "high", ErrConflict, "only from its settings.json"},
		{"cursor-agent", "gpt-5.5", "high", ErrConflict, "only as part of the model id"},
		{"claude-code", "sonnet", "minimal", ErrUnknownValue, `"low", "medium", "high", "xhigh" or "max", not "minimal"`},
		{"pi", "anthropic/claude-sonnet-5", "none", ErrUnknownValue, `not "none"`},
		{"claude-code", "claude-haiku-4-5", "low", ErrUnknownValue, "runs claude-haiku-4-5 with no reasoning effort"},
		{"claude-code", "claude-opus-4-5", "max", ErrUnknownValue, `on claude-opus-4-5, not "max"`},
	} {
		doc := effortEntry(tc.harness, tc.model, tc.effort)
		err := rejects(t, doc, "providers.llm.m.effort")
		if !errors.Is(err, tc.kind) || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s %s %s: got %v, want %v saying %q", tc.harness, tc.model, tc.effort, err, tc.kind, tc.says)
		}
		assertAdmissionOnly(t, doc, "providers.llm.m.effort")
	}
	err := rejects(t, effortEntry("custom", "m", "high", `overrides: {binary: x, complete_args: [x], `+
		`output: text}`), "providers.llm.m.effort")
	if !strings.Contains(err.Error(), "declare overrides.effort") {
		t.Errorf("a custom entry is not told how to declare an effort:\n%v", err)
	}
}

// AN OVERRIDE IS HOW A NEWER RELEASE'S LEVEL IS ADMITTED, with no Crewlet release.
func TestAnEffortOverrideAdmitsANewLevel(t *testing.T) {
	t.Parallel()
	doc := effortEntry("claude-code", "opus", "ultra")
	rejects(t, doc, "providers.llm.m.effort")
	mustCompany(t, effortEntry("claude-code", "opus", "ultra",
		`overrides: {effort: {levels: [low, medium, high, xhigh, max, ultra]}}`))
}
