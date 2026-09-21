package maintenance

import (
	"context"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/store"
)

// The retentions, one per table and tied to what that table is FOR.
//
// Not a single global number: these tables answer different questions over
// different horizons, and a shared constant would be wrong for all of them at
// once — too short for the ledger a scheduler still consults, too long for a
// dedupe window that stops mattering in minutes.
//
// Only the tables THIS NODE owns are here. The four that answer a question a
// fleet has to agree on — the completion ledger, the delivery dedupe, the
// rate valve and the per-node apply status — moved to internal/coord, and
// their retentions moved with them: a bucket's own age, declared beside the
// contract in coord/fleet.go. Restating them here would be two numbers to
// keep equal for no reader's benefit.
const (
	// ScheduledRunRetention is bounded by how far back a fire could still
	// be re-evaluated, not by how much history is pleasant to read: a
	// claim row older than the catchup ceiling can no longer refuse any
	// tick, because no tick will evaluate a fire that old.
	//
	// The margin over that ceiling is generous because this is the one
	// swept table a human actually reads — the dashboard's schedules view
	// — and a week of scheduled runs is both a useful history and a
	// trivial number of rows.
	ScheduledRunRetention = 7 * 24 * time.Hour

	// ConversationRetention is how long a seat remembers what it already
	// said in one thread. Thirty days is the default; it is the one
	// retention an operator can override, because a company running
	// long-lived tickets may legitimately want a conversation remembered
	// past the event store's own horizon.
	ConversationRetention = 30 * 24 * time.Hour

	// ChannelRetention keeps a CLOSED agent-to-agent channel readable for
	// a week — long enough for an operator to reconstruct an exchange from
	// the dashboard after a weekend.
	ChannelRetention = 7 * 24 * time.Hour

	// ChannelIdleTimeout is how long an OPEN channel may stay open with
	// nothing happening.
	//
	// A channel is closed by the answering turn. A turn that crashes, or a
	// node that dies between the wake and the answer, leaves one open
	// forever — and an open channel is not free: it is a row, and it is a
	// promise to the requester that a reply may still arrive.
	//
	// One hour, against a turn whose own worst case is around twenty
	// minutes under the Execute extension ceiling. Three times the longest
	// thing that could legitimately still be running, so the sweep never
	// closes a channel somebody is about to answer on.
	ChannelIdleTimeout = time.Hour

	// CounterpartyRetention is how long a profile survives with no
	// interaction.
	//
	// The table had NO horizon at all: one row per distinct human or seat a
	// seat has ever messaged, and — unlike every other table here — it is
	// also republished to every peer for each held seat on every
	// memory-sync cycle, so its growth costs more than storage.
	//
	// A HUNDRED AND EIGHTY DAYS, twice FollowRetention above, and the
	// factor is the reason rather than the number. A thread-follow is a
	// live conversation, which is stale after a quarter. A counterparty
	// profile is a durable preference — "this reviewer wants tests first" —
	// which does not stop being true because nobody has written this
	// quarter, so it deserves a horizon well past the point where the
	// relationship itself has clearly lapsed.
	//
	// The asymmetry makes it safe: dropping one costs the interaction
	// COUNT, which is a cadence signal rather than a fact, and the next
	// message re-creates the row and the seat re-learns from what it
	// observes. Keeping every profile for ever costs unbounded growth on a
	// table the fleet copies between nodes.
	CounterpartyRetention = 180 * 24 * time.Hour

	// RevisionRetention is how long a superseded company_config revision
	// is kept.
	//
	// # The table had no horizon at all
	//
	// A node keeps its OWN copy of every revision it has ever met, in an
	// append-only table, and nothing deleted from it — so a company that
	// edits its configuration daily accumulates a row per edit per node
	// for the life of the deployment, each row a copy of the whole
	// document. It is also where a pre-split revision's `roles[].email`
	// sits, which is why the sweep ships beside `crewlet config scrub`:
	// the scrub erases what is inside a row it keeps, and this is what
	// eventually removes the row.
	//
	// # The floor, which is a relation rather than a number
	//
	// An audit row that names a revision must still be able to open it, so
	// this may never fall below [store.EventRetention] — the audit log's
	// own horizon. That relation, not this value, is the thing to preserve
	// if either moves; TestARevisionOutlivesTheAuditRowThatNamesIt is what
	// holds it, because the two constants are in different packages and
	// nothing else connects them.
	//
	// # Four hundred days, and why not a year
	//
	// The gestures that reach furthest back into configuration history are
	// ANNUAL: a reorganisation repeated each year, a compliance review, a
	// renewal that changes an integration. Exactly 365 days makes finding
	// last year's revision a coin flip on the day the gesture is repeated
	// — it is gone if this year's run is a day late. Four hundred days is
	// that cycle plus five weeks, which is the margin an annual thing
	// actually drifts by.
	//
	// It costs little: the active revision and its whole parent chain are
	// kept whatever their age (see [store.Configs.Purge]), so what this
	// bounds is abandoned branches, and a revision after the org chart's
	// split is a few kilobytes rather than a whole company.
	RevisionRetention = 400 * 24 * time.Hour
)

