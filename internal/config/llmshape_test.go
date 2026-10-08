package config

import (
	"strings"
	"testing"
)

// The controls for the per-model rules in TestCompanyValidatorRejections: the
// same dials where the model takes them load clean, and a model written as a
// `${VAR}` is left to the backend, which judges it once it resolves.
func TestAnAnthropicEntryLoadsWithTheDialsItsModelTakes(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"xhigh on the current generation": "      type: anthropic\n      model: claude-opus-5-5\n      reasoning_effort: xhigh\n",
		"max on Opus 4.6":                 "      type: anthropic\n      model: claude-opus-4-6\n      reasoning_effort: max\n",
		"a budget on Haiku 4.5":           "      type: anthropic\n      model: claude-haiku-4-5\n      reasoning_budget_tokens: 1024\n",
		"a budget and an effort on Opus 4.5 through Vertex": "      type: anthropic\n      model: claude-opus-4-5@20251101\n" +
			"      reasoning_effort: high\n      reasoning_budget_tokens: 63999\n",
		"an alias named by claude_model": "      type: anthropic\n      model: gw-fast\n      claude_model: claude-haiku-4-5\n" +
			"      reasoning_budget_tokens: 2048\n",
		"a referenced model judged at build":  "      type: anthropic\n      model: ${LLM_MODEL}\n      reasoning_budget_tokens: 2048\n",
		"xhigh on an openai reasoning model":  "      type: openai\n      model: gpt-5\n      reasoning: true\n      reasoning_effort: xhigh\n",
		"an unknown id with nothing to judge": "      type: anthropic\n      model: claude-opus-5-7\n      reasoning_effort: max\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mustCompany(t, "name: Acme\nproviders:\n  llm:\n    default:\n"+body)
		})
	}
}

// An anthropic model the table does not know is VALID and worth a warning: it
// is most likely newer than the table, and wrong only as an alias hiding an
// older model, which nothing offline can tell. Located at claude_model, the
// field that settles it, and silent wherever there is nothing to settle.
func TestAnUnknownClaudeModelIsAnAdvisoryAtClaudeModel(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, "name: Acme\nproviders:\n  llm:\n"+
		"    gw:\n      type: anthropic\n      model: gw-fast\n"+
		"    named:\n      type: anthropic\n      model: gw-small\n      claude_model: claude-haiku-4-5\n"+
		"    known:\n      type: anthropic\n      model: us.anthropic.claude-sonnet-5-5-v1:0\n"+
		"    referenced:\n      type: anthropic\n      model: ${LLM_MODEL}\n"+
		"    other:\n      type: openai\n      model: gpt-5\n")
	var got []Warning
	for _, w := range cfg.AdvisoryWarnings() {
		if strings.HasPrefix(w.Path, "providers.") {
			got = append(got, w)
		}
	}
	if len(got) != 1 {
		t.Fatalf("provider advisories = %+v, want exactly the one for gw", got)
	}
	if w := got[0]; w.Path != "providers.llm.gw.claude_model" || w.Kind != WarningAdvisory ||
		!strings.Contains(w.Message, `"gw-fast"`) || !strings.Contains(w.Message, "claude_model") {
		t.Fatalf("advisory = %+v, want it at providers.llm.gw.claude_model naming the model", w)
	}
}
