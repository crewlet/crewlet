package jetstream

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/queue"
)

// defaultFetchWait bounds one Fetch call. Short enough that quiesce, pause
// and shutdown take effect promptly; long enough that an idle seat is not
// spinning. Pull consumers make this a poll interval rather than a hostage
// window — nothing is pushed into a client-side queue, so an idle fetch
// holds no mail.
const defaultFetchWait = time.Second

// drainWait is the fetch window used when collecting the tail of a batch that
// is already locally available.
//
// LOAD-BEARING at the shipped default. With `notification_coalesce_window_seconds`
// at 0 the linger deadline has already passed by the first tail fetch, so this
// single window IS the whole busy-case coalescing mechanism: everything that
// piled up while the previous handler ran has to come back inside it, or it
// waits for the next drain and the seat runs a second turn.
//
// 50 ms because that is what the fetch is for — collecting what the server has
// ALREADY got, not waiting for arrivals — and it is two orders of magnitude
// above a round trip to a broker in the same process or the same datacentre,
// which is where this one is (embedded by default, an in-cluster NATS
// otherwise). It is not a latency budget for a distant broker: a deployment
// whose broker is far enough away for 50 ms to truncate a backlog should raise
// the linger window, which is the knob for exactly that and is measured
// against the ack budget rather than guessed at here.
//
// Never longer than the idle poll: an operator who shortened FetchWait wanted
// a more responsive consumer, and a tail fetch that outlasted the poll would
// take that back on every drain.
const drainWait = 50 * time.Millisecond

// drainFetchWait is the tail window this queue actually uses.
func (q *Queue) drainFetchWait() time.Duration {
	return min(drainWait, q.fetchWait())
}

// defaultNakDelay spaces out the FIRST redelivery of a failing message. Not
// zero: an immediately-redelivered failure spins the loop at full speed
// against whatever is broken. A deferral takes a different path and is
// immediate.
const defaultNakDelay = time.Second

// defaultNakCeiling caps the doubling that follows.
//
// FLAT SPACING SPENT THE WHOLE BUDGET IN HALF A MINUTE. A message gets
// maxDeliver attempts, and one second apart puts the last of them 25 seconds
// after the first failure — so an LLM entry sitting out, a vendor's rate-limit
// window or a database coming back up all dead-letter a message that would
// have been handled on the next attempt. It is also the shape that hammers
// whatever is already struggling: 25 requests a second apart is what a
// provider that just answered "slow down" receives.
//
// Doubling from a second to this spends the budget over about ten minutes,
// which covers the minute an LLM entry sits out after its harness reports a spent
// plan or a rate limit (internal/providers/llm/harness, bench.go)
// several times over while still retrying a momentary blip within a second or
// two. A failure that outlasts
// that window is not transient, and holding a seat's mailbox behind it is
// worse than the dead-letter copy.
//
// The in-memory twin redelivers immediately and deliberately: it models
// ordering and the delivery budget, not the clock, and a suite that waited
// out real backoff would spend its life in timers. What that leaves out is
// what the clock does to ORDER here: while a failure waits out its backoff
// this consumer serves never-delivered mail, newer events of the failed
// message's own conversation included, and they are handled first, in calls
// of their own. The contract allows both answers and orders neither across
// calls — see queue.OrderForDispatch.
const defaultNakCeiling = 30 * time.Second

func (q *Queue) fetchWait() time.Duration {
	if q.cfg.FetchWait > 0 {
		return q.cfg.FetchWait
	}
	return defaultFetchWait
}

func (q *Queue) nakDelay() time.Duration {
	if q.cfg.NakDelay > 0 {
		return q.cfg.NakDelay
	}
	return defaultNakDelay
}

func (q *Queue) nakCeiling() time.Duration {
	if q.cfg.NakCeiling > 0 {
		return q.cfg.NakCeiling
	}
	return defaultNakCeiling
}

// nakJitter spreads each redelivery across a fifth of its wait, as the engine's
// other retries of a failing dependency do, so seats that failed together do
// not all come back on the same second.
const nakJitter = 0.2

// nakBackoff is how long the nth spaced return of a message waits: its
// [Queue.nakSpacing], spread by [nakJitter].
func (q *Queue) nakBackoff(n uint64) time.Duration {
	return backoff.Jitter(q.nakSpacing(n), nakJitter)
}

// nakSpacing is the wait the nth spaced return is spread around.
//
// The first waits [Queue.nakDelay] and each one after that waits twice as
// long, up to [Queue.nakCeiling]. n counts from 1, and what it counts is the
// caller's: for a FAILURE, how many times this message has failed HERE — see
// [attachment.failures] for why that is not JetStream's delivery count — and
// for a PASS, that delivery count itself — see [attachment.pass] for why it
// is not the failures.
//
// The arithmetic is [backoff.Doubling]'s, which is what keeps a count off the
// wire from overflowing into a negative wait. The count is clamped to an int
// before it gets there, because a uint64 past the int range would convert to
// a negative attempt and read as the first return rather than the latest.
func (q *Queue) nakSpacing(n uint64) time.Duration {
	return backoff.Doubling(int(min(n, math.MaxInt32)), q.nakDelay(), q.nakCeiling())
}

