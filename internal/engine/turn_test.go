package engine_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/workkey"
)

var clock = time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

func ev(kind string) *events.Event {
	return &events.Event{ID: uuid.New(), Type: kind}
}

func inThread(kind, conversation string) *events.Event {
	e := ev(kind)
	e.Payload = map[string]any{"conversation_key": conversation}
	return e
}

// recorder captures what the dispatcher asked of the turn engine.
type recorder struct {
	reqs     []engine.Request
	keys     []string
	result   turn.Result
	err      error
	parked   [][]*events.Event
	paused   []string
	deferred []string
}

func (r *recorder) run(ctx context.Context, req engine.Request) (turn.Result, error) {
	r.reqs = append(r.reqs, req)
	// The work key must be on the CONTEXT by the time the turn runs: the
	// writers that must not duplicate under it sit frames below this one.
	r.keys = append(r.keys, workkey.From(ctx))
	return r.result, r.err
}

func dispatcher(t *testing.T, r *recorder) *engine.Dispatcher {
	t.Helper()
	return &engine.Dispatcher{
		Ledgered: func(kind string) bool { return kind == "notification" },
		Turn:     r.run,
		Park: func(_ context.Context, _ string, evs []*events.Event) error {
			r.parked = append(r.parked, evs)
			return nil
		},
		Pause: func(_ context.Context, handle, _ string) error {
			r.paused = append(r.paused, handle)
			return nil
		},
		NoteDeferred: func(handle string) { r.deferred = append(r.deferred, handle) },
		Now:          func() time.Time { return clock },
	}
}

func TestAHealthyPartitionReachesTheTurnEngine(t *testing.T) {
	t.Parallel()
	// The control. Without it every guard assertion below passes for a
	// dispatcher that refuses everything.
	r := &recorder{result: turn.Result{Decision: phase.Done, Artifact: "posted"}}
	d := dispatcher(t, r)
	got := d.Dispatch(context.Background(), "ceo", []*events.Event{ev("notification")})

	if got.Outcome != queue.OutcomeAck {
		t.Errorf("outcome = %v, want an ack", got.Outcome)
	}
	if len(r.reqs) != 1 {
		t.Fatalf("the turn engine ran %d times", len(r.reqs))
	}
	if r.reqs[0].Handle != "ceo" || len(r.reqs[0].Events) != 1 {
		t.Errorf("request = %+v", r.reqs[0])
	}
	if r.reqs[0].WorkKey == "" {
		t.Error("the dispatch carried no work key")
	}
	if r.keys[0] != r.reqs[0].WorkKey {
		t.Errorf("the context key %q does not match the request's %q", r.keys[0], r.reqs[0].WorkKey)
	}
}

func TestAGuardStopsTheTurnBeforeItStarts(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		conds   inbox.Conditions
		outcome queue.Outcome
		parked  bool
		paused  bool
		noted   bool
	}{
		"not owned": {inbox.Conditions{}, queue.OutcomeDefer, false, false, true},
		"no engine": {
			inbox.Conditions{Owned: true}, queue.OutcomeAck, true, true, false},
		"sandbox": {
			inbox.Conditions{Owned: true, TurnEngineReady: true, AwaitingSandbox: true},
			queue.OutcomeAck, true, false, false},
		"shedding": {
			inbox.Conditions{Owned: true, TurnEngineReady: true},
			queue.OutcomeDefer, false, false, true},
	} {
		r := &recorder{}
		d := dispatcher(t, r)
		d.Conditions = func(string) inbox.Conditions { return tc.conds }
		got := d.Dispatch(context.Background(), "ceo", []*events.Event{ev("notification")})

		if got.Outcome != tc.outcome {
			t.Errorf("%s: outcome = %v, want %v", name, got.Outcome, tc.outcome)
		}
		if len(r.reqs) != 0 {
			t.Errorf("%s: the turn ran despite the guard", name)
		}
		if parked := len(r.parked) > 0; parked != tc.parked {
			t.Errorf("%s: parked = %v, want %v", name, parked, tc.parked)
		}
		if paused := len(r.paused) > 0; paused != tc.paused {
			t.Errorf("%s: paused = %v, want %v", name, paused, tc.paused)
		}
		if noted := len(r.deferred) > 0; noted != tc.noted {
			t.Errorf("%s: noted-deferred = %v, want %v", name, noted, tc.noted)
		}
	}
}

