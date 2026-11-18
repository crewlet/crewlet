package natsobj

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/objstore"
)

// memberWindow is the window on a MEMBER's own client — a data node, whose
// connection reaches its own embedded member in its own process, so no link
// lies between the put and the stream for a request to queue on: sixteen
// messages, two mebibytes.
//
// SIZED BY THROUGHPUT, the smallest window measured at the client library's
// own: equal within the runs' spread at one replica, with and without an
// fsync a message, where four lost up to a tenth and sixty-four gained
// nothing. COUNTED FOR THE CONNECTION, so K puts at once share it rather than
// holding K of it in the member's memory and its stream's pipeline — and it
// costs them nothing measurable (the package doc's figures): eight 16 MiB
// puts at once ran at 159–177 MB/s together on a member's own client against
// 164–183 with sixteen each, and at three replicas, every message a quorum
// append, at 50–62 against 47–63, where sixty-four shared ran at 42–65 and a
// lone put at 48–69 against 59–68 with sixteen.
const memberWindow = 16

// newMemberWindow is the window a member's own client is given: [memberWindow]
// whole messages, in the bytes a put takes for each ([pieceTake]), shared by
// every put on the connection first come first served, since K puts at once
// share it rather than hold K of it in the member's memory and its stream's
// pipeline.
func newMemberWindow() *jsapi.Window {
	w, err := jsapi.NewWindow(memberWindow * pieceTake(MessageBytes))
	if err != nil {
		panic(err) // a constant above zero
	}
	return w
}

// pieceTake is what a put's message of dataBytes takes of its window: the
// bytes its request puts on the link ([jsapi.RequestBytes]), so a small
// object or an object's last piece takes what it carries, never a whole
// [MessageBytes], and small transfers interleave.
func pieceTake(dataBytes int) int { return jsapi.RequestBytes(dataBytes) }

// purgeWait bounds each step of a failed put's clean-up: the wait for the
// acknowledgements still out, and each purge after it.
//
// FIVE SECONDS, the broker API's own request timeout (the client's
// defaultAPITimeout), which a purge is: a healthy broker answers in
// milliseconds, and one that has not in five seconds is the reason the put
// failed in the first place.
const purgeWait = 5 * time.Second

