package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// A DOMAIN CONSUMER'S STANDING PULL, made by hand, because the client library
// makes neither half of it in the shape an applier needs.
//
// # What the library offers, and what each one costs
//
// A domain applier needs every pull bounded by BOTH a count and a byte total —
// a count because one pull is one transaction's worth of records, bytes
// because a record may be a barrier's hundred bytes or a bulk write's
// megabytes. The server's own request carries both (`batch` and `max_bytes`
// on `CONSUMER.MSG.NEXT`) and enforces both. The library does not let a
// caller send that request as a fetch:
//
//   - FetchBytes sends `max_bytes` with the batch FIXED at a million, and
//     sizes the two channels it delivers through by that million — 32 MiB
//     allocated on every call, measured, whatever the log holds. An idle
//     applier pulls twice a second per domain, so three idle consumers
//     allocated 181 MiB/s, and a process running a few hundred of them was
//     OOM-killed at 12.7 GB inside a minute.
//   - Fetch sends a count and no byte bound at all, and sizes the same two
//     channels by the count: ~172 KiB per idle pull at the applier's four
//     thousand, which is the right order of defect rather than a fix.
//
// And both are a POLL: an inbox subscribed for one request and dropped when
// the client decides the request is over. The server decides that too, on its
// own clock, and under load the two disagree — a request the client has given
// up on is still being served, and a record the server delivers to it lands
// on an inbox nobody holds. That record is in flight against the consumer
// until the ACK WINDOW redelivers it, thirty seconds later. Measured on a
// three-member cluster under CPU contention: apply latency up to 30.8–34 s,
// and 57,000-record drains taking 28–99 s instead of about one. A poll also
// holds whatever reached it until its request ENDS — the whole wait on a quiet
// log — so a record appended a millisecond into a half-second pull was applied
// half a second later: 500 ms and more at the 99th percentile across 129
// streams.
//
// # So the pull STANDS
//
// One inbox for the life of the handle, which every request of the consumer is
// answered on, and a buffer of what it delivered that no fetch has taken. A
// delivery can then reach nothing but the buffer — whichever request it
// answers, and however long after the client stopped counting that request —
// so nothing waits out an ack window for a reader that was there all along,
// and the next fetch hands it over at once. A fetch returns as soon as a burst
// of records is in, which is what [statelog.Fetcher] asks ("waiting up to
// wait for the first one"): the newest delivery's own count of what is still
// pending behind it says when a burst is complete, and the runner decides
// whether to fetch again. Measured on the same cluster, a standing pull with a
// message cap and a byte cap applied at a p99 of 3–13 ms, and an idle one
// costs 9–13 KiB/s. Measured here, against one embedded member: a record
// appended into a standing fetch reaches its caller in 57 µs at the median and
// 318 µs at the 99th percentile, where the fetch that held it for its wait
// took the whole half-second; an idle fetch allocates about 5 KiB.
//
// The library's own standing pull (Consume / Messages) is not used, for the
// reason the poll's allocation was not accepted: it keeps requests standing
// ahead of the reader by itself, so what it has delivered and nobody has
// asked for is in flight whether or not an applier is running, and it needs a
// Stop that nothing on the applier's side calls — a runner stopped and
// another started over the same handle would find the first one's prefetch
// holding the consumer's whole in-flight ceiling. Here a request is sent only
// by a fetch, one at a time, so what stands is at most one fetch's worth, and
// the buffer that holds what it delivered is the handle's — the next runner
// over it takes what the last one left.
//
// # And a handle nobody will read again GIVES IT BACK
//
// "The next runner over it" is true only while the handle has one. A reader
// that is done with the consumer for good — an engine stopping, while the
// process and its connection go on and a new engine opens its own handle on
// the same consumer — leaves the handle holding whatever the buffer has and a
// request the server is still serving, and the server serves requests oldest
// first: a record appended next was delivered to the abandoned handle rather
// than to the new one's request queued behind it, and sat in a buffer nobody
// would read until the ack window redelivered it. Measured in the engine's own
// case: a read barrier appended 42 ms into the second engine went to the first
// one's inbox and was redelivered ten seconds later, so the node stayed
// unhydrated past its ten-second bound. So a handle is RELEASED
// ([DomainConsumer.Close]): what it holds is negatively acknowledged, which
// the broker redelivers at once, and its inbox is drained, so a request it
// left standing has a reply nobody is interested in and the server serves the
// next request of the consumer instead — measured on a member that serves
// leaves as well as on one that does not.