func TestAParkIsNeverAckedUntilItsRequeueLands(t *testing.T) {
	t.Parallel()
	// Acking a park whose requeue failed drops the work entirely — the
	// broker believes it was handled and nothing holds it.
	r := &recorder{}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true, AwaitingSandbox: true}
	}
	d.Park = func(context.Context, string, []*events.Event) error {
		return errors.New("broker unreachable")
	}
	got := d.Dispatch(context.Background(), "ceo", []*events.Event{ev("notification")})
	if got.Outcome != queue.OutcomeNak {
		t.Errorf("outcome = %v, want a NAK", got.Outcome)
	}
}

func TestAFailedPauseDoesNotPark(t *testing.T) {
	t.Parallel()
	// The pause is what stops the requeued copies looping back at whatever
	// rate the broker will serve. Parking without it is worse than doing
	// nothing.
	r := &recorder{}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions { return inbox.Conditions{Owned: true} }
	d.Pause = func(context.Context, string, string) error { return errors.New("no") }
	got := d.Dispatch(context.Background(), "ceo", []*events.Event{ev("notification")})
	if got.Outcome != queue.OutcomeNak {
		t.Errorf("outcome = %v, want a NAK", got.Outcome)
	}
	if len(r.parked) != 0 {
		t.Error("the partition was parked despite the pause failing")
	}
}

func TestNoParkPathNAKsRatherThanDropping(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	d := dispatcher(t, r)
	d.Park = nil
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true, AwaitingSandbox: true}
	}
	if got := d.Dispatch(context.Background(), "ceo", []*events.Event{ev("notification")}); got.Outcome != queue.OutcomeNak {
		t.Errorf("outcome = %v, want a NAK", got.Outcome)
	}
}

func TestAlreadyWorkedTriggersDropOutBeforeCoalescing(t *testing.T) {
	t.Parallel()
	// A redelivery that overlaps a previous one PARTIALLY — (A, B) after
	// (A, B, C) was worked — must skip A and B and run C. Reading the
	// ledger after coalescing would merge them all into one digest and run
	// the lot again.
	completions := ledgerstore.NewMemoryCompletions()
	a, b, c := ev("notification"), ev("notification"), ev("notification")
	ctx := context.Background()
	for _, e := range []*events.Event{a, b} {
		if err := completions.Record(ctx, "ceo", workkey.Derive([]string{e.ID.String()}), "", clock); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Completions = completions
	if got := d.Dispatch(ctx, "ceo", []*events.Event{a, b, c}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}
	if len(r.reqs) != 1 {
		t.Fatalf("the turn ran %d times", len(r.reqs))
	}
	if ids := eventIDs(r.reqs[0].Events); !slices.Equal(ids, []string{c.ID.String()}) {
		t.Errorf("the turn received %v, want only the unworked event", ids)
	}
	// One event left, so nothing to merge.
	if r.reqs[0].Coalesce {
		t.Error("a single surviving event was routed for coalescing")
	}
}

func TestTheLedgerReadSeesTheDEDUPEDPartition(t *testing.T) {
	t.Parallel()
	// The guards run first, and the first of them is the same-id dedupe.
	// Reading the ledger against the RAW partition instead returns a
	// filtered copy of the raw list — so the duplicates survive to the turn
	// and the seat answers the same message twice in one dispatch.
	//
	// Found by mutation: with no duplicate in any ledger-path test, reading
	// the wrong list changed nothing.
	completions := ledgerstore.NewMemoryCompletions()
	a, b := ev("notification"), ev("notification")
	dupA := *a
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Completions = completions

	got := d.Dispatch(context.Background(), "ceo", []*events.Event{a, &dupA, b})
	if got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}
	if len(r.reqs) != 1 {
		t.Fatalf("the turn ran %d times", len(r.reqs))
	}
	if ids := eventIDs(r.reqs[0].Events); !slices.Equal(ids, []string{a.ID.String(), b.ID.String()}) {
		t.Errorf("the turn received %v, want the two distinct events", ids)
	}
}

