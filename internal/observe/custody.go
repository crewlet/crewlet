package observe

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
)

// Custody: the event log of a node that keeps none (ADR-0025).
//
// The [Writer] rule is that the publisher writes its own rows, inline, into its
// own database — and a node without the `data` role deletes its database at
// every boot. Written there, its audit rows would be gone at its next restart,
// and nothing could have read them in between: the API runs only where the
// data is. So such a node hands its events to the data nodes, and exactly ONE
// of them keeps each one.
//
// # Two halves, and the broker between them
//
// [Custody] runs on the node without `data`. It buffers what the node publishes
// for a moment — a turn never waits on its own audit trail — and publishes it
// in batches onto [topics.CustodyRecords], durably: a batch the broker has
// acknowledged survives the node that published it, which a batch held in its
// memory waiting for a data node to answer did not.
//
// [Keeper] runs on every data node, in one fleet-wide group, [CustodyGroup].
// Each batch it takes it writes into its own event log, where `GET /events` and
// the fleet's history reads find it.
//
// # Exactly one keeper, decided after the write
//
// A group delivers a batch whose acknowledgement was lost AGAIN, to whichever
// member asks next — and a publisher that was not told its batch was stored
// sends it again. Written by two data nodes, a batch is two copies of one
// history, and every figure derived from a node's own log counts it twice: the
// usage domain sums each data node's day, so a stateless node's spend would be
// billed once per data node that wrote it. So after writing a batch a keeper
// CLAIMS it in coordination, create-only ([coord.Custody]): the claim's winner
// keeps the batch, any other writer deletes its copy, and the delivery is
// acknowledged only once that is settled — or once the fleet's record of its
// keeper is one nothing can read ([coord.ErrCustodyUndecidable]), which no
// redelivery could settle and every one would only write again on another
// data node.
//
// Write first and claim second, never the reverse: a node that claimed and died
// before writing would hold a batch nobody wrote. In this order the worst a
// crash leaves is a written copy whose keeper this node never learned —
// remembered in its own store ([store.EventLog.WriteCustody]) and settled by
// [Keeper.Reconcile] the same way, at its next boot and on every pass after.
//
// Until the node that lost the claim has settled its copy, both logs hold the
// batch's rows. That is the one moment two stores share a row, and the fleet's
// history reads are built for it: every count a node answers is of the rows it
// KEEPS, with the rows of a batch it has not settled named beside it, and the
// asker counts each named row once (internal/eventfan, and internal/store's
// unsettled.go).

// CustodyGroup is the data nodes' fleet-wide group on [topics.CustodyRecords].
const CustodyGroup = "event-custody"

// Publisher is what [Custody] publishes its batches through: the node's queue.
type Publisher interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
	EnsureSubscription(ctx context.Context, topic, group string) (bool, error)
}

// Custody publishes what this node publishes onto the custody topic, for a data
// node to keep.
//
// # It never holds a publish, and says what it drops
//
// A listener runs in the publishing goroutine, so the event goes into a
// BOUNDED buffer and one goroutine publishes batches of it. A broker that
// cannot be reached is retried with a backoff while the buffer holds; a buffer
// that fills drops the NEWEST event and counts it, and the count is logged as
// the buffer drains, so a gap in the audit log is a line in this node's log
// rather than a silence.
//
// # A batch keeps its identity until the broker has it
//
// A batch is cut once and published under its own id until a publish is
// acknowledged. A publish whose acknowledgement was lost may have landed, and
// sent again under a new id it would be a second batch the keepers could not
// tell from the first; under the same id it is the same batch, which a keeper
// that already holds it writes over harmlessly and claims to the same answer.
type Custody struct {
	pub Publisher

	// buffered, maxBatch and maxBatchBytes are [custodyBuffer],
	// [custodyBatch] and [custodyBatchBytes]; flushEvery and retryBase
	// the pacing. Fields so a test can run them in milliseconds.
	buffered      int
	maxBatch      int
	maxBatchBytes int
	flushEvery    time.Duration
	retryBase     time.Duration

	mu      sync.Mutex
	pending []held
	// cut is the batch published and not yet acknowledged, nil when none
	// is: it goes first, under the id it was cut with.
	cut      *cutBatch
	ensured  bool
	dropped  int
	wake     chan struct{}
	stop     context.CancelFunc
	done     chan struct{}
	newBatch func() uuid.UUID
}

