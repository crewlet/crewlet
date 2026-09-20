package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/schedule/sqlledger"
	"github.com/crewlet/crewlet/internal/tracker"
)

// maintenanceDutyName is the fleet singleton the retention sweep claims.
const maintenanceDutyName = "maintenance"

// maintenanceDutyTTL is how long the duty survives without a re-claim.
//
// Three ticks, matching the sandbox waiter's ratio: one missed tick must not
// hand the duty to a peer, because a sweep is a burst of range deletes and
// two nodes doing it at once is exactly what the singleton avoids. Three
// ticks is long enough to ride out a slow claim and short enough that a dead
// node's duty is picked up within the hour.
const maintenanceDutyTTL = 3 * maintenance.Interval

// startMaintenance arms the retention sweep.
//
// EVERY short-horizon table in this process's stores, in one worker. They
// were all designed to be swept — each migration says so and each ships the
// index for it — and a `purge` that exists on every store with nothing calling
// it sweeps none of them. Wiring them here,
// rather than each subsystem arming its own loop, is what makes that
// impossible to repeat quietly: a store with a Purge and no entry in this
// function is visible in one place.
//
// Started LAST, like the sandbox waiter, because the duty is claimed under
// the node's own incarnation.
func (e *Engine) startMaintenance(ctx context.Context) {
	var jobs []maintenance.Job
	if db := e.backends.Store; db != nil {
		// Every one of these tables lives in this one store, which is
		// what makes a single list of them honest rather than a
		// coincidence: a node's short-horizon state IS its local index.
		jobs = append(jobs, maintenance.StoreJobs(db)...)
		jobs = append(jobs, maintenance.LearningJobs(learning.NewDiary(db))...)
		jobs = append(jobs, maintenance.CounterpartyJobs(learning.NewCounterparties(db))...)
		jobs = append(jobs, maintenance.ScheduleJobs(sqlledger.New(db.SQL()))...)
		jobs = append(jobs, maintenance.LedgerJobs(
			ledgerstore.NewConversations(db),
			e.ConversationRetention())...)
	}
	if fleet := e.backends.Fleet; fleet != nil {
		// The one shared surface swept here, and the exception the
		// coordination store's own retention rule makes room for: its
		// buckets expire on age, and a bucket's age cannot tell an OPEN
		// channel from a closed one. Closing an idle ask and deleting a
		// closed one are decisions, so they are taken under the same
		// singleton duty as every other sweep rather than by a clock.
		jobs = append(jobs, maintenance.ChannelJobs(a2a.NewCoordStore(fleet))...)
		// The NATIVE backends' own records, on the same edge and for a
		// related reason: their family holds several classes under one
		// grammar, and only some of them age out — so no bucket age can
		// express the retention and it is taken as a decision here.
		//
		// Contributed only where THIS node runs the backend, which is
		// what makes it correct to sweep a fleet-wide record from a
		// per-node job list: the duty is a singleton, so exactly one
		// node's list runs per tick, and a node with no backend
		// contributes nothing rather than an empty sweep.
		if e.native != nil {
			if e.native.writer != nil {
				// THE TRACKER'S OWN JOBS, and they are a different
				// kind of thing from a sweep: its records are a log
				// and nothing deletes them here. They finish work a
				// crash left half-done — a re-spread walk, an
				// abandoned merge, a one-sided dependency — and tell
				// the tasks a close unblocked. Every one is GATED,
				// so a tick with nothing to do costs one indexed
				// read.
				jobs = append(jobs, tracker.Jobs(tracker.DutyDeps{
					DB: e.backends.Store, Writer: e.native.writer,
					NodeID: e.native.nodeID,
					// AND THE LEAD MAP, for the one repair whose
					// commit carries a wake. Read per call against
					// the epoch current when the job runs, for the
					// reason every other live seam here is: the duty
					// outlives a revision, and a captured map would
					// route by an org chart that has since moved.
					Leads: liveLeads{engine: e},
				})...)
				// AND THE INBOX'S OWN SWEEP, which is a range
				// delete rather than a repair and is therefore
				// PER NODE — `tracker_notifications` is
				// Divergent, so each node holds its own rows and
				// a singleton would tidy one and let the rest
				// grow for ever. Contributed here rather than in
				// tracker.Jobs because that list runs under the
				// duty and this one must not.
				jobs = append(jobs, tracker.InboxJobs(
					e.backends.Store, e.inboxRetention())...)
			}
			// AND THE STATE LOG'S OWN OPERATION LEDGERS, one per
			// registered domain. Every `<domain>_ops` migration says
			// the table is swept and ships the index a range delete
			// needs, and nothing swept them: a row per applied record,
			// kept for ever, on every node. PER NODE rather than under
			// the singleton, because each node owns its own copy —
			// see [maintenance.StatelogJobs].
			//
			// NO HORIZON PASSED: each ledger states its domain's own,
			// because the table's size is that domain's commit rate
			// times its retention and one number sized them all alike.
			if e.native.log != nil {
				jobs = append(jobs, maintenance.StatelogJobs(e.native.log.opsLedgers())...)
			}
			// THE KNOWLEDGE BASE HAS NO SWEEP ANY MORE, and its
			// absence is a consequence rather than an omission. Its
			// three passes were a change retention, a revision prune
			// and an orphan collector; the prune now RIDES EACH
			// COMMIT as the record's own list of retired versions, the
			// orphans cannot occur because a create is one
			// transaction, and the history is a Replicated table an
			// applier owns — so deleting a row here on one node's own
			// authority is exactly what the identity claim forbids.
		}
	}
	if e.mailboxes != nil {
		// The second shared record swept here, and for the channels'
		// reason: a mailbox record has no age that could tell a seat in
		// the company from one that left, so retiring a removed seat's
		// mailbox is a decision taken against the active revision.
		jobs = append(jobs, e.mailboxes.Jobs()...)
	}

	w, err := maintenance.New(maintenance.Options{
		Jobs: jobs,
		ClaimDuty: maintenance.DutyFunc(
			e.workerDuty(maintenanceDutyName, maintenanceDutyTTL)),
	})
	if err != nil {
		// A JOB THIS WIRING GOT WRONG IS A WIRING BUG, not a runtime
		// condition, and it is the same for every node of the fleet — so
		// it cannot be recovered from here and must not be swallowed.
		// Every table these jobs cover grows for the life of the
		// deployment if its sweep never runs, and that has no other
		// symptom until a volume fills. Logged at ERROR with the whole
		// list rather than returned, because startMaintenance is the last
		// thing a boot does and refusing to serve a company over a
		// housekeeping misconfiguration is the worse of the two failures.
		log.ErrorContext(ctx, "maintenance_worker_not_started", "error", err.Error())
		return
	}
	e.maintenance = w
	// Detached, for the same reason the node's loops are: a sweep loop
	// bound to a signal context stops at SIGTERM, which is harmless here
	// but would make the worker's lifetime differ from every other loop's
	// for no reason a reader could find.
	e.maintenance.Start(context.WithoutCancel(ctx))
}

