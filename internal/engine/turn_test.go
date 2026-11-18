package engine_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/workkey"
)

var clock = time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

// ev builds one inbox trigger the way its producer builds it.
//
// "notification" resolves to events.New over the registered payload, never an
// event literal: a literal has no typed body, so its partition does not decode,
// the dispatcher degrades to per-event dispatch, and every coalescing assertion
// here would pass on the degrade branch with the merge never run. Any other
// kind is a bare envelope, which is what an unknown event type decodes to.
func ev(kind string) *events.Event {
	if kind == "notification" {
		e := events.New(types.ExternalNotification{
			NotificationSource: "slack", SourceEventType: "message",
			Sender: "ana", Subject: "a message", Body: "hello",
		}, events.TraceContext{})
		// As internal/notify stamps it: the envelope names the wake's
		// producer, "notify.slack", never the bare app name, which would
		// assert against a shape nothing publishes.
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

// notifyStamp writes both keys as internal/notify does, through the
// constants, so a renamed field fails here rather than leaving a test writing
// a field production no longer reads.
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

// headroom states each message's own remaining deliveries, per event rather
// than one partition number, because the gate under test reads each message's
// own (see [Dispatcher.mayOfferAnswer]).
func headroom(ctx context.Context, perMessage map[*events.Event]int) context.Context {
	hs := make([]queue.Headroom, 0, len(perMessage))
	for ev, left := range perMessage {
		hs = append(hs, queue.Headroom{ID: ev.ID, Left: left})
	}
	return queue.WithHeadroom(ctx, hs)
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
	holds     []inbox.Hold
	deferred  []string
	// sandboxHolds is every seat the dispatcher asked the sandbox
	// coordinator to hold.
	sandboxHolds []string
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
		Pause: func(_ context.Context, handle string, hold inbox.Hold, _ string) error {
			r.paused = append(r.paused, handle)
			r.holds = append(r.holds, hold)
			return nil
		},
		HoldSandbox: func(_ context.Context, handle string) {
			r.sandboxHolds = append(r.sandboxHolds, handle)
		},
		NoteDeferred: func(handle string) { r.deferred = append(r.deferred, handle) },
		Now:          func() time.Time { return clock },
	}
}

func TestAHealthyPartitionReachesTheTurnEngine(t *testing.T) {
	t.Parallel()
	// The control: without it every guard assertion below passes for a
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
	// AND A RUN ID OF ITS OWN: a redelivery re-derives the same work key,
	// so a run keyed on it writes the retry's phases over the previous
	// attempt's (ADR-0017).
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
		// HELD AND DEFERRED, never parked: a park republishes onto the
		// inbox the consumer is still reading, for the length of the run.
		"sandbox": {
			inbox.Conditions{Owned: true, TurnEngineReady: true, SeatHeldBySandbox: true},
			queue.OutcomeDefer, false, false, true},
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
	// The one screening that still parks: a company with no model, behind
	// the hold it takes first.
	d.Conditions = func(string) inbox.Conditions { return inbox.Conditions{Owned: true} }
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
	// The pause stops the requeued copies looping back at whatever rate the
	// broker serves; parking without it is worse than doing nothing.
	r := &recorder{}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions { return inbox.Conditions{Owned: true} }
	d.Pause = func(context.Context, string, inbox.Hold, string) error { return errors.New("no") }
	got := d.Dispatch(context.Background(), "ceo", []*events.Event{ev("notification")})
	// DEFERRED, not NAKed: a refused hold is the node's condition, and a
	// Nak returns the delivery behind its conversation's newer mail
	// (queue.OutcomeNak) where a deferral keeps its place.
	if got.Outcome != queue.OutcomeDefer {
		t.Errorf("outcome = %v, want a deferral", got.Outcome)
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
	d.Conditions = func(string) inbox.Conditions { return inbox.Conditions{Owned: true} }
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
	// The same-id dedupe is the first guard. A ledger read against the RAW
	// partition returns a filtered copy of the raw list, so the duplicate
	// survives to the turn and the seat answers one message twice.
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
	// The partition's own key covers the set that ran TOGETHER; a later
	// redelivery of a subset keys differently, matches nothing and runs
	// again: the partial-overlap bug, one step later.
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
// A broken phase whose record shows no act (a provider that never answered, a
// runner that could not be built) wrote nothing, so a redelivery runs it
// cleanly. Other failures take other verbs: a seat moved mid-turn is deferred
// (below); a refused budget, a chain wholly at its usage limit, or an
// incurable failure seen twice parks or pauses the seat.
func TestABrokenPhaseWhoseRecordShowsNoActStillNAKsAndRecordsNothing(t *testing.T) {
	t.Parallel()
	completions := ledgerstore.NewMemoryCompletions()
	a := ev("notification")
	// Acted is false: the zero Result is a turn whose record shows no act,
	// the safe answer and the one every pre-effect failure produces.
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
// It is [inbox.Screen]'s "seat is not owned here" caught a phase later (the
// window spans the whole turn), so it gets the same disposition: a deferral
// hands the delivery to the new owner AT ONCE and quiesces this attachment,
// where a NAK returns it on the failure path's doubling spacing and leaves
// this node consuming a mailbox it no longer owns.
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
	// The host must hear of it, or the consumer stays attached to a seat
	// this node no longer serves.
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
// internal/workkey guards only two writes: every MCP write, chat post,
// colleague ask and executor launch is keyed on nothing, so redelivering a
// deterministic mid-turn failure replays round one's external effects across
// the broker's whole delivery budget.
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
	// AND IT SAYS SO: giving up on a trigger silently is worse than the
	// storm.
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

// A PANICKED TURN IS RECORDED, even though its record shows no write.
//
// A no-act turn keeps its retry because it may succeed next time; a panic will
// not, since the redelivery runs the same code on the same input.
func TestAPanickedTurnIsRecordedRatherThanRedelivered(t *testing.T) {
	t.Parallel()
	completions := ledgerstore.NewMemoryCompletions()
	a := ev("notification")
	r := &recorder{
		// What turn.Run returns for a panicking phase: a record showing
		// no write, the unhandled-exception guard, the panic as the error.
		result: turn.Result{Decision: phase.Failed, Breach: &turn.Breach{
			Kind: types.GuardUnhandledException, Detail: "panic: nil map",
		}},
		err: fmt.Errorf("turn: execute round 1: %w", turn.Recovered(panics.ScopeTurn, "nil map")),
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
	// ONE record from the dispatcher, the skipped trigger: the breach is the
	// TURN's to publish (its telemetry did, from the result above), and a
	// second one here would count one panic twice.
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
// loop (the screening stages, the turn's set-up and tear-down) is this frame's.
// Each case must settle the delivery, record the trigger and put the failure on
// the seat, because no turn telemetry ran to do it.
//
// AND THE BREACH NAMES THE RUN THE PANIC ENDED in `turn_id`, never the work
// key: a turn announces itself under its run id before its prefetch, and every
// turn event is filed under it (ADR-0017), so the breach joins the turn's
// start. A screening-stage panic precedes any run and names none.
func TestAPanicOutsideTheLoopIsRecoveredAtTheDispatcher(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		arrange func(d *engine.Dispatcher, r *recorder)
		// ran reports whether the panic came from inside the turn, where
		// a run had been minted for it to name.
		ran bool
	}{
		"in the turn's own frames": {
			arrange: func(_ *engine.Dispatcher, r *recorder) {
				r.panicWith = "runner could not be built: nil registry"
			},
			ran: true,
		},
		"in a screening stage": {
			arrange: func(d *engine.Dispatcher, _ *recorder) {
				d.Conditions = func(string) inbox.Conditions { panic("lease table returned nil") }
			},
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
			tc.arrange(d, r)
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
					"records the failure and shows whatever it was last doing", typesSeen(seen))
			}
			if breach.Kind != types.GuardUnhandledException || breach.RoleName != "CEO" ||
				breach.Agent != "a-1" {
				t.Errorf("breach = %+v, want unhandled_exception addressed to CEO/a-1", breach)
			}
			wantRun := ""
			if tc.ran {
				if len(r.reqs) != 1 || r.reqs[0].RunID == "" {
					t.Fatalf("the turn ran %d times, want once under a minted run", len(r.reqs))
				}
				wantRun = r.reqs[0].RunID
			}
			if breach.TurnID != wantRun {
				t.Errorf("breach turn id = %q, want %q: the run the panic ended, "+
					"which every other record of that turn is filed under", breach.TurnID, wantRun)
			}
			if breach.WorkKey != key {
				t.Errorf("breach work key = %q, want the partition's %q", breach.WorkKey, key)
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
// A park publishes one copy per event and acks, so a panic part-way leaves
// some copies on the queue. Those are lost only if this frame records them (a
// completion record stops a copy ever running), so the park narrows what it
// holds to nothing BEFORE it publishes.
func TestAPanicWhileRequeuingLeavesThePublishedCopiesToRun(t *testing.T) {
	t.Parallel()
	completions := ledgerstore.NewMemoryCompletions()
	first, second := ev("notification"), ev("notification")
	r := &recorder{}
	d := dispatcher(t, r)
	d.Completions = completions
	d.Identify = func(string) (string, string) { return "CEO", "a-1" }
	// The company configures no model, so the whole delivery is requeued
	// behind the hold rather than worked.
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, AdmitsTriggers: true}
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

// AND THE SAME HOLDS WHEN A DEGRADED TAIL'S REQUEUE PANICS PART-WAY.
//
// Only the head is ever this delivery's to settle, and it stops answering for
// the tail once the first copy is published, so the narrowing happens BEFORE
// the requeue: narrowed after, this frame records a tail whose copies are
// already waiting to run, and that record drops them unrun.
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
	// Publishes the first copy, then dies: [Engine.park]'s shape, one
	// Publish per event.
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
// when the head's turn panics the tail's copies are already back on the queue;
// recording the tail would drop them unrun, the loss inbox.Degraded keys the
// head apart to prevent.
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
			// THE HEAD'S OWN KEY: the tail was never this delivery's to
			// answer for, so a key over both names a unit nobody worked.
			if data.WorkKey != headKey {
				t.Errorf("breach work key = %q, want the head's own key %q", data.WorkKey, headKey)
			}
			if len(r.reqs) != 1 || data.TurnID != r.reqs[0].RunID {
				t.Errorf("breach turn id = %q, want the run the head's turn was "+
					"dispatched under", data.TurnID)
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

func TestOnlyLedgeredTypesAreRecorded(t *testing.T) {
	t.Parallel()
	// A type the ledger never writes is never looked up, so recording one
	// produces a row nothing will match.
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
	// With no ledgerable trigger the work key is empty ("nothing to
	// collapse"), but the RUN is still identified: an execution always is.
	if r.keys[0] != "" {
		t.Errorf("work key = %q, want empty", r.keys[0])
	}
	if r.reqs[0].RunID == "" {
		t.Error("a turn with no ledgerable trigger still ran with no run id")
	}
}

func TestTheConversationKeyComesFromTheFirstEventThatNamesOne(t *testing.T) {
	t.Parallel()
	// A partition is one conversation by construction (the broker keys on the
	// partition key, which refines the conversation identity), so taking the
	// first keeps the answer stable rather than depending on which sorts last.
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
	// and a completion ledger adds nothing, so a dispatcher with no
	// conditions must SERVE, not refuse.
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := &engine.Dispatcher{Turn: r.run, Now: func() time.Time { return clock }}
	if got := d.Dispatch(context.Background(), "ceo", []*events.Event{ev("notification")}); got.Outcome != queue.OutcomeAck {
		t.Errorf("outcome = %v", got.Outcome)
	}
	if len(r.reqs) != 1 {
		t.Error("a bare dispatcher refused to run a turn")
	}
}

func eventIDs(evs []*events.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.ID.String())
	}
	return out
}

// A MERGE HAS TO BE RECORDED HERE, the one place the constituent list
// exists: a coalesced digest is minted fresh and remembers nothing it absorbed.
// Nothing else emits the record, and without it Company › Integrations counts
// the deliveries that arrived but not the turns they became.
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
	// THE PARTITION, on the field that says so: the record's subject is the
	// batch that merged, and `conversation_key` is the identity's tag.
	if rec.PartitionKey != "slack:C1/T1" {
		t.Errorf("partition = %q", rec.PartitionKey)
	}
	// THE VENDOR, not the wake's producer ("notify.slack"): every other
	// notification event carries the bare app name, and a record that
	// disagrees matches no dashboard filter.
	if rec.NotificationSource != "slack" {
		t.Errorf("source = %q, want the bare third-party app name", rec.NotificationSource)
	}
	// The SPAN they arrived in: the count alone cannot tell an operator a
	// burst from a slow thread.
	if rec.FirstAt != clock.UTC().Format(time.RFC3339) ||
		rec.LastAt != clock.Add(2*time.Minute).UTC().Format(time.RFC3339) {
		t.Errorf("span = %s..%s", rec.FirstAt, rec.LastAt)
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
// Without Depth and DelegationChain on the Request every inbox turn runs at
// depth 0, turn.CheckDepth never fires, and turn_engine.delegation_depth_limit
// (the one guard against two agents asking each other the same question until
// a budget runs out) bounds nothing.
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
	if res := engine.MailboxTurn(context.Background(), e, "ceo", []*events.Event{ask}); res.Outcome != queue.OutcomeAck {
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

// A SEAT ITS EXECUTOR HOLDS DEFERS UNDER THE SANDBOX HOLD, AND OFFERS NOTHING.
//
// The coordinator holds the inbox while a run holds the seat, so a delivery
// reaches here only by racing that hold or finding it refused. A park would
// requeue it as the next thing the consumer fetches, round and round for the
// whole run. Instead the hold is asked for again, the delivery is deferred at
// the head, and the seat host is told the consumer stopped, so its renew lifts
// the quiesce and leaves the hold as the only stop. An answer to ANOTHER of the
// seat's runs waits under the hold and is offered when the run lets go.
func TestAHeldSeatDefersUnderTheSandboxHoldAndOffersNothing(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, SeatHeldBySandbox: true, SandboxAwaitsAnswer: true}
	}
	offered := false
	d.Answer = func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
		offered = true
		return sandbox.AnswerConsumed, nil
	}

	got := d.Dispatch(context.Background(), "swe",
		[]*events.Event{inThread("notification", "chat:C1")})
	if got.Outcome != queue.OutcomeDefer {
		t.Errorf("outcome = %v, want a deferral that keeps the delivery at the head", got.Outcome)
	}
	if len(r.parked) != 0 {
		t.Errorf("the delivery was republished onto the inbox the consumer is reading: %v", r.parked)
	}
	if !slices.Equal(r.sandboxHolds, []string{"swe"}) {
		t.Errorf("the sandbox hold was asked for on %v, want the seat's — a delivery that "+
			"raced a refused hold must retry it", r.sandboxHolds)
	}
	if !slices.Equal(r.deferred, []string{"swe"}) {
		t.Errorf("deferral noted for %v, want the seat: an un-noted quiesce outlasts the hold", r.deferred)
	}
	if offered {
		t.Error("a held seat offered its mail as an answer")
	}
	if len(r.reqs) != 0 || len(r.paused) != 0 {
		t.Errorf("a held seat ran %d turns and took %v holds of its own", len(r.reqs), r.paused)
	}
}

// THE ANSWER ARRIVES AT A SEAT THAT LOOKS IDLE, and has to be claimed before
// an ordinary turn eats it.
//
// A run parked on a question gives its seat back (a person can take days), so
// the reply reaches the dispatcher on the plain proceed path, carrying nothing
// that says what it answers. Offered only while the seat is HELD, the one state
// a parked run is never in, no answer would reach the run that asked, and every
// box would wait out its pause TTL.
func TestAnAnswerIsClaimedBeforeItBecomesAnOrdinaryTurn(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions {
		// FREE, with a question open on it: what a parked run leaves.
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, SandboxAwaitsAnswer: true}
	}
	var asked []string
	d.Answer = func(_ context.Context, handle string, reply sandbox.Reply) (sandbox.AnswerDisposition, error) {
		conv, answer, trigger := unpackReply(reply)
		asked = append(asked, handle+"/"+conv.Identity+"/"+conv.Partition)
		if answer == "" || trigger == nil {
			t.Error("the answer text and its trigger did not reach the coordinator")
		}
		return sandbox.AnswerConsumed, nil
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

// AND AN ANSWER THE RUN IS STILL OWED IS HANDED BACK RATHER THAN RUN.
//
// The coordinator matched a parked run but could not resume it (no resumer
// here, a conversation from a build it cannot read, a seat not in its company)
// and gave the claim back, so the run awaits THIS message again; worked as a
// turn, it is consumed and the executor waits out its pause TTL. It is
// DEFERRED: a requeue republishes at the inbox's TAIL, and a NAK is withheld
// for a backoff while newer mail is served (queue.OutcomeNak), so either lets
// the person's next message be taken as the answer. A deferral returns it at
// the HEAD (queue.OutcomeDefer); the inbox stopping until the seat host's next
// renew is right, since what reaches here is a failure to RECORD the answer, a
// store failure under which no turn could run either.
func TestAnAnswerAParkedRunIsStillOwedIsHandedBackRatherThanRun(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions {
		// FREE, with a question open on it: the shape a parked run leaves.
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, SandboxAwaitsAnswer: true}
	}
	d.Answer = func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
		return sandbox.AnswerDeferred, sandbox.ErrResumeUnavailable
	}

	answer := inThread("notification", "chat:C1")
	got := d.Dispatch(context.Background(), "swe", []*events.Event{answer})

	if got.Outcome != queue.OutcomeDefer {
		t.Errorf("outcome = %v, want a deferral: the one return that keeps the "+
			"answer ahead of the person's next message", got.Outcome)
	}
	if !strings.Contains(got.Reason, sandbox.ErrResumeUnavailable.Error()) {
		t.Errorf("the deferral carried %q, want the coordinator's own failure: the "+
			"queue logs this one", got.Reason)
	}
	if len(r.reqs) != 0 {
		t.Errorf("a turn ran on an answer a parked executor is still owed: %d", len(r.reqs))
	}
	if len(r.parked) != 0 {
		t.Fatalf("the answer was republished as well as handed back (%v): a "+
			"requeue leaves a second copy for the redelivery to meet", r.parked)
	}
}

// THE HEADROOM IS WHAT THE MESSAGE CARRIES, not what this process remembers.
//
// Every hand-back spends one of the message's deliveries, so offering until the
// last would hand the final one back and the broker would dead-letter a
// person's reply. The coordinator's own ceiling cannot prevent that: it is per
// node and per process, and resets on a seat handoff or a restart, which do NOT
// reset the broker's count. The boundary is the assertion: one delivery either
// side of the reserve decides whether a parked run is offered at all, and a
// node whose transport stated no count must not stop answering parked runs.
func TestADeliveryInsideTheReserveIsNotOfferedToAParkedRun(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		left      int
		stated    bool
		wantOffer bool
	}{
		"one delivery outside the reserve": {left: sandbox.AnswerDeliveryReserve + 1, stated: true, wantOffer: true},
		"at the reserve":                   {left: sandbox.AnswerDeliveryReserve, stated: true, wantOffer: false},
		"one delivery left":                {left: 1, stated: true, wantOffer: false},
		"none left at all":                 {left: 0, stated: true, wantOffer: false},
		"a full budget":                    {left: 24, stated: true, wantOffer: true},
		"no count stated":                  {stated: false, wantOffer: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := &recorder{result: turn.Result{Decision: phase.Done}}
			d := dispatcher(t, r)
			d.Conditions = func(string) inbox.Conditions {
				// FREE, with a question open on it: what a parked run
				// leaves.
				return inbox.Conditions{Owned: true, TurnEngineReady: true,
					AdmitsTriggers: true, SandboxAwaitsAnswer: true}
			}
			var offers int
			d.Answer = func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
				offers++
				return sandbox.AnswerDeferred, sandbox.ErrResumeUnavailable
			}

			reply := inThread("notification", "chat:C1")
			ctx := context.Background()
			if tc.stated {
				ctx = headroom(ctx, map[*events.Event]int{reply: tc.left})
			}
			got := d.Dispatch(ctx, "swe", []*events.Event{reply})

			if tc.wantOffer {
				if offers != 1 {
					t.Fatalf("the delivery was offered %d times, want once: it has "+
						"deliveries to spare, so a run still owed it must be asked", offers)
				}
				if got.Outcome != queue.OutcomeDefer {
					t.Errorf("outcome = %v, want a deferral: the run is still owed this answer",
						got.Outcome)
				}
				if len(r.reqs) != 0 {
					t.Errorf("a turn ran on an answer a parked executor is still owed")
				}
				return
			}
			if offers != 0 {
				t.Fatalf("the delivery was offered to a parked run with %d deliveries "+
					"left: handing it back again risks the broker dead-lettering the "+
					"reply, which is the one ending this route must never take", tc.left)
			}
			if got.Outcome != queue.OutcomeAck {
				t.Errorf("outcome = %v, want an ack: what is left of this message "+
					"belongs to the ordinary route now", got.Outcome)
			}
			if len(r.reqs) != 1 {
				t.Fatalf("the ordinary turn ran %d times, want once: the message was "+
					"neither offered nor worked, so nothing at all happened to it",
					len(r.reqs))
			}
		})
	}
}

// AND NO NUMBER OF HANDOFFS SPENDS A REPLY'S LAST DELIVERIES.
//
// The property the reserve exists for, driven: walk one message down from a
// full budget, one hand-back per delivery, with a coordinator that defers
// every time (a resume failing alike on every node, each handoff starting a
// fresh per-process count). The loop must end at the reserve, leaving the
// ordinary route exactly the deliveries it was promised.
func TestNoNumberOfHandoffsSpendsAReplysLastDeliveries(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, SandboxAwaitsAnswer: true}
	}
	d.Answer = func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
		// EVERY node fails the same way, each with its own attempt
		// count.
		return sandbox.AnswerDeferred, sandbox.ErrResumeUnavailable
	}

	answer := inThread("notification", "chat:C1")
	left := 24 // a message on its first delivery of a 25-delivery budget
	for {
		got := d.Dispatch(headroom(context.Background(), map[*events.Event]int{answer: left}),
			"swe", []*events.Event{answer})
		if got.Outcome != queue.OutcomeDefer {
			break
		}
		// The hand-back spends one, whatever it meant.
		left--
		if left < 0 {
			t.Fatal("the answer route handed one reply back through its whole " +
				"delivery budget: the next return dead-letters the person's reply")
		}
	}
	if left != sandbox.AnswerDeliveryReserve {
		t.Errorf("the ordinary route was left %d deliveries, want the reserve of %d",
			left, sandbox.AnswerDeliveryReserve)
	}
	if len(r.reqs) != 1 {
		t.Errorf("the ordinary turn ran %d times once the offer stopped, want once",
			len(r.reqs))
	}
}