// held is one buffered event and what it adds to a batch.
type held struct {
	ev   *events.Event
	size int
}

// cutBatch is a batch cut from the buffer, with the id it is published under.
type cutBatch struct {
	id     uuid.UUID
	events []held
}

// custodyBuffer is how many events wait for the broker.
//
// FOUR THOUSAND AND NINETY-SIX: a turn publishes a handful of events per phase
// and a node runs at most `node.max_concurrent` turns, so this is minutes of a
// busy node's audit trail — long enough to ride out the broker's members
// restarting — while its worst case, every event carrying a large phase
// payload, stays in the tens of megabytes a stateless node is sized for.
const custodyBuffer = 4096

// custodyBatch and custodyBatchBytes bound one batch, whichever is reached
// first: a batch well under the broker's message limit
// ([queue.MaxPayloadBytes]) with room for the envelope, so one oversized phase
// event cannot make a whole batch unsendable.
const (
	custodyBatch      = 256
	custodyBatchBytes = 1 << 20
)

// custodyFlush is how long an event waits for companions before it ships.
//
// ONE SECOND: the audit trail is read by a person after the fact, so a second
// of latency is invisible, and it turns the dozens of events one turn
// publishes in a burst into one message rather than dozens. It is also the
// window a node that dies takes with it, which is why it is not longer.
const custodyFlush = time.Second

// custodyRetryBase is the first wait after a publish failed, doubling to the
// flush interval's sixty-fold — a minute — for a broker that stays away.
const custodyRetryBase = 500 * time.Millisecond

// NewCustody builds a custody publisher over the node's queue, stopped.
func NewCustody(pub Publisher) *Custody {
	if pub == nil {
		return nil
	}
	return &Custody{
		pub: pub, buffered: custodyBuffer, maxBatch: custodyBatch,
		maxBatchBytes: custodyBatchBytes, flushEvery: custodyFlush,
		retryBase: custodyRetryBase, wake: make(chan struct{}, 1),
		newBatch: uuid.New,
	}
}

// Listen returns the publish listener to register, or nil.
func (c *Custody) Listen() queue.PublishListener {
	if c == nil {
		return nil
	}
	return c.onPublish
}

// onPublish buffers one event the event log would keep. Never blocks, never
// fails the publish.
//
// ONLY WHAT A DATA NODE WOULD WRITE: a type with no category is not persisted
// anywhere, so carrying it would cost the broker and the keeper for a row
// nobody stores — and the custody batch itself is such a type, so this node's
// own batches are never handed to custody.
func (c *Custody) onPublish(_ context.Context, _ string, ev *events.Event) {
	if ev == nil || Category(ev.Type) == "" {
		return
	}
	raw := encode(ev)
	if raw == nil {
		return
	}
	c.mu.Lock()
	if len(c.pending) >= c.buffered {
		c.dropped++
		c.mu.Unlock()
		return
	}
	c.pending = append(c.pending, held{ev: ev, size: len(raw)})
	full := len(c.pending) >= c.maxBatch
	c.mu.Unlock()
	if full {
		c.signal()
	}
}

func (c *Custody) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Start runs the publishing loop, detached from ctx like every loop a node
// owns: [Custody.Stop] is what ends it, after a last flush.
func (c *Custody) Start(ctx context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.done != nil {
		c.mu.Unlock()
		return
	}
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	c.stop, c.done = stop, make(chan struct{})
	c.mu.Unlock()
	go c.run(loop)
}

