package config_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
)

// The balancer's strategies are config's, word for word: a strategy config
// admits that the balancer does not know would route by neither rule.
func TestFleetStrategiesAreConfigs(t *testing.T) {
	t.Parallel()
	var want []string
	for _, s := range config.LLMFleetStrategies {
		want = append(want, string(s))
	}
	var got []string
	for _, s := range fleet.Strategies {
		got = append(got, string(s))
	}
	if !slices.Equal(got, want) {
		t.Errorf("fleet.Strategies = %v, config.LLMFleetStrategies = %v", got, want)
	}
}

// What a phase resolves against carries every fleet with its defaults
// resolved, default-fleet synthesised while unwritten.
func TestPhaseModelsCarriesEveryFleet(t *testing.T) {
	t.Parallel()
	p := config.Providers{LLMFleets: map[string]config.LLMFleet{
		"opus": {Name: "Claude Opus", Strategy: config.FleetSequential, Members: []string{"a", "b"}},
	}}
	got := p.PhaseModels()
	opus := got.Fleets["opus"]
	if opus.Strategy != fleet.Sequential || opus.Threshold != config.DefaultFleetThreshold ||
		!slices.Equal(opus.Members, []string{"a", "b"}) {
		t.Errorf("opus = %+v, want sequential over a, b at the default threshold", opus)
	}
	def, ok := got.Fleets[phase.DefaultFleet]
	if !ok || def.Strategy != fleet.Balanced || len(def.Members) != 0 {
		t.Errorf("default-fleet = %+v (%v), want the synthesised empty balanced fleet", def, ok)
	}
	if phase.DefaultFleet != config.DefaultFleetKey {
		t.Errorf("phase.DefaultFleet = %q, config.DefaultFleetKey = %q", phase.DefaultFleet, config.DefaultFleetKey)
	}
	role := &config.Role{Name: "Engineer", LLM: config.PhaseLLM{Default: config.ProviderKeys{config.FleetRef("opus")}}}
	withEntries := config.Providers{LLM: map[string]config.LLMProvider{"a": {}, "b": {}}, LLMFleets: p.LLMFleets}
	if res := phase.Resolve(role.Seat(), phase.Execute, withEntries.PhaseModels()); res.Fleet != "opus" ||
		!slices.Equal(res.Keys, []string{"a", "b"}) {
		t.Errorf("a seat set to fleet:opus resolves to %+v, want the fleet's members", res)
	}
}