func TestAFullyWorkedPartitionAcksWithoutRunning(t *testing.T) {
	t.Parallel()
	completions := ledgerstore.NewMemoryCompletions()
	a := ev("notification")
	ctx := context.Background()
	if err := completions.Record(ctx, "ceo", workkey.Derive([]string{a.ID.String()}), "", clock); err != nil {
		t.Fatalf("Record: %v", err)
	}
	r := &recorder{}
	d := dispatcher(t, r)
	d.Completions = completions
	if got := d.Dispatch(ctx, "ceo", []*events.Event{a}); got.Outcome != queue.OutcomeAck {
		t.Errorf("outcome = %v, want an ack", got.Outcome)
	}
	if len(r.reqs) != 0 {
		t.Error("a fully-worked partition still ran a turn")
	}
}

func TestEachConstituentIsRecordedUnderItsOwnKey(t *testing.T) {
	t.Parallel()
	// The partition's own key covers the set that ran TOGETHER. A later
	// redelivery of a subset keys differently and would match nothing, so
	// the subset would run again — which is the partial-overlap bug, one
	// step later.
	completions := ledgerstore.NewMemoryCompletions()
	a, b := ev("notification"), ev("notification")
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Completions = completions
	ctx := context.Background()
	if got := d.Dispatch(ctx, "ceo", []*events.Event{a, b}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}

	worked := completions.Worked(ctx, "ceo", []string{
		workkey.Derive([]string{a.ID.String()}),
		workkey.Derive([]string{b.ID.String()}),
	})
	if len(worked) != 2 {
		t.Errorf("recorded %d constituents, want both", len(worked))
	}
	// And the subset redelivery is now a no-op.
	r.reqs = nil
	if got := d.Dispatch(ctx, "ceo", []*events.Event{a}); got.Outcome != queue.OutcomeAck {
		t.Errorf("outcome = %v", got.Outcome)
	}
	if len(r.reqs) != 0 {
		t.Error("a subset redelivery ran again")
	}
}

func TestAFailedTurnIsStillRecordedAsWorked(t *testing.T) {
	t.Parallel()
	// The ledger answers "has this trigger been worked", not "did the work
	// succeed". Re-running a failing turn on every redelivery is how one
	// bad trigger becomes an infinite loop.
	completions := ledgerstore.NewMemoryCompletions()
	a := ev("notification")
	r := &recorder{result: turn.Result{Decision: phase.Failed}}
	d := dispatcher(t, r)
	d.Completions = completions
	ctx := context.Background()
	d.Dispatch(ctx, "ceo", []*events.Event{a})

	if !completions.Worked(ctx, "ceo", []string{workkey.Derive([]string{a.ID.String()})})[workkey.Derive([]string{a.ID.String()})] {
		t.Error("a failed turn was not recorded, so its trigger will run forever")
	}
}

func TestABrokenPhaseNAKsAndRecordsNothing(t *testing.T) {
	t.Parallel()
	// A broken phase is not a failed turn. Nothing was recorded, so the
	// redelivery runs cleanly.
	completions := ledgerstore.NewMemoryCompletions()
	a := ev("notification")
	r := &recorder{err: errors.New("provider unreachable")}
	d := dispatcher(t, r)
	d.Completions = completions
	ctx := context.Background()

	got := d.Dispatch(ctx, "ceo", []*events.Event{a})
	if got.Outcome != queue.OutcomeNak {
		t.Errorf("outcome = %v, want a NAK", got.Outcome)
	}
	if len(completions.Worked(ctx, "ceo", []string{workkey.Derive([]string{a.ID.String()})})) != 0 {
		t.Error("a broken phase recorded the trigger as worked")
	}
}