// Stop ends the loop after one last attempt to publish what is buffered,
// bounded by ctx. What cannot be published by then is lost with the node, and
// logged as such.
func (c *Custody) Stop(ctx context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	stop, done := c.stop, c.done
	c.mu.Unlock()
	if stop == nil {
		return
	}
	stop()
	<-done
	// THE CALLER'S DEADLINE WITHOUT ITS CANCELLATION: a stop is routinely
	// driven by the signal it is cleaning up after, and a flush that
	// inherited it would send nothing.
	flush := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		flush, cancel = context.WithDeadline(flush, deadline)
		defer cancel()
	}
	for {
		batch := c.next()
		if batch == nil {
			return
		}
		if err := c.send(flush, batch); err != nil {
			c.mu.Lock()
			left := len(c.pending)
			c.mu.Unlock()
			log.WarnContext(ctx, "event_custody_lost", "events", len(batch.events)+left,
				"error", err.Error(),
				"detail", "this node holds no store, and the broker did not take "+
					"these events before it stopped: they are not in any event log")
			return
		}
	}
}

func (c *Custody) run(ctx context.Context) {
	defer close(c.done)
	ticker := time.NewTicker(c.flushEvery)
	defer ticker.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		case <-ticker.C:
		}
		for ctx.Err() == nil {
			batch := c.next()
			if batch == nil {
				break
			}
			if err := c.send(ctx, batch); err != nil {
				failures++
				wait := backoff.Jitter(backoff.Doubling(failures, c.retryBase, 60*c.flushEvery), 0.2)
				if failures == 1 || failures%10 == 0 {
					log.WarnContext(ctx, "event_custody_retrying", "events", len(batch.events),
						"batch", batch.id.String(), "attempt", failures,
						"retry_in", wait.String(), "error", err.Error())
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
				continue
			}
			if failures > 0 {
				log.InfoContext(ctx, "event_custody_recovered", "attempts", failures)
			}
			failures = 0
		}
	}
}

// next is the batch to publish: the one cut and not yet acknowledged, or a new
// one off the buffer, bounded by count and bytes — nil when there is nothing.
func (c *Custody) next() *cutBatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cut != nil {
		return c.cut
	}
	n, size := 0, 0
	for n < len(c.pending) && n < c.maxBatch {
		size += c.pending[n].size
		if n > 0 && size > c.maxBatchBytes {
			break
		}
		n++
	}
	if n == 0 {
		return nil
	}
	c.cut = &cutBatch{id: c.newBatch(), events: c.pending[:n:n]}
	c.pending = c.pending[n:]
	return c.cut
}

// send publishes one batch and, once the broker has it, lets it go and reports
// what was dropped since the last report.
//
// THE SUBSCRIPTION FIRST: the custody stream keeps a message only while a
// group that has not taken it exists, so a batch published before the group
// does is kept by nobody. The group is the data nodes', and created here too
// because this node cannot know whether any of them has attached yet.
func (c *Custody) send(ctx context.Context, batch *cutBatch) error {
	c.mu.Lock()
	ensured := c.ensured
	c.mu.Unlock()
	if !ensured {
		if _, err := c.pub.EnsureSubscription(ctx, topics.CustodyRecords, CustodyGroup); err != nil {
			return fmt.Errorf("observe: create the custody group: %w", err)
		}
		c.mu.Lock()
		c.ensured = true
		c.mu.Unlock()
	}
	carried := make([]*events.Event, len(batch.events))
	for i, h := range batch.events {
		carried[i] = h.ev
	}
	ev := events.New(types.CustodyBatch{Events: carried}, events.TraceContext{})
	ev.ID = batch.id
	ev.Source = "custody"
	if err := c.pub.Publish(ctx, topics.CustodyRecords, ev); err != nil {
		if !errors.Is(err, queue.ErrTooLarge) {
			return fmt.Errorf("observe: publish custody batch %s: %w", batch.id, err)
		}
		// A BATCH THE BROKER WILL NEVER TAKE is let go and said, never
		// retried: a batch is cut under a mebibyte, so this is ONE event
		// past the broker's message limit, and retrying it would hold
		// every event behind it for ever.
		log.WarnContext(ctx, "event_custody_oversized", "events", len(batch.events),
			"batch", batch.id.String(), "error", err.Error(),
			"detail", "an event larger than the broker carries; it is in no "+
				"event log, and the events after it are carried as usual")
	}
	c.mu.Lock()
	c.cut = nil
	dropped := c.dropped
	c.dropped = 0
	c.mu.Unlock()
	if dropped > 0 {
		log.WarnContext(ctx, "event_custody_dropped", "events", dropped,
			"detail", "the custody buffer was full while the broker was not taking "+
				"batches, so these never reached any event log")
	}
	return nil
}

