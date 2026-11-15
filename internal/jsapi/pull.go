package jsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
)

// A CONSUMER'S PULLS, ASKED BY HAND ON ONE INBOX THE READER HOLDS.
//
// A reader of a consumer it made for one read — internal/queue/jetstream's
// copy reads and its dead-letter listings — asks the consumer's pulls itself
// (`CONSUMER.MSG.NEXT`), every one of them on ONE INBOX it holds until it is
// done ([Pulls]), rather than through the client's fetch, which subscribes an
// inbox for one pull and drops it when the CLIENT decides the pull is over: a
// delivery the server made before then, still crossing a link when the client
// gave up, lands on an inbox nobody holds, and a read that went on counted it
// as lost. Held for the whole read, the inbox takes a delivery however late it
// arrives, and the consumer's own count of its deliveries ([Pulls.Next])
// tells a delivery that never arrived from one still on its way.
//
// # Every word the server sends about a pull, named
//
// A pull is answered with deliveries and with STATUSES — a status line and no
// payload — and the reader is told each one for what it is ([Event]): a
// delivery, an idle heartbeat (`100`) a pull asked with one is sent while the
// consumer holds it with nothing to deliver, its expiry (`408`), the byte
// bound the next message would pass (`409 Message Size Exceeds MaxBytes`),
// the count filled with bytes to spare (`409 Batch Completed`), and anything
// else, which is the pull REFUSED: the consumer deleted under it, its leader
// moving, too many pulls waiting, nobody answering (`503`). And which of them
// ENDS the pull ([Event.Ended]), counted as the server counts it — so a reader
// that asks its next pull only once the last has ended never has one pull's
// status read as the next one's end.
//
// # A quiet pull settled by the consumer's word
//
// A pull that hears nothing for a while asks the consumer what it says of
// itself ([Pulls.Quiet]): a consumer that holds no pull and has delivered
// nothing never got the pull — a cluster drops a new consumer's FIRST pull
// routinely, and natsobj measured one read in twenty-five on a loaded
// three-member fleet — and is made again ([FirstPullRemakes]); one that holds
// the pull, or has delivered what has not arrived, is being read however
// slowly, and is waited on, on the same inbox.

// The headers the connection parses a status line into: a status arrives as
// `NATS/1.0 408 Request Timeout` with no payload, and the client library
// parses those two parts into these two headers under names it does not
// export.
const (
	statusHeader      = "Status"
	descriptionHeader = "Description"
)

// The status codes a pull's inbox receives.
const (
	// StatusHeartbeat is an idle heartbeat (`100`), sent while the
	// consumer holds a pull asked with one and has nothing to deliver.
	StatusHeartbeat = "100"
	statusNoMsgs    = "404"
	statusTimeout   = "408"
	statusConflict  = "409"
)

// PullStatus reads a delivery as a status: empty for a record, and for a
// status, whether it ends the request as a FAILURE.
//
// THE ORDINARY ENDINGS ARE NOT FAILURES: the wait running out (`408`), a
// no-wait request finding nothing (`404`), and the two `409`s that are the
// server's own bounds doing their job — the next record would pass the byte
// bound, or the batch filled with bytes to spare. Every other status is the
// request refused or cut off — the consumer deleted under it, its leader
// moving, too many requests already waiting, no JetStream answering — and
// the reader reports it rather than returning what looks like an empty log.
func PullStatus(msg *nats.Msg) (string, error) {
	if len(msg.Data) > 0 || len(msg.Header) == 0 {
		return "", nil
	}
	code := msg.Header.Get(statusHeader)
	if code == "" {
		return "", nil
	}
	description := msg.Header.Get(descriptionHeader)
	switch code {
	case StatusHeartbeat, statusNoMsgs, statusTimeout:
		return code, nil
	case statusConflict:
		if pastMaxBytes(msg) || batchCompleted(msg) {
			return code, nil
		}
	}
	return code, fmt.Errorf("%w: the server ended the pull with %s %s",
		ErrPullRefused, code, description)
}

// pastMaxBytes reports a status that ended a pull because the next record
// would pass the pull's byte bound — the `409` whose text says so, which the
// server sends in place of that record rather than deliver it (nats-server
// v2.15.2, consumer.go's nextWaiting: "Message Size Exceeds MaxBytes").
func pastMaxBytes(msg *nats.Msg) bool {
	return msg.Header.Get(statusHeader) == statusConflict &&
		strings.Contains(strings.ToLower(msg.Header.Get(descriptionHeader)), "exceeds maxbytes")
}