func TestTheConversationLedgerIsReadInAndWrittenBack(t *testing.T) {
	t.Parallel()
	conversations := ledgerstore.NewMemoryConversations()
	ctx := context.Background()
	if err := conversations.Append(ctx, "ceo", "slack:C1",
		ledger.Session{Reply: "said this before"}, "wk-old", clock, 0); err != nil {
		t.Fatalf("Append: %v", err)
	}

	r := &recorder{result: turn.Result{
		Decision: phase.Done, Artifact: "and now this",
		LastReview: &turn.Review{CompletedWork: "the post landed"},
	}}
	d := dispatcher(t, r)
	d.Conversations = conversations
	if got := d.Dispatch(ctx, "ceo", []*events.Event{inThread("notification", "slack:C1")}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}

	if len(r.reqs) != 1 {
		t.Fatalf("the turn ran %d times", len(r.reqs))
	}
	if r.reqs[0].ConversationKey != "slack:C1" {
		t.Errorf("conversation = %q", r.reqs[0].ConversationKey)
	}
	if len(r.reqs[0].History) != 1 || r.reqs[0].History[0].Reply != "said this before" {
		t.Errorf("the turn did not receive its conversation history: %+v", r.reqs[0].History)
	}

	after, err := conversations.History(ctx, "ceo", "slack:C1", 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("history = %d entries, want the turn appended", len(after))
	}
	if after[1].Reply != "and now this" || after[1].Decision != "done" {
		t.Errorf("the appended entry = %+v", after[1])
	}
	// The reviewer's own prose about what landed survives a `done` round,
	// which appends no iteration record of its own.
	if after[1].CompletedWork != "the post landed" {
		t.Errorf("completed work = %q", after[1].CompletedWork)
	}
}

