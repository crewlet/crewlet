package config_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// default-fleet exists for every reading while the document does not hold it,
// and a read of the document never carries it until somebody writes it.
func TestDefaultFleetIsSynthesisedAndNeverServedUnwritten(t *testing.T) {
	t.Parallel()
	var p config.Providers
	f, ok := p.Fleet(config.DefaultFleetKey)
	if !ok || f.Name != config.DefaultFleetKey || f.EffectiveStrategy() != config.FleetBalanced || len(f.Members) != 0 {
		t.Fatalf("Fleet(default-fleet) = %+v, %v; want the synthesised empty balanced fleet", f, ok)
	}
	if got := p.FleetKeys(); !slices.Equal(got, []string{config.DefaultFleetKey}) {
		t.Fatalf("FleetKeys() = %v, want [default-fleet]", got)
	}
	if p.DefaultFleetWritten() {
		t.Fatal("DefaultFleetWritten() on an empty document = true")
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "llm_fleets") {
		t.Fatalf("an unwritten default-fleet is served: %s", data)
	}

	p.LLMFleets = map[string]config.LLMFleet{
		config.DefaultFleetKey: {Name: config.DefaultFleetKey, Members: []string{"a"}},
		"claude-opus":          {Name: "Claude Opus", Strategy: config.FleetSequential},
	}
	if f, _ := p.Fleet(config.DefaultFleetKey); !slices.Equal(f.Members, []string{"a"}) {
		t.Fatalf("a written default-fleet reads %+v, want its members", f)
	}
	if got := p.FleetKeys(); !slices.Equal(got, []string{"claude-opus", config.DefaultFleetKey}) {
		t.Fatalf("FleetKeys() = %v", got)
	}
	if _, ok := p.Fleet("nope"); ok {
		t.Fatal("Fleet(nope) found a fleet")
	}
}

// A fleet reference is told apart from an entry key by its prefix alone,
// which no entry key can carry.
func TestAFleetReferenceIsNeverAnEntryKey(t *testing.T) {
	t.Parallel()
	if key, ok := config.FleetOf(config.FleetRef("claude-opus")); !ok || key != "claude-opus" {
		t.Fatalf("FleetOf(FleetRef) = %q, %v", key, ok)
	}
	if _, ok := config.FleetOf("claude-max"); ok {
		t.Fatal("an entry key read as a fleet reference")
	}
	if config.ProviderKeyPattern().MatchString(config.FleetRef("x")) {
		t.Fatal("a fleet reference matches the entry key grammar")
	}
}

// A key made from a name is in the key grammar, and a clash takes the next
// free number.
func TestSuggestFleetKeyIsAValidFreeKey(t *testing.T) {
	t.Parallel()
	taken := map[string]bool{"claude-opus": true, "claude-opus-2": true}
	cases := []struct{ name, want string }{
		{"Claude Opus", "claude-opus-3"},
		{"  GPT-5 Codex!! ", "gpt-5-codex"},
		{"Team / Night shift", "team-night-shift"},
		{"???", "fleet"},
		{strings.Repeat("a", 70), strings.Repeat("a", 63)},
	}
	for _, tc := range cases {
		got := config.SuggestFleetKey(tc.name, func(k string) bool { return taken[k] })
		if got != tc.want || !config.ValidFleetKey(got) {
			t.Errorf("SuggestFleetKey(%q) = %q (valid %v), want %q", tc.name, got, config.ValidFleetKey(got), tc.want)
		}
	}
}

// The class a fleet holds one of comes from the model id, the id itself when
// no family names it.
func TestModelClass(t *testing.T) {
	t.Parallel()
	for model, want := range map[string]string{
		"opus": "Claude Opus", "claude-sonnet-5": "Claude Sonnet", "claude-haiku-4-5": "Claude Haiku",
		"gpt-5-codex": "GPT-5 Codex", "openrouter/qwen3": "openrouter/qwen3",
	} {
		if got := config.ModelClass(model); got != want {
			t.Errorf("ModelClass(%q) = %q, want %q", model, got, want)
		}
	}
}
