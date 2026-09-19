package engine_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/workkey"
)

var clock = time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

// ev builds one inbox trigger, THE WAY ITS PRODUCER BUILDS IT.
//
// "notification" is this file's shorthand for a real inbound message, and it
// resolves to events.New over the registered payload rather than to an event
// literal. The distinction is load-bearing rather than tidy: a literal carries
// no typed body, so a partition of them does not DECODE, so the dispatcher
// declines to merge it and degrades to per-event dispatch — and every
// coalescing assertion in this file would pass on the degrade branch while the
// merge itself ran for nobody.
//
// Any other kind is a bare envelope on purpose: that is what an event of a
// type this build does not know decodes to.
func ev(kind string) *events.Event {
	if kind == "notification" {
		e := events.New(types.ExternalNotification{
			NotificationSource: "slack", SourceEventType: "message",
			Sender: "ana", Subject: "a message", Body: "hello",
		}, events.TraceContext{})
		// AS internal/notify STAMPS IT: the envelope names the PRODUCER
		// of the wake, not the third-party app: "notify.slack", never "slack".
		// A test that wrote the bare third-party app name here asserted against a
		// shape nothing publishes, and passed for a coalescing record that
		// filed every merge under a source no dashboard filter matches.
		e.Source = "notify.slack"
		e.Timestamp = clock
		return e
	}
	return &events.Event{ID: uuid.New(), Type: kind}
}

// notificationType is the wire type ev("notification") produces.
var notificationType = types.ExternalNotification{}.EventType()

// said builds a notification with a body of its own, for the cases that read
// the merged digest rather than only counting constituents.
func said(sender, body string, at time.Time) *events.Event {
	salient := body
	e := events.New(types.ExternalNotification{
		NotificationSource: "slack", SourceEventType: "message",
		Sender: sender, Subject: "a message", Body: "TRIAGE SCAFFOLDING\n\n" + body,
		SalientBody: &salient,
	}, events.TraceContext{})
	e.Source = "notify.slack"
	e.Timestamp = at
	notifyStamp(e, "slack:C1", "slack:C1")
	return e
}

// notifyStamp writes both keys the way internal/notify does — THROUGH THE
// CONSTANTS, never the literals they hold: with the identity read falling back
// to the partition field for an older peer's event, a test spelling
// "conversation_key" out keeps passing whichever field production reads, which
// is the blind spot node/concurrency_test.go records having shipped once.
func notifyStamp(e *events.Event, partition, conversation string) {
	if e.Payload == nil {
		e.Payload = map[string]any{}
	}
	e.Payload[notify.PartitionField] = partition
	e.Payload[notify.ConversationField] = conversation
}

// inThread is an event whose two keys coincide — a shared channel, an issue,
// a page: every source but a direct message.
func inThread(kind, conversation string) *events.Event {
	e := ev(kind)
	notifyStamp(e, conversation, conversation)
	return e
}

// notifyStampedIn is one notification with both keys stated outright, for the
// cases that need two events in one partition.
func notifyStampedIn(partition, conversation string) *events.Event {
	e := ev("notification")
	notifyStamp(e, partition, conversation)
	return e
}

// inDirectThread is the one shape where they differ: a reply in the thread a
// direct message started partitions on the thread and belongs to the whole DM
// channel.
func inDirectThread(kind, channel, thread string) *events.Event {
	e := ev(kind)
	notifyStamp(e, channel+":"+thread, channel)
	return e
}

// recorder captures what the dispatcher asked of the turn engine.
type recorder struct {
	reqs      []engine.Request
	keys      []string
	result    turn.Result
	err       error
	panicWith any
	parked    [][]*events.Event
	paused    []string
	deferred  []string
}

func (r *recorder) run(_ context.Context, req engine.Request) (turn.Result, error) {
	r.reqs = append(r.reqs, req)
	r.keys = append(r.keys, req.WorkKey)
	if r.panicWith != nil {
		panic(r.panicWith)
	}
	return r.result, r.err
}

