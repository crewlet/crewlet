// Package queuetest is the conformance suite that certifies an EventQueue
// backend.
//
// One suite, every backend. The in-memory twin and NATS JetStream both have
// to answer the same questions, because everything above internal/queue is
// forbidden to know which one is running — so the only place a backend
// difference can be caught is here. A backend this suite has not certified
// does not exist as far as the engine is concerned.
//
// The suite is written for an ASYNCHRONOUS backend even though the in-memory
// twin dispatches inline: every positive assertion waits for the handler to
// speak (a channel wake, not a poll). A wait for several deliveries waits for
// as long as the backend keeps delivering: its bound is a pause with nothing
// delivered (stallFor), never the wait's own length, which a loaded runner
// stretches on a healthy broker. A wait for one answer is held to one such
// pause. A negative assertion is read behind something that would have
// carried what it rules out — a later broadcast of the same sender, a fence
// through the same feed, a barrier through the listeners, a withdrawal's
// answer to its own bound, the backend's own count, its own report that it
// has settled ([Capabilities.Settle]). Where nothing a
// backend exports would carry it, the absence is decided beside the backend,
// where it can be exact, and never behind a quiet window here: that a handler
// never runs beside itself, a listener's or a self-publishing subscription's,
// is decided on the twin in a bubble and on JetStream by where its code sits
// (runBroadcast's and runReentrancy's one-at-a-time cases name both). An
// assertion that reads state the instant after Publish returns would pass on
// the twin and fail on a real broker, which is precisely the divergence this
// suite exists to prevent.
//
// Subtests are named after the contract clause they certify, so a failure
// here names the rule it broke rather than the call it made.
//
// # If you are bringing up a new backend and a case fails, suspect the case
//
// Not politeness — the measured record of this suite. Every case here was
// written against at most two backends, and each time a third opinion arrived
// the suite was wrong more often than the backend was. What that looked like:
//
//   - It required a deferral to cost nothing. JetStream trades that away
//     deliberately and raises MaxDeliver to absorb it, so this became the
//     FreeDeferral capability — and that was the wrong repair. A capability
//     the TWIN declares and the shipped broker does not runs its case against
//     the twin alone, and it put the suite's own flag against the contract's
//     own sentence, which says every return that puts a message back spends a
//     delivery. The twin spends one now and the flag is gone; what the case
//     certifies on both backends is the contract's number.
//   - It required a nak to replay from the head. JetStream returns a FAILED
//     delivery behind never-delivered ones, so only the twin did this, and it
//     became the HeadReplayOnNak capability — the FreeDeferral mistake again:
//     a flag the twin declared and the shipped broker did not, so every
//     engine and node test run on the twin certified a seat that retried a
//     failure ahead of the conversation's newer mail. The twin returns a
//     failure behind them now, the flag is gone, and
//     a_failure_returns_behind_waiting_mail_and_a_deferral_at_the_head
//     asserts the one order on both backends.
//   - It required a FAILED delivery and a newer event of its conversation to
//     reach one handler call. The contract orders a partition, never the
//     calls a conversation is spread across, and a failure waits out its
//     redelivery backoff while the newer event is dispatched without it. Two
//     cases met the requirement only because the JetStream harness cut that
//     backoff to half their window, and a loaded runner lost the race. They
//     hand back by deferral now, which no backend spaces — see
//     handBackLinger.
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
// that already agreed. Check the contract in queue.go, and check whether the
// operation is already gated by a [Capabilities] flag, before concluding the
// backend is wrong: a documented degradation is a permitted exception. If the
// property is real
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
//     fourteen verbs, and the window it never visited held a hold that survived
//     into the next life and left a restarted seat silently deaf. Build the
//     WHOLE matrix rather than probing the verb you suspect — the suspected one
//     is chosen from inside the blind spot.
//
// One lifecycle this suite visits only through a capability: the BACKEND'S
// REACHABILITY. Every other case runs against a healthy broker, and what a verb
// answers when the broker is unreachable is a contract clause of its own —
// "nil, false, or an empty slice with no error" all read as facts about a
// subscription that a call which never reached the broker does not have, and
// an error with no mark read above the queue as this node's own fault. The
// contract has no hook to inject that fault, so [Capabilities.BrokerDown] is
// one, and runReachability holds every verb to queue.ErrUnavailable through it.
// What it still cannot visit is a broker that comes BACK: the real backend's
// harness can only take its server away, so recovery is a known gap rather
// than an assumption.
//
// One limit, and it binds the two entries above that recommend a guard rather
// than excusing them: a guard that names its own assumption DIAGNOSES a race,
// it does not close one. Check-then-act only shrinks the window — measured in
// this repo's sibling suite, both failures its guard was written for still
// happened, one round trip after the check passed. This suite's racing cases
// learned it twice over: the pause and stop cases paid a window sized for the
// slowest plausible backend AND reported which side lost, and a loaded runner
// still lost, with a message that could not tell the suite's lost race from a
// backend that flushed past the pause. What closed that race was taking the
// window's end off the wall clock (Capabilities.LingerClock), so that nothing
// is left to lose. Read a green from a guarded case as "the race did not fire",
// never as "the race is gone".
//
// None of these require anyone to be suspicious at the right moment, which is
// the only property that survives contact with a blind spot.
package queuetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
// Several properties this suite certifies concern state the contract does not
// expose (the mail a subscription retains, the reasons holding an attachment
// paused, which seats a node serves). Seat ownership rests on them, so they
// must be asserted. A backend supplies them here, and a subtest whose
// capability is absent skips with a named reason rather than silently passing.
//
// [Capabilities.Settle] is the one a backend must supply, and [RunWith] refuses
// a backend without it: every case asserting that nothing more was delivered
// reads behind it. Every other field may be left zero.
type Capabilities struct {
	// Settle returns once q will dispatch nothing more until the next verb
	// is sent to it: every handler call it already owes has been made. It
	// fails, naming what it was still waiting for, if ctx ends first.
	//
	// THE SIGNAL A NEGATIVE ASSERTION IS READ BEHIND. "This must NOT be
	// delivered" has no completion to wait on, and a quiet window is a bet on
	// latency: a late event or redelivery passed with the bug present. Read
	// behind Settle, the journal holds all the backend would ever hand over.
	// "The next verb" is the case's, a handler's included; what reaches a
	// handler other than a subscription's (feed, listener, answerer) the cases
	// fence themselves. The suite also waits for WaitForHandlers after it.
	Settle func(ctx context.Context, t *testing.T, q queue.EventQueue) error

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
	// It asks for TOTAL ATTEMPTS, an observable, not a "budget", which is not
	// one number and which the contract never defines: the twin counts
	// redeliveries AFTER the first (24 means 25 attempts), NATS MaxDeliver
	// INCLUDING it (25 means 25). Naming the observable lets each backend
	// translate from what its broker counts; a convention defined here would
	// make every passing backend agree with the suite because it said so.
	WithDeliveryAttempts func(t *testing.T, attempts int) queue.EventQueue

	// READ-YOUR-OWN-WRITE, required of every inspection function below: each
	// must reflect an operation that has already RETURNED, because cases
	// asserting an absence read them with no wait.
	//
	// A stale view fails ABSOLUTE reads loudly but passes a before/after
	// DIFFERENTIAL vacuously: assertUntouched's two reads lag alike.
	// ensure_subscription_on_an_existing_one_keeps_its_mail_and_proposes_nothing
	// passed with a wipe in place. No case can detect a violation; the shipped
	// backend once sized a backlog from its CONSUMER's pending count, which lags
	// an acked publish. A requirement on the CAPABILITY, not EventQueue: a
	// backend that cannot honour it leaves these nil and skips the group.

	// AND NOTHING HIDDEN BEHIND AN UNREPORTED FAILURE, which is the same
	// class of promise as the paragraphs above and was broken the same way.
	// The two reads below can fail — they are broker round trips on the
	// backend that ships — and their adapters discarded the error, so "the
	// broker did not answer" arrived here as "the seat is holding no mail".
	// That is the engine's most incident-hardened rule broken inside the
	// suite that exists to enforce it: held, definitively not held, and
	// could-not-tell are three facts, and NegativePaths asserts absences
	// almost exclusively. Hence the *testing.T — an adapter reports the
	// failure as a failure instead of returning it as an emptiness, and no
	// case has to thread an error through a polling predicate to get it.

	// Backlog reports the events a subscription retains and has not
	// delivered — the mail an unowned seat is holding.
	Backlog func(t *testing.T, q queue.EventQueue, topic, group string) []*events.Event

	// DeadLetters reports the events a subscription gave up on.
	DeadLetters func(t *testing.T, q queue.EventQueue, topic, group string) []*events.Event

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

	// Lingering reports whether this client holds a batch for a
	// subscription in an open linger window that has outlasted what a
	// window of nothing collects, so that what is published now joins the
	// batch only because the window is open.
	//
	// THE STATE A WINDOW CASE PUBLISHES INTO. A pull backend's drain runs a
	// tail fetch at any window, zero included, so an event published before it
	// is collected whether or not the window did anything; a fixed sleep to
	// get past it passed a case whenever a runner scheduled the drain late. A
	// backend whose window never opens never reports one, so a case waiting on
	// this fails rather than passing on nothing.
	Lingering func(q queue.EventQueue, topic, group string) bool

	// InWindow reports how many events this client holds for a subscription
	// in an open linger window — the batch the window hands over when it
	// ends — and zero while no window of it is open.
	//
	// THE STATE A COALESCING CASE ENDS ITS WINDOW BEHIND. The case publishes its
	// next event into the window and ends it on LingerClock, but only once the
	// window holds that event: on a fetching backend, published is not yet
	// collected, and a window ended early hands over the first event alone,
	// which reads as a window that did not coalesce. A backend that never counts
	// what its window holds fails the case's wait rather than passing it.
	InWindow func(q queue.EventQueue, topic, group string) int

	// LingerClock returns a fresh, UNSTARTED queue whose batch linger windows
	// end on clock rather than the wall clock: a window ends once the case has
	// moved clock past its end ([Clock.Advance]), never before. Like
	// WithDeliveryAttempts it builds the queue, as the clock is fixed at
	// construction.
	//
	// THE CLOCK A CASE ENDS A WINDOW ON, BEHIND A VERB. Cases asking what a
	// pause, hold or stop does to an open window need it open at the verb and
	// ended after, and on the wall clock nothing orders the two (a second of
	// slack still lost). Here the case lands its verb, waits for the backend to
	// take it in (Parked, or InWindow), then ends the window. ONLY THE WINDOW'S
	// END is on it; fetches, redelivery spacing and ack windows stay wall-clock.
	LingerClock func(t *testing.T, clock *Clock) queue.EventQueue

	// Parked returns once q's consumer of (topic, group) has taken in the
	// verbs sent to q before the call: it stands with its gate shut and
	// holds nothing it took before the gate shut, so it hands a handler
	// nothing until a verb opens the gate again. It fails, naming what it was
	// still waiting for, when ctx ends first — and at once on a pair nothing
	// of q's is attached to, where it would decide nothing.
	//
	// A case ends a window behind it ([Capabilities.LingerClock]), else a
	// consumer reading its gate between fetches would see the window end first.
	// On the shipped backend the consume loop has come round to its gate and
	// found it shut; on the twin, which reads gates under one lock, the verb
	// has returned and no handler of q's runs.
	Parked func(ctx context.Context, t *testing.T, q queue.EventQueue, topic, group string) error

	// StreamSubscriptions reports how many of q's own stream subscriptions
	// ([queue.EventQueue.SubscribeStream]) can still hand their handler a
	// message: on the backend that ships, a feed whose consumption has not
	// ended, which is later than its Unsubscribe returning.
	//
	// THE STATE AN UNSUBSCRIBED FEED IS READ OFF. A feed is no consume loop
	// Settle could wait out, and once it is gone nothing it receives can be
	// fenced behind: a case asserting that an unsubscribed feed receives
	// nothing reads its journal once this reports the feed gone, when there
	// is nothing left to hand it anything.
	StreamSubscriptions func(q queue.EventQueue) int

	// ListenBarrier returns once every broadcast q itself has sent has been
	// handled by each of q's own listeners it reached — the handler called
	// on it and returned — failing the case if ctx ends first.
	//
	// THE BARRIER A LISTENER'S SILENCE IS READ BEHIND where no broadcast can
	// fence it. A later broadcast fences the listener that hears it, but a
	// WITHDRAWN listener hears neither, and its silence is the same as a
	// withdrawal that left it standing with its handler not yet run. On the
	// backend that ships it is the NATS client's own Barrier behind a flush;
	// on the twin, a marker through each inbox.
	ListenBarrier func(ctx context.Context, t *testing.T, q queue.EventQueue)

	// Answering reports, of q's own Serve registrations on subject, the
	// answers they are running and the requests they are holding for a
	// slot — [queue.AnswerGate.Occupancy], summed.
	//
	// THE STATE A BOUND IS READ OFF. That the request past a registration's
	// bound waits is otherwise an absence — the answerer was not called —
	// and an absence read after a pause passes a registration that ignores
	// its bound whenever that request reaches the answerer late. Held, the
	// request is counted waiting by the bound's own decision; admitted, it is
	// counted running, so a case reads one of the two rather than waiting to
	// see whether the answerer is called.
	Answering func(q queue.EventQueue, subject string) (running, waiting int)

	// ConsumerProposals reports how many consumer creates THIS client has
	// asked the broker for since it was built — the cost side of
	// EnsureSubscription, which the contract says nothing about.
	//
	// Re-ensuring must keep the mail (the contract's half) and must not ASK: on
	// the shipped broker a consumer create is a Raft proposal even when a no-op,
	// so ensuring every mailbox at each boot replicated a write per seat. A
	// backend that went back to proposing would pass every other case. SCOPED TO
	// THE CLIENT UNDER TEST (an admin connection would report zero for ever),
	// and not part of [subscriptionState], whose unit is one (topic, group) pair.
	ConsumerProposals func(q queue.EventQueue) int

	// BrokerDown takes away the broker q is a client of, for the rest of the
	// case, as a broker whose server stopped is taken away: q is NOT
	// stopped, and as far as it knows it is still live. It returns once q
	// has noticed, so the next verb sent meets the outage rather than racing
	// it.
	//
	// THE ONLY FAULT INJECTION IN THE SUITE, for runReachability: whether a
	// verb that could not reach its broker says so with queue.ErrUnavailable,
	// which every caller retries, rather than an unmarked error read as this
	// node's own fault, or queue.ErrNotLive, which a seat release reads as
	// proof the mailbox is torn down. One-way, because the shipped backend's
	// harness can only stop a server; see the package doc.
	BrokerDown func(t *testing.T, q queue.EventQueue)

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

	// RequiresStart declares that this backend's publish, subscription and
	// attachment verbs refuse on a queue that has not been started or has
	// been stopped.
	//
	// A capability because both answers are legitimate (on JetStream, Open
	// connects and Start is a no-op), but the CONSISTENCY is not optional:
	// an_unstarted_queue_answers_the_same_way_for_every_verb sends all
	// twenty-two verbs and refuses a backend that answers some one way and some
	// the other. After Stop there is no choice: see
	// a_stopped_queue_refuses_every_verb.
	RequiresStart bool

	// Restartable declares that Start on a stopped queue re-establishes it
	// and delivery resumes.
	//
	// A capability because the contract does not say: delivery pause is
	// documented one-way, so Stop may be terminal. The twin restarts (Stop is
	// a client disconnect); JetStream cannot (Stop closes the NATS connection).
	// UNSETTLED, deliberately: nothing restarts a queue in-process today, but a
	// config-reload path would trip over it, so the suite renders no verdict
	// until the contract picks a side. "Restartable" would make JetStream's
	// behaviour a bug; "terminal" puts the words in Stop's doc in queue.go.
	Restartable bool
}

