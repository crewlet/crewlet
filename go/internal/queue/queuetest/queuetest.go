// Package queuetest is the conformance suite that certifies an EventQueue
// backend.
//
// One suite, every backend. The in-memory twin, embedded JetStream and Pulsar
// all have to answer the same questions, because everything above
// internal/queue is forbidden to know which one is running — so the only place
// a backend difference can be caught is here. A backend this suite has not
// certified does not exist as far as the engine is concerned.
//
// The suite is written for an ASYNCHRONOUS backend even though the in-memory
// twin dispatches inline: every positive assertion waits for the handler to
// speak (a channel wake, not a poll), and every negative assertion holds a
// quiet window open. An assertion that reads state the instant after Publish
// returns would pass on the twin and fail on a real broker, which is precisely
// the divergence this suite exists to prevent.
//
// Subtests are named after the Python suite they are ported from
// (tests/test_queue/test_protocol.py), so a failure here names the spec that
// describes it.
package queuetest

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// Capabilities describes what a backend can additionally be asked, beyond the
// EventQueue contract itself.
//
// Several of the properties this suite certifies are about state the contract
// deliberately does not expose — the mail a subscription retains, the reasons
// holding an attachment paused, which seats a node is serving. Those are the
// properties seat ownership rests on, so they must be asserted; they are just
// not things a producer or consumer asks the queue. A backend supplies them
// here, and a subtest whose capability is absent skips with a named reason
// rather than silently passing.
//
// The zero value is valid: it runs everything that needs nothing but the
// contract.
type Capabilities struct {
	// Peer returns a second, UNSTARTED client on the same broker as q —
	// another node in the fleet. Absent when a backend cannot model a
	// fleet in one process.
	Peer func(t *testing.T, q queue.EventQueue) queue.EventQueue

	// WithDeliveryAttempts returns a fresh UNSTARTED queue configured so
	// that a persistently failing event is handed to a handler exactly
	// attempts times in total before it is dead-lettered. The dead-letter
	// subtests need a small number; a production default would mean dozens
	// of handler invocations per assertion.
	//
	// It asks for TOTAL ATTEMPTS — an observable — rather than for a
	// "budget", because a budget is not one number and the contract never
	// says which one it is. Pulsar and the Python twin count redeliveries
	// AFTER the first delivery (10 means 11 attempts); NATS MaxDeliver
	// counts deliveries INCLUDING the first (25 means 25). Both numbers
	// live in this repo, in different documents, meaning different things.
	//
	// The earlier form of this field named a budget and defined the
	// convention in this comment, which made every backend that passed
	// agree with the suite because the suite told it to — the agreement was
	// real code and read exactly like evidence for a convention nothing had
	// decided. Naming the observable lets each backend translate from
	// whatever its broker counts, and leaves the suite asserting only what
	// it can actually see.
	WithDeliveryAttempts func(t *testing.T, attempts int) queue.EventQueue

	// WithRedeliveryBudget is the superseded form of WithDeliveryAttempts,
	// counting redeliveries after the first delivery. Kept so a backend
	// that already supplies it keeps being certified; set
	// WithDeliveryAttempts instead and this can go.
	WithRedeliveryBudget func(t *testing.T, budget int) queue.EventQueue

	// Backlog reports the events a subscription retains and has not
	// delivered — the mail an unowned seat is holding.
	Backlog func(q queue.EventQueue, topic, group string) []*events.Event

	// DeadLetters reports the events a subscription gave up on.
	DeadLetters func(q queue.EventQueue, topic, group string) []*events.Event

	// Attachments reports every (topic, group) pair THIS client is
	// attached to. Scoped to the client, never the broker: "attached to
	// exactly the seats I own" is the assertion that catches a
	// double-consumer split-brain, and a fleet-wide answer cannot make it.
	Attachments func(q queue.EventQueue) [][2]string

	// PauseHolds reports the reasons currently holding this client's
	// attachment paused.
	PauseHolds func(q queue.EventQueue, topic, group string) []string

	// Quiescing reports whether this client has stopped taking work on a
	// subscription.
	//
	// Distinct from a pause hold and separately observable because the two
	// are cleared by different things — a hold by the subsystem that took
	// it, a quiesce by detaching or attaching again — and because a stale
	// quiesce is invisible from outside until someone attaches, which is
	// what let one sit unnoticed long enough to strand a seat.
	Quiescing func(q queue.EventQueue, topic, group string) bool

	// History reports every event published through this backend, for the
	// one assertion that has to distinguish "not delivered" from "not
	// accepted".
	History func(q queue.EventQueue) []*events.Event

	// InlineDispatch declares that Publish does not return until the
	// events it could reach have been dispatched. It unlocks the
	// assertions that pin down exact batch boundaries, which only a
	// backend with no fetch latency can promise.
	InlineDispatch bool

	// StrictRoundRobin declares that competing consumers are served in
	// strict rotation. Without it the suite still requires that each
	// event reaches exactly one member and that the load is shared, which
	// is the part every broker owes.
	StrictRoundRobin bool

	// FreeDeferral declares that a deferral costs the message nothing —
	// it returns unacked with no redelivery accrued, so its dead-letter
	// budget is whole afterwards.
	//
	// A capability rather than a requirement, for the same measured reason
	// as HeadReplayOnNak and from the same decision. On Pulsar a graceful
	// close returns unacked messages at redeliveryCount 0, so a seat
	// handoff is free; on JetStream nothing is released by closing, so
	// deferral is implemented with Nak() and costs one delivery count —
	// and MaxDeliver was re-derived from 10 to 25 precisely to absorb
	// handoffs (rewrite/decisions/102-jetstream-redelivery.md, decisions 1
	// and 2).
	//
	// The invariant every backend still owes is the one the contract
	// states: a deferral must not cause a HEALTHY event to die. A backend
	// that spends a count per handoff satisfies it by sizing the budget so
	// handoffs cannot exhaust it, not by making the handoff free. This flag
	// only asks a backend that IS free to stay free.
	FreeDeferral bool

	// HeadReplayOnNak declares that a negatively acknowledged event
	// returns to the FRONT of the mailbox, ahead of events already queued
	// behind it.
	//
	// A capability rather than a requirement, and deliberately so:
	// measured, Pulsar replays from the head while JetStream returns a
	// redelivered message BEHIND never-delivered ones
	// (rewrite/decisions/102-jetstream-redelivery.md). The engine no
	// longer depends on either — within-conversation order comes from
	// event timestamps, which
	// within_a_partition_events_are_ordered_by_timestamp certifies for
	// every backend. This flag only asks a backend that DOES replay from
	// the head to keep doing it, so the property cannot rot unnoticed on
	// the twin the fleet suite runs against.
	HeadReplayOnNak bool

	// RejectsPublishBeforeStart declares that Publish on an unstarted or
	// stopped queue returns an error rather than silently accepting.
	RejectsPublishBeforeStart bool

	// Restartable declares that Start on a stopped queue re-establishes it
	// and delivery resumes.
	//
	// A capability rather than a requirement because the contract does not
	// say. Start "connects the backend and begins consuming" and Stop
	// "closes the connection", which reads as restartable — but delivery
	// pause is documented one-way ("once paused, the engine is shutting
	// down"), so a backend may reasonably treat Stop as terminal and
	// require a fresh queue. Two backends already answer differently. See
	// rewrite/questions/queue-contract-restart-after-stop.md; until that is
	// settled the suite must not render a verdict on it.
	Restartable bool
}

