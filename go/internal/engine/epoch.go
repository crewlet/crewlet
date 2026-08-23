package engine

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
)

// The epoch and how a new one replaces it.
//
// A CONFIG REVISION IS AN IMMUTABLE EPOCH, AND APPLYING ONE PUBLISHES A NEW
// EPOCH — nothing is ever mutated in place (rewrite/decisions/404). The Python
// this replaces applied a revision by mutating the live objects and keeping
// their identity, so that anything holding a reference kept working. That is
// precisely the problem: anything holding a reference kept working, and kept
// reading, mid-turn, values from two different revisions. A turn that read the
// budget cap before the swap and the model chain after it ran under a company
// that never existed, and nothing raised.
//
// Here a turn PINS the epoch once at the top and reads only that for the rest
// of the turn. A revision published mid-turn is simply not observed by that
// turn — the guarantee, not a limitation. The next turn gets it.

// epoch holds the current company.
//
// An atomic pointer and nothing else: readers never block, and there is no
// lock because there is only ever one writer — see [Engine.Apply].
type epoch struct {
	current atomic.Pointer[Company]
}

// Company is the epoch this engine is running.
//
// Every caller that needs more than one fact from it must take it ONCE and
// read that value, not call this twice: two calls can straddle a publish, and
// a caller that read the org from one epoch and the models from the next is
// running a company that never existed. That is the whole hazard this design
// exists to remove, and it is removable only at the call site.
func (e *Engine) Company() *Company { return e.epoch.current.Load() }

// Apply publishes a new epoch, reporting what happened to this node.
//
// The build comes first and touches nothing: [NewCompany] validates, resolves
// the org and constructs the providers without reaching the network. So a
// revision that cannot be built is refused with the previous epoch still
// current and still correct — there is no rollback path because there was no
// mutation, which is the point of publishing rather than mutating.
//
// The three outcomes belong to the control plane, not to this function's
// convenience:
//
//   - ok      — published, and this node is serving it.
//   - error   — refused; this node still serves the PRIOR epoch correctly,
//     which is a legitimate degraded-but-correct state and safe to route to.
//   - degraded — the apply failed AFTER a restart-required subsystem was
//     mutated, so rollback could not restore it.
//
// DEGRADED IS NOT REACHABLE YET, and saying so is more useful than a hook with
// nothing behind it. It becomes reachable when the first subsystem that cannot
// be un-applied is wired: the per-role MCP children (Phase 6) and the
// notification transports (Phase 7). Both are applied LAST when they arrive,
// for exactly this reason — every step before them rolls back by publishing
// the previous epoch, so the window in which degraded is reachable is as small
// as the ordering can make it.
// ONE CALLER: the reconciler's tick, which is synchronous. The API's write
// path does NOT reach here — it activates a revision and lets the tick apply
// it, because activation also has to move the pointer, record the outcome and
// reset the attempt budget, none of which this function does. A lock here
// would guard a path that has never had a second writer, and would imply a
// concurrency story that does not exist.
func (e *Engine) Apply(ctx context.Context, cfg *config.Company) (configplane.ApplyStatus, error) {
	next, err := NewCompany(cfg)
	if err != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the revision was refused before anything changed; "+
				"this node still serves the previous epoch")
		return configplane.StatusError, fmt.Errorf("engine: apply: %w", err)
	}
	// Equipped before it is published, for the same reason as at boot: a
	// turn can start the instant the pointer moves, and a revision that
	// silently dropped every builtin would look like a model that stopped
	// using its tools.
	if err := e.equip(next); err != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the revision built but could not be equipped with this "+
				"node's tools; the previous epoch is still current")
		return configplane.StatusError, fmt.Errorf("engine: apply: %w", err)
	}
	// The sandbox MANAGER is swapped, and only the manager: the coordinator
	// and the waiter hold this process's busy set and poll loop, so
	// rebuilding them would forget which seats are mid-run and start a
	// second loop against the same rows. A revision whose provider block is
	// broken is refused here rather than published — the alternative serves
	// a company whose sandbox-enabled seats plan around a box that will
	// never be minted.
	if e.sandboxCoordinator != nil {
		manager, err := buildSandbox(next.Config)
		if err != nil {
			log.WarnContext(ctx, "config_apply_failed", "error", err,
				"detail", "the revision's providers.sandbox could not be built; "+
					"the previous epoch is still current")
			return configplane.StatusError, fmt.Errorf("engine: apply: %w", err)
		}
		if manager != nil {
			e.sandboxCoordinator.SetManager(manager)
		}
	}

	// The party index is rebuilt BEFORE the epoch is published, and the
	// order is a choice between two brief windows. Refreshing first means
	// a seat the revision REMOVED stays addressable for an instant, which
	// costs a recorded skip. Refreshing after means a seat the revision
	// ADDED is unresolvable while the epoch that has it is already
	// current — and during a rollout the new company is the one being
	// adopted, so the window that favours it is the right one.
	e.refreshParties(next)

	previous := e.Company()
	e.epoch.current.Store(next)

	log.InfoContext(ctx, "config_applied",
		"company", next.Config.Name, "seats", len(next.Seats()),
		"previous_seats", seatCount(previous))
	return configplane.StatusOK, nil
}

// seatCount reports a possibly-absent epoch's seat count, for the log line
// that says what changed. The first apply on a node has no previous epoch.
func seatCount(c *Company) int {
	if c == nil {
		return 0
	}
	return len(c.Seats())
}