// Factory builds a fresh, unstarted queue with the contract-level options
// given — see [RunWith].
type Factory func(t *testing.T, opts ...queue.Option) queue.EventQueue

// RunWith executes the full conformance suite against the backend produced by
// newQueue, with the capabilities it declares. A backend that can answer more
// gets certified on more; nothing it cannot answer fails, but for
// [Capabilities.Settle], which every backend must supply.
//
// newQueue must return a FRESH, UNSTARTED queue on every call: the suite owns
// the lifecycle, because start/stop ordering is itself part of the contract.
// It must honour the [queue.Option]s it is handed — they are the settings the
// CONTRACT defines, so a backend cannot answer them as a capability, and the
// suite certifies what every backend does with them.
func RunWith(t *testing.T, newQueue Factory, caps Capabilities) {
	t.Helper()
	if caps.Settle == nil {
		t.Fatal("queuetest: Capabilities.Settle is required: every case asserting that " +
			"nothing more was delivered reads its journal behind it, and without it each " +
			"would decide on a quiet window again")
	}
	s := &suite{newQueue: newQueue, caps: caps}
	// EVERY GROUP RUNS IN PARALLEL WITH THE OTHERS, as every case inside
	// each already does with its siblings: each case starts its own queue
	// (on the real backend, its own broker), so nothing one group does is
	// visible to another. Run one after another, each group cost its
	// SLOWEST case before the next could start — eleven tails in a row
	// where the cases fit four to a slot — while the concurrency stays the
	// -parallel bound every case's racing window was measured under.
	group := func(name string, run func(*testing.T)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run(t)
		})
	}
	group("EventQueue", s.runCore)
	group("Wire", s.runWire)
	group("Attachment", s.runAttachment)
	group("Stream", s.runStream)
	// The ephemeral pair, and the only verbs here that must leave
	// nothing behind. See runScatter.
	group("Scatter", s.runScatter)
	// The ephemeral fan-out presence travels on, which must leave nothing
	// behind either. See runBroadcast.
	group("Broadcast", s.runBroadcast)
	// What every registration's withdrawal answers when it is called
	// again. See runWithdrawal.
	group("Withdrawal", s.runWithdrawal)
	group("Batch", s.runBatch)
	group("Lane", s.runLane)
	// Not a feature — the reason the engine carries no re-entrancy
	// guard. See runReentrancy.
	group("Reentrancy", s.runReentrancy)
	group("Fleet", s.runFleet)
	// Which mailboxes exist, asked of the broker rather than of any
	// record. See runListing.
	group("Listing", s.runListing)
	// What a reader that settles its own deliveries keeps when it gives
	// up, in the one place every dead letter lives. See runDeadLetters.
	group("DeadLetters", s.runDeadLetters)
	// A "no" has two halves: the answer and the write that must not
	// happen. See runNegativePaths.
	group("NegativePaths", s.runNegativePaths)
	// What a verb answers when its broker is gone. See runReachability.
	group("Reachability", s.runReachability)
	// Named for what it is: shared contract functions, not backend
	// behaviour. See runContractPolicy for the scope this group does and
	// does not cover.
	group("ContractPolicyFunctions", s.runContractPolicy)
}