// AND THE PER-PROCESS CLAUSE STILL BOUNDS ONE PROCESS.
//
// The message-carried count holds across handoffs; [sandbox.MaxAnswerAttempts]
// still bounds one node, or a delivery with a whole budget would circle one
// seat's inbox for as long as the resume kept failing. So the coordinator
// spends its own budget with headroom to spare, and the dispatcher lets the
// message go when IT says so, not when the broker would.
func TestThePerProcessAttemptBudgetStillEndsTheHandBack(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, SandboxAwaitsAnswer: true}
	}
	// The coordinator's own bound, as this seam reports it: deferred until
	// the attempts are spent, then the ordinary message it looks like.
	var offers int
	d.Answer = func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
		offers++
		if offers < sandbox.MaxAnswerAttempts {
			return sandbox.AnswerDeferred, sandbox.ErrResumeUnavailable
		}
		return sandbox.AnswerNotMine, sandbox.ErrResumeUnavailable
	}

	answer := inThread("notification", "chat:C1")
	// A full budget throughout, so only the per-process clause can end
	// this: the reserve is never approached.
	ctx := headroom(context.Background(), map[*events.Event]int{answer: 24})
	var got queue.Result
	for range sandbox.MaxAnswerAttempts {
		got = d.Dispatch(ctx, "swe", []*events.Event{answer})
		if got.Outcome != queue.OutcomeDefer {
			break
		}
	}
	if offers != sandbox.MaxAnswerAttempts {
		t.Fatalf("the run was offered the message %d times, want %d: the "+
			"per-process ceiling is what ends a resume that fails the same way "+
			"on one node", offers, sandbox.MaxAnswerAttempts)
	}
	if got.Outcome != queue.OutcomeAck {
		t.Errorf("outcome = %v, want an ack once the attempts are spent", got.Outcome)
	}
	if len(r.reqs) != 1 {
		t.Errorf("the ordinary turn ran %d times, want once", len(r.reqs))
	}
}

