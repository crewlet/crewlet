package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// A PULL REQUEST, MADE BY HAND, because it is the one request the client
// library does not make in the shape a domain consumer needs.
//
// # What the library offers, and what each one costs
//
// A domain applier needs every pull bounded by BOTH a count and a byte total —
// a count because one pull is one transaction's worth of records, bytes
// because a record may be a barrier's hundred bytes or a bulk write's
// megabytes. The server's own request carries both (`batch` and `max_bytes`
// on `CONSUMER.MSG.NEXT`) and enforces both. The library does not let a
// caller send that request:
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
//   - Messages and Consume keep a PERSISTENT pull standing and buffer what
//     it delivers ahead of the reader, which would bound the allocation — but
//     records it has delivered and nobody has asked for yet are in flight
//     against the consumer's ack window, it needs a Stop that nothing on the
//     applier's side calls today (a runner stopped and another started over
//     the same connection would find the first one's prefetch holding the
//     consumer's whole in-flight ceiling), and every Reset would have to tear
//     it down and raise it again around the delete and create. It changes
//     what a pull IS in order to change what a pull allocates.
//
// So the request is made here: the server's own type, on the subject the
// connection's API answers, into an inbox subscription whose delivery is a
// callback appending to a slice. What a pull allocates is then a function of
// what it RECEIVED — the subscription, the request, a timer — and the count
// and the byte bound are both the SERVER's, which is the only place either is
// enforced for a record the client never saw.

// pullGrace is how long past its own expiry a pull waits for the server to
// say it ended.
//
// The server ends an expired request with a `408`, measured from when IT
// received the request, so the client's view runs late by a request's transit
// and the status's return. One second covers a leaf link across a network
// with room to spare; it is paid only when that status is lost, which is a
// connection that dropped mid-pull, and there nothing else could have been
// delivered either.
const pullGrace = time.Second

// pull is one pull request in flight.
type pull struct {
	sub *nats.Subscription

	// batch and maxBytes are what the request asked for, so the client can
	// tell a request the server has FULFILLED from one it is still serving:
	// a batch filled exactly, or a byte bound met exactly, ends with no
	// status at all.
	batch, maxBytes int

	// wake is signalled on every delivery, capacity one, so the reader is
	// never behind by more than one wake and the callback never blocks.
	wake chan struct{}

	mu        sync.Mutex
	delivered []*nats.Msg
	received  int
	bytes     int
	// ended is the server having nothing further to deliver to this
	// request, and err a status that says so as a failure.
	ended bool
	err   error
	// abandoned is the reader having stopped reading. What arrives after
	// that is handed straight back — see [pull.deliver].
	abandoned bool
}

// startPull sends one pull request for up to batch records and maxBytes of
// them (zero is no byte bound), standing for wait.
//
// A wait of zero or less asks for what is there now and waits for nothing.
func (q *Queue) startPull(stream, consumer string, batch, maxBytes int,
	wait time.Duration) (*pull, error) {

	subject, err := q.API().Subject(fmt.Sprintf(server.JSApiRequestNextT, stream, consumer))
	if err != nil {
		return nil, fmt.Errorf("jetstream: address the pull on %s: %w", consumer, err)
	}
	request := server.JSApiConsumerGetNextRequest{Batch: batch, MaxBytes: maxBytes}
	if wait > 0 {
		request.Expires = wait
	} else {
		request.NoWait = true
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("jetstream: encode the pull on %s: %w", consumer, err)
	}
	p := &pull{batch: batch, maxBytes: maxBytes, wake: make(chan struct{}, 1)}
	inbox := q.nc.NewInbox()
	// A CALLBACK SUBSCRIPTION rather than a channel one. A channel the
	// library delivers into has to hold the whole batch before the reader
	// drains it — a message that finds it full is DROPPED, and a dropped
	// delivery is a record in flight that nobody holds until the ack window
	// returns it — so it would have to be sized by the batch, which is the
	// allocation this file exists to remove. The callback's queue grows
	// with what arrives.
	if p.sub, err = q.nc.Subscribe(inbox, p.deliver); err != nil {
		return nil, fmt.Errorf("jetstream: subscribe for the pull on %s: %w", consumer, err)
	}
	if err := q.nc.PublishRequest(subject, inbox, body); err != nil {
		_ = p.sub.Unsubscribe()
		return nil, fmt.Errorf("jetstream: send the pull on %s: %w", consumer, err)
	}
	return p, nil
}

