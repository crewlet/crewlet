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
// speak (a channel wake, not a poll), and a negative assertion is read behind
// something that would have carried what it rules out — a later broadcast of
// the same sender, a fence through the same feed, a barrier through the
// listeners, a withdrawal's answer to its own bound, the backend's own count,
// its own report that it has settled ([Capabilities.Settle]). Where nothing a
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
// Several of the properties this suite certifies are about state the contract
// deliberately does not expose — the mail a subscription retains, the reasons
// holding an attachment paused, which seats a node is serving. Those are the
// properties seat ownership rests on, so they must be asserted; they are just
// not things a producer or consumer asks the queue. A backend supplies them
// here, and a subtest whose capability is absent skips with a named reason
// rather than silently passing.
//
// [Capabilities.Settle] is the one a backend must supply, and [RunWith] refuses
// a backend without it: every case asserting that nothing more was delivered
// reads behind it. Every other field may be left zero, which runs everything
// that needs nothing but the contract and Settle.
type Capabilities struct {
	// Settle returns once q will dispatch nothing more until the next verb
	// is sent to it: every handler call it already owes has been made. It
	// fails, naming what it was still waiting for, if ctx ends first.
	//
	// THE SIGNAL A NEGATIVE ASSERTION IS READ BEHIND. "This must NOT be
	// delivered" has no completion of its own to wait on, so the cases that
	// assert it held a quiet window open instead — and a window is a bet on
	// the backend's latency: an event the bug let through that landed after
	// it, a redelivery after its ack window or a dispatch a loaded runner
	// scheduled late, passed the case with the bug present. Read behind
	// Settle, the journal holds everything the backend was ever going to
	// hand over, so a leak is in it however late it came.
	//
	// "The next verb" is the case's: a verb a handler sends is part of the
	// dispatch Settle waits out. What it does not cover is what reaches a
	// handler other than a subscription's — a stream subscriber's feed, a
	// listener, an answerer — which the cases fence by their own means.
	//
	// The suite waits for [queue.EventQueue.WaitForHandlers] after it as
	// well, so a backend whose report a running handler could outlive is
	// still read behind that handler. On the twin it is its broker going
	// idle, no drain running and no linger window open; on the backend that
	// ships, every consume loop ended, parked at its gate, or its mailbox
	// empty.
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
	// It asks for TOTAL ATTEMPTS — an observable — rather than for a
	// "budget", because a budget is not one number and the contract never
	// says which one it is. The in-process twin counts redeliveries AFTER
	// the first delivery (24 means 25 attempts); NATS MaxDeliver counts
	// deliveries INCLUDING the first (25 means 25). Both numbers live in
	// this repo, in different documents, meaning different things.
	//
	// The earlier form of this field named a budget and defined the
	// convention in this comment, which made every backend that passed
	// agree with the suite because the suite told it to — the agreement was
	// real code and read exactly like evidence for a convention nothing had
	// decided. Naming the observable lets each backend translate from
	// whatever its broker counts, and leaves the suite asserting only what
	// it can actually see.
	WithDeliveryAttempts func(t *testing.T, attempts int) queue.EventQueue

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
	// it must keep — and
	// ensure_subscription_on_an_existing_one_keeps_its_mail_and_proposes_nothing
	// reported PASS with the wipe in place.
	//
	// So the hazard is worse than "the group goes quiet". The group gets LOUD
	// somewhere else, the noise is attributed to the lag, and the differential
	// cases stay green while proving nothing — which is exactly the pair of
	// symptoms that gets the wrong one investigated.
	//
	// Stated here because no case can discover it — and THE REASON GIVEN FOR
	// THAT WAS ITSELF WRONG, which is worth more than the conclusion it
	// supported. This paragraph used to say both backends made the property
	// true for free: the twin because it is a mutex over a map, and the
	// broker-backed one because it "reads statistics synchronously". The
	// twin's half is right. The broker-backed one's was not. It sized a
	// backlog read from its durable CONSUMER's pending count, and a consumer's
	// accounting is not caught up when the publish that produced it has
	// already been acked — the server stores the message and acks the
	// publisher, and the consumer's own count follows after. Measured on the
	// embedded broker: immediately after an acked Publish the consumer
	// reported nothing pending on roughly two reads in three from another
	// connection, and about one in nine from the publisher's own.
	//
	// So the one backend that ships violated the requirement stated here for
	// as long as this comment claimed it could not. It cost a red CI on a
	// merge that touched nothing in this package:
	// quiesce_with_no_attachment_changes_nothing read an empty baseline, the
	// publish became visible a fraction of a millisecond later, and Quiesce —
	// which with nothing attached writes nothing at all — was reported as
	// having changed the mail. Note which case: the two OTHER users of
	// assertUntouched wait for their baseline to settle before snapshotting,
	// so a lagging view reached the suite through the only one that did not.
	//
	// The lesson is the one above, sharpened. A requirement nothing asserts is
	// held up by a comment, so the comment has to be re-derived from the code
	// rather than inherited: "reads statistics synchronously" was plausible,
	// unchecked, and the exact opposite of what the code did. Nothing here can
	// tell a backend that GUARANTEES this from one that merely happens to,
	// which is the same blindness as fixtures shaped like the backend, reached
	// the same way — and the only remedy is still to write the requirement
	// down where the capability is supplied, now with what it costs when it is
	// written down wrongly.
	//
	// It is deliberately a requirement on the CAPABILITY and not on
	// EventQueue: nothing in the contract promises when a completed operation
	// becomes observable, and this suite has no business inventing that
	// promise. A backend that cannot honour it should leave these nil and
	// skip the group rather than supply a lagging view of itself.

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
	// THE STATE A WINDOW CASE PUBLISHES INTO. A case asking whether a window
	// coalesces has to publish its next event into the window, and after
	// what a window of nothing still collects: a pull backend's drain runs a
	// tail fetch at any window, zero included, and a backend that ignored
	// the linger coalesced an event that landed during it. Those cases slept
	// a fixed gap to get past it, and a runner that scheduled the drain later
	// than the gap had it fetch both events at once, so the case passed
	// whether the window did anything or not. A backend whose window never
	// opens never reports one, so a case waiting on this fails rather than
	// passing on nothing.
	Lingering func(q queue.EventQueue, topic, group string) bool

	// InWindow reports how many events this client holds for a subscription
	// in an open linger window — the batch the window hands over when it
	// ends — and zero while no window of it is open.
	//
	// THE STATE A COALESCING CASE ENDS ITS WINDOW BEHIND. A case asking
	// whether a window coalesces publishes its next event into the window
	// and ends the window on a clock it moves (LingerClock) — but only once
	// the window holds that event: on a backend that fetches, published is
	// not yet collected, and a window ended a moment early hands over the
	// first event alone, which the case would read as a window that did not
	// coalesce. Those cases raced a window of a second on the wall clock
	// instead, and a runner that published the second event late split the
	// batch and failed a healthy build. A backend that never counts what its
	// window holds fails the case's wait rather than passing it.
	InWindow func(q queue.EventQueue, topic, group string) int

	// LingerClock returns a fresh, UNSTARTED queue whose batch linger windows
	// end on clock rather than on the wall clock: a window opened on it ends
	// once the case has moved clock past the window's end ([Clock.Advance]),
	// and never before, however long the case takes to get there. Like
	// WithDeliveryAttempts it builds the queue, because the clock is fixed at
	// construction.
	//
	// THE CLOCK A CASE ENDS A WINDOW ON, BEHIND A VERB. What a pause, a hold
	// or a stop does to a window that is open when it lands — and whether a
	// window coalesces what is published into it — is asked by a case that
	// needs the window open at the verb and ended after it, and on the wall
	// clock nothing orders the two. Those cases bought slack with a window of
	// a second (five, for the stop) and still lost the race on a loaded
	// runner: the window ended before the verb landed, and a healthy build
	// failed — the pause and the stop reporting it as the suite's own lost
	// race, in the very words a backend that flushed past the verb was
	// reported in under the same delay. On this clock the case lands its
	// verb, waits for the backend to have taken it in (Parked, or InWindow
	// for a publish), and only then ends the window.
	//
	// ONLY THE WINDOW'S END IS ON IT. Every other wait a backend keeps — a
	// fetch's, a redelivery's spacing, an ack window — stays on the wall
	// clock, so a window the case holds open still costs the backend's
	// fetches real time, and a case ends one well inside the backend's ack
	// window rather than at leisure.
	LingerClock func(t *testing.T, clock *Clock) queue.EventQueue

	// Parked returns once q's consumer of (topic, group) has taken in the
	// verbs sent to q before the call: it stands with its gate shut and
	// holds nothing it took before the gate shut, so it hands a handler
	// nothing until a verb opens the gate again. It fails, naming what it was
	// still waiting for, when ctx ends first — and at once on a pair nothing
	// of q's is attached to, where it would decide nothing.
	//
	// WHAT A CASE ENDS A WINDOW BEHIND ([Capabilities.LingerClock]). A gate
	// the verb shut means nothing to the window until the consumer has read
	// it: a consumer that reads its gate between fetches, as the backend
	// that ships does, would otherwise see the window end on the clock first
	// and the gate after, and the case would no longer ask the question it is
	// named for — whether the gate alone holds the window's mail. On that
	// backend, the pair's consume loop has come round to its gate since the
	// call and found it shut; on the twin, which reads its gates under one
	// lock at every dispatch and every window's flush, it has once the verb
	// has returned and no handler of q's is running.
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
	// fence it. One process's broadcasts reach a listener in the order sent,
	// so a later one is a fence for the listener that hears it; but a
	// WITHDRAWN listener hears the fence no more than the broadcast, and its
	// silence after one is the same on a withdrawal that left it standing,
	// whose handler had simply not run yet. A barrier through every
	// listener q still holds is what such a registration would have carried
	// the broadcast past. On the backend that ships it is the NATS client's
	// own Barrier behind a flush; on the twin, a marker through each inbox.
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
	// EnsureSubscription, which the contract deliberately says nothing
	// about.
	//
	// It exists because "ensure" has two halves and only one of them is
	// observable through the contract. The contract's half is that
	// re-ensuring keeps the mail. The other half is that re-ensuring must
	// not ASK: on the broker this engine ships a consumer create is a
	// proposal through the metadata Raft group whether or not it turns
	// out to be a no-op, so a node ensuring a company's mailboxes at
	// every boot and every config apply replicated one write per seat to
	// be told what it had just read. Nothing in the contract can see that,
	// and a backend that quietly went back to proposing would pass every
	// other case here.
	//
	// SCOPED TO THE CLIENT UNDER TEST, never to an inspection client: the
	// number describes who did the asking, and a capability that read it
	// off an admin connection would report zero for ever.
	//
	// Deliberately NOT part of [subscriptionState]: that struct is the
	// state of one (topic, group) pair and this is a client-wide counter,
	// so an assertUntouched over it would be comparing a number to itself
	// on a case that never touched a consumer. The requirement written
	// there — that a new observable of a SUBSCRIPTION is added to it —
	// still stands and this is not one.
	ConsumerProposals func(q queue.EventQueue) int

	// BrokerDown takes away the broker q is a client of, for the rest of the
	// case, as a broker whose server stopped is taken away: q is NOT
	// stopped, and as far as it knows it is still live. It returns once q
	// has noticed, so the next verb sent meets the outage rather than racing
	// it.
	//
	// THE ONLY FAULT INJECTION IN THE SUITE, and it exists for
	// runReachability: whether a verb that could not reach its broker says
	// so with queue.ErrUnavailable — a condition every caller retries —
	// rather than with an unmarked error a caller above the queue reads as
	// this node's own fault, or with queue.ErrNotLive, which a seat release
	// reads as proof the mailbox is torn down. One-way, because the
	// shipped backend's harness can only stop a server; see the package
	// doc.
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
	// A capability rather than a requirement because both answers are
	// legitimate — on JetStream, Open establishes the connection and the
	// streams, so Start is a no-op — but the CONSISTENCY is not optional.
	// Whichever way this flag points,
	// an_unstarted_queue_answers_the_same_way_for_every_verb sends all
	// twenty-one and refuses a backend that answers some one way and some the
	// other. After Stop there is no choice — see
	// a_stopped_queue_refuses_every_verb.
	//
	// It was RequiresStart, one verb, and the narrowness was
	// the bug: the twin refused Publish and Subscribe while
	// EnsureSubscription created durable state and PauseTopic took a hold
	// that outlived the stop.
	RequiresStart bool

	// Restartable declares that Start on a stopped queue re-establishes it
	// and delivery resumes.
	//
	// A capability rather than a requirement because the contract does not
	// say. Start "connects the backend and begins consuming" and Stop
	// "closes the connection", which reads as restartable — but delivery
	// pause is documented one-way ("once paused, the engine is shutting
	// down"), so a backend may reasonably treat Stop as terminal and
	// require a fresh queue. Two backends already answer differently: the
	// in-memory twin restarts, because there Stop is a client disconnect
	// and the broker, its mail and the cleared holds outlive it, while
	// JetStream cannot, because Stop closes the NATS connection and Start
	// does not re-establish it.
	//
	// UNSETTLED, and deliberately left that way here rather than decided by
	// a test: nothing in the engine restarts a queue in-process today, so
	// the ambiguity costs nothing yet, but it is exactly the lifecycle point
	// a live config-reload path would trip over. Until the contract picks a
	// side the suite must not render a verdict on it. Either answer lands
	// somewhere concrete — "restartable" makes JetStream's behaviour a bug
	// and this flag a plain requirement, "terminal" keeps the flag and puts
	// the words in Stop's own doc in queue.go.
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
	// BOUNDED AS LIVENESS by settleFor: a healthy backend settles once its
	// dispatch has come round, which the harnesses keep well inside it, and
	// one that never does — a mailbox it never empties, a loop it never ends
	// — is what fails here. The bound decides nothing about what was
	// delivered: the read behind it does.
	settling, cancel := context.WithTimeout(ctx, settleFor)
	defer cancel()
	for _, q := range queues {
		if err := settle(settling, t, q); err != nil {
			return fmt.Errorf("a %s queue did not settle: %w", q.Backend(), err)
		}
	}
	for _, q := range queues {
		running, err := q.WaitForHandlers(settling, 0)
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
// BOUNDED AS LIVENESS by settleFor: a consumer takes a verb in once its loop
// has come round, which the harnesses keep well inside it, and one that never
// does — a loop that ended rather than park, a gate it never reads — is what
// fails here. The bound decides nothing about what was delivered.
func (s *suite) awaitParked(ctx context.Context, t *testing.T, q queue.EventQueue, topic, group string) {
	t.Helper()
	parking, cancel := context.WithTimeout(ctx, settleFor)
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

	// quietFor is the timeout WaitForHandlers is handed where the case
	// needs one that is not zero: on an idle queue, which returns without
	// spending it, and beside a handler the case holds, which outlasts it.
	//
	// IT WAS HOW LONG A NEGATIVE ASSERTION — "this must NOT be delivered" —
	// HELD A QUIET WINDOW OPEN, and no case does that any more. Its reason
	// was wrong before its number was: it said a leak "tends to leak
	// immediately, so this only has to outlast one dispatch cycle" — written
	// against a backend whose dispatch cycle is microseconds, and false for
	// one where a cycle is a network round trip. On a backend slower than
	// the window, a negative assertion passed because nothing had ARRIVED
	// yet, not because nothing was leaked: a false PASS, which is worse than
	// a flake, since it quietly weakens coverage on exactly the backends
	// most likely to leak. MEASURED: a redelivery after an ack it dropped,
	// or a detached loop's dispatch a runner scheduled 300 ms late, passed
	// the window with the bug present, so staysAt reads behind the backend's
	// Settle instead; and the linger cases whose tail still held one open
	// end their window on a clock they move (Capabilities.LingerClock). Do
	// not bring it back as a "wait for something" budget. Positive halves
	// wait on settleFor and on a signal; negative ones on Settle.
	quietFor = 150 * time.Millisecond

	// keepFor ends a case that waits on the keep of a dead letter at the
	// transport's ceiling, and only so that a hang ends: such a case is
	// decided by the keep's own completion, which a drain reports
	// (WaitForHandlers), never by this bound. Generous on purpose: a keep of
	// megabytes under the race detector on a loaded runner outlasted
	// settleFor, which is sized for a delivery of a few bytes.
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

	// lingerFor is the batch linger window the batching subtests use. It
	// has to be long enough that a drain collects, inside one window, the
	// mail already waiting for it, and short enough that a test waiting out
	// several windows stays quick.
	//
	// NO CASE PUBLISHES INTO A LIVE WINDOW ANY MORE. The cases whose subject
	// is an arrival joining an open window — coalescing it, held to options
	// reloaded meanwhile — end their window on a clock they move
	// (clockedWindow), as the pause, hold and stop cases do. All seven
	// cases on this one hold their mail before the drain starts. Six queue
	// a backlog before they attach (queueBacklog):
	// linger_partitions_by_key_preserving_arrival_order,
	// failing_partition_redelivers_only_itself,
	// batch_key_failure_falls_back_to_unique_key,
	// an_aged_conversation_dispatches_before_a_fresher_one,
	// within_a_partition_events_are_ordered_by_timestamp and
	// max_batch_chunks_oversized_buffers, which once published FIVE events
	// in a bare loop into one window, ten milliseconds a publish where a
	// publish is a network round trip. The seventh,
	// an_undispatched_partition_pays_for_its_hand_back, fills a held
	// subscription (holdForOneBatch). So this window bounds only how long a
	// drain has to collect what the broker already holds — fetches, not
	// publishes — and a case whose subject is what one drain does with
	// several events has them all waiting for that drain.
	//
	// The value stands on a NARROWER base than "no backend has reported a
	// flake" suggests, which is what this used to claim. Ten repeats of the
	// Batch group on JetStream came back clean, and the twin is the fastest
	// backend and so the least informative. A case needing more publishes
	// per window should say so rather than assume this covers it.
	//
	// What the base no longer has is a THIRD opinion. Every backend this
	// suite has ever certified found more bugs in the suite than in itself,
	// so the loss is real even though both remaining backends now run with
	// no external service and no environment variable to forget.
	//
	// The suite never asks for a linger above queue.MaxLingerSeconds, and
	// must not: backends size their dispatch budget to that ceiling and are
	// right to refuse a window they cannot honour rather than silently
	// clamping it. A case needing a longer window to make something
	// observable should find another way to observe it.
	lingerFor = 50 * time.Millisecond

	// handBackLinger is the window a case opens when the drain it waits for
	// has to carry a HANDED-BACK message beside fresh ones — the two cases
	// that build a partition out of a redelivery and never-delivered mail
	// (a_redelivered_event_rejoins_its_conversation_in_timestamp_order and
	// a_partition_at_mixed_counts_tells_each_message_its_own_headroom).
	//
	// WHAT IT BOUNDS IS A BROKER TAKING A RETURN IT HAS ALREADY BEEN SENT,
	// and nothing else. Both cases hand their message back by DEFERRAL,
	// which every backend returns at once, and let the attachment fetch
	// again only once the deferral has been applied, with the fresh mail
	// already in the mailbox. On the in-memory twin the return has landed
	// by then, synchronously. On JetStream it is a message nothing
	// acknowledges, taken by a broker goroutine separate from the one that
	// serves fetches — so the fetch the window opens on can be served
	// first, and the return then has to land inside the window. That is
	// one internal hop, cheaper than a publish round trip (a handful of
	// those measured 200-330ms at worst under a parallel -race suite), and
	// this sits above that measurement rather than at it.
	//
	// IT USED TO BOUND A REDELIVERY SPACING, as mixedCountLinger, and that
	// was the defect rather than the margin. Both cases handed their
	// message back as a FAILURE, which returns on the backend's backoff
	// (25ms doubling to 50ms in the JetStream harness, a second doubling
	// to thirty in production), so "the two meet in one drain" was a race
	// between a broker timer and a constant — one the shipped spacing
	// never wins, and one the contract never promised anybody would: see
	// queue.OrderForDispatch.
	//
	// Nowhere near queue.MaxLingerSeconds, so no backend has to refuse it.
	handBackLinger = 400 * time.Millisecond

	// clockedWindow is the linger a case gets whose window ends on a clock
	// it moves ([Capabilities.LingerClock]): the contract's ceiling,
	// queue.MaxLingerSeconds, which on that clock costs nothing.
	//
	// THE CEILING, SO A CLOCK THE BACKEND IGNORED SHOWS. A backend that kept
	// the window on the wall clock despite the clock it was handed would end
	// a short window by itself a moment later, and a case could pass on that
	// as though it had moved the clock; a window of a minute outlasts every
	// bound the suite waits, so a case whose window only the clock can end
	// fails instead.
	//
	// IT REPLACED racingWindow, a window of a second on the wall clock that
	// every case whose SETUP had to land inside a window paid: the pause,
	// the hold, the stop and the two coalescing cases. That was sized for the
	// slowest backend plausible rather than the measurement to hand — the
	// twin's publish-to-pause gap was 1.0ms at worst under 10x CPU load,
	// which made a 50ms window look like 50x headroom, while a handful of
	// publish round trips under a parallel -race suite measured 200-330ms —
	// and it still lost to a loaded runner, since no size of window orders a
	// verb before its end. A constant sized against the quickest backend is
	// how a suite acquires a race it cannot see; a clock the case moves is
	// how it gives one up.
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
	deadline := time.NewTimer(settleFor) //fixedwait:liveness
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

// awaitLabelsInAnyOrder waits for exactly these labels, in whatever sequence.
//
// For the cases whose subject is WHAT was delivered rather than in what
// order — chiefly a redelivery that races a fresh publish, where the order
// rests on a backoff window the shipped broker measures in wall-clock time.
// The order the contract DOES state (a failure behind the mail waiting when it
// failed, a hand-back at the head) is certified where it belongs, by
// a_failure_returns_behind_waiting_mail_and_a_deferral_at_the_head, which
// holds the mail before it is published so nothing races. A case that
// asserted a sequence the contract does not state would pass on one backend,
// pass on the other whenever the timing happened to favour it, and fail
// under load: a flake that reads as a broker bug.
//
// The expectation is still passed down, so a timeout can report a delivery
// that CONTRADICTS rather than merely lags — a missing event and a surplus
// one are both still failures here.
func (j *journal) awaitLabelsInAnyOrder(t *testing.T, what string, want ...string) {
	t.Helper()
	j.awaitExpecting(t, what, func(seen []string) bool {
		return sameLabels(seen, want)
	}, want, true)
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
// THE NEGATIVE HALF OF THE CONTRACT — "a paused attachment delivers nothing" —
// has no completion of its own to wait on, so it is read behind the backend's
// report that nothing more is coming. It used to be read after a quiet window,
// which a leak that came late passed: a detached loop's dispatch a runner
// scheduled late, or the redelivery of an ack that was never sent, which waits
// out the whole ack window. Settled, a backend has made every call it owed, so
// the leak is in the journal however late it came.
//
// What is still not claimed is that no duplicate ever arrives: the engine
// promises bounded duplication, not exactly-once. What settling adds is that a
// healthy backend owes nothing — an ack the broker has not taken leaves the
// message in its mailbox, which Settle does not read as settled — so an ack
// that did not stop a retry fails here now, rather than in a window too short
// to see the retry.
//
// A stream subscriber's journal is refused, because a feed is no consume loop
// and nothing settles it: read one behind a fence ([journal.fenced]).
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
		func(seen []string) bool { return slices.Contains(seen, fence) }, all, true)
	if got := j.all(); !equalStrings(got, all) {
		t.Fatalf("%s: the subscriber received %v by the fence, want %v", what, got, all)
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
	// verdictUnmet — a wait that records no deliveries (on backend state,
	// on a signal) and whose condition never held, so it concludes nothing
	// about them.
	verdictUnmet
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
	case verdictUnmet:
		t.Fatalf("timed out after %s waiting for %s; the condition never held\n"+
			"\tThis wait records no deliveries, so it has none to report and cannot tell "+
			"a backend slower than this budget from a wrong one: treat timing and "+
			"behaviour as both live. %s", settleFor, what, budgetNote)
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
	deadline := time.NewTimer(settleFor) //fixedwait:liveness
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
	case <-time.After(settleFor): //fixedwait:liveness
		if rescue != nil {
			rescue()
		}
		// The case closes some of these channels itself, so a signal that
		// never came says nothing about what was delivered.
		timedOut(t, what, verdictUnmet, nil)
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
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			// State the suite polls has no "saw something" half — the
			// condition is simply still false — so this always reports the
			// ambiguous case, which it genuinely is. Not verdictNothing,
			// whose "NOTHING was delivered" this poll cannot know: most
			// cases wait here on state their handlers were already handed
			// events to move, and that sentence sent the reader to the
			// case's subject and setup.
			timedOut(t, what, verdictUnmet, nil)
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
// before it fails the case: a withdrawal that ignores its bound waits on that
// handler for ever, and only it meets the bound.
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
func tryPublish(ctx context.Context, q queue.EventQueue, topic string, ev *events.Event) {
	_ = q.Publish(ctx, topic, ev)
}

func publish(ctx context.Context, t *testing.T, q queue.EventQueue, topic string, ev *events.Event) {
	t.Helper()
	if err := q.Publish(ctx, topic, ev); err != nil {
		t.Fatalf("Publish(%s, %s): %v", topic, ev.Type, err)
	}
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
		// BOUNDED AS LIVENESS by settleFor: the barrier waits only for
		// handlers to return, and no case holds one while it asks, so
		// only a barrier that never passes meets the bound — and fails.
		bounded, cancel := context.WithTimeout(ctx, settleFor)
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