// ConversationRetention reads the operator's horizon for the conversation
// ledger, in days, off the ACTIVE epoch.
//
// Exported alongside [Engine.Maintenance] for the same reason: what a
// company actually forgets, and when, is an operator question.
//
// Read once at start, so a live config change lands at the next process
// start like every other sweep parameter — the alternative is a worker whose
// horizons move under it mid-tick, for a table whose horizon is measured in
// weeks.
//
// The field has existed since the ledger shipped and nothing ever read it:
// an operator setting `retention_days: 7` got thirty days of conversations,
// silently, because there was no sweep to honour it.
func (e *Engine) ConversationRetention() time.Duration {
	// AN UNCONFIGURED NODE HAS NO HORIZON TO STATE, and says so with a
	// zero rather than inventing one: [maintenance.LedgerJobs] already
	// reads zero as "take the floor", which is the same answer and only
	// one place to change it. What it must never do is return a literal
	// zero DURATION to a caller that would read it as "retain nothing" —
	// which is exactly why that floor is there.
	c := e.Company()
	if c == nil {
		return 0
	}
	// No zero check beyond that: validation refuses retention_days below 1
	// and fills the shipped default when it is unset, so a config that
	// reached an engine always carries a positive number.
	days := c.Config.TurnEngine.ConversationSession.RetentionDays
	return time.Duration(days) * 24 * time.Hour
}

// buildMailboxes builds the seat mailbox registry and its sweep.
//
// Nil, with no error, on backends without a fleet store, a queue or a lease
// store: there is no shared record to keep and nothing to retire through, and
// a node built that way registers nothing rather than failing every seat.
//
// A retirement claims the seat's lease under an incarnation of its OWN, never
// the node's: a claim by an owner that already holds a lease doubles as a
// renew, so sharing the seat host's owner would let a retirement take the
// lease of a seat this node is running. The TTL is the one the lease store was
// opened with, which [Engine.leaseTTL] must already hold.
func (e *Engine) buildMailboxes(b *Backends, nodeID string) (*maintenance.Mailboxes, error) {
	if b == nil || b.Fleet == nil || b.Queue == nil || b.Coord == nil {
		return nil, nil
	}
	return maintenance.NewMailboxes(maintenance.MailboxOptions{
		Records: b.Fleet, Queue: b.Queue, Leases: b.Coord,
		Owner:    config.NewIncarnation(nodeID),
		LeaseTTL: e.leaseTTL,
		Roster:   e.activeSeatHandles,
		Runs:     e.retireSeatRuns,
	})
}

