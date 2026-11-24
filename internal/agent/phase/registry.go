package phase

import (
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
)

var log = logging.Get("agent.phase")

// Registry is the configured providers.llm map, in CONFIG ORDER.
//
// Order is a field because the fallback's last resort is "the first provider
// configured", which a map's randomised range order cannot answer.
//
// A nil *Registry is the company with no models (valid: an org chart written
// before its credentials exist), and every method answers for it: no keys, no
// providers, and a Chain or Head refusing with [ErrNoProviders]. It is nil-safe
// because consumers hold it behind an interface, where a nil pointer is not a
// nil interface, so `models == nil` passes it through and a call would panic.
type Registry struct {
	order []string
	byKey map[string]llm.Provider

	// fleets are the company's LLM fleets, default-fleet included, and
	// balance what a fleet's members are read by at a pick.
	fleets  map[string]Fleet
	balance Balance
}

// Fleet is one LLM fleet as a phase is resolved against it (config's
// providers.llm_fleets, default-fleet synthesised).
type Fleet struct {
	Strategy fleet.Strategy
	// Threshold is threshold_points, its default resolved.
	Threshold int
	// Members are providers.llm keys in list order.
	Members []string
}

// Models is what a phase is resolved against: the providers.llm keys in
// config order and the fleets by key.
type Models struct {
	Order  []string
	Fleets map[string]Fleet
}

// DefaultFleet is the fleet every company has, which a seat naming nothing
// runs on once it has a member (config.DefaultFleetKey).
const DefaultFleet = "default-fleet"

// fleetPrefix starts a reference to a fleet (config.FleetRefPrefix).
const fleetPrefix = "fleet:"

