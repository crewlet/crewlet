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
// Load-bearing at the shipped default: with `notification_coalesce_window_seconds`
// at 0 the linger deadline has passed by the first tail fetch, so this window is
// the whole busy-case coalescing mechanism; what piled up while the previous
// handler ran must come back inside it, or the seat runs a second turn.
//
// 50 ms because the fetch collects what the server already has, two orders of
// magnitude above a round trip to a same-process or same-datacentre broker. A
// far broker whose backlog it truncates should raise the linger window.
// Never longer than the idle poll, which an operator shortened for responsiveness.
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
// Flat spacing would spend the budget in half a minute, dead-lettering what a
// sat-out LLM entry or a recovering database would have handled. Doubling to
// this spends it over about ten minutes, several times the minute an entry sits
// out after a spent plan (internal/providers/llm/harness, bench.go), while a
// blip is retried within a second or two.
// The in-memory twin redelivers immediately, modelling the budget, not the
// clock; so it omits that a backing-off failure lets newer mail of its
// conversation go first. The contract orders neither (queue.OrderForDispatch).
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
// The first waits [Queue.nakDelay], each later one twice as long, up to
// [Queue.nakCeiling]. n counts from 1 and is the caller's: for a failure, how
// many times this message failed here ([attachment.failures]); for a pass, the
// delivery count itself ([attachment.pass]).
//
// The arithmetic is [backoff.Doubling]'s, which keeps a count off the wire from
// overflowing into a negative wait. The count is clamped to an int first: a
// uint64 past the int range would convert to a negative attempt and read as the
// first return rather than the latest.
func (q *Queue) nakSpacing(n uint64) time.Duration {
	return backoff.Doubling(int(min(n, math.MaxInt32)), q.nakDelay(), q.nakCeiling())
}

// LeastTimeToDeadLetter is the least time this backend carries a failing
// message, at its defaults, from its first delivery to its dead letter, every
// return but the last spaced by [Queue.nakBackoff] at the floor of its jitter:
// about 481 s. For a layer above that holds a wait of its own under it, in a
// test (internal/sandbox's re-announcement). The least on the failure path
// alone: a hand-back returns at once, and each attachment spaces its own
// failures from the first (the package doc's "The least time a failing message
// is carried").
func LeastTimeToDeadLetter() time.Duration {
	// THE DEFAULTS: a queue configured with nothing, whose spacing reads
	// nothing else ([Queue.nakSpacing]). The floor is what [backoff.Jitter]
	// returns at the far end of its spread.
	defaults := &Queue{}
	var least time.Duration
	for n := uint64(1); n < uint64(budgetFor(defaults.cfg)); n++ {
		spacing := defaults.nakSpacing(n)
		least += spacing - time.Duration(float64(spacing)*nakJitter)
	}
	return least
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

	// failures counts how many times each message has failed here, keyed on
	// its stream sequence. Not the delivery count: that moves on every healthy
	// return ([budgetFor]), so a message handed back five times would meet its
	// first genuine failure at the 30 s ceiling.
	// In memory because a Nak cannot carry a header; a redelivery elsewhere or
	// after a re-attach restarts at one, the safe direction (a fast retry).
	// Each record also says whether a failure was the message's own, not a
	// node's ([queue.NakNode]), and whether this attachment wrote its failure
	// mark on the broker (failuremarks.go), so a return spending the budget
	// knows where to keep the message without asking the broker.
	failures   map[uint64]*failureRecord
	failuresMu sync.Mutex

	// spaced is the spaced returns the loop owes ([attachment.sendSpaced]),
	// touched by the loop alone.
	spaced []spacedReturn

	// aside is what this attachment holds aside ([queue.OutcomeAside]),
	// readmitted what a Readmit handed back for the next drain, readmits the
	// keys readmitted since the running dispatch began, and keeperRunning
	// whether [attachment.keepAside] runs (aside.go).
	asideMu       sync.Mutex
	aside         []asideDelivery
	readmitted    []delivery
	readmits      map[string]struct{}
	keeperRunning bool
}

