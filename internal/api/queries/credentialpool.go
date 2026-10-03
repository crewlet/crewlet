// Every model's key bag, and which of its keys a vendor is refusing now.

package queries

import (
	"context"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/providers/credential"
)

// CredentialKeyState is one configured key's condition, decided here so a
// screen never re-derives it from a deadline and a clock.
type CredentialKeyState string

const (
	// KeyReady — a call can lease it now.
	KeyReady CredentialKeyState = "ready"
	// KeyCooling — benched after a rate-limit or auth refusal, by this node
	// or a peer, until `cooling_until`.
	KeyCooling CredentialKeyState = "cooling"
	// KeyUnresolved — it resolved to nothing on this node: a `${VAR}` no
	// secret or environment variable sets, or the conventional variable of
	// an entry that names no key. It is not in the pool at all.
	KeyUnresolved CredentialKeyState = "unresolved"
	// KeyDuplicate — the same value as an earlier key, which the pool holds
	// once. `same_as` names that key; its state is this one's.
	KeyDuplicate CredentialKeyState = "duplicate"
)

// CredentialKeyStates is every key state, for the gate holding the
// dashboard's copy.
var CredentialKeyStates = []CredentialKeyState{KeyReady, KeyCooling, KeyUnresolved, KeyDuplicate}

// Valid reports whether s is a state this build sends.
func (s CredentialKeyState) Valid() bool { return slices.Contains(CredentialKeyStates, s) }

// CredentialKeySource is where a configured key's value comes from.
type CredentialKeySource string

const (
	// KeyFromReference — a whole `${VAR}`, named by `ref`.
	KeyFromReference CredentialKeySource = "reference"
	// KeyFromDefault — the vendor's conventional variable, named by `ref`,
	// read because the entry names no api_keys at all.
	KeyFromDefault CredentialKeySource = "default"
	// KeyInline — a value written into the document itself. It has no name
	// to show, only its position; `ref` is empty.
	KeyInline CredentialKeySource = "inline"
)

// CredentialKeySources is every source, for the gate.
var CredentialKeySources = []CredentialKeySource{KeyFromReference, KeyFromDefault, KeyInline}

// Valid reports whether s is a source this build sends.
func (s CredentialKeySource) Valid() bool { return slices.Contains(CredentialKeySources, s) }

// CredentialPoolState is one provider entry's condition as a whole.
type CredentialPoolState string

const (
	// PoolReady — every key resolves and none is cooling.
	PoolReady CredentialPoolState = "ready"
	// PoolDegraded — at least one key can be leased, and at least one
	// cannot: cooling, or resolving to nothing. Calls still land, on fewer
	// keys than configured.
	PoolDegraded CredentialPoolState = "degraded"
	// PoolExhausted — every key that resolves is cooling: each call on this
	// entry falls through to the seat's next model until one lifts.
	PoolExhausted CredentialPoolState = "exhausted"
	// PoolNoKey — no key resolves at all: every call is a 401.
	PoolNoKey CredentialPoolState = "no_key"
	// PoolLogin — a cli-agent entry: one login held by the CLI, no key bag,
	// nothing that rotates or cools.
	PoolLogin CredentialPoolState = "login"
)

// CredentialPoolStates is every entry state, for the gate.
var CredentialPoolStates = []CredentialPoolState{PoolReady, PoolDegraded, PoolExhausted, PoolNoKey, PoolLogin}

// Valid reports whether s is a state this build sends.
func (s CredentialPoolState) Valid() bool { return slices.Contains(CredentialPoolStates, s) }

// CredentialPoolAnswer is `credential_pool`.
type CredentialPoolAnswer struct {
	// Node is the node that answered: `uses` and `in_flight` are its own
	// pool's counts, and `unresolved` is what ITS environment and the
	// company's secrets resolve to.
	Node string `json:"node"`
	// Fleet is whether the fleet's cooldown ledger was read. False leaves
	// every `cooling_until` this node's own view — a key a peer benched in
	// the last refresh interval reads ready — and FleetError says why.
	Fleet      bool   `json:"fleet"`
	FleetError string `json:"fleet_error"`
	// Providers in config order — the order a seat that names no model
	// falls back through. ALWAYS A LIST.
	Providers []CredentialPoolRow `json:"providers"`
}

// CredentialPoolRow is one `providers.llm` entry.
type CredentialPoolRow struct {
	Key   string              `json:"key"`
	Type  string              `json:"type"`
	Model string              `json:"model"`
	State CredentialPoolState `json:"state"`
	// Ready counts the keys a call could lease now.
	Ready int `json:"ready"`
	// RateLimitSeconds and AuthSeconds are the bench times in force,
	// defaults applied.
	RateLimitSeconds int `json:"rate_limit_seconds"`
	AuthSeconds      int `json:"auth_seconds"`
	// Keys in declaration order. Empty for a `login` entry.
	Keys []CredentialKeyRow `json:"keys"`
}

