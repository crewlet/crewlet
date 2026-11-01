package natsobj

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"

	"github.com/crewlet/crewlet/internal/objstore"
)

// messageCrossing is how long one message of a put takes to cross a link at
// the floor every leg is promised: [MessageBytes] at [objstore.MiBPace],
// 3.75 seconds.
const messageCrossing = objstore.MiBPace * MessageBytes / objstore.MiB

// linkWindow is the window across a link for a node that renews its leases
// every heartbeat: the most whole messages a link at the floor carries within
// one heartbeat, at least one and at most [memberWindow] — four, half a
// mebibyte, at the shipped lease (45 s, renewed every 15 s).
//
// SIZED BY THE LEASES QUEUED BEHIND IT. A link carries the node's lease
// renewals in the order they were sent, so a renewal sent while the window
// is full crosses only once the window has: a window that crosses within a
// heartbeat lands a renewal sent a heartbeat after the last with a whole
// interval of its lease to spare — the margin the lease's own doc keeps for a
// missed beat. The HEARTBEAT IS THE NODE'S, never the shipped one:
// `coordination.lease_ttl_seconds` moves it (the TTL in force over
// seat.HeartbeatRatio), and a window sized against the shipped fifteen
// seconds held a renewal on a 15-second lease (renewed every five) for 20 s,
// past the lease — the node lost its seats to its own uploads. A test holds
// the arithmetic against the lease's own constants, over the shipped TTL and
// others.
//
// AT LEAST ONE, because a put holds a message out to move at all. A heartbeat
// shorter than one message's crossing — a lease under 11.25 s — cannot keep
// that margin behind even one, and under 5.6 s a renewal behind it at the
// floor lands after its lease. No node holds such a lease: config refuses
// one under config.MinLeaseTTL, which is over config.LinkLeaseFloor.
//
// AT MOST [memberWindow], the window sized by throughput: a longer lease buys
// a link no wider window, because sixty-four messages bought nothing over
// sixteen through a leaf as on a member's own client, and sixteen is the
// widest whose acknowledgements were measured at the floor across a leaf's
// compressed link (blocks 26.4 s apart, under [objstore.Stall]).
//
// It is also what keeps a stall honest across the link: no message waits
// behind more than the window, so at the floor every one is acknowledged
// within a heartbeat of its publish, and a leaf's compressed link, which
// delivers what it flushed as one block, never more than the window in one.
//
// What a small window costs is throughput on a fast link (the package doc's
// figures, through a leaf on loopback at `stream.sync: always`): a lone
// 16 MiB put ran at 122–134 MB/s with four out against 129–153 with sixteen
// a put, and eight puts at once at 119–140 MB/s together against 158. A
// lease under 22.5 s gets a window of ONE, a message a round trip: measured
// the same way (interleaved, 2026-10-18, on four vCPUs shared with other
// work), a lone 16 MiB put at 47–83 MB/s and eight at once at 56–78 together,
// against 120–139 and 130–135 with four, and two out at 109–125 and 102–122
// — about half a fast link's throughput, and on a link with a longer round
// trip than loopback's the share lost is larger, since a window of one moves
// 128 KiB a round trip. That is the price, on a link that did not need the
// bound, of a node that does not lose its seats to its own uploads on one
// that does.
func linkWindow(heartbeat time.Duration) int {
	return min(max(int(heartbeat/messageCrossing), 1), memberWindow)
}

// memberWindow is the window on a MEMBER's own client — a data node, whose
// connection reaches its own embedded member in its own process, so no link
// lies between the put and the stream for a renewal to queue on: sixteen
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