func dispatcher(t *testing.T, r *recorder) *engine.Dispatcher {
	t.Helper()
	return &engine.Dispatcher{
		Ledgered: func(kind string) bool { return kind == notificationType },
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
	// AND A RUN ID OF ITS OWN. A redelivered trigger re-derives the same
	// work key by design, so a dispatch that reused it as the run's
	// identity wrote the retry's phases on top of the previous attempt's.
	// See ADR-0017.
	if r.reqs[0].RunID == "" {
		t.Error("the dispatch minted no run id")
	}
	if r.reqs[0].RunID == r.reqs[0].WorkKey {
		t.Error("the run id IS the work key — a re-run would collide with the attempt it repeats")
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
			inbox.Conditions{Owned: true, TurnEngineReady: true, SeatHeldBySandbox: true},
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
		return inbox.Conditions{Owned: true, TurnEngineReady: true, SeatHeldBySandbox: true}
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
		return inbox.Conditions{Owned: true, TurnEngineReady: true, SeatHeldBySandbox: true}
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

// THE TRANSIENT CASE KEEPS ITS RETRY, and this is the test a wrong fix breaks.
//
// A broken phase that proved nothing reached outside the engine is the case
// the dispatcher's old comment described for EVERY failure: nothing was
// recorded, so a redelivery genuinely does run it cleanly. A provider that
// never answered, a runner that could not be built and a refused budget all
// land here — none of them proves a write, and every one of them is worth
// trying again. A seat handed to another node mid-turn is worth trying again
// too, and gets the cheaper verb: see the deferral test below.
func TestABrokenPhaseThatProvedNothingStillNAKsAndRecordsNothing(t *testing.T) {
	t.Parallel()
	completions := ledgerstore.NewMemoryCompletions()
	a := ev("notification")
	// Acted is false: the zero Result is a turn that proved nothing, which
	// is the safe answer and the one every pre-effect failure produces.
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

// A SEAT THAT MOVED MID-TURN DEFERS RATHER THAN NAKS.
//
// It is the same condition [inbox.Screen] refuses before the turn starts —
// "seat is not owned here" — caught a phase later, because the window is open
// for the whole length of a turn and only one end of it was ever checked. One
// condition, so one disposition: a deferral hands the delivery to the seat's
// new owner at zero accrued redeliveries and quiesces this attachment, where
// a NAK spends one of the trigger's twenty-five deliveries on a node with no
// further claim to the seat.
func TestASeatThatMovedMidTurnIsDeferredToItsNewOwner(t *testing.T) {
	t.Parallel()
	completions := ledgerstore.NewMemoryCompletions()
	a := ev("notification")
	r := &recorder{err: fmt.Errorf("runner: execute: %w", seat.ErrSeatMoved)}
	d := dispatcher(t, r)
	d.Completions = completions
	ctx := context.Background()

	got := d.Dispatch(ctx, "ceo", []*events.Event{a})
	if got.Outcome != queue.OutcomeDefer {
		t.Fatalf("outcome = %v, want a deferral: the delivery is healthy and a "+
			"successor is entitled to it", got.Outcome)
	}
	if len(completions.Worked(ctx, "ceo", []string{workkey.Derive([]string{a.ID.String()})})) != 0 {
		t.Error("a seat that moved recorded the trigger as worked, so its new " +
			"owner will never run it")
	}
	// The host has to hear about it, or the consumer stays attached to a
	// seat this node no longer serves.
	if len(r.deferred) != 1 || r.deferred[0] != "ceo" {
		t.Errorf("deferred = %v, want the seat host told once", r.deferred)
	}
}

// AND NOT WHEN THE TURN ALREADY ACTED, which is the half a deferral must not
// take: handing the delivery to a successor that would repeat a chat post is
// the storm [Dispatcher.abandon] exists to stop, and it does not stop being
// that because the reason this node gave up was losing the seat.
func TestASeatThatMovedAfterActingIsStillRecordedRatherThanDeferred(t *testing.T) {
	t.Parallel()
	completions := ledgerstore.NewMemoryCompletions()
	a := ev("notification")
	r := &recorder{
		result: turn.Result{Acted: true},
		err:    fmt.Errorf("runner: execute: %w", seat.ErrSeatMoved),
	}
	d := dispatcher(t, r)
	d.Completions = completions
	ctx := context.Background()

	got := d.Dispatch(ctx, "ceo", []*events.Event{a})
	if got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v, want an ACK — the successor would repeat this "+
			"turn's writes", got.Outcome)
	}
	key := workkey.Derive([]string{a.ID.String()})
	if !completions.Worked(ctx, "ceo", []string{key})[key] {
		t.Error("the trigger was not recorded, so a peer's redelivery runs it again")
	}
}

// A TURN THAT ALREADY WROTE OUTSIDE THE ENGINE IS NOT REDELIVERED.
//
// The NAK above used to be unconditional, on a premise that held only for the
// two writes internal/workkey guards: every MCP write, chat post, colleague
// ask and coding run is keyed on nothing, so a deterministic mid-turn failure
// replayed round one's external effects across the broker's whole delivery
// budget — 25 attempts a second apart.
func TestATurnThatBrokeAfterActingIsRecordedRatherThanRedelivered(t *testing.T) {
	t.Parallel()
	completions := ledgerstore.NewMemoryCompletions()
	a := ev("notification")
	r := &recorder{
		result: turn.Result{Acted: true},
		err:    errors.New("the reviewer's provider went away"),
	}
	d := dispatcher(t, r)
	d.Completions = completions
	var seen []*events.Event
	d.Observe = func(_ context.Context, e *events.Event) { seen = append(seen, e) }
	ctx := context.Background()

	got := d.Dispatch(ctx, "ceo", []*events.Event{a})
	if got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v, want an ACK — a retry would repeat this turn's writes", got.Outcome)
	}
	key := workkey.Derive([]string{a.ID.String()})
	if !completions.Worked(ctx, "ceo", []string{key})[key] {
		t.Error("the trigger was not recorded, so a park requeue or a peer's " +
			"redelivery would run it again")
	}
	// AND IT SAYS SO. Giving up on a trigger silently is the one thing
	// worse than the storm.
	if len(seen) != 1 {
		t.Fatalf("observed %d events, want the abandoned trigger on the record", len(seen))
	}
	skipped, ok := seen[0].Data.(*types.TurnTriggerSkipped)
	if !ok {
		t.Fatalf("observed %T, want a TurnTriggerSkipped", seen[0].Data)
	}
	if skipped.TriggerID != a.ID.String() {
		t.Errorf("trigger id = %q, want the abandoned trigger's", skipped.TriggerID)
	}
	if !strings.Contains(skipped.Reason, "outside the engine") {
		t.Errorf("reason = %q, want it to say why the trigger will not come back", skipped.Reason)
	}
}

// A PANICKED TURN IS RECORDED, even though its record proves no write.
//
// The proof rule above keeps a retry for a turn that proved nothing, because
// such a turn may succeed next time. A panic will not: the redelivery runs the
// same code on the same input. Before the loop recovered panics this reached
// the queue backend's own guard, which NAKs, and the trigger came back up to
// the whole delivery budget.
func TestAPanickedTurnIsRecordedRatherThanRedelivered(t *testing.T) {
	t.Parallel()
	completions := ledgerstore.NewMemoryCompletions()
	a := ev("notification")
	r := &recorder{
		// What turn.Run hands back for a panicking phase: no proven
		// write, the unhandled-exception guard, and the panic as the
		// error.
		result: turn.Result{Decision: phase.Failed, Breach: &turn.Breach{
			Kind: types.GuardUnhandledException, Detail: "panic: nil map",
		}},
		err: fmt.Errorf("turn: execute round 1: %w", turn.Recovered("nil map")),
	}
	d := dispatcher(t, r)
	d.Completions = completions
	var seen []*events.Event
	d.Observe = func(_ context.Context, e *events.Event) { seen = append(seen, e) }
	d.Identify = func(string) (string, string) { return "CEO", "a-1" }
	ctx := context.Background()

	got := d.Dispatch(ctx, "ceo", []*events.Event{a})
	if got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v, want an ACK: a redelivery reaches the same panic", got.Outcome)
	}
	key := workkey.Derive([]string{a.ID.String()})
	if !completions.Worked(ctx, "ceo", []string{key})[key] {
		t.Error("the panicked trigger was not recorded, so a peer's redelivery runs it again")
	}
	// ONE record from the dispatcher, the skipped trigger. The breach is the
	// TURN's to publish (its telemetry already did, from the result above),
	// and a second one here would count one panic twice.
	if len(seen) != 1 {
		t.Fatalf("observed %d events (%v), want only the skipped trigger", len(seen), typesSeen(seen))
	}
	skipped, ok := seen[0].Data.(*types.TurnTriggerSkipped)
	if !ok {
		t.Fatalf("observed %T, want a TurnTriggerSkipped", seen[0].Data)
	}
	if !strings.Contains(skipped.Reason, "panicked") {
		t.Errorf("reason = %q, want it to say the turn panicked", skipped.Reason)
	}
}