// batchCompleted reports a status that ended a pull because its count was
// delivered with bytes of its byte bound to spare — the `409` the server sends
// after the delivery that filled the count (nats-server v2.15.2, consumer.go's
// getNextMsg loop: JsPullRequestRemainingBytesT, "Batch Completed").
func batchCompleted(msg *nats.Msg) bool {
	return msg.Header.Get(statusHeader) == statusConflict &&
		strings.Contains(strings.ToLower(msg.Header.Get(descriptionHeader)), "batch completed")
}

// ErrPullRefused is a pull the server ended as a failure rather than by
// running out of time or filling a bound.
var ErrPullRefused = errors.New("jsapi: pull refused")

// ErrDeliveryLost is a delivery whose consumer counted one before it that
// never reached the reader: a reader that went on past it would take the next
// delivery for the one it lost, and a message missing from a read reads to its
// reader as one that is not there.
var ErrDeliveryLost = errors.New("jsapi: a delivery the consumer counted never arrived")

// EventKind is what one word on a pull's inbox is ([Event]).
type EventKind string

// The words a pull's inbox carries.
const (
	// Delivered is a message the consumer delivered.
	Delivered EventKind = "delivered"
	// Heartbeat is an idle heartbeat (`100`): the consumer holds the pull,
	// asked with a heartbeat, and has nothing to deliver to it now.
	Heartbeat EventKind = "heartbeat"
	// Expired is the pull's expiry (`408`), or the end of a no-wait pull
	// that found nothing (`404`), which [Pulls.Ask] never asks.
	Expired EventKind = "expired"
	// PastMaxBytes is the pull ended in place of a message larger than what
	// is left of its byte bound (`409 Message Size Exceeds MaxBytes`).
	PastMaxBytes EventKind = "past max bytes"
	// BatchCompleted is the pull's count delivered with bytes of its byte
	// bound to spare (`409 Batch Completed`), sent after the delivery that
	// filled it.
	BatchCompleted EventKind = "batch completed"
	// Refused is any other status: the consumer deleted under the pull, its
	// leader moving, too many pulls waiting, nobody answering (`503`).
	Refused EventKind = "refused"
)

// Valid reports whether k is one of the kinds a pull's inbox carries.
func (k EventKind) Valid() bool {
	switch k {
	case Delivered, Heartbeat, Expired, PastMaxBytes, BatchCompleted, Refused:
		return true
	}
	return false
}

// Event is one word on a pull's inbox: a delivery, or a status about the pull.
type Event struct {
	Kind EventKind
	// Msg and Meta are the delivery and where it stands, for [Delivered].
	Msg  *nats.Msg
	Meta *nats.MsgMetadata
	// Code and Description are the status line's, for every other kind.
	Code        string
	Description string
	// Ended says the event ends the pull the reader asked last ([Pulls.Ask]):
	// every status but a heartbeat, and a delivery only where the server
	// sends nothing after it — one that used the pull's byte bound exactly,
	// or the last of the count of a pull asked with no byte bound. A pull
	// whose count runs out with bytes of its bound to spare ends at the
	// [BatchCompleted] that always follows its last delivery, never at the
	// delivery: read as the end there, that status would arrive after the
	// next pull was asked and end it in the reader's books.
	Ended bool

	refusal error
}

// Refusal is the pull's refusal, wrapping [ErrPullRefused], for [Refused];
// nil for every other kind.
func (e Event) Refusal() error { return e.refusal }

// openPull is the pull the reader asked last, as the server counts what it
// still owes it.
type openPull struct {
	// count is how many deliveries the pull may still bring.
	count int
	// bytesLeft is what is left of its byte bound, and bounded whether it
	// carried one.
	bytesLeft int
	bounded   bool
}

// Pulls is one reader's pulls of one consumer, every one of them asked on ONE
// INBOX the reader holds until it is done ([Pulls.Stop]) — see this file's
// head — the consumer's own count of the last delivery the reader took, and
// the pull it asked last.
//
// ITS OWN RATHER THAN THE CLIENT'S FETCH, which drops its inbox when the
// client decides the pull is over; and rather than internal/queue/jetstream's
// standing pull of a domain consumer, which keeps a buffer, hands records back
// and keeps requests standing for an applier that reads a consumer for its
// whole life: these pulls are one reader's, for a consumer it reads and is done
// with. One reader at a time: nothing here is safe for two goroutines at once.
type Pulls struct {
	nc   *nats.Conn
	cons jetstream.Consumer
	// timing is what the consumer's info is asked at ([Pulls.Quiet]).
	timing jsprovision.Timing
	// subject is the subject the consumer's pulls are asked on, in the API
	// the connection speaks.
	subject string
	sub     *nats.Subscription
	// arrived is the consumer's count of the last delivery the reader
	// took.
	arrived uint64
	open    openPull
}