// ---- the data node's half --------------------------------------------- //

// CustodyLog is the store half a [Keeper] writes through: this node's event
// log ([store.EventLog]).
type CustodyLog interface {
	WriteCustody(ctx context.Context, b store.CustodyBatch, at time.Time) error
	SettleCustody(ctx context.Context, batchID string, kept bool) error
	UnsettledCustody(ctx context.Context, before time.Time, after store.UnsettledBatch,
		limit int) ([]store.UnsettledBatch, error)
}

// CustodyClaims is the coordination half: who keeps a batch ([coord.Custody]).
type CustodyClaims interface {
	ClaimCustody(ctx context.Context, batch, node string) (string, error)
}

// Attachment is how the keeper joins the custody group and leaves it: the
// node's queue, whose voluntary leave is quiesce, wait, detach.
type Attachment interface {
	Subscribe(ctx context.Context, topic, group string, h queue.Handler) error
	Quiesce(ctx context.Context, topic, group string) (bool, error)
	Detach(ctx context.Context, topic, group string) (bool, error)
}

// Keeper writes custody batches into this data node's event log, keeping each
// one exactly once across the fleet — see the file doc.
type Keeper struct {
	log    CustodyLog
	claims CustodyClaims
	node   string
	now    func() time.Time

	queue Attachment
	stop  context.CancelFunc
	done  chan struct{}

	// mu guards the batches in hand: how many deliveries are being written
	// and settled, whether the keeper is leaving, and what a leave waiting
	// for the last of them is told by.
	mu       sync.Mutex
	inHand   int
	stopping bool
	idle     chan struct{}
}

// keeperSettleGrace is how old a batch this node wrote must be before
// [Keeper.Reconcile] settles it: past the moment the delivery that wrote it
// claims it itself. A shorter grace costs nothing worse than a claim made
// twice — both are told the same keeper — so it is sized against the noise,
// not against correctness.
const keeperSettleGrace = time.Minute

// keeperPass is how often a running keeper settles what is left unsettled: a
// batch whose claim failed with the store unreachable, or whose settle failed
// after its keeper was decided. A minute, so a copy that is not this node's
// to keep is gone about as soon as the store answers again.
const keeperPass = time.Minute

// keeperPassBatches is how many unsettled batches one read of the store lists:
// the pass reads the next page after the last batch of this one, so a pass
// still reaches every batch, and a page bounds what one read holds. The table
// holds only what was written in the last few moments, so a second page is
// read only after the coordination store was away for a while.
const keeperPassBatches = 256

// keeperStopWait is how long [Keeper.Stop] waits for a batch in hand to settle
// before it lets go of the group.
//
// ONE SECOND, an order of magnitude past a healthy delivery — one store
// transaction and one create-only claim, milliseconds each — and what it
// orders is narrow: a SETTLE landing before the usage publisher's last flush,
// since a settle as kept is what makes a batch's rows count. A delivery still
// in hand after it is waiting on a coordination store that is not answering.
// The detach that follows cancels its context, so its claim fails in this
// process: the batch goes back to the group, and a copy this node already
// wrote stays unsettled, counted in no day, until this node's next start
// reconciles it — which is also where a settle as kept marks its days.
// Waiting out the claim's own request timeout instead would add
// those seconds to every stop of a member that has lost its stores, for no
// row a count would see.
const keeperStopWait = time.Second

// NewKeeper builds this data node's keeper.
func NewKeeper(log CustodyLog, claims CustodyClaims, node string) (*Keeper, error) {
	switch {
	case log == nil:
		return nil, errors.New("observe: a custody keeper needs this node's event log")
	case claims == nil:
		return nil, errors.New("observe: a custody keeper needs the fleet's custody claims")
	case node == "":
		return nil, errors.New("observe: a custody keeper needs this node's id")
	}
	return &Keeper{log: log, claims: claims, node: node, now: time.Now}, nil
}