// The `enabled` toggle is documented as a live kill switch that "restores the
// previous prompt exactly". It was read NOWHERE — the dispatcher wired its
// conversation store whenever a store existed — so the switch did nothing, and
// nothing noticed.
func TestTheConversationLedgerKillSwitchActuallyStops(t *testing.T) {
	t.Parallel()
	conversations := ledgerstore.NewMemoryConversations()
	ctx := context.Background()
	if err := conversations.Append(ctx, "ceo", "slack:C1",
		ledger.Session{Reply: "said this before"}, "wk-old", clock, 0); err != nil {
		t.Fatalf("Append: %v", err)
	}

	r := &recorder{result: turn.Result{Decision: phase.Done, Artifact: "and now this"}}
	d := dispatcher(t, r)
	d.Conversations = conversations
	d.Conversation = func() config.ConversationSession {
		return config.ConversationSession{Enabled: org.Off(), MaxEntries: 20, RetentionDays: 30}
	}
	d.Dispatch(ctx, "ceo", []*events.Event{inThread("notification", "slack:C1")})

	if len(r.reqs) != 1 {
		t.Fatalf("the turn ran %d times", len(r.reqs))
	}
	// Neither half: no history read in, no entry written back.
	if len(r.reqs[0].History) != 0 {
		t.Errorf("history reached a turn with the ledger switched off: %+v", r.reqs[0].History)
	}
	after, err := conversations.History(ctx, "ceo", "slack:C1", 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(after) != 1 {
		t.Errorf("history = %d entries; the turn wrote one with the ledger off", len(after))
	}
}

// max_entries is documented as what bounds a DM, whose conversation key is the
// whole channel and so never stops receiving entries. It was passed to Append
// as a literal 0 — "keep everything" — so the table grew for the life of the
// deployment.
func TestTheConversationLedgerTrimsToTheConfiguredKeep(t *testing.T) {
	t.Parallel()
	conversations := ledgerstore.NewMemoryConversations()
	ctx := context.Background()
	for i := range 6 {
		if err := conversations.Append(ctx, "ceo", "slack:C1",
			ledger.Session{Reply: "older " + strconv.Itoa(i)},
			"wk-"+strconv.Itoa(i), clock, 0); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	r := &recorder{result: turn.Result{Decision: phase.Done, Artifact: "newest"}}
	d := dispatcher(t, r)
	d.Conversations = conversations
	d.Conversation = func() config.ConversationSession {
		return config.ConversationSession{MaxEntries: 3, RetentionDays: 30}
	}
	d.Dispatch(ctx, "ceo", []*events.Event{inThread("notification", "slack:C1")})

	after, err := conversations.History(ctx, "ceo", "slack:C1", 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(after) != 3 {
		t.Fatalf("history = %d entries, want the configured keep of 3", len(after))
	}
	// The NEWEST survive: a trim that kept the oldest would hand the next
	// turn a history that stops before the message it is answering.
	if after[len(after)-1].Reply != "newest" {
		t.Errorf("the trim dropped the newest entry: %+v", after)
	}
}

func TestATriggerWithNoConversationWritesNoConversationEntry(t *testing.T) {
	t.Parallel()
	// A scheduled fire, a sub-agent, an internal event. Writing it under an
	// empty key would collect every such turn into one pseudo-conversation
	// and feed it back to the next one as history.
	conversations := ledgerstore.NewMemoryConversations()
	r := &recorder{result: turn.Result{Decision: phase.Done, Artifact: "x"}}
	d := dispatcher(t, r)
	d.Conversations = conversations
	ctx := context.Background()
	d.Dispatch(ctx, "ceo", []*events.Event{ev("notification")})

	got, err := conversations.History(ctx, "ceo", "", 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("an unkeyed turn wrote %d conversation entries", len(got))
	}
}

func TestAnUnreadableConversationIsLessContextNotAFailedTurn(t *testing.T) {
	t.Parallel()
	// The read RAISES rather than returning empty, precisely so this
	// decision is made here and visibly, instead of a database outage
	// looking like a first turn.
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Conversations = failingConversations{}
	got := d.Dispatch(context.Background(), "ceo",
		[]*events.Event{inThread("notification", "slack:C1")})

	if got.Outcome != queue.OutcomeAck {
		t.Errorf("outcome = %v, want the turn to have run anyway", got.Outcome)
	}
	if len(r.reqs) != 1 {
		t.Fatalf("the turn ran %d times", len(r.reqs))
	}
	if len(r.reqs[0].History) != 0 {
		t.Errorf("history = %+v, want none", r.reqs[0].History)
	}
}

func TestOnlyLedgeredTypesAreRecorded(t *testing.T) {
	t.Parallel()
	// A type the ledger never writes cannot be looked up by it, so
	// recording one produces a row nothing will ever match.
	completions := ledgerstore.NewMemoryCompletions()
	internal := ev("internal")
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Completions = completions
	ctx := context.Background()
	d.Dispatch(ctx, "ceo", []*events.Event{internal})

	if len(r.reqs) != 1 {
		t.Fatalf("the turn ran %d times", len(r.reqs))
	}
	// It still DISPATCHES — not ledgered is not irrelevant.
	if len(r.reqs[0].Events) != 1 {
		t.Error("a non-ledgered event was dropped from the dispatch")
	}
	if got := completions.Worked(ctx, "ceo",
		[]string{workkey.Derive([]string{internal.ID.String()})}); len(got) != 0 {
		t.Error("a non-ledgered event was recorded")
	}
	// And with no ledgerable trigger the work key is empty, which is the
	// documented "nothing to collapse".
	if r.keys[0] != "" {
		t.Errorf("work key = %q, want empty", r.keys[0])
	}
}

func TestTheConversationKeyComesFromTheFirstEventThatNamesOne(t *testing.T) {
	t.Parallel()
	// A partition is one conversation by construction — that is what the
	// broker's key function guarantees — so taking the first keeps the
	// answer stable rather than depending on which event sorts last.
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Dispatch(context.Background(), "ceo", []*events.Event{
		ev("notification"),
		inThread("notification", "slack:C1"),
		inThread("notification", "slack:C2"),
	})
	if len(r.reqs) != 1 {
		t.Fatalf("the turn ran %d times", len(r.reqs))
	}
	if r.reqs[0].ConversationKey != "slack:C1" {
		t.Errorf("conversation = %q, want the first named", r.reqs[0].ConversationKey)
	}
	if !r.reqs[0].Coalesce {
		t.Error("a three-event partition was not routed for coalescing")
	}
}

func TestNothingWiredIsTheEmbeddedSingleNodeCase(t *testing.T) {
	t.Parallel()
	// With no peer to race, the seat lease is the whole mutual exclusion
	// and there is nothing for a completion ledger to add. A dispatcher
	// with no conditions must therefore SERVE, not refuse.
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := &engine.Dispatcher{Turn: r.run, Now: func() time.Time { return clock }}
	if got := d.Dispatch(context.Background(), "ceo", []*events.Event{ev("notification")}); got.Outcome != queue.OutcomeAck {
		t.Errorf("outcome = %v", got.Outcome)
	}
	if len(r.reqs) != 1 {
		t.Error("a bare dispatcher refused to run a turn")
	}
}

type failingConversations struct{}

func (failingConversations) Append(context.Context, string, string, ledger.Session, string, time.Time, int) error {
	return errors.New("store down")
}

func (failingConversations) History(context.Context, string, string, int) ([]ledger.Session, error) {
	return nil, errors.New("store down")
}

func (failingConversations) Threads(context.Context, string, int) ([]ledgerstore.Thread, error) {
	return nil, errors.New("store down")
}

func (failingConversations) Purge(context.Context, time.Time) (int64, error) {
	return 0, errors.New("store down")
}

func eventIDs(evs []*events.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.ID.String())
	}
	return out
}

// A MERGE HAS TO BE RECORDED HERE, because here is where the constituent
// list exists: a coalesced digest is minted fresh and carries no memory of
// what it absorbed, so by the time a turn is running there is nothing left
// to count. Nothing else emits it, which is why the Integrations room could
// report how many deliveries ARRIVED and not how many turns they became.
func TestAMergedPartitionIsRecordedWithItsConstituents(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	var seen []*events.Event
	d.Observe = func(_ context.Context, e *events.Event) { seen = append(seen, e) }

	first := inThread("notification", "slack:C1/T1")
	first.Source = "slack"
	first.Timestamp = clock
	second := inThread("notification", "slack:C1/T1")
	second.Source = "slack"
	second.Timestamp = clock.Add(2 * time.Minute)

	if got := d.Dispatch(context.Background(), "ceo",
		[]*events.Event{first, second}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}
	if len(seen) != 1 {
		t.Fatalf("%d observations, want one merge record", len(seen))
	}
	rec, ok := seen[0].Data.(*types.NotificationsCoalesced)
	if !ok {
		t.Fatalf("observation is %T", seen[0].Data)
	}
	if rec.Count != 2 || rec.AgentHandle != "ceo" {
		t.Errorf("record = %+v", rec)
	}
	if rec.ConversationKey != "slack:C1/T1" {
		t.Errorf("conversation = %q", rec.ConversationKey)
	}
	// The SPAN they arrived in, which is what an operator reads to see how
	// hard batching kicked in — the count alone cannot say whether it was
	// a burst or a slow thread.
	if rec.FirstAt != clock.UTC().Format(time.RFC3339) ||
		rec.LastAt != clock.Add(2*time.Minute).UTC().Format(time.RFC3339) {
		t.Errorf("span = %s..%s", rec.FirstAt, rec.LastAt)
	}
	if rec.NotificationSource != "slack" {
		t.Errorf("source = %q, want the vendor rather than the engine", rec.NotificationSource)
	}
}

// A PARTITION OF ONE IS NOT A MERGE, and a record on every single delivery
// would make the count meaningless.
func TestASingleEventIsNotRecordedAsAMerge(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	var seen int
	d.Observe = func(context.Context, *events.Event) { seen++ }
	if got := d.Dispatch(context.Background(), "ceo",
		[]*events.Event{ev("notification")}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}
	if seen != 0 {
		t.Errorf("%d observations for one event, want none", seen)
	}
}

// THE TRIGGER'S DELEGATION REACHES THE TURN THAT RUNS FOR IT.
//
// The regression this exists for: the Request literal in Dispatch set neither
// Depth nor DelegationChain, so every turn on the inbox path ran at depth 0.
// turn.CheckDepth compared a constant zero against the limit and could never
// fire, and turn_engine.delegation_depth_limit — the one guard against two
// agents asking each other the same question until a budget runs out — bounded
// nothing at all.
func TestTheTriggersDelegationReachesTheTurn(t *testing.T) {
	t.Parallel()
	var got engine.Request
	d := &engine.Dispatcher{
		Conditions: func(string) inbox.Conditions {
			return inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true}
		},
		Turn: func(_ context.Context, req engine.Request) (turn.Result, error) {
			got = req
			return turn.Result{Decision: phase.Done}, nil
		},
	}
	e := newEngine(t, engine.Options{Dispatch: d})

	ask := ev("a2a_request")
	ask.DelegationDepth = 2
	ask.DelegationChain = []string{"ceo", "cto"}
	if res := e.Dispatch(context.Background(), "ceo", []*events.Event{ask}); res.Outcome != queue.OutcomeAck {
		t.Fatalf("dispatch = %+v", res)
	}

	if got.Depth != 2 {
		t.Errorf("the turn ran at depth %d, want the trigger's 2 — the cap is "+
			"measured against this and bounds nothing at zero", got.Depth)
	}
	if len(got.DelegationChain) != 2 {
		t.Errorf("the turn carries chain %v, want the trigger's", got.DelegationChain)
	}
}

