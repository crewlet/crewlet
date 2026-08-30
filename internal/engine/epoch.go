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
// EPOCH — nothing is ever mutated in place (adrs/404). Applying a revision by
// mutating the live objects keeps their identity, so that anything holding a
// reference keeps working. That is
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
//
// The second return is the subsystems this apply GOT THROUGH, in the order it
// went through them. On a failure it is what was already mutated when the
// refusal happened, which is the whole of what makes a degraded apply
// diagnosable after the fact — it travels on ConfigRevisionApplied into the
// audit event log, where it outlives the fleet view's one-minute bucket.
func (e *Engine) Apply(ctx context.Context, cfg *config.Company) (configplane.ApplyStatus, []string, error) {
	var applied []string
	// THE SNAPSHOT FIRST, because re-activating an unchanged revision is
	// the documented rotation gesture: the payload has not moved, so the
	// only thing that can have is what its ${VAR} references resolve to.
	// Rebuilding the epoch without re-reading the store would make that
	// gesture a no-op and rotation impossible without a restart.
	e.refreshSecrets(ctx)
	applied = append(applied, "secrets")
	next, err := NewCompanyWith(cfg, e.resolver())
	if err != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the revision was refused before anything changed; "+
				"this node still serves the previous epoch")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	applied = append(applied, "company")
	// Equipped before it is published, for the same reason as at boot: a
	// turn can start the instant the pointer moves, and a revision that
	// silently dropped every builtin would look like a model that stopped
	// using its tools.
	if err := e.equip(ctx, next); err != nil {
		log.WarnContext(ctx, "config_apply_failed", "error", err,
			"detail", "the revision built but could not be equipped with this "+
				"node's tools; the previous epoch is still current")
		return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
	}
	applied = append(applied, "tools")
	// The learning workers are rebuilt for the new epoch — they hold its
	// org and its model registry — while the dispatcher, its subscription
	// and its redelivery ring stay put. A failure leaves the previous
	// epoch's workers serving rather than failing the apply: reflecting
	// against a stale org is a far smaller wrong than not reflecting.
	e.reconfigureReflection(next)
	applied = append(applied, "learning")
	// The sandbox MANAGER is swapped, and only the manager: the coordinator
	// and the waiter hold this process's busy set and poll loop, so
	// rebuilding them would forget which seats are mid-run and start a
	// second loop against the same rows. A revision whose provider block is
	// broken is refused here rather than published — the alternative serves
	// a company whose sandbox-enabled seats plan around a box that will
	// never be minted.
	if e.sandboxCoordinator != nil {
		manager, err := buildSandbox(next.Config, e.resolver(), e.sandboxOtel)
		if err != nil {
			log.WarnContext(ctx, "config_apply_failed", "error", err,
				"detail", "the revision's providers.sandbox could not be built; "+
					"the previous epoch is still current")
			return configplane.StatusError, applied, fmt.Errorf("engine: apply: %w", err)
		}
		if manager != nil {
			e.sandboxCoordinator.SetManager(manager)
		}
		applied = append(applied, "sandbox")
	}

	// The party index is rebuilt BEFORE the epoch is published, and the
	// order is a choice between two brief windows. Refreshing first means
	// a seat the revision REMOVED stays addressable for an instant, which
	// costs a recorded skip. Refreshing after means a seat the revision
	// ADDED is unresolvable while the epoch that has it is already
	// current — and during a rollout the new company is the one being
	// adopted, so the window that favours it is the right one.
	e.refreshParties(next)
	applied = append(applied, "parties")
	// The TRACKER is rebuilt on the same edge and for the same reason: its
	// lead map is derived from the org, so a node that kept its boot-time
	// parser would route the new revision's work items by the old
	// company's org chart.
	e.reconcileConfluence(next)
	e.reconcileJira(ctx, next)
	e.reconcileGitLab(ctx, next)
	e.reconcileGitHub(ctx, next)
	applied = append(applied, "integrations")

	previous := e.Company()
	e.epoch.current.Store(next)
	applied = append(applied, "epoch")

	// AFTER the epoch is published, because this reads the seat list off
	// the CURRENT company: a revision that adds a role adds a seat, and
	// until something creates its mailbox every event published to it is
	// dropped rather than retained. Nil on an engine built without a node
	// — `crewlet validate` applies to nothing.
	if e.node != nil {
		e.node.EnsureMailboxes(ctx)
		applied = append(applied, "mailboxes")
	}
	// AFTER the epoch is published too, and for a sharper version of the
	// same reason: the tick reads schedules off the CURRENT company, so
	// arming from `next` before it is current would open a window in which
	// the loop fires the outgoing company's crons. A founder's first
	// schedule starts the loop here; their last one removed stops it.
	e.reconcileScheduler(ctx, next)
	applied = append(applied, "scheduler")

	log.InfoContext(ctx, "config_applied",
		"company", next.Config.Name, "seats", len(next.Seats()),
		"previous_seats", seatCount(previous))
	// LAST, after the epoch is current and everything derived from it has
	// been rebuilt, so a surface that reads the company on this signal
	// reads the one now serving rather than the one being replaced.
	e.notifyApplied(ctx)
	return configplane.StatusOK, applied, nil
}

// seatCount reports a possibly-absent epoch's seat count, for the log line
// that says what changed. The first apply on a node has no previous epoch.
func seatCount(c *Company) int {
	if c == nil {
		return 0
	}
	return len(c.Seats())
}