// Start settles what an earlier run of this node left unsettled, attaches the
// keeper to the group, and runs the settling pass until [Keeper.Stop].
//
// SETTLED BEFORE IT ATTACHES, so the copies the last run left — a batch in
// hand when it stopped, one whose claim a crash cut short or the coordination
// store did not answer — are resolved before this node takes anything new.
// Nothing writes this node's store while it is stopped, so how long it was
// away does not change how many there are, only how many of them are somebody
// else's: the group redelivered each batch this node had not acknowledged to
// the data nodes still running, and whichever claimed it first keeps it.
func (k *Keeper) Start(ctx context.Context, sub Attachment) error {
	if _, err := k.Reconcile(ctx); err != nil {
		// NOT FATAL: the store will answer later, and the pass retries.
		log.WarnContext(ctx, "event_custody_unsettled", "error", err.Error(),
			"detail", "the custody batches this node wrote before it stopped "+
				"could not be settled yet; the next pass tries again")
	}
	// DETACHED from the caller's context: the attachment is the process's
	// rather than the call's, and one bound to a start-up context stops
	// taking batches the moment that context ends.
	if err := sub.Subscribe(context.WithoutCancel(ctx), topics.CustodyRecords, CustodyGroup,
		k.take); err != nil {
		return fmt.Errorf("observe: attach the custody keeper: %w", err)
	}
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	k.queue, k.stop, k.done = sub, stop, make(chan struct{})
	go k.run(loop)
	return nil
}

// Stop LEAVES THE GROUP and ends the settling pass: it stops taking batches,
// waits up to [keeperStopWait] for the one in hand to settle, detaches, and
// then ends the pass.
//
// THE GROUP IS LEFT HERE rather than with the queue, because this node's
// usage publisher runs its last flush between the two: every batch this node
// settles as kept makes rows count on its day, so a keeper still taking
// batches when that flush runs settles rows into a day the node — if it never
// starts again — never publishes. A batch it did not take goes to the group's
// other data nodes, or waits on the stream for the next start; one it had
// taken and not settled is handed back to them by the detach. ctx bounds the
// leave, the wait included.
func (k *Keeper) Stop(ctx context.Context) {
	if k == nil || k.stop == nil {
		return
	}
	k.mu.Lock()
	k.stopping = true
	k.mu.Unlock()
	if _, err := k.queue.Quiesce(ctx, topics.CustodyRecords, CustodyGroup); err != nil {
		log.WarnContext(ctx, "event_custody_not_quiesced", "error", err.Error(),
			"detail", "the keeper is leaving the group without having stopped "+
				"its deliveries first; a batch delivered meanwhile is handed back")
	}
	wait, cancel := context.WithTimeout(ctx, keeperStopWait)
	k.settled(wait)
	cancel()
	if _, err := k.queue.Detach(ctx, topics.CustodyRecords, CustodyGroup); err != nil {
		log.WarnContext(ctx, "event_custody_not_detached", "error", err.Error(),
			"detail", "the keeper's attachment ends with the queue instead")
	}
	k.stop()
	<-k.done
}

// take is the group's handler: [Keeper.Handle] for a batch delivered while the
// keeper is attached, counted while it is in hand, and handed back untouched
// once the keeper is leaving.
func (k *Keeper) take(ctx context.Context, ev *events.Event) queue.Result {
	k.mu.Lock()
	if k.stopping {
		k.mu.Unlock()
		return queue.Defer("the custody keeper is leaving the group")
	}
	k.inHand++
	k.mu.Unlock()
	defer func() {
		k.mu.Lock()
		k.inHand--
		if k.inHand == 0 && k.idle != nil {
			close(k.idle)
			k.idle = nil
		}
		k.mu.Unlock()
	}()
	return k.Handle(ctx, ev)
}

// settled waits until no batch is in hand, or ctx ends.
func (k *Keeper) settled(ctx context.Context) {
	k.mu.Lock()
	if k.inHand == 0 {
		k.mu.Unlock()
		return
	}
	if k.idle == nil {
		k.idle = make(chan struct{})
	}
	idle := k.idle
	k.mu.Unlock()
	select {
	case <-idle:
	case <-ctx.Done():
		log.WarnContext(ctx, "event_custody_left_in_hand",
			"detail", "a custody batch was still being settled when the keeper "+
				"left the group; the detach cancelled it and handed it back to "+
				"the group, and a copy this node already wrote stays unsettled "+
				"until this node's next start settles it")
	}
}