// RunLagged runs, alone, the cases a loaded broker's pauses once cut while
// they were still delivering (#1204), against a backend newQueue builds across
// a link the caller makes LAG: one that answers every request a while late.
// It is [RunWith]'s contract for newQueue and caps, and certifies that the
// suite's waits hold a pause to stallFor rather than a wait to its length.
//
// TWO CASES, NOT THE SUITE: every round trip pays twice the lag. These are the
// shapes that were cut (redeliveries; a handler's own publish coming back);
// the others wait the same way, through awaitDeliveries.
//
// Mutation: bound a journal's wait at three seconds from its start, as
// settleFor did, and both cases fail.
func RunLagged(t *testing.T, newQueue Factory, caps Capabilities) {
	t.Helper()
	if caps.Settle == nil {
		t.Fatal("queuetest: Capabilities.Settle is required, as RunWith's doc says")
	}
	s := &suite{newQueue: newQueue, caps: caps}
	ctx := t.Context()
	t.Run("handler_failure_triggers_redelivery", func(t *testing.T) {
		t.Parallel()
		s.handlerFailureTriggersRedelivery(ctx, t)
	})
	t.Run("a_handler_that_publishes_to_its_own_subscription_is_not_re_entered", func(t *testing.T) {
		t.Parallel()
		s.publishesToItsOwnSubscription(ctx, t)
	})
}

type suite struct {
	newQueue Factory
	caps     Capabilities
}

// start returns a started queue whose Stop is already registered as cleanup.
func (s *suite) start(ctx context.Context, t *testing.T, opts ...queue.Option) queue.EventQueue {
	t.Helper()
	return s.startQueue(ctx, t, s.newQueue(t, opts...))
}

// startQueue starts q, registers its Stop as cleanup, and records it as one of
// the queues the case t started, which staysAt settles before it reads.
func (s *suite) startQueue(ctx context.Context, t *testing.T, q queue.EventQueue) queue.EventQueue {
	t.Helper()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// WithoutCancel because the case's context is cancelled BEFORE its
	// cleanups run: a Stop handed the cancelled one would skip the very
	// teardown this cleanup exists to perform, and a queue left running
	// past its case leaks a consumer into the next one.
	stopCtx := context.WithoutCancel(ctx)
	t.Cleanup(func() { _ = q.Stop(stopCtx) })
	started.add(t, s.caps.Settle, q)
	return q
}

// started is every queue a case started through the suite, by the case, so a
// read that nothing more was delivered can settle each one first: staysAt is
// handed only the case's T. See [settleStarted].
var started = startedQueues{by: map[*testing.T]*caseQueues{}}

type startedQueues struct {
	mu sync.Mutex
	by map[*testing.T]*caseQueues
}

// caseQueues is what one case started, and the backend's Settle for them.
type caseQueues struct {
	settle func(ctx context.Context, t *testing.T, q queue.EventQueue) error
	queues []queue.EventQueue
}

func (r *startedQueues) add(t *testing.T, settle func(context.Context, *testing.T, queue.EventQueue) error, q queue.EventQueue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.by[t]
	if !ok {
		c = &caseQueues{settle: settle}
		r.by[t] = c
		// GONE WITH THE CASE: on the backend that ships a queue holds its
		// whole broker, and a registry nothing prunes keeps every broker
		// the binary ever started.
		t.Cleanup(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			delete(r.by, t)
		})
	}
	c.queues = append(c.queues, q)
}