// attachment is ONE consumer this process runs for a (topic, group) pair.
//
// A process may run several for one pair — the contract's "close this
// process's consumer(s)" is plural on purpose. A seat inbox has exactly one,
// because there membership IS ownership, but a fleet-wide worker group may
// legitimately run more than one member on a node.
type attachment struct {
	q    *Queue
	key  attachKey
	cons jetstream.Consumer

	// lane is the topic's priority lane, nil on a topic that carries none
	// ([queue.HasLane]). Fetched before cons on every cycle, and part of
	// this one attachment rather than one of its own, so every hold,
	// quiesce, pause and detach of the topic stops both.
	lane jetstream.Consumer

	// running is the mail a handler of this attachment is running on right
	// now, per mailbox: what [Queue.Waiting] takes off the consumers'
	// unfinished counts, because a message a turn is working on is not
	// waiting for one.
	running struct{ priority, background atomic.Int64 }

	log    *slog.Logger
	cancel context.CancelFunc
	done   chan struct{}

	// fetching bounds every fetch this attachment makes, and abandon ends
	// whichever one is outstanding. STOP ALONE CALLS IT, never Detach — see
	// [attachment.stop] for why the two differ.
	fetching context.Context
	abandon  context.CancelFunc

	// quiesced stops this consumer taking NEW work while staying
	// attached. A running handler finishes; nothing new is fetched. Its
	// inverse is required, not optional: a node whose coordination store
	// blipped quiesces and must be able to come back, or it holds the
	// seat, stays attached, and consumes nothing for the rest of its life
	// — owned, attached and silently deaf.
	quiesced atomic.Bool

	// paused is the process-wide shutdown delivery pause.
	paused atomic.Bool

	// lingering is set once a fetch of this attachment's linger window
	// returns with the window's deadline still ahead, and cleared when the
	// drain returns ([attachment.drain]); a window of nothing never sets it.
	// Reported by [Queue.Lingering].
	lingering atomic.Bool

	// windowed is how many events the drain has collected for the batch it
	// is building — what its linger window hands over when it ends — set as
	// each one is collected and cleared when the drain returns
	// ([attachment.drain]). Reported by [Queue.InWindow].
	windowed atomic.Int64

	// detached is set the moment Detach or Stop is called. It is what
	// makes the contract's two halves both true at once: Detach returns
	// WITHOUT joining a running handler, and yet no NEW handler starts
	// after it returns. The consume loop may still be parked in a fetch
	// when Detach lands, so cancelling the context alone would let one
	// more message through — the delivery a successor is already entitled
	// to.
	detached atomic.Bool

	// id names this attachment in [Queue.Loops], unique among this
	// client's attachments for its whole life. Set before the loop starts.
	id uint64

	// top is where the consume loop last stood at the top of its cycle:
	// how many tops it has reached, shifted left one, with the low bit set
	// when it was blocked there. ONE WORD, written by the loop alone, so a
	// reader never pairs one top's count with another top's gate. Reported
	// by [Queue.Loops].
	top atomic.Uint64

	// ended is set once the consume loop has returned, under the queue's
	// lock, so that Detach or Stop never keeps a loop that can no longer
	// forget itself. See [Queue.loopEnded].
	ended bool

	// failures counts how many times each message has FAILED here, keyed
	// on its stream sequence.
	//
	// # Why the delivery count cannot answer this
	//
	// The backoff doubles per failure, and it was doubling per DELIVERY —
	// which this package documents as counting something else entirely.
	// [budgetFor]'s own comment says the budget "covers poison, node-death
	// AND HANDOFF — the last because a deferred delivery returns via Nak
	// and that increments the count (measured)". Every healthy return goes
	// through the same counter: a deferral when a lease moves, a hand-back
	// when a hold or a pause lands between the fetch and the dispatch. So
	// a message handed back five times for reasons that were nobody's
	// fault met its FIRST genuine failure already five steps up the
	// doubling curve — at the shipped values, straight at the 30 s
	// ceiling. THE FIRST FAILURE IS STILL FAST is the other half of the
	// backoff decision, and it was not true.
	//
	// # Why in memory, and what a reset costs
	//
	// There is nowhere else to put it: a Nak cannot carry a header, and
	// the count is worth exactly one attachment's lifetime. A redelivery
	// that lands on another node — or here after a re-attach — starts at
	// one again, which is the SAFE direction: the cost is a fast retry,
	// where the alternative was a slow first one. The budget is unchanged
	// and still counts deliveries, because dead-lettering is about how
	// many times a message has been handed to anybody at all.
	failures   map[uint64]uint64
	failuresMu sync.Mutex
}

// failed records one failure of a message and reports the count.
func (a *attachment) failed(seq uint64) uint64 {
	a.failuresMu.Lock()
	defer a.failuresMu.Unlock()
	if a.failures == nil {
		a.failures = map[uint64]uint64{}
	}
	a.failures[seq]++
	return a.failures[seq]
}

// settled forgets a message that will not come back — acked, terminated, or
// handed on. Without it the map would hold every sequence this attachment
// ever failed for the life of the seat.
func (a *attachment) settled(seq uint64) {
	a.failuresMu.Lock()
	defer a.failuresMu.Unlock()
	delete(a.failures, seq)
}

func (a *attachment) setQuiesced(v bool) { a.quiesced.Store(v) }
func (a *attachment) setPaused(v bool)   { a.paused.Store(v) }

// blocked reports whether this consumer may take new work.
func (a *attachment) blocked() bool {
	return a.detached.Load() || a.quiesced.Load() || a.paused.Load() || a.q.held(a.key)
}

// blockedBy names what blocks this consumer ([attachment.blocked]): the reason
// its own hand-back of a delivery it would not hand over is logged with, should
// that hand-back spend the delivery's budget.
func (a *attachment) blockedBy() string {
	switch {
	case a.detached.Load():
		return "detached"
	case a.quiesced.Load():
		return "quiesced"
	case a.paused.Load():
		return "paused"
	case a.q.held(a.key):
		return "held"
	}
	return "blocked"
}

// handBackStopping is the reason a delivery is handed back with when the
// attachment stops with it in hand: teardown, its context ended.
const handBackStopping = "stopping"

// reachedTop records that the consume loop stands at the top of a cycle, with
// its gate shut when blocked: everything it fetched before is dispatched or
// handed back, and a blocked loop fetches nothing until a verb opens its gate.
// Only the loop writes it, so a load and a store cannot lose a top.
func (a *attachment) reachedTop(blocked bool) {
	next := (a.top.Load()>>1 + 1) << 1
	if blocked {
		next |= 1
	}
	a.top.Store(next)
}

// close stops this consumer taking new work and returns immediately.
//
// It deliberately does NOT join a running handler: Detach is the
// fenced-release path, and a node that has lost a seat must give it up now
// rather than when the turn it should not be running finally ends.
//
// AND IT LEAVES AN OUTSTANDING FETCH TO END ON ITS OWN, which is the
// difference between this and [attachment.stop]. The pull request that fetch
// sent is still open at the broker, and a message published in the moment
// after the detach can be delivered to it. Left open, that message reaches
// [attachment.dispatchOne], finds this consumer detached, and is handed back
// at once for the successor. Abandoned, the client drops its subscription
// while the broker may already be sending, and a message in flight then is
// lost to nobody's hand-back: it waits out the whole ack window before the
// broker offers it again — on exactly the lease movement a hand-back exists
// to keep cheap.
func (a *attachment) close() {
	a.detached.Store(true)
	if a.cancel != nil {
		a.cancel()
	}
}

// stop is close, and the end of the outstanding fetch as well — the half only
// [Queue.Stop] may take.
//
// THE CONNECTION IS CLOSED NEXT whatever happens here, and closing it ends the
// broker's interest in every pull request on it: a message in flight to one
// is lost to the same ack window either way, so ending the fetch first loses
// nothing a stop was not already losing. What it removes is the wait. A loop
// parked in a fetch noticed its cancellation only when the fetch ran out — a
// second at the shipped poll — so every stop of a queue with an idle
// attachment spent the whole [stopGrace] waiting for loops it then abandoned
// anyway.
func (a *attachment) stop() {
	a.close()
	if a.abandon != nil {
		a.abandon()
	}
}