// AND A SPENT SIBLING DOES NOT SPEAK FOR A FRESH REPLY.
//
// The partition's headroom is the smallest of its messages', and this gate
// must not read it: a conversation whose earlier message was handed back into
// the reserve keeps collecting replies, each on its FIRST delivery, and gated
// on the partition's number every one is refused the answer route and consumed
// by an ordinary turn while a run is still parked on the question. One message
// with deliveries to spare is enough: see [sandbox.MayOfferAnswer] for why the
// co-partitioned message is not saved by a refusal.
func TestAFreshReplyIsOfferedBesideASpentSibling(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, SandboxAwaitsAnswer: true}
	}
	var offers int
	d.Answer = func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
		offers++
		return sandbox.AnswerDeferred, sandbox.ErrResumeUnavailable
	}

	// One conversation, two messages: the older handed back into the
	// reserve, the reply on its first delivery of a 25-delivery budget.
	spent := notifyStampedIn("chat:C1", "chat:C1")
	reply := notifyStampedIn("chat:C1", "chat:C1")
	ctx := headroom(context.Background(), map[*events.Event]int{
		spent: 1,
		reply: 24,
	})

	got := d.Dispatch(ctx, "swe", []*events.Event{spent, reply})

	if offers != 1 {
		t.Fatalf("the delivery was offered %d times, want once: the reply has a "+
			"whole budget in hand, and a parked run is still owed it however "+
			"little the message batched beside it has left", offers)
	}
	if got.Outcome != queue.OutcomeDefer {
		t.Errorf("outcome = %v, want a deferral: the run is still owed this answer", got.Outcome)
	}
	if len(r.reqs) != 0 {
		t.Errorf("a turn ran on an answer a parked executor is still owed: %d", len(r.reqs))
	}
}