// window is the places every put on one node's connection shares for the
// messages it holds OUT — published and not yet acknowledged by the stream —
// before a put waits for an acknowledgement to publish another: [linkWindow]
// of them where that connection crosses a LINK to the stream (a leaf's link to
// its members, or a dial to an external cluster), [memberWindow] where it does
// not, handed out in the order they were asked for.
//
// THE CONNECTION'S, NEVER A PUT'S, because what it protects is shared: every
// put a node makes — a person's upload, a push's bundle, a seat's file — goes
// out on the one connection its leases, its events and its coordination ride
// ([Backend.Put]), and a window counted per put let K puts at once hold K
// windows on it.
//
// FIRST COME, FIRST SERVED, A PLACE AT A TIME, so puts sharing the window
// interleave rather than one taking it whole: a put asks for a place for each
// message, and a place given back goes to the oldest request, whichever put
// made it.
//
// A put waiting for a place has nothing out, so it owes the broker nothing of
// its own; it is ended as stalled only when the WINDOW has had nothing
// acknowledged for [objstore.Stall] while it held messages out — the node's
// connection taking nothing at all — and never because the puts ahead of it
// are slow.
type window struct {
	size int

	mu sync.Mutex
	// out is how many messages hold a place: every put's on the connection.
	out int
	// last is when the broker last acknowledged one of them, or when one
	// went out into an empty window — the instant a waiter's stall is
	// measured from.
	last time.Time
	// queue is the requests for a place not yet given one, oldest first.
	queue []*placeRequest
}

// placeRequest is one request for a place in a [window]; given is closed when
// it has one.
type placeRequest struct{ given chan struct{} }

func newWindow(size int) *window { return &window{size: size} }

// take holds a place in w for one message, waiting its turn behind every
// request made before it. It answers with ctx's cause if ctx ends first, and
// with [objstore.ErrStalled] if w has had nothing acknowledged for stall
// while it held messages out.
func (w *window) take(ctx context.Context, stall time.Duration) error {
	w.mu.Lock()
	if len(w.queue) == 0 && w.out < w.size {
		w.enterLocked()
		w.mu.Unlock()
		return nil
	}
	req := &placeRequest{given: make(chan struct{})}
	w.queue = append(w.queue, req)
	wait := w.quietLeftLocked(stall)
	w.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-req.given:
			return nil
		case <-ctx.Done():
			w.withdraw(req)
			return context.Cause(ctx)
		case <-timer.C:
			w.mu.Lock()
			if w.givenLocked(req) {
				w.mu.Unlock()
				return nil
			}
			if wait = w.quietLeftLocked(stall); wait > 0 {
				w.mu.Unlock()
				timer.Reset(wait)
				continue
			}
			out := w.out
			w.dropLocked(req)
			w.mu.Unlock()
			return fmt.Errorf("%w: the broker acknowledged nothing on this node's connection for %v, "+
				"with %d messages of its puts out and this one waiting for a place among them",
				objstore.ErrStalled, stall, out)
		}
	}
}

// give returns a place: its message was acknowledged — acked — or will never
// be, the stream having refused it, its publish having failed, or its put
// having given up on it ([jetstream.JetStream.CleanupPublisher]). Only an
// acknowledgement is the broker taking something, and only it moves w's last.
func (w *window) give(acked bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.out = max(w.out-1, 0)
	if acked {
		w.last = time.Now()
	}
	for len(w.queue) > 0 && w.out < w.size {
		req := w.queue[0]
		w.queue = w.queue[1:]
		w.enterLocked()
		close(req.given)
	}
}

// enterLocked counts one more message out, starting the window's clock where
// it was empty: nothing was out, so the broker owed nothing until now.
func (w *window) enterLocked() {
	if w.out == 0 {
		w.last = time.Now()
	}
	w.out++
}

// quietLeftLocked is how long w may yet go with nothing acknowledged before a
// waiter is stalled: the whole bound while nothing is out.
func (w *window) quietLeftLocked(stall time.Duration) time.Duration {
	if w.out == 0 {
		return stall
	}
	return stall - time.Since(w.last)
}

// givenLocked reports whether req has been given its place.
func (w *window) givenLocked(req *placeRequest) bool {
	select {
	case <-req.given:
		return true
	default:
		return false
	}
}

// withdraw takes req back: out of the queue if it is still waiting, and its
// place given back if it was given one as its context ended.
func (w *window) withdraw(req *placeRequest) {
	w.mu.Lock()
	given := w.givenLocked(req)
	if !given {
		w.dropLocked(req)
	}
	w.mu.Unlock()
	if given {
		w.give(false)
	}
}