// fetch asks the broker for up to n messages, waiting at most wait for them,
// and hands each to take as it arrives — the one way every consume loop here
// fetches, so a stop reaches every outstanding fetch through
// [attachment.fetching].
//
// # Why two context options and no expiry option
//
// The client's own expiry option (FetchMaxWait) is a timer and nothing else:
// a fetch sent with it ran to the end of its window whatever became of the
// loop that sent it, so a stop waited for it. Its context option is the
// cancellable form, but it couples two durations the expiry option kept
// apart. A context's deadline becomes the request's expiry with a tenth held
// back (at most a second), and the same deadline then ENDS THE CLIENT'S WAIT —
// so a message the broker hands over just before its expiry has a tenth of
// the window to arrive before the client stops listening, five milliseconds
// on a tail fetch, and one that arrives after is dropped unacknowledged and
// sits out the whole ack window before the broker offers it again. The expiry
// option's client waits a full second past the expiry, which is the margin
// worth keeping.
//
// So the request is sent with TWO: the first carries only a deadline, which
// the client turns into the expiry and nothing else, and the second replaces
// it as the context the client waits under — [attachment.fetching], which has
// no deadline, so the wait ends on the broker's own timeout, a second's grace
// after it, or a stop. The first deadline is set past wait by exactly the share
// the client holds back, so the expiry the broker is given is wait itself;
// [TestAFetchExpiresAfterItsWaitAndAStopEndsItAtOnce] pins both halves against
// the vendored client.
func (a *attachment) fetch(n int, wait time.Duration, take func(jetstream.Msg)) error {
	if err := a.fetching.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(wait + expiryHeldBack(wait))
	expiry, cancel := context.WithDeadline(context.Background(), deadline)
	batch, err := a.cons.Fetch(n, jetstream.FetchContext(expiry), jetstream.FetchContext(a.fetching))
	// The options are applied before Fetch returns, and the first one's
	// context is read for its deadline alone.
	cancel()
	if err != nil {
		// A WINDOW THAT LAPSED BEFORE THE REQUEST WAS SENT IS AN EMPTY
		// FETCH, not a failure. The client reads that deadline when it
		// applies the option and refuses one already past, so a goroutine
		// descheduled between taking it and sending the request for longer
		// than the window — fifty-five milliseconds on a tail fetch, on a
		// loaded runner — was told "invalid option", logged fetch_failed
		// and slept a whole poll. Nothing was sent, so nothing was missed,
		// and the window it asked for has gone by all the same: the loop
		// asks again, as after any empty fetch. Judged by the deadline
		// itself rather than by the context's Err, which its timer sets a
		// moment after the instant the client compared against.
		if errors.Is(err, jetstream.ErrInvalidOption) && !time.Now().Before(deadline) {
			return nil
		}
		return err
	}
	for msg := range batch.Messages() {
		take(msg)
	}
	// A REFUSED REQUEST IS A FAILED FETCH, not an empty one. The broker can
	// answer a pull at once with a status: a 503 (no responders), a 409 (a
	// deleted consumer, a leadership change, a server shutdown) or a 400.
	// The client then closes the batch with nothing in it and keeps the
	// status for Error, reading only the broker's own "nothing for you"
	// (404, 408) as no error. Dropped here, the refusal read as an empty
	// fetch, and both consume loops asked again at once for as long as it
	// lasted, with no fetch_failed and no poll of rest. Returned, it is
	// logged and waited out like any failed fetch, and a stop still reads
	// as the cancellation logFetchErr passes over. Read after the drain:
	// the client sets the error before it closes the channel.
	return batch.Error()
}

// expiryHeldBack is how far past the wanted expiry a fetch's deadline is set,
// so that what the client keeps back of it — a tenth, at most a second
// (jetstream.FetchContext) — leaves exactly the wanted expiry.
func expiryHeldBack(wait time.Duration) time.Duration {
	return min(wait/9, time.Second)
}

// wait blocks until the consume loop has exited, or the deadline passes.
//
// Used by Stop, where the caller has already drained in-flight handlers and
// wants the goroutines actually gone. Bounded, because a handler that never
// returns must not turn shutdown into a hang — the process is leaving
// anyway.
func (a *attachment) wait(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-a.done:
	case <-t.C:
	}
}

// --- pause holds ----------------------------------------------------------
//
// Holds live on the QUEUE, keyed by the (topic, group) pair, not on an
// attachment. Two reasons, both learned the hard way.
//
// Keyed by the PAIR rather than by topic alone, because a topic-keyed hold
// also gated every other group on a shared subject, and because a second
// subsystem gating one inbox must not release the first one's hold.
//
// Owned by the QUEUE rather than by an attachment, because a hold is
// routinely taken BEFORE anything attaches: an engine that knows a seat is
// busy pauses first and attaches after. A hold stored on the attachment
// would simply be dropped in that window, and the seat would take exactly
// the work it was told not to.

func (q *Queue) held(key attachKey) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.holds[key]) > 0
}

// PauseTopic takes a named hold on one subscription.
func (q *Queue) PauseTopic(_ context.Context, topic, group, reason string) error {
	if reason == "" {
		reason = "default"
	}
	key := attachKey{topic, group}
	q.mu.Lock()
	defer q.mu.Unlock()
	// A hold on a closed client gates nothing: Stop dropped every
	// attachment and cleared the hold map on the way out. Returning nil
	// here told a shutdown path it had gated a subscription that no longer
	// existed. See queue.EventQueue's Stop.
	if q.closed {
		return ErrClosed
	}
	if q.holds[key] == nil {
		q.holds[key] = map[string]struct{}{}
	}
	q.holds[key][reason] = struct{}{}
	return nil
}

// ResumeTopic releases one reason's hold, flushing when none remain.
func (q *Queue) ResumeTopic(_ context.Context, topic, group, reason string) error {
	if reason == "" {
		reason = "default"
	}
	key := attachKey{topic, group}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrClosed
	}
	delete(q.holds[key], reason)
	if len(q.holds[key]) == 0 {
		delete(q.holds, key)
	}
	return nil
}

// PauseHolds reports the reasons currently holding a subscription paused.
func (q *Queue) PauseHolds(topic, group string) []string {
	key := attachKey{topic, group}
	q.mu.Lock()
	defer q.mu.Unlock()
	out := slices.Sorted(maps.Keys(q.holds[key]))
	return out
}

// Quiescing reports whether this client has stopped taking work on a
// subscription.
//
// Separate from a pause hold although both stop deliveries, because the two
// are cleared by different things — a hold by the subsystem that took it, a
// quiesce by detaching or attaching again — and because a stale quiesce is
// invisible from outside until someone attaches, which is what let one sit
// unnoticed long enough to strand a seat.
//
// Reported so the conformance suite can see it. A negative path asserts that
// an operation which declined to act touched NOTHING, and it compares every
// observable a backend exports; one this backend keeps to itself is one that
// case silently stops checking. Quiescing was exactly that here — the state
// existed and was writable, and the case written to catch a stale quiesce was
// checking it on the twin alone.
func (q *Queue) Quiescing(topic, group string) bool {
	for _, a := range q.lookup(topic, group) {
		if a.quiesced.Load() {
			return true
		}
	}
	return false
}

