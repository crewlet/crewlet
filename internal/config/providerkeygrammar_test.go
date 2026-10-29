package config

import (
	"errors"
	"strings"
	"testing"
)

// providerKeysDoc is a company whose providers.llm holds one claude-code entry
// under each key given.
func providerKeysDoc(keys ...string) string {
	var b strings.Builder
	b.WriteString("name: Acme\nproviders:\n  llm:\n")
	for _, key := range keys {
		b.WriteString("    " + yamlQuote(key) + ":\n      model: sonnet\n")
	}
	return b.String()
}

func yamlQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

// A PROVIDER KEY NAMES A DIRECTORY AND A VARIABLE, so it is held to a grammar
// both can carry. The default state_dir joins the key under the CLI home, so a
// `/` nested one entry's login inside another's and `..` left the home; the
// bundle variable upper-cases it, so `Fast` and `fast` were two keys with one
// bundle. Refused on a write, warned on apply: the entry still runs.
func TestAProviderKeyOutsideItsGrammarIsRefused(t *testing.T) {
	t.Parallel()
	for _, key := range []string{
		"team/fast", "..", "Fast", "-fast", ".fast", "fast model", "fast:1",
		strings.Repeat("a", 64),
	} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			doc := providerKeysDoc(key)
			path := "providers.llm." + key
			err := rejects(t, doc, path)
			if !errors.Is(err, ErrShape) || !strings.Contains(err.Error(), "is not a provider key") {
				t.Errorf("want a shape refusal naming the grammar, got %v", err)
			}
			assertAdmissionOnly(t, doc, path)
		})
	}
}

// The grammar admits every key a person writes for a model, the longest one
// included, and the operator's DNS-label resource names.
func TestAProviderKeyInsideItsGrammarIsAccepted(t *testing.T) {
	t.Parallel()
	for _, key := range []string{
		"default", "gpt-5", "claude_opus", "sonnet.4", "0", "4o-mini",
		strings.Repeat("a", 63),
	} {
		if !ProviderKeyPattern.MatchString(key) {
			t.Errorf("ProviderKeyPattern refuses %q", key)
		}
		mustCompany(t, providerKeysDoc(key))
	}
}

// TWO KEYS MAY NOT SHARE ONE CREDENTIAL BUNDLE. The bundle variable maps `.`,
// `_` and `-` alike, so `gpt-5`, `gpt_5` and `gpt.5` all read
// CREWLET_LLM_CLI_GPT_5_CREDENTIALS: each entry was restored the others'
// login, and an export from one overwrote the others'. Every key after the
// first is reported, naming the one it collides with.
func TestTwoProviderKeysMayNotShareABundleVariable(t *testing.T) {
	t.Parallel()
	doc := providerKeysDoc("gpt-5", "gpt_5", "gpt.5")
	cfg, err := ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	problems := Problems(cfg.Validate())
	reported := map[string]string{}
	for _, p := range problems {
		reported[p.Path] = p.Message
	}
	// Sorted: gpt-5 < gpt.5 < gpt_5, so gpt-5 is the first and is fine.
	if _, ok := reported["providers.llm.gpt-5"]; ok {
		t.Errorf("the first key is reported too: %+v", problems)
	}
	for _, key := range []string{"gpt.5", "gpt_5"} {
		message, ok := reported["providers.llm."+key]
		if !ok {
			t.Errorf("%s is not reported: %+v", key, problems)
			continue
		}
		if !strings.Contains(message, `"gpt-5"`) || !strings.Contains(message, "CREWLET_LLM_CLI_GPT_5_CREDENTIALS") {
			t.Errorf("%s's refusal does not name the key and the variable it shares: %s", key, message)
		}
	}
	assertAdmissionOnly(t, doc, "providers.llm.gpt_5")

	// Keys that differ in a letter or a digit read bundles of their own.
	mustCompany(t, providerKeysDoc("gpt-5", "gpt-6", "gpt-5a"))
}
