package engine_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/schedule"
)

// THE assertion whose absence was the bug. Every one of these tables ships a
// Purge and an index for it, and every migration says the rows are swept —
// and if nothing ever calls any of them, all of them grow for the life of the
// deployment. A store gaining a Purge with no entry here
// now fails this list rather than going quietly unswept.
func TestTheEngineSweepsEveryShortHorizonTable(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})

	w := e.Maintenance()
	if w == nil {
		t.Fatal("the engine armed no retention sweep")
	}
	// STARTED, not merely constructed. A worker that was built and never
	// run looks identical from every other vantage point to one quietly
	// doing its job — which is exactly how a full set of Purge methods
	// ships with nothing ever calling them.
	if !w.Running() {
		t.Fatal("the sweep was built but never started")
	}
	got := w.Jobs()
	slices.Sort(got)
	want := []string{
		// NOT HERE: webhook_deliveries, rate_limits, turn_completions
		// and config_apply_status. All four moved to the coordination
		// store, where a bucket's own age is the retention and the
		// BROKER expires the records — so there is nothing on this node
		// left to sweep.
		"a2a_channels",
		"a2a_channels_idle",
		// The diary sweep earns its place here the hard way: Expire
		// shipped with the diary, diary.go described the background
		// sweep, and for as long as this list did not name it, nothing
		// anywhere called it — expired short-term memories stayed rows
		// every recall scanned, forever.
		"agent_diary",
		"agent_diary_long",
		"chat_thread_follows",
		"conversation_sessions",
		// Added the same way the diary was: the table shipped with a
		// memsync entry that republishes every row to every peer on
		// every cycle, and no horizon anywhere — so it grew with the
		// deployment's AGE rather than its size, and this list not
		// naming it was the only place that showed.
		"counterparty_profiles",
		"events",
		"scheduled_runs",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("swept tables:\n got %v\nwant %v", got, want)
	}
}

// The tick must stay shorter than every horizon, or a table sits past its
// own horizon for the difference and the horizon stops describing the table.
// [maintenance.New] enforces this at runtime by raising a short horizon; this
// asserts the SHIPPED ones never need raising, so nobody discovers the rule
// from a warning in production.
func TestEveryRetentionOutlastsTheSweepInterval(t *testing.T) {
	t.Parallel()
	for table, horizon := range map[string]time.Duration{
		// NOT turn_completions: it is retained by the COORDINATION
		// bucket now, whose age no sweep here ticks against.
		// coordtest holds that horizon to the catchup ceiling.
		"scheduled_runs":        maintenance.ScheduledRunRetention,
		"conversation_sessions": maintenance.ConversationRetention,
		"a2a_channels":          maintenance.ChannelRetention,
		"a2a_channels_idle":     maintenance.ChannelIdleTimeout,
		"chat_thread_follows":   maintenance.FollowRetention,
		"counterparty_profiles": maintenance.CounterpartyRetention,
	} {
		if horizon <= maintenance.Interval {
			t.Errorf("%s retention (%v) is not longer than the %v tick",
				table, horizon, maintenance.Interval)
		}
	}
}

// A scheduler claim answers "did this fire already run?", so deleting a row a
// tick could still evaluate lets that fire run TWICE. Its floor is the catchup
// ceiling, not a number somebody liked. The completion ledger is held to the
// same rule in coordtest, against coord.LedgerRetention.
func TestTheScheduleHorizonOutlastsTheCatchupCeiling(t *testing.T) {
	t.Parallel()
	for name, horizon := range map[string]time.Duration{
		"scheduled_runs": maintenance.ScheduledRunRetention,
	} {
		if horizon <= schedule.DefaultCatchupMax {
			t.Errorf("%s retention (%v) can delete a row a tick could still evaluate (catchup %v)",
				name, horizon, schedule.DefaultCatchupMax)
		}
	}
}

// The operator's retention_days has existed since the conversation ledger
// shipped and nothing ever read it: setting it to 7 got you thirty days of
// conversations, silently, because there was no sweep to honour it.
func TestTheOperatorsConversationHorizonIsHonoured(t *testing.T) {
	t.Parallel()
	doc := companyDoc + `
turn_engine:
  conversation_session:
    retention_days: 3
`
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})

	if got := e.ConversationRetention(); got != 3*24*time.Hour {
		t.Fatalf("conversation retention = %v, want the configured 3 days", got)
	}
	// And an unset one falls back to the ledger's own default rather than
	// to zero, which would mean "delete everything on the next tick".
	plain := newEngine(t, engine.Options{})
	if got := plain.ConversationRetention(); got != 30*24*time.Hour {
		t.Fatalf("the default retention is %v, want 30 days", got)
	}
}

// The delivery horizon and the rate-window sweep are GONE, not forgotten:
// both moved to the coordination store, where the bucket's own age is the
// retention and the broker expires the records. The equivalent guard is
// coordtest's TestTheRetentionsOutlastWhatTheyCover, which holds the bucket
// ages against the cadences they have to outlast.