// Lingering reports whether this client holds a batch for a subscription in a
// linger window that has outlived a fetch, past what a window of nothing
// collects: what is published now joins that batch only because the window is
// open.
//
// Reported so the conformance suite can see it. A case asking whether a window
// coalesces publishes into one this reports open, and NOTHING ELSE THIS BACKEND
// EXPORTS SHOWS ONE: a message a drain holds is delivered and unacknowledged,
// as one a handler is running on is, and to the broker a window's fetch is a
// pull request like the tail fetch a window of nothing runs.
func (q *Queue) Lingering(topic, group string) bool {
	for _, a := range q.lookup(topic, group) {
		if a.lingering.Load() {
			return true
		}
	}
	return false
}

// InWindow reports how many events this client holds for a subscription in a
// batch drain — the batch its linger window hands over when it ends — and zero
// while no drain of it is collecting.
//
// Reported so the conformance suite can see it, beside [Queue.Lingering]. A
// case asking whether a window coalesces publishes its next event into the
// window and ends the window on a clock it moves, and it must not move that
// clock until the window holds the event: published is not yet fetched, and a
// window ended a moment early hands over the first event alone. NOTHING ELSE
// THIS BACKEND EXPORTS SAYS IT HOLDS IT: a message a drain has fetched is
// delivered and unacknowledged, as one a handler is running on is.
func (q *Queue) InWindow(topic, group string) int {
	n := 0
	for _, a := range q.lookup(topic, group) {
		n += int(a.windowed.Load())
	}
	return n
}

// Loop is one consume loop of this client, as [Queue.Loops] reports it.
type Loop struct {
	// ID names the loop's attachment, unique among this client's
	// attachments for its whole life.
	ID           uint64
	Topic, Group string
	// Cycles is how many times the loop has stood at the top of its cycle,
	// and Parked whether its gate was shut there the last time — held,
	// quiesced, paused or detached — so that it fetched nothing after it.
	Cycles uint64
	Parked bool
}

// Loops reports every consume loop of this client that has not returned: each
// attachment's, and each one Detach or Stop has let go of until its loop ends.
//
// Reported so the conformance suite can see it. A case asserting that nothing
// more was delivered reads its journal once this backend has nothing left that
// could deliver, and NOTHING ELSE THIS BACKEND EXPORTS SAYS THAT. A loop is the
// one goroutine that dispatches for its attachment, and it dispatches only
// between a fetch and its next loop top, so a loop that has ended, or has stood
// at a top with its gate shut since the case's last verb, has nothing left to
// hand a handler; the broker's own counts are snapshots, which cannot tell a
// loop parked at its gate from one between two pulls. A loop that has been let
// go of is still reported until it ends, because it can still be inside a
// handler, or holding a fetch that delivers.
func (q *Queue) Loops() []Loop {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Loop
	report := func(a *attachment) {
		top := a.top.Load()
		out = append(out, Loop{
			ID: a.id, Topic: a.key.topic, Group: a.key.group,
			Cycles: top >> 1, Parked: top&1 == 1,
		})
	}
	for _, atts := range q.attachments {
		for _, a := range atts {
			report(a)
		}
	}
	for a := range q.retired {
		report(a)
	}
	slices.SortFunc(out, func(x, y Loop) int { return cmp.Compare(x.ID, y.ID) })
	return out
}

// StreamSubscriptions reports how many of this client's live feeds
// ([Queue.SubscribeStream]) are still consuming: opened, and with their
// consumption not yet over.
//
// Reported so the conformance suite can see it. A case asserting that an
// unsubscribed feed receives nothing reads it once the feed has stopped, and
// THAT IS LATER THAN ITS UNSUBSCRIBE RETURNING: the client ends the
// consumption on a goroutine of its own, and a message already in the
// callback's path can still be handed over meanwhile. Read off the
// consumption's own Closed signal, taken when the feed opened, which the client
// closes once its callback has returned for the last time.
func (q *Queue) StreamSubscriptions() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.feeds = consumingFeeds(q.feeds)
	return len(q.feeds)
}

// consumingFeeds drops the feeds whose consumption has ended.
func consumingFeeds(feeds []<-chan struct{}) []<-chan struct{} {
	return slices.DeleteFunc(feeds, func(ended <-chan struct{}) bool {
		select {
		case <-ended:
			return true
		default:
			return false
		}
	})
}

// --- attaching ------------------------------------------------------------

// attach creates or reuses the durable consumer and starts a loop.
func (q *Queue) attach(ctx context.Context, topic, group string, loop func(context.Context, *attachment)) error {
	if topic == "" || group == "" {
		return fmt.Errorf("%w: topic and group are required", ErrSubject)
	}
	if _, err := q.EnsureSubscription(ctx, topic, group); err != nil {
		return err
	}
	stream, err := q.streamFor(ctx, topic)
	if err != nil {
		return err
	}
	// THE FIFTH SIBLING READ-BACK, and the one that kept the one-shot,
	// unbudgeted form every other provisioning read has been given up.
	//
	// It sat on the boot's context, which carries no deadline, so nats.go
	// applied its own five-second default — twenty-four times tighter than
	// [jsprovision.Budget] and tighter than [jsprovision.SlowAfter], so
	// the breadcrumb that would have named it could never fire. A
	// clustered boot failed here as `open consumer
	// crewlet.notifications.inbound/notify-inbound: context deadline
	// exceeded` with nothing logged before it.
	//
	// TWO VERBS, because there are two hazards and one tool does not cover
	// both. [jsprovision.ReadWith] re-issues a request the group never
	// answered, under [jsprovision.LookupBudget] as its ceiling;
	// [jsprovision.SettleWith]
	// waits out the propagation window, and this read is inside one BY
	// CONSTRUCTION — EnsureSubscription above has just created the
	// consumer, so the member that made it can still be told it is not
	// there. Silence and not-yet-visible are different answers with
	// different remedies, which is why neither verb subsumes the other.
	cons, err := q.openConsumer(ctx, stream, topic, group, consumerName(topic, group))
	if err != nil {
		return err
	}
	var lane jetstream.Consumer
	if queue.HasLane(topic) {
		// EnsureSubscription above made the lane with the topic's own
		// consumer, so a laned attachment that cannot open it is refused
		// the way one that cannot open the topic's is.
		if lane, err = q.openConsumer(ctx, stream, topic, group,
			consumerName(queue.LaneOf(topic), group)); err != nil {
			return err
		}
	}

	key := attachKey{topic, group}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return ErrClosed
	}
	// A process-wide delivery pause must apply to an attachment created
	// during a drain, or a late subscribe would start handlers the drain
	// is waiting to finish.
	paused := q.paused
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	fetching, abandon := context.WithCancel(context.WithoutCancel(ctx))
	q.lastLoop++
	a := &attachment{
		q: q, key: key, cons: cons, lane: lane, id: q.lastLoop,
		log:    q.log.With("topic", topic, "group", group),
		cancel: cancel, done: make(chan struct{}),
		fetching: fetching, abandon: abandon,
	}
	a.paused.Store(paused)
	q.attachments[key] = append(q.attachments[key], a)
	q.mu.Unlock()

	go a.run(loopCtx, loop)
	return nil
}