// Balance is what a registry reads a fleet's members by at a pick.
type Balance struct {
	// Ledger is the node's balancer memory, and its clock.
	Ledger *fleet.Ledger
	// Signals is what this node knows of one member's sign-in now.
	Signals func(key string) fleet.Signals
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

// NewFleetRegistry is [NewRegistry] resolving the company's fleets too: a
// phase set to `fleet:<key>`, or naming nothing once default-fleet has a
// member, runs on the member b's balancer picks for each piece of work.
func NewFleetRegistry(entries []Entry, fleets map[string]Fleet, b Balance) (*Registry, error) {
	if b.Ledger == nil || b.Signals == nil {
		return nil, errors.New("phase: a fleet registry needs a ledger and a reading of its members")
	}
	r, err := NewRegistry(entries)
	if err != nil {
		return nil, err
	}
	r.fleets, r.balance = fleets, b
	return r, nil
}

// EmptyFleetError is a phase set to a fleet with no member: it cannot run,
// and waits until one joins.
type EmptyFleetError struct {
	// Fleet is the fleet's key.
	Fleet string
}

func (e *EmptyFleetError) Error() string {
	return fmt.Sprintf("phase: fleet %q has no members yet: the seat waits until one joins", e.Fleet)
}

// models is what r resolves against.
func (r *Registry) models() Models { return Models{Order: r.order, Fleets: r.fleets} }

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

// Chain resolves the ordered fallback chain a phase runs on, by [Resolve]'s
// levels: the role's per-phase chain, its llm chain, default-fleet once it has
// a member, the "default" key, the first provider in config order. A level
// naming a fleet is that fleet. A key the registry lacks falls through,
// logged here and refused by config validation before a turn spends tokens.
//
// A fleet's chain is its balancer's walk for this seat job now, read without
// recording anything: what runs work takes [Registry.Walk]. A fleet with no
// member is an [*EmptyFleetError]; otherwise the slice is non-empty whenever
// the error is nil. A registry with nothing to resolve refuses with
// [ErrNoProviders].
func (r *Registry) Chain(role *org.Role, ph Phase) ([]chain.Member, error) {
	w, err := r.Walk(role, ph)
	return w.Members, err
}

// Walk is a phase's chain for a piece of work that runs on it: the members,
// and on a fleet the balancer's pick of each, index for index, which the
// chain takes as its member takes the call ([chain.Options.Fleet]) and a
// launch as it takes the run. Picks is nil on a pinned chain.
type Walk struct {
	Members []chain.Member
	Picks   []*fleet.Pick
}

// Walk is [Registry.Chain] with the picks a fleet made: see [Walk].
func (r *Registry) Walk(role *org.Role, ph Phase) (Walk, error) {
	if role == nil {
		return Walk{}, fmt.Errorf("phase: no role")
	}
	if r == nil || len(r.order) == 0 {
		return Walk{}, ErrNoProviders
	}
	res, missing := resolve(role, ph, r.models())
	if len(missing) > 0 {
		log.Warn("phase_provider_key_unknown",
			"role", role.Name, "phase", ph.String(),
			"missing", missing, "configured", r.order)
	}
	if res.Fleet != "" {
		return r.fleetWalk(role, ph, res)
	}
	members := make([]chain.Member, 0, len(res.Keys))
	for _, key := range res.Keys {
		members = append(members, chain.Member{Key: key, Provider: r.byKey[key]})
	}
	return Walk{Members: members}, nil
}

// fleetWalk is the walk res's fleet takes for role's ph now.
func (r *Registry) fleetWalk(role *org.Role, ph Phase, res Resolution) (Walk, error) {
	picks, err := r.FleetWalk(res.Fleet, fleet.Job{Fleet: res.Fleet, Subject: role.Handle(), Job: ph.String()})
	if err != nil {
		return Walk{}, err
	}
	members := make([]chain.Member, 0, len(picks))
	for _, pick := range picks {
		members = append(members, chain.Member{Key: pick.Key, Provider: r.byKey[pick.Key]})
	}
	return Walk{Members: members, Picks: picks}, nil
}

// Home is the member that took job's last piece of work on this node, "" for
// none (or a registry with no fleets).
func (r *Registry) Home(job fleet.Job) string {
	if r == nil || r.balance.Ledger == nil {
		return ""
	}
	return r.balance.Ledger.Home(job)
}

// FleetWalk is fleet key's balancer walk for job now, for work outside a
// seat's phases (a helper's turn): picks the caller takes as a member takes
// the work. An unknown fleet or one with no member the registry configures
// is an [*EmptyFleetError].
func (r *Registry) FleetWalk(key string, job fleet.Job) ([]*fleet.Pick, error) {
	if r == nil || r.balance.Ledger == nil {
		return nil, &EmptyFleetError{Fleet: key}
	}
	spec := r.fleets[key]
	f := fleet.Fleet{Key: key, Strategy: spec.Strategy, Threshold: spec.Threshold}
	now := r.balance.Ledger.Now()
	for _, member := range spec.Members {
		if !r.Has(member) || slices.ContainsFunc(f.Members, func(m fleet.Member) bool { return m.Key == member }) {
			continue
		}
		signals := r.balance.Signals(member)
		signals.Key, signals.LastServed = member, r.balance.Ledger.LastServed(member)
		f.Members = append(f.Members, fleet.Read(signals, now))
	}
	picks := r.balance.Ledger.Walk(f, job)
	if len(picks) == 0 {
		return nil, &EmptyFleetError{Fleet: key}
	}
	return picks, nil
}

// Resolution is what a phase runs on: the entries a call may try, the
// level that decided them, and the fleet they come from.
type Resolution struct {
	// Keys are a pinned chain's entries in the order it tries them, or a
	// fleet's members in list order (its balancer orders them per piece of
	// work). Empty only for a fleet with no member.
	Keys []string
	// Source is the level that decided.
	Source Source
	// Fleet is the fleet's key, "" for a pinned chain.
	Fleet string
}

// Resolve is [Registry.Chain] as provider KEYS: what role runs ph on, given
// the company's models, with no provider built and no pick made.
//
// THE SAME FUNCTION Chain resolves through, rather than a description of it,
// because it is published: the org projection shows every seat's chain to the
// people watching the company, and a chain worked out beside the one a turn
// runs on is one that is right until either rule moves. Zero for a role that is
// nil and for a company with no providers, which has no chain to show.
//
// "Written" and "decided" differ exactly where it matters: a seat whose field
// names only keys the company lacks wrote a field and decided nothing, and its
// phase runs on the fallback, so Source credits it with nothing.
func Resolve(role *org.Role, ph Phase, models Models) Resolution {
	if role == nil || len(models.Order) == 0 {
		return Resolution{}
	}
	res, _ := resolve(role, ph, models)
	return res
}

// Source is which of [Registry.Chain]'s levels decided a phase's chain.
type Source string

const (
	// SourceSeat is the phase's own field on the seat: `llm` for the
	// executor, `llm_<phase>` (or the same key inside the `llm` mapping)
	// for a satellite.
	SourceSeat Source = "seat"
	// SourceExecutor is a phase with no field of its own written (or none
	// at all, onboarding's case) that fell through to the seat's `llm`, its
	// executor's chain.
	SourceExecutor Source = "executor"
	// SourceFleet is default-fleet, for a seat whose fields name nothing
	// the company has, once it has a member. A fleet a field names is that
	// field's level; [Resolution.Fleet] names the fleet either way.
	SourceFleet Source = "fleet"
	// SourceDefaultEntry is the providers.llm entry keyed "default", for a
	// seat whose fields name no configured entry.
	SourceDefaultEntry Source = "default-entry"
	// SourceFirstEntry is the first entry in config order, for a company
	// with no "default" entry.
	SourceFirstEntry Source = "first-entry"
)

// Sources is the closed set, in [Registry.Chain]'s order.
var Sources = []Source{SourceSeat, SourceExecutor, SourceFleet, SourceDefaultEntry, SourceFirstEntry}

// Valid reports whether s is one of [Sources].
func (s Source) Valid() bool { return slices.Contains(Sources, s) }

// resolve is the levels [Registry.Chain] documents, over keys alone: what it
// lands on, never empty for a non-empty order but on a fleet with no member,
// and every key the role named that the company does not configure.
func resolve(role *org.Role, ph Phase, models Models) (res Resolution, missing []string) {
	known := make(map[string]bool, len(models.Order))
	for _, key := range models.Order {
		known[key] = true
	}
	candidates, source := roleKeys(role, ph), SourceSeat
	if len(candidates) == 0 {
		candidates = role.LLM
		// The executor's own field IS `llm`, and so is onboarding's
		// only chain: only a satellite that named nothing fell through.
		if ph != Execute {
			source = SourceExecutor
		}
	}
	// A FLEET STANDS ALONE: config refuses `fleet:` inside a list, so a
	// reference is the whole field, and one inside a list names no entry.
	if len(candidates) == 1 {
		if key, ok := strings.CutPrefix(candidates[0], fleetPrefix); ok {
			if f, ok := models.Fleets[key]; ok {
				return Resolution{Keys: membersOf(f, known), Source: source, Fleet: key}, nil
			}
		}
	}
	var keys []string
	seen := make(map[string]bool, len(candidates))
	for _, key := range candidates {
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if known[key] {
			keys = append(keys, key)
		} else {
			missing = append(missing, key)
		}
	}
	if len(keys) > 0 {
		return Resolution{Keys: keys, Source: source}, missing
	}
	// UNSET RUNS ON default-fleet ONCE IT HAS A MEMBER; until then the
	// fallbacks below, so a company that never set one up does not stop.
	if f, ok := models.Fleets[DefaultFleet]; ok {
		if members := membersOf(f, known); len(members) > 0 {
			return Resolution{Keys: members, Source: SourceFleet, Fleet: DefaultFleet}, missing
		}
	}
	if known["default"] {
		return Resolution{Keys: []string{"default"}, Source: SourceDefaultEntry}, missing
	}
	return Resolution{Keys: []string{models.Order[0]}, Source: SourceFirstEntry}, missing
}

// membersOf is f's members the company configures, in list order: config
// refuses any other, so none is dropped but by a revision built by hand.
func membersOf(f Fleet, known map[string]bool) []string {
	var out []string
	for _, key := range f.Members {
		if known[key] && !slices.Contains(out, key) {
			out = append(out, key)
		}
	}
	return out
}

// Take is [Registry.Head] for a piece of work that runs on it now: on an LLM
// fleet, the balancer's pick, taken, so it becomes the seat job's home.
func (r *Registry) Take(role *org.Role, ph Phase) (chain.Member, error) {
	w, err := r.Walk(role, ph)
	if err != nil {
		return chain.Member{}, err
	}
	if len(w.Picks) > 0 {
		w.Picks[0].Take()
	}
	return w.Members[0], nil
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
// the summariser, the round-cap judge, the busy seat's wake triage — and each falls straight through to
// llm when it names nothing.
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
	case Triage:
		return role.LLMTriage
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
var All = []Phase{Execute, Review, Subagent, Auxiliary, Judge, Onboarding, Triage}
