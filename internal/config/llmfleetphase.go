package config

import (
	"slices"
	"strings"

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

// FleetKind is how the entry signs in, as an LLM fleet reads it: a plan
// under auth.mode subscription, a key under api-key or inherit-env, and under
// env a gateway where its env signs in with a gateway's token or switch,
// else a cloud provider's account.
func (l LLMProvider) FleetKind() fleet.Kind {
	switch l.Auth.Mode {
	case "", AuthSubscription:
		return fleet.KindPlan
	case AuthEnv:
		for name, value := range l.Env {
			if strings.TrimSpace(value) != "" && slices.Contains(gatewaySignIns, name) {
				return fleet.KindGateway
			}
		}
		return fleet.KindCloud
	}
	return fleet.KindAPI
}

// gatewaySignIns are the env variables that sign a harness in through a
// gateway rather than a cloud provider's account (harnessprofile's
// sign_in_env lists both kinds).
var gatewaySignIns = []string{"ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_GATEWAY"}