// AND WHEN EVERY MESSAGE IS SPENT THE ROUTE STILL LETS GO.
//
// The fold is "any", not "always": a partition whose messages are ALL inside
// the reserve has nothing to offer with and belongs to the ordinary route.
// Without this, reading each message's own count would remove the bound.
func TestAPartitionWhoseMessagesAreAllSpentIsNotOffered(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, SandboxAwaitsAnswer: true}
	}
	var offers int
	d.Answer = func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
		offers++
		return sandbox.AnswerDeferred, sandbox.ErrResumeUnavailable
	}

	first := notifyStampedIn("chat:C1", "chat:C1")
	second := notifyStampedIn("chat:C1", "chat:C1")
	ctx := headroom(context.Background(), map[*events.Event]int{
		first:  sandbox.AnswerDeliveryReserve,
		second: 1,
	})

	got := d.Dispatch(ctx, "swe", []*events.Event{first, second})

	if offers != 0 {
		t.Fatalf("a delivery with nothing outside the reserve was offered %d times: "+
			"handing it back again risks the broker dead-lettering the reply", offers)
	}
	if got.Outcome != queue.OutcomeAck {
		t.Errorf("outcome = %v, want an ack: what is left of these messages belongs "+
			"to the ordinary route now", got.Outcome)
	}
	if len(r.reqs) != 1 {
		t.Errorf("the ordinary turn ran %d times, want once", len(r.reqs))
	}
}

