package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/schedule/sqlledger"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/worklet"
)

// maintenanceDutyName is the fleet singleton the retention sweep claims.
const maintenanceDutyName = "maintenance"

// maintenanceDutyTTL is how long the duty survives without a re-claim: three
// ticks, the sandbox waiter's ratio. One missed tick must not hand a burst of
// range deletes to a peer while this node may still run it, and a dead node's
// duty is still picked up within the hour.
const maintenanceDutyTTL = 3 * maintenance.Interval

// startMaintenance arms the retention sweep: every short-horizon table in this
// process's stores, in one worker, so a store with a Purge and no entry in
// [Engine.maintenanceJobs] is visible in one place rather than hidden in a
// loop each subsystem arms, or forgets to, for itself.
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
		// A WIRING BUG, the same on every node, and unswept tables grow with
		// no symptom until a volume fills: so logged at ERROR, never
		// swallowed. Not returned, because this is the last thing a boot does
		// and refusing to serve a company over housekeeping is worse.
		log.ErrorContext(ctx, "maintenance_worker_not_started", "error", err.Error())
		return
	}
	e.maintenance.Store(w)
	// Detached like the node's other loops, so its lifetime matches theirs
	// rather than ending at SIGTERM.
	w.Start(context.WithoutCancel(ctx))
}