// A PANIC THE LOOP NEVER SAW IS RECOVERED AT THE DISPATCHER.
//
// The loop recovers a panicking phase; everything between the broker and the
// loop is this frame's: the screening stages, and the turn's own set-up and
// tear-down around the loop. Each case panics somewhere different, and each
// must settle the delivery, record the trigger and put the seat AFK, because
// no turn telemetry ran to do it.
func TestAPanicOutsideTheLoopIsRecoveredAtTheDispatcher(t *testing.T) {
	t.Parallel()
	for name, arrange := range map[string]func(d *engine.Dispatcher, r *recorder){
		"in the turn's own frames": func(_ *engine.Dispatcher, r *recorder) {
			r.panicWith = "runner could not be built: nil registry"
		},
		"in a screening stage": func(d *engine.Dispatcher, _ *recorder) {
			d.Conditions = func(string) inbox.Conditions { panic("lease table returned nil") }
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			completions := ledgerstore.NewMemoryCompletions()
			a := ev("notification")
			r := &recorder{}
			d := dispatcher(t, r)
			d.Completions = completions
			var seen []*events.Event
			d.Observe = func(_ context.Context, e *events.Event) { seen = append(seen, e) }
			d.Identify = func(handle string) (string, string) {
				if handle != "ceo" {
					t.Errorf("identified %q, want the dispatched seat", handle)
				}
				return "CEO", "a-1"
			}
			arrange(d, r)
			ctx := context.Background()

			got := d.Dispatch(ctx, "ceo", []*events.Event{a})
			if got.Outcome != queue.OutcomeAck {
				t.Fatalf("outcome = %v, want an ACK: an escaped panic is NAKed by the "+
					"queue backend and redelivered into the same defect", got.Outcome)
			}
			key := workkey.Derive([]string{a.ID.String()})
			if !completions.Worked(ctx, "ceo", []string{key})[key] {
				t.Error("the trigger was not recorded")
			}
			var breach *types.TurnGuardBreach
			var skipped *types.TurnTriggerSkipped
			for _, e := range seen {
				switch data := e.Data.(type) {
				case *types.TurnGuardBreach:
					breach = data
				case *types.TurnTriggerSkipped:
					skipped = data
				}
			}
			if breach == nil {
				t.Fatalf("observed %v, want a guard breach: without it the seat never "+
					"goes AFK and shows whatever it was last doing", typesSeen(seen))
			}
			if breach.Kind != types.GuardUnhandledException || breach.RoleName != "CEO" ||
				breach.Agent != "a-1" {
				t.Errorf("breach = %+v, want unhandled_exception addressed to CEO/a-1", breach)
			}
			if breach.TurnID != key {
				t.Errorf("breach turn id = %q, want the partition's work key %q", breach.TurnID, key)
			}
			if skipped == nil || !strings.Contains(skipped.Reason, "panicked") {
				t.Errorf("skipped = %+v, want the trigger on the record as panicked", skipped)
			}
		})
	}
}

// A seat the company does not name gets no breach, and the delivery is still
// settled: a breach addressed to no role moves no seat, and refusing to settle
// over it would hand the panic back to the queue.
func TestAPanicForAnUnknownSeatIsStillSettled(t *testing.T) {
	t.Parallel()
	r := &recorder{panicWith: "boom"}
	d := dispatcher(t, r)
	var seen []*events.Event
	d.Observe = func(_ context.Context, e *events.Event) { seen = append(seen, e) }
	d.Identify = func(string) (string, string) { return "", "" }

	got := d.Dispatch(context.Background(), "ghost", []*events.Event{ev("notification")})
	if got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v, want an ACK", got.Outcome)
	}
	for _, e := range seen {
		if _, ok := e.Data.(*types.TurnGuardBreach); ok {
			t.Errorf("a breach was published for a seat that does not exist: %+v", e.Data)
		}
	}
}

// A PANIC WHILE REQUEUING CLAIMS NOTHING IT HAS ALREADY HANDED BACK.
//
// A park requeues the whole delivery and acks, one publish per event, so a
// panic part-way through leaves some copies on the queue and the rest
// unpublished. The unpublished ones are lost to the ack whatever this frame
// does; the published ones are lost only if it records them, because a
// completion record is what stops a copy ever running. So the park narrows
// what it holds to nothing BEFORE it publishes, rather than after.
func TestAPanicWhileRequeuingLeavesThePublishedCopiesToRun(t *testing.T) {
	t.Parallel()
	completions := ledgerstore.NewMemoryCompletions()
	first, second := ev("notification"), ev("notification")
	r := &recorder{}
	d := dispatcher(t, r)
	d.Completions = completions
	d.Identify = func(string) (string, string) { return "CEO", "a-1" }
	// The seat is parked on a detached run, so the whole delivery is
	// requeued rather than worked.
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true,
			SeatHeldBySandbox: true}
	}
	d.Park = func(_ context.Context, _ string, evs []*events.Event) error {
		r.parked = append(r.parked, evs[:1])
		panic("the broker client died mid-requeue")
	}
	ctx := context.Background()

	got := d.Dispatch(ctx, "ceo", []*events.Event{first, second})
	if got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v, want an ACK: a redelivery reaches the same defect", got.Outcome)
	}
	if len(r.parked) != 1 || len(r.parked[0]) != 1 {
		t.Fatalf("the requeue did not publish before it panicked: %v", r.parked)
	}
	keys := []string{
		workkey.Derive([]string{first.ID.String()}),
		workkey.Derive([]string{second.ID.String()}),
	}
	for _, key := range completions.Worked(ctx, "ceo", keys) {
		if key {
			t.Error("a panicking requeue recorded what it had already published, " +
				"so that copy is dropped unrun when the seat takes its next delivery")
		}
	}
	if len(r.reqs) != 0 {
		t.Error("a parked seat ran a turn")
	}
}

// AND THE SAME HOLDS WHEN THE REQUEUE ITSELF PANICS PART-WAY.
//
// The degraded path requeues the tail one event at a time, so a panic inside
// that loop leaves some of the tail's copies on the queue. Only the head is
// ever this delivery's to settle, and it stops being answerable for the tail
// the moment the first copy is published — so the narrowing happens BEFORE the
// requeue, not after. Narrowed after, this frame records a tail whose copies
// are already waiting to run, and a completion record is what stops a copy
// ever running.
func TestAPanicWhileRequeuingADegradedTailKeepsNoClaimOnIt(t *testing.T) {
	t.Parallel()
	completions := ledgerstore.NewMemoryCompletions()
	r := &recorder{}
	d := dispatcher(t, r)
	d.Completions = completions
	d.Identify = func(string) (string, string) { return "CEO", "a-1" }
	ctx := context.Background()

	head := said("ana", "first", clock)
	opaque := &events.Event{ID: uuid.New(), Type: notificationType, Timestamp: clock.Add(time.Minute)}
	notifyStamp(opaque, "slack:C1", "slack:C1")
	// Publishes the first copy, then dies — the shape [Engine.park]'s own
	// loop has, one Publish per event.
	d.Park = func(_ context.Context, _ string, evs []*events.Event) error {
		r.parked = append(r.parked, evs)
		panic("the broker client died mid-requeue")
	}

	if got := d.Dispatch(ctx, "ceo", []*events.Event{head, opaque}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v, want an ACK: a redelivery reaches the same defect", got.Outcome)
	}
	tailKey := workkey.Derive([]string{opaque.ID.String()})
	if completions.Worked(ctx, "ceo", []string{tailKey})[tailKey] {
		t.Error("a panicking requeue recorded the tail it was part-way through " +
			"publishing, so that copy is dropped unrun")
	}
	if len(r.reqs) != 0 {
		t.Error("the head ran although the requeue never returned")
	}
}

