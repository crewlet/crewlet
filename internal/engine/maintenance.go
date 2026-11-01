package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/schedule/sqlledger"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/worklet"
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
	w, err := maintenance.New(maintenance.Options{
		Jobs: e.maintenanceJobs(),
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
	e.maintenance.Store(w)
	// Detached, for the same reason the node's loops are: a sweep loop
	// bound to a signal context stops at SIGTERM, which is harmless here
	// but would make the worker's lifetime differ from every other loop's
	// for no reason a reader could find.
	w.Start(context.WithoutCancel(ctx))
}

// maintenanceJobs is every job the sweep runs on this node, as it stands now.
func (e *Engine) maintenanceJobs() []maintenance.Job {
	var jobs []maintenance.Job
	if db := e.backends.Store; db != nil {
		// Every one of these tables lives in this one store, which is
		// what makes a single list of them honest rather than a
		// coincidence: a node's short-horizon state IS its local index.
		jobs = append(jobs, maintenance.StoreJobs(db)...)
		// THE CONTROL PLANE'S OWN TABLE, named separately rather than
		// folded into the store sweep: company_config is the one table
		// here whose rows are configuration, and an operator reading
		// the job list wants to see that it is swept at all — it was
		// not, for the life of every deployment before this.
		jobs = append(jobs, maintenance.ConfigJobs(db)...)
		jobs = append(jobs, maintenance.LearningJobs(learning.NewDiary(db))...)
		jobs = append(jobs, maintenance.CounterpartyJobs(learning.NewCounterparties(db))...)
		jobs = append(jobs, maintenance.ScheduleJobs(sqlledger.New(db.SQL()))...)
		// THE COMPANY'S HORIZON, READ AT EVERY SWEEP: an apply moves
		// it under a running node, and a horizon read here, once, held
		// the sweep to the revision the node booted on. See
		// [maintenance.Horizon].
		jobs = append(jobs, maintenance.LedgerJobs(
			ledgerstore.NewConversations(db), e.ConversationRetention)...)
	}
	if fleet := e.backends.Fleet; fleet != nil {
		// The one shared surface swept here, and the exception the
		// coordination store's own retention rule makes room for: its
		// buckets expire on age, and a bucket's age cannot tell an OPEN
		// channel from a closed one. Closing an idle ask and deleting a
		// closed one are decisions, so they are taken under the same
		// singleton duty as every other sweep rather than by a clock.
		//
		// [a2a.NewSweeper] rather than the store, because closing an
		// idle channel publishes a2a_channel_closed and what that means
		// on the wire belongs to internal/a2a — and rather than the
		// whole [a2a.Service], which needs a directory this caller
		// deliberately has none of. A directory is built from the
		// RUNNING COMPANY, so demanding one here would tie the retention
		// of a fleet-wide record to whether this node has applied a
		// configuration. Asking for the two verbs the sweep actually
		// uses is also what stops the service's own requirements
		// silently disarming it: built as a Service with an empty
		// Options, this construction began failing the day the directory
		// became mandatory, and both jobs vanished behind a warning.
		//
		// AND WITH THE ENGINE'S ANSWER TO WHETHER A QUIET CHANNEL IS STILL
		// OWED, so the ask an executor is working on — or one waiting in a
		// seat's held inbox — is not closed under it: see [colleagueOwed].
		if queue := e.backends.Queue; queue != nil {
			sweeper, err := a2a.NewSweeper(a2a.NewCoordStore(fleet), queue,
				colleagueOwed{engine: e, runs: sandbox.NewCoordStore(fleet)}, nil)
			if err != nil {
				// Logged rather than fatal, matching a2aService: a
				// company whose channels are not swept is degraded,
				// not down.
				log.Warn("a2a_sweep_unavailable", "error", err,
					"hint", "idle agent-to-agent channels will stay open")
			} else {
				jobs = append(jobs, maintenance.ChannelJobs(sweeper)...)
			}
		}
		// AND THE COORDINATION STORE'S OWN REMOVAL MARKERS, for the
		// channels' reason one level down: the buckets that keep them
		// are the ones no clock may age, so a marker there stays for the
		// life of the deployment unless a decision removes it — and
		// every listing that meets one pays a leader read for it. See
		// [maintenance.MarkerJobs].
		jobs = append(jobs, maintenance.MarkerJobs(fleet)...)
		// AND EACH HUMAN SEAT'S CHAT READ STATE, for the mailboxes'
		// reason: its bucket has no age, so the record of a seat the
		// company no longer has is forgotten only by a decision taken
		// against the revision the fleet is pointed at — a grace after a
		// sweep first saw the seat gone. Registered whatever chat backend
		// the company runs, because the records outlive a move off Chatlet
		// and the job costs one listing of an empty bucket where
		// there are none. See [maintenance.ChatReadJobs].
		jobs = append(jobs, maintenance.ChatReadJobs(fleet, e.humanSeatHandles)...)
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
		core := e.core.Load()
		if n := e.native.Load(); n != nil && n.writer != nil && core != nil {
			// THE REPLICATED ESTATE WITH ITS WRITE SIDE, which is what
			// the tracker's two exceptions to the applier-only rule need
			// — the duty's probe clear and the inbox sweep, each argued
			// where internal/store's applier gate allows it — and nothing
			// else on this list takes. A node holding no estate runs no
			// core and reaches the tracker through the router, so it
			// contributes neither.
			trackerEstate := e.backends.Store.Replicated()
			// THE TRACKER'S OWN JOBS, and they are a different kind of
			// thing from a sweep: its records are a log and nothing
			// deletes them here. They finish work a crash left half-done
			// — a re-spread walk, an abandoned merge or cross-project
			// move, a one-sided dependency — and tell the tasks a close
			// unblocked. Every one is GATED, so a tick with nothing to do
			// costs one indexed read.
			jobs = append(jobs, worklet.Jobs(worklet.DutyDeps{
				DB: trackerEstate, Writer: n.writer,
				NodeID: core.nodeID,
				// AND THE LEAD MAP, for the one repair whose commit
				// carries a wake. Read per call against the epoch
				// current when the job runs, for the reason every other
				// live seam here is: the duty outlives a revision, and a
				// captured map would route by an org chart that has
				// since moved.
				Leads: liveLeads{engine: e},
			})...)
			// AND THE INBOX'S OWN SWEEP, which is a range delete rather
			// than a repair and is therefore PER NODE —
			// `tracker_notifications` is Divergent, so each data node
			// holds its own rows and a singleton would tidy one and let
			// the rest grow for ever. Contributed here rather than in
			// worklet.Jobs because that list runs under the duty and
			// this one must not. Its horizon is the one the PUBLISHED
			// company states, asked at every sweep — the one the
			// applier reads per batch (see [worklet.InboxJobs]).
			jobs = append(jobs, worklet.InboxJobs(trackerEstate, e.inboxRetention)...)
		}
		if core != nil {
			// AND THE STATE LOG'S OWN OPERATION LEDGERS, one per
			// registered domain that keeps one — the CORE's, so they are
			// swept from boot on every data node, a node with no company
			// included: its identity log is written from the first
			// invitation. Every `<domain>_ops` migration says the table
			// is swept and ships the index a range delete needs, and
			// nothing swept them: a row per applied record, kept for
			// ever, on every node. PER NODE rather than under the
			// singleton, because each node owns its own copy — see
			// [maintenance.StatelogJobs].
			jobs = append(jobs, maintenance.StatelogJobs(core.log.opsLedgers())...)
			// THE KNOWLEDGE BASE HAS NO SWEEP ANY MORE, and its absence
			// is a consequence rather than an omission. Its three passes
			// were a change retention, a revision prune and an orphan
			// collector; the prune now RIDES EACH COMMIT as the record's
			// own list of retired versions, the orphans cannot occur
			// because a create is one transaction, and the history is a
			// Replicated table an applier owns — so deleting a row here
			// on one node's own authority is exactly what the identity
			// claim forbids.
		}
	}
	if e.mailboxes != nil {
		// The second shared record swept here, and for the channels'
		// reason: a mailbox record has no age that could tell a seat in
		// the company from one that left, so retiring a removed seat's
		// mailbox is a decision taken against the active revision.
		jobs = append(jobs, e.mailboxes.Jobs()...)
	}
	return jobs
}

// rebuildMaintenance replaces the sweep with one built against what the node
// runs now.
//
// FOR A NODE'S FIRST NATIVE RUNTIME, and nothing else: [Engine.startMaintenance]
// reads its job list once, and on a node that booted without a company "once"
// was before there were native halves to contribute the tracker's repairs and
// the inbox sweep. The operation ledgers are the core's, and were in the list
// from boot. A company's HORIZONS are no reason to rebuild — every job asks its
// horizon at every sweep ([maintenance.Horizon]), so a revision that moves one
// is honoured by the next sweep of the worker already running. The old worker
// stops — its in-flight tick waited out — before the new one is built, so two
// sweeps never run at once.
//
// A node that does not publish never started a sweep, and does not start one
// here either.
func (e *Engine) rebuildMaintenance(ctx context.Context) {
	if !e.mode.Publishes() {
		return
	}
	e.stopMaintenance()
	e.startMaintenance(ctx)
	if w := e.maintenance.Load(); w != nil {
		log.InfoContext(ctx, "maintenance_rebuilt", "jobs", w.Jobs(),
			"detail", "the sweep was rebuilt for this node's first company")
	}
}

// ConversationRetention reads the operator's horizon for the conversation
// ledger, in days, off the ACTIVE epoch.
//
// Exported alongside [Engine.Maintenance] for the same reason: what a
// company actually forgets, and when, is an operator question.
//
// Read by the sweep at EVERY SWEEP, so a revision that moves it is honoured on
// the next one. It was read once, when the sweep was built, and that held the
// conversation ledger to the revision the node booted on: a company that
// shortened its retention kept every conversation it had asked to forget for
// as long as the node stayed up. A horizon cannot move under a sweep in
// flight — each job asks it once, as it starts.
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
// lease of a seat this node is running. That makes it the ONE lease owner in
// this process that is not [Engine.incarnation], and deliberately: every other
// lease is this process speaking for itself, while a retirement is a separate
// party that has to lose to this node's own host. The TTL is the one the lease
// store was opened with, which [Engine.leaseTTL] must already hold.
func (e *Engine) buildMailboxes(b *Backends, nodeID string) (*maintenance.Mailboxes, error) {
	if b == nil || b.Fleet == nil || b.Queue == nil || b.Coord == nil {
		return nil, nil
	}
	return maintenance.NewMailboxes(maintenance.MailboxOptions{
		Records: b.Fleet, Queue: b.Queue, Leases: b.Coord,
		Owner:    config.NewIncarnation(nodeID),
		LeaseTTL: e.leaseTTL,
		Roster:   e.activeSeats,
		Runs:     e.retireSeatRuns,
	})
}

// retireSeatRuns ends a retired seat's detached executors, for the mailbox
// retirement that holds the seat's lease under owner and epoch.
//
// BY THE SEAT'S ID, which is what the retiring mailbox is filed and named
// under; handle is the record's label, empty for a mailbox the broker's listing
// alone found, and selects nothing. See
// [sandbox.PendingStore.ListActiveForSeatID]. The errors here name the seat by
// its id alone, because the retirement that called this wraps them in the label
// it holds.
//
// Through the sandbox coordinator, which reclaims each run's box, announces the
// loss and finishes its record. A node without one (no company it has served
// reached a sandbox cell) cannot reach a box, so it ends nothing, and it must
// not let the retirement delete the subscriptions of a seat whose runs are still
// recorded: it reads the fleet's run records itself, and refuses while the seat
// has any. The duty's next holder, or this node once it runs a coordinator,
// retires the seat instead.
func (e *Engine) retireSeatRuns(ctx context.Context, seat uuid.UUID, handle, owner string, epoch int64) error {
	if rt := e.sandbox.Load(); rt != nil {
		return rt.coordinator.RetireSeat(ctx, seat, handle, owner, epoch)
	}
	if e.backends == nil || e.backends.Fleet == nil {
		return fmt.Errorf("engine: this node has no fleet store, so it cannot tell whether "+
			"retired seat %s left its executor in a box", seat)
	}
	runs, err := sandbox.NewCoordStore(e.backends.Fleet).ListActiveForSeatID(ctx, seat)
	if err != nil {
		return fmt.Errorf("engine: reading the executor records of retired seat %s: %w", seat, err)
	}
	if len(runs) > 0 {
		return fmt.Errorf("engine: retired seat %s still has %d executor records and this node runs no "+
			"sandbox coordinator to end them (no company it has served configures providers.sandbox); "+
			"its mailbox is kept until a node that runs one holds the maintenance duty, or this "+
			"node applies a company that configures providers.sandbox", seat, len(runs))
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

// activeSeats is the mailbox sweep's roster: the agent seats of the revision
// the fleet is pointed at, or an error saying why that is unknown.
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
// The reads are ordered so the company judged is EXACTLY the pointer's: the
// pointer is read first, and the company this node publishes is accepted only
// when it was applied from that very activation ([Engine.fleetCompany]).
func (e *Engine) activeSeats(ctx context.Context) ([]placement.Seat, error) {
	company, err := e.fleetCompany(ctx, e.Company())
	if err != nil {
		return nil, err
	}
	return company.Seats(), nil
}

// humanSeatHandles is the chat read-state reconcile's roster: every HUMAN seat
// of the revision the fleet is pointed at, or an error saying why that is
// unknown ([Engine.fleetCompany]).
//
// HUMAN SEATS, because a read state is kept under the seat a person acts as
// and only a human seat is ever acted as by a person or a bound machine: a
// record under any other handle is one nobody can reach, which this roster
// lets the reconcile forget a grace after it is first seen rather than keep
// for as long as an agent seat of that name exists. And the SEAT rather than
// its holder, because the record is the seat's: a person removed or moved
// from a seat the company still has leaves it to the seat's next holder, as
// the seat's inbox stays.
func (e *Engine) humanSeatHandles(ctx context.Context) ([]string, error) {
	company, err := e.fleetCompany(ctx, e.Company())
	if err != nil {
		return nil, err
	}
	var handles []string
	for role := range company.Org.AllRoles() {
		if role.EffectiveKind() == org.KindHuman {
			handles = append(handles, role.Handle())
		}
	}
	slices.Sort(handles)
	return handles, nil
}

// fleetCompany is c when it is the company of the activation the fleet is
// pointed at, or an error saying why that is unknown.
//
// THE RULE EVERY FLEET DECISION FOLLOWS THAT DESTROYS, BY THE COMPANY
// DOCUMENT, SOMETHING NO UNDO RESTORES:
//
//   - the mailbox sweep, which judges a seat absent and retires its mail;
//   - the clearing of a removed seat's pause ([Engine.clearRemovedSeatPauses]),
//     a pause a person placed;
//   - the forgetting of a removed human seat's chat read state
//     ([Engine.humanSeatHandles]), every room's cursor the seat held;
//   - the chat prune ([Engine.chatRetentionPolicy]), which deletes on every
//     data node each message older than the horizon the document states;
//   - the assistant's retention purge ([Engine.assistantRetention]), which
//     purges every conversation left unused for the term the document states.
//
// A node applying a revision the fleet has since replaced, or running one the
// fleet never activated, has neither a seat the newer revision added nor the
// horizon it states, and would destroy by what it has instead. So each of them
// acts only on the company this answers and on nothing while it is unknown:
// what it would have destroyed waits for a node that runs the fleet's
// revision, which is late rather than wrong.
//
// COMPARED ON THE ACTIVATION'S INSTANT, which the pointer publishes strictly
// later on every activation ([coord.ActivationAt]) and which a company carries
// from the apply that built it ([Company.ActivatedAt]) — so a re-activation of
// an unchanged revision, the rotation gesture, is a different activation here
// too, and a company applied from the pointer's own activation is accepted
// even while the reconciler is still inside the apply that publishes it. A
// company no activation has named, whether one handed to [New] in code or an
// offline command's offer the fleet has not taken, is unknown until this node
// applies the pointer's revision.
func (e *Engine) fleetCompany(ctx context.Context, c *Company) (*Company, error) {
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
	switch {
	case c == nil:
		return nil, errors.New("engine: this node publishes no company yet; nothing " +
			"the company document would destroy is decided here until it does")
	case c.ActivatedAt.IsZero():
		// AN OFFER, most likely: what this node runs no activation has
		// named, so there is no instant of its own to report.
		return nil, fmt.Errorf("engine: this node serves a company no activation has "+
			"named (an offline command's choice the fleet has not taken, or one "+
			"handed to the engine in code) and the fleet is on activation epoch %d "+
			"of %s; nothing the company document would destroy is decided here "+
			"until this node has applied it", target.Epoch,
			target.At.Format(time.RFC3339Nano))
	case store.EncodeTime(c.ActivatedAt) != store.EncodeTime(target.At):
		return nil, fmt.Errorf("engine: this node serves the activation of %s and the "+
			"fleet is on activation epoch %d of %s; nothing the company document would "+
			"destroy is decided here until this node has applied it",
			c.ActivatedAt.Format(time.RFC3339Nano), target.Epoch,
			target.At.Format(time.RFC3339Nano))
	}
	return c, nil
}

// Maintenance exposes the retention sweep.
//
// Exported because "is housekeeping running, and over what?" is a question
// an operator has to be able to ask: the failure this whole package fixes
// was invisible precisely because nothing anywhere could answer it.
func (e *Engine) Maintenance() *maintenance.Worker { return e.maintenance.Load() }

// stopMaintenance ends the sweep, waiting for an in-flight tick.
func (e *Engine) stopMaintenance() {
	if w := e.maintenance.Swap(nil); w != nil {
		w.Stop()
	}
}

// inboxRetention is how long this company's inbox rows live, read off the
// PUBLISHED company each time it is asked — the sweep asks it at every sweep,
// for the reason [Engine.ConversationRetention] is.
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
	return c.Config.Tracker.Worklet.InboxRetention()
}