// maintenanceJobs is every job the sweep runs on this node, as it stands now.
func (e *Engine) maintenanceJobs() []maintenance.Job {
	var jobs []maintenance.Job
	if db := e.backends.Store; db != nil {
		jobs = append(jobs, maintenance.StoreJobs(db)...)
		// THE CONTROL PLANE'S OWN TABLE, named apart from the store sweep
		// so an operator reading the job list sees that company_config,
		// the one table of configuration rows, is swept.
		jobs = append(jobs, maintenance.ConfigJobs(db)...)
		jobs = append(jobs, maintenance.LearningJobs(learning.NewDiary(db))...)
		jobs = append(jobs, maintenance.CounterpartyJobs(learning.NewCounterparties(db))...)
		jobs = append(jobs, maintenance.ScheduleJobs(sqlledger.New(db.SQL()))...)
	}
	// THE FLEET'S MCP LISTINGS, which no age may forget: what a node that
	// died or restarted left behind, and the schemas nothing names any more.
	jobs = append(jobs, e.mcpListingJobs()...)
	if fleet := e.backends.Fleet; fleet != nil {
		// The coordination store expires buckets on age, which cannot tell
		// an OPEN channel from a closed one, so closing an idle ask and
		// deleting a closed one are decisions, taken under the singleton
		// duty. [a2a.NewSweeper], not the store, because what
		// a2a_channel_closed means on the wire is internal/a2a's; and not an
		// [a2a.Service], whose directory is built from the RUNNING COMPANY
		// and would tie a fleet-wide record's retention to this node having
		// applied one, or disarm the sweep when a Service requirement grows.
		// [colleagueOwed] keeps an ask an executor is working on, or one
		// waiting in a seat's held inbox, from being closed under it.
		if queue := e.backends.Queue; queue != nil {
			sweeper, err := a2a.NewSweeper(a2a.NewCoordStore(fleet), queue,
				colleagueOwed{engine: e, runs: sandbox.NewCoordStore(fleet)}, nil)
			if err != nil {
				// Logged rather than fatal, as a2aService is: unswept
				// channels degrade a company, not down it.
				log.Warn("a2a_sweep_unavailable", "error", err,
					"hint", "idle agent-to-agent channels will stay open")
			} else {
				jobs = append(jobs, maintenance.ChannelJobs(sweeper)...)
			}
		}
		// AND THE COORDINATION STORE'S OWN REMOVAL MARKERS, for the
		// channels' reason: their buckets have no age, so a marker stays
		// unless a decision removes it, and every listing that meets one
		// pays a leader read. See [maintenance.MarkerJobs].
		jobs = append(jobs, maintenance.MarkerJobs(fleet)...)
		// AND THE ACTIVATION'S BODIES no pointer names, at their own
		// horizon. See [maintenance.BodyJobs].
		jobs = append(jobs, maintenance.BodyJobs(fleet)...)
		// AND EACH HUMAN SEAT'S CHAT READ STATE, for the mailboxes' reason:
		// its bucket has no age, so a removed seat's record is forgotten
		// only by a decision against the fleet's revision. Registered
		// whatever the chat backend, because the records outlive a move off
		// Chatlet. See [maintenance.ChatReadJobs].
		jobs = append(jobs, maintenance.ChatReadJobs(fleet, e.humanSeatHandles)...)
		// AND EACH HUMAN SEAT'S HOME BOARD, for the same reason, judged by
		// the same reconcile. See [maintenance.HomeBoardJobs].
		jobs = append(jobs, maintenance.HomeBoardJobs(fleet, e.humanSeatHandles)...)
		// AND THEIR ASK CREWLET INSTRUCTIONS, likewise. See
		// [maintenance.InstructionsJobs].
		jobs = append(jobs, maintenance.InstructionsJobs(fleet, e.humanSeatHandles)...)
		// The NATIVE backends' records: only some of their classes age out,
		// so no bucket age expresses the retention. Contributed only where
		// THIS node runs the backend; the duty is a singleton, so one node's
		// list runs per tick, and a node without the backend adds nothing.
		core := e.core.Load()
		if n := e.native.Load(); n != nil && n.writer != nil && core != nil {
			// THE REPLICATED ESTATE WITH ITS WRITE SIDE, for the tracker's
			// two exceptions to the applier-only rule (the duty's probe
			// clear and the inbox sweep, argued at internal/store's applier
			// gate). A node holding no estate runs no core and adds neither.
			trackerEstate := e.backends.Store.Replicated()
			// THE TRACKER'S OWN JOBS are repairs, not sweeps: they delete
			// nothing, but finish work a crash left half-done (a re-spread
			// walk, an abandoned merge or cross-project move, a one-sided
			// dependency) and tell the tasks a close unblocked. Each is
			// GATED, so an idle tick costs one indexed read.
			jobs = append(jobs, worklet.Jobs(worklet.DutyDeps{
				DB: trackerEstate, Writer: n.writer,
				NodeID: core.nodeID,
				// AND THE LEAD MAP, for the one repair whose commit
				// carries a wake: read per call, because the duty
				// outlives a revision and a captured map would route by
				// a stale org chart.
				Leads: liveLeads{engine: e},
			})...)
			// AND THE POLL OF THE CHANGES TASKS WAIT ON, for what no
			// webhook delivered (ADR-0121).
			jobs = append(jobs, maintenance.Job{Name: "tracker_linked_changes", Scope: maintenance.Fleet,
				Gate: e.openChangesPending, Run: e.pollChanges})
			// AND THE INBOX'S OWN SWEEP, a range delete outside
			// worklet.Jobs' duty and so PER NODE: `tracker_notifications`
			// is Divergent, and a singleton would tidy one node's rows and
			// let the rest grow. Its horizon is the published company's,
			// asked at every sweep (see [worklet.InboxJobs]).
			jobs = append(jobs, worklet.InboxJobs(trackerEstate, e.inboxRetention)...)
		}
		if core != nil {
			// AND THE STATE LOG'S OPERATION LEDGERS, one per registered
			// domain that keeps one: the CORE's, so swept from boot on
			// every data node, one with no company included (its identity
			// log is written from the first invitation). PER NODE, not
			// under the singleton, because each node owns its copy; see
			// [maintenance.StatelogJobs].
			jobs = append(jobs, maintenance.StatelogJobs(core.log.opsLedgers())...)
			// AND THE CODE LOG'S NOTICES, the core's too: every data node
			// applies the code log, and its applier writes a notice per
			// recipient into a DIVERGENT table, so each node sweeps its
			// own: a person's at the company's inbox horizon, a seat's at
			// the turn-detail horizon (gitlet.NoticeJobs).
			jobs = append(jobs, gitlet.NoticeJobs(e.backends.Store.Replicated(), e.inboxRetention)...)
			// AND THE ARTIFACTS LOG'S, for the same reason: its applier writes
			// a person's notice per comment record into a DIVERGENT table,
			// swept per node at the company's inbox horizon.
			jobs = append(jobs, artifactsdomain.NoticeJobs(e.backends.Store.Replicated(), e.inboxRetention)...)
			// THE KNOWLEDGE BASE HAS NO SWEEP, by design: each commit
			// carries its retired versions, a create is one transaction so
			// no orphan occurs, and its history is a Replicated table an
			// applier owns, which no node may delete from on its own
			// authority.
		}
	}
	if e.mailboxes != nil {
		// For the channels' reason: a mailbox has no age that tells a seat
		// in the company from one that left, so retiring a removed seat's
		// mailbox is a decision against the active revision.
		jobs = append(jobs, e.mailboxes.Jobs()...)
	}
	return jobs
}

