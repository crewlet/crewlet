package maintenance

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/store"
)

// The retentions, one per table and tied to what that table is FOR.
//
// Not one global number: the tables answer different questions over different
// horizons. Only the tables THIS NODE owns are here; the four a fleet must
// agree on (completion ledger, delivery dedupe, rate valve, per-node apply
// status) keep their retention as a bucket's own age in coord/fleet.go.
const (
	// ScheduledRunRetention is bounded by how far back a fire could still
	// be re-evaluated: a claim row older than the catchup ceiling can no
	// longer refuse any tick. The margin is generous because the dashboard's
	// schedules view reads this table and a week is few rows.
	//
	// internal/usage READS IT TOO (PublisherDeps.FireHistory): a day older
	// than this is derived again without its schedules, since the sweep may
	// have cut into its fires and a short schedule-day would replace a whole
	// one on every node. Shortening it shortens that re-derived history.
	ScheduledRunRetention = 7 * 24 * time.Hour

	// ChannelRetention keeps a CLOSED agent-to-agent channel readable for
	// a week — long enough for an operator to reconstruct an exchange from
	// the dashboard after a weekend.
	ChannelRetention = 7 * 24 * time.Hour

	// ChannelIdleTimeout is how long an OPEN channel may stay quiet before
	// the sweep closes it, where nothing still owes its answer.
	//
	// Waits with no clock-sized end are SPARED, not timed (internal/a2a's
	// Owed). The longest silence left is a delivery a dead node held
	// unacknowledged: ack wait (jetstream.DefaultAckWait, 30 min), the next
	// holder's lease (seat.SeatLeaseTTL, 45 s), two dispatch segments
	// (runner.ClaimTTL, 15 min): about an hour. Two hours: closing early drops
	// an answer on its way, closing late only delays a2a_channel_closed.
	// TestAnIdleAskOutlastsTheSilenceOfOneStillOwed (internal/engine) holds it.
	ChannelIdleTimeout = 2 * time.Hour

	// CounterpartyRetention is how long a profile survives with no
	// interaction.
	//
	// Republished to every peer each memory-sync cycle, so growth costs more
	// than storage. A HUNDRED AND EIGHTY DAYS, twice FollowRetention above: a
	// follow is stale after a quarter, a profile is a durable preference.
	// Dropping one costs only the interaction COUNT.
	CounterpartyRetention = 180 * 24 * time.Hour

	// RevisionRetention is how long a superseded company_config revision
	// is kept.
	//
	// The floor is a relation: an audit row naming a revision must still be
	// able to open it, so this never falls below [store.EventRetention]
	// (TestARevisionOutlivesTheAuditRowThatNamesIt; different packages).
	//
	// Four hundred days, not a year: the furthest-reaching lookups are ANNUAL,
	// and 365 days loses last year's revision if this year's run is a day
	// late. The active revision and its parent chain are kept whatever their
	// age ([store.Configs.Purge]); this bounds only abandoned branches.
	RevisionRetention = 400 * 24 * time.Hour
)

// StoreJobs is the sweep for everything in the main store.
//
// EVERY JOB HERE IS [NodeLocal], a property of the estate: this is the node's
// own file, so no peer's sweep can reach these rows and a tick skipped because
// somebody else holds the duty skips them for ever (`events`, the audit log,
// included).
//
// A nil database contributes nothing rather than a job that fails every tick:
// a deployment with no store is real, and its in-memory twins prune themselves
// inline.
func StoreJobs(db *store.DB) []Job {
	if db == nil {
		return nil
	}
	log := db.Events()
	return []Job{
		// The event log carries its OWN horizon — retention is a property
		// of the log, set where the log is configured — so it declares no
		// Horizon here and ignores the cutoff.
		{Name: "events", Scope: NodeLocal,
			Run: func(ctx context.Context, _, _ time.Time) (int64, error) {
				return log.Purge(ctx)
			}},
		// NOT HERE any more: webhook_deliveries, rate_limits,
		// turn_completions, config_apply_status and chat_thread_follows.
		// All five moved to the coordination store, where a bucket's own
		// age is the retention and the BROKER expires the records — so
		// there is nothing left for a sweep to delete, and a job that
		// swept an empty table every tick would only report that it had.
	}
}