// run is the consume loop's goroutine: the loop, and then the release of what
// it held, however it ends.
func (a *attachment) run(ctx context.Context, loop func(context.Context, *attachment)) {
	defer close(a.done)
	// Released when the loop ends, however it ends: a detached attachment's
	// loop exits on its own once its fetch runs out, and nothing else would
	// release the fetch scope it no longer uses.
	defer a.abandon()
	// Forgotten by [Queue.Loops] before done closes, however it ends.
	defer a.q.loopEnded(a)
	loop(ctx, a)
}

// retireLocked keeps the attachments this client has just let go of in
// [Queue.Loops] until their loops return, each forgetting itself as it does
// ([Queue.loopEnded]). A loop that has ended already is not kept: nothing
// would forget it.
func (q *Queue) retireLocked(atts []*attachment) {
	for _, a := range atts {
		if !a.ended {
			q.retired[a] = struct{}{}
		}
	}
}

// loopEnded records that a's consume loop has returned, and forgets it.
func (q *Queue) loopEnded(a *attachment) {
	q.mu.Lock()
	defer q.mu.Unlock()
	a.ended = true
	delete(q.retired, a)
}

// openConsumer opens one durable consumer an attachment reads from, by its
// durable name: the topic's own, or its priority lane's. The topic and group
// name it in an error.
func (q *Queue) openConsumer(ctx context.Context, stream, topic, group, name string) (
	jetstream.Consumer, error) {
	lookupCtx, cancelLookup := context.WithTimeout(ctx, q.provisioning().Lookup)
	stopLookup := jsprovision.WhenSlow(lookupCtx, func(after time.Duration) {
		q.log.WarnContext(ctx, "jetstream_consumer_attach_slow", "stream", stream,
			"consumer", name, "waited", after,
			"detail", "still opening the mailbox this attachment consumes from; "+
				"on a fleet that is a metadata group that has not settled")
	})
	consumer := func(ctx context.Context) (jetstream.Consumer, error) {
		return q.js.Consumer(ctx, stream, name)
	}
	cons, err := jsprovision.ReadWith(lookupCtx, q.provisioning(), consumer, nil)
	stopLookup()
	cancelLookup()
	if jsprovision.NotYetVisible(err) {
		// ON ctx, NOT lookupCtx, for the reason [jsprovision.SettleWith]
		// gives: lookupCtx may be the deadline that just expired, and a
		// read-back handed an expired context asks once and gives up.
		cons, err = jsprovision.SettleWith(ctx, q.provisioning(), consumer)
	}
	if err != nil {
		return nil, fmt.Errorf("open consumer %s/%s: %w", topic, group, q.brokerFailed(ctx, err))
	}
	return cons, nil
}

// Subscribe attaches a competing-consumer handler.
func (q *Queue) Subscribe(ctx context.Context, topic, group string, h queue.Handler) error {
	return q.attach(ctx, topic, group, func(ctx context.Context, a *attachment) {
		for {
			if ctx.Err() != nil {
				return
			}
			// THE LOOP TOP that [Queue.Loops] reports: the gate is read
			// once, and what it said is what the loop then does.
			blocked := a.blocked()
			a.reachedTop(blocked)
			if blocked {
				// Nothing is fetched while blocked, so nothing is held
				// hostage — the pull model's quiet advantage over a
				// pushed prefetch.
				if sleep(ctx, a.q.fetchWait()) != nil {
					return
				}
				continue
			}
			// THE LANE FIRST, one message at a time like the rest: a
			// message it held is this round's delivery, and the topic is
			// read only when the lane had nothing.
			took := false
			if err := a.fetchLane(1, func(msg jetstream.Msg) {
				took = true
				a.dispatchOne(ctx, msg, h)
			}); err != nil {
				a.logLaneFetchErr(ctx, err)
			}
			if took {
				continue
			}
			//nolint:contextcheck // a fetch runs on the attachment's fetch scope, never the loop's: see [attachment.fetching]
			if err := a.fetch(1, a.q.fetchWait(), func(msg jetstream.Msg) {
				a.dispatchOne(ctx, msg, h)
			}); err != nil {
				a.logFetchErr(ctx, err)
			}
		}
	})
}

// fetchLane takes what the priority lane holds RIGHT NOW, up to n, and hands
// each to take; on a topic with no lane it does nothing.
//
// WITHOUT WAITING, because the lane is asked first on every cycle and is
// empty on almost every one: a wait here would be added to every delivery the
// topic makes. A priority event that lands a moment after this asked is taken
// on the next cycle, which on an idle consumer is at most one poll away
// ([Queue.fetchWait]).
//
// NOT ON THE FETCH SCOPE ([attachment.fetching]), which only bounds a waiting
// fetch: a no-wait request is answered at once, and at worst one second after
// a broker that never answers it, so a stop that lands during one is held no
// longer than that.
func (a *attachment) fetchLane(n int, take func(jetstream.Msg)) error {
	if a.lane == nil || n < 1 {
		return nil
	}
	if err := a.fetching.Err(); err != nil {
		return err
	}
	batch, err := a.lane.FetchNoWait(n)
	if err != nil {
		return err
	}
	for msg := range batch.Messages() {
		take(msg)
	}
	return batch.Error()
}

// logLaneFetchErr reports a priority lane that could not be read, and does
// not sleep: the topic is read next either way, and a lane that failed is
// asked again on the next cycle. A stop is not a failure.
func (a *attachment) logLaneFetchErr(ctx context.Context, err error) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	a.log.Warn("lane_fetch_failed", "error", err.Error(),
		"detail", "the priority lane could not be read on this cycle; the topic's own "+
			"mail is read as usual and the lane is asked again on the next one")
}

func (a *attachment) logFetchErr(ctx context.Context, err error) {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return
	}
	a.log.Warn("fetch_failed", "error", err.Error())
	_ = sleep(ctx, a.q.fetchWait())
}

