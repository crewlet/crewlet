package notify

import (
	"hash/fnv"
	"maps"
	"strings"
)

// The words a working indicator shows while an agent reasons.
//
// # The rule every line here follows
//
// A phrase says the agent is BUSY and nothing else. It never claims a fact
// about this particular turn — not what the agent is doing, not what it has
// found, not how far along it is — because the line is drawn from a pool
// before any of that is known, and a reader cannot tell a decorative claim
// from a reported one. "is thinking it through" is safe; "is reading your
// ticket" is a lie eight times out of nine.
//
// The built-ins lean on the engine's own name; a company running Crewlet
// will want its own, and overrides layer per phase.

// DefaultPhase names the pool a phase with none of its own falls back to.
const DefaultPhase = "default"

// PhasePhrases are the shipped pools. Treat as immutable.
var PhasePhrases = map[string][]string{
	"onboarding": {
		"is getting crewleted in...",
		"is getting up to speed...",
		"is settling in...",
		"is getting its bearings...",
		"is finding its feet...",
		"is learning the secret handshake...",
		"is finding the coffee machine...",
		"is unpacking its desk...",
	},
	// One pool where there were two. The turn's own work is one phase now,
	// and it thinks and acts inside it — so the planning lines are not
	// stale, they are the first half of what `execute` covers, and both
	// pools are merged rather than one of them dropped.
	"execute": {
		"is crewleting...",
		"is crewing...",
		"is thinking...",
		"is scheming...",
		"is thinking it through...",
		"is mulling it over...",
		"is putting the pieces together...",
		"is working on it...",
		"is doing the doing...",
		"is rolling up its sleeves...",
		"is cracking on...",
		"is making things happen...",
		"is getting it done...",
	},
	"review": {
		"is re-crewleting...",
		"is double-checking...",
		"is marking its own homework...",
		"is squinting at its own work...",
		"is sanity-checking...",
		"is having one more look...",
		"is asking itself the hard questions...",
		"is doing one last pass...",
	},
	DefaultPhase: {
		"is crewleting...",
		"is thinking...",
		"is on it...",
		"is busy being useful...",
	},
}

// Phrases are the pools one company draws its working status from.
type Phrases struct{ pools map[string][]string }

// NewPhrases layers an operator's overrides over the shipped pools, per
// phase, so a company can replace only the phases it cares about.
//
// An absent or empty pool KEEPS the built-in one: "override nothing" and
// "override with nothing" are the same request, and an empty status is not a
// rendering — it is a cleared indicator, which is the opposite of what an
// operator writing a phrase list is asking for.
func NewPhrases(overrides map[string][]string) Phrases {
	pools := make(map[string][]string, len(PhasePhrases))
	for phase, pool := range PhasePhrases {
		pools[phase] = pool
	}
	for phase, pool := range overrides {
		cleaned := make([]string, 0, len(pool))
		for _, p := range pool {
			if p = strings.TrimSpace(p); p != "" {
				cleaned = append(cleaned, p)
			}
		}
		if len(cleaned) > 0 {
			pools[phase] = cleaned
		}
	}
	return Phrases{pools: pools}
}

// Pools exposes a copy, for the operator surface that shows what a company
// will say.
func (p Phrases) Pools() map[string][]string { return maps.Clone(p.pools) }

// Pick chooses a phase's line for a session rotation steps in.
//
// DETERMINISTIC in (seed, phase, rotation), and that is the whole design:
// the heartbeat re-asserts the same status every refresh interval, so a line
// that moved between re-assertions would flicker under a reader who is
// watching it. Different turns in one thread start from different points in
// the pool, because the seed is the turn's.
//
// A stable hash rather than a runtime one: Go's map hash is seeded per
// process, so a restart mid-conversation would jump to an unrelated line for
// the same turn.
func (p Phrases) Pick(phase, seed string, rotation int) string {
	pool := p.pools[phase]
	if len(pool) == 0 {
		pool = p.pools[DefaultPhase]
	}
	if len(pool) == 0 {
		pool = PhasePhrases[DefaultPhase]
	}
	h := fnv.New64a()
	h.Write([]byte(seed))
	h.Write([]byte{'|'})
	h.Write([]byte(phase))
	// Rotation can be negative only through a caller's arithmetic error;
	// the modulo is taken on a uint so a wrapped index cannot panic.
	idx := (h.Sum64() + uint64(rotation)) % uint64(len(pool))
	return pool[idx]
}
