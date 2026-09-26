package observe

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/store"
)

// Shipper hands a batch of records to a node that keeps them, answering how
// many it took.
type Shipper interface {
	AppendEvents(ctx context.Context, records []store.EventRecord) (int, error)
}

// Custody persists what this node publishes on ANOTHER node's event log.
//
// # Why a node would not keep its own
//
// The [Writer] rule is that the publisher writes its own rows, inline, into
// its own database — and a node without the `data` role deletes its database
// at every boot. Written there, its audit rows would be gone at its next
// restart, and nothing could have read them in between: the API runs only
// where the data is. So such a node hands each record to a data node, whose
// log holds it beside its own.
//
// # It never holds a publish, and says what it drops
//
// A listener runs in the publishing goroutine, and a turn must never wait on
// its own audit trail — so the record goes into a BOUNDED buffer and one
// goroutine ships batches of it. A data node that cannot be reached is
// retried with a backoff while the buffer holds; a buffer that fills drops
// the NEWEST record and counts it, and the count is logged as the buffer
// drains, so a gap in the audit log is a line in this node's log rather than
// a silence. The same (time, id) pair twice is nothing — the receiving log's
// append is idempotent — so a batch whose answer was lost is sent again.
type Custody struct {
	ship Shipper

	// buffered, maxBatch and maxBatchBytes are [custodyBuffer],
	// [custodyBatch] and [custodyBatchBytes]; flushEvery and retryBase
	// the pacing. Fields so a test can run them in milliseconds.
	buffered      int
	maxBatch      int
	maxBatchBytes int
	flushEvery    time.Duration
	retryBase     time.Duration

	mu      sync.Mutex
	pending []store.EventRecord
	dropped int
	wake    chan struct{}
	stop    context.CancelFunc
	done    chan struct{}
}

// custodyBuffer is how many records wait for a data node.
//
// FOUR THOUSAND AND NINETY-SIX: a turn persists a handful of records per
// phase and a node runs at most `node.max_concurrent` turns, so this is
// minutes of a busy node's audit trail — long enough to ride out a data node
// restarting — while its worst case, every record carrying a large phase
// payload, stays in the tens of megabytes a stateless node is sized for.
const custodyBuffer = 4096

// custodyBatch and custodyBatchBytes bound one shipment, whichever is reached
// first: a batch well under the broker's message limit
// ([queue.MaxPayloadBytes]) with room for the envelope, so one oversized
// phase record cannot make a whole batch unsendable.
const (
	custodyBatch      = 256
	custodyBatchBytes = 1 << 20
)

// custodyFlush is how long a record waits for companions before it ships.
//
// ONE SECOND: the audit trail is read by a person after the fact, so a second
// of latency is invisible, and it turns the dozens of records one turn
// publishes in a burst into one request rather than dozens.
const custodyFlush = time.Second

// custodyRetryBase is the first wait after a shipment failed, doubling to the
// flush interval's sixty-fold — a minute — for a data node that stays away.
const custodyRetryBase = 500 * time.Millisecond

// NewCustody builds a custody forwarder over a shipper, stopped.
func NewCustody(ship Shipper) *Custody {
	if ship == nil {
		return nil
	}
	return &Custody{
		ship: ship, buffered: custodyBuffer, maxBatch: custodyBatch,
		maxBatchBytes: custodyBatchBytes, flushEvery: custodyFlush,
		retryBase: custodyRetryBase, wake: make(chan struct{}, 1),
	}
}

// Listen returns the publish listener to register, or nil.
func (c *Custody) Listen() queue.PublishListener {
	if c == nil {
		return nil
	}
	return c.onPublish
}

// onPublish buffers one record. Never blocks, never fails the publish.
func (c *Custody) onPublish(_ context.Context, _ string, ev *events.Event) {
	rec, ok := Record(ev)
	if !ok {
		return
	}
	c.mu.Lock()
	if len(c.pending) >= c.buffered {
		c.dropped++
		c.mu.Unlock()
		return
	}
	c.pending = append(c.pending, rec)
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

// Start runs the shipping loop, detached from ctx like every loop a node
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

// Stop ends the loop after one last attempt to ship what is buffered,
// bounded by ctx. What cannot be shipped by then is lost with the node, and
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
		if len(batch) == 0 {
			return
		}
		if err := c.send(flush, batch); err != nil {
			c.mu.Lock()
			left := len(c.pending)
			c.mu.Unlock()
			log.WarnContext(ctx, "event_custody_lost", "records", len(batch)+left,
				"error", err.Error(),
				"detail", "this node holds no store, and no data node took these "+
					"records before it stopped: they are not in any event log")
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
			if len(batch) == 0 {
				break
			}
			if err := c.send(ctx, batch); err != nil {
				failures++
				c.requeue(batch)
				wait := backoff.Jitter(backoff.Doubling(failures, c.retryBase, 60*c.flushEvery), 0.2)
				if failures == 1 || failures%10 == 0 {
					log.WarnContext(ctx, "event_custody_retrying", "records", len(batch),
						"attempt", failures, "retry_in", wait.String(), "error", err.Error())
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

// next takes the next batch off the buffer, bounded by count and bytes.
func (c *Custody) next() []store.EventRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, size := 0, 0
	for n < len(c.pending) && n < c.maxBatch {
		size += len(c.pending[n].Payload) + recordOverhead
		if n > 0 && size > c.maxBatchBytes {
			break
		}
		n++
	}
	batch := c.pending[:n:n]
	c.pending = c.pending[n:]
	return batch
}

// recordOverhead is a record's size beyond its payload, as a batch bound
// counts it — the identity, the tags and the envelope, rounded up.
const recordOverhead = 1024

// requeue puts a batch that did not ship back at the front, within the
// buffer's bound: what does not fit is counted as dropped.
func (c *Custody) requeue(batch []store.EventRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.buffered - len(c.pending)
	if room < len(batch) {
		c.dropped += len(batch) - max(room, 0)
		batch = batch[:max(room, 0)]
	}
	c.pending = append(slices.Clone(batch), c.pending...)
}

// send ships one batch and reports what was dropped since the last report.
func (c *Custody) send(ctx context.Context, batch []store.EventRecord) error {
	taken, err := c.ship.AppendEvents(ctx, batch)
	if err != nil && taken > 0 && taken < len(batch) {
		// PART OF IT LANDED: put back only the rest, so a retry does not
		// ship what the log already holds (harmless, but not free).
		c.requeue(batch[taken:])
		return nil
	}
	if err != nil {
		return err
	}
	c.mu.Lock()
	dropped := c.dropped
	c.dropped = 0
	c.mu.Unlock()
	if dropped > 0 {
		log.WarnContext(ctx, "event_custody_dropped", "records", dropped,
			"detail", "the custody buffer was full while no data node was taking "+
				"records, so these never reached any event log")
	}
	return nil
}
