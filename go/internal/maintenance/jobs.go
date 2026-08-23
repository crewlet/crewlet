package maintenance

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/a2a"
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
const (
	// DeliveryRetention is garbage collection, NOT expiry. Claim enforces
	// the TTL itself in its conflict clause, so a row past it is already
	// re-claimable and deleting it changes no behaviour. The multiplier
	// keeps that true when the sweep is late: a row goes only once it is
	// well past the point any claim could still consult it.
	DeliveryRetention = 4 * store.DeliveryTTL

	// RateLimitRetention is one width past the widest window any caller
	// could plausibly ask for. The valve's own window is a second and
	// Allow only ever reads the CURRENT one, so an hour is three orders of
	// magnitude of headroom on a row that cannot affect an answer.
	RateLimitRetention = time.Hour

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

	// CompletionRetention is the same bound for the same reason, and it is
	// the one with teeth: the completion ledger answers "has this trigger
	// already been worked?", so deleting a row a tick could still evaluate
	// lets that fire run TWICE. Its floor is therefore the catchup
	// ceiling, and the week is margin on top.
	CompletionRetention = 7 * 24 * time.Hour

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

	// ApplyStatusRetention bounds the config plane's per-node rows.
	//
	// The odd one out, and the reason it went unswept longest: it is keyed
	// by NODE rather than by event, so it does not look like a
	// short-horizon table. It grows the same way regardless — a node that
	// is scaled in, redeployed or crashed leaves its last row behind,
	// which under generated pod names is one row per pod that ever ran.
	ApplyStatusRetention = 7 * 24 * time.Hour
)

// StoreJobs is the sweep for everything in the main store.
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
		{Name: "events", Run: func(ctx context.Context, _, _ time.Time) (int64, error) {
			return log.Purge(ctx)
		}},
		Purge("webhook_deliveries", DeliveryRetention, db.DeliveryLog().Purge),
		Purge("rate_limits", RateLimitRetention, db.RateLimits().Purge),
		Purge("config_apply_status", ApplyStatusRetention, db.ControlPlane().Purge),
	}
}

// ChannelJobs is the sweep for agent-to-agent channels: close what nobody
// answered, then delete what has been closed long enough.
//
// TWO jobs rather than one, and the order matters only in that both run
// every tick. Closing is a state change and deleting is garbage collection —
// a channel closed by this tick is a week away from being deleted, which is
// the week an operator has to read it.
func ChannelJobs(s a2a.Store) []Job {
	if s == nil {
		return nil
	}
	return []Job{
		{
			Name: "a2a_channels_idle", Horizon: ChannelIdleTimeout,
			Run: func(ctx context.Context, now, cutoff time.Time) (int64, error) {
				closed, err := s.CloseIdle(ctx, cutoff, now)
				return int64(len(closed)), err
			},
		},
		Purge("a2a_channels", ChannelRetention, s.Purge),
	}
}

// ScheduleJobs is the sweep for the scheduled-run ledger.
func ScheduleJobs(l schedule.Ledger) []Job {
	if l == nil {
		return nil
	}
	return []Job{PurgeN("scheduled_runs", ScheduledRunRetention, l.Purge)}
}

// LedgerJobs is the sweep for the turn ledgers.
//
// conversationRetention is the operator-facing horizon. Zero or less takes
// [ConversationRetention] — the engine's config validation refuses a
// retention below one day, so this floor is for a caller that built its
// stores directly, and it exists because the alternative reading of zero is
// "delete every conversation on the next tick".
func LedgerJobs(c ledgerstore.Completions, s ledgerstore.Conversations, conversationRetention time.Duration) []Job {
	if conversationRetention <= 0 {
		conversationRetention = ConversationRetention
	}
	var jobs []Job
	if c != nil {
		jobs = append(jobs, Purge("turn_completions", CompletionRetention, c.Purge))
	}
	if s != nil {
		jobs = append(jobs, Purge("conversation_sessions", conversationRetention, s.Purge))
	}
	return jobs
}