// of returns what the case t started, and the Settle to settle it with.
func (r *startedQueues) of(t *testing.T) ([]queue.EventQueue, func(context.Context, *testing.T, queue.EventQueue) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.by[t]
	if !ok {
		return nil, nil
	}
	return slices.Clone(c.queues), c.settle
}

// settleStarted settles every queue the case t started through the suite and
// waits for none of them to be running a handler, so that a journal read after
// it holds everything those queues were going to hand over. It reports what it
// was still waiting for when it gave up.
//
// EVERY QUEUE THE CASE STARTED, never one it names: a peer's consumer is a
// delivery path as much as the queue under test's, and a case asserting that a
// detached client receives nothing is asserting about both.
func settleStarted(ctx context.Context, t *testing.T) error {
	t.Helper()
	queues, settle := started.of(t)
	if len(queues) == 0 {
		return errors.New("the case started no queue through the suite (s.start or " +
			"s.startQueue), so nothing was settled and a read behind it would decide nothing")
	}
	// BOUNDED AS LIVENESS by stallFor: a healthy backend settles once its
	// dispatch has come round, which the harnesses keep well inside it, and
	// one that never does — a mailbox it never empties, a loop it never ends
	// — is what fails here. A Settle shows no progress short of its answer,
	// so each is one pause, and each gets a stallFor of its own: a case
	// that started three queues takes three answers, and one bound shared
	// by all of them would be a wait's length again. The bound decides
	// nothing about what was delivered: the read behind it does.
	for _, q := range queues {
		settling, cancel := context.WithTimeout(ctx, stallFor)
		err := settle(settling, t, q)
		cancel()
		if err != nil {
			return fmt.Errorf("a %s queue did not settle: %w", q.Backend(), err)
		}
	}
	for _, q := range queues {
		draining, cancel := context.WithTimeout(ctx, stallFor)
		running, err := q.WaitForHandlers(draining, 0)
		cancel()
		switch {
		case err != nil:
			return fmt.Errorf("a settled %s queue still had %d handler(s) running when "+
				"the bound ended: %w", q.Backend(), running, err)
		case running != 0:
			return fmt.Errorf("a %s queue started %d handler(s) after it settled",
				q.Backend(), running)
		}
	}
	return nil
}

// awaitParked waits until q's consumer of (topic, group) has taken in the verbs
// the case sent it ([Capabilities.Parked]), and fails the case with what it was
// still waiting for otherwise. A case ends a window on its own clock behind it,
// so that what closes the window is the gate the verb shut and never the clock.
//
// BOUNDED AS LIVENESS by stallFor, a pause with nothing to show for it, which a
// consumer's loop coming round once is: it takes a verb in once it has, which
// the harnesses keep well inside it, and one that never does — a loop that
// ended rather than park, a gate it never reads — is what fails here. The
// bound decides nothing about what was delivered.
func (s *suite) awaitParked(ctx context.Context, t *testing.T, q queue.EventQueue, topic, group string) {
	t.Helper()
	parking, cancel := context.WithTimeout(ctx, stallFor)
	defer cancel()
	if err := s.caps.Parked(parking, t, q, topic, group); err != nil {
		t.Fatalf("the %s consumer of (%s, %s) did not take the case's verb in: %v",
			q.Backend(), topic, group, err)
	}
}

// holdReason is the pause-hold reason the cases take when one reason is all a
// case needs.
//
// The engine's own, so a case reads as the hold a node actually takes: a seat
// on a node with no turn engine pauses its inbox before requeuing, and that is
// the only reason the engine passes to PauseTopic today. The contract is about
// reasons in general, and a case that needs two uses a second one beside it.
const holdReason = "no_turn_engine"

// --- timing budgets -------------------------------------------------------