// failureRecord is what one attachment knows of one message's failures
// ([attachment.failures]).
type failureRecord struct {
	// count is how many times the message failed here, the node's own
	// failures included: what its spacing doubles by.
	count uint64
	// itself is whether one of them was the message's own
	// ([queue.Result.FailsMessage]).
	itself bool
	// mark is the sequence the failure mark this attachment wrote of the
	// message is stored at ([Queue.markFailure]), zero for none.
	mark uint64
}

// failed records one failure of a message and reports the count; itself says
// whether it was the message's own, and the record answers whether its mark is
// still to be written — the message's own failure, here for the first time or
// with the mark not written yet.
func (a *attachment) failed(seq uint64, itself bool) (count uint64, mark bool) {
	a.failuresMu.Lock()
	defer a.failuresMu.Unlock()
	if a.failures == nil {
		a.failures = map[uint64]*failureRecord{}
	}
	rec := a.failures[seq]
	if rec == nil {
		rec = &failureRecord{}
		a.failures[seq] = rec
	}
	rec.count++
	rec.itself = rec.itself || itself
	return rec.count, itself && rec.mark == 0
}

// marked records that this attachment wrote seq's failure mark, stored at
// mark.
func (a *attachment) marked(seq, mark uint64) {
	a.failuresMu.Lock()
	defer a.failuresMu.Unlock()
	if rec := a.failures[seq]; rec != nil {
		rec.mark = mark
	}
}

// failedItself reports whether a failure of seq here was the message's own.
func (a *attachment) failedItself(seq uint64) bool {
	a.failuresMu.Lock()
	defer a.failuresMu.Unlock()
	rec := a.failures[seq]
	return rec != nil && rec.itself
}

// settled forgets a message that will not come back — acked, terminated, or
// handed on — and answers where the failure mark this attachment wrote of it
// is stored, zero for none, which its settling deletes. Without it the map
// would hold every sequence this attachment ever failed for the life of the
// seat.
func (a *attachment) settled(seq uint64) (mark uint64) {
	a.failuresMu.Lock()
	defer a.failuresMu.Unlock()
	rec := a.failures[seq]
	delete(a.failures, seq)
	if rec == nil {
		return 0
	}
	return rec.mark
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
// handed back, and a blocked loop fetches nothing until a verb opens its gate,
// so it sends the spaced returns it owes first ([attachment.sendSpaced]).
// Only the loop writes it, so a load and a store cannot lose a top.
func (a *attachment) reachedTop(blocked bool) {
	next := (a.top.Load()>>1 + 1) << 1
	if blocked {
		a.sendSpaced()
		next |= 1
	}
	a.top.Store(next)
}

// close stops this consumer taking new work and returns immediately.
//
// It does not join a running handler: Detach is the fenced-release path, and a
// node that has lost a seat must give it up now, not when its turn ends.
//
// It leaves an outstanding fetch to end on its own, unlike [attachment.stop].
// That fetch's pull request is still open at the broker, so a message
// published just after the detach reaches [attachment.dispatchOne], finds this
// consumer detached, and is handed back at once for the successor. Abandoned,
// the client drops its subscription while the broker may be sending, and the
// message in flight waits out the whole ack window, on exactly the lease
// movement a hand-back exists to keep cheap.
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

// fetch asks the broker for up to n messages, waiting at most wait, hands each
// to take and reports whether the broker answered (sent, and not refused); the
// one way every loop fetches, so a stop reaches it ([attachment.fetching]).
//
// Two context options, no expiry option: FetchMaxWait is a timer only, so a stop
// waited out the window. The context option's deadline both becomes the
// request's expiry (a tenth held back) and ends the client's wait, so a message
// handed over just before the expiry is dropped and sits out the ack window. So
// the first carries only a deadline, past wait by the held-back share so the
// broker's expiry is wait itself; the second, [attachment.fetching], has none,
// ending on the broker's timeout plus grace or a stop (pinned by
// [TestAFetchExpiresAfterItsWaitAndAStopEndsItAtOnce]).
func (a *attachment) fetch(n int, wait time.Duration, take func(jetstream.Msg)) (bool, error) {
	if err := a.fetching.Err(); err != nil {
		return false, err
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
			return false, nil
		}
		return false, err
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
	err = batch.Error()
	return err == nil, err
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
// attachment's consumer, and each is TAKEN FOR ONE ATTACHMENT of the pair:
// the pair's current attachment, minted on its first read and ended by its
// Detach, and a hold asked for any other is refused. Why each of the three is
// so is the package doc's "A pause hold lives on the queue, for one
// attachment".

// Attachment answers the pair's current attachment, minting it on the first
// read since the pair's last Detach.
func (q *Queue) Attachment(_ context.Context, topic, group string) (queue.Attachment, error) {
	key := attachKey{topic, group}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return 0, ErrClosed
	}
	at, ok := q.current[key]
	if !ok {
		q.minted++
		at = q.minted
		q.current[key] = at
	}
	return at, nil
}

func (q *Queue) held(key attachKey) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.holds[key]) > 0
}