// dispatchOne runs a handler for one message and applies its outcome.
func (a *attachment) dispatchOne(ctx context.Context, msg jetstream.Msg, h queue.Handler) {
	ev, ok := a.decode(msg)
	if !ok {
		return
	}
	// A hold taken while this message was in flight must still stop it.
	// The engine pauses a seat's inbox precisely to keep a fresh turn from
	// starting, and a message already fetched is exactly what would start
	// one. Returning it costs a delivery, as every return does: this
	// consumer picks it up again when the hold lifts, or a successor does.
	//
	// IN FLIGHT UNTIL IT IS SETTLED, though no handler runs: the hand-back
	// that spends the message's budget keeps its dead letter, a write that
	// crosses a link at its bytes' pace, and a drain that did not count it
	// stopped the queue under the keep ([queue.EventQueue.WaitForHandlers]).
	if a.blocked() {
		a.q.beginHandler()
		a.handBack(ctx, msg, ev, a.blockedBy())
		a.q.endHandler()
		return
	}
	a.q.beginHandler()
	held := delivery{msg: msg, ev: ev}
	done := a.runningOn(ev)
	res := runHandler(a.withProgress(a.withHeadroom(ctx, held), held), a.log, ev, h)
	a.apply(ctx, msg, ev, res)
	// IN FLIGHT UNTIL WHAT THE HANDLER RETURNED IS SETTLED, not only until
	// it returned: settling a return that spends the budget is a dead
	// letter's keep, a write that waits as long as its bytes take to cross
	// a link, and a drain that read the delivery as finished once the
	// handler returned stopped the queue under it, the copy lost
	// ([queue.EventQueue.WaitForHandlers]).
	a.q.endHandler()
	done()
}

// runningOn counts evs as mail a handler of this attachment is running on, per
// mailbox, until the returned func is called. The lane holds exactly the
// marked events of a laned topic ([queue.SubjectFor]), so the mark says which
// mailbox each came from.
func (a *attachment) runningOn(evs ...*events.Event) func() {
	var priority, background int64
	for _, ev := range evs {
		if a.lane != nil && queue.Prioritized(ev) {
			priority++
		} else {
			background++
		}
	}
	a.running.priority.Add(priority)
	a.running.background.Add(background)
	return func() {
		a.running.priority.Add(-priority)
		a.running.background.Add(-background)
	}
}

// withHeadroom states EACH message's remaining headroom on the handler's
// context — see [queue.DeliveriesLeftFor] for the number one message
// carries and [queue.DeliveriesLeft] for the partition's own, which the
// contract folds from this same list so no backend decides it.
//
// Off THIS message's own metadata rather than off [attachment.failures],
// which counts something else entirely: the map is one attachment's record of
// FAILURES and resets on every re-attach, while the budget a message is
// spending is carried by the message and survives every handoff. Confusing
// the two is the defect the value exists to end.
//
// A message whose metadata will not parse is LEFT OUT rather than guessed at,
// for the same reason [attachment.nakOrDeadLetter] still returns it: the
// count is unreadable, not zero.
func (a *attachment) withHeadroom(ctx context.Context, ds ...delivery) context.Context {
	perMessage := make([]queue.Headroom, 0, len(ds))
	for _, d := range ds {
		md, err := d.msg.Metadata()
		if err != nil || d.ev == nil {
			continue
		}
		perMessage = append(perMessage, queue.Headroom{
			ID:   d.ev.ID,
			Left: budgetFor(a.q.cfg) - int(md.NumDelivered),
		})
	}
	return queue.WithHeadroom(ctx, perMessage)
}

// withProgress states how the handler keeps the messages it holds from timing
// out ([queue.WithProgress]): an in-progress acknowledgement of each, which
// restarts its ack window, every third of that window, so two signals can be
// missed before the third is late.
func (a *attachment) withProgress(ctx context.Context, ds ...delivery) context.Context {
	return queue.WithProgress(ctx, a.q.ackWait()/3, func() error {
		for _, d := range ds {
			if err := d.msg.InProgress(); err != nil {
				return fmt.Errorf("jetstream: in-progress acknowledgement: %w", err)
			}
		}
		return nil
	})
}

// decode parses a message, acking-and-dropping a corrupt payload.
//
// A corrupt payload is never NAKed: redelivering it cannot make it parse, so
// a NAK just spends its delivery budget until it dead-letters, having
// blocked the subscription for every retry in between.
func (a *attachment) decode(msg jetstream.Msg) (*events.Event, bool) {
	var ev events.Event
	if err := json.Unmarshal(msg.Data(), &ev); err != nil {
		a.log.Error("event_decode_failed", "error", err.Error())
		_ = msg.Ack()
		return nil, false
	}
	return &ev, true
}

// runHandler invokes a handler, converting a panic into a Nak so an
// unexpected failure redelivers instead of killing the loop.
func runHandler(ctx context.Context, log *slog.Logger, ev *events.Event, h queue.Handler) (res queue.Result) {
	defer func() {
		if r := recover(); r != nil {
			contained := panics.Recovered(panics.ScopeQueue, r)
			// The Nak carries the panic VALUE to the redelivery machinery;
			// the stack goes to the log, because a handler that panics
			// every delivery is a bug to find, not a delivery to retry.
			log.Error("handler_panicked", "event_type", ev.Type,
				"panic", r, "stack", contained.Stack)
			res = queue.Nak(fmt.Errorf("handler panicked: %w", contained.Err()))
		}
	}()
	return h(ctx, ev)
}

func runBatchHandler(ctx context.Context, log *slog.Logger, evs []*events.Event, h queue.BatchHandler) (res queue.Result) {
	defer func() {
		if r := recover(); r != nil {
			contained := panics.Recovered(panics.ScopeQueue, r)
			log.Error("batch_handler_panicked", "batch", len(evs),
				"panic", r, "stack", contained.Stack)
			res = queue.Nak(fmt.Errorf("batch handler panicked: %w", contained.Err()))
		}
	}()
	return h(ctx, evs)
}

// apply turns a handler outcome into broker operations.
func (a *attachment) apply(ctx context.Context, msg jetstream.Msg, ev *events.Event, res queue.Result) {
	queue.LogResult(ctx, a.log, a.key.topic, a.key.group, ev, res)
	switch res.Outcome {
	case queue.OutcomeAck:
		// FORGOTTEN FIRST. A message that succeeded after failing twice
		// must not carry those two failures into a later redelivery of
		// the same sequence, and the map would otherwise hold every
		// sequence this seat ever failed for the life of the attachment.
		if md, err := msg.Metadata(); err == nil {
			a.settled(md.Sequence.Stream)
		}
		if err := msg.Ack(); err != nil {
			a.log.Warn("ack_failed", "error", err.Error())
		}

	case queue.OutcomeDefer:
		// Defer means: this process has lost the right to do this work.
		// Nak returns it immediately — measured at about a millisecond,
		// where letting the ack timer expire would park a seat's mail
		// for the whole AckWait window on every lease movement.
		//
		// It costs one delivery count, which is why the budget covers
		// handoffs — and why it goes through the same budget check the
		// failure path uses rather than a bare Nak. A bare one spent the
		// budget without owning its boundary, so a healthy event whose 25
		// deliveries went on lease movements was discarded by the broker's
		// MaxDeliver backstop with no dead-letter copy and no log line.
		// Never republish instead: a republished event is a NEW message,
		// and the completion ledger's idempotency plus the batch layer's
		// aging both key on the identity a Nak preserves.
		a.handBack(ctx, msg, ev, res.Reason)
		// Quiescing is the other half of a deferral: continuing to fetch
		// would hand this process more work it has equally lost the
		// right to do.
		a.setQuiesced(true)

	case queue.OutcomeNak:
		a.nakOrDeadLetter(ctx, msg, ev)

	case queue.OutcomePass:
		// Neither a failure nor a deferral, and the attachment goes on
		// taking mail: see [attachment.pass].
		a.pass(ctx, msg, ev, res.Reason)
	}
}