// THE ONE WAY OUT OF THE SANDBOX PARK. A coding run that stops to ask a
// person something leaves its seat busy, so every inbound on that seat is
// requeued — including the person's reply. Without this seam the answer is
// parked behind the question for ever: the run sits awaiting until its box's
// pause TTL reclaims it, and the person who answered is never told anything
// happened.
func TestAnAnswerToAParkedRunIsHandledRatherThanRequeued(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, AwaitingSandbox: true}
	}
	var asked []string
	d.Answer = func(_ context.Context, handle, conversation, answer string,
		trigger *events.Event,
	) (bool, error) {
		asked = append(asked, handle+"/"+conversation)
		if answer == "" || trigger == nil {
			t.Error("the answer text and its trigger did not reach the coordinator")
		}
		return true, nil
	}

	got := d.Dispatch(context.Background(), "swe",
		[]*events.Event{inThread("notification", "chat:C1")})
	if got.Outcome != queue.OutcomeAck {
		t.Errorf("outcome = %v, want an ack — the delivery was handled", got.Outcome)
	}
	if len(r.parked) != 0 {
		t.Errorf("the answer was requeued behind the question it answers: %v", r.parked)
	}
	if !slices.Equal(asked, []string{"swe/chat:C1"}) {
		t.Errorf("the coordinator was asked %v", asked)
	}
}