// Put implements [objstore.Backend].
//
// THE BACKEND'S OWN PUT, IN THE LIBRARY'S FORMAT — an object is its pieces on
// `$O.<bucket>.C.<nuid>` and then one message on `$O.<bucket>.M.<name>`
// naming them, with `Nats-Rollup: sub` — so `nats object get` reads what it
// stored, and every get, stat and listing here reads it as it reads the
// library's. Never the library's put, which gave each message's
// acknowledgement five seconds from its PUBLISH: a message published behind
// bytes the link has not yet carried is acknowledged only once they have
// crossed, so a put across any link slower than its queued bytes in five
// seconds failed while it was moving (measured: 2 MiB at 1.25× the floor
// failed at 10.0 s, over TCP and through a leaf alike, and stored in 48.5 s
// by this put), on a frozen TCP link it filled the socket until the
// connection's write deadline took the node's whole connection down, and
// through a frozen leaf it failed in five seconds on a metadata lookup a key
// that is never reused does not need.
//
// # Paced by the broker's acknowledgements
//
// Pieces of [MessageBytes] are published IN ORDER from ONE buffer, refilled as
// soon as each publish returns — nats.go copies a payload into its write
// buffer at the publish, and only a RE-publish reads the message again — each
// taking its bytes ([pieceTake]) from the [Backend]'s window first: across a
// link the link's ONE window, which its downloads, walks and memory replays
// share both ways, and on a member's own client [memberWindow] messages the
// connection's puts share; and with NO PUBLISH RETRY, because the library's
// retry of a piece answered "no responders" re-sends from its one reused
// message over its one reused buffer, which by then holds a later piece: a
// piece the stream did not take fails the put instead, which its caller asks
// again whole. The metadata goes last, once every piece is acknowledged.
//
// A PUT ENDS ONLY WHEN THE BROKER HAS ACKNOWLEDGED NOTHING FOR [objstore.Stall]
// WHILE PIECES ARE OUT ([objstore.ErrStalled]), so a put moving at any pace the
// broker acknowledges is never ended, and one waiting on its caller's slow body
// — nothing out — is not waiting on the broker; nor is one waiting its turn
// in the window, which takes no stall of its own, since every take ahead of
// it is bounded by its own. Across a metered link the stall runs only while
// the link carries nothing at the floor ([Backend.held]): a put whose pieces
// wait behind the link's other traffic is not stalled while it moves. No
// message waits behind more than the window, so across a link at the floor
// each one's bytes have crossed within four seconds of its publish
// ([jsapi.LinkWindow]) behind the window's other takes, and its
// acknowledgement follows a round trip later, behind whatever the link
// carries back — a
// leaf's compressed link, which delivers what it flushed as one block,
// included: sixteen messages out through a leaf at the floor, two mebibytes,
// the window a member's own client is given, were acknowledged in blocks up
// to 26.4 s apart (3.1 s over TCP), under the minute.
//
// # A failed put leaves nothing a stat or a listing finds
//
// The caller's context ending reaches it as a failed read
// ([objstore.ContextReader]), as every failure does, and the clean-up runs
// under a context that does not end with the caller's: it waits up to
// [purgeWait] for the acknowledgements still out — purged before them, a put
// through a leaf had pieces land after the purge — and then purges the
// METADATA FIRST where it was published, since one whose acknowledgement was
// lost may have landed and would name pieces the next purge removes, and then
// the pieces, each within [purgeWait]. A piece a very slow or a thawed link
// delivers after even that is an unfinished upload, which the collector
// abandons a day later ([Backend.Pending]). A piece the stream refuses because
// the bucket holds its whole reservation fails the put at once, as any refused
// piece does, and is answered as [objstore.ErrFull].
func (b *Backend) Put(ctx context.Context, name string, r io.Reader, m objstore.PutMeta) error {
	p := &put{b: b, name: name, nuid: nuid.Next(), changed: make(chan struct{})}
	putCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	p.cancel = cancel
	pub, err := jetstream.New(b.js.Conn(),
		jetstream.WithPublishAsyncAckHandler(func(_ jetstream.JetStream, msg *nats.Msg, _ *jetstream.PubAck) {
			p.answered(msg, nil)
		}),
		jetstream.WithPublishAsyncErrHandler(func(_ jetstream.JetStream, msg *nats.Msg, err error) {
			p.answered(msg, err)
		}))
	if err != nil {
		return fmt.Errorf("natsobj: put %s: a publisher: %w", name, err)
	}
	// LAST, after the clean-up: it fails every acknowledgement still out,
	// which the clean-up's wait for them would read as answered — and gives
	// their places in the window back, each through its error; and without
	// it every put would leave its reply subscription on the connection for
	// good.
	defer pub.CleanupPublisher()
	p.pub = pub
	if err := p.run(putCtx, r, m); err != nil {
		p.clean(context.WithoutCancel(ctx))
		if full(err) {
			// THE RESERVATION IS SPENT: the stream refused a message past
			// its ceiling, which is the bucket filling, never the broker
			// failing, and the one refusal its caller can name to a person
			// ([objstore.ErrFull]), with the room this bucket needs.
			return &objstore.Full{Remedy: FullRemedy, Err: fmt.Errorf(
				"natsobj: put %s: %s holds as many bytes as it reserves: %w", name, Bucket, err)}
		}
		return fmt.Errorf("natsobj: put %s: %w", name, err)
	}
	return nil
}

// put is one [Backend.Put] in flight.
type put struct {
	b    *Backend
	name string
	nuid string
	pub  jetstream.JetStream

	// cancel ends the put's context with a cause: the stall
	// ([put.check]), or an acknowledgement in error.
	cancel context.CancelCauseFunc

	mu sync.Mutex
	// out is how many of the put's messages are published and not yet
	// acknowledged.
	out int
	// last is when the broker last acknowledged one of them, or when one
	// went out into an empty window — the instant the stall is measured
	// from.
	last time.Time
	// timer runs [put.check] while messages are out.
	timer *time.Timer
	// changed is closed, and replaced, at every acknowledgement.
	changed chan struct{}
	// metaOut is set once the metadata message has been handed to the
	// connection, so a clean-up purges it; and piecesOut once a piece has,
	// so a put that never had a place in the window — nothing of it sent —
	// is not held a purge's [purgeWait] on a connection that answers
	// nothing.
	metaOut, piecesOut bool
}