// StoreJobs is the sweep for everything in the main store.
//
// EVERY JOB HERE IS [NodeLocal], and that is a property of the estate rather
// than of any one table: this is the node's own file, owned exclusively by
// this process, so no peer's sweep can reach these rows and a tick that skips
// them because somebody else holds the duty skips them for ever. Both were
// [Fleet] by omission until the scope became a required answer — including
// `events`, which is the audit log, so a company's configured event retention
// applied on whichever single node happened to hold the duty.
//
// A nil database contributes nothing rather than a job that fails every
// tick: a deployment with no store is a real deployment, and its in-memory
// twins prune themselves inline because a process-local map dies with the
// process.
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
	return []Job{Purge("company_config", NodeLocal, RevisionRetention,
		db.Configs().Purge)}
}

// OpsLedger is the slice of a state-log applier this sweep calls. Declared
// here, by the consumer, like every other seam in this tree.
type OpsLedger interface {
	// PurgeOps deletes this node's operation rows applied before cutoff,
	// reporting how many went.
	PurgeOps(ctx context.Context, cutoff time.Time) (int64, error)

	// PurgeAnchors deletes this node's arbitration anchors below the
	// domain's published trim floor, reporting how many went.
	//
	// KEYED ON THE FLOOR AND NOT ON A CUTOFF, which is why it does not
	// take one: an anchor answers what a subject's last record expects,
	// and a row above the floor must stay however old it is, because an
	// anchor read as absent hands the next writer an expectation the
	// broker refuses for ever. The caller has no floor to pass — the
	// domain publishes its own — so the seam asks for none.
	PurgeAnchors(ctx context.Context) (int64, error)
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
}

// StatelogJobs sweeps each registered domain's operation ledger.
//
// [NodeLocal], and that is what this job is FOR. Every `<domain>_ops` migration
// says the table is swept and ships `<domain>_ops_swept_idx` for the range
// delete — and nothing swept them, so a row was written for every applied
// record and never deleted, on every node, for the life of the deployment. On
// the census rate that is 357 MB a year of a table whose only reader asks
// "did my operation land", a question nobody asks about a month-old op id.
//
// It is node-local rather than a fleet singleton because each node owns its
// own copy: the rows record which operations THIS applier wrote. Swept under
// the singleton it would be tidied on one node and grow for ever on the others
// — which looks exactly like a sweep that is working, to the operator who
// checks the node it ran on. For a long time this was the ONLY job that said
// so, while six others needed to; see [Scope].
func StatelogJobs(domains map[string]OpsHorizon) []Job {
	names := make([]string, 0, len(domains))
	for name := range domains {
		names = append(names, name)
	}
	// SORTED, so the log's job order is the same on every node and every
	// tick. A map's iteration order would make one node's sweep line look
	// like a different sweep from its peer's.
	slices.Sort(names)

	jobs := make([]Job, 0, 2*len(names))
	for _, name := range names {
		domain := domains[name]
		jobs = append(jobs, Job{
			Name: name + "_ops", Scope: NodeLocal, Horizon: domain.Retention,
			Run: func(ctx context.Context, _, cutoff time.Time) (int64, error) {
				return domain.Ledger.PurgeOps(ctx, cutoff)
			},
		}, Job{
			// AND THE ANCHORS, which had no sweep at all:
			// `0001_the_state_log_lands.sql` ships
			// `statelog_anchor_swept_idx` and states that the sweep
			// needs it, and nothing ever wrote the delete. One row
			// per arbitrated subject, for the life of the
			// deployment, in the replicated estate and therefore in
			// every snapshot artefact and every backup.
			//
			// NO HORIZON, because this one is not keyed on a clock:
			// the cutoff is the domain's own published trim floor,
			// which the ledger reads for itself. A horizon here
			// would be a second opinion about what a log still
			// holds.
			Name: name + "_anchors", Scope: NodeLocal,
			Run: func(ctx context.Context, _, _ time.Time) (int64, error) {
				return domain.Ledger.PurgeAnchors(ctx)
			},
		})
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
}

// LearningJobs is the sweep for the learning subsystem's diary.
//
// No Horizon: every short-term entry carries its own deadline (`ttl_until`,
// stamped at write), so the job needs now rather than a cutoff, and long-term
// entries — NULL deadline — are never touched. The read path already filters
// expired rows out of recall, but reads cannot delete: without this job every
// expired short-term memory stays a row the per-agent vector scan pays for on
// every turn start, for the life of the deployment.
//
// This is the exact failure the package doc names — Diary.Expire existed,
// diary.go's comments described the background sweep, and nothing anywhere
// called it.
//
// [NodeLocal]: a seat's diary rows are written into whichever node's store was
// running its turn, and memsync carries them between nodes as a compacted
// changelog rather than making one node's copy authoritative. So every node
// holds rows only its own sweep can reach.
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
	return []Job{Purge("counterparty_profiles", NodeLocal, CounterpartyRetention, c.Purge)}
}