// NewPulls opens a reader's pulls of cons, a consumer of stream, on an inbox
// of their own on nc, addressed in api; the consumer's info is asked at timing
// ([Pulls.Quiet]).
func NewPulls(nc *nats.Conn, api API, stream string, cons jetstream.Consumer,
	timing jsprovision.Timing) (*Pulls, error) {

	if nc == nil {
		return nil, errors.New("jsapi: a NATS connection is required for a consumer's pulls")
	}
	next, err := api.Subject(fmt.Sprintf(server.JSApiRequestNextT, stream, cons.CachedInfo().Name))
	if err != nil {
		return nil, fmt.Errorf("address the pulls of %s: %w", stream, err)
	}
	sub, err := nc.SubscribeSync(nc.NewInbox())
	if err != nil {
		return nil, fmt.Errorf("subscribe the pulls of %s: %w", stream, err)
	}
	return &Pulls{nc: nc, cons: cons, timing: timing, subject: next, sub: sub}, nil
}

// Stop ends the reader's pulls: a delivery that arrives after it is dropped,
// as the consumer it was delivered for is about to be.
func (p *Pulls) Stop() { _ = p.sub.Unsubscribe() }

// Ask asks the consumer for up to n deliveries on the reader's inbox — none
// larger than maxBytes in all as the server counts them, where maxBytes is not
// zero — held by the server for no longer than expires, and sent an idle
// heartbeat every heartbeat while it holds the pull with nothing to deliver,
// where heartbeat is not zero. The pull is the reader's open one from here
// ([Event.Ended]); asked while another is open, the books follow this one, and
// a status the other still ends with reads as this one's end — so a reader
// that needs every end exact asks again only once the last has ended.
//
// A pull with no expiry would stand on the server for good, and one whose
// heartbeat is more than half its expiry the server refuses (nats-server
// v2.15.2, consumer.go's nextReqFromMsg), so both are refused here, by name.
func (p *Pulls) Ask(n, maxBytes int, expires, heartbeat time.Duration) error {
	switch {
	case n < 1:
		return fmt.Errorf("jsapi: a pull asks for at least one delivery, not %d", n)
	case maxBytes < 0:
		return fmt.Errorf("jsapi: a pull's byte bound is %d; zero is none", maxBytes)
	case expires <= 0:
		return fmt.Errorf("jsapi: a pull's expiry is %v; one with none would stand for good", expires)
	case heartbeat < 0 || 2*heartbeat > expires:
		return fmt.Errorf("jsapi: a pull's heartbeat of %v is not between none and half its "+
			"expiry of %v", heartbeat, expires)
	}
	body, err := json.Marshal(server.JSApiConsumerGetNextRequest{
		Batch: n, MaxBytes: maxBytes, Expires: expires, Heartbeat: heartbeat})
	if err != nil {
		return fmt.Errorf("encode a pull: %w", err)
	}
	if err := p.nc.PublishRequest(p.subject, p.sub.Subject, body); err != nil {
		return fmt.Errorf("send a pull: %w", err)
	}
	p.open = openPull{count: n, bytesLeft: maxBytes, bounded: maxBytes > 0}
	return nil
}

// Next is the next word on the reader's inbox before ctx ends ([Event]).
//
// THE CONSUMER'S OWN COUNT GUARDS EVERY DELIVERY: one it counted that never
// arrived fails the read at the delivery after it ([ErrDeliveryLost]).
func (p *Pulls) Next(ctx context.Context) (Event, error) {
	msg, err := p.sub.NextMsgWithContext(ctx)
	if err != nil {
		return Event{}, err
	}
	return p.read(msg)
}

// read is msg, the next word on the reader's inbox, as an [Event], counted
// against the open pull.
func (p *Pulls) read(msg *nats.Msg) (Event, error) {
	code, failure := PullStatus(msg)
	if code == "" {
		return p.delivered(msg)
	}
	ev := Event{Code: code, Description: msg.Header.Get(descriptionHeader), Ended: true}
	switch {
	case failure != nil:
		ev.Kind, ev.refusal = Refused, failure
	case code == StatusHeartbeat:
		ev.Kind, ev.Ended = Heartbeat, false
	case code == statusTimeout, code == statusNoMsgs:
		ev.Kind = Expired
	case pastMaxBytes(msg):
		ev.Kind = PastMaxBytes
	default:
		ev.Kind = BatchCompleted
	}
	if ev.Ended {
		p.open = openPull{}
	}
	return ev, nil
}