// AND WHAT IS OFFERED IS WHAT SURVIVED THE LEDGER.
//
// The offer sits after the completion read, so a trigger this seat already
// worked is never spliced into an executor as its answer too. The hand-back
// still returns the whole delivery and loses nothing: the redelivery reads the
// same ledger and drops the worked trigger before offering again. It is the
// OFFER that is narrowed, not the hand-back.
func TestAnAnswerIsOfferedOnlyWhatTheLedgerLeft(t *testing.T) {
	t.Parallel()
	completions := ledgerstore.NewMemoryCompletions()
	worked := inThread("notification", "chat:C1")
	fresh := inThread("notification", "chat:C1")
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
	var offered *events.Event
	d.Answer = func(_ context.Context, _ string, reply sandbox.Reply) (sandbox.AnswerDisposition, error) {
		_, _, trigger := unpackReply(reply)
		offered = trigger
		return sandbox.AnswerDeferred, sandbox.ErrResumeUnavailable
	}

	if got := d.Dispatch(ctx, "swe", []*events.Event{worked, fresh}); got.Outcome != queue.OutcomeDefer {
		t.Errorf("outcome = %v, want a deferral", got.Outcome)
	}
	if offered != fresh {
		t.Fatalf("offered %v, want only the trigger the ledger had not worked", offered)
	}
	if len(r.parked) != 0 {
		t.Fatalf("the delivery was republished as well as handed back (%v)", r.parked)
	}
}

// AND A RUN THAT IS TERMINALLY GONE LETS ITS ANSWER BE A TURN.
//
// The coordinator matched a run and settled it (nothing to resume into, or a
// claim it could not give back, so it ended the run). Nothing comes back for
// this delivery: a requeue circles a run that no longer exists, and an ack
// swallows the person's message. It runs as the ordinary message it looks like.
//
// A DISPOSITION THIS BUILD CANNOT READ TAKES THE SAME PATH: nothing here makes
// one, so it means a seam that answered nothing, read as a node with no
// coordinator, which cannot loop. A requeue would circle unbounded, since the
// bound lives with the matched run and an unreadable answer names none.
func TestAnAnswerForARunThatIsGoneRunsItsOrdinaryTurn(t *testing.T) {
	t.Parallel()
	for name, disposition := range map[string]sandbox.AnswerDisposition{
		"a run that is terminally gone":          sandbox.AnswerNotMine,
		"a disposition this build does not know": sandbox.AnswerDisposition("something else"),
		"no disposition at all":                  sandbox.AnswerDisposition(""),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := &recorder{result: turn.Result{Decision: phase.Done}}
			d := dispatcher(t, r)
			d.Conditions = func(string) inbox.Conditions {
				return inbox.Conditions{Owned: true, TurnEngineReady: true,
					AdmitsTriggers: true, SandboxAwaitsAnswer: true}
			}
			d.Answer = func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
				return disposition, nil
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
				t.Errorf("the delivery was requeued with nothing to bound it: %v", r.parked)
			}
		})
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
	d.Answer = func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
		return sandbox.AnswerNotMine, nil
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
// The ordering under test: the offer sits AFTER the completion read, so a
// redelivered trigger that already produced a turn is not used a second time
// as an executor's answer, whose only disambiguation is "the next thing to
// arrive here".
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
	d.Answer = func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
		called = true
		return sandbox.AnswerConsumed, nil
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
// coordinator that errored, a disposition this build cannot read and a node
// with no coordinator each become the ordinary turn they look like: acking a
// message nothing handled loses it. The one exception, a delivery a run is
// still OWED, is TestAnAnswerAParkedRunIsStillOwedIsHandedBackRatherThanRun.
func TestADeliveryThatIsNotTheAnswerBecomesATurn(t *testing.T) {
	t.Parallel()
	type answerer func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error)
	for name, answer := range map[string]answerer{
		"not this run's answer": func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
			return sandbox.AnswerNotMine, nil
		},
		"an unreadable store": func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
			return sandbox.AnswerNotMine, errors.New("the coordination store is unreachable")
		},
		"a disposition this build does not know": func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
			return sandbox.AnswerDisposition("something else"), nil
		},
		"no coordinator": nil,
	} {
		r := &recorder{result: turn.Result{Decision: phase.Done}}
		d := dispatcher(t, r)
		d.Conditions = func(string) inbox.Conditions {
			return inbox.Conditions{Owned: true, TurnEngineReady: true,
				AdmitsTriggers: true, SandboxAwaitsAnswer: true}
		}
		d.Answer = answer

		got := d.Dispatch(context.Background(), "swe",
			[]*events.Event{inThread("notification", "chat:C1")})
		if got.Outcome != queue.OutcomeAck {
			t.Errorf("%s: outcome = %v, want an ack for a worked turn", name, got.Outcome)
		}
		if len(r.reqs) != 1 {
			t.Errorf("%s: %d turns ran, want the delivery worked as the message it is",
				name, len(r.reqs))
		}
	}
}