// A PANIC SETTLES WHAT THE DELIVERY STILL HOLDS, NOT WHAT IT ARRIVED WITH.
//
// A partition that will not merge requeues its tail before its head runs, so
// when the head's turn panics the tail's copies are already back on the queue.
// Recording the tail as worked would drop every one of them unrun when it came
// back, the loss inbox.Degraded keys the head apart to prevent.
func TestAPanicInADegradedHeadLeavesTheRequeuedTailToRun(t *testing.T) {
	t.Parallel()
	completions := ledgerstore.NewMemoryCompletions()
	r := &recorder{panicWith: "nil map in the head's turn"}
	d := dispatcher(t, r)
	d.Completions = completions
	var seen []*events.Event
	d.Observe = func(_ context.Context, e *events.Event) { seen = append(seen, e) }
	d.Identify = func(string) (string, string) { return "CEO", "a-1" }
	ctx := context.Background()

	head := said("ana", "first", clock)
	opaque := &events.Event{ID: uuid.New(), Type: notificationType, Timestamp: clock.Add(time.Minute)}
	notifyStamp(opaque, "slack:C1", "slack:C1")

	if got := d.Dispatch(ctx, "ceo", []*events.Event{head, opaque}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v, want an ACK", got.Outcome)
	}
	if len(r.parked) != 1 || len(r.parked[0]) != 1 || r.parked[0][0].ID != opaque.ID {
		t.Fatalf("the tail was not requeued before the head ran: %v", r.parked)
	}
	headKey := workkey.Derive([]string{head.ID.String()})
	tailKey := workkey.Derive([]string{opaque.ID.String()})
	worked := completions.Worked(ctx, "ceo", []string{headKey, tailKey})
	if !worked[headKey] {
		t.Error("the head whose turn panicked was not recorded, so it runs the defect again")
	}
	if worked[tailKey] {
		t.Error("the requeued tail was recorded as worked, so its copy is dropped unrun")
	}
	for _, e := range seen {
		switch data := e.Data.(type) {
		case *types.TurnTriggerSkipped:
			if data.TriggerID != head.ID.String() {
				t.Errorf("a skip record names %s, which this delivery handed back "+
					"to the queue", data.TriggerID)
			}
		case *types.TurnGuardBreach:
			if data.TurnID != headKey {
				t.Errorf("breach turn id = %q, want the head's own key %q", data.TurnID, headKey)
			}
		}
	}
}

// And a constituent the ledger had already dropped is not put on the record a
// second time as panicked: it was settled by the turn that worked it.
func TestAPanicDoesNotReRecordWhatTheLedgerAlreadyDropped(t *testing.T) {
	t.Parallel()
	completions := ledgerstore.NewMemoryCompletions()
	worked, fresh := ev("notification"), ev("notification")
	ctx := context.Background()
	if err := completions.Record(ctx, "ceo",
		workkey.Derive([]string{worked.ID.String()}), "", clock); err != nil {
		t.Fatalf("Record: %v", err)
	}
	r := &recorder{panicWith: "boom"}
	d := dispatcher(t, r)
	d.Completions = completions
	var seen []*events.Event
	d.Observe = func(_ context.Context, e *events.Event) { seen = append(seen, e) }
	d.Identify = func(string) (string, string) { return "CEO", "a-1" }

	if got := d.Dispatch(ctx, "ceo", []*events.Event{worked, fresh}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v, want an ACK", got.Outcome)
	}
	reasons := map[string][]string{}
	for _, e := range seen {
		if skip, ok := e.Data.(*types.TurnTriggerSkipped); ok {
			reasons[skip.TriggerID] = append(reasons[skip.TriggerID], skip.Reason)
		}
	}
	if got := reasons[worked.ID.String()]; len(got) != 1 || strings.Contains(got[0], "panicked") {
		t.Errorf("the already-worked trigger's records = %q, want the one skip saying "+
			"it was worked, and no panic", got)
	}
	if got := reasons[fresh.ID.String()]; len(got) != 1 || !strings.Contains(got[0], "panicked") {
		t.Errorf("the trigger whose turn panicked has records %q, want one naming the panic", got)
	}
}