// run publishes r's pieces and then the metadata naming them, and answers once
// the broker has acknowledged every one — or the first failure.
func (p *put) run(ctx context.Context, r io.Reader, m objstore.PutMeta) error {
	body := objstore.ContextReader(ctx, r)
	buf := make([]byte, MessageBytes)
	digest := sha256.New()
	var size uint64
	var chunks uint32
	for {
		n, end, err := objstore.Fill(body, buf)
		if err != nil {
			// THE PUT'S OWN CAUSE where it has one — a stall or a piece
			// the stream refused, which ended ctx and so failed this read
			// as cancelled — and otherwise the read's own error.
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return err
		}
		if n > 0 {
			_, _ = digest.Write(buf[:n])
			if err := p.publish(ctx, &nats.Msg{Subject: pieceSubject(p.nuid), Data: buf[:n]}, false); err != nil {
				return err
			}
			size += uint64(n)
			chunks++
		}
		if end {
			break
		}
	}
	// EVERY PIECE ACKNOWLEDGED BEFORE ANYTHING NAMES THEM, so a put the
	// broker stops acknowledging has its metadata nowhere: published behind
	// its last piece at once, as the library's was, the metadata of a put
	// stalled on a frozen leaf was still on the link with the clean-up's
	// purges behind it, and the stream's ingest and its API keep no order
	// between them — after the thaw a listing found the object the purge had
	// missed (measured). What the wait costs is a round trip to the stream a
	// put: nothing on one member, a quorum's at three replicas (64 KiB took
	// 4.6–5.5 ms rather than 3.4–3.8).
	if err := p.wait(ctx, func() bool { return p.out == 0 }); err != nil {
		return err
	}
	meta := jetstream.ObjectMeta{Name: p.name,
		Opts: &jetstream.ObjectMetaOptions{ChunkSize: MessageBytes}}
	if m.ContentType != "" {
		meta.Headers = nats.Header{"Content-Type": []string{m.ContentType}}
	}
	data, err := json.Marshal(jetstream.ObjectInfo{ObjectMeta: meta, Bucket: Bucket, NUID: p.nuid,
		Size: size, Chunks: chunks, Digest: jetstream.GetObjectDigestValue(digest)})
	if err != nil {
		return fmt.Errorf("encode its metadata: %w", err)
	}
	msg := nats.NewMsg(metaSubject(p.name))
	msg.Header.Set(jetstream.MsgRollup, jetstream.MsgRollupSubject)
	msg.Data = data
	if err := p.publish(ctx, msg, true); err != nil {
		return err
	}
	if err := p.wait(ctx, func() bool { return p.out == 0 }); err != nil {
		return err
	}
	p.mu.Lock()
	p.timer.Stop()
	p.mu.Unlock()
	return nil
}

// publish sends msg — the put's metadata where meta — once its bytes are
// taken from the [Backend]'s window ([pieceTake]), counting it out first: an
// acknowledgement can arrive before the publish call returns. Its turn is
// waited for under ctx alone: a put with pieces of its own out is ended by its
// own stall ([put.check]), which cancels ctx.
//
// NO RETRY ([jetstream.WithRetryAttempts] 0): see [Backend.Put].
func (p *put) publish(ctx context.Context, msg *nats.Msg, meta bool) error {
	if err := p.b.window.Take(ctx, pieceTake(len(msg.Data))); err != nil {
		return err
	}
	p.mu.Lock()
	if meta {
		p.metaOut = true
	} else {
		p.piecesOut = true
	}
	if p.out == 0 {
		// THE STALL'S CLOCK STARTS HERE: nothing was out, so the broker
		// owed nothing until now.
		p.last = time.Now()
		if p.timer == nil {
			p.timer = time.AfterFunc(p.b.stall, p.check)
		} else {
			p.timer.Reset(p.b.stall)
		}
	}
	p.out++
	p.mu.Unlock()
	if _, err := p.pub.PublishMsgAsync(msg, jetstream.WithRetryAttempts(0)); err != nil {
		// NOT SENT: no answer will come, so its take comes back here.
		p.settle(msg)
		return fmt.Errorf("publish %s: %w", msg.Subject, err)
	}
	return nil
}