// pass returns a message this node will not handle and a peer may
// ([queue.OutcomePass]) — through the budget boundary every return crosses,
// so a message no node reads is dead-lettered with its copy rather than handed
// round for ever, and spaced as a failure is, by [Queue.nakBackoff].
//
// AS A PASS, should this return be the one that spends the budget: its copy is
// kept on its group's unhandled subject ([queue.RetiredTo]), apart from the
// group's poison, which is where a reader that can handle it finds it again,
// and its line names reason.
//
// BY ITS DELIVERY COUNT, NOT [attachment.failures]. A pass is no failure, so
// counting it as one would start this node's first genuine failure of the
// message up the curve, which is what that map exists to prevent; and the count
// that has to grow is the one every node shares. A group's older nodes passing
// a message from one to the next would each bring it back at their own count:
// four of them taking it in turn wait a second four times, then two seconds
// four times, and give it up in about four minutes rather than the ten its
// budget spans.
//
// NOT QUIESCED, which is the whole difference from a deferral: nothing is
// wrong with this node, only with this message here, so the attachment keeps
// taking the rest of its mail while the message waits out its spacing.
func (a *attachment) pass(ctx context.Context, msg jetstream.Msg, ev *events.Event, reason string) {
	// A MESSAGE WHOSE METADATA WILL NOT PARSE is passed at the shortest
	// spacing, for [attachment.nakOrDeadLetter]'s reason: returnMsg reads
	// the same metadata for the budget and treats an unreadable one as "not
	// yet spent".
	var delivered uint64 = 1
	if md, err := msg.Metadata(); err == nil {
		delivered = md.NumDelivered
	}
	a.returnMsg(ctx, msg, ev, a.q.nakBackoff(delivered), queue.OutcomePass, reason)
}

// nakOrDeadLetter redelivers a FAILED message until its budget is spent, then
// dead-letters it. Spaced by [Queue.nakBackoff], because an immediately
// redelivered failure spins the loop at full speed against whatever is broken
// and a flat spacing spends every attempt before it can heal.
//
// The decision lives here rather than relying on MaxDeliver alone because
// this is where the dead-letter subject is known and the message body is in
// hand. MaxDeliver stays configured as a backstop so a bug here cannot
// produce an infinite loop.
func (a *attachment) nakOrDeadLetter(ctx context.Context, msg jetstream.Msg, ev *events.Event) {
	// A MESSAGE WHOSE METADATA WILL NOT PARSE STILL GOES BACK, at the
	// shortest spacing: the alternative is dropping a delivery over a
	// header, and returnMsg reads the same metadata for the budget and
	// already treats an unreadable one as "not yet spent".
	var failures uint64 = 1
	if md, err := msg.Metadata(); err == nil {
		failures = a.failed(md.Sequence.Stream)
	}
	a.returnMsg(ctx, msg, ev, a.q.nakBackoff(failures), queue.OutcomeNak, "")
}

// handBack returns a HEALTHY message, ev — a deferral, a hold, a pause,
// teardown — for reason, which is logged should this return spend its budget.
//
// IMMEDIATE, and that is the whole reason a deferral NAKs rather than letting
// the ack timer expire: measured at about a millisecond, where the timer would
// park a seat's mail for the full ack window on every lease movement. It still
// goes through the budget boundary, because a Nak spends a delivery whatever
// it means — so a message whose deliveries went on handoffs is dead-lettered
// with a line rather than discarded by the broker's backstop in silence.
func (a *attachment) handBack(ctx context.Context, msg jetstream.Msg, ev *events.Event, reason string) {
	a.returnMsg(ctx, msg, ev, 0, queue.OutcomeDefer, reason)
}

// returnMsg is the one place a message goes back, and the one place the
// delivery budget is spent. delay 0 returns it immediately. ev is the event it
// carries, for the line; spent is the return it is — a failure (Nak), a
// hand-back (Defer) or a pass — and reason why, which decide where its dead
// letter is kept and what its line says should this return spend its budget
// ([attachment.retire]).
//
// A DELIVERY AT OR PAST THE BUDGET IS RETIRED, not only the budget's own: the
// broker's backstop stands [keepRetries] deliveries past it ([backstopFor]),
// so a message whose keep failed, or whose budget's delivery a node killed or
// stopped never settled, is delivered again — to a handler first, which may
// handle it — and retired by whichever return meets it there.
func (a *attachment) returnMsg(ctx context.Context, msg jetstream.Msg, ev *events.Event,
	delay time.Duration, spent queue.Outcome, reason string) {

	md, err := msg.Metadata()
	if err == nil && md.NumDelivered >= uint64(budgetFor(a.q.cfg)) {
		a.retire(ctx, msg, ev, md, spent, reason)
		return
	}
	if delay <= 0 {
		if err := msg.Nak(); err != nil {
			a.log.Warn("nak_failed", "error", err.Error())
		}
		return
	}
	if err := msg.NakWithDelay(delay); err != nil {
		a.log.Warn("nak_failed", "error", err.Error())
	}
}