// pullGrace is how long past its own expiry a request is still counted as one
// the server may be serving.
//
// The server ends an expired request with a `408`, measured from when IT
// received the request, so the client's view runs late by a request's transit
// and the status's return. One second covers a leaf link across a network
// with room to spare. Past it the request is presumed gone and a fetch sends
// another — and a delivery it does still make lands in the buffer like any
// other, so a server running later than this costs a second request rather
// than a record.
const pullGrace = time.Second

// puller is one consumer's standing pull.
type puller struct {
	nc      *nats.Conn
	log     *slog.Logger
	subject string // the consumer's MSG.NEXT subject, in the connection's API
	sub     *nats.Subscription

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
	// requests are the requests the server may still be serving, oldest
	// first — the order the server fills and ends them in.
	requests []*standingRequest
	// failure is a status that ended a request as a failure, for the next
	// fetch to report.
	failure error
	// released is the handle having been given up ([puller.release]): no
	// fetch will run again, and a record the inbox still hands over is
	// handed back rather than buffered.
	released bool
}

// standingRequest is one request as the client counts it: what the server
// still owes it, and when the client stops expecting its end.
type standingRequest struct {
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
	p := &puller{nc: q.nc, log: q.log, subject: subject, wake: make(chan struct{}, 1)}
	// A CALLBACK SUBSCRIPTION rather than a channel one. A channel the
	// library delivers into drops what finds it full, and a dropped
	// delivery is a record in flight that nobody holds until the ack
	// window returns it — so it would have to be sized by the largest
	// batch, which is the allocation this file exists to remove. The
	// callback's queue grows with what arrives.
	if p.sub, err = q.nc.Subscribe(q.nc.NewInbox(), p.deliver); err != nil {
		return nil, fmt.Errorf("jetstream: subscribe the pulls on %s: %w", consumer, err)
	}
	return p, nil
}

// close ends the standing inbox. What it had buffered is dropped with it,
// which is right only where the consumer those records were delivered for is
// going too — see [DomainConsumer.Reset]. A handle whose consumer stays is
// [puller.release]d instead.
func (p *puller) close() { _ = p.sub.Unsubscribe() }

