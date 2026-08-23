// Package engine assembles one process's participation in a company: the
// config, the org, the models, the tools, and the turn machinery that runs
// between them.
//
// It is the ENTANGLEMENT POINT the rewrite plan names, and the reason it is a
// package of small files rather than one large one. Python's equivalent was a
// 7,500-line module whose two hardest passages — the inbox handler and the
// config apply — could only be read by scrolling past everything else. Here
// each of those is a package of its own (internal/agent/inbox, and the config
// plane), and what remains here is the WIRING: which concrete thing satisfies
// which seam.
package engine

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/credential"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/anthropic"
	"github.com/crewlet/crewlet/internal/providers/llm/openai"
)

// buildProviders turns the company's providers.llm map into the ordered
// registry the per-phase resolver reads.
//
// ORDER COMES FROM THE CONFIG DOCUMENT, not from the map. Resolution's last
// resort is "the first provider configured", which is a real answer over the
// order an operator wrote and no answer at all over a Go map — two seats booted
// from one config would land on different models, and one seat would change
// model across a restart. config.Company.ProviderOrder is what preserves it.
func buildProviders(c *config.Company, r *config.Resolver) (*phase.Registry, error) {
	order := c.Providers.ProviderOrder()
	entries := make([]phase.Entry, 0, len(order))
	for _, key := range order {
		spec, ok := c.Providers.LLM[key]
		if !ok {
			// ProviderOrder is derived from the same map, so this cannot
			// happen — but a silent skip here would produce a registry
			// missing a provider the operator configured, and the seat
			// using it would fall back to another model with nothing said.
			return nil, fmt.Errorf("engine: provider %q is in the order but not in the map", key)
		}
		p, err := buildProvider(key, spec, r)
		if err != nil {
			return nil, err
		}
		entries = append(entries, phase.Entry{Key: key, Provider: p})
	}
	return phase.NewRegistry(entries)
}

// buildProvider constructs one backend.
//
// A provider whose credentials are missing still BUILDS. Every call then comes
// back a clean 401, which names the provider and the vendor — far easier to
// diagnose than a constructor that refused to exist and took the whole company
// down at boot with a message about one key.
func buildProvider(key string, spec config.LLMProvider, r *config.Resolver) (llm.Provider, error) {
	timeout := time.Duration(spec.TimeoutSeconds * float64(time.Second))
	// RESOLVED HERE, at the moment the provider is built, which is the only
	// place a key value ever exists in this process. Tier B stores its
	// references verbatim — that is what keeps an exported revision free of
	// resolved secrets — so a backend handed spec.APIKeys directly would
	// send the literal "${ANTHROPIC_API_KEY}" as its credential and get a
	// 401 that names the vendor rather than the misconfiguration.
	keys := spec.ResolvedKeys(r)
	// Through the accessors, not the raw fields: those apply the bounds and
	// the defaults, and reading the fields directly would send a zero
	// cooldown for every provider that did not configure one.
	cooldowns := credential.Policy{
		RateLimit: time.Duration(spec.Cooldowns.RateLimit()) * time.Second,
		Auth:      time.Duration(spec.Cooldowns.Auth()) * time.Second,
	}

	switch spec.Type {
	case config.LLMAnthropic:
		return anthropic.New(anthropic.Config{
			Model: spec.Model, APIKeys: keys, BaseURL: spec.BaseURL,
			Timeout: timeout, Cooldowns: cooldowns,
			Reasoning: spec.Reasoning, ThinkingBudget: spec.ReasoningBudgetTokens,
		})
	case config.LLMOpenAI, config.LLMOpenAICompatible:
		return openai.New(openai.Config{
			Model: spec.Model, APIKeys: keys, BaseURL: spec.BaseURL,
			Timeout: timeout, Cooldowns: cooldowns,
			Reasoning: spec.Reasoning, ReasoningEffort: string(spec.ReasoningEffort),
		})
	case config.LLMCLIAgent:
		// The subscription-CLI backend is a Phase-4 item that has not
		// landed. Naming it explicitly is the point: an unknown-type error
		// would tell an operator their config is wrong, when in fact it is
		// this build that is incomplete.
		return nil, fmt.Errorf("engine: provider %q: the %q backend is not in this build yet",
			key, spec.Type)
	default:
		return nil, fmt.Errorf("engine: provider %q: unknown type %q", key, spec.Type)
	}
}

// log is the package logger.
var log = logging.Get("engine")