func typesSeen(evs []*events.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

// ABANDONING A TURN FILES NO REPLY TO THE THREAD.
//
// A broken turn has no answer: its artifact is whichever round closed last, so
// writing it back as this turn's reply tells the next turn on that thread the
// work was done. RecordSession's own doc commemorates exactly that bug for the
// suspended case — which is why the abandon path writes the completion rows
// itself instead of reusing recordWorked.
func TestAnAbandonedTurnWritesNoConversationEntry(t *testing.T) {
	t.Parallel()
	conversations := ledgerstore.NewMemoryConversations()
	ctx := context.Background()
	r := &recorder{
		result: turn.Result{Acted: true, Decision: phase.Failed, Artifact: "a half-written draft"},
		err:    errors.New("the reviewer's provider went away"),
	}
	d := dispatcher(t, r)
	d.Conversations = conversations
	d.Completions = ledgerstore.NewMemoryCompletions()

	if got := d.Dispatch(ctx, "ceo", []*events.Event{inThread("notification", "slack:C1")}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}
	after, err := conversations.History(ctx, "ceo", "slack:C1", 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("history = %+v, want nothing — a broken turn said nothing to file", after)
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
		Decision: phase.Done, Delivered: true, Artifact: "and now this",
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

// A TURN THAT TOLD NOBODY MUST NOT WRITE THAT IT DID.
//
// The artifact filed here is the reviewer's prose about the turn, not anything
// a tool sent, and it was recorded unconditionally as the seat's own "You
// replied". So a turn that did real work and never reached the person waiting
// — the round budget ran out, the loop broke, the reviewer closed it anyway —
// wrote into its own history that it had answered. That record is the one the
// NEXT turn on the thread reads back, which is what made the failure seal
// itself: a follow-up asking "did you do it?" is answered against a reply that
// was never sent.
func TestAnUndeliveredTurnIsNotRecordedAsAReply(t *testing.T) {
	t.Parallel()
	conversations := ledgerstore.NewMemoryConversations()
	ctx := context.Background()

	// Done, with real work behind it, and nothing delivered.
	r := &recorder{result: turn.Result{
		Decision: phase.Done, Delivered: false,
		Artifact: "Task LEAD-1 was opened and is ready to refine",
	}}
	d := dispatcher(t, r)
	d.Conversations = conversations
	if got := d.Dispatch(ctx, "ceo",
		[]*events.Event{inThread("notification", "slack:C1")}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}

	after, err := conversations.History(ctx, "ceo", "slack:C1", 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("history = %d entries, want the turn's own", len(after))
	}
	if after[0].Reply != "" {
		t.Errorf("reply = %q — the seat recorded an answer it never gave, and "+
			"its next turn on this thread will read it back as fact", after[0].Reply)
	}
	// KEPT, not dropped. The conclusion is real context for the next turn;
	// what it must not be is a reply.
	if after[0].Unsent != "Task LEAD-1 was opened and is ready to refine" {
		t.Errorf("unsent = %q, want the artifact — dropping it trades a wrong "+
			"answer for a blank one", after[0].Unsent)
	}
	// And the rendered block a model actually reads has to SAY so, because
	// "no reply line" is something the reader must notice while "nobody
	// received this" is something it must answer.
	block := ledger.RenderHistory(after, ledger.HistoryOptions{MaxChars: 10_000})
	if !strings.Contains(block, "did NOT reply") {
		t.Errorf("the history block does not say the turn reached nobody:\n%s", block)
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

	r := &recorder{result: turn.Result{Decision: phase.Done, Delivered: true, Artifact: "and now this"}}
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

	r := &recorder{result: turn.Result{Decision: phase.Done, Delivered: true, Artifact: "newest"}}
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
	// documented "nothing to collapse" — while the RUN is still identified,
	// because an execution always has one.
	if r.keys[0] != "" {
		t.Errorf("work key = %q, want empty", r.keys[0])
	}
	if r.reqs[0].RunID == "" {
		t.Error("a turn with no ledgerable trigger still ran with no run id")
	}
}

func TestTheConversationKeyComesFromTheFirstEventThatNamesOne(t *testing.T) {
	t.Parallel()
	// A partition is one conversation by construction — the broker's key
	// function guarantees the partition key and a source's partition key
	// refines its identity — so taking the first keeps the answer stable
	// rather than depending on which event sorts last.
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
	first.Timestamp = clock
	second := inThread("notification", "slack:C1/T1")
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
	// THE VENDOR, not the producer of the wake. internal/notify stamps the
	// envelope "notify.slack"; every other notification event carries the
	// bare third-party app name, and a record that disagrees is filed under a source
	// no dashboard filter matches — so the Integrations room reported zero
	// coalesced merges for every integration.
	if rec.NotificationSource != "slack" {
		t.Errorf("source = %q, want the bare third-party app name", rec.NotificationSource)
	}
	// The SPAN they arrived in, which is what an operator reads to see how
	// hard batching kicked in — the count alone cannot say whether it was
	// a burst or a slow thread.
	if rec.FirstAt != clock.UTC().Format(time.RFC3339) ||
		rec.LastAt != clock.Add(2*time.Minute).UTC().Format(time.RFC3339) {
		t.Errorf("span = %s..%s", rec.FirstAt, rec.LastAt)
	}
	if rec.NotificationSource != "slack" {
		t.Errorf("source = %q, want the third-party app rather than the engine", rec.NotificationSource)
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
			AdmitsTriggers: true, SeatHeldBySandbox: true}
	}
	var asked []string
	d.Answer = func(_ context.Context, handle string, conv sandbox.ConversationRef,
		answer string, trigger *events.Event,
	) (bool, error) {
		asked = append(asked, handle+"/"+conv.Identity)
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

// THE ANSWER ARRIVES AT A SEAT THAT LOOKS IDLE, and has to be claimed before
// an ordinary turn eats it.
//
// The park above is the case where a SECOND run holds the seat. The ordinary
// one is this: a run parked on a question gives its seat back — the reply
// arrives on that seat's own inbox and a person can take days — so the answer
// reaches the dispatcher through the plain proceed path, carrying nothing that
// says what it answers. Offered only from the park, the match could run only
// while the seat was HELD, which is the one state a parked run is never in: no
// clarification answer ever reached the run that asked it, and every box
// waited out its pause TTL with the reply sitting in the inbox.
func TestAnAnswerIsClaimedBeforeItBecomesAnOrdinaryTurn(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions {
		// FREE, with a question open on it — which is what a parked run
		// actually leaves behind.
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, SandboxAwaitsAnswer: true}
	}
	var asked []string
	d.Answer = func(_ context.Context, handle string, conv sandbox.ConversationRef,
		answer string, trigger *events.Event,
	) (bool, error) {
		asked = append(asked, handle+"/"+conv.Identity+"/"+conv.Partition)
		if answer == "" || trigger == nil {
			t.Error("the answer text and its trigger did not reach the coordinator")
		}
		return true, nil
	}

	got := d.Dispatch(context.Background(), "swe",
		[]*events.Event{inDirectThread("notification", "chat:D1", "root-1")})
	if got.Outcome != queue.OutcomeAck {
		t.Errorf("outcome = %v, want an ack — the delivery was handled", got.Outcome)
	}
	if !slices.Equal(asked, []string{"swe/chat:D1/chat:D1:root-1"}) {
		t.Errorf("the coordinator was asked %v, want the conversation and the "+
			"batch the reply arrived in", asked)
	}
	if len(r.reqs) != 0 {
		t.Errorf("a turn ran on the answer as well as the resume it triggered: %d", len(r.reqs))
	}
	if len(r.parked) != 0 {
		t.Errorf("a seat with nothing holding it parked its mail: %v", r.parked)
	}
}

// AND FALLS THROUGH TO THE TURN WHEN IT IS NOT THE ANSWER. A seat with a
// question open is an ordinary working seat: every message on it that no
// parked run claims is work like any other, and swallowing those would make a
// forgotten clarification deafen the seat until its pause TTL expired.
func TestAMessageThatAnswersNothingStillRunsItsTurn(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, SandboxAwaitsAnswer: true}
	}
	d.Answer = func(context.Context, string, sandbox.ConversationRef, string,
		*events.Event,
	) (bool, error) {
		return false, nil
	}

	got := d.Dispatch(context.Background(), "swe",
		[]*events.Event{inThread("notification", "chat:C1")})
	if got.Outcome != queue.OutcomeAck {
		t.Errorf("outcome = %v, want an ack for a completed turn", got.Outcome)
	}
	if len(r.reqs) != 1 {
		t.Fatalf("the turn engine ran %d times, want the ordinary turn", len(r.reqs))
	}
	if len(r.parked) != 0 {
		t.Errorf("the delivery was parked instead: %v", r.parked)
	}
}

// A TRIGGER THE LEDGER HAS ALREADY WORKED IS NOT OFFERED EITHER.
//
// The offer on this path sits AFTER the completion read, and that is the
// ordering under test: a redelivery of a trigger that already produced a turn
// must not be spliced into somebody's coding run as the answer to its
// question, which is a second use of one message and the run's whole
// disambiguation is "the next thing to arrive here".
func TestAnAlreadyWorkedTriggerIsNotOfferedAsAnAnswer(t *testing.T) {
	t.Parallel()
	completions := ledgerstore.NewMemoryCompletions()
	worked := inThread("notification", "chat:C1")
	ctx := context.Background()
	if err := completions.Record(ctx, "swe",
		workkey.Derive([]string{worked.ID.String()}), "", clock); err != nil {
		t.Fatalf("Record: %v", err)
	}

	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Completions = completions
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, SandboxAwaitsAnswer: true}
	}
	called := false
	d.Answer = func(context.Context, string, sandbox.ConversationRef, string,
		*events.Event,
	) (bool, error) {
		called = true
		return true, nil
	}

	if got := d.Dispatch(ctx, "swe", []*events.Event{worked}); got.Outcome != queue.OutcomeAck {
		t.Errorf("outcome = %v, want an ack", got.Outcome)
	}
	if called {
		t.Error("a trigger this seat had already worked was offered as a clarification answer")
	}
	if len(r.reqs) != 0 {
		t.Errorf("the worked trigger ran again (%d turns)", len(r.reqs))
	}
}

