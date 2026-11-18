package jetstream

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/jsapi"
)

// A DOMAIN CONSUMER'S STANDING PULL, made by hand: each pull bounded by both a
// count and a byte total, answered on one inbox for the handle's life (package
// doc, "A pull allocates for what it receives, never for what it may").
//
// Not the library's Consume / Messages: its prefetch keeps requests standing
// ahead of the reader, holding records in flight whether or not an applier
// runs, and needs a Stop nothing on the applier's side calls. Here only a fetch
// sends a request, one at a time, and the buffer is the handle's, so the next
// runner over it takes what the last left. A handle given up for good is
// released ([puller.release]); a failed pull hands its buffer back with the
// failure ([puller.fetch]); a status ends the request whose own reply subject
// (`<inbox>.<n>`) it arrived on, never the oldest ([puller.deliver]).

// pullGrace is how long past its own expiry a request is still counted as one
// the server may be serving.
//
// The server's `408` is timed from when IT received the request, so the
// client's view runs late by a round trip; one second covers a leaf link with
// room to spare. A server later than this costs a second request, never a
// record: what it still delivers lands in the buffer like any other.
const pullGrace = time.Second

// puller is one consumer's standing pull.
type puller struct {
	nc      *nats.Conn
	log     *slog.Logger
	subject string // the consumer's MSG.NEXT subject, in the connection's API
	// inbox is the standing inbox's prefix: [puller.sub] holds every subject
	// beneath it, and each request replies on one of its own.
	inbox string
	sub   *nats.Subscription

	// fetching serialises fetches: the buffer's order is the log's, and two
	// readers would each take part of it.
	fetching sync.Mutex

	// wake is signalled on every delivery and status, capacity one, so a
	// fetch is never behind by more than one wake and the callback never
	// blocks.
	wake chan struct{}

	mu sync.Mutex
	// buffered is what has been delivered and no fetch has taken, in
	// delivery order.
	buffered []*nats.Msg
	// settled is the newest delivery having reported nothing pending
	// behind it: the burst it belonged to is complete.
	settled bool
	// behind is what the newest delivery's own metadata counted the
	// consumer still holding past it, and counted whether that delivery's
	// metadata could be read — see [puller.undelivered].
	behind  uint64
	counted bool
	// requests are the requests the server may still be serving, oldest
	// first — the order the server fills them in.
	requests []*standingRequest
	// sent numbers the requests, naming each one's reply subject.
	sent uint64
	// failure is a status that ended a request as a failure, for the next
	// fetch to report — handing back what the buffer held with it.
	failure error
	// released is the handle having been given up ([puller.release]): no
	// fetch will run again, and a record the inbox still hands over is
	// handed back rather than buffered.
	released bool
}

// standingRequest is one request as the client counts it: what the server
// still owes it, and when the client stops expecting its end.
type standingRequest struct {
	// reply is the subject the server answers this request on, and so the
	// subject of the status that ends it. Empty for none the server holds.
	reply string
	batch int
	// bytesLeft is what is left of a byte bound, and bounded whether the
	// request carried one.
	bytesLeft int
	bounded   bool
	until     time.Time
}

// newPuller subscribes the standing inbox of one consumer.
func (q *Queue) newPuller(stream, consumer string) (*puller, error) {
	subject, err := q.API().Subject(fmt.Sprintf(server.JSApiRequestNextT, stream, consumer))
	if err != nil {
		return nil, fmt.Errorf("jetstream: address the pulls on %s: %w", consumer, err)
	}
	p := &puller{nc: q.nc, log: q.log, subject: subject, inbox: q.nc.NewInbox(),
		wake: make(chan struct{}, 1)}
	// A CALLBACK SUBSCRIPTION: a channel one drops what finds it full, leaving
	// a record in flight until the ack window, so it would have to be sized by
	// the largest batch, the allocation this file removes. ONE WILDCARD, so
	// every reply subject is covered before its request is sent: a per-request
	// subscription that has not yet reached the server (and across a leaf link,
	// the hub) leaves no interest in the reply, and the request is dropped.
	if p.sub, err = q.nc.Subscribe(p.inbox+".*", p.deliver); err != nil {
		return nil, fmt.Errorf("jetstream: subscribe the pulls on %s: %w", consumer, err)
	}
	return p, nil
}

// close ends the standing inbox. What it had buffered is dropped with it,
// which is right only where the consumer those records were delivered for is
// going too — see [DomainConsumer.Reset]. A handle whose consumer stays is
// [puller.release]d instead.
func (p *puller) close() { _ = p.sub.Unsubscribe() }

// release gives the pull up for good, for a consumer that stays (an engine
// stopping while the process and a new engine's handle go on). The server
// serves a consumer's requests oldest first, so this handle's buffer and
// standing request would otherwise take the next records, stranded until the
// ack window. What the buffer holds is handed back, and the inbox is DRAINED
// rather than dropped: what had already reached this client still passes to
// [puller.deliver], which hands it back too, and a request still standing has a
// reply nobody is interested in, so the server skips to the consumer's next.
// Called once no fetch will run again.
func (p *puller) release() {
	p.mu.Lock()
	p.released = true
	held := p.buffered
	p.buffered = nil
	p.mu.Unlock()
	p.handBack(held)
	if err := p.sub.Drain(); err != nil {
		p.log.Warn("jetstream_pull_release_failed", "subject", p.subject,
			"error", err.Error(),
			"detail", "the released state-log pull's inbox could not be drained; "+
				"it stays subscribed and hands back what it is still delivered")
	}
}

