package engine_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/store"
)

// THE assertion whose absence was the bug. Every one of these tables ships a
// Purge and an index for it, every migration says the rows are swept, and in
// the Python engine nothing ever called any of them — so all of them grew
// for the life of the deployment. A store gaining a Purge with no entry here
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
	// doing its job — which is exactly how the Python engine shipped a
	// full set of Purge methods that nothing ever called.
	if !w.Running() {
		t.Fatal("the sweep was built but never started")
	}
	got := w.Jobs()
	slices.Sort(got)
	want := []string{
		"a2a_channels",
		"a2a_channels_idle",
		"chat_thread_follows",
		"config_apply_status",
		"conversation_sessions",
		"events",
		"rate_limits",
		"scheduled_runs",
		"turn_completions",
		"webhook_deliveries",
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
		"webhook_deliveries":    maintenance.DeliveryRetention,
		"rate_limits":           maintenance.RateLimitRetention,
		"scheduled_runs":        maintenance.ScheduledRunRetention,
		"turn_completions":      maintenance.CompletionRetention,
		"conversation_sessions": maintenance.ConversationRetention,
		"a2a_channels":          maintenance.ChannelRetention,
		"a2a_channels_idle":     maintenance.ChannelIdleTimeout,
		"chat_thread_follows":   maintenance.FollowRetention,
		"config_apply_status":   maintenance.ApplyStatusRetention,
	} {
		if horizon <= maintenance.Interval {
			t.Errorf("%s retention (%v) is not longer than the %v tick",
				table, horizon, maintenance.Interval)
		}
	}
}

// The completion ledger answers "has this trigger already been worked?", so
// deleting a row a tick could still evaluate lets that fire run TWICE. Its
// floor is the catchup ceiling, not a number somebody liked.
func TestTheCompletionHorizonOutlastsTheCatchupCeiling(t *testing.T) {
	t.Parallel()
	for name, horizon := range map[string]time.Duration{
		"turn_completions": maintenance.CompletionRetention,
		"scheduled_runs":   maintenance.ScheduledRunRetention,
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

// The delivery horizon is derived from the claim TTL rather than written as
// a number, so raising one raises the other and a sweep can never delete a
// row a claim would still consult.
func TestTheDeliveryHorizonTracksTheClaimTTL(t *testing.T) {
	t.Parallel()
	if maintenance.DeliveryRetention <= store.DeliveryTTL {
		t.Fatalf("the sweep (%v) can reach rows a claim still consults (%v)",
			maintenance.DeliveryRetention, store.DeliveryTTL)
	}
}