// FAIL-OPEN, in every direction. A delivery that is NOT the answer, a
// conversation the partition cannot name, a coordinator that errored, and a
// node with no coordinator at all must each park as before: parking is
// recoverable, and acking a message nothing handled is not.
func TestADeliveryThatIsNotTheAnswerStillParks(t *testing.T) {
	t.Parallel()
	type answerer func(context.Context, string, sandbox.ConversationRef, string,
		*events.Event) (bool, error)
	for name, answer := range map[string]answerer{
		"not this run's answer": func(context.Context, string, sandbox.ConversationRef,
			string, *events.Event,
		) (bool, error) {
			return false, nil
		},
		"an unreadable store": func(context.Context, string, sandbox.ConversationRef,
			string, *events.Event,
		) (bool, error) {
			return false, errors.New("the coordination store is unreachable")
		},
		"no coordinator": nil,
	} {
		r := &recorder{}
		d := dispatcher(t, r)
		d.Conditions = func(string) inbox.Conditions {
			return inbox.Conditions{Owned: true, TurnEngineReady: true,
				AdmitsTriggers: true, SeatHeldBySandbox: true}
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
			AdmitsTriggers: true, SeatHeldBySandbox: true}
	}
	called := false
	d.Answer = func(context.Context, string, sandbox.ConversationRef, string,
		*events.Event,
	) (bool, error) {
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
	d.Answer = func(context.Context, string, sandbox.ConversationRef, string,
		*events.Event,
	) (bool, error) {
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

// ONE CONVERSATION, ONE NOTIFICATION — the whole point of coalescing, and the
// half that shipped without a caller.
//
// Batching landed on its own: a partition of five already became one turn.
// What that turn was HANDED was the five enriched bodies concatenated, because
// notify.Coalesce was called by nothing, so the seat read the third-party app's triage
// scaffolding five times, each copy pointing at staler state, and five separate
// asks, which a model answers separately. The assertion that catches it is the
// scaffolding count: a merged digest renders it ONCE.
func TestACoalescedConversationReachesTheTurnAsOneDigest(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)

	first := said("ana", "Can you take the deploy today?", clock)
	second := said("bo", "Blocked on the migration — see PROJ-4.", clock.Add(time.Minute))
	third := said("ana", "Never mind, I have it.", clock.Add(2*time.Minute))

	if got := d.Dispatch(context.Background(), "ceo",
		[]*events.Event{first, second, third}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}
	if len(r.reqs) != 1 {
		t.Fatalf("the turn ran %d times", len(r.reqs))
	}

	// ONE trigger event, not three.
	if n := len(r.reqs[0].Trigger); n != 1 {
		t.Fatalf("the turn was handed %d asks, want one merged digest", n)
	}
	ask := engine.DescribeTrigger(r.reqs[0].Ask())

	// EVERY constituent survives the merge: a digest that kept only the
	// latest hands the seat a follow-up with no idea what it follows.
	for _, want := range []string{
		"Can you take the deploy today?",
		"Blocked on the migration — see PROJ-4.",
		"Never mind, I have it.",
	} {
		if !strings.Contains(ask, want) {
			t.Errorf("the digest lost %q\ngot:\n%s", want, ask)
		}
	}
	// AND THE SCAFFOLDING RENDERS ONCE. This is the assertion that fails
	// without the merge: three concatenated bodies carry three copies.
	if n := strings.Count(ask, "TRIAGE SCAFFOLDING"); n != 1 {
		t.Errorf("the third-party app scaffolding rendered %d times, want once\ngot:\n%s", n, ask)
	}
	// The digest says it IS one, so the seat answers once rather than
	// three times.
	if !strings.Contains(ask, "Coalesced updates") {
		t.Errorf("the merged ask does not tell the seat it is one piece of work:\n%s", ask)
	}

	// THE BOOKKEEPING STILL READS THE CONSTITUENTS. The merged digest is
	// minted fresh, so a ledger keyed on it would match nothing and re-run
	// the turn on every redelivery.
	if n := len(r.reqs[0].Events); n != 3 {
		t.Errorf("the dispatch carries %d constituents, want all three", n)
	}
	if r.reqs[0].ConversationKey != "slack:C1" {
		t.Errorf("conversation = %q", r.reqs[0].ConversationKey)
	}
}

// A MERGE MUST NOT LAUNDER WHAT BOUNDS THE TURN.
//
// The merged event replaces three envelopes with one, so every fact that
// bounded the partition has to be carried onto it deliberately: the deepest
// delegation (or an ask arriving beside a shallow mention resets the cap the
// ping-pong guard is measured against), the delivery obligation (or one direct
// ask inside a burst of broadcasts becomes traffic the seat may ignore), and
// the recon flag (or a bare pointer merged with a substantive message stops
// telling the prefetch to go and look).
func TestAMergeCarriesWhatBoundsTheTurn(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)

	passing := said("ana", "fyi", clock)
	asked := said("bo", "can you review this?", clock.Add(time.Minute))
	if n, ok := events.DataAs[*types.ExternalNotification](asked); ok {
		n.Addressed = true
		n.ContextRequiresRecon = true
	}
	passing.DelegationDepth, passing.DelegationChain = 3, []string{"a", "b", "c"}

	if got := d.Dispatch(context.Background(), "ceo",
		[]*events.Event{passing, asked}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}
	if len(r.reqs) != 1 || len(r.reqs[0].Trigger) != 1 {
		t.Fatalf("the partition did not merge into one ask: %+v", r.reqs)
	}
	merged := r.reqs[0].Trigger[0]

	if merged.DelegationDepth != 3 {
		t.Errorf("the merged ask charges depth %d, want the deepest constituent's 3",
			merged.DelegationDepth)
	}
	if len(merged.DelegationChain) != 3 {
		t.Errorf("the merged ask carries chain %v, want the deepest constituent's",
			merged.DelegationChain)
	}
	note, ok := events.DataAs[*types.ExternalNotification](merged)
	if !ok {
		t.Fatalf("the merged ask carries %T, not a notification", merged.Data)
	}
	if !note.Addressed {
		t.Error("the ask was laundered by the merge — the seat may now end in silence")
	}
	if !note.ContextRequiresRecon {
		t.Error("the pointer was laundered by the merge — the prefetch will search on it")
	}
	if len(note.Messages) != 2 {
		t.Errorf("the merged ask carries %d constituents, want both — the learning "+
			"workers observe each distinct sender", len(note.Messages))
	}
	// BOTH KEYS ride the merged envelope, because from here on this one
	// event IS the partition and a reader handed it must be able to ask
	// either question.
	if got, _ := merged.Payload[notify.PartitionField].(string); got != "slack:C1" {
		t.Errorf("the merged ask carries partition key %q", got)
	}
	if got, _ := merged.Payload[notify.ConversationField].(string); got != "slack:C1" {
		t.Errorf("the merged ask carries conversation identity %q", got)
	}
	// And the obligation the turn engine enforces is derived from the
	// constituents either way — including WHERE it is owed, which a merge
	// must not launder any more than it may launder the ask itself.
	if got := engine.ReplyFor(r.reqs[0].Events); got != turn.ToolReply("slack") {
		t.Errorf("the turn owes %+v, want a tool delivery on slack", got)
	}
}

// A PARTITION THAT WILL NOT MERGE DEGRADES RATHER THAN LOSING AN EVENT.
//
// The tail is requeued FIRST and the head runs in the ack scope already open,
// so a requeue failure aborts before any work has run and a completed turn is
// never replayed by a later event's failure.
func TestAnUnmergeablePartitionDegradesToPerEventDispatch(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)

	// A constituent with no typed body — what an event whose payload this
	// build cannot decode looks like.
	head := said("ana", "first", clock)
	opaque := &events.Event{ID: uuid.New(), Type: notificationType, Timestamp: clock.Add(time.Minute)}
	notifyStamp(opaque, "slack:C1", "slack:C1")

	if got := d.Dispatch(context.Background(), "ceo",
		[]*events.Event{head, opaque}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}
	if len(r.reqs) != 1 {
		t.Fatalf("the turn ran %d times", len(r.reqs))
	}
	if ids := eventIDs(r.reqs[0].Events); !slices.Equal(ids, []string{head.ID.String()}) {
		t.Errorf("the turn received %v, want only the head", ids)
	}
	if r.reqs[0].Coalesce {
		t.Error("a partition that did not merge still reported itself coalesced")
	}
	// The tail went back on the queue rather than being dropped.
	if len(r.parked) != 1 || len(r.parked[0]) != 1 ||
		r.parked[0][0].ID != opaque.ID {
		t.Errorf("the tail was not requeued: %v", r.parked)
	}
	// And the head is recorded under its OWN key, not the partition's: only
	// the head ran, and the tail's copies are still on the queue.
	if r.keys[0] != workkey.Derive([]string{head.ID.String()}) {
		t.Errorf("the head was recorded under %q, want its own key", r.keys[0])
	}
}