// handBack negatively acknowledges records no reader will take, which the
// broker redelivers at once to whichever request of the consumer is waiting.
//
// A failed hand-back leaves the record in flight until the ack window, thirty
// seconds of a strict log standing still, so it is logged: ONCE FOR THE CALL,
// naming how many and the first, since a failed pull hands back everything
// over the connection that just failed.
func (p *puller) handBack(msgs []*nats.Msg) {
	failed, seq := 0, uint64(0)
	var first error
	for _, msg := range msgs {
		err := msg.Nak()
		if err == nil {
			continue
		}
		if failed++; first == nil {
			first = err
			if meta, unreadable := msg.Metadata(); unreadable == nil {
				seq = meta.Sequence.Stream
			}
		}
	}
	if first != nil {
		p.log.Warn("jetstream_pull_handback_failed", "subject", p.subject,
			"records", failed, "seq", seq, "error", first.Error(),
			"detail", "records delivered to a state-log pull nobody will read "+
				"them from could not be handed back, so the broker redelivers "+
				"them only once their acknowledgement window passes; seq is the "+
				"first of them")
	}
}

// request sends one pull for up to batch records and maxBytes of them (zero is
// no byte bound), standing for wait; a wait of zero or less asks for what is
// there now and waits for nothing. Held under p.mu.
func (p *puller) request(batch, maxBytes int, wait time.Duration) error {
	request := server.JSApiConsumerGetNextRequest{Batch: batch, MaxBytes: maxBytes}
	if wait > 0 {
		request.Expires = wait
	} else {
		request.NoWait = true
	}
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("jetstream: encode a pull: %w", err)
	}
	p.sent++
	reply := p.inbox + "." + strconv.FormatUint(p.sent, 10)
	if err := p.nc.PublishRequest(p.subject, reply, body); err != nil {
		return fmt.Errorf("jetstream: send a pull: %w", err)
	}
	p.requests = append(p.requests, &standingRequest{
		reply: reply, batch: batch, bytesLeft: maxBytes, bounded: maxBytes > 0,
		until: time.Now().Add(max(wait, 0) + pullGrace),
	})
	return nil
}