// PauseTopic takes a named hold on one subscription, for the attachment at
// names, refusing an attachment that is not the pair's current one.
func (q *Queue) PauseTopic(_ context.Context, topic, group string, at queue.Attachment, reason string) error {
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
	if cur, ok := q.current[key]; !ok || cur != at {
		return fmt.Errorf("jetstream: hold %q on %s/%s for attachment %d: %w",
			reason, topic, group, at, queue.ErrAttachmentGone)
	}
	if q.holds[key] == nil {
		q.holds[key] = map[string]struct{}{}
	}
	q.holds[key][reason] = struct{}{}
	return nil
}

// ResumeTopic releases one reason's hold on the attachment at names, flushing
// when none remain; under an attachment that has ended it lifts nothing.
func (q *Queue) ResumeTopic(_ context.Context, topic, group string, at queue.Attachment, reason string) error {
	if reason == "" {
		reason = "default"
	}
	key := attachKey{topic, group}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrClosed
	}
	if cur, ok := q.current[key]; !ok || cur != at {
		return nil
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
// Separate from a pause hold although both stop deliveries: a hold is cleared
// by the subsystem that took it, a quiesce by detaching or attaching again, and
// a stale quiesce is invisible from outside until someone attaches, which can
// strand a seat.
//
// Reported for the conformance suite: a negative path asserts that an operation
// which declined to act touched nothing, comparing every observable a backend
// exports, so one kept to itself is one that case silently stops checking.
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
// Reported for the conformance suite: a case asserting that nothing more was
// delivered reads its journal once this backend has nothing left that could
// deliver, and nothing else it exports says that. A loop is the one goroutine
// that dispatches for its attachment, only between a fetch and its next loop
// top, so a loop that has ended, or has stood at a top with its gate shut since
// the case's last verb, has nothing left to hand a handler; the broker's counts
// are snapshots and cannot tell a parked loop from one between two pulls. A
// loop let go of is reported until it ends, since it can still be inside a
// handler or holding a fetch that delivers.
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
// Reported for the conformance suite. A case asserting that an unsubscribed
// feed receives nothing reads it once the feed has stopped, which is later
// than its unsubscribe returning: the client ends the consumption on its own
// goroutine, and a message already in the callback's path can still be handed
// over. Read off the consumption's Closed signal, which the client closes once
// its callback has returned for the last time.
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
	// A provisioning read-back, budgeted like its siblings: the boot's
	// context carries no deadline, so unbudgeted it would get nats.go's own
	// five-second default, tighter than [jsprovision.Budget] and
	// [jsprovision.SlowAfter], and a clustered boot would fail here with
	// nothing logged before it.
	//
	// Two verbs, for two hazards. [jsprovision.ReadWith] re-issues a request
	// the group never answered, under [jsprovision.LookupBudget];
	// [jsprovision.SettleWith] waits out the propagation window, which this
	// read is inside by construction: EnsureSubscription has just created
	// the consumer, so the member that made it can still be told it is not
	// there. Silence and not-yet-visible need different remedies.
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
	// What it held aside goes back first, before the loop is forgotten and a
	// stop closes the connection under it, and what it owes spaced with it.
	defer a.handBackAside(ctx)
	defer a.sendSpaced()
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
			a.takeOne(ctx, h)
		}
	})
}