// delivered is msg, a delivery, as an [Event]: refused where the consumer
// counted a delivery before it that never arrived, and ended where the server
// sends nothing more for the open pull after it.
func (p *Pulls) delivered(msg *nats.Msg) (Event, error) {
	meta, err := msg.Metadata()
	if err != nil {
		return Event{}, fmt.Errorf("a message was delivered with no position this build "+
			"can read: %w", err)
	}
	if meta.Sequence.Consumer != p.arrived+1 {
		return Event{}, fmt.Errorf("%w: the consumer counted delivery %d after %d",
			ErrDeliveryLost, meta.Sequence.Consumer, p.arrived)
	}
	p.arrived = meta.Sequence.Consumer
	ev := Event{Kind: Delivered, Msg: msg, Meta: meta}
	if p.open.count == 0 {
		// NO PULL OPEN in the reader's books: one asked before the last
		// ended, whose delivery is counted against none.
		return ev, nil
	}
	p.open.count--
	if p.open.bounded {
		// COUNTED AS THE SERVER COUNTS IT: the subject, the reply, the
		// header block and the payload (nats.go's Msg.Size, the server's
		// sz in consumer.go's getNextMsg loop). A delivery that leaves
		// nothing of the bound is the server's last for the pull, and it
		// sends no status after it (consumer.go: at zero it sets the
		// request's count to one, and sends a Batch Completed only for
		// bytes to spare).
		p.open.bytesLeft -= msg.Size()
		ev.Ended = p.open.bytesLeft == 0
	} else {
		// WITH NO BYTE BOUND the count's last delivery is the end, and
		// nothing follows it.
		ev.Ended = p.open.count == 0
	}
	if ev.Ended {
		p.open = openPull{}
	}
	return ev, nil
}

// QuietSays is what a consumer's own info says of a reader that heard nothing
// from it for a while ([Pulls.Quiet]).
type QuietSays string

const (
	// StillCrossing says the consumer delivered what the reader has not
	// had yet, or holds the reader's pull with something to deliver — what
	// the reader waits for is on its way.
	StillCrossing QuietSays = "still crossing"
	// Unpulled says the consumer holds messages, holds no pull, and
	// delivered nothing the reader has not had — the reader's pull never
	// reached it.
	Unpulled QuietSays = "unpulled"
	// Drained says the consumer holds nothing more, and the reader has had
	// all it delivered.
	Drained QuietSays = "drained"
)

// Valid reports whether s is one of the answers [Pulls.Quiet] gives.
func (s QuietSays) Valid() bool {
	switch s {
	case StillCrossing, Unpulled, Drained:
		return true
	}
	return false
}

// Quiet asks the consumer what it says of a reader that heard nothing from it
// for a while, and answers its info with that — asked again while nobody
// answers, as every read of a leader is ([jsprovision.ReadWith]): only the
// consumer's leader answers its info, and a member that still believes in one
// its group just lost drops the request without a word.
//
// arrival is how long the answer may take to reach the reader behind what the
// consumer delivered ahead of it, beyond the read term and the ceiling
// ([jsprovision.Timing.Arriving]): the answer crosses the same link as the
// deliveries, and a message arrives whole only once its last byte is in, so a
// probe given less than a message still crossing was asked again behind it
// until the ceiling, and failed a read whose message was on its way.
func (p *Pulls) Quiet(ctx context.Context, arrival time.Duration) (QuietSays, *jetstream.ConsumerInfo, error) {
	info, err := jsprovision.ReadWith(ctx, p.timing.Arriving(arrival),
		func(ctx context.Context) (*jetstream.ConsumerInfo, error) { return p.cons.Info(ctx) }, nil)
	switch {
	case err != nil:
		return "", nil, err
	case info.Delivered.Consumer > p.arrived:
		return StillCrossing, info, nil
	case info.Delivered.Consumer < p.arrived:
		return "", nil, fmt.Errorf("the consumer counts %d deliveries, and its reader took %d",
			info.Delivered.Consumer, p.arrived)
	case info.NumPending == 0:
		return Drained, info, nil
	case info.NumWaiting > 0:
		return StillCrossing, info, nil
	}
	return Unpulled, info, nil
}

// Owed is how many deliveries the consumer info describes still owes its
// reader: what it holds, and what it delivered that has not arrived.
func (p *Pulls) Owed(info *jetstream.ConsumerInfo) uint64 {
	return info.NumPending + (info.Delivered.Consumer - p.arrived)
}

// FirstPullRemakes is how many times a reader makes its consumer again after
// one never got its first pull ([Unpulled]), before it asks that consumer once
// more on the same inbox and waits for the delivery like any other.
//
// THREE, internal/objstore/natsobj's own number for the same drop, for its
// reason: a cluster dropped a first pull one read in twenty-five at the worst
// measured, and a pull made again was dropped as often as a first one, so
// after three one read in four hundred thousand would still be waiting — and a
// consumer still without its pull after three is behind a link slow enough
// to hold the pull itself back, where a fourth consumer only adds a create
// while the pull is on its way.
const FirstPullRemakes = 3
