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

	// FollowRetention is how long a chat thread-follow survives with no
	// activity. updated_at is refreshed on every re-assert — a mention, a
	// collective address, the seat posting into the thread — so it is a
	// true last-activity stamp rather than a creation date.
	//
	// Ninety days is the point past which a chat thread has stopped being
	// a live conversation on every backend that ships one: Slack and
	// Mattermost both surface a quarter-old thread only through search.
	//
	// The asymmetry decides the value. Dropping a stale follow costs at
	// most one missed NON-mention reply, and the very next mention
	// re-follows through the ordinary path — while keeping every follow
	// for ever costs unbounded growth on a table read on the hot path of
	// every inbound chat message. A cheap, self-healing miss beats an
	// unbounded read.
	FollowRetention = 90 * 24 * time.Hour
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
		// NOT HERE any more: webhook_deliveries, rate_limits,
		// turn_completions and config_apply_status. All four moved to the
		// coordination store, where a bucket's own age is the retention
		// and the BROKER expires the records — so there is nothing left
		// for a sweep to delete, and a job that swept an empty table
		// every tick would only report that it had.
		Purge("chat_thread_follows", FollowRetention, db.ThreadFollows().Purge),
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
// ONE JOB, not two: the completion ledger moved to the coordination store,
// where the bucket's own age is the retention and the broker expires the
// records — see coord.LedgerRetention, and coordtest's guard that it still
// outlasts the scheduler's catchup ceiling.
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
	return []Job{Purge("conversation_sessions", conversationRetention, s.Purge)}
}