// Run executes the full conformance suite against the backend produced by
// newQueue, with no backend-specific capabilities declared.
//
// newQueue must return a FRESH, UNSTARTED queue on every call: the suite owns
// the lifecycle, because start/stop ordering is itself part of the contract.
func Run(t *testing.T, newQueue func(t *testing.T) queue.EventQueue) {
	RunWith(t, newQueue, Capabilities{})
}

// RunWith is Run with the backend's capabilities filled in. A backend that can
// answer more gets certified on more; nothing it cannot answer fails.
func RunWith(t *testing.T, newQueue func(t *testing.T) queue.EventQueue, caps Capabilities) {
	t.Helper()
	s := &suite{newQueue: newQueue, caps: caps}
	t.Run("EventQueue", s.runCore)
	t.Run("Wire", s.runWire)
	t.Run("Attachment", s.runAttachment)
	t.Run("Stream", s.runStream)
	t.Run("Batch", s.runBatch)
	t.Run("Fleet", s.runFleet)
	// A "no" has two halves: the answer and the write that must not
	// happen. See runNegativePaths.
	t.Run("NegativePaths", s.runNegativePaths)
	// Named for what it is: shared contract functions, not backend
	// behaviour. See runContractPolicy for the scope this group does and
	// does not cover.
	t.Run("ContractPolicyFunctions", s.runContractPolicy)
}

type suite struct {
	newQueue func(t *testing.T) queue.EventQueue
	caps     Capabilities
}

