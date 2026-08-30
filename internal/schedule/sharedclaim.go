package schedule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The at-most-once guard, on the fleet.
//
// `scheduled_runs` lived in the node's own exclusively-owned database, and the
// scheduler is a singleton DUTY — so it moves, on a lease lapse, a drain or a
// rolling upgrade. The new holder read an EMPTY ledger and its catchup pass
// re-fired everything the previous holder had already claimed: every company
// got two standups, from the one subsystem whose whole contract is that it
// cannot. The scheduler already refuses a nil ledger to prevent exactly this
// ("a scheduler with a process-local claim looks identical to a correct one
// until there are two nodes"), which the per-node table made a refusal of the
// symptom rather than the cause.
//
// The split is the one migration 0011 made between the shared token counter
// and the per-agent token_usage rows: what the FLEET has to agree on is "may I
// start", and nothing more. The node's own ledger stays as its audit record of
// what it dispatched — which is what the dashboard reads and what the
// retention sweep purges.

// FireClaims is the fleet-shared half of the guard, satisfied by
// coord.Fires.
//
// Declared here rather than imported as coord's own interface for the reason
// every consumer interface in this tree is: this package needs one method, and
// a scheduler that could reach the whole coordination store is a scheduler
// whose blast radius nobody can see from its type.
type FireClaims interface {
	// ClaimFire records one fire identity, reporting whether THIS caller
	// wrote it. An error is UNKNOWN and the caller fails closed.
	ClaimFire(ctx context.Context, key string, at time.Time) (bool, error)
}

// ErrNoFireClaims is a SharedClaimer built without the shared half.
var ErrNoFireClaims = errors.New("schedule: no fleet claim store")

// SharedClaimer is the [Claimer] the engine actually wires.
type SharedClaimer struct {
	fires   FireClaims
	history Ledger
}

var _ Claimer = (*SharedClaimer)(nil)

// NewSharedClaimer joins the fleet's claim to this node's audit ledger.
//
// history may be nil — a node with no local store still has to be refused a
// fire a peer has claimed, and losing the audit row is a gap in a dashboard
// rather than a duplicated turn. The shared half is required, because without
// it there is no guard at all.
func NewSharedClaimer(fires FireClaims, history Ledger) (*SharedClaimer, error) {
	if fires == nil {
		return nil, ErrNoFireClaims
	}
	return &SharedClaimer{fires: fires, history: history}, nil
}

// Claim takes the fire on the fleet, then records it locally.
//
// THE ORDER CARRIES THE GUARANTEE. The fleet's answer decides, and the local
// write happens only after it says yes — the other order would record a
// dispatch this node is about to be refused, and an operator reading the
// dashboard would see a fire that never happened.
//
// The local write is BEST EFFORT and its own answer is discarded. It is an
// audit row: failing the claim because the row could not be written would turn
// a full disk into a company whose crons stop, and a `false` from it means the
// row is already there, which is not a reason to skip a fire the fleet has
// just granted.
func (c *SharedClaimer) Claim(ctx context.Context, run Run) (bool, error) {
	// The instant on the shared record is the fire's DUE time, never the
	// wall clock: this package has exactly one clock and it is not here
	// (see clock_guard_test.go), and the due time is the more useful
	// answer anyway — a catchup claim stamped "now" says when the node
	// woke up, while the schedule stamp says which minute was made up.
	// Nothing branches on it; it is what an operator reads out of the
	// bucket when asking which fire a key belongs to.
	at := run.ScheduledAt
	if at.IsZero() {
		at = run.FiredAt
	}
	won, err := c.fires.ClaimFire(ctx, FireClaimKey(run.FireKey), at)
	if err != nil {
		return false, fmt.Errorf("schedule: claim %s on the fleet: %w",
			FireClaimKey(run.FireKey), err)
	}
	if !won {
		return false, nil
	}
	if c.history != nil {
		if _, err := c.history.Claim(ctx, run); err != nil {
			log.WarnContext(ctx, "scheduled_run_not_recorded", "error", err,
				"schedule", run.ScheduleName, "scope_id", run.ScopeID,
				"fire", run.FireLabel,
				"detail", "the fire was claimed and dispatched; only this node's audit row is missing")
		}
	}
	return true, nil
}

// FireClaimKey renders a fire identity as one key, injectively.
//
// [FireKey] is deliberately a struct because a delimiter-joined identity
// COLLIDES the moment a delimiter appears in a name: a unit "Q3|Launch" with a
// schedule "Launch" would join to the same string as a unit "Q3" with a
// schedule "|Launch", and one of the two fires would then be silently
// suppressed by the other. A coordination store has only keys, so the join has
// to happen somewhere — it happens here, once, with each component escaped so
// the mapping stays injective. Same reasoning, and same reason, as the
// resource-name escaping in internal/coord/kv.
func FireClaimKey(k FireKey) string {
	return strings.Join([]string{
		escapeFireComponent(string(k.Scope)),
		escapeFireComponent(k.ScopeID),
		escapeFireComponent(k.ScheduleName),
		escapeFireComponent(k.FireLabel),
		escapeFireComponent(k.TargetHandle),
	}, "|")
}

// escapeFireComponent makes '|' impossible inside a component.
//
// The backslash escapes ITSELF first. Without that step `a\` + `b` and `a` +
// `\b` would both render `a\|b` — the escape character is just another
// delimiter, and an unescaped one reintroduces the collision this exists to
// remove one level down.
func escapeFireComponent(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, "|", `\p`)
}
