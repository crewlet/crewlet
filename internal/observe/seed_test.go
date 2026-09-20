package observe_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

func openStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "seed.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// storePhase writes one completed phase the way the publish listener would,
// which is what puts its spend in the promoted columns.
func storePhase(t *testing.T, log *store.EventLog, role string, tokenCount int, at time.Time) string {
	t.Helper()
	ev := events.New(types.AgentPhaseCompleted{
		RoleName: role, Agent: "a-1", TurnID: "tn-1", Phase: types.PhaseExecute,
		Model: "claude-sonnet-5", TotalTokens: tokenCount,
		InputTokens: tokenCount / 2, OutputTokens: tokenCount / 2,
	}, events.TraceContext{})
	ev.Source = role
	ev.Timestamp = at
	rec, ok := observe.Record(ev)
	if !ok {
		t.Fatal("a phase completion did not render as a store row")
	}
	if err := log.Append(t.Context(), rec); err != nil {
		t.Fatalf("append: %v", err)
	}
	return rec.ID
}

// A RESTARTED PROCESS STARTS FROM WHAT IT HAS ALREADY RECORDED.
//
// The feed, the spend rollup and the per-agent rows folded from it all came
// up empty in every process until this read existed, so a restart showed an
// operator a company that had apparently done nothing.
func TestSeedingReadsTheFeedAndTheSpendWindowFromTheStore(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	now := time.Now().UTC()
	recent := storePhase(t, log, "Lead", 30, now.Add(-time.Hour))
	// Inside the store's read floor and outside the live spend window: it
	// belongs in the feed and not in the rollup.
	old := storePhase(t, log, "Lead", 900, now.Add(-72*time.Hour))

	live := livestate.New()
	if err := observe.Seed(t.Context(), log, live); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	ids := map[string]bool{}
	for _, row := range live.RecentEvents(0) {
		ids[row.ID] = true
		if row.Type != "agent_phase_completed" || row.Category == "" {
			t.Errorf("seeded row = %+v, want the stored type and category", row)
		}
		if row.Topic != "crewlet.events.agent_phase_completed" {
			t.Errorf("seeded row topic = %q, want the subject a live row carries", row.Topic)
		}
	}
	if !ids[recent] || !ids[old] {
		t.Errorf("feed = %v, want both stored events", ids)
	}
	rollup := tokens.Aggregate(live.SpendRecords(), tokens.Options{})
	if rollup.Totals.TotalTokens != 30 || rollup.Totals.Calls != 1 {
		t.Errorf("rollup = %d tokens over %d calls, want only the phase inside the "+
			"live window", rollup.Totals.TotalTokens, rollup.Totals.Calls)
	}
	if len(rollup.ByAgent) != 1 || rollup.ByAgent[0].Role != "Lead" {
		t.Errorf("by_agent = %+v, want the seat the stored phase named", rollup.ByAgent)
	}
}

// A SEEDED ROW KEEPS ITS FAILURE MARK. The feed listing never selects the
// payload, so the mark a live row derives from it has to come back through the
// tag the writer stamped; a seed that dropped it would show every failure from
// before the restart as a success, on the first screen an operator opens after
// one.
func TestASeededRowKeepsItsFailureMark(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	failed := events.New(types.AgentPhaseCompleted{
		RoleName: "Lead", Agent: "a-1", TurnID: "tn-1", Phase: types.PhaseExecute,
		Model: "claude-sonnet-5", Failed: true,
	}, events.TraceContext{})
	failed.Timestamp = time.Now().UTC().Add(-time.Minute)
	rec, ok := observe.Record(failed)
	if !ok {
		t.Fatal("a failed phase did not render as a store row")
	}
	if err := log.Append(t.Context(), rec); err != nil {
		t.Fatalf("append: %v", err)
	}
	fine := storePhase(t, log, "Lead", 10, time.Now().UTC().Add(-2*time.Minute))

	live := livestate.New()
	if err := observe.Seed(t.Context(), log, live); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	marks := map[string]bool{}
	for _, row := range live.RecentEvents(0) {
		marks[row.ID] = row.Failed
	}
	if failedMark, seeded := marks[rec.ID]; !seeded || !failedMark {
		t.Errorf("the failed phase seeded as (listed %v, failed %v), want a failed row", seeded, failedMark)
	}
	if marks[fine] {
		t.Error("an ordinary phase seeded as failed")
	}
}

