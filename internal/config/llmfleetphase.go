package config

import (
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
)

// PhaseModels is what a seat's phase is resolved against ([phase.Resolve]):
// the providers.llm keys in order and every fleet, default-fleet synthesised.
func (p *Providers) PhaseModels() phase.Models {
	fleets := p.Fleets()
	out := phase.Models{Order: p.ProviderOrder(), Fleets: make(map[string]phase.Fleet, len(fleets))}
	for key, f := range fleets {
		out.Fleets[key] = phase.Fleet{Strategy: fleet.Strategy(f.EffectiveStrategy()),
			Threshold: f.Threshold(), Members: f.Members}
	}
	return out
}