// dropLocked takes req out of the queue.
func (w *window) dropLocked(req *placeRequest) {
	w.queue = slices.DeleteFunc(w.queue, func(r *placeRequest) bool { return r == req })
}

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
// waiting for a place in the connection's [window], which every put on it
// shares; and with NO PUBLISH RETRY, because the library's
// retry of a piece answered "no responders" re-sends from its one reused
// message over its one reused buffer, which by then holds a later piece: a
// piece the stream did not take fails the put instead, which its caller asks
// again whole. The metadata goes last, once every piece is acknowledged.
//
// A PUT ENDS ONLY WHEN THE BROKER HAS ACKNOWLEDGED NOTHING FOR [objstore.Stall]
// WHILE PIECES ARE OUT ([objstore.ErrStalled]), so a put moving at any pace the
// broker acknowledges is never ended, and one waiting on its caller's slow body
// — nothing out — is not waiting on the broker; nor is one waiting for a place
// in the window, which ends only when the connection has had nothing
// acknowledged for the minute ([window]). No message waits behind more than
// the window, so across a link at the floor each is acknowledged within a
// heartbeat of the node's lease of its publish ([linkWindow]; fifteen seconds
// at the shipped lease) — a leaf's compressed link, which delivers what it
// flushed as one block, included: a window of two mebibytes, the widest a link
// is given, came in blocks up to 26.4 s apart there (3.1 s over TCP).
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
		jetstream.WithPublishAsyncAckHandler(func(jetstream.JetStream, *nats.Msg, *jetstream.PubAck) {
			p.answered(nil)
		}),
		jetstream.WithPublishAsyncErrHandler(func(_ jetstream.JetStream, _ *nats.Msg, err error) {
			p.answered(err)
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

// publish sends msg — the put's metadata where meta — once it has a place in
// the connection's [window], counting it out first: an acknowledgement can
// arrive before the publish call returns.
//
// NO RETRY ([jetstream.WithRetryAttempts] 0): see [Backend.Put].
func (p *put) publish(ctx context.Context, msg *nats.Msg, meta bool) error {
	if err := p.b.window.take(ctx, p.b.stall); err != nil {
		// A PUT WITH MESSAGES OF ITS OWN OUT is told its own stall: the
		// window's last acknowledgement is never older than the put's, so
		// a window quiet for the bound is a put quiet for it too, and what
		// the put had out is what its caller wants to read.
		p.mu.Lock()
		out := p.out
		p.mu.Unlock()
		if out > 0 && errors.Is(err, objstore.ErrStalled) {
			return p.stalled(out)
		}
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
		// NOT SENT: no answer will come, so its place comes back here.
		p.settle(false)
		return fmt.Errorf("publish %s: %w", msg.Subject, err)
	}
	return nil
}

// answered is the broker's answer to one message: its acknowledgement, or err
// — the stream refused it, the connection lost it, or the put gave up on it
// ([jetstream.JetStream.CleanupPublisher]) — which fails the put at once.
// Called once a message the publish sent, so its place in the window comes
// back exactly once.
func (p *put) answered(err error) {
	p.settle(err == nil)
	if err != nil {
		p.cancel(err)
	}
}

// settle counts one message of the put's no longer out — acknowledged where
// acked — and gives its place in the window back.
func (p *put) settle(acked bool) {
	p.mu.Lock()
	p.out = max(p.out-1, 0)
	p.last = time.Now()
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
	p.b.window.give(acked)
}

// check is the stall timer: a put whose messages are out and that has had no
// acknowledgement for the stall bound is ended, as [objstore.ErrStalled].
//
// THE DECISION RE-READS THE LAST ACKNOWLEDGEMENT, under the same lock every
// acknowledgement is recorded under, so one recorded before it resets the
// minute and the timer is a sample, never a deadline fixed when the put began;
// and nothing out is nothing owed — a put waiting on its caller's body is not
// waiting on the broker.
func (p *put) check() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.out == 0 {
		return
	}
	if quiet := time.Since(p.last); quiet < p.b.stall {
		p.timer.Reset(p.b.stall - quiet)
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