// AND A DISPATCH THAT COULD NOT BE MADE NAMES BOTH KEYS IT WAS CARRYING.
//
// Only the store knows which age of row the offer matches, so a line naming the
// identity alone cannot say what was compared with what (the keys differ for a
// top-level DM reply to a question parked from a thread). Not parallel: it
// swaps the process-wide logger.
func TestAFailedAnswerDispatchNamesBothKeys(t *testing.T) {
	logs := &logBuffer{}
	logging.Configure(slog.LevelWarn, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, SandboxAwaitsAnswer: true}
	}
	d.Answer = func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
		return sandbox.AnswerNotMine, errors.New("the coordination store is unreachable")
	}
	d.Dispatch(context.Background(), "swe",
		[]*events.Event{inThread("notification", "chat:D1")})

	records := logs.records(t, "sandbox_answer_dispatch_failed")
	if len(records) != 1 {
		t.Fatalf("%d lines recorded the failed dispatch, want one", len(records))
	}
	for key, want := range map[string]any{
		"conversation": "chat:D1", "partition": "chat:D1",
	} {
		if records[0][key] != want {
			t.Errorf("%s = %v, want %v — a reader cannot tell what was "+
				"compared against what", key, records[0][key], want)
		}
	}
}

// A partition with no conversation cannot be matched against a question asked
// in one, so it is worked without asking: the coordinator's own disambiguation
// is positional within a conversation, and offering it a key-less delivery
// would let a scheduled fire answer somebody's question.
func TestADeliveryWithNoConversationIsNotOfferedAsAnAnswer(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, SandboxAwaitsAnswer: true}
	}
	called := false
	d.Answer = func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
		called = true
		return sandbox.AnswerConsumed, nil
	}
	d.Dispatch(context.Background(), "swe", []*events.Event{ev("notification")})
	if called {
		t.Error("a delivery with no conversation was offered as an answer")
	}
	if len(r.reqs) != 1 {
		t.Errorf("it was not worked either (%d turns)", len(r.reqs))
	}
}

// AND NEVER FROM A PARK. A park is a node that cannot run the turn at all — no
// turn engine, a person's pause — and offering those deliveries to a
// coordinator would answer a question with a message the seat was never able
// to read.
func TestAParkNeverOffersItsDeliveryAsAnAnswer(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	d := dispatcher(t, r)
	d.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: false, AdmitsTriggers: true}
	}
	called := false
	d.Answer = func(context.Context, string, sandbox.Reply) (sandbox.AnswerDisposition, error) {
		called = true
		return sandbox.AnswerConsumed, nil
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

// ONE CONVERSATION, ONE NOTIFICATION: the whole point of coalescing.
//
// A partition's one turn is handed one notify.Coalesce digest, not the enriched
// bodies concatenated: those repeat the app's triage scaffolding, each copy
// pointing at staler state, and make separate asks a model answers separately.
// The scaffolding count catches it: a merged digest renders it ONCE.
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
	ask := engine.DescribeTrigger(r.reqs[0].Ask(), "")

	// EVERY constituent survives: a digest of only the latest hands the
	// seat a follow-up with no idea what it follows.
	for _, want := range []string{
		"Can you take the deploy today?",
		"Blocked on the migration — see PROJ-4.",
		"Never mind, I have it.",
	} {
		if !strings.Contains(ask, want) {
			t.Errorf("the digest lost %q\ngot:\n%s", want, ask)
		}
	}
	// AND THE SCAFFOLDING RENDERS ONCE: three concatenated bodies carry
	// three copies.
	if n := strings.Count(ask, "TRIAGE SCAFFOLDING"); n != 1 {
		t.Errorf("the third-party app scaffolding rendered %d times, want once\ngot:\n%s", n, ask)
	}
	// The digest says it IS one, so the seat answers once.
	if !strings.Contains(ask, "Coalesced updates") {
		t.Errorf("the merged ask does not tell the seat it is one piece of work:\n%s", ask)
	}

	// THE BOOKKEEPING STILL READS THE CONSTITUENTS: the digest is minted
	// fresh, so a ledger keyed on it matches nothing and re-runs the turn on
	// every redelivery.
	if n := len(r.reqs[0].Events); n != 3 {
		t.Errorf("the dispatch carries %d constituents, want all three", n)
	}
	if r.reqs[0].ConversationKey != "slack:C1" {
		t.Errorf("conversation = %q", r.reqs[0].ConversationKey)
	}
}

// A MERGE MUST NOT LAUNDER WHAT BOUNDS THE TURN.
//
// The merged event replaces the envelopes, so each fact that bounded the
// partition is carried onto it: the deepest delegation (or an ask beside a
// shallow mention resets the ping-pong guard's cap), the delivery obligation
// (or one direct ask in a burst of broadcasts becomes ignorable traffic), and
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
	// BOTH KEYS ride the merged envelope: from here on it IS the partition,
	// and a reader handed it must be able to ask either question.
	if got, _ := merged.Payload[notify.PartitionField].(string); got != "slack:C1" {
		t.Errorf("the merged ask carries partition key %q", got)
	}
	if got, _ := merged.Payload[notify.ConversationField].(string); got != "slack:C1" {
		t.Errorf("the merged ask carries conversation identity %q", got)
	}
	// And the obligation the turn engine enforces is derived from the
	// constituents, including WHERE it is owed, which a merge must not
	// launder either.
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

	// A constituent with no typed body: an event whose payload this build
	// cannot decode.
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

// A SKIPPED TRIGGER IS ON THE RECORD, as a types.TurnTriggerSkipped.
//
// A turn finished and shipped its effects, and the delivery came back (from a
// node that died before acking, or a drain that outlasted the ack window).
// Without a record the feed shows the arrivals and one turn, and cannot tell
// "the agent never answered" from "it already answered, on a node since dead":
// the same observation and opposite problems.
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
// Its constituent list is the one place a conversation's senders, per-message
// bodies and recon flags are combined, so the prefetch and the learning
// workers read it rather than re-deriving them from the partition: two answers
// to one question are free to disagree.
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
	// IN THE ORDER THEY SPOKE. The flat Sender mirrors the LATEST
	// constituent, so a reader that also took it puts the last speaker at
	// the head of a list whose whole contract is the order.
	if !slices.Equal(spoke, []string{"ana", "bo", "cy"}) {
		t.Errorf("the merged ask's speakers are %v", spoke)
	}
	if note.Sender != "cy" {
		t.Errorf("the merged ask's flat sender = %q, want the latest constituent's", note.Sender)
	}
}

