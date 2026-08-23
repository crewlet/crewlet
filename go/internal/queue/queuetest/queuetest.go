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
//
// # If you are bringing up a new backend and a case fails, suspect the case
//
// Not politeness — the measured record of this suite. Every case here was
// written against at most two backends, and each time a third opinion arrived
// the suite was wrong more often than the backend was. What that looked like:
//
//   - It required a deferral to cost nothing. d-102 decisions 1-2 explicitly
//     trade that away on JetStream and raise MaxDeliver to absorb it. Now
//     FreeDeferral.
//   - It required a nak to replay from the head. Measured Pulsar-only; JetStream
//     returns redelivered messages behind never-delivered ones. Now
//     HeadReplayOnNak.
//   - It required a stopped queue to restart. The contract does not say, and two
//     backends answered differently. Now Restartable.
//   - It required a publish to `crewlet.events` — a subject the grammar cannot
//     produce — to succeed, purely to have something for a wildcard to reject.
//   - Its WithRedeliveryBudget doc invented a counting convention the contract
//     never stated, and a backend then wrote `MaxDeliver: budget + 1` to satisfy
//     the sentence. That agreement looked like evidence and was manufactured.
//
// So when this suite fails a backend nobody here wrote, the first question is
// whether the case states a real invariant or an accident of the two backends
// that already agreed. Check rewrite/decisions/ for the operation before
// concluding the backend is wrong: a recorded degradation is a permitted
// exception, and the corpus is where permission lives. If the property is real
// but not universal, it becomes a Capabilities flag with the reason at the skip
// — not a requirement, and not a deleted case.
//
// # Checks that do not depend on you being careful
//
// The section above asks you to suspect the case. Both conformance suites in
// this repo carried that instruction, both authors had read it, and both then
// shipped cases requiring more of a backend than the contract allowed. It does
// not fail from being unread. It fails because a suite cannot audit itself: the
// fixtures, the constants, the helpers and the doctrine are written by the same
// mind as the cases, so they are blind in the same places and fail together.
//
// What actually caught things was mechanical. Each of these is here because it
// caught something that careful reading had already missed:
//
//   - A mutation must LAND in the branch it is proving. A nak-to-drop meant to
//     produce a prefix produced a divergence; disabling a flush check produced
//     nothing, because the drain re-checked. Both "verified" a branch neither
//     had reached. Read the failure message, not the exit status.
//
//   - A mutation harness must tell BUILD FAILED from CAUGHT. A non-compiling
//     mutation satisfies "the suite failed", so a harness keyed on that reports
//     every broken patch as a catch.
//
//   - It also needs DID NOT LAND, distinct from CLEAN, and this is the verdict
//     worth the most. A patch whose anchor no longer matches leaves the tree
//     unmodified; the suite then passes and the harness reports a GAP that does
//     not exist. That is a false FINDING, which sends someone to change working
//     code — strictly worse than a false green, which only misses a catch.
//     Compare a marker count before and after; do not trust a patch tool's
//     silence.
//
//     Which gives the rule for when it matters: EVERY CLAIM RESTING ON A CLEAN
//     RESULT NEEDS A LANDING CHECK, and claims resting on a FAIL do not. A
//     failure naming a subtest is self-evidencing — a patch that never applied
//     cannot produce one — so "this mutation is caught by that case" carries
//     its own proof. "This mutation passes, therefore a gap exists" carries
//     none, and CLEAN is the one verdict a non-landing patch can forge. Every
//     documented gap in this suite is either landing-checked directly or
//     retroactively evidenced by a later CAUGHT using the same patch text.
//
//   - A control set needs a row expected to come back CLEAN for a written-down
//     reason. Without one, a harness biased toward CAUGHT and a suite that
//     catches everything are indistinguishable.
//
//   - Any invariant about WHERE code sits belongs on the syntax tree. Line
//     scans cannot see scope: one here reported six violations, all false, and
//     another missed every function that unlocked inside an early return.
//
//   - A guard asserting an ABSENCE must also assert it matched its subject.
//     Renaming one field named `mu` blinds such a matcher, and it then passes
//     for ever having inspected nothing.
//
//   - Test a guard's STATED LIMITS, not only its guarantee. Both source guards
//     here carried a "what this does not catch" paragraph that named the exact
//     form the guard exists to find — one construction copied to a second site.
//     Measured in both directions afterwards: each catches its lexical form and
//     neither sees through a named call, which is what the paragraphs now say.
//     The shape survives because a limit reads as modesty, and nobody re-checks
//     a guard for claiming to do LESS than it does; a bound is as much a claim
//     as a guarantee, and it was the only claim here never put to a test.
//
//   - A diagnostic must not assert a cause it cannot distinguish. "Delivery
//     happened, so this is not timing" is false for every case awaiting a
//     sequence, because a prefix is exactly what a slow backend produces.
//
//   - Enumerate what the suite SENDS, not only what it asserts. Every topic and
//     group here was a plain lowercase identifier, which no mutation could ever
//     have revealed — and probing one dotted name found a live collision in a
//     shipped backend.
//
//   - Enumerate the POINTS IN A LIFECYCLE at which each operation is sent. A
//     separate axis from the one above, and a suite can be thorough on values
//     while never touching it: after a Stop this suite sent exactly two of
//     eleven verbs, and the window it never visited held a hold that survived
//     into the next life and left a restarted seat silently deaf. Build the
//     WHOLE matrix rather than probing the verb you suspect — the suspected one
//     is chosen from inside the blind spot.
//
// One lifecycle this suite does not visit at all: the BACKEND'S REACHABILITY.
// Every case here runs against a healthy broker, so nothing certifies what a
// verb answers when the store is unreachable — and "nil, false, or an empty
// slice with no error" all read as facts about a subscription that a call which
// never reached the broker does not have. Closing it needs fault injection the
// contract has no hook for, so it is named as a known gap rather than left as
// an assumption.
//
// One limit, and it binds the two entries above that recommend a guard rather
// than excusing them: a guard that names its own assumption DIAGNOSES a race,
// it does not close one. Check-then-act only shrinks the window — measured in
// this repo's sibling suite, both failures its guard was written for still
// happened, one round trip after the check passed. So the racing cases here pay
// a window sized for the slowest plausible backend AS WELL as reporting which
// side lost; the message alone would have shipped the same race with a better
// error string. Read a green from a guarded case as "the race did not fire",
// never as "the race is gone".
//
// None of these require anyone to be suspicious at the right moment, which is
// the only property that survives contact with a blind spot.
package queuetest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
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

	// READ-YOUR-OWN-WRITE, required of every inspection function below.
	//
	// A backend supplying these must have them reflect an operation that has
	// already RETURNED. Cases that assert an absence read them with no wait —
	// most of NegativePaths does, because "this refusal wrote nothing" has no
	// signal to wait on.
	//
	// MEASURED, because this comment previously claimed something broader and
	// was wrong. Serving inspection from a view one call stale does NOT make
	// the group pass vacuously: 9 cases across the suite fail loudly, because
	// a case reading an ABSOLUTE value sees a stale one and reports it.
	//
	// The vacuous shape is narrower and it is the one that matters here. A
	// before/after DIFFERENTIAL cancels a uniform lag: assertUntouched reads a
	// correct baseline, the operation writes, the second read has not caught up,
	// before equals after, and the case passes. Verified by running a lagging
	// view together with a real corruption — EnsureSubscription wiping the mail
	// it must keep — and ensure_subscription_on_an_existing_one_keeps_its_mail
	// reported PASS with the wipe in place.
	//
	// So the hazard is worse than "the group goes quiet". The group gets LOUD
	// somewhere else, the noise is attributed to the lag, and the differential
	// cases stay green while proving nothing — which is exactly the pair of
	// symptoms that gets the wrong one investigated.
	//
	// Stated here because no case can discover it: both backends this suite
	// was built against make it true for free — the twin is a mutex over a
	// map, and the broker-backed one reads statistics synchronously — so
	// nothing here can tell a backend that GUARANTEES it from one that merely
	// happens to. That is the same blindness as fixtures shaped like the
	// backend, reached the same way, and the only remedy is to write the
	// requirement down where the capability is supplied.
	//
	// It is deliberately a requirement on the CAPABILITY and not on
	// EventQueue: nothing in the contract promises when a completed operation
	// becomes observable, and this suite has no business inventing that
	// promise. A backend that cannot honour it should leave these nil and
	// skip the group rather than supply a lagging view of itself.

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
	// Not a feature — the reason the engine carries no re-entrancy
	// guard. See runReentrancy.
	t.Run("Reentrancy", s.runReentrancy)
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
	// MEASURED, and the measurement moved what this paragraph says. Worst
	// wait observed across a full run: 0.98ms on the in-memory twin, and
	// 503-509ms on JetStream over five runs. Headroom on a real broker is
	// therefore about 6x — not the comfortable margin "generous" implies.
	//
	// What consumes it is NOT delivery latency. In 5 of 5 runs the slowest
	// wait was a REDELIVERY case, and redelivery is spaced by backend
	// POLICY rather than backend health: JetStream ships a 1s nak delay and
	// its conformance harness cuts that to 25ms so this suite can finish.
	// At the shipped value three redeliveries would exhaust this budget on
	// a perfectly healthy broker.
	//
	// So the IMPLICIT CONTRACT here is about redelivery SPACING, not
	// hand-over: a backend must tune its redelivery delay down for the
	// conformance run. This used to prescribe the opposite — "a backend
	// that needs longer should say so rather than have this raised" — which
	// sends a backend author to raise a constant every positive assertion
	// in the suite pays, when the repair is one constant of their own.
	// Raising this is the wrong fix for the only pressure ever measured on
	// it.
	settleFor = 3 * time.Second

	// quietFor is how long a negative assertion — "this must NOT be
	// delivered" — holds the window open.
	//
	// WHAT IT DOES AND DOES NOT COVER, because the reason here was wrong
	// before the number was. It used to say a leak "tends to leak
	// immediately, so this only has to outlast one dispatch cycle" — written
	// against a backend whose dispatch cycle is microseconds, and false for
	// one where a cycle is a network round trip. On a backend slower than
	// this window, a negative assertion passes because nothing had ARRIVED
	// yet, not because nothing was leaked.
	//
	// That is a false PASS, which is worse than a flake: it quietly weakens
	// coverage on exactly the backends most likely to leak, and it does so
	// silently. The value stays — every negative assertion pays it, and no
	// window is provably long enough for an arbitrary backend — but do not
	// read this as a general "wait for something" budget, and do not add a
	// case whose POSITIVE half depends on it. Positive halves wait on
	// settleFor and on a signal.
	quietFor = 150 * time.Millisecond

	// lingerFor is the batch linger window the batching subtests use. It
	// has to be long enough that several publishes land inside one window,
	// and short enough that a test waiting out several windows stays quick.
	//
	// "Several publishes land inside one window" was measured on the
	// in-memory twin, where a publish is a mutex operation, and that is the
	// claim to distrust rather than the number: max_batch_chunks_oversized_
	// buffers needs FIVE publishes inside one window, which is 10ms each on
	// a backend where a publish is a network round trip. That dependency is
	// real: the case publishes in a bare loop, unlike the deferral cases,
	// which go through fillOneBatch's pause/publish/resume and do not race
	// the window at all.
	//
	// The value stands on a NARROWER base than "no backend has reported a
	// flake" suggests, which is what this used to claim. Ten repeats of the
	// Batch group on JetStream came back clean, and the twin is the fastest
	// backend and so the least informative. Pulsar contributes nothing at
	// all: its TestConformance SKIPS without a live broker, so wherever one
	// is absent this suite is certifying two backends, not three. A case
	// needing more publishes per window should say so rather than assume
	// this covers it.
	//
	// The suite never asks for a linger above queue.MaxLingerSeconds, and
	// must not: backends size their dispatch budget to that ceiling and are
	// right to refuse a window they cannot honour rather than silently
	// clamping it. A case needing a longer window to make something
	// observable should find another way to observe it.
	lingerFor = 50 * time.Millisecond

	// racingWindow is the linger a case gets when its SETUP has to complete
	// while the window is still open — the pause and stop cases, where the
	// window must be open when the verb lands AND must expire afterwards.
	// Neither ordering removes that, so those two pay a longer wait and no
	// other case does.
	//
	// Sized from measurement, but deliberately not from the measurement that
	// was available: the publish-to-pause gap is 1.0ms worst-case on the
	// in-memory twin under 10x CPU load with -race -count=6, which made the
	// old 50ms window look like 50x headroom. That is the FASTEST backend,
	// where both calls are local mutex operations. On a networked backend a
	// publish is a round trip, and the measured cost of a handful of those
	// under a parallel -race suite is 200-330ms. A constant sized against the
	// quickest backend is how a suite acquires a race it cannot see, so this
	// is sized for the slowest one plausible.
	racingWindow = 1 * time.Second
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
	deliveriesObserved.Add(1)
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
	j.awaitExpecting(t, what, cond, nil, false)
}