// CredentialKeyRow is one configured key. It has no member a value could
// travel in: a variable's NAME, where the key came from, and a 12-character
// hint (the one the engine's `credential_cooled` log lines carry) that no one
// can reverse.
type CredentialKeyRow struct {
	Ref    string              `json:"ref"`
	Source CredentialKeySource `json:"source"`
	Hint   string              `json:"hint"`
	State  CredentialKeyState  `json:"state"`
	// CoolingUntil is when a cooling key lifts — the later of this node's
	// bench and the fleet's record. Null unless cooling.
	CoolingUntil *time.Time `json:"cooling_until"`
	// SameAs is the 1-based position of the key a duplicate repeats, 0
	// otherwise.
	SameAs int `json:"same_as"`
	// Uses and InFlight are this node's leases of it since the epoch was
	// built.
	Uses     int `json:"uses"`
	InFlight int `json:"in_flight"`
}

// credentialPool answers every model's key bag and its cooldowns.
//
// THE FLEET'S LEDGER AND THIS NODE'S POOL, the later deadline winning. The
// ledger is the company's answer — a bench any node found is on it — and the
// pool adds the one case the ledger misses: a bench whose publish failed, which
// this node still honours. The pool alone is not enough either, because it
// learns a peer's bench only on its next refresh, and a screen reading a key
// the fleet benched a second ago as ready would send an operator looking for a
// fault in the wrong place.
//
// An UNREADABLE LEDGER IS NOT AN ERROR here, unlike the lease table under
// `fleet`: this node's pool is still a true answer about this node, so it is
// sent, marked `fleet: false`, with the reason — never passed off as the
// company's.
func (s Sources) credentialPool(ctx context.Context, _ Params) (any, error) {
	now := s.clock()
	out := CredentialPoolAnswer{Node: s.NodeID, Providers: []CredentialPoolRow{}}

	var ledger map[string]time.Time
	switch {
	case s.Cooldowns == nil:
		out.FleetError = "this node has no fleet cooldown ledger"
	default:
		read, err := s.Cooldowns.Since(ctx, now)
		if err != nil {
			out.FleetError = err.Error()
		} else {
			ledger, out.Fleet = read, true
		}
	}

	for _, pool := range s.CredentialPools() {
		out.Providers = append(out.Providers, credentialRow(pool, ledger, now))
	}
	return out, nil
}

// credentialRow is one entry, judged.
func credentialRow(pool engine.CredentialPool, ledger map[string]time.Time, now time.Time) CredentialPoolRow {
	row := CredentialPoolRow{
		Key:              pool.Provider,
		Type:             string(pool.Type),
		Model:            pool.Model,
		RateLimitSeconds: int(pool.RateLimit / time.Second),
		AuthSeconds:      int(pool.Auth / time.Second),
		Keys:             make([]CredentialKeyRow, 0, len(pool.Keys)),
	}
	if !pool.Pooled {
		row.State = PoolLogin
		return row
	}

	first := map[string]int{}
	resolved, cooling := 0, 0
	for i, key := range pool.Keys {
		k := CredentialKeyRow{
			Ref: key.Ref, Source: KeyFromReference, Hint: key.Hint,
			Uses: key.Uses, InFlight: key.InFlight,
		}
		switch {
		case key.Default:
			k.Source = KeyFromDefault
		case key.Inline:
			k.Source = KeyInline
		}
		switch {
		case key.Hint == "":
			k.State = KeyUnresolved
		case key.Duplicate:
			k.State, k.SameAs = KeyDuplicate, first[key.Hint]
		default:
			first[key.Hint] = i + 1
			resolved++
			until := now.Add(key.Cooling)
			if fleet, ok := ledger[credential.FleetKey(pool.Provider, key.Hint)]; ok && fleet.After(until) {
				until = fleet
			}
			if until.After(now) {
				k.State = KeyCooling
				at := until.UTC()
				k.CoolingUntil = &at
				cooling++
			} else {
				k.State = KeyReady
				row.Ready++
			}
		}
		row.Keys = append(row.Keys, k)
	}

	switch {
	case resolved == 0:
		row.State = PoolNoKey
	case cooling == resolved:
		row.State = PoolExhausted
	case row.Ready == len(pool.Keys)-countDuplicates(row.Keys):
		row.State = PoolReady
	default:
		row.State = PoolDegraded
	}
	return row
}

func countDuplicates(keys []CredentialKeyRow) int {
	n := 0
	for _, k := range keys {
		if k.State == KeyDuplicate {
			n++
		}
	}
	return n
}