// start returns a started queue whose Stop is already registered as cleanup.
func (s *suite) start(t *testing.T) queue.EventQueue {
	t.Helper()
	return startQueue(t, s.newQueue(t))
}

func startQueue(t *testing.T, q queue.EventQueue) queue.EventQueue {
	t.Helper()
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	return q
}

// --- timing budgets -------------------------------------------------------

const (
	// settleFor bounds how long a positive assertion waits for a backend
	// to deliver. Generous on purpose: a timeout here must mean "never
	// delivered", never "delivered on a loaded CI box a moment late".
	//
	// It is also an IMPLICIT CONTRACT on backends, stated here because
	// nothing else states it: a backend whose delivery latency can exceed
	// this fails the suite with no hint that a constant is the reason. A
	// backend that needs longer should say so rather than have this raised
	// — a queue that takes more than three seconds to hand over an event
	// already waiting is a finding, not a tuning problem.
	settleFor = 3 * time.Second

	// quietFor is how long a negative assertion — "this must NOT be
	// delivered" — holds the window open. A paused attachment that leaks
	// tends to leak immediately, so this only has to outlast one dispatch
	// cycle.
	quietFor = 150 * time.Millisecond

	// lingerFor is the batch linger window the batching subtests use. It
	// has to be long enough that several publishes land inside one window
	// on a loaded machine, and short enough that a test which waits out
	// several windows stays quick.
	//
	// The suite never asks for a linger above queue.MaxLingerSeconds, and
	// must not: backends size their dispatch budget to that ceiling and are
	// right to refuse a window they cannot honour rather than silently
	// clamping it. A case needing a longer window to make something
	// observable should find another way to observe it.
	lingerFor = 50 * time.Millisecond
)

// --- observing handlers ---------------------------------------------------

// journal records what handlers saw and wakes a waiter on every record.
//
// Waking on a channel rather than polling a counter is not only idiom here: a
// poll interval silently sets a floor on how fast a test can observe a
// delivery, and these assertions are about ordering between deliveries.
type journal struct {
	mu   sync.Mutex
	seen []string
	ping chan struct{}
}

func newJournal() *journal {
	return &journal{ping: make(chan struct{}, 1)}
}

func (j *journal) record(label string) {
	j.mu.Lock()
	j.seen = append(j.seen, label)
	j.mu.Unlock()
	j.wake()
}

func (j *journal) wake() {
	select {
	case j.ping <- struct{}{}:
	default:
	}
}

func (j *journal) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.seen...)
}

func (j *journal) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.seen)
}

// await blocks until cond accepts what has been recorded, failing the test
// with the last observation if the settle budget runs out.
func (j *journal) await(t *testing.T, what string, cond func(seen []string) bool) {
	t.Helper()
	deadline := time.NewTimer(settleFor)
	defer deadline.Stop()
	for {
		if cond(j.all()) {
			return
		}
		select {
		case <-j.ping:
		case <-deadline.C:
			if cond(j.all()) {
				return
			}
			t.Fatalf("timed out waiting for %s; handlers saw %v", what, j.all())
		}
	}
}

// awaitLabels waits for exactly this sequence of labels, in order.
func (j *journal) awaitLabels(t *testing.T, what string, want ...string) {
	t.Helper()
	j.await(t, what, func(seen []string) bool { return equalStrings(seen, want) })
}

// staysAt holds a quiet window open and fails if anything more is delivered.
// The negative half of the contract — "a paused attachment delivers nothing" —
// has no completion signal to wait on, so it is asserted over elapsed time.
//
// It bounds a SHORT window on purpose, and deliberately does not claim that no
// duplicate ever arrives later: the engine promises bounded duplication, not
// exactly-once, so a redelivery after an ack timeout is permitted behaviour and
// must not read as a conformance failure. What this catches is the thing that
// leaks immediately — a gate that never closed, or an ack that did not stop a
// retry — which is the failure mode worth a test.
func (j *journal) staysAt(t *testing.T, want int, what string) {
	t.Helper()
	time.Sleep(quietFor)
	if got := j.all(); len(got) != want {
		t.Fatalf("%s: expected delivery to stay at %d, saw %v", what, want, got)
	}
}

// batchJournal is journal for batch handlers: it keeps each batch whole,
// because the shape of the batches IS what the batching contract promises.
type batchJournal struct {
	mu      sync.Mutex
	batches [][]string
	ping    chan struct{}
}

func newBatchJournal() *batchJournal {
	return &batchJournal{ping: make(chan struct{}, 1)}
}