// A REQUEUE THAT FAILS ABORTS BEFORE ANY WORK HAS RUN.
func TestADegradeWhoseRequeueFailsRunsNothing(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Park = func(context.Context, string, []*events.Event) error {
		return errors.New("broker down")
	}

	head := said("ana", "first", clock)
	opaque := &events.Event{ID: uuid.New(), Type: notificationType, Timestamp: clock.Add(time.Minute)}
	notifyStamp(opaque, "slack:C1", "slack:C1")

	if got := d.Dispatch(context.Background(), "ceo",
		[]*events.Event{head, opaque}); got.Outcome != queue.OutcomeNak {
		t.Errorf("outcome = %v, want a nak so the whole partition comes back", got.Outcome)
	}
	if len(r.reqs) != 0 {
		t.Error("a turn ran on a partition whose tail could not be requeued")
	}
}

// A SKIPPED TRIGGER IS ON THE RECORD.
//
// The narrow window this covers: a turn finished, its outbound effects
// shipped, and the delivery came back — from a node that died before acking,
// or from a drain whose partitions together outlasted the ack window. Without
// a record the feed shows the arrivals and one turn, and nothing distinguishes
// "the agent never answered" from "the agent already answered, on a node that
// has since died" — the same observation and opposite problems.
//
// types.TurnTriggerSkipped was registered, categorised and documented as
// shipped with no producer anywhere, so the case stayed exactly as invisible
// as it was before the type existed.
func TestATriggerTheLedgerAlreadyWorkedIsRecordedAsSkipped(t *testing.T) {
	t.Parallel()
	completions := ledgerstore.NewMemoryCompletions()
	worked, fresh := ev("notification"), ev("notification")
	ctx := context.Background()
	if err := completions.Record(ctx, "ceo",
		workkey.Derive([]string{worked.ID.String()}), "", clock); err != nil {
		t.Fatalf("Record: %v", err)
	}

	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Completions = completions
	var seen []*events.Event
	d.Observe = func(_ context.Context, e *events.Event) { seen = append(seen, e) }

	if got := d.Dispatch(ctx, "ceo", []*events.Event{worked, fresh}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}

	var skips []*types.TurnTriggerSkipped
	for _, e := range seen {
		if s, ok := events.DataAs[*types.TurnTriggerSkipped](e); ok {
			skips = append(skips, s)
		}
	}
	if len(skips) != 1 {
		t.Fatalf("%d skip records, want one for the trigger the ledger had worked", len(skips))
	}
	if skips[0].TriggerID != worked.ID.String() || skips[0].AgentHandle != "ceo" {
		t.Errorf("skip record = %+v", skips[0])
	}
	if skips[0].Reason == "" {
		t.Error("a skip with no reason is the case an operator most needs to read")
	}
	// The counterfactual: a partition with nothing worked records no skip,
	// or every dispatch in the company would file one.
	seen = nil
	if got := d.Dispatch(ctx, "cto", []*events.Event{ev("notification")}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}
	for _, e := range seen {
		if _, ok := events.DataAs[*types.TurnTriggerSkipped](e); ok {
			t.Error("a dispatch that skipped nothing filed a skip record")
		}
	}
}

// THE MERGED DIGEST IS WHAT THE TURN'S CONTEXT IS BUILT FROM.
//
// Its constituent list is the one place a conversation's senders, their
// per-message bodies and their recon flags are combined, so the prefetch and
// the learning workers read it rather than re-deriving the same facts from the
// partition. Two answers to one question are free to disagree; one of them was
// also unreachable, which is how a populated field ends up read by nothing.
func TestTheMergedAskCarriesEverySpeakerInTheOrderTheySpoke(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)

	if got := d.Dispatch(context.Background(), "ceo", []*events.Event{
		said("ana", "first", clock),
		said("bo", "second", clock.Add(time.Minute)),
		said("cy", "third", clock.Add(2*time.Minute)),
	}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}
	if len(r.reqs) != 1 || len(r.reqs[0].Trigger) != 1 {
		t.Fatalf("the partition did not merge into one ask")
	}
	note, ok := events.DataAs[*types.ExternalNotification](r.reqs[0].Trigger[0])
	if !ok {
		t.Fatalf("the merged ask carries %T", r.reqs[0].Trigger[0].Data)
	}
	var spoke []string
	for _, m := range note.Messages {
		spoke = append(spoke, m.Sender)
	}
	// IN THE ORDER THEY SPOKE. The flat Sender field mirrors the LATEST
	// constituent, so a reader that took it as well as the list put the
	// last speaker at the head of a list whose whole contract is the order.
	if !slices.Equal(spoke, []string{"ana", "bo", "cy"}) {
		t.Errorf("the merged ask's speakers are %v", spoke)
	}
	if note.Sender != "cy" {
		t.Errorf("the merged ask's flat sender = %q, want the latest constituent's", note.Sender)
	}
}