// retireSeatRuns ends a retired seat's detached coding runs, for the mailbox
// retirement that holds the seat's lease under owner and epoch.
//
// Through the sandbox coordinator, which reclaims each run's box, announces the
// loss and finishes its record. A node without one (its company configured no
// sandbox when it started) cannot reach a box, so it ends nothing, and it must
// not let the retirement delete the subscriptions of a seat whose runs are still
// recorded: it reads the fleet's run records itself, and refuses while the seat
// has any. The duty's next holder, or this node once it runs a coordinator,
// retires the seat instead.
func (e *Engine) retireSeatRuns(ctx context.Context, handle, owner string, epoch int64) error {
	if c := e.sandboxCoordinator; c != nil {
		return c.RetireSeat(ctx, handle, owner, epoch)
	}
	if e.backends == nil || e.backends.Fleet == nil {
		return fmt.Errorf("engine: this node has no fleet store, so it cannot tell whether "+
			"retired seat %q left coding runs behind", handle)
	}
	runs, err := sandbox.NewCoordStore(e.backends.Fleet).ListActiveForSeat(ctx, handle)
	if err != nil {
		return fmt.Errorf("engine: reading the coding runs of retired seat %q: %w", handle, err)
	}
	if len(runs) > 0 {
		return fmt.Errorf("engine: retired seat %q still has %d coding runs and this node runs no "+
			"sandbox coordinator to end them (providers.sandbox was not configured when it started); "+
			"its mailbox is kept until a node that runs one holds the maintenance duty, or this "+
			"node is restarted on a company that configures providers.sandbox", handle, len(runs))
	}
	return nil
}

// mailboxRegistry is the node's view of [Engine.mailboxes].
//
// A function rather than a plain assignment because the field is a pointer and
// the node's is an interface: a nil *Mailboxes stored in the interface is a
// non-nil registry whose every call panics, and the node checks for nil.
func (e *Engine) mailboxRegistry() node.MailboxRegistry {
	if e.mailboxes == nil {
		return nil
	}
	return e.mailboxes
}

// activeSeatHandles is the mailbox sweep's roster: the agent seats of the
// revision the fleet is pointed at, or an error saying why that is unknown.
//
// # Only when this node serves that revision
//
// The sweep judges a seat absent from the roster, and a retirement deletes mail
// that cannot be recovered, so the roster must be the FLEET's current revision
// rather than whatever this node happens to be running. A node that has not yet
// applied the pointer's epoch (propagation, a refused apply, a node stuck on an
// older revision) answers unknown, and the tick stamps and retires nothing. The
// next holder of the duty, or this node once it catches up, judges instead.
//
// The three reads are ordered so the roster can only be NEWER than the epoch it
// is checked against, never older. The pointer is read first; the applied epoch
// next; the company last. An apply installs its company before the reconciler
// records its epoch, so a company read after a matching epoch is that
// revision's or a later one, and a later revision is only ever a truer roster.
func (e *Engine) activeSeatHandles(ctx context.Context) ([]string, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil, fmt.Errorf("engine: this node has no fleet store to read the activation pointer from")
	}
	target, found, err := e.backends.Fleet.Target(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: read the activation pointer: %w", err)
	}
	if !found {
		return nil, maintenance.ErrNoActiveRevision
	}
	r := e.reconciler.Load()
	if r == nil {
		return nil, fmt.Errorf("engine: this node runs no reconciler, so it cannot tell whether it "+
			"serves activation epoch %d", target.Epoch)
	}
	applied := r.Applied()
	company := e.Company()
	if applied != target.Epoch || company == nil {
		return nil, fmt.Errorf("engine: this node serves activation epoch %d and the fleet is on %d; "+
			"seats are judged once this node has applied it", applied, target.Epoch)
	}
	seats := company.Seats()
	handles := make([]string, 0, len(seats))
	for _, seat := range seats {
		handles = append(handles, seat.Handle)
	}
	return handles, nil
}

// Maintenance exposes the retention sweep.
//
// Exported because "is housekeeping running, and over what?" is a question
// an operator has to be able to ask: the failure this whole package fixes
// was invisible precisely because nothing anywhere could answer it.
func (e *Engine) Maintenance() *maintenance.Worker { return e.maintenance }

// stopMaintenance ends the sweep, waiting for an in-flight tick.
func (e *Engine) stopMaintenance() {
	if e.maintenance != nil {
		e.maintenance.Stop()
	}
}

// inboxRetention is how long this company's inbox rows live.
//
// A NODE WITH NO COMPANY STATES NO HORIZON, for the reason
// [Engine.ConversationRetention] gives: a literal zero duration read as
// "retain nothing" would delete the inbox, so the shipped default is what an
// unconfigured node sweeps on. Validation refuses a configured value outside
// its own bounds and fills the default when it is unset.
func (e *Engine) inboxRetention() time.Duration {
	c := e.Company()
	if c == nil {
		return config.DefaultInboxRetentionDays * 24 * time.Hour
	}
	return c.Config.Tracker.Native.InboxRetention()
}