// ConfigJobs is the sweep for the company's stored configuration revisions.
//
// [NodeLocal], for the reason every job in [StoreJobs] is: this is the node's
// own file, and each node adopts its own copy of every revision it meets. A
// fleet singleton would tidy the node holding the duty and let the table grow
// for ever on every other — which looks exactly like a sweep that works, to
// the operator who checks the node it ran on.
//
// SEPARATE FROM [StoreJobs] because it is armed separately: this is the one
// sweep whose rows are the CONTROL PLANE's, and a deployment reading the job
// list wants to see it named rather than folded into a general store sweep.
func ConfigJobs(db *store.DB) []Job {
	if db == nil {
		return nil
	}
	return []Job{Purge("company_config", NodeLocal, Fixed(RevisionRetention),
		db.Configs().Purge)}
}

// OpsLedger is the slice of a state-log applier this sweep calls. Declared
// here, by the consumer, like every other seam in this tree.
type OpsLedger interface {
	// PurgeOps deletes this node's operation rows applied before cutoff,
	// reporting how many went.
	PurgeOps(ctx context.Context, cutoff time.Time) (int64, error)

	// PurgeOpsOfKind is PurgeOps over one subject kind's rows.
	PurgeOpsOfKind(ctx context.Context, kind string, cutoff time.Time) (int64, error)
}

// OpsHorizon is one domain's operation-ledger horizon, declared at its
// registration entry.
//
// PER DOMAIN, because the question the ledger answers is "did my operation
// land", asked by a RETRYING CLIENT: a seat told to carry an op id forward and
// re-ask on its next wake, after a weekend. That client is the same for every
// domain today, which is why they all take the framework's default — but the
// horizon is the domain's to state, so a domain whose writers re-ask on a
// different rhythm says so where it is declared rather than changing a
// constant every other domain reads.
type OpsHorizon struct {
	Ledger    OpsLedger
	Retention time.Duration

	// Kinds are the subject kinds whose rows go sooner than Retention,
	// each with its own horizon — a kind no slow client writes, whose op
	// ids are re-asked inside the publisher's resolve budget if at all.
	// The identity domain's sessions are the one today: a row per sign-in
	// and per sign-out, kept a month for a question nobody asks after an
	// hour. Empty is "one horizon for the whole ledger".
	Kinds map[string]time.Duration
}

// StatelogJobs sweeps each registered domain's operation ledger: one job per
// ledger, named `<domain>_ops` after the domain it is keyed by, and one more
// per subject kind with a horizon of its own (`<domain>_ops_<kind>`). Every
// horizon here is this build's own and never moves, so each is [Fixed].
//
// [NodeLocal]: the rows record which operations THIS applier wrote, so under
// the singleton one node would be tidied and the others grow for ever (see
// [Scope]; 357 MB a year at the census rate). The anchors (`statelog_anchor`)
// are not swept here: they are REPLICATED rows, which only an applier writes
// (ADR-0002), and a per-node delete would leave two copies of one log with
// different anchors. The trim removes them at one position (ADR-0080).
func StatelogJobs(domains map[string]OpsHorizon) []Job {
	// SORTED, so the log's job order is the same on every node and every
	// tick. A map's iteration order would make one node's sweep line look
	// like a different sweep from its peer's.
	names := slices.Sorted(maps.Keys(domains))

	jobs := make([]Job, 0, len(names))
	for _, name := range names {
		domain := domains[name]
		jobs = append(jobs, Job{
			Name: name + "_ops", Scope: NodeLocal, Horizon: Fixed(domain.Retention),
			Run: func(ctx context.Context, _, cutoff time.Time) (int64, error) {
				return domain.Ledger.PurgeOps(ctx, cutoff)
			},
		})
		// A KIND WITH A HORIZON OF ITS OWN is a job of its own, so its
		// deletions are reported under a name that says which rows went
		// and the default job's count keeps meaning what it did. Sorted
		// for the reason the domains are.
		for _, kind := range slices.Sorted(maps.Keys(domain.Kinds)) {
			jobs = append(jobs, Job{
				Name: name + "_ops_" + kind, Scope: NodeLocal,
				Horizon: Fixed(domain.Kinds[kind]),
				Run: func(ctx context.Context, _, cutoff time.Time) (int64, error) {
					return domain.Ledger.PurgeOpsOfKind(ctx, kind, cutoff)
				},
			})
		}
	}
	return jobs
}