// FAIL-OPEN, in every direction. A delivery that is NOT the answer, a
// conversation the partition cannot name, a coordinator that errored, and a
// node with no coordinator at all must each park as before: parking is
// recoverable, and acking a message nothing handled is not.
func TestADeliveryThatIsNotTheAnswerStillParks(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]func(context.Context, string, string, string, *events.Event) (bool, error){
		"not this run's answer": func(context.Context, string, string, string, *events.Event) (bool, error) {
			return false, nil
		},
		"an unreadable store": func(context.Context, string, string, string, *events.Event) (bool, error) {
			return false, errors.New("the coordination store is unreachable")
		},
		"no coordinator": nil,
	} {
		r := &recorder{}
		d := dispatcher(t, r)
		d.Conditions = func(string) inbox.Conditions {
			return inbox.Conditions{Owned: true, TurnEngineReady: true,
				AdmitsTriggers: true, AwaitingSandbox: true}
		}
		d.Answer = answer

		got := d.Dispatch(context.Background(), "swe",
			[]*events.Event{inThread("notification", "chat:C1")})
		if got.Outcome != queue.OutcomeAck {
			t.Errorf("%s: outcome = %v, want an ack for a successful park", name, got.Outcome)
		}
		if len(r.parked) != 1 {
			t.Errorf("%s: the delivery was not parked (%d parks)", name, len(r.parked))
		}
		if len(r.reqs) != 0 {
			t.Errorf("%s: a turn ran on a seat parked on a coding run", name)
		}
	}
}

// A partition with no conversation cannot be matched against a question asked
// in one, so it parks without asking: the coordinator's own disambiguation is
// positional within a conversation, and offering it a key-less delivery would
// let a scheduled fire answer somebody's question.
func TestADeliveryWithNoConversationIsNotOfferedAsAnAnswer(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, AwaitingSandbox: true}
	}
	called := false
	d.Answer = func(context.Context, string, string, string, *events.Event) (bool, error) {
		called = true
		return true, nil
	}
	d.Dispatch(context.Background(), "swe", []*events.Event{ev("notification")})
	if called {
		t.Error("a delivery with no conversation was offered as an answer")
	}
	if len(r.parked) != 1 {
		t.Errorf("it was not parked either (%d parks)", len(r.parked))
	}
}

// AND ONLY THE SANDBOX PARK. Every other park is a node that cannot run the
// turn at all — no turn engine, a shedding config posture — and offering
// those deliveries to a coordinator would answer a question with a message
// the seat was never able to read.
func TestOnlyTheSandboxParkOffersItsDeliveryAsAnAnswer(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: false, AdmitsTriggers: true}
	}
	called := false
	d.Answer = func(context.Context, string, string, string, *events.Event) (bool, error) {
		called = true
		return true, nil
	}
	d.Dispatch(context.Background(), "swe",
		[]*events.Event{inThread("notification", "chat:C1")})
	if called {
		t.Error("a park for a missing turn engine was offered as an answer")
	}
	if len(r.parked) != 1 {
		t.Errorf("it was not parked (%d parks)", len(r.parked))
	}
}