const (
	// stallFor is how long a positive wait goes WITHOUT PROGRESS before it is
	// a hang: no delivery recorded on a journal, or no answer from a wait with
	// no progress short of it (a signal, polled state, Settle, a drain).
	//
	// IT BOUNDS A PAUSE, NOT THE WAIT: a loaded runner pauses a healthy broker
	// (one answered in 6.59 s), and a 3 s total bound cut cases mid-delivery
	// (#1204). 30 s is 4.5x that and at most half of clockedWindow (a test holds
	// both). It costs a broken build time: ~340 cases stall past the Makefile's
	// 30-minute -timeout, so read the first printed failure. Redelivery spacing
	// binds one gap at a time (JetStream's shipped 30 s ceiling meets it): tune
	// the backend's constant, never this. A clock deciding a LOCAL answer's "at
	// once" would fail healthy builds.
	stallFor = 30 * time.Second

	// hangFor is the BACKSTOP a wait on a journal holds itself to while
	// deliveries keep arriving without meeting its condition: a redelivery
	// storm or a loop handing the same message round for ever, which restarts
	// stallFor on every lap.
	//
	// A healthy build never reaches it: most waits are for a handful of
	// deliveries after a possible pause (the longest, seven, is under a minute
	// at stallFor's measured 6.59 s), and the drain-overlap case's two hundred
	// messages arrive in one steady stream. A case waiting for many more PAUSED
	// deliveries in ONE wait should split it, not raise this.
	hangFor = 2 * time.Minute

	// quietFor is the timeout WaitForHandlers is handed where the case
	// needs one that is not zero: on an idle queue, which returns without
	// spending it, and beside a handler the case holds, which outlasts it.
	//
	// IT NO LONGER HOLDS A QUIET WINDOW OPEN for a negative assertion: on a slow
	// backend that passed because nothing had ARRIVED yet, a false PASS (a
	// redelivery after a dropped ack, or a dispatch 300 ms late, passed with the
	// bug present). So staysAt reads behind Settle and linger cases end their
	// window on LingerClock. Do not bring it back as a "wait for something"
	// budget: positive halves wait on a signal bounded by stallFor.
	quietFor = 150 * time.Millisecond

	// keepFor ends a case that waits on the keep of a dead letter at the
	// transport's ceiling, and only so that a hang ends: such a case is
	// decided by the keep's own completion, which a drain reports
	// (WaitForHandlers), never by this bound. Generous on purpose: a keep of
	// megabytes under the race detector on a loaded runner outlasted the
	// three seconds the suite's positive waits had then, which were sized
	// for a delivery of a few bytes, and a keep is one step with no
	// progress to see inside it, so stallFor's pause is no measure of it.
	keepFor = 2 * time.Minute

	// overlapRounds is how many publish/drain pairs the overlap case races
	// against each other.
	//
	// Enough that the interleaving it hunts — a handler starting exactly
	// while a drain observes an empty queue — happens many times in one
	// run, and small enough that the case costs a fraction of a second on
	// every backend. The bug it exists for surfaced roughly once in thirty
	// full-suite runs by accident; thirty deliberate attempts per backend
	// per run is a different order of likelihood.
	overlapRounds = 200

	// lingerFor is the batch linger window the batching subtests use: long
	// enough that a drain collects, inside one window, the mail already
	// waiting for it, short enough that waiting out several stays quick.
	//
	// NO CASE PUBLISHES INTO A LIVE WINDOW. Arrival-joining cases end it on a
	// clock they move (clockedWindow); the seven on this one hold their mail
	// before the drain starts (queueBacklog, or holdForOneBatch), so it bounds
	// only how long a drain collects what the broker already holds. The value
	// rests on a narrow base (ten clean JetStream repeats of the Batch group;
	// the twin informs least): a case needing more publishes per window should
	// say so. The suite never asks above queue.MaxLingerSeconds, which backends
	// size their dispatch budget to and rightly refuse rather than clamp.
	lingerFor = 50 * time.Millisecond

	// handBackLinger is the window a case opens when the drain it waits for
	// has to carry a HANDED-BACK message beside fresh ones
	// (a_redelivered_event_rejoins_its_conversation_in_timestamp_order and
	// a_partition_at_mixed_counts_tells_each_message_its_own_headroom).
	//
	// IT BOUNDS A BROKER TAKING A RETURN IT HAS ALREADY BEEN SENT. Both cases
	// hand back by DEFERRAL and fetch again once it is applied. On JetStream a
	// broker goroutine apart from the fetch server takes it, so the return must
	// land inside the window: one internal hop, cheaper than a publish round
	// trip (200-330ms worst under -race), so this sits above that. A FAILURE
	// hand-back would race the backend's backoff (queue.OrderForDispatch).
	handBackLinger = 400 * time.Millisecond

	// clockedWindow is the linger a case gets whose window ends on a clock
	// it moves ([Capabilities.LingerClock]): the contract's ceiling,
	// queue.MaxLingerSeconds, which on that clock costs nothing.
	//
	// THE CEILING, SO A CLOCK THE BACKEND IGNORED SHOWS. A backend keeping the
	// window on the wall clock would end a short window by itself a moment later,
	// and a case could pass as though it had moved the clock. A window of a
	// minute outlasts the pause any wait here allows (stallFor, half of it) and
	// delivers nothing while open, so a case only the clock can end fails
	// instead. No size of wall-clock window orders a verb before its end: a
	// constant sized against the quickest backend is how a suite acquires a race
	// it cannot see.
	clockedWindow = time.Duration(queue.MaxLingerSeconds) * time.Second
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

	// streamed marks a stream subscriber's journal (streamTo), which
	// staysAt refuses: a feed is no consume loop, so Settle does not wait it
	// out. Set before the journal is handed to a backend, and read on the
	// case's own goroutine.
	streamed bool
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

// await blocks until cond accepts what has been recorded, failing the test
// with the last observation if delivery stops short of it ([awaitDeliveries]).
func (j *journal) await(t *testing.T, what string, cond func(seen []string) bool) {
	t.Helper()
	j.awaitExpecting(t, what, cond, nil)
}

// awaitExpecting is await plus, when the caller has one, a judge of what was
// seen against what it waits for ([judge]) — which is the only thing that lets
// a timeout tell a lagging delivery from a wrong one, and lets a wait end the
// moment what it saw can no longer become what it waits for. Passed down
// rather than stored on the journal so two awaits on one journal cannot
// inherit each other's expectation.
func (j *journal) awaitExpecting(t *testing.T, what string, cond func(seen []string) bool, judged judge[string]) {
	t.Helper()
	if ended := j.waitOn(cond, judged); ended != endMet {
		seen := j.all()
		timedOut(t, what, ended, judged.verdict(seen), seen)
	}
}

// waitOn is awaitExpecting's wait, without its failure: how it ended.
func (j *journal) waitOn(cond func(seen []string) bool, judged judge[string]) waitEnd {
	return awaitDeliveries(func() bool { return cond(j.all()) }, judged.dooms(j.all), j.ping)
}

// judge classifies what a journal has recorded against what a wait expects:
// verdictPartial while more deliveries could still meet it, verdictDiverged
// once none could.
//
// DIVERGED IS FINAL, AND A JUDGE MUST ONLY SAY IT WHEN IT IS: a journal only
// grows, so a judge reports verdictDiverged only for a record no later
// delivery could turn into the one expected — a sequence that went wrong, a
// label delivered more times than it is expected. That is what lets the wait
// end on it at once ([endDiverged]) rather than at a bound; a judge that said
// it of a record still able to meet the expectation would fail a healthy case.
// A nil judge has no expectation and concludes nothing.
type judge[T any] func(seen []T) deliveryVerdict

// dooms is the condition a wait ends on as diverged, read through all; nil for
// a wait with no judge, which never ends so.
func (j judge[T]) dooms(all func() []T) func() bool {
	if j == nil {
		return nil
	}
	return func() bool { return j(all()) == verdictDiverged }
}

// verdict classifies seen for a timeout: nothing, before anything else, and
// verdictUnknown under no judge, since an opaque condition genuinely cannot
// say whether more was in flight, and saying so is better than picking.
func (j judge[T]) verdict(seen []T) deliveryVerdict {
	switch {
	case len(seen) == 0:
		return verdictNothing
	case j == nil:
		return verdictUnknown
	}
	return j(seen)
}

// inOrder judges against an exact sequence: a strict prefix of want is
// partial, anything else has diverged.
func inOrder[T comparable](want []T) judge[T] {
	return func(seen []T) deliveryVerdict {
		for i, got := range seen {
			if i >= len(want) || want[i] != got {
				return verdictDiverged
			}
		}
		return verdictPartial
	}
}

// inAnyOrder judges against a multiset: what was seen, each label no more
// often than want holds it, is partial; a label want does not hold, or holds
// fewer times, has diverged.
func inAnyOrder(want []string) judge[string] {
	return func(seen []string) deliveryVerdict {
		left := make(map[string]int, len(want))
		for _, label := range want {
			left[label]++
		}
		for _, label := range seen {
			if left[label] == 0 {
				return verdictDiverged
			}
			left[label]--
		}
		return verdictPartial
	}
}

// waitEnd is how a positive wait ended.
type waitEnd int

const (
	// endMet — the condition held.
	endMet waitEnd = iota
	// endStalled — nothing moved for stallFor: no delivery, for a wait on a
	// journal; no answer, for a wait that sees nothing short of one.
	endStalled
	// endBackstop — deliveries kept arriving for hangFor and none of them
	// met the condition.
	endBackstop
	// endDiverged — what was recorded can no longer meet the condition: a
	// judge said so ([judge]), and a journal only grows.
	endDiverged
)

// how says, for a failure, what ended a wait whose condition never held. unmet
// is a wait that records no deliveries, so its pause is one with no answer.
func (e waitEnd) how(unmet bool) string {
	switch e {
	case endBackstop:
		return fmt.Sprintf("deliveries kept arriving for %s (hangFor, the backstop) and "+
			"none met the condition", hangFor)
	case endDiverged:
		return "what was delivered can no longer become what was expected"
	case endStalled:
		if unmet {
			return fmt.Sprintf("the condition did not hold for %s (stallFor)", stallFor)
		}
		return fmt.Sprintf("nothing was delivered for %s (stallFor)", stallFor)
	}
	return "the condition held"
}

// awaitDeliveries waits until met holds, reading it again whenever delivered
// says a delivery was recorded, and reports how the wait ended: endMet, or the
// bound that ended it, or endDiverged once diverged (when not nil) says no
// later delivery could meet it.
//
// BOUNDED BY PROGRESS, NOT BY ELAPSED TIME: every delivery restarts the
// stallFor bound, so a slow loaded runner is never cut while still delivering
// (#1204). hangFor from the start backstops a backend that keeps delivering and
// never meets the condition. Both are LIVENESS; met, diverged and the verdict
// decide what was delivered. A delivery recorded before the wait began restarts
// the bound once at the start: what met reads first already holds it.
func awaitDeliveries(met, diverged func() bool, delivered <-chan struct{}) waitEnd {
	stalled := time.NewTimer(stallFor) //fixedwait:liveness
	defer stalled.Stop()
	backstop := time.NewTimer(hangFor) //fixedwait:liveness
	defer backstop.Stop()
	// READ ON EVERY WAKE, met first: a condition met by the delivery that
	// also ended a bound is met, never a timeout, and one met by a record
	// a judge would also call diverged is met too.
	decided := func() (waitEnd, bool) {
		switch {
		case met():
			return endMet, true
		case diverged != nil && diverged():
			return endDiverged, true
		}
		return 0, false
	}
	ending := func(bound waitEnd) waitEnd {
		if end, ok := decided(); ok {
			return end
		}
		return bound
	}
	for {
		if end, ok := decided(); ok {
			return end
		}
		select {
		case <-delivered:
			// Reset discards a firing nobody received (Go 1.23's timers),
			// so a stall the delivery raced is not read after it.
			stalled.Reset(stallFor)
		case <-stalled.C:
			return ending(endStalled)
		case <-backstop.C:
			return ending(endBackstop)
		}
	}
}

// awaitLabels waits for exactly this sequence of labels, in order.
func (j *journal) awaitLabels(t *testing.T, what string, want ...string) {
	t.Helper()
	j.awaitExpecting(t, what, func(seen []string) bool { return equalStrings(seen, want) },
		inOrder(want))
}

// awaitLabelsInAnyOrder waits for exactly these labels, in whatever sequence.
//
// For cases whose subject is WHAT was delivered, not in what order (chiefly a
// redelivery racing a fresh publish, whose order rests on a backoff measured in
// wall-clock time). The order the contract does state is certified by
// a_failure_returns_behind_waiting_mail_and_a_deferral_at_the_head, which holds
// the mail before publishing. The expectation is passed down as a multiset
// (inAnyOrder), so a wait ends at once on a delivery that CONTRADICTS it
// (unexpected, or too often) rather than merely lags; a missing event fails
// once delivery stops short of it.
func (j *journal) awaitLabelsInAnyOrder(t *testing.T, what string, want ...string) {
	t.Helper()
	j.awaitExpecting(t, what, func(seen []string) bool {
		return sameLabels(seen, want)
	}, inAnyOrder(want))
}

// sameLabels compares two label lists as MULTISETS: a duplicate delivery is a
// difference, because bounded duplication is a fact a case may be asserting
// about rather than noise to fold away.
func sameLabels(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, label := range a {
		counts[label]++
	}
	for _, label := range b {
		counts[label]--
		if counts[label] < 0 {
			return false
		}
	}
	return true
}

// staysAt fails unless delivery stays at want, read once every queue the case
// started has settled ([Capabilities.Settle]) and is running no handler.
//
// A negative assertion ("a paused attachment delivers nothing") has no
// completion to wait on, so it is read behind the backend's report that nothing
// more is coming, not after a quiet window, which a late leak passed. It does
// not claim no duplicate ever arrives (bounded duplication, not exactly-once),
// but an ack the broker has not taken leaves the message in its mailbox, which
// Settle does not read as settled, so an ack that did not stop a retry fails.
// A stream subscriber's journal is refused, as nothing settles a feed: read one
// behind a fence ([journal.fenced]).
func (j *journal) staysAt(ctx context.Context, t *testing.T, want int, what string) {
	t.Helper()
	if j.streamed {
		t.Fatalf("%s: staysAt cannot read a stream subscriber's journal: a feed is no "+
			"consume loop, so Settle does not wait it out and the read would decide "+
			"nothing; read it behind a fence (journal.fenced)", what)
	}
	if err := settleStarted(ctx, t); err != nil {
		t.Fatalf("%s: delivery could not be read as staying at %d, because %v; "+
			"handlers saw %v", what, want, err, j.all())
	}
	if got := j.all(); len(got) != want {
		t.Fatalf("%s: expected delivery to stay at %d, saw %v", what, want, got)
	}
}

// fenceLabel is the type of the event journal.fenced publishes, and with the
// subject it goes to, its label in a stream subscriber's journal.
const fenceLabel = "queuetest_fence"

// fenced fails unless a stream subscriber's journal holds exactly want, read
// behind a FENCE: an event published to subject after everything the case
// published, which the subscriber has to receive after all of it.
//
// A feed hands its subscriber what its stream holds in the stream's own order,
// one call at a time (on the backend that ships, one ephemeral consumer per
// subscription, delivering to a serial callback; on the twin, inline in each
// Publish). So once the fence has arrived, everything the stream held ahead of
// it has too, and whatever a broken match let through is already in the
// journal. subject must be one the case's pattern matches, on the stream the
// events under test were published to.
func (j *journal) fenced(ctx context.Context, t *testing.T, q queue.EventQueue, what, subject string, want ...string) {
	t.Helper()
	publish(ctx, t, q, subject, newEvent(fenceLabel))
	fence := subject + "/" + fenceLabel
	all := append(slices.Clone(want), fence)
	j.awaitExpecting(t, "the fence published behind the case's events",
		func(seen []string) bool { return slices.Contains(seen, fence) }, inOrder(all))
	if got := j.all(); !equalStrings(got, all) {
		t.Fatalf("%s: the subscriber received %v by the fence, want %v", what, got, all)
	}
}

// deliveriesObserved counts every delivery this suite records, across every
// case in the run. It is the evidence a timeout needs and cannot otherwise get:
// a backend that delivered thousands of events elsewhere in the same run has
// demonstrably not been starved by the bound that ended this case.
var deliveriesObserved atomic.Int64

// deliveryVerdict is what a failed wait can honestly conclude about its cause.
type deliveryVerdict int

const (
	// verdictNothing — nothing arrived at all.
	verdictNothing deliveryVerdict = iota
	// verdictPartial — what arrived is still on its way to what was
	// expected: a strict prefix of a sequence, a part of a multiset.
	verdictPartial
	// verdictDiverged — what arrived contradicts what was expected, and no
	// later delivery can take it back ([judge]).
	verdictDiverged
	// verdictUnknown — something arrived under an opaque condition, so
	// whether more was still in flight cannot be determined.
	verdictUnknown
	// verdictUnmet — a wait that records no deliveries (on backend state,
	// on a signal) and whose condition never held, so it concludes nothing
	// about them.
	verdictUnmet
)

// pauseNote is what a failure on a wait for deliveries says of the bound that
// ended it.
const pauseNote = "stallFor is a constant this suite advertises, not a promise the " +
	"contract makes, and on a wait for deliveries it bounds a PAUSE rather than the " +
	"wait: every delivery restarts it, so a backend still delivering, however slowly, " +
	"is never cut by it. Raising it is almost never the repair. Measured, the pressure " +
	"on it is a loaded runner's pause (6.59 s at worst, through a leaf under -race) and " +
	"REDELIVERY SPACING: check your backend's redelivery delay and its ceiling first " +
	"(JetStream's harness cuts a 1s nak delay, doubling to 30s, to 25ms and 50ms for " +
	"exactly this reason)."

// answerNote is what a failure on a wait for one answer — a signal, a polled
// state — says of the bound that ended it.
const answerNote = "stallFor is a constant this suite advertises, not a promise the " +
	"contract makes. This wait sees no progress short of its answer, so it allows the " +
	"answer one pause, and a loaded runner's measured worst (6.59 s, through a leaf " +
	"under -race) is far inside it. Raising it is almost never the repair."

// timedOut ends a case whose positive wait ended (ended) before its condition
// held, naming only what the evidence supports.
//
// It asserts no cause from a prefix: for a case awaiting a SEQUENCE, a backend
// slower than the bound delivers e1 and not yet e2, which is "something
// arrived, condition unmet", so a prefix is reported as a prefix and nothing is
// concluded from it. The no-delivery case cites how many deliveries this
// backend managed elsewhere rather than offering advice. Only a record a judge
// has called diverged, which no later delivery could mend, clears the bound.
func timedOut(t *testing.T, what string, ended waitEnd, v deliveryVerdict, observed any) {
	t.Helper()
	how := ended.how(v == verdictUnmet)
	switch v {
	case verdictPartial:
		t.Fatalf("timed out waiting for %s: %s; delivery is INCOMPLETE, not wrong: "+
			"handlers saw %v, which is still on its way to what was expected\n"+
			"\tThat is what a backend slower than this bound produces as much as a wrong "+
			"one, so timing and behaviour are both live. This helper cannot tell them apart "+
			"and does not guess. %s", what, how, observed, pauseNote)
	case verdictDiverged:
		t.Fatalf("waiting for %s: %s; handlers saw %v, which CONTRADICTS what was "+
			"expected rather than lagging it\n"+
			"\tDelivery happened and was wrong, so no bound is the suspect.",
			what, how, observed)
	case verdictUnknown:
		t.Fatalf("timed out waiting for %s: %s; handlers saw %v under a condition this "+
			"helper cannot decompose\n"+
			"\tWhether more was still in flight is unknown, so treat timing and behaviour "+
			"as both live. %s", what, how, observed, pauseNote)
	case verdictUnmet:
		t.Fatalf("timed out waiting for %s: %s\n"+
			"\tThis wait records no deliveries, so it has none to report and cannot tell "+
			"a backend slower than this bound from a wrong one: treat timing and "+
			"behaviour as both live. %s", what, how, answerNote)
	default:
		seen := deliveriesObserved.Load()
		evidence := fmt.Sprintf("This backend delivered %d events elsewhere in this run, "+
			"so it is evidently delivering — look at this case's own subject first.", seen)
		if seen == 0 {
			evidence = "This backend has delivered NOTHING anywhere in this run, so suspect " +
				"its setup or this bound before this case."
		}
		t.Fatalf("timed out waiting for %s: %s, from the wait's start\n\t%s %s",
			what, how, evidence, pauseNote)
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
	b.awaitExpecting(t, what, cond, nil)
}

// awaitExpecting is [journal.awaitExpecting] for batches, bounded alike by
// progress ([awaitDeliveries]): every batch recorded restarts the pause. Its
// judge reads the batches' sizes.
func (b *batchJournal) awaitExpecting(t *testing.T, what string, cond func(batches [][]string) bool, judged judge[int]) {
	t.Helper()
	if ended := b.waitOn(cond, judged); ended != endMet {
		timedOut(t, what, ended, judged.verdict(b.sizes()), b.all())
	}
}

// waitOn is awaitExpecting's wait, without its failure: how it ended.
func (b *batchJournal) waitOn(cond func(batches [][]string) bool, judged judge[int]) waitEnd {
	return awaitDeliveries(func() bool { return cond(b.all()) }, judged.dooms(b.sizes), b.ping)
}

func (b *batchJournal) awaitSizes(t *testing.T, what string, want ...int) {
	t.Helper()
	b.awaitExpecting(t, what, func([][]string) bool { return equalInts(b.sizes(), want) },
		inOrder(want))
}

// staysAt is [journal.staysAt] for batches: it fails unless the batch count
// stays at want, read once every queue the case started has settled.
func (b *batchJournal) staysAt(ctx context.Context, t *testing.T, want int, what string) {
	t.Helper()
	if err := settleStarted(ctx, t); err != nil {
		t.Fatalf("%s: the batch count could not be read as staying at %d, because %v; "+
			"handlers saw %v", what, want, err, b.all())
	}
	if got := b.all(); len(got) != want {
		t.Fatalf("%s: expected batch count to stay at %d, saw %v", what, want, got)
	}
}

// --- state assertions -----------------------------------------------------

// awaitSignal waits for a channel to close, failing the test rather than
// hanging its binary.
//
// BOUNDED BY stallFor, the pause every positive wait allows: a signal is one
// step, with no progress to see short of it, so the wait for it is one pause.
//
// Every unbounded `<-ch` in a shared suite is a ten-minute timeout and a
// goroutine dump for whichever backend author trips it, on a day they were
// debugging something else. rescue runs first on the failure path, to unblock
// whatever the suite is holding so the process can still exit and report; a
// case that holds nothing a cleanup will not release passes nil, which a
// timeout must report rather than call.
func awaitSignal(t *testing.T, ch <-chan struct{}, what string, rescue func()) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(stallFor): //fixedwait:liveness
		if rescue != nil {
			rescue()
		}
		// The case closes some of these channels itself, so a signal that
		// never came says nothing about what was delivered.
		timedOut(t, what, endStalled, verdictUnmet, nil)
	}
}