// rebuildMaintenance replaces the sweep with one built against what the node
// runs now.
//
// FOR A NODE'S FIRST NATIVE RUNTIME only: [Engine.startMaintenance] reads its
// job list once, which on a node booted without a company came before the
// native halves (the tracker's repairs and the inbox sweep); the ledgers are the
// core's, listed from boot. A company's
// horizons need no rebuild, since every job asks its horizon at every sweep
// ([maintenance.Horizon]). The old worker stops, its in-flight tick waited
// out, before the new one is built, so two sweeps never run at once. A node
// that does not publish never started a sweep and starts none here.
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

// buildMailboxes builds the seat mailbox registry and its sweep: nil, with no
// error, on backends without a fleet store, a queue or a lease store, which
// have nothing to keep or retire through, so no seat fails over it.
//
// A retirement claims the seat's lease under an incarnation of its OWN, the
// one lease owner in this process that is not [Engine.incarnation]: a claim by
// an owner already holding a lease doubles as a renew, so the seat host's owner
// would let a retirement take the lease of a seat this node runs, and a
// retirement must lose to this node's own host. The TTL is the lease store's,
// which [Engine.leaseTTL] must already hold.
func (e *Engine) buildMailboxes(b *Backends, nodeID string) (*maintenance.Mailboxes, error) {
	if b == nil || b.Fleet == nil || b.Queue == nil || b.Coord == nil {
		return nil, nil
	}
	return maintenance.NewMailboxes(maintenance.MailboxOptions{
		Records: b.Fleet, Queue: b.Queue, Leases: b.Coord,
		Owner:    config.NewIncarnation(nodeID),
		LeaseTTL: e.leaseTTL,
		Roster:   e.activeSeats,
		Work:     e.rehomeSeatWork,
		Runs:     e.retireSeatRuns,
		Sessions: e.retireSeatSessions,
	})
}

// retireSeatRuns ends a retired seat's detached executors, for the mailbox
// retirement holding the seat's lease under owner and epoch, BY THE SEAT'S ID
// ([sandbox.PendingStore.ListActiveForSeatID]): handle is only a label, empty
// for a mailbox the broker's listing alone found; errors name the id, which
// the calling retirement wraps in its label.
//
// The sandbox coordinator reclaims each run's box, announces the loss and
// finishes its record. A node without one cannot reach a box, so it refuses
// while the fleet records any run for the seat, leaving it to a node that runs
// one. A run left WAITING on a tool call ([sandbox.SeatRunsWaiting]) is handed
// back beside the error, never inside it, so the retirement keeps the mailbox
// without failing its tick.
func (e *Engine) retireSeatRuns(ctx context.Context, seat uuid.UUID, handle, owner string, epoch int64,
) (maintenance.SeatRunsWaiting, error) {
	if rt := e.sandbox.Load(); rt != nil {
		waiting, err := rt.coordinator.RetireSeat(ctx, seat, handle, owner, epoch)
		return maintenance.SeatRunsWaiting{Calls: waiting.Calls, Until: waiting.Until}, err
	}
	if e.backends == nil || e.backends.Fleet == nil {
		return maintenance.SeatRunsWaiting{}, fmt.Errorf("engine: this node has no fleet store, so it "+
			"cannot tell whether retired seat %s left its executor in a box", seat)
	}
	runs, err := sandbox.NewCoordStore(e.backends.Fleet).ListActiveForSeatID(ctx, seat)
	if err != nil {
		return maintenance.SeatRunsWaiting{}, fmt.Errorf("engine: reading the executor records of "+
			"retired seat %s: %w", seat, err)
	}
	if len(runs) > 0 {
		return maintenance.SeatRunsWaiting{}, fmt.Errorf("engine: retired seat %s still has %d executor "+
			"records and this node runs no sandbox coordinator to end them (no company it has served "+
			"configures providers.sandbox); its mailbox is kept until a node that runs one holds the "+
			"maintenance duty, or this node applies a company that configures providers.sandbox",
			seat, len(runs))
	}
	return maintenance.SeatRunsWaiting{}, nil
}