// THE SAME TRIGGER, TWICE, IS TWO RUNS OF ONE UNIT OF WORK.
//
// A NAK redelivers the very same event, so the work key (derived from the
// trigger's ids) is reproduced by design. The run must not be: everything a
// turn publishes is keyed on it, so a retry under the first attempt's id writes
// its phases, live row and executor row over that attempt's (ADR-0017).
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

// AND THE INSTANT THE WORK BEGAN IS REPRODUCED WITH THE KEY, half of the same
// identity: every operation id a turn derives from the key carries it as its
// mint instant, and the state log refuses to decide again an operation minted
// before its node adopted a donated snapshot. Taken from the dispatch's own
// clock or run, a re-run after an adoption would read as a new operation.
func TestARedeliveredTriggerReproducesWhenItsWorkBegan(t *testing.T) {
	t.Parallel()
	r := &recorder{result: turn.Result{Decision: phase.Done, Artifact: "posted"}}
	d := dispatcher(t, r)
	trigger := ev("notification")
	trigger.Timestamp = time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)

	d.Dispatch(context.Background(), "ceo", []*events.Event{trigger})
	d.Dispatch(context.Background(), "ceo", []*events.Event{trigger})

	if len(r.reqs) != 2 {
		t.Fatalf("the turn engine ran %d times, want 2", len(r.reqs))
	}
	for i, req := range r.reqs {
		if !req.WorkSince.Equal(trigger.Timestamp) {
			t.Errorf("run %d began its work at %s, want the trigger's own %s — "+
				"the instant a write derived from the key carries has to be one "+
				"every redelivery reproduces", i+1, req.WorkSince, trigger.Timestamp)
		}
		// AND A RUN'S OWN ID CARRIES WHEN IT STARTED: the instant a write
		// seeded from the run rather than a key carries.
		if _, ok := statelog.OpMintedAt(req.RunID); !ok {
			t.Errorf("run id %q carries no instant — a write seeded from it "+
				"would read as older than every adoption", req.RunID)
		}
	}
}

// A DIRECT MESSAGE'S TWO TURNS SERVE ONE CONVERSATION, stated end to end
// inside the dispatcher.
//
// Turn one is a burst of top-level DMs, one partition on the bare channel.
// Turn two is the person's reply in the thread the agent opened: a DIFFERENT
// partition (it must not merge with unrelated top-level pings) and the SAME
// conversation, so it reads turn one as the conversation's head, continues its
// session and is told how it ended.
//
// Mutation: have conversationIdentityOf answer partitionKeyOf's value and the
// reply serves "chat:D1:root-1", which turns this red.
func TestADirectMessagesThreadReplyServesTheBurstsConversation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	burst := &recorder{result: turn.Result{
		Decision: phase.Done, Delivered: true, Artifact: "answered the DM",
	}}
	d := dispatcher(t, burst)
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
	if got := d2.Dispatch(ctx, "ceo", []*events.Event{
		inDirectThread("notification", "chat:D1", "root-1"),
	}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}
	if len(reply.reqs) != 1 {
		t.Fatalf("the reply ran %d turns", len(reply.reqs))
	}
	if reply.reqs[0].ConversationKey != "chat:D1" {
		t.Fatalf("the reply served %q, want the DM line the burst used — the "+
			"seat's own answer of a moment earlier would be a conversation its "+
			"next turn never reads", reply.reqs[0].ConversationKey)
	}
}

// A RUN PARKED FROM A TOP-LEVEL DM IS ANSWERED BY THE PERSON'S THREADED REPLY:
// the dispatcher offers the coordinator the WHOLE DM LINE, the value the row
// was parked under. A top-level DM partitions on the bare channel, so its run
// parks under the channel, and [notify.ChatPrompt] tells the seat to reply AS A
// THREAD, so the answer arrives partitioned on that thread: offered the
// partition, the coordinator could never match it. The partition still
// travels, to tell two runs parked on one DM line apart
// ([sandbox.ConversationRef.Best]), and the ordinary turn serves the identity.
//
// Mutation: offer the coordinator the partition as the identity and the first
// half goes red; serve the partition and the second does.
func TestADMThreadReplyAnswersAndServesTheWholeDMLine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var offered sandbox.ConversationRef
	parked := &recorder{}
	dp := dispatcher(t, parked)
	dp.Conditions = func(string) inbox.Conditions {
		return inbox.Conditions{Owned: true, TurnEngineReady: true,
			AdmitsTriggers: true, SandboxAwaitsAnswer: true}
	}
	dp.Answer = func(_ context.Context, _ string, reply sandbox.Reply) (sandbox.AnswerDisposition, error) {
		conv, _, _ := unpackReply(reply)
		offered = conv
		return sandbox.AnswerConsumed, nil
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
		t.Errorf("the partition did not travel beside it (%q), so two runs "+
			"parked from two threads of one DM cannot be told apart",
			offered.Partition)
	}

	// THE SAME TRIGGER through an ordinary dispatch: the turn serves the DM
	// line rather than the thread the reply landed in.
	r := &recorder{result: turn.Result{
		Decision: phase.Done, Delivered: true, Artifact: "done",
	}}
	d := dispatcher(t, r)
	if got := d.Dispatch(ctx, "ceo", []*events.Event{
		inDirectThread("notification", "chat:D1", "root-1"),
	}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("outcome = %v", got.Outcome)
	}
	if len(r.reqs) != 1 || r.reqs[0].ConversationKey != "chat:D1" {
		t.Fatalf("the turn served conversation %+v, want the whole DM line", r.reqs)
	}
}