// CounterpartyStore is the slice of the counterparty profiles this sweep
// calls. Declared here, by the consumer, like every other seam in this tree.
type CounterpartyStore interface {
	// Purge drops profiles with no interaction since cutoff, reporting how
	// many went.
	Purge(ctx context.Context, cutoff time.Time) (int64, error)
}

// DiaryStore is the slice of the learning diary this sweep calls. Declared
// here, by the consumer, like every other seam in this tree.
type DiaryStore interface {
	// Expire deletes short-term entries whose deadline has passed,
	// reporting how many went.
	Expire(ctx context.Context, now time.Time) (int64, error)

	// TrimLong drops a seat's least-useful durable entries once it holds
	// more than cap of them. Zero takes the shipped cap.
	TrimLong(ctx context.Context, cap int) (int64, error)

	// RetireHeld deletes entries held back for longer than a held note's
	// life, reporting how many went.
	RetireHeld(ctx context.Context, now time.Time) (int64, error)
}

// LearningJobs is the sweep for the learning subsystem's diary.
//
// No Horizon: every short-term entry carries its own deadline (`ttl_until`),
// so the job needs now rather than a cutoff, and long-term entries (NULL
// deadline) are never touched. Recall filters expired rows but reads cannot
// delete, so without this every expired memory stays a row the per-agent
// vector scan pays for on every turn start.
//
// [NodeLocal]: diary rows are written into whichever node ran the turn and
// memsync carries them as a compacted changelog, so every node holds rows only
// its own sweep can reach.
func LearningJobs(d DiaryStore) []Job {
	if d == nil {
		return nil
	}
	return []Job{
		{
			Name: "agent_diary", Scope: NodeLocal,
			Run: func(ctx context.Context, now, _ time.Time) (int64, error) {
				return d.Expire(ctx, now)
			},
		},
		{
			// The DURABLE half, and the only sweep here bounded by a
			// count rather than a clock: a diary_long row is a fact
			// the agent marked durable, so it has no deadline to
			// pass — but recall scans every one of them on every
			// turn start, so it cannot be unbounded either. See
			// learning.DiaryLongCap.
			Name: "agent_diary_long", Scope: NodeLocal,
			Run: func(ctx context.Context, _, _ time.Time) (int64, error) {
				return d.TrimLong(ctx, 0)
			},
		},
		{
			// THE HELD HALF: a note an upgrade or an access change held back,
			// and nobody kept again within a held note's life
			// (learning.HeldNoteLife).
			Name: "agent_diary_held", Scope: NodeLocal,
			Run: func(ctx context.Context, now, _ time.Time) (int64, error) {
				return d.RetireHeld(ctx, now)
			},
		},
	}
}

// CounterpartyJobs is the sweep for what a seat has learned about other
// people.
//
// Separate from LearningJobs because the seam is: this reads a different
// store, and folding it in would make LearningJobs' one parameter two.
//
// [NodeLocal] for the same reason as the diary, and with more at stake: a
// profile is republished to every peer on each memory-sync cycle, so a copy
// that is never swept is a copy that is repeatedly sent.
func CounterpartyJobs(c CounterpartyStore) []Job {
	if c == nil {
		return nil
	}
	return []Job{Purge("counterparty_profiles", NodeLocal, Fixed(CounterpartyRetention), c.Purge)}
}

