package phase

import (
	"errors"
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
//
// A NIL *Registry IS THE COMPANY WITH NO MODELS, and every method answers for
// it: no keys, no providers, and a Chain or Head that refuses with
// [ErrNoProviders]. An empty providers.llm is a valid company (an org chart
// written before its credentials exist), and the epoch spells it as no
// registry at all rather than an empty one. Nil-safe rather than merely
// documented, because several consumers hold the registry behind an
// interface, where a nil pointer is not a nil interface: a guard written as
// `models == nil` passes it through, and the first method call would have
// been a panic deep inside a turn.
type Registry struct {
	order []string
	byKey map[string]llm.Provider
}

// Entry is one configured provider, in the order it was written.
type Entry struct {
	Key      string
	Provider llm.Provider
}

// ErrNoProviders reports a model resolution against a company that configures
// no providers.llm at all.
//
// Worded for the operator who reads it at the end of a failed call, because
// the fix is always the same edit: the company is valid, it is running, and
// what it lacks is one entry under providers.llm.
var ErrNoProviders = errors.New("phase: the company configures no model provider; " +
	"add one under providers.llm")

// NewRegistry builds the registry from config-ordered entries.
//
// An empty list is refused: a registry answers which model a seat runs on,
// and one holding nothing has no answer to give. The company that configures
// no model is not built through here at all; it has no registry (see the
// nil-receiver rule on [Registry]), which is the one spelling of that state
// every consumer reads.
func NewRegistry(entries []Entry) (*Registry, error) {
	if len(entries) == 0 {
		return nil, ErrNoProviders
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

// Keys returns the configured keys in config order, and none for a nil
// registry.
func (r *Registry) Keys() []string {
	if r == nil {
		return nil
	}
	return slices.Clone(r.order)
}

// All yields every configured provider, in config order.
//
// Ordered for the same reason [Registry.Keys] is: a caller equipping every
// backend with something — the fleet's credential ledger, at the time of
// writing — logs what it did, and a log whose lines reshuffle per run is one
// nobody can diff against the config that produced it.
func (r *Registry) All() iter.Seq2[string, llm.Provider] {
	return func(yield func(string, llm.Provider) bool) {
		if r == nil {
			return
		}
		for _, key := range r.order {
			if !yield(key, r.byKey[key]) {
				return
			}
		}
	}
}

// Provider resolves ONE configured key, with no chain and no fallback.
//
// For a caller that was given an explicit model name — a worker template, a
// delegate task — where falling back would defeat the point of naming it. A
// key that misses is (nil, false) rather than a substitution, so the caller
// can refuse and say which keys exist.
func (r *Registry) Provider(key string) (llm.Provider, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.byKey[key]
	return p, ok
}

// Has reports whether a key is configured. The config validator uses it to
// reject a role naming a provider that does not exist — which is where that
// typo should die, rather than here where it can only be survived.
func (r *Registry) Has(key string) bool {
	if r == nil {
		return false
	}
	_, ok := r.byKey[key]
	return ok
}

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
// The returned slice is always non-empty; a nil error guarantees it. A
// registry with nothing to resolve (nil, or a zero value built without
// [NewRegistry]) refuses with [ErrNoProviders].
func (r *Registry) Chain(role *org.Role, ph Phase) ([]chain.Member, error) {
	if role == nil {
		return nil, fmt.Errorf("phase: no role")
	}
	if r == nil || len(r.order) == 0 {
		return nil, ErrNoProviders
	}
	keys, missing := resolve(role, ph, r.order, r.Has)
	if len(missing) > 0 {
		log.Warn("phase_provider_key_unknown",
			"role", role.Name, "phase", ph.String(),
			"missing", missing, "configured", r.order)
	}
	members := make([]chain.Member, 0, len(keys))
	for _, key := range keys {
		members = append(members, chain.Member{Key: key, Provider: r.byKey[key]})
	}
	return members, nil
}

// Resolve is [Registry.Chain] as provider KEYS: the ordered chain role runs ph
// on, given the company's providers.llm keys in config order, with no provider
// built. The first key is the model the phase runs on and every later one is
// a fallback, in the order the chain tries them.
//
// THE SAME FUNCTION Chain resolves through, rather than a description of it,
// because it is published: the org projection shows every seat's chain to the
// people watching the company, and a chain worked out beside the one a turn
// runs on is one that is right until either rule moves. Nil for a role that is
// nil and for a company with no providers, which has no chain to show.
func Resolve(role *org.Role, ph Phase, order []string) []string {
	if role == nil || len(order) == 0 {
		return nil
	}
	known := make(map[string]bool, len(order))
	for _, key := range order {
		known[key] = true
	}
	keys, _ := resolve(role, ph, order, func(key string) bool { return known[key] })
	return keys
}

// resolve is the four levels [Registry.Chain] documents, over keys alone:
// the chain it lands on, never empty for a non-empty order, and every key the
// role named that the company does not configure.
func resolve(role *org.Role, ph Phase, order []string, has func(string) bool) (keys, missing []string) {
	candidates := roleKeys(role, ph)
	if len(candidates) == 0 {
		candidates = role.LLM
	}
	seen := make(map[string]bool, len(candidates))
	for _, key := range candidates {
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if has(key) {
			keys = append(keys, key)
		} else {
			missing = append(missing, key)
		}
	}
	if len(keys) > 0 {
		return keys, missing
	}
	if has("default") {
		return []string{"default"}, missing
	}
	return []string{order[0]}, missing
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
	case Onboarding:
		// NO FIELD OF ITS OWN, deliberately: onboarding reads the team's
		// pages and writes conventions once, on a seat's first turn, and
		// an operator pointing that at a different model from the rest of
		// the seat's work has no reason to. Nil resolves to role.LLM.
		return nil
	default:
		return nil
	}
}

// All is every phase, for callers that must cover the set exhaustively.
//
// ONBOARDING IS IN IT. It was left out because it is the one phase with no
// per-phase provider field of its own — it resolves to the role's default
// chain — but "has no own field" is a fact about [roleKeys], not a reason to
// be missing from the set. A caller iterating this to cover every phase was
// silently skipping the one that runs FIRST on a seat's first turn.
var All = []Phase{Execute, Review, Subagent, Auxiliary, Judge, Sandbox, Onboarding}