func (k *Keeper) run(ctx context.Context) {
	defer close(k.done)
	ticker := time.NewTicker(keeperPass)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if _, err := k.Reconcile(ctx); err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "event_custody_unsettled", "error", err.Error(),
				"detail", "the pass stopped where the coordination store or this "+
					"node's own store could not answer; the custody batches it had "+
					"not reached keep their rows until a later pass learns which "+
					"node keeps each")
		}
	}
}

// Handle writes one custody batch into this node's log and settles it.
func (k *Keeper) Handle(ctx context.Context, ev *events.Event) queue.Result {
	batch, ok := events.DataAs[*types.CustodyBatch](ev)
	if !ok {
		// A CARRIER NOBODY CAN READ is acknowledged and said, never
		// redelivered: no build that decodes it this way will decode it
		// another, and a redelivery would only spend the group's budget
		// on it before the dead-letter queue took it.
		log.WarnContext(ctx, "event_custody_unreadable", "batch", ev.ID.String(),
			"origin", ev.Node, "type", ev.Type,
			"detail", "a custody batch whose events could not be decoded; they "+
				"are in no event log")
		return queue.Ack()
	}
	records := make([]store.EventRecord, 0, len(batch.Events))
	for _, inner := range batch.Events {
		// THE ROW THIS NODE'S OWN LISTENER WOULD WRITE, by the same
		// function, so a stateless node's event and a data node's are
		// one shape — and a type this build does not place is left out
		// here as it would be on the node that published it.
		if rec, ok := Record(inner); ok {
			records = append(records, rec)
		}
	}
	id := ev.ID.String()
	if err := k.log.WriteCustody(ctx, store.CustodyBatch{
		ID: id, Origin: ev.Node, Records: records,
	}, k.now()); err != nil {
		return queue.Nak(fmt.Errorf("observe: write custody batch %s: %w", id, err))
	}
	if err := k.settle(ctx, id); err != nil {
		if errors.Is(err, coord.ErrCustodyUndecidable) {
			// ACKNOWLEDGED, never redelivered: whichever node took it
			// next would be told the same — the record is the fleet's —
			// and would only write one more copy nobody can settle. Its
			// copy here stays, and every pass names it.
			undecided(ctx, store.UnsettledBatch{ID: id, Origin: ev.Node}, err)
			return queue.Ack()
		}
		// UNSETTLED, and said by the pass that settles it; the delivery
		// goes back, and whichever node takes it next is told the same
		// keeper this one will be.
		return queue.Nak(err)
	}
	return queue.Ack()
}

// settle claims a batch this node wrote and keeps or deletes its copy on the
// answer.
func (k *Keeper) settle(ctx context.Context, id string) error {
	keeper, err := k.claims.ClaimCustody(ctx, id, k.node)
	if err != nil {
		return fmt.Errorf("observe: claim custody batch %s: %w", id, err)
	}
	if err := k.log.SettleCustody(ctx, id, keeper == k.node); err != nil {
		return fmt.Errorf("observe: settle custody batch %s (kept by %s): %w", id, keeper, err)
	}
	if keeper != k.node {
		log.DebugContext(ctx, "event_custody_released", "batch", id, "keeper", keeper)
	}
	return nil
}

// KeeperPass is what one [Keeper.Reconcile] did.
type KeeperPass struct {
	// Settled is how many batches it settled, kept, deleted or let go.
	Settled int
	// Undecidable are the batches it left unsettled because the fleet's
	// record of their keeper cannot be read ([coord.ErrCustodyUndecidable]),
	// oldest first: each keeps its rows, counted in no day, and is asked
	// about again by the next pass.
	Undecidable []store.UnsettledBatch
}

