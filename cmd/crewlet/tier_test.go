package main

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// A DOCUMENT IS RECOGNISED BY THE KEYS IT REALLY HAS.
//
// The vocabularies were restated by hand here and had drifted the way a
// restated list does: `agents` was counted and appears nowhere in the tree,
// the schema, the examples or the docs, while `roles`, `units`,
// `mcp_servers`, `turn_engine` and `scheduling` were missing. A units-only
// document — the commonest authoring shape, and what a `validate -json` fix
// loop is most often pointed at — scored nothing on either side and was
// reported undecidable.
func TestATierIsDetectedFromTheKeysADocumentReallyHas(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		doc  string
		want Tier
	}{
		{"a units-only company", "units:\n  - name: Engineering\n", TierCompany},
		{"a roles-only company", "roles:\n  - name: SWE\n", TierCompany},
		{"mcp servers alone", "mcp_servers:\n  - name: tracker\n", TierCompany},
		{"turn engine alone", "turn_engine:\n  max_tool_rounds: 4\n", TierCompany},
		{"scheduling alone", "scheduling:\n  tick_seconds: 30\n", TierCompany},
		{"a named company", "name: Acme\n", TierCompany},
		{"a store-only bootstrap", "store:\n  path: ./x.db\n", TierBootstrap},
		{"a node-only bootstrap", "node:\n  id: n1\n", TierBootstrap},
		{"an api-only bootstrap", "api:\n  port: 8000\n", TierBootstrap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := detectTier([]byte(tc.doc))
			if err != nil {
				t.Fatalf("detectTier: %v", err)
			}
			if got != tc.want {
				t.Errorf("tier = %q, want %q", got, tc.want)
			}
		})
	}
}

// A document with nothing either tier owns is still an ERROR naming -tier,
// never a guess: guessing wrong reports every field of the file as invalid,
// and an operator reading that cannot tell it from a broken document.
func TestADocumentWithNoTierKeysIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := detectTier([]byte("colour: blue\n")); err == nil {
		t.Fatal("a document belonging to neither tier was assigned one")
	}
}

// AND THE TWO VOCABULARIES STAY DISJOINT. They are derived by subtracting
// each tier's keys from the other's, so an overlap can only mean the
// derivation broke — and an overlapping key would score for both sides and
// make every document carrying it undecidable.
func TestTheTierVocabulariesShareNoKey(t *testing.T) {
	t.Parallel()
	bootstrap := config.BootstrapKeys()
	for _, key := range config.CompanyKeys() {
		if slices.Contains(bootstrap, key) {
			t.Errorf("%q counts for both tiers", key)
		}
	}
	// And neither is empty, which is what a broken derivation looks like:
	// every document would then be undecidable.
	if len(config.CompanyKeys()) == 0 || len(bootstrap) == 0 {
		t.Fatal("a tier vocabulary came back empty")
	}
	// The key the hand-written list invented must not reappear.
	if slices.Contains(config.CompanyKeys(), "agents") {
		t.Error("`agents` is not a key any config struct declares")
	}
}