// retire keeps msg, the delivery md describes, as its subscription's dead
// letter once spent — the return it was answered with — spent its budget, and
// terminates it: on the subject the contract keeps that return's copy on
// ([queue.RetiredTo]), a failure's with the group's poison and a pass's or a
// hand-back's with the group's unhandled deliveries, where a reader that can
// handle it now finds it.
//
// TERMINATED ONLY ONCE THE COPY IS KEPT. A keep that fails — refused, cut by a
// stop that closed the connection, a dead-letter stream electing its leader —
// leaves the message to the broker, returned at the failure path's spacing,
// and its next delivery keeps the copy: the broker's backstop stands
// [keepRetries] deliveries past the budget for that ([backstopFor]), so the
// delivery that spent the budget is not the broker's last. Terminated after a
// failed keep, as it was, the message was gone with no copy, though nothing
// was wrong with it but the moment. Only a keep that fails at the backstop's
// own delivery, which nothing follows, loses the copy, said as such.
//
// KEPT UNDER THE MESSAGE'S OWN IDENTITY ([letterOf]), so a keep made again
// after one whose answer was lost — the entry stored, the error all the
// keeper heard — is stored as nothing and answered already kept: one entry,
// however many keeps of it.
//
// KEPT WHETHER OR NOT THE LOOP IS STOPPING, under ctx's values without its
// cancellation: a stop cancels the consume loop's context, and the copy of a
// message whose last delivery a stop met — a hand-back of a batch taken as the
// queue stopped, or a pass or a failure handled just before — was kept under
// that context and lost with it, though nothing was wrong with the keep. The
// write is bounded all the same, by its own budget ([Queue.write]); a drain
// waits for it, since a delivery is in flight until its return is settled
// ([attachment.dispatchOne]); and so does a stop, which closes the connection
// only once no keep is in flight ([Queue.awaitKeeps]).
//
// ITS LINE IS THE CONTRACT'S ([queue.LogDeadLettered]), once the copy is kept,
// never one spelled here, so both backends name one situation alike.
func (a *attachment) retire(ctx context.Context, msg jetstream.Msg, ev *events.Event,
	md *jetstream.MsgMetadata, spent queue.Outcome, reason string) {

	a.q.keeping.Begin()
	defer a.q.keeping.End()
	subject := queue.RetiredTo(a.key.topic, a.key.group, spent)
	if err := a.q.deadLetter(context.WithoutCancel(ctx), subject, letterOf(md), msg.Data()); err != nil {
		a.keepFailed(ctx, msg, md, subject, err)
		return
	}
	// Terminated here, so nothing will redeliver this sequence and its
	// failure count is dead weight.
	a.settled(md.Sequence.Stream)
	queue.LogDeadLettered(ctx, a.log, a.key.topic, a.key.group, ev,
		int(min(md.NumDelivered, math.MaxInt32)), spent, reason)
	// Term, not Ack: the message is being discarded from this
	// subscription deliberately, and Term says so to anyone reading
	// consumer state.
	if err := msg.Term(); err != nil {
		a.log.Warn("term_failed", "error", err.Error())
	}
}

// keepFailed answers a retirement whose keep failed with err: the message
// goes back, at the failure path's spacing, for its next delivery to keep —
// unless md is the broker's last delivery of it ([backstopFor]), when nothing
// would bring it back and the copy is lost.
func (a *attachment) keepFailed(ctx context.Context, msg jetstream.Msg, md *jetstream.MsgMetadata,
	subject string, err error) {

	if md.NumDelivered >= uint64(backstopFor(a.q.cfg)) {
		a.settled(md.Sequence.Stream)
		a.log.ErrorContext(ctx, "dead_letter_failed", "subject", subject,
			"deliveries", md.NumDelivered, "error", err.Error(),
			"detail", "the copy is lost: this was the broker's last delivery of the message, "+
				"the keep failed at every one of the deliveries its backstop gives past the "+
				"budget, and nothing redelivers it")
		if termErr := msg.Term(); termErr != nil {
			a.log.Warn("term_failed", "error", termErr.Error())
		}
		return
	}
	a.log.WarnContext(ctx, "dead_letter_failed", "subject", subject,
		"deliveries", md.NumDelivered, "error", err.Error(),
		"detail", "nothing was kept: the message goes back, and its next delivery keeps the copy")
	if nakErr := msg.NakWithDelay(a.q.nakBackoff(md.NumDelivered)); nakErr != nil {
		a.log.Warn("nak_failed", "error", nakErr.Error())
	}
}

// letterOf is the identity a subscription's dead letter of the delivery md
// describes is kept under ([Queue.deadLetter]): the message the broker stored —
// its stream, its sequence there and the broker's instant for it — which every
// delivery of it carries alike, whichever node it reaches. The instant tells a
// message apart from the one a stream deleted and made again stores at a
// reused sequence. Quoted, so no stream's name runs into the numbers after it.
func letterOf(md *jetstream.MsgMetadata) string {
	return fmt.Sprintf("%q %d %d", md.Stream, md.Sequence.Stream, md.Timestamp.UnixNano())
}

// --- the four attachment verbs -------------------------------------------

// Quiesce stops taking new work while staying attached.
func (q *Queue) Quiesce(_ context.Context, topic, group string) (bool, error) {
	if q.isClosed() {
		return false, ErrClosed
	}
	atts := q.lookup(topic, group)
	for _, a := range atts {
		a.setQuiesced(true)
	}
	return len(atts) > 0, nil
}

// Unquiesce resumes a quiesced attachment.
//
// Does NOT touch pause holds: a seat resuming from a stale-renew window may
// still be legitimately held by another subsystem (the engine's own is the
// park of a node with no turn engine), and clearing that would restart the
// requeue loop the hold exists to stop.
//
// This needs no prefetch reclamation — pull
// consumers never pushed anything into a client-side queue, so resuming is
// simply fetching again.
func (q *Queue) Unquiesce(_ context.Context, topic, group string) (bool, error) {
	if q.isClosed() {
		return false, ErrClosed
	}
	var was bool
	for _, a := range q.lookup(topic, group) {
		if a.quiesced.Swap(false) {
			was = true
		}
	}
	return was, nil
}

// Detach closes this process's consumers, leaving the durable subscription.
//
// Non-destructive: the subscription, its cursor and its retained mail all
// survive, so unacked messages return to whoever attaches next and messages
// published while nothing is attached are kept. That is what makes a seat
// handoff cheap and an unowned seat safe.
//
// Releases this pair's pause holds — a hold is state about THIS attachment,
// and one that outlived a detach would leave a node that re-attached later
// silently deaf.
func (q *Queue) Detach(_ context.Context, topic, group string) (bool, error) {
	key := attachKey{topic, group}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return false, ErrClosed
	}
	atts := q.attachments[key]
	delete(q.attachments, key)
	delete(q.holds, key)
	// Let go of, and still reported by Loops until each loop returns:
	// Detach does not join a running handler, so a loop may outlive it.
	q.retireLocked(atts)
	q.mu.Unlock()

	for _, a := range atts {
		a.close()
	}
	return len(atts) > 0, nil
}

// isClosed reports whether Stop has run, for the verbs that must refuse
// afterwards rather than report success against a connection that is gone.
func (q *Queue) isClosed() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

func (q *Queue) lookup(topic, group string) []*attachment {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.attachments[attachKey{topic, group}])
}

// --- in-flight accounting -------------------------------------------------

func (q *Queue) beginHandler() { q.inFlight.Begin() }
func (q *Queue) endHandler()   { q.inFlight.End() }

// sleep waits for d or until ctx is done, reporting the context error so a
// caller can return promptly on shutdown.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