func (b *batchJournal) record(evs []*events.Event) {
	b.mu.Lock()
	b.batches = append(b.batches, labelsOf(evs))
	b.mu.Unlock()
	select {
	case b.ping <- struct{}{}:
	default:
	}
}

func (b *batchJournal) all() [][]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([][]string, len(b.batches))
	copy(out, b.batches)
	return out
}

func (b *batchJournal) sizes() []int {
	got := b.all()
	out := make([]int, len(got))
	for i, batch := range got {
		out[i] = len(batch)
	}
	return out
}

func (b *batchJournal) await(t *testing.T, what string, cond func(batches [][]string) bool) {
	t.Helper()
	deadline := time.NewTimer(settleFor)
	defer deadline.Stop()
	for {
		if cond(b.all()) {
			return
		}
		select {
		case <-b.ping:
		case <-deadline.C:
			if cond(b.all()) {
				return
			}
			t.Fatalf("timed out waiting for %s; handlers saw %v", what, b.all())
		}
	}
}

func (b *batchJournal) awaitSizes(t *testing.T, what string, want ...int) {
	t.Helper()
	b.await(t, what, func([][]string) bool { return equalInts(b.sizes(), want) })
}

func (b *batchJournal) staysAt(t *testing.T, want int, what string) {
	t.Helper()
	time.Sleep(quietFor)
	if got := b.all(); len(got) != want {
		t.Fatalf("%s: expected batch count to stay at %d, saw %v", what, want, got)
	}
}

// --- state assertions -----------------------------------------------------

// awaitSignal waits for a channel to close, failing the test rather than
// hanging its binary.
//
// Every unbounded `<-ch` in a shared suite is a ten-minute timeout and a
// goroutine dump for whichever backend author trips it, on a day they were
// debugging something else. rescue runs first on the failure path, to unblock
// whatever the suite is holding so the process can still exit and report.
func awaitSignal(t *testing.T, ch <-chan struct{}, what string, rescue func()) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(settleFor):
		rescue()
		t.Fatalf("timed out after %s waiting for %s", settleFor, what)
	}
}

// awaitState waits on a capability-supplied view of backend state.
//
// Unlike a handler observation there is no signal to wake on — nothing calls
// back when a deferral has been applied to a backlog — so this one genuinely
// polls, and says so rather than pretending a channel exists.
func awaitState(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(settleFor)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// --- events ---------------------------------------------------------------

// newEvent builds a probe event whose Type carries the test's label for it.
//
// No payload type is registered: an event whose type this build does not know
// is a case the envelope must carry losslessly (see internal/events), so
// publishing unregistered types is a property worth exercising, not a gap.
// The conversation key rides in the envelope's own Payload map, which every
// backend has to round-trip whether or not it knows the type.
func newEvent(label string) *events.Event {
	return &events.Event{
		ID:        uuid.New(),
		Type:      label,
		Timestamp: time.Now().UTC(),
		Source:    "queuetest",
	}
}

// newConvEvent is newEvent plus the conversation key batch partitioning uses.
//
// The key is a STRING, and every other free-form payload value this suite
// publishes is too. That is convenient — a string survives any codec unchanged,
// so partitioning assertions never depend on the wire — but it means these
// fixtures cannot see a payload change type or go missing in transit. A suite
// whose fixtures are already in the shape its backend produces is not testing
// the boundary, it is arranging not to look at it.
//
// Wire/a_free_form_payload_value_survives_whatever_type_it_lands_as is the case
// that does look, with a number, a slice, a bool and a nested map. Anything
// added here should stay string-keyed for the partitioning cases and leave the
// wire question to that one.
func newConvEvent(label, conv string) *events.Event {
	ev := newEvent(label)
	ev.Payload = map[string]any{"conv": conv}
	return ev
}

// convKey is the BatchKeyFunc the batching subtests partition with.
func convKey(ev *events.Event) string {
	if ev == nil {
		return ""
	}
	if conv, ok := ev.Payload["conv"].(string); ok {
		return conv
	}
	// Empty is the contract's signal to fall back to a per-event key, so
	// an event with no conversation is its own partition.
	return ""
}

// canonicalJSON renders a value the way the wire would, so two payloads can be
// compared for VALUE equality without either side asserting a Go type.
func canonicalJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("canonicalise %v: %v", v, err)
	}
	return string(raw)
}