// deliver is the inbox's callback: one delivery, or one status about the
// request whose reply subject it arrived on.
func (p *puller) deliver(msg *nats.Msg) {
	status, failure := jsapi.PullStatus(msg)
	p.mu.Lock()
	if p.released {
		p.mu.Unlock()
		if status == "" {
			p.handBack([]*nats.Msg{msg})
		}
		return
	}
	switch {
	case status == "":
		p.buffered = append(p.buffered, msg)
		// THE BURST IS COMPLETE when the consumer had nothing left to
		// deliver behind this record. A delivery whose metadata cannot
		// be read says nothing either way, and is treated as complete
		// so the fetch hands over what it has rather than waiting on
		// an answer that will not come.
		meta, unreadable := msg.Metadata()
		p.settled = unreadable != nil || meta.NumPending == 0
		p.behind, p.counted = 0, unreadable == nil
		if p.counted {
			p.behind = meta.NumPending
		}
		if len(p.requests) > 0 {
			// A record names no request, and the server fills the
			// requests it holds in turn, so it is counted against
			// the oldest. One that fills — its batch met, or a byte
			// bound met exactly — the server has ended: with no
			// word, or, for a batch met under a byte bound with
			// bytes to spare, with a `409 Batch Completed` naming a
			// request no longer counted here.
			r := p.requests[0]
			r.batch--
			r.bytesLeft -= msg.Size()
			if r.batch <= 0 || (r.bounded && r.bytesLeft <= 0) {
				p.requests = p.requests[1:]
			}
		}
	case status == jsapi.StatusHeartbeat:
		// Not asked for, and no answer about any request.
		p.mu.Unlock()
		return
	default:
		// THE REQUEST THE STATUS NAMES, wherever it stands, and none if it
		// names one no longer counted. Matching the oldest was wrong: a
		// request with a byte bound whose batch fills is followed by a
		// `409 Batch Completed` after its last record was counted, which then
		// ended the next fetch's request, and the server held two
		// (TestAPullHonoursBothOfItsBounds).
		for i, r := range p.requests {
			if r.reply == msg.Subject {
				p.requests = append(p.requests[:i:i], p.requests[i+1:]...)
				break
			}
		}
		if failure != nil {
			p.failure = failure
		}
	}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// fetch returns the next burst of records — up to maxMessages of them and
// maxBytes (zero: no byte bound) — waiting up to wait for the first.
//
// It sends a request only when none the server may still be serving is
// standing, so what stands is at most one fetch's worth; a request that
// outlives this call goes on delivering into the buffer, and the next fetch
// takes what it delivered.
//
// A CONTEXT THAT ENDS leaves everything delivered in the buffer: the records
// are in flight against the consumer whoever holds them, and the next fetch
// on this handle — the next runner's — takes them at once, where returned
// beside the error they would be dropped by a caller that is stopping.
func (p *puller) fetch(ctx context.Context, maxMessages, maxBytes int,
	wait time.Duration) ([]*nats.Msg, error) {

	p.fetching.Lock()
	defer p.fetching.Unlock()
	start := time.Now()
	deadline := start.Add(max(wait, 0))
	if wait <= 0 {
		// A no-wait request's only end is its 404, so the grace alone
		// bounds it.
		deadline = start.Add(pullGrace)
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	asked := false
	for {
		now := time.Now()
		p.mu.Lock()
		if failure := p.failure; failure != nil {
			// WHAT THE PULL HELD GOES BACK WITH THE FAILURE, never to the
			// next fetch: the applier reads a failure as the point after which
			// the broker may be another history (a restore under a running
			// node), so a record kept from before it would arrive as though
			// delivered after. Handed back, the broker redelivers it as it is.
			held := p.buffered
			p.buffered, p.failure = nil, nil
			p.mu.Unlock()
			p.handBack(held)
			return nil, failure
		}
		for len(p.requests) > 0 && now.After(p.requests[0].until) {
			// Presumed gone — see [pullGrace]. What it still
			// delivers lands in the buffer all the same.
			p.requests = p.requests[1:]
		}
		standing := len(p.requests) > 0
		if len(p.buffered) > 0 &&
			(p.settled || !standing || !now.Before(deadline) || p.full(maxMessages, maxBytes)) {
			taken := p.take(maxMessages, maxBytes)
			p.mu.Unlock()
			return taken, nil
		}
		if !standing {
			if asked && (wait <= 0 || !now.Before(deadline)) {
				// The no-wait request ended, or the wait is spent,
				// and nothing came.
				p.mu.Unlock()
				return nil, nil
			}
			ask := deadline.Sub(now)
			if wait <= 0 {
				ask = 0
			}
			if err := p.request(maxMessages, maxBytes, ask); err != nil {
				p.mu.Unlock()
				return nil, err
			}
			asked = true
		}
		wakeAt := p.wakeAt(now, deadline)
		p.mu.Unlock()
		timer.Reset(time.Until(wakeAt))
		select {
		case <-p.wake:
		case <-timer.C:
			// The deadline, or the oldest request presumed gone: the
			// loop hands over what came, sends the request that
			// replaces it, or finds that nothing did.
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// wakeAt is when a fetch waiting at now has to look again without being woken:
// its deadline, or the instant the oldest standing request is presumed gone,
// whichever is first, and past the deadline always the latter. Held under
// p.mu, with a request standing.
//
// THE PRESUMPTION IS WHAT BOUNDS THE WAIT, because a request's ending status
// may never arrive: a dropped connection takes the server's copy with it, and
// a slow consumer's inbox drops statuses. Checked only on a delivery, a fetch
// past its deadline would wait on a request nothing serves, and one before it
// could not send its own while an earlier fetch's request stood.
func (p *puller) wakeAt(now, deadline time.Time) time.Time {
	until := p.requests[0].until
	if !now.Before(deadline) || until.Before(deadline) {
		return until
	}
	return deadline
}

// undelivered is how many records of the consumer no fetch has taken yet: what
// the pull holds, plus what the newest delivery's metadata counted behind it
// (the consumer info's count, as of that delivery). False until a delivery's
// metadata could be read.
//
// IT ASKS THE BROKER NOTHING, unlike a consumer info: the applier asks on
// every run that holds records, and an info is answered only by the consumer's
// leader, behind its API queue, on the commit path. A record appended after
// the newest delivery is counted once a standing request delivers it.
func (p *puller) undelivered() (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.counted {
		return 0, false
	}
	return p.behind + uint64(len(p.buffered)), true
}

// full reports whether the buffer already holds a whole fetch. Held under p.mu.
func (p *puller) full(maxMessages, maxBytes int) bool {
	if len(p.buffered) >= maxMessages {
		return true
	}
	if maxBytes <= 0 {
		return false
	}
	total := 0
	for _, msg := range p.buffered {
		total += msg.Size()
	}
	return total >= maxBytes
}

// take removes and returns the buffer's head: up to maxMessages records and
// maxBytes of them, and always at least one — a record larger than the bound
// is one the server would not have sent under it, so it came under another
// request's and is handed over rather than stranded. Held under p.mu.
func (p *puller) take(maxMessages, maxBytes int) []*nats.Msg {
	n, total := 0, 0
	for n < len(p.buffered) && n < maxMessages {
		size := p.buffered[n].Size()
		if n > 0 && maxBytes > 0 && total+size > maxBytes {
			break
		}
		total += size
		n++
	}
	taken := make([]*nats.Msg, n)
	copy(taken, p.buffered[:n])
	if n == len(p.buffered) {
		p.buffered = nil
	} else {
		p.buffered = append([]*nats.Msg(nil), p.buffered[n:]...)
	}
	return taken
}