// takeOne is one cycle of a Subscribe loop whose gate is open: one delivery
// dispatched, or none waiting.
//
// A READMITTED DELIVERY FIRST ([queue.OutcomeAside]): it was fetched before
// anything waiting now. Then THE LANE, one message at a time like the rest: a
// message it held is this round's delivery, and the topic is read only when
// the lane had nothing. A cycle the broker answered with nothing sends what the
// loop owes ([attachment.sendSpaced]), which a delivery taken sends first
// (dispatchOne); one whose fetch failed or was never sent keeps it owed.
func (a *attachment) takeOne(ctx context.Context, h queue.Handler) {
	took := false
	take := func(readmitted bool) func(jetstream.Msg) {
		return func(msg jetstream.Msg) {
			took = true
			a.dispatchOne(ctx, msg, readmitted, h)
		}
	}
	if a.readmittedOne(take(true)); took {
		return
	}
	if err := a.fetchLane(1, take(false)); err != nil {
		a.logLaneFetchErr(ctx, err)
	}
	if took {
		return
	}
	//nolint:contextcheck // a fetch runs on the attachment's fetch scope, never the loop's: see [attachment.fetching]
	answered, err := a.fetch(1, a.q.fetchWait(), take(false))
	if answered && !took {
		a.sendSpaced()
	}
	if err != nil {
		a.logFetchErr(ctx, err)
	}
}

// fetchLane takes what the priority lane holds RIGHT NOW, up to n, and hands
// each to take; on a topic with no lane it does nothing.
//
// Without waiting: the lane is asked first on every cycle and is almost always
// empty, so a wait would be added to every delivery the topic makes. An event
// landing just after is taken next cycle, at most one poll away
// ([Queue.fetchWait]).
//
// Not on the fetch scope ([attachment.fetching]), which only bounds a waiting
// fetch: a no-wait request is answered at once, at worst one second after a
// silent broker, so a stop is held no longer than that.
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

// dispatchOne runs a handler for one message and applies its outcome;
// readmitted says a Readmit handed it back ([queue.OutcomeAside]). The spaced
// returns the loop owes go back first, behind msg ([attachment.sendSpaced]).
func (a *attachment) dispatchOne(ctx context.Context, msg jetstream.Msg, readmitted bool, h queue.Handler) {
	a.sendSpaced()
	held, ok := a.toDelivery(msg)
	if !ok {
		return
	}
	held.readmitted = readmitted
	ev := held.ev
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
		a.setBack(ctx, []delivery{held}, a.blockedBy())
		return
	}
	a.q.beginHandler()
	done := a.runningOn(ev)
	a.beginDispatch()
	res := runHandler(a.withDelivery(ctx, held), a.log, ev, h)
	a.apply(ctx, held, res)
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

// withHeadroom states each message's remaining headroom on the handler's
// context: [queue.DeliveriesLeftFor] for one message, [queue.DeliveriesLeft]
// for the partition's, which the contract folds from this same list so no
// backend decides it.
//
// Off this message's own metadata, not [attachment.failures]: that map is one
// attachment's record of failures and resets on every re-attach, while the
// budget a message spends is carried by the message and survives every
// handoff.
//
// A message whose metadata will not parse is left out: unreadable, not zero.
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

