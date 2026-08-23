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

	// WithRedeliveryBudget returns a fresh UNSTARTED queue whose
	// dead-letter budget is exactly budget redeliveries after the first
	// delivery. The dead-letter subtests need a small budget; running
	// them against a production default would mean dozens of handler
	// invocations per assertion.
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
	t.Run("Attachment", s.runAttachment)
	t.Run("Stream", s.runStream)
	t.Run("Batch", s.runBatch)
	t.Run("Fleet", s.runFleet)
	t.Run("BatchOptionsAndOrdering", s.runOrdering)
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

func (s *suite) needBudget(t *testing.T) func(t *testing.T, budget int) queue.EventQueue {
	t.Helper()
	if s.caps.WithRedeliveryBudget == nil {
		t.Skip("backend cannot be built with a specific redelivery budget")
	}
	return s.caps.WithRedeliveryBudget
}

func (s *suite) needPeer(t *testing.T) func(t *testing.T, q queue.EventQueue) queue.EventQueue {
	t.Helper()
	if s.caps.Peer == nil {
		t.Skip("backend cannot model a second node on the same broker")
	}
	return s.caps.Peer
}