// deliver is the subscription's callback: one delivery or one status.
//
// A DELIVERY TO AN ABANDONED PULL IS HANDED BACK AT ONCE. A reader that stops
// early leaves its request standing on the server for the rest of its wait,
// and a record appended in that window is delivered to it — held against the
// consumer's in-flight count until the ACK WINDOW redelivers it, thirty
// seconds later. On a process that exits the connection goes with it and so
// does the request; on one that stops an applier and starts another over the
// same connection — an engine restarted in-process, an estate adopted and
// reopened — the next applier waited out that window for a record already on
// the log, and a barrier appended in it refused every linearizable read as
// `behind` for the whole of it. A negative acknowledgement redelivers at once,
// to whichever pull asks next.
func (p *pull) deliver(msg *nats.Msg) {
	status, failure := pullStatus(msg)
	p.mu.Lock()
	switch {
	case status == "" && p.abandoned:
		p.mu.Unlock()
		_ = msg.Nak()
		return
	case status == "":
		p.delivered = append(p.delivered, msg)
		p.received++
		p.bytes += msg.Size()
		// FULFILLED WITHOUT A WORD: a batch filled exactly, or a byte
		// bound met exactly, is a request the server has removed and
		// ends no status for.
		if p.received >= p.batch || (p.maxBytes > 0 && p.bytes >= p.maxBytes) {
			p.ended = true
		}
	case status == statusHeartbeat:
		// Not asked for, and no answer about this request.
		p.mu.Unlock()
		return
	default:
		p.ended, p.err = true, failure
	}
	ended, abandoned := p.ended, p.abandoned
	p.mu.Unlock()
	if ended && abandoned {
		// THE DELIVERY'S OWN SUBSCRIPTION, never the field: the field is
		// written by the goroutine that made the request, and nothing
		// orders that write before this callback's read.
		_ = msg.Sub.Unsubscribe()
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// take hands the reader what has been delivered since it last looked, and
// whether the request has ended.
func (p *pull) take() ([]*nats.Msg, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	taken := p.delivered
	p.delivered = nil
	return taken, p.ended, p.err
}

// finish closes a request whose reader has everything it will get.
func (p *pull) finish() { _ = p.sub.Unsubscribe() }

// abandon stops reading a request the server may still be serving, and
// returns what reached it before the reader stopped: those are delivered, and
// the reader returns them rather than handing them back.
//
// The subscription stays for as long as the request can still deliver — until
// its ending status arrives, or its expiry and [pullGrace] have passed — so
// that what arrives meanwhile is handed back rather than dropped by the
// connection as mail to nobody. Its lifetime is the request's: at most the
// wait it asked for, and the grace.
func (p *pull) abandon(remaining time.Duration) []*nats.Msg {
	p.mu.Lock()
	taken := p.delivered
	p.delivered = nil
	p.abandoned = true
	ended := p.ended
	p.mu.Unlock()
	if ended || remaining <= 0 {
		p.finish()
		return taken
	}
	time.AfterFunc(remaining+pullGrace, p.finish)
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

// read collects a pull until it ends, the context ends, or its wait and
// [pullGrace] have passed, handing each delivery to keep.
//
// WHAT IS COLLECTED IS RETURNED, on every path: a reader that stops early
// keeps what already reached it, because those records are delivered and
// held against the consumer's in-flight count whether or not anybody reads
// them.
func (p *pull) read(ctx context.Context, wait time.Duration, keep func(*nats.Msg)) error {
	started := time.Now()
	deadline := time.NewTimer(max(wait, 0) + pullGrace)
	defer deadline.Stop()
	for {
		taken, ended, err := p.take()
		for _, msg := range taken {
			keep(msg)
		}
		if ended {
			p.finish()
			return err
		}
		select {
		case <-p.wake:
		case <-deadline.C:
			// THE SERVER'S ENDING NEVER CAME, which is a connection that
			// dropped the request. Nothing more can be delivered to it.
			for _, msg := range p.abandon(0) {
				keep(msg)
			}
			return nil
		case <-ctx.Done():
			for _, msg := range p.abandon(wait - time.Since(started)) {
				keep(msg)
			}
			return ctx.Err()
		}
	}
}