// Reconcile settles every batch this node wrote at least [keeperSettleGrace]
// before it began and has not settled, reporting what it settled and what it
// could not.
//
// A BATCH OLDER THAN THE EVENT LOG KEEPS ROWS is let go without asking: its
// rows are past the log's retention and swept with every other row of their
// age, and its keeper's record may have aged out with them
// ([coord.CustodyRetention]), so there is nothing left to decide.
//
// A BATCH WHOSE KEEPER CANNOT BE DECIDED IS STEPPED OVER, never stopped at:
// the fleet holds a record of its keeper that nothing can read, which says
// nothing about any other batch and will say the same on every pass. Stopped
// at, it held every batch written after it — the listing is oldest first —
// unsettled on every pass and every boot until it aged past the log's
// retention: a batch this node keeps counted in none of its days for a month,
// its days by then older than the floor its usage publisher derives again,
// and a copy another node keeps left here beside the keeper's for as long. So
// it is named ([undecided]), left as it is, and the pass reads on after it.
//
// It STOPS at an answer that is unknown — the coordination store unreachable,
// a claim this build cannot classify, this node's own store failing, ctx
// ending — because that is no fact about one batch: the next would meet the
// same, each after its own wait. What it settled and stepped over by then is
// reported beside the error.
func (k *Keeper) Reconcile(ctx context.Context) (KeeperPass, error) {
	var pass KeeperPass
	// ONE INSTANT FOR THE PASS: the listing reads on after the last batch it
	// gave, and a bound fixed at the start is one no batch written during
	// the pass can fall under, so the pass ends.
	now := k.now()
	after := store.UnsettledBatch{}
	for {
		batches, err := k.log.UnsettledCustody(ctx, now.Add(-keeperSettleGrace), after, keeperPassBatches)
		if err != nil {
			return pass, err
		}
		for _, b := range batches {
			if ended := ctx.Err(); ended != nil {
				return pass, ended
			}
			if now.Sub(b.WrittenAt) > store.EventRetention {
				err = k.log.SettleCustody(ctx, b.ID, false)
			} else {
				err = k.settle(ctx, b.ID)
			}
			switch {
			case errors.Is(err, coord.ErrCustodyUndecidable):
				undecided(ctx, b, err)
				pass.Undecidable = append(pass.Undecidable, b)
			case err != nil:
				return pass, err
			default:
				pass.Settled++
			}
		}
		if len(batches) < keeperPassBatches {
			return pass, nil
		}
		after = batches[len(batches)-1]
	}
}

// undecided says that a batch this node wrote cannot be settled, because the
// fleet's record of its keeper cannot be read: once when its delivery meets
// it, and on every pass after for as long as the record stands — the pass
// naming when the batch was first written, which is when the event log's
// retention lets it go.
//
// LOUD, and on every pass: it is a value no claim of this engine writes, and
// what clears it is somebody removing it — the next claim then creates the
// record afresh, and the copies settle — or the month the event log keeps the
// batch's rows, until which a batch this node may keep is counted in none of
// its days.
//
// The detail says WHERE the removal is possible, because on the default
// topology it is not: the record is in the coordination store's custody
// bucket, which the error names with its key, and only an external NATS
// cluster is one an operator's own client is meant to reach — an embedded
// broker offers no way in from outside the engine. And it says what to know
// first, since a removal over a batch some node already keeps as its own
// hands it a second keeper.
func undecided(ctx context.Context, b store.UnsettledBatch, err error) {
	attrs := []any{"batch", b.ID, "origin", b.Origin}
	if !b.WrittenAt.IsZero() {
		attrs = append(attrs, "written_at", b.WrittenAt)
	}
	log.WarnContext(ctx, "event_custody_undecidable", append(attrs, "error", err.Error(),
		"detail", "the fleet's record of which data node keeps this custody batch "+
			"cannot be read, so this node's copy is neither kept nor deleted: its rows "+
			"stay, counted in no day, until the batch is past the event log's "+
			"retention and goes with them, or until the record — under the key and in "+
			"the bucket the error names — is removed and the next claim creates it "+
			"again. Only an external NATS cluster offers a way to remove it "+
			"(nats kv del <bucket> <key>), and only once no data node kept the batch "+
			"before its record became unreadable: such a node logs nothing, and the "+
			"claim after the removal would keep a second copy")...)
}