// awaitState waits on a capability-supplied view of backend state.
//
// Nothing calls back when a deferral has been applied to a backlog, so this
// genuinely polls rather than pretending a channel exists.
//
// BOUNDED BY stallFor, as a signal is: the state a case polls is one step (a
// broker reading back what an acknowledged publish stored, a verb applied, a
// window opened), and the only progress the poll sees is the condition
// holding. A case whose state moves through MANY hand-overs waits on a journal
// instead, whose every delivery restarts the bound (#1197).
func awaitState(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(stallFor)
	for {
		if cond() {
			return
		}
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			// State the suite polls has no "saw something" half — the
			// condition is simply still false — so this always reports the
			// ambiguous case, which it genuinely is. Not verdictNothing,
			// whose "NOTHING was delivered" this poll cannot know: most
			// cases wait here on state their handlers were already handed
			// events to move, and that sentence sent the reader to the
			// case's subject and setup.
			timedOut(t, what, endStalled, verdictUnmet, nil)
		}
		time.Sleep(2 * time.Millisecond) //fixedwait:liveness
	}
}

// --- withdrawals ----------------------------------------------------------

// heldHandler is a registration — a listener or an answerer — whose handler a
// case holds inside the first message it is handed, until release.
type heldHandler struct {
	stop queue.Unsubscribe
	// release lets the held handler return. Idempotent, and registered as a
	// cleanup by whatever built the registration, so a case that fails with
	// the handler held does not leave its queue's stop waiting on it.
	release func()
	// finished is set as the handler returns.
	finished *atomic.Bool
}