// A WEBHOOK DELIVERY'S SEEDED ROW READS LIKE ITS LIVE ONE. The receiver
// writes the row itself and ingests its own envelope, so the two halves of one
// feed are built in different places and must not name the delivery
// differently.
func TestASeededWebhookRowCarriesTheTopicTheReceiverPushes(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	if err := log.Append(t.Context(), store.EventRecord{
		ID: "wh-1", Type: "pull_request", Source: "github", Time: time.Now().UTC(),
		Category: events.WebhookCategory, Summary: "a delivery", Actor: "github",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	live := livestate.New()
	if err := observe.Seed(t.Context(), log, live); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	rows := live.RecentEvents(0)
	if len(rows) != 1 {
		t.Fatalf("feed = %+v, want the one delivery", rows)
	}
	if want := livestate.WebhookTopic("github"); rows[0].Topic != want {
		t.Errorf("topic = %q, want %q", rows[0].Topic, want)
	}
}

// EACH HALF FAILS ON ITS OWN, and the failure is REPORTED rather than
// swallowed: a feed that starts at this process's boot looks exactly like a
// company that has done nothing, and nothing else says which it is.
func TestAnUnreadableHalfIsReportedAndDoesNotCostTheOther(t *testing.T) {
	t.Parallel()
	boom := errors.New("the store could not be read")
	live := livestate.New()
	err := observe.Seed(t.Context(), halfBroken{feed: boom}, live)
	if err == nil {
		t.Fatal("an unreadable feed was seeded silently")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the store's own cause", err)
	}
	if len(live.SpendRecords()) != 1 {
		t.Errorf("records = %+v, want the half that could be read", live.SpendRecords())
	}
	if len(live.RecentEvents(0)) != 0 {
		t.Error("the failing half seeded rows anyway")
	}

	// AND THE MIRROR, which is the half the seed exists for: a spend read
	// that fails must still leave the feed seeded. Without this case a
	// change that let either error short-circuit the other would pass.
	live = livestate.New()
	err = observe.Seed(t.Context(), halfBroken{spend: boom}, live)
	if err == nil {
		t.Fatal("an unreadable spend window was seeded silently")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want the store's own cause", err)
	}
	if len(live.RecentEvents(0)) != 1 {
		t.Errorf("feed = %+v, want the half that could be read", live.RecentEvents(0))
	}
	if len(live.SpendRecords()) != 0 {
		t.Error("the failing half seeded records anyway")
	}
}

// halfBroken answers one read and fails the other.
type halfBroken struct{ feed, spend error }

func (h halfBroken) List(_ context.Context, _ store.ListQuery) ([]store.EventRecord, error) {
	if h.feed != nil {
		return nil, h.feed
	}
	return []store.EventRecord{{
		ID: "e1", Type: "agent_turn_completed", Category: "agent", Time: time.Now().UTC(),
	}}, nil
}

func (h halfBroken) PhaseTokens(_ context.Context, _ store.PhaseTokenQuery) ([]tokens.Record, error) {
	if h.spend != nil {
		return nil, h.spend
	}
	return []tokens.Record{{
		EventID: "p1", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		AgentRole: "Lead", Phase: "execute", TotalTokens: 10,
	}}, nil
}

// THE SEED READS NO MORE SPEND THAN THE PROJECTION KEEPS. A day busier than
// the record cap used to be read in full, inside the seed's time budget, and
// cut to the cap on arrival; on the one kind of company the cap exists for, a
// read that ran out of budget seeded no spend at all.
func TestTheSpendSeedStopsAtTheProjectionsRecordCap(t *testing.T) {
	t.Parallel()
	history := &recordingHistory{}
	if err := observe.Seed(t.Context(), history, livestate.New()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if history.spend.Limit != livestate.SpendRecordLimit {
		t.Errorf("spend read limit = %d, want the projection's record cap %d",
			history.spend.Limit, livestate.SpendRecordLimit)
	}
	// AS AN INSTANT, so the read covers the projection's window whatever
	// that window is. Asked for in whole days it would agree only while
	// the window is a whole number of them.
	if history.spend.SinceDays != 0 {
		t.Errorf("spend read named %d days; the window is named as an instant, "+
			"and a day count beside it is a second answer the store may prefer",
			history.spend.SinceDays)
	}
	if want := time.Now().UTC().Add(-livestate.LiveSpendWindow); history.spend.Since.Sub(want).Abs() > time.Minute {
		t.Errorf("spend read from %v, want the live window's start around %v",
			history.spend.Since, want)
	}
	if history.feed.Limit != livestate.EventFeedLimit {
		t.Errorf("feed read limit = %d, want the ring's %d",
			history.feed.Limit, livestate.EventFeedLimit)
	}
}

// recordingHistory answers nothing and remembers what it was asked.
type recordingHistory struct {
	feed  store.ListQuery
	spend store.PhaseTokenQuery
}

func (r *recordingHistory) List(_ context.Context, q store.ListQuery) ([]store.EventRecord, error) {
	r.feed = q
	return nil, nil
}

func (r *recordingHistory) PhaseTokens(_ context.Context, q store.PhaseTokenQuery) ([]tokens.Record, error) {
	r.spend = q
	return nil, nil
}