// withDelivery is ctx as a handler of ds is handed it: each message's headroom,
// how to keep them from timing out, and how many more the attachment takes
// aside ([queue.AsideRoom]).
func (a *attachment) withDelivery(ctx context.Context, ds ...delivery) context.Context {
	return queue.WithAsideRoom(a.withProgress(a.withHeadroom(ctx, ds...), ds...), a.asideRoom())
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
func (a *attachment) apply(ctx context.Context, held delivery, res queue.Result) {
	msg, ev := held.msg, held.ev
	queue.LogResult(ctx, a.log, a.key.topic, a.key.group, ev, res)
	switch res.Outcome {
	case queue.OutcomeAck:
		// FORGOTTEN FIRST. A message that succeeded after failing twice
		// must not carry those two failures into a later redelivery of
		// the same sequence, and the map would otherwise hold every
		// sequence this seat ever failed for the life of the attachment.
		var mark uint64
		if md, err := msg.Metadata(); err == nil {
			mark = a.settled(md.Sequence.Stream)
		}
		if err := msg.Ack(); err != nil {
			a.log.Warn("ack_failed", "error", err.Error())
		}
		a.clearMark(ctx, mark)

	case queue.OutcomeDefer:
		// Defer means this process has lost the right to do this work. Nak
		// returns it at once; letting the ack timer expire would park a
		// seat's mail for the whole AckWait on every lease movement.
		//
		// It costs one delivery count, so it goes through the budget check
		// the failure path uses, not a bare Nak, which would let MaxDeliver
		// discard a healthy event with no dead-letter copy or line. Never
		// republish: a republished event is a NEW message, and the
		// completion ledger and the batch layer's aging key on the identity
		// a Nak preserves.
		a.handBack(ctx, msg, ev, res.Reason)
		// Quiescing is the other half of a deferral: continuing to fetch
		// would hand this process more work it has equally lost the
		// right to do.
		a.setQuiesced(true)

	case queue.OutcomeNak:
		a.nakOrDeadLetter(ctx, msg, ev, res.FailsMessage())

	case queue.OutcomePass:
		// Neither a failure nor a deferral, and the attachment goes on
		// taking mail: see [attachment.pass].
		a.pass(ctx, msg, ev, res.Reason)

	case queue.OutcomeAside:
		a.setAside(ctx, []delivery{held}, res.Key)
	}
}

// pass returns a message this node will not handle and a peer may
// ([queue.OutcomePass]) through the budget boundary every return crosses, so a
// message no node reads is dead-lettered rather than handed round for ever, and
// spaced as a failure is, by [Queue.nakBackoff].
//
// A pass that spends the budget keeps its copy on the group's unhandled
// subject, apart from the poison, unless a delivery failed as itself
// ([queue.RetiredTo]); its line names reason. Spaced by delivery
// count, not [attachment.failures]: a pass is no failure, and the count that
// must grow is the one every node shares, or four nodes would give a message
// up in four minutes, not the budget's ten. Not quiesced, unlike a deferral:
// nothing is wrong with this node, only with this message.
func (a *attachment) pass(ctx context.Context, msg jetstream.Msg, ev *events.Event, reason string) {
	// A MESSAGE WHOSE METADATA WILL NOT PARSE is passed at the shortest
	// spacing, for [attachment.nakOrDeadLetter]'s reason: returnMsg reads
	// the same metadata for the budget and treats an unreadable one as "not
	// yet spent".
	var delivered uint64 = 1
	if md, err := msg.Metadata(); err == nil {
		delivered = md.NumDelivered
	}
	a.returnMsg(ctx, msg, ev, a.q.nakBackoff(delivered), queue.OutcomePass, reason, false)
}

// nakOrDeadLetter redelivers a FAILED message until its budget is spent, then
// dead-letters it. Spaced by [Queue.nakBackoff]: immediate redelivery spins
// against whatever is broken, and flat spacing spends every attempt before it
// can heal. The decision lives here because the dead-letter subject and body
// are known here; MaxDeliver stays as a backstop against an infinite loop.
//
// itself is whether the failure is the message's own
// ([queue.Result.FailsMessage]), not this node's ([queue.NakNode]): it is
// marked on the broker once before the message goes back
// ([attachment.markFailed]), so whichever member retires it keeps it with the
// poison.
func (a *attachment) nakOrDeadLetter(ctx context.Context, msg jetstream.Msg, ev *events.Event,
	itself bool) {

	// A MESSAGE WHOSE METADATA WILL NOT PARSE STILL GOES BACK, at the
	// shortest spacing: the alternative is dropping a delivery over a
	// header, and returnMsg reads the same metadata for the budget and
	// already treats an unreadable one as "not yet spent".
	var failures uint64 = 1
	if md, err := msg.Metadata(); err == nil {
		var mark bool
		failures, mark = a.failed(md.Sequence.Stream, itself)
		if mark {
			a.markFailed(ctx, md)
		}
	}
	a.returnMsg(ctx, msg, ev, a.q.nakBackoff(failures), queue.OutcomeNak, "", itself)
}

// markFailed writes the failure mark of the message md describes, which failed
// as itself here (failuremarks.go), BEFORE the message goes back, so the member
// its next delivery reaches reads it. A mark that could not be written is
// logged and written again at the message's next failure here; meanwhile a
// member that retires it keeps it with the unhandled deliveries, the safe
// direction. Under ctx's values without its cancellation, as a keep is
// ([attachment.retire]): a stop is no reason to forget a failure.
func (a *attachment) markFailed(ctx context.Context, md *jetstream.MsgMetadata) {
	mark, err := a.q.markFailure(context.WithoutCancel(ctx), failureSubject(a.key, md))
	if err != nil {
		a.log.WarnContext(ctx, "failure_mark_unkept", "seq", md.Sequence.Stream, "error", err.Error(),
			"detail", "the message failed as itself, and the member that retires it may not know: "+
				"kept with the unhandled deliveries rather than the poison, should its budget be "+
				"spent before a failure here writes the mark")
		return
	}
	a.marked(md.Sequence.Stream, mark)
}

// clearMark deletes the failure mark stored at mark, zero for none, of a
// message now settled: nothing will read it again. One that could not be
// deleted — gone already, replaced by another member's — goes with the marks'
// stream's age.
func (a *attachment) clearMark(ctx context.Context, mark uint64) {
	if mark == 0 {
		return
	}
	if err := a.q.clearFailure(context.WithoutCancel(ctx), mark); err != nil {
		a.log.DebugContext(ctx, "failure_mark_undeleted", "mark", mark, "error", err.Error(),
			"detail", "the settled message's failure mark goes with its stream's age")
	}
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
	a.returnMsg(ctx, msg, ev, 0, queue.OutcomeDefer, reason, false)
}

// returnMsg is the one place a message goes back, and the one place the
// delivery budget is spent. delay 0 returns it immediately; a positive delay is
// owed until the loop holds its next delivery ([attachment.sendSpaced]). ev is
// the event for the line; spent is the return (a failure, hand-back or pass)
// and reason why; itself is whether this return is a failure of the message's
// own, which with its earlier failures decides where its dead letter is kept
// ([attachment.retire]).
//
// A delivery at or past the budget is retired, not only the budget's own: the
// broker's backstop stands [keepRetries] deliveries past it ([backstopFor]),
// so a message whose keep failed, or whose budget delivery never settled, is
// delivered again and retired by whichever return meets it.
func (a *attachment) returnMsg(ctx context.Context, msg jetstream.Msg, ev *events.Event,
	delay time.Duration, spent queue.Outcome, reason string, itself bool) {

	md, err := msg.Metadata()
	if err == nil && md.NumDelivered >= uint64(budgetFor(a.q.cfg)) {
		a.retire(ctx, msg, ev, md, spent, reason, itself)
		return
	}
	if delay <= 0 {
		if err := msg.Nak(); err != nil {
			a.log.Warn("nak_failed", "error", err.Error())
		}
		return
	}
	// IN PROGRESS WHILE OWED: its ack window restarts now, where the return
	// used to land, so holding it costs no redelivery; a process that dies
	// before sending it leaves it to that window, delivered again at least once.
	if err := msg.InProgress(); err != nil {
		a.log.Warn("owed_progress_failed", "error", err.Error(),
			"detail", "a return owed until the loop's next delivery keeps its last ack window")
	}
	a.spaced = append(a.spaced, spacedReturn{msg: msg, delay: delay})
}

// spacedReturn is a failure's or a pass's return of msg at delay, which the
// loop owes until [attachment.sendSpaced].
type spacedReturn struct {
	msg   jetstream.Msg
	delay time.Duration
}

// sendSpaced sends the spaced returns the loop owes. The loop calls it once its
// next cycle holds a delivery or the broker answered that none waited, or once
// it will take none (gated, or ending), so each comes back BEHIND the mail that
// waited (package doc, "What it costs, measured"); a fetch refused or never
// sent keeps them owed, that mail still waiting. Sent at once, a return's
// backoff raced the next pull, which a loaded node or a leaf's link loses, and
// the broker serves a due redelivery before new mail. Owed for a fetch, or a
// drain's later handlers and the next drain, as that drain's own mail waits.
func (a *attachment) sendSpaced() {
	for _, r := range a.spaced {
		if err := r.msg.NakWithDelay(r.delay); err != nil {
			a.log.Warn("nak_failed", "error", err.Error())
		}
	}
	a.spaced = nil
}

// retire keeps msg (delivery md), whose return spent its budget, as its
// subscription's dead letter and terminates it: with the group's poison, or its
// unhandled deliveries if no delivery failed as itself ([queue.RetiredTo]).
// Whether it failed is the message's ([attachment.failedAsItself]), not this
// return's. Terminated only once the copy is kept: a failed keep leaves the
// message to the broker, whose backstop stands [keepRetries] deliveries past
// the budget ([backstopFor]); only a failure at the backstop's own delivery
// loses the copy, said as such. Kept under the message's identity ([letterOf]),
// so a repeat is answered already kept, and under ctx's values without its
// cancellation, since a stop would lose the copy; the write is bounded
// ([Queue.write]), and a drain and a stop wait for it ([attachment.dispatchOne],
// [Queue.awaitKeeps]). Its line is the contract's ([queue.LogDeadLettered]).
func (a *attachment) retire(ctx context.Context, msg jetstream.Msg, ev *events.Event,
	md *jetstream.MsgMetadata, spent queue.Outcome, reason string, itself bool) {

	a.q.keeping.Begin()
	defer a.q.keeping.End()
	keep := context.WithoutCancel(ctx)
	failed, found := a.failedAsItself(keep, md, itself)
	subject := queue.RetiredTo(a.key.topic, a.key.group, failed)
	if err := a.q.deadLetter(keep, subject, letterOf(md), msg.Data()); err != nil {
		a.keepFailed(ctx, msg, md, subject, err)
		return
	}
	// Terminated here, so nothing will redeliver this sequence and its
	// failure count is dead weight.
	own := a.settled(md.Sequence.Stream)
	queue.LogDeadLettered(ctx, a.log, a.key.topic, a.key.group, ev,
		int(min(md.NumDelivered, math.MaxInt32)), spent, reason, failed)
	// Term, not Ack: the message is being discarded from this
	// subscription deliberately, and Term says so to anyone reading
	// consumer state.
	if err := msg.Term(); err != nil {
		a.log.Warn("term_failed", "error", err.Error())
	}
	// ITS MARKS, whoever wrote them: the message is settled.
	a.clearMark(ctx, own)
	if found != own {
		a.clearMark(ctx, found)
	}
}

// failedAsItself reports whether the message md describes failed as itself at
// any of its deliveries: this return a failure of its own (itself), a failure
// of it here before ([attachment.failedItself]), or a mark on the broker that
// the member it failed on wrote (failuremarks.go), read only when neither
// answers — and, where it read one, where that mark is stored. A mark the
// broker could not be asked for answers no: the message is kept with the
// unhandled deliveries, the safe direction, and the line says so.
func (a *attachment) failedAsItself(ctx context.Context, md *jetstream.MsgMetadata,
	itself bool) (failed bool, found uint64) {

	if itself || a.failedItself(md.Sequence.Stream) {
		return true, 0
	}
	found, marked, err := a.q.failureMarked(ctx, failureSubject(a.key, md))
	if err != nil {
		a.log.WarnContext(ctx, "failure_mark_unread", "seq", md.Sequence.Stream, "error", err.Error(),
			"detail", "whether the message failed as itself on another member is unknown, so it "+
				"is kept with the unhandled deliveries: a copy its reader may find failing, never "+
				"a healthy delivery kept with the poison")
		return false, 0
	}
	return marked, found
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
// Releases this pair's pause holds and ENDS ITS ATTACHMENT, attached or not:
// a hold is state about THIS attachment, and one that outlived a detach, or
// was taken for it after one, would leave a node that re-attached later
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
	delete(q.current, key)
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
