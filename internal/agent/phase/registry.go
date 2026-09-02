package phase

import (
	"fmt"
	"iter"
	"slices"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

var log = logging.Get("agent.phase")

// Registry is the configured providers.llm map, in CONFIG ORDER.
//
// The order is the point. The last resort of the fallback below is "the first
// provider configured", which is a real answer over an ordered list and no
// answer at all over a Go map — range order is randomised per iteration, so a
// map-backed registry would hand two seats booted from one config two
// different models, and hand the same seat a different one on restart. So the
// order is a field rather than something the map is trusted to remember.
type Registry struct {
	order []string
	byKey map[string]llm.Provider
}

// Entry is one configured provider, in the order it was written.
type Entry struct {
	Key      string
	Provider llm.Provider
}

// NewRegistry builds the registry from config-ordered entries.
//
// An empty registry is refused here rather than at the first turn: a company
// with no models is a config error, and discovering it when a seat tries to
// think reports it as a nil provider deep in a phase, naming neither the seat
// nor the config that produced it.
func NewRegistry(entries []Entry) (*Registry, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("phase: no LLM providers configured")
	}
	r := &Registry{
		order: make([]string, 0, len(entries)),
		byKey: make(map[string]llm.Provider, len(entries)),
	}
	for i, e := range entries {
		switch {
		case e.Key == "":
			return nil, fmt.Errorf("phase: provider %d has no key", i)
		case e.Provider == nil:
			return nil, fmt.Errorf("phase: provider %q has no backend", e.Key)
		case r.byKey[e.Key] != nil:
			return nil, fmt.Errorf("phase: provider %q configured twice", e.Key)
		}
		r.order = append(r.order, e.Key)
		r.byKey[e.Key] = e.Provider
	}
	return r, nil
}

// Keys returns the configured keys in config order.
func (r *Registry) Keys() []string { return slices.Clone(r.order) }

// All yields every configured provider, in config order.
//
// Ordered for the same reason [Registry.Keys] is: a caller equipping every
// backend with something — the fleet's credential ledger, at the time of
// writing — logs what it did, and a log whose lines reshuffle per run is one
// nobody can diff against the config that produced it.
func (r *Registry) All() iter.Seq2[string, llm.Provider] {
	return func(yield func(string, llm.Provider) bool) {
		for _, key := range r.order {
			if !yield(key, r.byKey[key]) {
				return
			}
		}
	}
}

// Has reports whether a key is configured. The config validator uses it to
// reject a role naming a provider that does not exist — which is where that
// typo should die, rather than here where it can only be survived.
func (r *Registry) Has(key string) bool { _, ok := r.byKey[key]; return ok }

// Chain resolves the ordered fallback chain a phase runs on.
//
// Four levels, in priority:
//
//  1. the role's per-phase chain (llm_review, llm_judge, …) if set,
//  2. the role's llm chain,
//  3. the "default" key,
//  4. the first provider in config order.
//
// Levels 3 and 4 exist for the role that names nothing, which is the common
// case: most seats do not care which model they run on. A role that names a
// key the registry LACKS also lands there, and that is a typo silently
// rerouting a seat to a model nobody chose — so it is logged loudly here and
// rejected outright by config validation, which is the only place it can be
// caught before a turn spends tokens on the wrong model.
//
// The returned slice is always non-empty; a nil error guarantees it.
func (r *Registry) Chain(role *org.Role, ph Phase) ([]chain.Member, error) {
	if role == nil {
		return nil, fmt.Errorf("phase: no role")
	}
	if len(r.order) == 0 {
		return nil, fmt.Errorf("phase: no LLM providers configured")
	}

	candidates := roleKeys(role, ph)
	if len(candidates) == 0 {
		candidates = role.LLM
	}

	var members []chain.Member
	var missing []string
	seen := make(map[string]bool, len(candidates))
	for _, key := range candidates {
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if p, ok := r.byKey[key]; ok {
			members = append(members, chain.Member{Key: key, Provider: p})
		} else {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		log.Warn("phase_provider_key_unknown",
			"role", role.Name, "phase", ph.String(),
			"missing", missing, "configured", r.order)
	}
	if len(members) > 0 {
		return members, nil
	}

	if p, ok := r.byKey["default"]; ok {
		return []chain.Member{{Key: "default", Provider: p}}, nil
	}
	first := r.order[0]
	return []chain.Member{{Key: first, Provider: r.byKey[first]}}, nil
}

// Head returns the chain's first member, for callers that want one provider
// and no fallback.
func (r *Registry) Head(role *org.Role, ph Phase) (chain.Member, error) {
	members, err := r.Chain(role, ph)
	if err != nil {
		return chain.Member{}, err
	}
	return members[0], nil
}

// roleKeys returns the role's per-phase chain for ph, before any fallback.
//
// THE EXECUTOR IS NOT HERE, and its absence is the rule: `llm` is the seat's
// model, and the executor is what runs on it. Every phase below is a satellite
// an operator may point somewhere cheaper — the reviewer, a spawned worker,
// the summariser, the round-cap judge, the coding agent — and each falls
// straight through to llm when it names nothing.
func roleKeys(role *org.Role, ph Phase) org.ProviderKeys {
	switch ph {
	case Review:
		return role.LLMReview
	case Subagent:
		return role.LLMSubagent
	case Auxiliary:
		return role.LLMAuxiliary
	case Judge:
		return role.LLMJudge
	case Sandbox:
		return role.LLMSandbox
	default:
		return nil
	}
}

// RoleKeys returns the raw per-phase chain a role declares for ph, with no
// registry and no fallback applied.
//
// Exported for the config validator, which must check every key an operator
// WROTE — including ones the fallback would quietly survive. Resolution and
// validation ask different questions of the same field, and the validator
// asking Chain() would only ever see the answer that hid the problem.
func RoleKeys(role *org.Role, ph Phase) org.ProviderKeys { return roleKeys(role, ph) }

// All is every phase, for callers that must cover the set exhaustively.
var All = []Phase{Execute, Review, Subagent, Auxiliary, Judge, Sandbox}