// Channels is the half of the agent-to-agent surface this sweep drives.
//
// DECLARED HERE, by the caller, and narrowed to two methods — which is what
// changed the shape of this job rather than only its type. It used to take
// a2a.Store and close idle channels through it directly, and that close
// returned the channels it closed precisely so its caller could announce them;
// this one threw the list away, so a swept close was the one channel ending
// that published no event at all. Closing is now asked of internal/a2a, which
// owns what a close means on the wire — and which asks, before each one,
// whether the channel's answer is still owed — and internal/maintenance stays a
// scheduler of jobs rather than a second author of event payloads. a2a.Sweeper
// is the implementation.
type Channels interface {
	// SweepIdle closes every channel idle since before cutoff whose answer
	// is no longer owed, announces each one, and reports how many it
	// closed.
	SweepIdle(ctx context.Context, cutoff time.Time) (int, error)

	// Purge deletes channels closed before cutoff.
	Purge(ctx context.Context, cutoff time.Time) (int64, error)
}

// ChannelJobs is the sweep for agent-to-agent channels: close what nobody will
// answer, then delete what has been closed long enough.
//
// TWO jobs: closing is a state change, deleting is garbage collection, and a
// channel closed this tick gets a week for an operator to read it.
//
// A shared record swept here, an exception to the coordination store's
// retention rule (the mailboxes and [MarkerJobs] are the others): a bucket's
// age cannot tell an OPEN channel from a closed one, so a TTL would reap the
// authorization record of an ask still awaiting its answer. Both halves are
// decisions under the singleton duty. See coord.Channels and
// internal/store/schema/0012.
func ChannelJobs(c Channels) []Job {
	if c == nil {
		return nil
	}
	return []Job{
		{
			// The CUTOFF is what this job acts on and the tick's `now` is
			// not: the sweeper reads its own clock for the close instant,
			// so a channel's closed_at and the event's duration come from
			// one reading rather than two.
			Name: "a2a_channels_idle", Scope: Fleet, Horizon: Fixed(ChannelIdleTimeout),
			Run: func(ctx context.Context, _, cutoff time.Time) (int64, error) {
				closed, err := c.SweepIdle(ctx, cutoff)
				return int64(closed), err
			},
		},
		Purge("a2a_channels", Fleet, Fixed(ChannelRetention), c.Purge),
	}
}

// Markers is the coordination store's marker sweep. Declared here, by the
// consumer, like every other seam in this tree; coord.Fleet is the
// implementation.
type Markers interface {
	// SweepMarkers removes the removal markers written before cutoff from
	// every record family no clock ages, and every part beside a sandbox
	// run (its bridged tool log, the sealed secrets its box was handed)
	// written before it whose run has no record — what a node that stopped
	// between a run's removals leaves — reporting how many went, both kinds
	// counted together.
	SweepMarkers(ctx context.Context, cutoff time.Time) (int64, error)
}

// MarkerJobs is the sweep for the coordination store's removal MARKERS: the
// delete or purge record a KV removal leaves in a bucket the broker never
// ages, which every listing pays a leader read for. Also the one LIVE-LOOKING
// value a removal can leave: a part beside a sandbox run (tool log, sealed
// secrets) whose record is gone because its node stopped between the two
// removals. The swept count is both kinds.
//
// A shared record swept here, as the channels, mailboxes and chat read state
// are: these buckets hold records no clock may reap (an open ask, a parked
// run, a credential). [Fleet]: one copy, so N sweepers would be N times the
// purges. Its horizon, [coord.MarkerRetention], is where the value is argued.
func MarkerJobs(m Markers) []Job {
	if m == nil {
		return nil
	}
	return []Job{Purge("coordination_markers", Fleet, Fixed(coord.MarkerRetention), m.SweepMarkers)}
}

// ScheduleJobs is the sweep for the scheduled-run ledger.
//
// [NodeLocal]. The fleet half of scheduling — "may I start this fire" — moved
// to coordination, where the bucket's own age is the retention; what is left
// in `scheduled_runs` is this node's own audit row for the fires it ran. See
// internal/schedule/sharedclaim.go for the split and what the old shape cost.
func ScheduleJobs(l schedule.Ledger) []Job {
	if l == nil {
		return nil
	}
	return []Job{PurgeN("scheduled_runs", NodeLocal, Fixed(ScheduledRunRetention), l.Purge)}
}