// AN ANSWER THE STORE COULD NOT PLACE IS HANDED BACK, NOT SPENT.
//
// A failed lookup means the coordinator could not establish that nothing is
// owed the delivery, and a retry seconds later may match the run that asked.
// AND THE FAIL-OPEN IS STILL RIGHT on a seat with no parked run, where an
// unreadable store must not swallow every ordinary message. The two resolve on
// the seat's own awaiting count, held in this node's memory with no store read.
//
// Driven with the REAL coordinator, because the fact under test is the pair:
// what it concludes from an unreadable lookup, and what the dispatcher does.
func TestAnAnswerTheStoreCouldNotPlaceIsHandedBackRatherThanRun(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		awaiting bool
		outcome  queue.Outcome
		turns    int
	}{
		// Something on this seat IS waiting for a reply and only WHICH row
		// could not be read: the delivery comes back.
		"a run is awaiting an answer on this seat": {
			awaiting: true, outcome: queue.OutcomeDefer, turns: 0,
		},
		// Nothing is owed the delivery, so a hand-back has nothing to
		// come back to: it is the ordinary message it looks like.
		"no run is awaiting anything here": {
			awaiting: false, outcome: queue.OutcomeAck, turns: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := &recorder{result: turn.Result{Decision: phase.Done}}
			d := dispatcher(t, r)
			d.Conditions = func(string) inbox.Conditions {
				// FREE, with a question open on it: what a parked run
				// leaves, and how this seat is screened whether or not the
				// store can still say which run.
				return inbox.Conditions{Owned: true, TurnEngineReady: true,
					AdmitsTriggers: true, SandboxAwaitsAnswer: true}
			}
			d.Answer = unreadableAnswerLookup(t, tc.awaiting).TryResumeFromAnswer

			got := d.Dispatch(context.Background(), "swe",
				[]*events.Event{inThread("notification", "chat:C1")})

			if got.Outcome != tc.outcome {
				t.Errorf("outcome = %v, want %v", got.Outcome, tc.outcome)
			}
			if len(r.reqs) != tc.turns {
				t.Errorf("%d turns ran, want %d: a reply a parked run is owed "+
					"must not be worked as an unrelated message, and one no run "+
					"is owed must not be held back for ever", len(r.reqs), tc.turns)
			}
		})
	}
}

// unreadableAnswerLookup is a real coordinator whose store cannot answer which
// of a seat's runs is awaiting a reply — optionally with one that is.
func unreadableAnswerLookup(t *testing.T, awaiting bool) *sandbox.Coordinator {
	t.Helper()
	ctx := context.Background()
	store := sandbox.NewCoordStore(coordmemory.NewFleet())
	blind := &atomic.Bool{}
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: sandbox.NewFakeProvider()},
		Runners:   map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Pushes:   noPushes{},
		Answers:  fleetAnswers(),
		Audience: noAudience{},
		Queue:    discardPublisher{}, Pending: blindLookupStore{PendingStore: store, blind: blind}, Manager: manager,
		// A resumer that fails the test: the lookup fails first, and a
		// resume here would run the answer after the store said it could
		// not tell which run is owed it.
		Resume: unreachedResumer{t},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	if !awaiting {
		blind.Store(true)
		return coordinator
	}
	run := sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", AgentID: "a-1", Role: "SWE",
		Harness: "claude-code", ConversationKey: "chat:C1", PartitionKey: "chat:C1",
	}
	if _, err := store.BeginLaunch(ctx, run, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	sandboxtest.Ask(t, store, "t1", sandboxtest.Question("which branch?", sandbox.AskToRequester))
	if err := store.MarkAwaiting(ctx, "t1", sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	// The seat's counts come from the store ONCE, here, as a node claiming
	// the seat seeds them: from now on they are this process's memory,
	// which still answers when the lookup cannot.
	if err := coordinator.OnStarted(ctx, types.ExecutorLaunched{
		Agent: "a-1", AgentHandle: "swe", TurnID: "t1",
	}); err != nil {
		t.Fatalf("OnStarted: %v", err)
	}
	if _, awaits := coordinator.SeatRuns("swe"); !awaits {
		t.Fatal("the seat does not report the run parked on its question")
	}
	blind.Store(true)
	return coordinator
}

// blindLookupStore is a store that works except for the one read the answer
// match turns on — the seat's run listing — once blind is set: the seat's
// counts are seeded from that same read first, as a node claiming the seat
// seeds them.
type blindLookupStore struct {
	sandbox.PendingStore
	blind *atomic.Bool
}

func (s blindLookupStore) ListActiveForSeat(ctx context.Context, handle string) ([]sandbox.PendingRun, error) {
	if s.blind.Load() {
		return nil, errors.New("the coordination store refused the call")
	}
	return s.PendingStore.ListActiveForSeat(ctx, handle)
}

// discardPublisher stands in for the queue: this case publishes nothing.
type discardPublisher struct{}

func (discardPublisher) Publish(context.Context, string, *events.Event) error { return nil }

// unreachedResumer stands in the seam a coordinator requires on a path that
// must never get there — a stub that answered quietly would let a resume
// through as a pass.
type unreachedResumer struct{ t *testing.T }

func (r unreachedResumer) Resume(context.Context, sandbox.ResumeRequest) error {
	r.t.Helper()
	r.t.Fatal("the answer was resumed although the lookup that matches it to a " +
		"run could not be made")
	return nil
}

// noAudience resolves every question to nobody: these coordinators are about
// the answer route, not whom a question is put to (audience_internal_test.go).
type noAudience struct{}

func (noAudience) ResolveAudience(sandbox.PendingRun, string) sandbox.Audience {
	return sandbox.Audience{}
}

// MayAnswer lets every seat's holder answer: these coordinators are about the
// answer route, not whether its writer may act.
func (noAudience) MayAnswer(context.Context, string) (bool, error) { return true, nil }

// noPushes reads every run as having pushed nothing: these coordinators run no
// repository on Gitlet (runpushes_internal_test.go reads the record).
type noPushes struct{}

func (noPushes) Pushed(context.Context, sandbox.PendingRun) (sandbox.PushRecord, error) {
	return sandbox.PushRecord{}, nil
}

func (noPushes) Changes(_ context.Context, _ sandbox.PendingRun, record sandbox.PushRecord,
	_ bool) ([]sandbox.PushedRef, []sandbox.OpenedChange, error) {
	return record.Refs, nil, nil
}

// fleetAnswers is the seal these coordinators seal and open answers under
// ([sandbox.CoordinatorOptions.Answers]): one fleet keyring for every one of
// them, as a fleet shares one, so a node opens what another recorded.
var fleetAnswers = sync.OnceValue(func() *sandbox.AnswerSeal {
	seal, err := sandbox.NewAnswerSeal(recordseal.OneKey("answers", "the engine cases' fleet key material"))
	if err != nil {
		panic(err)
	}
	return seal
})

// unpackReply splits a [sandbox.Reply] into the conversation, the text and the
// delivery's leading event.
func unpackReply(reply sandbox.Reply) (sandbox.ConversationRef, string, *events.Event) {
	var trigger *events.Event
	if len(reply.Events) > 0 {
		trigger = reply.Events[0]
	}
	return reply.Conv, reply.Text, trigger
}