// release gives the pull up for good, for a consumer that stays: what the
// buffer holds is handed back, and the inbox is DRAINED rather than dropped —
// the server stops routing to it, and whatever had already reached this
// client is still passed to [puller.deliver], which hands it back too, where
// an unsubscribe would discard it in flight. A request still standing on the
// server then has a reply nobody is interested in, and the server skips it
// for the next request of the consumer. Called once no fetch will run again.
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
// A hand-back that fails leaves the record where an unread delivery always
// was — in flight until the ack window redelivers it — and says so, since
// that is thirty seconds of a strict log standing still.
func (p *puller) handBack(msgs []*nats.Msg) {
	for _, msg := range msgs {
		if err := msg.Nak(); err != nil {
			seq := uint64(0)
			if meta, unreadable := msg.Metadata(); unreadable == nil {
				seq = meta.Sequence.Stream
			}
			p.log.Warn("jetstream_pull_handback_failed", "subject", p.subject,
				"seq", seq, "error", err.Error(),
				"detail", "a record delivered to a released state-log pull could "+
					"not be handed back, so the broker redelivers it only once "+
					"its acknowledgement window passes")
		}
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
	if err := p.nc.PublishRequest(p.subject, p.sub.Subject, body); err != nil {
		return fmt.Errorf("jetstream: send a pull: %w", err)
	}
	p.requests = append(p.requests, &standingRequest{
		batch: batch, bytesLeft: maxBytes, bounded: maxBytes > 0,
		until: time.Now().Add(max(wait, 0) + pullGrace),
	})
	return nil
}

// deliver is the inbox's callback: one delivery, or one status about the
// oldest standing request.
func (p *puller) deliver(msg *nats.Msg) {
	status, failure := pullStatus(msg)
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
		if len(p.requests) > 0 {
			// The server fills its requests oldest first, and one it
			// has filled — a batch met, or a byte bound met exactly —
			// it removes without a word.
			r := p.requests[0]
			r.batch--
			r.bytesLeft -= msg.Size()
			if r.batch <= 0 || (r.bounded && r.bytesLeft <= 0) {
				p.requests = p.requests[1:]
			}
		}
	case status == statusHeartbeat:
		// Not asked for, and no answer about any request.
		p.mu.Unlock()
		return
	default:
		if len(p.requests) > 0 {
			p.requests = p.requests[1:]
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
			p.failure = nil
			p.mu.Unlock()
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
// whichever is first — and past the deadline, always the latter. Held under
// p.mu, with a request standing.
//
// THE PRESUMPTION IS WHAT BOUNDS THE WAIT, because a request's ending status
// is not guaranteed to arrive: a connection that drops takes the server's copy
// of the request with it and nobody is left to send the `408`, and a slow
// consumer's inbox drops a status like anything else. Evaluated only when a
// delivery woke the fetch, the rule [pullGrace] states did nothing on exactly
// those paths — past its deadline a fetch waited out an hour's timer on a
// request nothing was serving, and before it, a request left standing by an
// earlier fetch kept this one from sending its own until its whole wait had
// passed, so a record appended meanwhile was delivered to nobody.
func (p *puller) wakeAt(now, deadline time.Time) time.Time {
	until := p.requests[0].until
	if !now.Before(deadline) || until.Before(deadline) {
		return until
	}
	return deadline
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

// statusHeader and descriptionHeader are where the connection puts a status
// line's code and its text: a status arrives as `NATS/1.0 408 Request Timeout`
// with no payload, and the client library parses those two parts into these
// two headers under names it does not export.
const (
	statusHeader      = "Status"
	descriptionHeader = "Description"
)

// The status codes a pull's inbox receives.
const (
	statusHeartbeat = "100"
	statusNoMsgs    = "404"
	statusTimeout   = "408"
	statusConflict  = "409"
)

// pullStatus reads a delivery as a status: empty for a record, and for a
// status, whether it ends the request as a FAILURE.
//
// THE ORDINARY ENDINGS ARE NOT FAILURES: the wait running out (`408`), a
// no-wait request finding nothing (`404`), and the two `409`s that are the
// server's own bounds doing their job — the next record would pass the byte
// bound, or the batch filled with bytes to spare. Every other status is the
// request refused or cut off — the consumer deleted under it, its leader
// moving, too many requests already waiting, no JetStream answering — and
// the reader reports it rather than returning what looks like an empty log.
func pullStatus(msg *nats.Msg) (string, error) {
	if len(msg.Data) > 0 || len(msg.Header) == 0 {
		return "", nil
	}
	code := msg.Header.Get(statusHeader)
	if code == "" {
		return "", nil
	}
	description := msg.Header.Get(descriptionHeader)
	switch code {
	case statusHeartbeat, statusNoMsgs, statusTimeout:
		return code, nil
	case statusConflict:
		lower := strings.ToLower(description)
		if strings.Contains(lower, "exceeds maxbytes") || strings.Contains(lower, "batch completed") {
			return code, nil
		}
	}
	return code, fmt.Errorf("%w: the server ended the pull with %s %s",
		errPullRefused, code, description)
}

// errPullRefused is a pull the server ended as a failure rather than by
// running out of time or filling a bound.
var errPullRefused = errors.New("jetstream: pull refused")