// THE SAME TRIGGER, TWICE, IS TWO RUNS OF ONE UNIT OF WORK.
//
// A turn that fails without reaching outside the engine is NAK'd, and the
// broker redelivers the very same event — so the work key, which is derived
// from the trigger's ids, is reproduced by construction. That is what it is
// for. What must NOT be reproduced is the run: everything a turn publishes is
// keyed on it, so a second attempt under the first one's id writes its phases,
// its live row and its detached sandbox run on top of the attempt it is
// repeating. See ADR-0017.
func TestARedeliveredTriggerRunsUnderItsOwnIdentity(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done, Artifact: "posted"}}
	d := dispatcher(t, r)
	// The SAME event both times, which is exactly what a NAK redelivers.
	trigger := ev("notification")

	d.Dispatch(context.Background(), "ceo", []*events.Event{trigger})
	d.Dispatch(context.Background(), "ceo", []*events.Event{trigger})

	if len(r.reqs) != 2 {
		t.Fatalf("the turn engine ran %d times, want 2", len(r.reqs))
	}
	if r.reqs[0].WorkKey != r.reqs[1].WorkKey {
		t.Errorf("the work key changed across a redelivery (%q then %q) — the "+
			"completion ledger and the episode row could no longer collapse it",
			r.reqs[0].WorkKey, r.reqs[1].WorkKey)
	}
	if r.reqs[0].RunID == r.reqs[1].RunID {
		t.Errorf("both runs share the id %q — the retry would publish its phases "+
			"under the identity the previous attempt already occupied", r.reqs[0].RunID)
	}
}

// A DIRECT MESSAGE'S TWO TURNS SHARE ONE LEDGER, which is the defect this
// split exists to fix and the only test that states it end to end inside the
// dispatcher.
//
// Turn one is a burst of top-level DMs: one partition on the bare channel,
// filed under the channel. Turn two is the person's reply in the thread the
// agent opened: a DIFFERENT partition — it must not merge with unrelated
// top-level pings — and the SAME conversation, so it reads turn one's entry
// back. While one value answered both questions turn two looked its history up
// under "chat:D1:root" and found a first turn, every time.
func TestADirectMessagesThreadReplyReadsTheBurstsLedgerEntry(t *testing.T) {
	t.Parallel()
	conversations := ledgerstore.NewMemoryConversations()
	ctx := context.Background()

	burst := &recorder{result: turn.Result{
		Decision: phase.Done, Delivered: true, Artifact: "answered the DM",
	}}
	d := dispatcher(t, burst)
	d.Conversations = conversations
	if got := d.Dispatch(ctx, "ceo", []*events.Event{
		notifyStampedIn("chat:D1", "chat:D1"),
		notifyStampedIn("chat:D1", "chat:D1"),
	}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}
	if len(burst.reqs) != 1 {
		t.Fatalf("a typing burst ran %d turns, want one", len(burst.reqs))
	}
	if burst.reqs[0].ConversationKey != "chat:D1" {
		t.Fatalf("the burst filed under %q", burst.reqs[0].ConversationKey)
	}

	reply := &recorder{result: turn.Result{Decision: phase.Done}}
	d2 := dispatcher(t, reply)
	d2.Conversations = conversations
	if got := d2.Dispatch(ctx, "ceo", []*events.Event{
		inDirectThread("notification", "chat:D1", "root-1"),
	}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}
	if len(reply.reqs) != 1 {
		t.Fatalf("the reply ran %d turns", len(reply.reqs))
	}
	if reply.reqs[0].ConversationKey != "chat:D1" {
		t.Fatalf("the reply filed under %q, want the DM line the burst used",
			reply.reqs[0].ConversationKey)
	}
	if len(reply.reqs[0].History) != 1 ||
		reply.reqs[0].History[0].Reply != "answered the DM" {
		t.Fatalf("the reply turn read %+v as its history — the seat's own answer of a "+
			"moment earlier is filed where it cannot see it", reply.reqs[0].History)
	}
}

// A RUN PARKED FROM A TOP-LEVEL DM IS ANSWERED BY THE PERSON'S THREADED REPLY,
// and the dispatcher's half of that is which conversation it offers the
// coordinator for such a reply: the WHOLE DM LINE, because that is the value
// the row was parked under.
//
// The engine's own prompt is what makes the two differ. A top-level direct
// message partitions on the bare channel, so a run launched from that turn
// parks under the channel — and [notify.ChatPrompt] then tells the seat to
// reply AS A THREAD, so the person's answer arrives partitioned on the thread
// it opened. While this offered the partition, the coordinator compared two
// strings that could never be equal: the clarification was silently never
// delivered and the box waited out its pause TTL.
//
// THE PARTITION STILL TRAVELS, and only for the rows parked before an identity
// was ever written — see [sandbox.ConversationRef.Answers]. The ledger read
// and the session write take the identity as they already did, which is the
// second half asserted here: one trigger, and every reader of its conversation
// answering the same thing.
func TestADMThreadReplyAnswersAndFilesUnderTheWholeDMLine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var offered sandbox.ConversationRef
	parked := &recorder{}
	dp := dispatcher(t, parked)
	dp.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, SeatHeldBySandbox: true}
	}
	dp.Answer = func(_ context.Context, _ string, conv sandbox.ConversationRef,
		_ string, _ *events.Event,
	) (bool, error) {
		offered = conv
		return true, nil
	}
	if got := dp.Dispatch(ctx, "swe", []*events.Event{
		inDirectThread("notification", "chat:D1", "root-1"),
	}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}
	if offered.Identity != "chat:D1" {
		t.Errorf("the parked run was offered the conversation %q, want the DM line — "+
			"a run launched from a top-level DM is parked under it and can be "+
			"answered by nothing else", offered.Identity)
	}
	if offered.Partition != "chat:D1:root-1" {
		t.Errorf("the partition did not travel beside it (%q), so a row parked "+
			"before the identity existed carries nothing this can match",
			offered.Partition)
	}

	// THE SAME TRIGGER through an ordinary dispatch: the ledger keys on the
	// DM line rather than on the thread the reply happened to land in.
	conversations := ledgerstore.NewMemoryConversations()
	r := &recorder{result: turn.Result{
		Decision: phase.Done, Delivered: true, Artifact: "done",
	}}
	d := dispatcher(t, r)
	d.Conversations = conversations
	if got := d.Dispatch(ctx, "ceo", []*events.Event{
		inDirectThread("notification", "chat:D1", "root-1"),
	}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}
	if len(r.reqs) != 1 || r.reqs[0].ConversationKey != "chat:D1" {
		t.Fatalf("the turn served conversation %+v, want the whole DM line", r.reqs)
	}
	filed, err := conversations.History(ctx, "ceo", "chat:D1", 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(filed) != 1 {
		t.Fatalf("the ledger holds %d entries under the DM line, want the turn's", len(filed))
	}
}
