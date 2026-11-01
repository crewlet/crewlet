package config

import (
	"errors"
	"strings"
	"testing"
)

// opencodeEntry is a company whose one entry drives OpenCode on model.
func opencodeEntry(model string, lines ...string) string {
	var b strings.Builder
	b.WriteString("name: Acme\nproviders:\n  llm:\n    oc:\n")
	b.WriteString("      model: " + model + "\n      harness: opencode\n")
	for _, line := range lines {
		b.WriteString("      " + line + "\n")
	}
	return b.String()
}

// AN OPENCODE MODEL NAMES ITS PROVIDER, OR EVERY CALL FAILS.
//
// OpenCode splits its model flag at the first slash, so a bare
// `claude-sonnet-5` is the provider "claude-sonnet-5" with no model and every
// call — its runs on this host and an executor run, handed the same value — is
// "Invalid model reference". Refused on a write at the field, admitted on apply,
// because the provider builds and the grammar is a vendor fact a newer peer
// may know better.
func TestAnOpenCodeModelWithNoProviderIsRefused(t *testing.T) {
	t.Parallel()
	for _, model := range []string{"claude-sonnet-5", `"/claude-sonnet-5"`, "anthropic/"} {
		doc := opencodeEntry(model)
		err := rejects(t, doc, "providers.llm.oc.model")
		if !errors.Is(err, ErrConflict) {
			t.Errorf("%s: want %v, got %v", model, ErrConflict, err)
		}
		if !strings.Contains(err.Error(), "splits it at the first slash") {
			t.Errorf("%s: the refusal does not say why:\n%v", model, err)
		}
		assertAdmissionOnly(t, doc, "providers.llm.oc.model")
	}
	for _, model := range []string{"anthropic/claude-sonnet-5", "openrouter/anthropic/claude-sonnet-5"} {
		mustCompany(t, opencodeEntry(model))
	}
	// Judged on the value the harness is handed: an override that puts the
	// provider in the flag admits a bare model, and one a profile that
	// declares nothing about the grammar is not judged at all.
	mustCompany(t, opencodeEntry("claude-sonnet-5",
		`overrides: {model_args: ["--model", "anthropic/{model}"]}`))
	mustCompany(t, opencodeEntry("claude-sonnet-5", "overrides: {model_names_provider: false}"))
	mustCompany(t, strings.Replace(opencodeEntry("sonnet"), "harness: opencode", "harness: claude-code", 1))
	// A reference is the store's and the environment's to fill in.
	mustCompany(t, opencodeEntry("${OC_MODEL}"))
}

// THE VALUE IS JUDGED, NOT THE ELEMENT CARRYING IT. A joined `--model={model}`
// is the same flag as the split form, so a bare model in it is refused and a
// qualified one admitted — judged as the element whole, `--model=anthropic` was
// the provider of every value.
func TestAJoinedModelFlagIsJudgedOnItsValue(t *testing.T) {
	t.Parallel()
	joined := `overrides: {model_args: ["--model={model}"]}`
	mustCompany(t, opencodeEntry("anthropic/claude-sonnet-5", joined))
	err := rejects(t, opencodeEntry("claude-sonnet-5", joined), "providers.llm.oc.model")
	if !strings.Contains(err.Error(), `"claude-sonnet-5" names the provider "claude-sonnet-5"`) {
		t.Errorf("the refusal does not judge the flag's value:\n%v", err)
	}
}

// THE REFUSAL SPEAKS FOR THE HARNESS IT IS ABOUT. Any profile can declare the
// grammar through an override, and a hermes entry told about OpenCode's error
// and `opencode models` is sent to a tool it does not run.
func TestTheModelGrammarRefusalNamesOnlyItsOwnHarness(t *testing.T) {
	t.Parallel()
	err := rejects(t, opencodeEntry("claude-sonnet-5"), "providers.llm.oc.model")
	if !strings.Contains(err.Error(), "opencode models") {
		t.Errorf("an opencode entry is not pointed at its own model listing:\n%v", err)
	}
	hermes := strings.Replace(opencodeEntry("claude-sonnet-5", "overrides: {model_names_provider: true}"),
		"harness: opencode", "harness: hermes", 1)
	err = rejects(t, hermes, "providers.llm.oc.model")
	if strings.Contains(err.Error(), "OpenCode") || strings.Contains(err.Error(), "opencode models") {
		t.Errorf("a hermes entry is pointed at OpenCode:\n%v", err)
	}
}
