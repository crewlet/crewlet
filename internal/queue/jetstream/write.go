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

// HOW THIS QUEUE SENDS A WRITE: every acknowledged write it makes — a publish
// ([Queue.Publish]), a dead letter's keep ([Queue.KeepDeadLetter], and the
// delivery budget's), a state log's append ([DomainLog.Append]) — is a message
// this package builds whole, waits within the budget [Queue.writing] gives it,
// and is sent by [sendWithin], ONE send at a time. The package doc's "A write
// across a link is given the time its bytes take to cross" says why.

// linkPaceOf is the slowest a queue's connection, made through embedded, may
// carry a mebibyte either way — a write's request towards the stream and a
// read's reply back: the object store's floor, [objstore.MiBPace], where it
// crosses a LINK — a dial to a broker somebody else runs (no embedded broker
// at all), or a leaf's link to the members — and none where it reaches the
// node's own member in its own process, which crosses nothing. It is the fact
// the engine hands the object store as natsobj.Config.AcrossLink, read here
// off how the connection was made.
//
// DECIDED ONCE, when the connection is made ([Queue.pace]), and every crossing
// on it reads that one decision — a read's reply ([Queue.recordCrossing], the
// dead letters' frames) and a write's request ([Queue.writing]) — so the two
// directions of one connection are never paced apart by a change to one of
// them.
func linkPaceOf(embedded *embeddedServer) time.Duration {
	if embedded == nil || embedded.leaf {
		return objstore.MiBPace
	}
	return 0
}

// writing is the budget an acknowledged write of msg on this queue's
// connection waits within, msg built whole as it is sent: the request term,
// and on a connection that crosses a link the time msg takes to cross it at
// its pace ([Queue.pace]) for each of its first [jsapi.WriteSends] sends
// ([jsapi.Budget]), a later send — an append's attempt after a leader's
// in-flight answer — waiting in the budget as it stands; a caller's own
// deadline kept as it is, by each send's context ([jsapi.Sends.Next]).
//
// THE ONE PLACE A WRITE OF THIS QUEUE IS BUDGETED, so the bytes a write is
// given and the bytes it sends are one count: msg's own, header block and
// payload ([carried]), never a figure a call site works out beside it.
func (q *Queue) writing(msg *nats.Msg) *jsapi.Sends {
	return jsapi.Budget(q.js, jsapi.Sending(carried(msg), q.pace))
}

// write sends msg, an acknowledged write that decides nothing on its own
// behalf — no compare-and-set — within the budget [Queue.writing] gives it,
// and answers its stream's acknowledgement: a keep reads whether it was a
// duplicate's ([Queue.KeepDeadLetter]).
func (q *Queue) write(ctx context.Context, msg *nats.Msg) (*jetstream.PubAck, error) {
	return sendWithin(ctx, q.writing(msg), q.js, msg)
}

// sendWithin makes the sends of one acknowledged write of msg on js, its
// caller's context ctx, each in the context sends gives it: ONE send at a
// time, with the client's own re-sends off, and the identical message again
// where nobody heard it — a quarter of a second later and [jsapi.WriteSends]
// sends in all, the client's own policy (jetstream.DefaultPubRetryWait and
// DefaultPubRetryAttempts), kept, and the number the budget gives a crossing
// to.
//
// # Why the client's re-sends are off and made here instead
//
// The client re-sends a message nobody heard inside the ONE context it was
// handed. Across a link to an external cluster the answer that nobody heard it
// comes from the far side, after the message has crossed whole, so each
// re-send crosses again — and a context sized for one crossing ended while
// the second was still on the link, a write the link was carrying at a pace
// the engine allows answered as an outage. Made here, each send waits in the
// context its budget gives it, a crossing more than the one before up to the
// write's [jsapi.WriteSends]-th send, and the budget as it stands after that —
// an append's later attempts make their sends here within the one budget
// ([jsapi.Sends]). A leaf's own broker answers that nobody heard it before
// anything crosses its link, and a member's in its own process, so there the
// extra crossing is room nobody uses rather than a wait anybody makes.
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

// carried is what msg carries across the link beside the protocol line that
// names it ([jsapi.Sending] counts that line): its header block, as the client
// encodes it, and its payload.
//
// READ OFF THE MESSAGE AS IT IS SENT, so a header a write adds is counted by
// adding it: each write builds its message whole — its headers set on it, not
// left to the client's options, which write them after this has counted —
// and sends it with no option that writes one ([sendWithin]).
func carried(msg *nats.Msg) int {
	return msg.Size() - len(msg.Subject) - len(msg.Reply)
}