// withdraw runs h's withdrawal under bound on a goroutine of its own, calls
// meanwhile (when set) while it runs, and returns what the withdrawal answered.
//
// BOUNDED AS LIVENESS, through awaitSignal, which releases the held handler
// before it fails the case: a withdrawal that ignores its bound and waits on
// that handler for ever is what it ends. Whether a withdrawal waited ON ITS
// BOUND is read off its answer, never off how soon it came: a case that hands
// it a bound already ended cancels that bound, so only a withdrawal that
// waited on it answers context.Canceled, while one that waited out a grace of
// its own answers that grace's own error, however short the grace.
func withdraw(bound context.Context, t *testing.T, h heldHandler, what string, meanwhile func()) error {
	t.Helper()
	answered := make(chan struct{})
	var err error
	go func() {
		defer close(answered)
		err = h.stop(bound)
	}()
	if meanwhile != nil {
		meanwhile()
	}
	awaitSignal(t, answered, what, h.release)
	return err
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
// The key is a STRING, like every other free-form payload value this suite
// publishes: a string survives any codec unchanged, so these fixtures cannot
// see a payload change type or go missing in transit, and a suite whose
// fixtures already have the backend's shape is not testing the boundary.
// Wire/a_free_form_payload_value_survives_whatever_type_it_lands_as is the case
// that does look; keep additions here string-keyed and leave the wire question
// to it.
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
func tryPublish(ctx context.Context, q queue.EventQueue, topic string, ev *events.Event) {
	_ = q.Publish(ctx, topic, ev)
}

func publish(ctx context.Context, t *testing.T, q queue.EventQueue, topic string, ev *events.Event) {
	t.Helper()
	if err := q.Publish(ctx, topic, ev); err != nil {
		t.Fatalf("Publish(%s, %s): %v", topic, ev.Type, err)
	}
}

// attachmentOf is the pair's current attachment, which a case's hold names.
// It reports a refusal with Errorf, so a handler's goroutine may call it, and
// then answers the zero Attachment, which every PauseTopic refuses.
func attachmentOf(ctx context.Context, t testing.TB, q queue.EventQueue, topic, group string) queue.Attachment {
	t.Helper()
	at, err := q.Attachment(ctx, topic, group)
	if err != nil {
		t.Errorf("Attachment(%s, %s): %v", topic, group, err)
	}
	return at
}

func subscribe(ctx context.Context, t *testing.T, q queue.EventQueue, topic, group string, h queue.Handler) {
	t.Helper()
	if err := q.Subscribe(ctx, topic, group, h); err != nil {
		t.Fatalf("Subscribe(%s, %s): %v", topic, group, err)
	}
}

func subscribeBatch(
	ctx context.Context,
	t *testing.T,
	q queue.EventQueue,
	topic, group string,
	h queue.BatchHandler,
	opts *queue.BatchOptions,
) {
	t.Helper()
	if err := q.SubscribeBatch(ctx, topic, group, h, convKey, opts); err != nil {
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
	// Bound to this test's own T so a case keeps calling it with three
	// arguments — including from inside a polling predicate, which is where
	// most of these reads happen and where returning an error would have to
	// be swallowed all over again.
	return func(q queue.EventQueue, topic, group string) []*events.Event {
		return s.caps.Backlog(t, q, topic, group)
	}
}

// optionalBacklog is needBacklog for a case that runs either way: it returns
// nil when the backend reports no backlog, rather than skipping the case.
func (s *suite) optionalBacklog(t *testing.T) func(q queue.EventQueue, topic, group string) []*events.Event {
	t.Helper()
	if s.caps.Backlog == nil {
		return nil
	}
	return func(q queue.EventQueue, topic, group string) []*events.Event {
		return s.caps.Backlog(t, q, topic, group)
	}
}

// optionalProposals is the consumer-proposal counter, or nil when the backend
// does not keep one.
//
// OPTIONAL rather than a skip, deliberately: the case that reads it also
// certifies that re-ensuring a mailbox keeps the mail, which is the contract
// itself and must not be skipped over a missing observable. A backend that
// cannot count proposals still has to answer the half that is written down.
func (s *suite) optionalProposals(t *testing.T) func(q queue.EventQueue) int {
	t.Helper()
	if s.caps.ConsumerProposals == nil {
		return nil
	}
	return s.caps.ConsumerProposals
}

func (s *suite) needDeadLetters(t *testing.T) func(q queue.EventQueue, topic, group string) []*events.Event {
	t.Helper()
	if s.caps.DeadLetters == nil {
		t.Skip("backend cannot report dead letters")
	}
	return func(q queue.EventQueue, topic, group string) []*events.Event {
		return s.caps.DeadLetters(t, q, topic, group)
	}
}

// needAttempts returns a constructor for a queue that gives a persistently
// failing event exactly attempts deliveries before dead-lettering it.
func (s *suite) needAttempts(t *testing.T) func(t *testing.T, attempts int) queue.EventQueue {
	t.Helper()
	if s.caps.WithDeliveryAttempts != nil {
		return s.caps.WithDeliveryAttempts
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

// needLingering returns the view of whether a batch is held in an open linger
// window that has outlasted what a window of nothing collects — the state a
// window case publishes into.
func (s *suite) needLingering(t *testing.T) func(q queue.EventQueue, topic, group string) bool {
	t.Helper()
	if s.caps.Lingering == nil {
		t.Skip("backend cannot report whether a batch is held in an open linger window")
	}
	return s.caps.Lingering
}

// needInWindow returns the view of how many events an open linger window
// holds — the state a coalescing case ends its window behind.
func (s *suite) needInWindow(t *testing.T) func(q queue.EventQueue, topic, group string) int {
	t.Helper()
	if s.caps.InWindow == nil {
		t.Skip("backend cannot report how many events an open linger window holds")
	}
	return s.caps.InWindow
}

// needLingerClock returns a constructor for a queue whose linger windows end on
// a clock the case moves, skipping the case unless the backend can also report
// its consumer parked, which a case moving that clock behind a pause or a hold
// waits for first ([suite.awaitParked]).
func (s *suite) needLingerClock(t *testing.T) func(t *testing.T, clock *Clock) queue.EventQueue {
	t.Helper()
	if s.caps.LingerClock == nil || s.caps.Parked == nil {
		t.Skip("backend cannot end a linger window on a clock the case moves, " +
			"or cannot report its consumer parked behind a verb")
	}
	return s.caps.LingerClock
}

// needStreamSubscriptions returns the count of a client's stream subscriptions
// still able to hand their handler a message — the state an unsubscribed feed
// is read off.
func (s *suite) needStreamSubscriptions(t *testing.T) func(q queue.EventQueue) int {
	t.Helper()
	if s.caps.StreamSubscriptions == nil {
		t.Skip("backend cannot report which of a client's stream subscriptions are still consuming")
	}
	return s.caps.StreamSubscriptions
}

// needListenBarrier returns the barrier a listener's silence is read behind
// where no broadcast can fence it — a withdrawn listener's.
func (s *suite) needListenBarrier(t *testing.T) func(ctx context.Context, q queue.EventQueue) {
	t.Helper()
	if s.caps.ListenBarrier == nil {
		t.Skip("backend cannot report its listeners having handled what they were handed")
	}
	return func(ctx context.Context, q queue.EventQueue) {
		// BOUNDED AS LIVENESS by stallFor: the barrier waits only for
		// handlers to return, and no case holds one while it asks, so
		// only a barrier that never passes meets the bound — and fails.
		bounded, cancel := context.WithTimeout(ctx, stallFor)
		defer cancel()
		s.caps.ListenBarrier(bounded, t, q)
	}
}

// needAnswering returns the view of what a client's registrations on a
// subject are running and holding for a slot — the state a bound is read off.
func (s *suite) needAnswering(t *testing.T) func(q queue.EventQueue, subject string) (running, waiting int) {
	t.Helper()
	if s.caps.Answering == nil {
		t.Skip("backend cannot report what a registration holds for a slot")
	}
	return s.caps.Answering
}

func (s *suite) needPeer(t *testing.T) func(t *testing.T, q queue.EventQueue) queue.EventQueue {
	t.Helper()
	if s.caps.Peer == nil {
		t.Skip("backend cannot model a second node on the same broker")
	}
	return s.caps.Peer
}