// answered is the broker's answer to one message: its acknowledgement, or err
// — the stream refused it, the connection lost it, or the put gave up on it
// ([jetstream.JetStream.CleanupPublisher]) — which fails the put at once.
// Called once for each message the publish sent, msg, so its take of the
// window comes back exactly once.
func (p *put) answered(msg *nats.Msg, err error) {
	p.settle(msg)
	if err != nil {
		p.cancel(err)
	}
}

// settle counts msg, one message of the put's, no longer out, and gives its
// take of the window back: what [put.publish] took for it, read off the
// message's own length, which the buffer refilled behind it does not move.
func (p *put) settle(msg *nats.Msg) {
	p.mu.Lock()
	p.out = max(p.out-1, 0)
	p.last = time.Now()
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
	p.b.window.Give(pieceTake(len(msg.Data)))
}

// check is the stall timer: a put whose messages are out and that has had no
// acknowledgement for the stall bound is ended, as [objstore.ErrStalled].
//
// THE DECISION RE-READS THE LAST ACKNOWLEDGEMENT, under the same lock every
// acknowledgement is recorded under, so one recorded before it resets the
// minute and the timer is a sample, never a deadline fixed when the put began;
// and nothing out is nothing owed — a put waiting on its caller's body is not
// waiting on the broker. Across a metered link a put quiet for the bound
// while the link moves is held, judged again a [quietWait] later
// ([Backend.held]).
func (p *put) check() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.out == 0 {
		return
	}
	quiet := time.Since(p.last)
	switch {
	case quiet < p.b.stall:
		p.timer.Reset(p.b.stall - quiet)
		return
	case p.b.held(quiet):
		p.timer.Reset(quietWait)
		return
	}
	p.cancel(p.stalled(p.out))
}

// stalled is the put's stall, with out of its messages out.
func (p *put) stalled(out int) error {
	return fmt.Errorf("%w: the broker acknowledged nothing of it for %v, with %d of its messages out",
		objstore.ErrStalled, p.b.stall, out)
}

// wait returns once ready holds — read under the put's lock — or the put's
// context ends, with its cause.
func (p *put) wait(ctx context.Context, ready func() bool) error {
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		p.mu.Lock()
		ok, changed := ready(), p.changed
		p.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
		}
	}
}

// clean takes back what a failed put stored, so no stat or listing finds it:
// see [Backend.Put].
//
// ctx ends with nothing the put's caller does — the failure the clean-up
// follows is often the caller giving up.
func (p *put) clean(ctx context.Context) {
	p.mu.Lock()
	if p.timer != nil {
		p.timer.Stop()
	}
	p.mu.Unlock()
	drain, cancel := context.WithTimeout(ctx, purgeWait)
	_ = p.wait(drain, func() bool { return p.out == 0 })
	cancel()
	p.mu.Lock()
	metaOut, piecesOut := p.metaOut, p.piecesOut
	p.mu.Unlock()
	if metaOut {
		if err := p.purge(ctx, metaSubject(p.name)); err != nil {
			log.WarnContext(ctx, "natsobj_put_cleanup_failed", "name", p.name, "subject", metaSubject(p.name),
				"error", err.Error(), "detail", "a failed put's metadata may still name its "+
					"pieces; the collector deletes an object no row names a day after it was written")
		}
	}
	if !piecesOut {
		return
	}
	if err := p.purge(ctx, pieceSubject(p.nuid)); err != nil {
		log.WarnContext(ctx, "natsobj_put_cleanup_failed", "name", p.name, "subject", pieceSubject(p.nuid),
			"error", err.Error(), "detail", "a failed put's pieces are an unfinished upload, which "+
				"the collector abandons a day after it began")
	}
}

// purge removes every message on subject, within [purgeWait].
func (p *put) purge(ctx context.Context, subject string) error {
	ctx, cancel := context.WithTimeout(ctx, purgeWait)
	defer cancel()
	if err := p.b.stream.Purge(ctx, jetstream.WithPurgeSubject(subject)); err != nil &&
		!errors.Is(err, jetstream.ErrStreamNotFound) {
		return err
	}
	return nil
}