// Channels is the half of the agent-to-agent surface this sweep drives.
//
// DECLARED HERE, by the caller, and narrowed to two methods — which is what
// changed the shape of this job rather than only its type. It used to take
// a2a.Store and call CloseIdle on it directly, and that method returns the
// channels it closed precisely so its caller can announce them; this one threw
// the list away, so a swept close was the one channel ending that published no
// event at all. Closing is now asked of the SERVICE, which owns what a close
// means on the wire, and internal/maintenance stays a scheduler of jobs rather
// than a second author of event payloads. a2a.Service is the implementation.
type Channels interface {
	// SweepIdle closes every channel idle since before cutoff, announces
	// each one, and reports how many it closed.
	SweepIdle(ctx context.Context, cutoff time.Time) (int, error)

	// Purge deletes channels closed before cutoff.
	Purge(ctx context.Context, cutoff time.Time) (int64, error)
}

// ChannelJobs is the sweep for agent-to-agent channels: close what nobody
// answered, then delete what has been closed long enough.
//
// TWO jobs rather than one, and the order matters only in that both run
// every tick. Closing is a state change and deleting is garbage collection —
// a channel closed by this tick is a week away from being deleted, which is
// the week an operator has to read it.
//
// The ONE shared record still swept here, and the exception the coordination
// store's retention rule makes room for. Every other bucket expires on its own
// age, which is why the four records above left this file — but a bucket's age
// cannot tell an OPEN channel from a closed one, so a TTL would reap the
// authorization record of an ask still waiting for its answer. Both halves are
// therefore decisions, taken under the same singleton duty as every local
// sweep. See coord.Channels and internal/store/schema/0012.
func ChannelJobs(c Channels) []Job {
	if c == nil {
		return nil
	}
	return []Job{
		{
			// The CUTOFF is what this job acts on and the tick's `now` is
			// not: the service reads its own clock for the close instant,
			// so a channel's closed_at and the event's duration come from
			// one reading rather than two.
			Name: "a2a_channels_idle", Scope: Fleet, Horizon: ChannelIdleTimeout,
			Run: func(ctx context.Context, _, cutoff time.Time) (int64, error) {
				closed, err := c.SweepIdle(ctx, cutoff)
				return int64(closed), err
			},
		},
		Purge("a2a_channels", Fleet, ChannelRetention, c.Purge),
	}
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
	return []Job{PurgeN("scheduled_runs", NodeLocal, ScheduledRunRetention, l.Purge)}
}

// LedgerJobs is the sweep for the turn ledgers.
//
// ONE JOB, not two: the completion ledger moved to the coordination store,
// where the bucket's own age is the retention and the broker expires the
// records — see coord.LedgerRetention, and coordtest's guard that it still
// outlasts the scheduler's catchup ceiling.
//
// [NodeLocal]: a conversation row records what a seat said in a thread from
// the node that ran the turn, so each node holds its own and no peer's sweep
// reaches it.
//
// conversationRetention is the operator-facing horizon. Zero or less takes
// [ConversationRetention] — the engine's config validation refuses a
// retention below one day, so this floor is for a caller that built its
// stores directly, and it exists because the alternative reading of zero is
// "delete every conversation on the next tick".
func LedgerJobs(s ledgerstore.Conversations, conversationRetention time.Duration) []Job {
	if conversationRetention <= 0 {
		conversationRetention = ConversationRetention
	}
	if s == nil {
		return nil
	}
	return []Job{Purge("conversation_sessions", NodeLocal, conversationRetention, s.Purge)}
}