// rehomeSeatWork re-homes the open work a retired seat still holds, for the
// mailbox retirement holding its lease (ADR-0162): each item is returned or
// handed to its lead by [worklet.Writer.RehomeDeparted], told which items the
// seat's runs are on, which the retirement ends next.
//
// AGAINST THE FLEET'S REVISION, as the session removal is: a node that has not
// applied it re-homes nothing and says why. A company whose tracker is not
// Worklet holds no work here; one that is, on a node that writes no tracker
// records, keeps the mailbox for a node that does.
func (e *Engine) rehomeSeatWork(ctx context.Context, seat uuid.UUID, handle string,
	retiring time.Time) error {

	company, err := e.servedFleetRevision(ctx)
	if err != nil {
		return fmt.Errorf("engine: retired seat %s's work is kept until this node applies the "+
			"revision the fleet runs: %w", seat, err)
	}
	if company.Config.TrackerBackendFor() != config.TrackerWorklet {
		return nil
	}
	writer := e.TrackerWriter()
	if writer == nil {
		return fmt.Errorf("engine: this node writes no tracker records, so retired seat %s's "+
			"work is re-homed by a node that does", seat)
	}
	if e.backends == nil || e.backends.Fleet == nil {
		return fmt.Errorf("engine: this node has no fleet store, so it cannot tell which items "+
			"retired seat %s's runs are on", seat)
	}
	runs, err := sandbox.NewCoordStore(e.backends.Fleet).ListActiveForSeatID(ctx, seat)
	if err != nil {
		return fmt.Errorf("engine: reading the runs of retired seat %s: %w", seat, err)
	}
	return rehomeDepartedWork(ctx, writer, company.Org, runItems(runs), seat, handle, retiring)
}

// departedWork is the slice of the tracker's writer a retirement re-homes a
// seat's work through.
type departedWork interface {
	DepartedHolders(ctx context.Context) ([]string, error)
	RehomeDeparted(ctx context.Context, d worklet.Departure) ([]worklet.Rehomed, error)
}

// rehomeDepartedWork is [Engine.rehomeSeatWork] once the fleet's revision and
// the seat's runs are read: o is the revision's chart and runs the items the
// seat's runs are on.
//
// A SEAT THE CHART STILL HOLDS keeps its work: an agent seat made a person's
// leaves the agent seats, and so retires its mailbox, while its person holds
// what it held. A mailbox with no handle (one only the broker's listing found)
// is the seat's by its id, derived from each departed holder's handle.
func rehomeDepartedWork(ctx context.Context, work departedWork, o *org.Organization,
	runs []string, seat uuid.UUID, handle string, retiring time.Time) error {

	handles := []string{handle}
	if handle == "" {
		holders, err := work.DepartedHolders(ctx)
		if err != nil {
			return fmt.Errorf("engine: read the departed holders of open work, to find retired "+
				"seat %s's: %w", seat, err)
		}
		handles = handles[:0]
		for _, h := range holders {
			if id, ok := org.DeriveAgentID(o.Name, h); ok && id == seat {
				handles = append(handles, h)
			}
		}
	}
	var errs []error
	for _, h := range handles {
		if o.SeatByHandle(h) != nil {
			continue
		}
		moved, err := work.RehomeDeparted(ctx, worklet.Departure{Handle: h, Runs: runs, At: retiring})
		if len(moved) > 0 {
			led := 0
			for _, m := range moved {
				if m.To != "" {
					led++
				}
			}
			log.InfoContext(ctx, "seat_work_rehomed", "handle", h, "seat", seat.String(),
				"items", len(moved), "to_leads", led, "returned", len(moved)-led,
				"detail", "a retired seat's open work was handed to its leads or returned "+
					"unassigned, each item with a history note naming the seat")
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("engine: re-home retired seat %q's work: %w", h, err))
		}
	}
	return errors.Join(errs...)
}

