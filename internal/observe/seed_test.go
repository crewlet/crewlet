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

// spendRows is the records half of SpendRecords, for the cases here that are
// about WHAT the seed retained rather than about the window that retention
// leaves the rollup covering.
func spendRows(s *livestate.LiveState) []tokens.Record {
	rows, _ := s.SpendRecords()
	return rows
}

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
	rollup := tokens.Aggregate(spendRows(live), tokens.Options{})
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
	if len(spendRows(live)) != 1 {
		t.Errorf("records = %+v, want the half that could be read", spendRows(live))
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
	if len(spendRows(live)) != 0 {
		t.Error("the failing half seeded records anyway")
	}
}

// A SLOW FEED READ DOES NOT SPEND THE WHOLE BUDGET.
//
// The two reads are sequential on the caller's one deadline, which is what
// bounds the seed so a store that will not answer cannot hold the listener
// shut. On the failure that budget exists for — a store that is SLOW rather
// than broken — the feed read used to spend all of it and the spend read was
// handed an expired context, so one slow half cost both and the doc's promise
// that each fails on its own held only for an instant failure.
func TestASlowFeedReadLeavesTheSpendReadItsOwnTime(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	live := livestate.New()
	history := &slowFeed{}
	err := observe.Seed(ctx, history, live)
	if err == nil {
		t.Fatal("the feed read outran its own half of the budget silently")
	}
	if !history.spendAsked {
		t.Fatal("the spend read was never made: the feed spent the whole budget")
	}
	if history.spendCtxErr != nil {
		t.Errorf("the spend read was handed an expired context (%v), so it "+
			"returned without touching the store", history.spendCtxErr)
	}
	if len(spendRows(live)) != 1 {
		t.Errorf("records = %+v, want the spend half seeded", spendRows(live))
	}
}

// slowFeed blocks its feed read until the context it was given is done, the
// way a cold or contended store answers, and records what the spend read saw.
type slowFeed struct {
	spendAsked  bool
	spendCtxErr error
}

func (h *slowFeed) List(ctx context.Context, _ store.ListQuery) ([]store.EventRecord, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (h *slowFeed) PhaseTokenTail(ctx context.Context, _ store.PhaseTokenQuery, _ int) ([]tokens.Record, bool, error) {
	h.spendAsked, h.spendCtxErr = true, ctx.Err()
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	return []tokens.Record{{
		EventID: "p1", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		AgentRole: "Lead", Phase: "execute", TotalTokens: 10,
	}}, false, nil
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

func (h halfBroken) PhaseTokenTail(_ context.Context, _ store.PhaseTokenQuery, _ int) ([]tokens.Record, bool, error) {
	if h.spend != nil {
		return nil, false, h.spend
	}
	return []tokens.Record{{
		EventID: "p1", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		AgentRole: "Lead", Phase: "execute", TotalTokens: 10,
	}}, false, nil
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
	if history.spendLimit != livestate.SpendRecordLimit {
		t.Errorf("spend read limit = %d, want the projection's record cap %d",
			history.spendLimit, livestate.SpendRecordLimit)
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

// recordingHistory remembers what it was asked, and answers the spend read
// with whatever it was given.
type recordingHistory struct {
	feed       store.ListQuery
	spend      store.PhaseTokenQuery
	spendLimit int

	records []tokens.Record
	more    bool
}

func (r *recordingHistory) List(_ context.Context, q store.ListQuery) ([]store.EventRecord, error) {
	r.feed = q
	return nil, nil
}

func (r *recordingHistory) PhaseTokenTail(_ context.Context, q store.PhaseTokenQuery, limit int) ([]tokens.Record, bool, error) {
	r.spend, r.spendLimit = q, limit
	return r.records, r.more, nil
}

// THE STORE'S "THERE IS MORE" REACHES THE PROJECTION. The tail read answers
// whether the window held records past the cap, and that answer is the only
// thing that can head the rollup with the span the kept records cover: a page
// of records says nothing about what was left behind it. A seed that dropped
// the flag would head a rollup over the newest records with the whole window,
// which on a money figure is a wrong total nobody can see.
func TestTheSeedCarriesTheStoresTruncationToTheProjection(t *testing.T) {
	t.Parallel()
	oldest := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	records := []tokens.Record{
		{EventID: "p2", Timestamp: oldest.Add(time.Minute).Format(time.RFC3339Nano),
			AgentRole: "Lead", Phase: "execute", TotalTokens: 10},
		{EventID: "p1", Timestamp: oldest.Format(time.RFC3339Nano),
			AgentRole: "Lead", Phase: "execute", TotalTokens: 10},
	}
	for _, tc := range []struct {
		name string
		more bool
		want time.Time
	}{
		// Nothing was left behind: the kept records are the whole window,
		// so the window's own heading is the true one.
		{"the window was read whole", false, time.Time{}},
		// Older records were left in the store: the rollup covers from the
		// earliest record kept, and says so.
		{"older records were left behind", true, oldest},
	} {
		live := livestate.New()
		history := &recordingHistory{records: records, more: tc.more}
		if err := observe.Seed(t.Context(), history, live); err != nil {
			t.Fatalf("%s: Seed: %v", tc.name, err)
		}
		if _, covered := live.SpendRecords(); !covered.Equal(tc.want) {
			t.Errorf("%s: the rollup covers from %v, want %v (zero is the whole window)",
				tc.name, covered, tc.want)
		}
	}
}
