package observe_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/tokens"
)

// seat is a fixture seat by its handle, with the derived id its stored rows
// carry ([storeTurnEnd] writes the same one).
func seat(handle string) config.AgentSeat {
	return config.AgentSeat{Handle: handle, AgentID: "id-" + handle}
}

func openStore(t *testing.T) *store.DB {
	t.Helper()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "seed.db"), store.Options{})
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
	if err := observe.Seed(t.Context(), eventfan.Solo("node-1", log), nil, live); err != nil {
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
	if err := observe.Seed(t.Context(), eventfan.Solo("node-1", log), nil, live); err != nil {
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
	if err := observe.Seed(t.Context(), eventfan.Solo("node-1", log), nil, live); err != nil {
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
	err := observe.Seed(t.Context(), halfBroken{feed: boom}, nil, live)
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
	err = observe.Seed(t.Context(), halfBroken{spend: boom}, nil, live)
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

// A SLOW FEED READ DOES NOT SPEND THE WHOLE BUDGET.
//
// One deadline bounds the whole seed, which is what stops a fleet that will not
// answer from holding the listener shut. On the failure that budget exists for
// — a read that is SLOW rather than broken — the reads used to run one after
// another, so the feed spent the budget and the spend read was handed an
// expired context: one slow read cost both, and "each fails on its own" held
// only for an instant failure. They run side by side now, each with the whole
// budget.
func TestASlowFeedReadLeavesTheSpendReadItsOwnTime(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	live := livestate.New()
	history := &slowFeed{}
	err := observe.Seed(ctx, history, nil, live)
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
	if len(live.SpendRecords()) != 1 {
		t.Errorf("records = %+v, want the spend half seeded", live.SpendRecords())
	}
}

// slowFeed blocks its feed read until the context it was given is done, the
// way a cold or contended store answers, and records what the spend read saw.
type slowFeed struct {
	mu          sync.Mutex
	spendAsked  bool
	spendCtxErr error
}

func (h *slowFeed) List(ctx context.Context, _ store.ListQuery) (eventfan.Listing, eventfan.Coverage, error) {
	<-ctx.Done()
	return eventfan.Listing{}, eventfan.Coverage{}, ctx.Err()
}

func (h *slowFeed) PhaseTokens(ctx context.Context, _ store.PhaseTokenQuery) ([]tokens.Record, eventfan.Coverage, error) {
	h.mu.Lock()
	h.spendAsked, h.spendCtxErr = true, ctx.Err()
	h.mu.Unlock()
	if ctx.Err() != nil {
		return nil, eventfan.Coverage{}, ctx.Err()
	}
	return []tokens.Record{{
		EventID: "p1", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		AgentRole: "Lead", Phase: "execute", TotalTokens: 10,
	}}, answered("node-1"), nil
}

func (h *slowFeed) Turns(context.Context, store.TurnQuery) (eventfan.TurnPage, eventfan.Coverage, error) {
	return eventfan.TurnPage{}, answered("node-1"), nil
}

// halfBroken answers one read and fails the other.
type halfBroken struct{ feed, spend error }

func (h halfBroken) List(_ context.Context, _ store.ListQuery) (eventfan.Listing, eventfan.Coverage, error) {
	if h.feed != nil {
		return eventfan.Listing{}, eventfan.Coverage{}, h.feed
	}
	return eventfan.Listing{Rows: []store.EventRecord{{
		ID: "e1", Type: "agent_turn_completed", Category: "agent", Time: time.Now().UTC(),
	}}}, answered("node-1"), nil
}

func (h halfBroken) PhaseTokens(_ context.Context, _ store.PhaseTokenQuery) ([]tokens.Record, eventfan.Coverage, error) {
	if h.spend != nil {
		return nil, eventfan.Coverage{}, h.spend
	}
	return []tokens.Record{{
		EventID: "p1", Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		AgentRole: "Lead", Phase: "execute", TotalTokens: 10,
	}}, answered("node-1"), nil
}

func (h halfBroken) Turns(context.Context, store.TurnQuery) (eventfan.TurnPage, eventfan.Coverage, error) {
	return eventfan.TurnPage{}, answered("node-1"), nil
}

// answered is the coverage of a read the named nodes all answered.
func answered(nodes ...string) eventfan.Coverage {
	c := eventfan.Coverage{Complete: true}
	for _, n := range nodes {
		c.Nodes = append(c.Nodes, eventfan.NodeCoverage{ID: n, Answered: true})
	}
	return c
}

// THE SEED READS NO MORE SPEND THAN THE PROJECTION KEEPS. A day busier than
// the record cap used to be read in full, inside the seed's time budget, and
// cut to the cap on arrival; on the one kind of company the cap exists for, a
// read that ran out of budget seeded no spend at all.
func TestTheSpendSeedStopsAtTheProjectionsRecordCap(t *testing.T) {
	t.Parallel()
	history := &recordingHistory{}
	if err := observe.Seed(t.Context(), history, nil, livestate.New()); err != nil {
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
	mu    sync.Mutex
	feed  store.ListQuery
	spend store.PhaseTokenQuery
	turns []store.TurnQuery
}

func (r *recordingHistory) List(_ context.Context, q store.ListQuery) (eventfan.Listing, eventfan.Coverage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.feed = q
	return eventfan.Listing{}, answered("node-1"), nil
}

func (r *recordingHistory) PhaseTokens(_ context.Context, q store.PhaseTokenQuery) ([]tokens.Record, eventfan.Coverage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spend = q
	return nil, answered("node-1"), nil
}

func (r *recordingHistory) Turns(_ context.Context, q store.TurnQuery) (eventfan.TurnPage, eventfan.Coverage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turns = append(r.turns, q)
	return eventfan.TurnPage{}, answered("node-1"), nil
}

// THE SEED READS EVERY SEAT'S NEWEST TURNS, once per seat and newest first,
// because a page of the company's newest turns says nothing about a seat that
// has been quiet longer than everybody else's recent work — and BY ITS DERIVED
// ID, never its role name, which two seats can share.
//
// Mutation: read by AgentRole, or skip the dedupe, and the reads named differ.
func TestSeedReadsEverySeatsNewestTurns(t *testing.T) {
	t.Parallel()
	history := &recordingHistory{}
	if err := observe.Seed(t.Context(), history,
		[]config.AgentSeat{seat("lead"), seat("coder"), seat("lead"), {}},
		livestate.New()); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	var ids []string
	for _, q := range history.turns {
		ids = append(ids, q.AgentID)
		if q.Limit != observe.SeedTurnsPerSeat || q.Sort != store.TurnSortStarted {
			t.Errorf("turn read %+v, want the newest %d by start", q, observe.SeedTurnsPerSeat)
		}
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"id-coder", "id-lead"}) {
		t.Errorf("turn reads for %v, want one per named seat, by its id", ids)
	}
}

// TWO SEATS OF ONE NAME ARE SEEDED APART.
//
// The seed used to ask for a seat's turns by its role name, so two unit seats
// stamped from one template — which share it — were each answered both seats'
// turns, and each was handed the newest of either as its last turn.
//
// Mutation: ask without the seat's id — all a name two seats share narrows
// to — and both seats carry run-b.
func TestTwoSeatsOfOneNameAreSeededFromTheirOwnTurns(t *testing.T) {
	t.Parallel()
	log := fanNode(t, "node-a")
	now := time.Now().UTC()
	storeTurnEnd(t, log, "eng-a", "run-a", now.Add(-2*time.Hour), "Engineer")
	storeTurnEnd(t, log, "eng-b", "run-b", now.Add(-time.Hour), "Engineer")

	live := livestate.New()
	if err := observe.Seed(t.Context(), eventfan.Solo("node-a", log),
		[]config.AgentSeat{seat("eng-a"), seat("eng-b")}, live); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	for handle, want := range map[string]string{"eng-a": "run-a", "eng-b": "run-b"} {
		if o := live.AgentOverlay(handle); o == nil || o.LastTurn == nil || o.LastTurn.TurnID != want {
			t.Errorf("%s's last turn = %+v, want %s — not its namesake's", handle, o, want)
		}
	}
}

// WHICH NODES THE SEED READ IS KEPT, and a node missing from any read is
// missing from the seed: a feed with one node's rows absent is a short feed,
// and a screen that could not say so would read as a quiet company.
func TestSeedReportsWhichNodesItCovered(t *testing.T) {
	t.Parallel()
	live := livestate.New()
	history := partialFleet{}
	if err := observe.Seed(t.Context(), history, []config.AgentSeat{seat("lead")}, live); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	coverage, seeded := live.SeededFrom()
	if !seeded {
		t.Fatal("the seed recorded no coverage")
	}
	if coverage.Complete || !slices.Equal(coverage.Missing(), []string{"node-2"}) {
		t.Errorf("seeded from %+v, want node-2 named as missing", coverage)
	}

	// EVERY READ ANSWERED BY EVERY NODE is complete — including on a company
	// with no agent seat, which asks nobody for turns and so has no read
	// whose silence could make the seed partial.
	live = livestate.New()
	if err := observe.Seed(t.Context(), halfBroken{}, nil, live); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if coverage, _ := live.SeededFrom(); !coverage.Complete {
		t.Errorf("seeded from %+v with every read answered, want it complete", coverage)
	}

	// A READ THAT FAILED leaves the seed incomplete, whoever answered the
	// rest: what it would have said is missing.
	live = livestate.New()
	_ = observe.Seed(t.Context(), halfBroken{feed: errors.New("boom")}, nil, live)
	if coverage, _ := live.SeededFrom(); coverage.Complete {
		t.Errorf("seeded from %+v after a failed read, want it incomplete", coverage)
	}
}

// partialFleet answers every read, with node-2 silent on the spend.
type partialFleet struct{}

func (partialFleet) List(context.Context, store.ListQuery) (eventfan.Listing, eventfan.Coverage, error) {
	return eventfan.Listing{}, answered("node-1", "node-2"), nil
}

func (partialFleet) PhaseTokens(context.Context, store.PhaseTokenQuery) ([]tokens.Record, eventfan.Coverage, error) {
	return nil, eventfan.Coverage{Nodes: []eventfan.NodeCoverage{
		{ID: "node-1", Answered: true},
		{ID: "node-2", Error: "no answer within the 2s fleet read budget"},
	}}, nil
}

func (partialFleet) Turns(context.Context, store.TurnQuery) (eventfan.TurnPage, eventfan.Coverage, error) {
	return eventfan.TurnPage{}, answered("node-1", "node-2"), nil
}

// A RESTARTED NODE'S FEED, ROLLUP AND SEATS ARE THE FLEET'S.
//
// The event store is per node, so a seed read from this node alone showed it
// only its own share of the company: a phase and a turn a peer published were
// in no screen of a node that had just restarted.
//
// Mutation: seed from eventfan.Solo over node-a's store and every one of
// node-b's records is missing.
func TestARestartedNodesSeedIsTheFleets(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, b := fanNode(t, "node-a"), fanNode(t, "node-b")
	now := time.Now().UTC()
	mine := storePhase(t, a, "Lead", 30, now.Add(-time.Hour))
	theirs := storePhase(t, b, "Coder", 70, now.Add(-30*time.Minute))
	storeTurnEnd(t, b, "coder", "tn-peer", now.Add(-29*time.Minute), "Coder")

	fan := &eventfan.Fleet{
		Self: "node-a", Local: a, Queue: fanQueue(t, broker, a, "node-a"),
		Roster: func(context.Context) ([]string, error) { return []string{"node-a", "node-b"}, nil },
		Budget: 5 * time.Second,
	}
	_ = fanQueue(t, broker, b, "node-b")
	live := livestate.New()
	if err := observe.Seed(t.Context(), fan, []config.AgentSeat{seat("lead"), seat("coder")}, live); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	ids := map[string]bool{}
	for _, row := range live.RecentEvents(0) {
		ids[row.ID] = true
	}
	if !ids[mine] || !ids[theirs] {
		t.Errorf("feed = %v, want both nodes' phases", ids)
	}
	if rollup := tokens.Aggregate(live.SpendRecords(), tokens.Options{}); rollup.Totals.TotalTokens != 100 {
		t.Errorf("rollup = %d tokens, want both nodes' 30 + 70", rollup.Totals.TotalTokens)
	}
	if o := live.AgentOverlay("coder"); o == nil || o.LastTurn == nil || o.LastTurn.TurnID != "tn-peer" {
		t.Errorf("the peer's seat has no last turn: %+v", o)
	}
	if coverage, _ := live.SeededFrom(); !coverage.Complete || len(coverage.Nodes) != 2 {
		t.Errorf("seeded from %+v, want both nodes", coverage)
	}
}

// fanNode is one node's own event store.
func fanNode(t *testing.T, id string) *store.EventLog {
	t.Helper()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), id+".db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	return db.Events()
}

// fanQueue starts a queue client for one node and makes it answer the fleet's
// history questions from its store.
func fanQueue(t *testing.T, broker *memory.Broker, log *store.EventLog, id string) queue.EventQueue {
	t.Helper()
	q := broker.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start %s: %v", id, err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	stop, err := eventfan.Serve(t.Context(), q, id, log)
	if err != nil {
		t.Fatalf("serve %s: %v", id, err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
	return q
}

// storeTurnEnd writes a turn's completion the way the publish listener would,
// for the seat of the handle given — under the derived id [seat] names it by —
// and the role name given.
func storeTurnEnd(t *testing.T, log *store.EventLog, handle, turnID string, at time.Time, role string) {
	t.Helper()
	ev := events.New(types.TurnCompleted{RoleName: role, AgentHandle: handle,
		Agent: seat(handle).AgentID, TurnID: turnID,
		StartedAt: at.Add(-time.Minute), EndedAt: at}, events.TraceContext{})
	ev.Source = role
	ev.Timestamp = at
	rec, ok := observe.Record(ev)
	if !ok {
		t.Fatal("a turn completion did not render as a store row")
	}
	if err := log.Append(t.Context(), rec); err != nil {
		t.Fatalf("append: %v", err)
	}
}

// ONE SEAT'S FAILED TURN READ COSTS THAT SEAT, NOT EVERY SEAT. The turn reads
// are one per seat, and a silent peer or a failed query fails some of them
// while the rest answer; treating the batch as one all-or-nothing read handed
// every seat on the node an empty card over one seat's error.
//
// Mutation: route the turn reads' result through the single-read report, and
// the seat whose read succeeded loses its last turn.
func TestOneSeatsFailedTurnReadKeepsTheOthersTurns(t *testing.T) {
	t.Parallel()
	log := fanNode(t, "node-a")
	now := time.Now().UTC()
	storeTurnEnd(t, log, "coder", "tn-coder", now.Add(-time.Hour), "Coder")
	storeTurnEnd(t, log, "lead", "tn-lead", now.Add(-time.Hour), "Lead")

	live := livestate.New()
	history := failingSeat{FleetHistory: eventfan.Solo("node-a", log), agentID: seat("lead").AgentID}
	err := observe.Seed(t.Context(), history, []config.AgentSeat{seat("lead"), seat("coder")}, live)
	if err == nil {
		t.Fatal("Seed reported no error for the seat whose read failed")
	}
	if o := live.AgentOverlay("coder"); o == nil || o.LastTurn == nil || o.LastTurn.TurnID != "tn-coder" {
		t.Errorf("the seat whose read succeeded lost its last turn: %+v", o)
	}
	if o := live.AgentOverlay("lead"); o != nil && o.LastTurn != nil {
		t.Errorf("the seat whose read failed has a last turn %+v from nowhere", o.LastTurn)
	}
	coverage, seeded := live.SeededFrom()
	if !seeded || coverage.Complete {
		t.Errorf("seeded from %+v (recorded %v), want an incomplete seed", coverage, seeded)
	}
	if !slices.ContainsFunc(coverage.Nodes, func(n eventfan.NodeCoverage) bool {
		return n.ID == "node-a" && n.Answered
	}) {
		t.Errorf("seeded from %+v, want node-a recorded as answering the reads it did", coverage)
	}
}

// failingSeat is a fleet whose turn read for one seat fails.
type failingSeat struct {
	observe.FleetHistory
	agentID string
}

func (f failingSeat) Turns(ctx context.Context, q store.TurnQuery) (eventfan.TurnPage, eventfan.Coverage, error) {
	if q.AgentID == f.agentID {
		return eventfan.TurnPage{}, eventfan.Coverage{}, errors.New("peer went silent")
	}
	return f.FleetHistory.Turns(ctx, q)
}

// A SEAT'S ROW NAMES THE SEAT, LIVE OR SEEDED, BY THE STORE'S OWN RULE.
//
// The event log narrows to one seat (`seat=`): the rows it pages in are
// narrowed by the store's promoted `agent_id`, and the rows the socket pushes
// have to be narrowed by the same value or the two halves of one list disagree
// about which events a seat published. So the live envelope and the row a
// restarted process seeds from the store both carry the seat, filled by
// `store.ExtractTags` — one rule, read in both places.
func TestALiveRowAndASeededRowNameTheSameSeat(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	log := db.Events()
	ev := events.New(types.AgentPhaseCompleted{
		RoleName: "Lead", Agent: "a-7", TurnID: "tn-1", Phase: types.PhaseExecute,
		Model: "claude-sonnet-5",
	}, events.TraceContext{})
	ev.Source = "Lead"
	ev.Timestamp = time.Now().UTC().Add(-time.Minute)

	env, ok := observe.Envelope(ev)
	if !ok {
		t.Fatal("a phase completion did not render as a live envelope")
	}
	if env.AgentID != "a-7" {
		t.Errorf("the live envelope names seat %q, want a-7", env.AgentID)
	}
	live := livestate.New()
	live.Apply(&env)
	pushed := live.RecentEvents(0)
	if len(pushed) != 1 || pushed[0].AgentID != "a-7" {
		t.Fatalf("the live feed row = %+v, want one naming a-7", pushed)
	}

	rec, ok := observe.Record(ev)
	if !ok {
		t.Fatal("a phase completion did not render as a store row")
	}
	if err := log.Append(t.Context(), rec); err != nil {
		t.Fatalf("append: %v", err)
	}
	seeded := livestate.New()
	if err := observe.Seed(t.Context(), eventfan.Solo("node-1", log), nil, seeded); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	rows := seeded.RecentEvents(0)
	if len(rows) != 1 || rows[0].AgentID != pushed[0].AgentID {
		t.Errorf("the seeded row = %+v, want it to name the seat the live one did", rows)
	}
}

// A SEAT'S SPEND IS ITS OWN ROW, LIVE OR SEEDED, keyed on the handle every
// record names it by.
//
// Two seats may share a name — unique seat names are an admission rule, so a
// stored revision, or one a newer node admitted, runs with a collision — and
// the live spend window filed them under that name: one row, both seats' spend.
// Both producers of that window hand the fold the handle now: the projection
// off the envelope, and the store off the column a restarted process seeds
// from. Each seat here spends through a phase and through the auxiliary model.
//
// Mutation: drop `agent_handle` from the store's promoted tags, and the seeded
// window is one row; drop the projection's read of it, and the live one is.
func TestASeatsSpendIsItsOwnRowLiveOrSeeded(t *testing.T) {
	t.Parallel()
	log := openStore(t).Events()
	at := time.Now().UTC().Add(-time.Minute)
	var evs []*events.Event
	for i, handle := range []string{"eng-a", "eng-b"} {
		spent := 100 * (i + 1)
		evs = append(evs,
			events.New(types.AgentPhaseCompleted{
				RoleName: "Engineer", AgentHandle: handle, Agent: "id-" + handle,
				TurnID: "tn-" + handle, Phase: types.PhaseExecute, Model: "claude-sonnet-5",
				TotalTokens: spent, InputTokens: spent,
			}, events.TraceContext{}),
			events.New(types.AuxiliarySpend{
				RoleName: "Engineer", AgentHandle: handle, Agent: "id-" + handle,
				TurnID: "tn-" + handle, Stage: types.AuxStageTurn, Purpose: types.AuxMemoryFilter,
				Model: "claude-haiku-4-5", Calls: 1, TotalTokens: i + 1, InputTokens: i + 1,
			}, events.TraceContext{}))
	}
	live := livestate.New()
	for _, ev := range evs {
		ev.Source, ev.Timestamp = "Engineer", at
		env, ok := observe.Envelope(ev)
		if !ok {
			t.Fatalf("%s did not render as a live envelope", ev.Type)
		}
		live.Apply(&env)
		rec, ok := observe.Record(ev)
		if !ok {
			t.Fatalf("%s did not render as a store row", ev.Type)
		}
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	seeded := livestate.New()
	if err := observe.Seed(t.Context(), eventfan.Solo("node-1", log), nil, seeded); err != nil {
		t.Fatalf("Seed: %v", err)
	}

	for window, state := range map[string]*livestate.LiveState{"live": live, "seeded": seeded} {
		rollup := tokens.Aggregate(state.SpendRecords(), tokens.Options{})
		seats := map[string]tokens.AgentRow{}
		for _, a := range rollup.ByAgent {
			seats[a.Handle] = a
		}
		if len(rollup.ByAgent) != 2 || seats["eng-a"].TotalTokens != 101 ||
			seats["eng-b"].TotalTokens != 202 {
			t.Errorf("%s by_agent = %+v, want eng-a's 101 and eng-b's 202 apart", window, rollup.ByAgent)
		}
		turns := map[string]string{}
		for _, turn := range rollup.ByTurn {
			turns[turn.TurnID] = turn.Handle
		}
		if turns["tn-eng-a"] != "eng-a" || turns["tn-eng-b"] != "eng-b" {
			t.Errorf("%s turns link to %v, want each to the seat that ran it", window, turns)
		}
	}
}

// AN A2A ROW NAMES ITS CHANNEL, LIVE OR SEEDED, BY THE STORE'S OWN RULE.
//
// The event log narrows to one agent-to-agent channel (`channel=`) — the link
// a channel's page carries — and the rows it pages in are narrowed by the
// store's promoted `channel_id`. The rows the socket pushes carried no channel
// at all, so under that filter the log could show no live row of the very
// conversation it was opened on. Both halves carry it now, filled by
// `store.ExtractTags` in both places.
//
// Mutation: leave ChannelID off the envelope, and the live row names none.
func TestALiveRowAndASeededRowNameTheSameChannel(t *testing.T) {
	t.Parallel()
	log := openStore(t).Events()
	ev := events.New(types.A2AMessageSent{
		ChannelID: "ch-9", Sender: "lead", Recipient: "eng", MessageID: "m-1", Content: "status?",
	}, events.TraceContext{})
	ev.Source = "Lead"
	ev.Timestamp = time.Now().UTC().Add(-time.Minute)

	env, ok := observe.Envelope(ev)
	if !ok {
		t.Fatal("an A2A message did not render as a live envelope")
	}
	live := livestate.New()
	live.Apply(&env)
	pushed := live.RecentEvents(0)
	if len(pushed) != 1 || pushed[0].ChannelID != "ch-9" {
		t.Fatalf("the live feed row = %+v, want one naming ch-9", pushed)
	}

	rec, ok := observe.Record(ev)
	if !ok {
		t.Fatal("an A2A message did not render as a store row")
	}
	if err := log.Append(t.Context(), rec); err != nil {
		t.Fatalf("append: %v", err)
	}
	seeded := livestate.New()
	if err := observe.Seed(t.Context(), eventfan.Solo("node-1", log), nil, seeded); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	rows := seeded.RecentEvents(0)
	if len(rows) != 1 || rows[0].ChannelID != "ch-9" {
		t.Errorf("the seeded row = %+v, want it to name ch-9 as the live one did", rows)
	}
}