// runItems is the Worklet items runs are on, by id.
func runItems(runs []sandbox.PendingRun) []string {
	var out []string
	for _, run := range runs {
		if item := run.WorkItem; item != nil && item.Backend == types.WorkWorklet &&
			item.ID != "" && !slices.Contains(out, item.ID) {
			out = append(out, item.ID)
		}
	}
	return out
}

// mailboxRegistry is the node's view of [Engine.mailboxes]: a nil *Mailboxes
// stored in the node's interface would be a non-nil registry whose every call
// panics, so nil is returned as an untyped nil.
func (e *Engine) mailboxRegistry() node.MailboxRegistry {
	if e.mailboxes == nil {
		return nil
	}
	return e.mailboxes
}

// activeSeats is the mailbox sweep's roster: the agent seats of the revision
// the fleet is pointed at, or an error saying why that is unknown.
//
// A retirement deletes mail that cannot be recovered, so the roster is the
// FLEET's current revision, never merely what this node runs: a node that has
// not applied the pointer's activation answers unknown and the tick retires
// nothing ([Engine.servedFleetRevision] holds that rule for every irreversible
// judgement). An unknown that is only this node still applying or starting is
// [maintenance.ErrRevisionPropagating], which the sweep passes without a
// warning, since every activation and boot puts every node there briefly.
func (e *Engine) activeSeats(ctx context.Context) ([]placement.Seat, error) {
	company, err := e.servedFleetRevision(ctx)
	var behind *errBehindFleet
	switch {
	case errors.As(err, &behind) && behind.reconciled():
		return nil, fmt.Errorf("%w; seats are judged once this node has applied it", err)
	case err != nil:
		return nil, err
	}
	return company.Seats(), nil
}

// humanSeatHandles is the chat read-state reconcile's roster: every HUMAN seat
// of the revision the fleet is pointed at, or an error saying why that is
// unknown ([Engine.servedFleetRevision]).
//
// Only a human seat is ever acted as by a person or a bound machine, so a read
// state under any other handle is unreachable and forgotten a grace after it
// is first seen. The SEAT, not its holder, because the record is the seat's
// and passes to its next holder, as the seat's inbox does.
func (e *Engine) humanSeatHandles(ctx context.Context) ([]string, error) {
	company, err := e.servedFleetRevision(ctx)
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

// Maintenance exposes the retention sweep, so an operator can ask whether
// housekeeping is running and over what.
func (e *Engine) Maintenance() *maintenance.Worker { return e.maintenance.Load() }

// stopMaintenance ends the sweep, waiting for an in-flight tick.
func (e *Engine) stopMaintenance() {
	if w := e.maintenance.Swap(nil); w != nil {
		w.Stop()
	}
}

// inboxRetention is how long this company's inbox rows live, read off the
// PUBLISHED company at every sweep ([maintenance.Horizon]), so a revision that
// moves it is honoured by the next one. A node with no company sweeps on the
// shipped default, because a zero read as "retain nothing" would delete the
// inbox; validation fills the default for a company that leaves it unset.
func (e *Engine) inboxRetention() time.Duration {
	c := e.Company()
	if c == nil {
		return config.DefaultInboxRetentionDays * 24 * time.Hour
	}
	return c.Config.Inbox.Retention()
}