// awaitExpecting is await plus, when the caller has one, the exact sequence it
// is waiting for — which is the only thing that lets a timeout tell a lagging
// delivery from a wrong one. Passed down rather than stored on the journal so
// two awaits on one journal cannot inherit each other's expectation.
func (j *journal) awaitExpecting(t *testing.T, what string, cond func(seen []string) bool, want []string, hasWant bool) {
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
			seen := j.all()
			timedOut(t, what, verdictFor(seen, want, hasWant), seen)
		}
	}
}

// awaitLabels waits for exactly this sequence of labels, in order.
func (j *journal) awaitLabels(t *testing.T, what string, want ...string) {
	t.Helper()
	j.awaitExpecting(t, what, func(seen []string) bool { return equalStrings(seen, want) }, want, true)
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

// deliveriesObserved counts every delivery this suite records, across every
// case in the run. It is the evidence a timeout needs and cannot otherwise get:
// a backend that delivered thousands of events inside this same budget has
// demonstrably not been starved by it.
var deliveriesObserved atomic.Int64

// deliveryVerdict is what a timeout can honestly conclude about its own cause.
type deliveryVerdict int

const (
	// verdictNothing — nothing arrived at all.
	verdictNothing deliveryVerdict = iota
	// verdictPartial — what arrived is a strict PREFIX of what was expected.
	verdictPartial
	// verdictDiverged — what arrived contradicts what was expected.
	verdictDiverged
	// verdictUnknown — something arrived under an opaque condition, so
	// whether more was still in flight cannot be determined.
	verdictUnknown
)

// timedOut ends a case that overran the settle budget, naming only what the
// evidence supports.
//
// An earlier version of this asserted its cause: anything that arrived at all
// meant "a behavioural difference, not a timing one, so settleFor is not the
// suspect". That is wrong for every case awaiting a SEQUENCE. A backend slower
// than the budget delivers e1 and not yet e2, which is "something arrived,
// condition unmet" — so the message told the one reader whose problem WAS
// timing to look anywhere but there. The fix for misattribution shipped with
// the same misattribution pointing the other way, which is worth remembering
// the next time a diagnostic feels obviously right.
//
// So a prefix is now reported as a prefix and nothing is concluded from it, and
// the no-delivery case cites how many deliveries this backend managed
// elsewhere rather than offering advice.
func timedOut(t *testing.T, what string, v deliveryVerdict, observed any) {
	t.Helper()
	const budgetNote = "settleFor is a constant this suite advertises, not a promise the " +
		"contract makes — and raising it is almost never the repair. Measured, the only pressure " +
		"ever seen on it is REDELIVERY SPACING rather than delivery: check your backend's " +
		"redelivery delay first (JetStream's harness cuts a 1s nak delay to 25ms for exactly " +
		"this reason). Raising this blunts the check for every other backend, and every positive " +
		"assertion in the suite pays for it."

	switch v {
	case verdictPartial:
		t.Fatalf("timed out after %s waiting for %s; delivery is INCOMPLETE, not wrong: "+
			"handlers saw %v, a prefix of what was expected\n"+
			"\tThat is exactly what a backend slower than this budget produces, so timing "+
			"and behaviour are both live. This helper cannot tell them apart and does not "+
			"guess. %s", settleFor, what, observed, budgetNote)
	case verdictDiverged:
		t.Fatalf("timed out after %s waiting for %s; handlers saw %v, which CONTRADICTS "+
			"what was expected rather than lagging it\n"+
			"\tDelivery happened and was wrong, so settleFor is not the suspect.",
			settleFor, what, observed)
	case verdictUnknown:
		t.Fatalf("timed out after %s waiting for %s; handlers saw %v under a condition this "+
			"helper cannot decompose\n"+
			"\tWhether more was still in flight is unknown, so treat timing and behaviour "+
			"as both live. %s", settleFor, what, observed, budgetNote)
	default:
		seen := deliveriesObserved.Load()
		evidence := fmt.Sprintf("This backend delivered %d events elsewhere in this run "+
			"inside the same budget, so the budget is evidently sufficient for it — look "+
			"at this case's own subject first.", seen)
		if seen == 0 {
			evidence = "This backend has delivered NOTHING anywhere in this run, so suspect " +
				"its setup or this budget before this case."
		}
		t.Fatalf("timed out after %s waiting for %s; NOTHING was delivered\n\t%s %s",
			settleFor, what, evidence, budgetNote)
	}
}

// verdictFor classifies a timeout, conceding verdictUnknown when the caller had
// no explicit expectation to compare against — an opaque condition genuinely
// cannot say whether more was in flight, and saying so is better than picking.
func verdictFor[T comparable](seen, want []T, hasWant bool) deliveryVerdict {
	if len(seen) == 0 {
		return verdictNothing
	}
	if !hasWant {
		return verdictUnknown
	}
	return prefixVerdict(seen, want)
}

// prefixVerdict classifies what was seen against what was wanted.
func prefixVerdict[T comparable](seen, want []T) deliveryVerdict {
	if len(seen) == 0 {
		return verdictNothing
	}
	for i, got := range seen {
		if i >= len(want) || want[i] != got {
			return verdictDiverged
		}
	}
	return verdictPartial
}

// staysAtRacing is staysAt for a case whose SETUP had to land inside a window
// the backend was already counting down.
//
// A plain negative assertion cannot tell the two causes apart, and its message
// picked one: "a paused queue flushed its linger window" blames the backend for
// what may have been the suite losing its own race. So this prints the raw
// elapsed and lets it decide, which is the only honest split available — and
// unlike a backend whose clock the suite can move, wall time here is always
// real evidence, because nothing in this suite can travel.
func (b *batchJournal) staysAtRacing(t *testing.T, want int, what string, window, setupTook time.Duration) {
	t.Helper()
	time.Sleep(quietFor)
	got := b.all()
	if len(got) == want {
		return
	}
	if setupTook >= window {
		t.Fatalf("%s: saw %v — but this case's own setup took %s against a %s window, "+
			"so the SUITE lost its race and this is not a backend defect.\n"+
			"\tIf a backend genuinely cannot complete the setup inside this window, that "+
			"is a finding to raise — not a number to quietly raise here, which would blunt "+
			"the check for every other backend.", what, got, setupTook, window)
	}
	t.Fatalf("%s: saw %v; the setup took %s, comfortably inside the %s window, so too "+
		"little time passed for this suite's own timing to explain it — this is a BACKEND "+
		"defect.", what, got, setupTook, window)
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
	deliveriesObserved.Add(int64(len(evs)))
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
	b.awaitExpecting(t, what, cond, nil, false)
}

func (b *batchJournal) awaitExpecting(t *testing.T, what string, cond func(batches [][]string) bool, want []int, hasWant bool) {
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
			timedOut(t, what, verdictFor(b.sizes(), want, hasWant), b.all())
		}
	}
}

func (b *batchJournal) awaitSizes(t *testing.T, what string, want ...int) {
	t.Helper()
	b.awaitExpecting(t, what, func([][]string) bool { return equalInts(b.sizes(), want) }, want, true)
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
		timedOut(t, what, verdictNothing, nil)
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
			// State the suite polls has no "saw something" half — the
			// condition is simply still false — so this always reports the
			// ambiguous case, which it genuinely is.
			timedOut(t, what, verdictNothing, nil)
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

// needQuiescing returns the view of whether an attachment has stopped taking
// work — the only signal that a deferral has actually been APPLIED.
func (s *suite) needQuiescing(t *testing.T) func(q queue.EventQueue, topic, group string) bool {
	t.Helper()
	if s.caps.Quiescing == nil {
		t.Skip("backend cannot report whether an attachment is quiesced")
	}
	return s.caps.Quiescing
}

func (s *suite) needPeer(t *testing.T) func(t *testing.T, q queue.EventQueue) queue.EventQueue {
	t.Helper()
	if s.caps.Peer == nil {
		t.Skip("backend cannot model a second node on the same broker")
	}
	return s.caps.Peer
}