// firstConv reads the conversation key of a delivered batch, reporting an empty
// one instead of panicking on it.
//
// evs is what the BACKEND returned, and on the healthy path it is never empty —
// so an unguarded evs[0] only fires on the day a backend is actually broken,
// which is the one day the output matters. A panic on a dispatch goroutine ends
// the whole test binary, so the suite's answer to a broken backend would be to
// destroy its own diagnosis: one unrelated case reported, then a stack trace,
// and nothing naming the real defect. t.Errorf rather than Fatalf, because this
// does not run on the test goroutine.
func firstConv(t *testing.T, evs []*events.Event) string {
	t.Helper()
	if len(evs) == 0 {
		t.Errorf("backend delivered an EMPTY batch; every partition holds at least one event")
		return ""
	}
	return convKey(evs[0])
}

func labelOf(ev *events.Event) string {
	if ev == nil {
		return ""
	}
	return ev.Type
}

func labelsOf(evs []*events.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, labelOf(ev))
	}
	return out
}

func convsOf(evs []*events.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, convKey(ev))
	}
	return out
}

// --- small helpers --------------------------------------------------------

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// tryPublish publishes without requiring success.
//
// For subjects that exist only to be REJECTED by a pattern under test: a
// backend is entitled to refuse a subject it has no stream for, and a refusal
// satisfies the property just as well as accepting the event and not matching
// it. Making those fatal would test the backend's subject topology rather
// than its wildcard matching.
func tryPublish(q queue.EventQueue, topic string, ev *events.Event) {
	_ = q.Publish(context.Background(), topic, ev)
}

func publish(t *testing.T, q queue.EventQueue, topic string, ev *events.Event) {
	t.Helper()
	if err := q.Publish(context.Background(), topic, ev); err != nil {
		t.Fatalf("Publish(%s, %s): %v", topic, ev.Type, err)
	}
}

func subscribe(t *testing.T, q queue.EventQueue, topic, group string, h queue.Handler) {
	t.Helper()
	if err := q.Subscribe(context.Background(), topic, group, h); err != nil {
		t.Fatalf("Subscribe(%s, %s): %v", topic, group, err)
	}
}

func subscribeBatch(t *testing.T, q queue.EventQueue, topic, group string, h queue.BatchHandler, opts *queue.BatchOptions) {
	t.Helper()
	if err := q.SubscribeBatch(context.Background(), topic, group, h, convKey, opts); err != nil {
		t.Fatalf("SubscribeBatch(%s, %s): %v", topic, group, err)
	}
}

// recordingHandler acknowledges every delivery and journals its label.
func recordingHandler(j *journal) queue.Handler {
	return func(_ context.Context, ev *events.Event) queue.Result {
		j.record(labelOf(ev))
		return queue.Ack()
	}
}

// recordingBatchHandler acknowledges every partition and journals it whole.
func recordingBatchHandler(b *batchJournal) queue.BatchHandler {
	return func(_ context.Context, evs []*events.Event) queue.Result {
		b.record(evs)
		return queue.Ack()
	}
}

func (s *suite) needBacklog(t *testing.T) func(q queue.EventQueue, topic, group string) []*events.Event {
	t.Helper()
	if s.caps.Backlog == nil {
		t.Skip("backend cannot report a subscription's retained mail")
	}
	return s.caps.Backlog
}

func (s *suite) needDeadLetters(t *testing.T) func(q queue.EventQueue, topic, group string) []*events.Event {
	t.Helper()
	if s.caps.DeadLetters == nil {
		t.Skip("backend cannot report dead letters")
	}
	return s.caps.DeadLetters
}

// needAttempts returns a constructor for a queue that gives a persistently
// failing event exactly attempts deliveries before dead-lettering it.
func (s *suite) needAttempts(t *testing.T) func(t *testing.T, attempts int) queue.EventQueue {
	t.Helper()
	if s.caps.WithDeliveryAttempts != nil {
		return s.caps.WithDeliveryAttempts
	}
	if legacy := s.caps.WithRedeliveryBudget; legacy != nil {
		// The superseded field counts redeliveries after the first, so one
		// fewer than the attempts the suite observes.
		return func(t *testing.T, attempts int) queue.EventQueue {
			t.Helper()
			return legacy(t, attempts-1)
		}
	}
	t.Skip("backend cannot be built with a specific delivery-attempt limit")
	return nil
}

func (s *suite) needPeer(t *testing.T) func(t *testing.T, q queue.EventQueue) queue.EventQueue {
	t.Helper()
	if s.caps.Peer == nil {
		t.Skip("backend cannot model a second node on the same broker")
	}
	return s.caps.Peer
}
