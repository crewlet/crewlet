package jetstream

import (
	"context"
	"errors"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/objstore"
)

// Every acknowledged write here (Publish, KeepDeadLetter and the delivery
// budget's keep, DomainLog.Append) is a message built whole, budgeted by
// [Queue.writing] and sent by [sendWithin], one send at a time (package doc,
// "A write across a link is given the time its bytes take to cross").

// link is what stands between a queue's connection and its streams: how slow it
// may be, and whether a leaf's link is part of it. Decided once, where the
// connection is made ([linkOf]), and read by every crossing on it (a read's
// reply, [copyRead], a write's request), so the two directions are never paced
// apart and no read decides on a fact the connection's making did not.
type link struct {
	// pace is the slowest the connection may carry a mebibyte either way:
	// [objstore.MiBPace] where it crosses a link (a dial to an external
	// broker, or a leaf's link to the members), zero on a member's own
	// in-process connection. The engine hands the object store the same fact
	// as natsobj.Config.AcrossLink.
	pace time.Duration
	// leaf is whether a leaf's link stands between the connection and every
	// stream leader. The leaf's server closes the whole link on a message
	// past its max_payload (nats-server v2.15.2, leafnode.go,
	// processLeafMsgArgs → maxPayloadViolation); no other server the engine
	// connects through checks size (a route carries any, a server checks no
	// delivery to its own client), so an external dial is paced but holds no
	// leaf link ([Queue.leaderCarries]).
	leaf bool
	// meter reads what the link carries each way at its sockets
	// ([LinkMeter]): the leaf tunnel's, or the one external dials count
	// into; nil on a member's own connection ([Queue.LinkMeter]).
	meter *LinkMeter
}

// linkOf is the link of a connection made through embedded: an external dial
// is paced at the floor and metered by dialled; a leaf's connection is paced,
// crosses the leaf link and is metered by its tunnel; a member's crosses
// nothing.
func linkOf(embedded *embeddedServer, dialled *LinkMeter) link {
	switch {
	case embedded == nil:
		return link{pace: objstore.MiBPace, meter: dialled}
	case embedded.leaf:
		return link{pace: objstore.MiBPace, leaf: true, meter: embedded.linkMeter()}
	}
	return link{}
}

// writing is the budget an acknowledged write of msg waits within: the request
// term, plus, across a link, msg's crossing time at [link.pace] for each of its
// first [jsapi.WriteSends] sends ([jsapi.Budget]); a later send waits in the
// budget as it stands, and a caller's deadline is kept ([jsapi.Sends.Next]).
// The one place a write is budgeted, so the bytes given and the bytes sent are
// one count, msg's own ([carried]), never a figure worked out at a call site.
func (q *Queue) writing(msg *nats.Msg) *jsapi.Sends {
	return jsapi.Budget(q.js, jsapi.Sending(carried(msg), q.link.pace))
}

// write sends msg, an acknowledged write with no compare-and-set, within
// [Queue.writing]'s budget, and answers the stream's acknowledgement (a keep
// reads whether it was a duplicate's, [Queue.KeepDeadLetter]).
func (q *Queue) write(ctx context.Context, msg *nats.Msg) (*jetstream.PubAck, error) {
	return sendWithin(ctx, q.writing(msg), q.js, msg)
}

// sendWithin makes the sends of one acknowledged write of msg, each in the
// context sends gives it: one at a time, the client's re-sends off, and the same
// message again where nobody heard it, after jetstream.DefaultPubRetryWait and
// up to [jsapi.WriteSends] sends (the client's own policy, and the count the
// budget gives a crossing to).
//
// The client would re-send inside the one context it was handed, but across a
// link to an external cluster "nobody heard it" comes from the far side after
// the message crossed whole, so a context sized for one crossing ends while the
// next is still on the link, a link at a pace the engine allows read as an
// outage. Here each send gets a crossing more; a leaf's or member's own broker
// answers before anything crosses, so there the room goes unused.
func sendWithin(ctx context.Context, sends *jsapi.Sends, js jetstream.JetStream,
	msg *nats.Msg) (*jetstream.PubAck, error) {

	for sent := 1; ; sent++ {
		send, done := sends.Next(ctx)
		ack, err := js.PublishMsg(send, msg, jetstream.WithRetryAttempts(0))
		done()
		if !errors.Is(err, jetstream.ErrNoStreamResponse) || sent == jsapi.WriteSends {
			return ack, err
		}
		if paused := sends.Pause(ctx, jetstream.DefaultPubRetryWait); paused != nil {
			return nil, paused
		}
	}
}

// carried is what msg carries across the link beside its protocol line
// ([jsapi.Sending] counts that): its header block as encoded, and its payload.
// It is read off the message as sent, so every write sets its headers on the
// message, never through a client option that adds them after this counted.
func carried(msg *nats.Msg) int {
	return msg.Size() - len(msg.Subject) - len(msg.Reply)
}
