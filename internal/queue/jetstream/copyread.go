package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue"
)

// HOW A MESSAGE TOO LARGE FOR A LEADER'S REPLY IS READ ACROSS A LEAF'S LINK.
//
// A stream's leader answers a read of one message (`STREAM.MSG.GET`,
// [jsapi.Leader]) in JSON, the message base64-encoded inside it, a third
// larger than the message ([jsapi.ReplyBytes]). The server on a leaf's side
// of its link closes the WHOLE link on any message past its max_payload, the
// transport's eight mebibytes (nats-server v2.15.2, leafnode.go,
// processLeafMsgArgs → maxPayloadViolation), so a leaf that asked the leader
// for a message of more than about six mebibytes — which the transport
// carries whole — had its link closed on the reply, asked again over the link
// redialled, and had it closed again, every lease and mailbox the node holds
// across it cut off until the read's deadline (crewlet/crewlet#1031).
//
// # Only across a leaf's link
//
// So a connection that crosses a LEAF'S LINK ([link.leaf]) reads such a
// message RAW, from a consumer's delivery, which carries the message as it was
// stored ([copyRead]). So does one whose leader's reply would fit but could
// not land inside the ceiling every read is held to, at the link's pace
// ([Queue.leaderCarries]): the leader's read is asked for no longer than that
// ceiling, so a reply that takes longer to cross failed at every ask, where a
// raw read waits for its message's own crossing.
//
// Every other connection keeps the leader's read. A member's own crosses no
// link, and a dial to a broker somebody else runs crosses only that broker's
// routes and its delivery to its own client, none of which holds a message to
// a max_payload: nats-server checks one only on a client's publish and on a
// leaf's message (client.go's processPub and processHeaderPub, leafnode.go's
// processLeafMsgArgs). And there the leader's read is the cheaper and the
// sturdier: one request, where a copy read is a stream's info, consumers the
// metadata leader makes and deletes, and a pull — on a state log's write path,
// which every create on an external fleet's large logs asks
// ([DomainLog.LastSeq]), that was about six metadata proposals in place of one
// message read, and a write that failed while the metadata group elected a
// leader, where the stream's own leader answers through it.
//
// # From the leader's own copy, through a consumer placed there
//
// The broker places a consumer of one replica on one replica of the stream
// (nats-server v2.15.2, jetstream_cluster.go's createGroupForConsumer), and
// the consumer reads that replica's copy. A copy behind the leader has not
// heard of what the leader stored a moment ago, and still holds what the
// leader removed a moment ago — so read there, an entry that exists could
// read as gone, and one a peer dropped as still held: the two answers a
// leader's read never gave. A copy cannot say how far behind it is, either: a
// consumer that found nothing at or past where it stands leaves its own
// position where it began on the pinned server's file store as often as it
// moves it to the copy's end (filestore.go's LoadNextMsg answers sequence 0
// for a match it did not find in a block), so "nothing here yet" and "nothing
// here any more" look the same.
//
// So a copy read takes its answer ONLY FROM THE LEADER'S OWN COPY. It asks
// the leader which server leads the stream ([jsapi.Leader.Leading]), and
// keeps a consumer only where the broker placed it on that server: the
// consumer's own create answer names the server it runs on, since the server
// that runs a consumer is the one that answers its creation (nats-server
// v2.15.2, jetstream_cluster.go's processClusterCreateConsumer, the info's
// cluster from clusterInfo), and a consumer on a server that is not
// clustered names none — there is one copy there, the leader's ([hosted]). A
// consumer placed elsewhere is deleted and made again, which the broker
// places afresh, until one is there ([copyRead.onLeader], [placements]). The
// leader's copy holds every message the leader acknowledged storing and none
// it acknowledged removing, as the leader's read answers from it; so each
// answer is the leader's — through a delivery the leader's server makes from
// that copy, which carries the message raw.
//
// THE BROKER'S PICK IS NOT UNIFORM. It shuffles the stream's live replicas
// and then moves those its metadata leader counts as caught up to the front
// ("Prefer caught up peers"), taking the first: the metadata leader itself
// always, and any other only once it has acknowledged the metadata leader
// since that leader's election and was heard from within three of its
// heartbeats (raft.go's IsFollowerCaughtUp). In a healthy cluster that is
// every live replica, and the pick is uniform among them. Just after the
// metadata group elects a leader, or while the stream's leader lags the
// metadata leader by more than three heartbeats, every consumer lands off the
// stream's leader however many are made back to back, so past one draw of
// them the read makes each a metadata heartbeat after the last
// ([placementPause]), which is what it takes that leader to be counted again.
//
// # Exactly the message at that sequence and subject, and nothing else
//
// What a consumer begun at a sequence has to deliver is counted on the
// leader's copy as the consumer is made (its NumPending: none means the
// leader holds nothing there or past it); a consumer begun at a sequence
// never delivers one before it ([atOrPast]); only a delivery at exactly the
// sequence is taken; and a payload is read only from a consumer filtered to
// the subject the probe of the same sequence found there — a message's
// subject never changes — under a byte bound the probed message fits and a
// larger one does not ([copyAt.delivered]): where a peer removed the message
// since the probe and a larger one follows it on its subject, the broker
// answers the pull with its bound rather than carry that one across the link
// for nothing, and the read asks the leader's copy again what the sequence
// holds ([copyRead.message]).
//
// # Every pull on one inbox, and a quiet one settled by the consumer's word
//
// A read's pulls are asked on an INBOX OF ITS OWN, held for the whole read
// ([pulling]), so a delivery reaches it however late it arrives: a pull the
// client had given up on, its inbox dropped at its expiry, threw away a
// delivery queued on the link behind other bytes, and the read failed on
// the consumer's count of a delivery that never arrived.
//
// A pull that hears nothing for a quiet second ([letterQuiet]) asks the
// consumer what it says of itself ([pulling.quiet]). A cluster drops a new
// consumer's FIRST pull routinely — natsobj measured one read in twenty-five
// on a loaded three-member fleet — and a consumer that holds no pull and has
// delivered nothing never got it: it is deleted and made again, up to
// [firstPullRemakes] times, as internal/objstore/natsobj's reads do. One that
// holds the pull, or has delivered what has not arrived, is being read
// however slowly — a whole message at the floor takes minutes to cross — and
// is waited on, on the same inbox. Waited on with no word, the read failed
// as a heartbeat missed: a pull long enough to have a heartbeat of its own
// that the broker never received sends none, and was ended at two of them,
// the whole wait spent, and never asked again.
//
// # One bound for the whole read, and the caller's own first
//
// Every request a read makes — the leader's info, the creates, the pulls, the
// consumer's info — runs under one context ([copyRead.bounded]): the caller's,
// so a caller that goes stops the read at once, a node's shutdown among them;
// and where the caller set no deadline, the ceiling every read is held to
// ([jsprovision.LookupBudget]) and the time the message it reads takes to
// cross raw at the link's pace ([jsapi.Sending]) — longer than the five
// seconds the client holds such a caller's one request to, because a copy
// read is several, and a delivery may wait on the link behind a whole message
// carried ahead of it.

// throwaway makes a consumer of stream that its reader makes, reads and
// deletes — acknowledging nothing, since a read must never change what the
// stream retains, and reaped by the broker [peekBackstop] after its reader
// stops pulling should the reader die before it deletes it — and answers it
// with the delete its reader defers. cfg says the rest: where it begins, what
// it filters to, and whether it delivers payloads.
func (q *Queue) throwaway(ctx context.Context, stream string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, func(), error) {

	cfg.AckPolicy = jetstream.AckNonePolicy
	cfg.InactiveThreshold = peekBackstop
	q.noteConsumerProposal()
	cons, err := q.js.CreateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, nil, err
	}
	return cons, func() {
		// WithoutCancel because a cleanup that inherits the cancellation
		// it is cleaning up after does nothing at all — and then its OWN
		// bound, because WithoutCancel drops the deadline along with the
		// cancellation.
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), peekCleanup)
		defer stop()
		_ = q.js.DeleteConsumer(cleanup, stream, cons.CachedInfo().Name)
	}, nil
}

// fetched is one message a consumer handed its reader, and where it stands.
type fetched struct {
	msg  *nats.Msg
	meta *nats.MsgMetadata
}

// pulling is one reader's pulls of one consumer, every one of them asked on
// ONE INBOX the reader holds until it is done ([pulling.stop]) — see this
// file's head — and the consumer's own count of the last delivery the reader
// took.
//
// ITS OWN RATHER THAN THE CLIENT'S FETCH, which subscribes an inbox for one
// pull and drops it when the client decides the pull is over: a delivery the
// server made before then, still crossing the link when the client gave up,
// lands on an inbox nobody holds. And rather than the domain consumers'
// standing pull ([puller]), which keeps a buffer, hands records back and
// keeps requests standing for an applier that reads a consumer for its whole
// life: a throwaway is read once, for one message or one listing, and
// deleted.
type pulling struct {
	nc   *nats.Conn
	cons jetstream.Consumer
	// timing is the queue's ([Queue.provisioning]): what the consumer's
	// info is asked at.
	timing jsprovision.Timing
	// subject is the subject the consumer's pulls are asked on, in the API
	// the connection speaks.
	subject string
	sub     *nats.Subscription
	// arrived is the consumer's count of the last delivery the reader
	// took.
	arrived uint64
}

// pulls opens a reader's pulls of cons, a consumer of stream, on an inbox of
// their own.
func (q *Queue) pulls(stream string, cons jetstream.Consumer) (*pulling, error) {
	next, err := q.API().Subject(fmt.Sprintf(server.JSApiRequestNextT, stream, cons.CachedInfo().Name))
	if err != nil {
		return nil, fmt.Errorf("address the pulls of %s: %w", stream, err)
	}
	sub, err := q.nc.SubscribeSync(q.nc.NewInbox())
	if err != nil {
		return nil, fmt.Errorf("subscribe the pulls of %s: %w", stream, err)
	}
	return &pulling{nc: q.nc, cons: cons, timing: q.provisioning(), subject: next, sub: sub}, nil
}

// stop ends the reader's pulls: a delivery that arrives after it is dropped,
// as the consumer it was delivered for is about to be.
func (p *pulling) stop() { _ = p.sub.Unsubscribe() }

// ask asks the consumer for up to n deliveries on the reader's inbox, held by
// the broker for no longer than expires and — where maxBytes is not zero —
// none larger than maxBytes as the broker counts a delivery.
func (p *pulling) ask(n int, expires time.Duration, maxBytes int) error {
	body, err := json.Marshal(server.JSApiConsumerGetNextRequest{
		Batch: n, Expires: expires, MaxBytes: maxBytes})
	if err != nil {
		return fmt.Errorf("encode a pull: %w", err)
	}
	if err := p.nc.PublishRequest(p.subject, p.sub.Subject, body); err != nil {
		return fmt.Errorf("send a pull: %w", err)
	}
	return nil
}

// errExpired is a pull the broker ended because its wait ran out with
// nothing more to deliver (`408`).
var errExpired = errors.New("the consumer's pull expired")

// errPastMaxBytes is a pull the broker ended in place of a delivery larger
// than its byte bound ([pastMaxBytes]).
var errPastMaxBytes = errors.New("the consumer's next message is larger than its pull's byte bound")

// next is the next delivery on the reader's inbox before ctx ends, or what
// the broker said of a pull instead: that it expired ([errExpired]), that its
// next message is past its byte bound ([errPastMaxBytes]), or that it was
// refused ([pullStatus]). A heartbeat says nothing and is passed over.
//
// THE CONSUMER'S OWN COUNT GUARDS EVERY DELIVERY: one it counted that never
// arrived fails the read at the delivery after it, since a reader that went on
// past it would take the next delivery for the one it lost, and a message
// missing from a read reads to its reader as one that is not there.
func (p *pulling) next(ctx context.Context) (fetched, error) {
	for {
		msg, err := p.sub.NextMsgWithContext(ctx)
		if err != nil {
			return fetched{}, err
		}
		status, failure := pullStatus(msg)
		switch {
		case failure != nil:
			return fetched{}, failure
		case status == statusHeartbeat:
			continue
		case status == statusTimeout:
			return fetched{}, errExpired
		case pastMaxBytes(msg):
			return fetched{}, errPastMaxBytes
		case status != "":
			continue
		}
		meta, err := msg.Metadata()
		if err != nil {
			return fetched{}, fmt.Errorf("a message was delivered with no position this build "+
				"can read: %w", err)
		}
		if meta.Sequence.Consumer != p.arrived+1 {
			return fetched{}, fmt.Errorf("the consumer counted delivery %d after %d: a message "+
				"it delivered never arrived", meta.Sequence.Consumer, p.arrived)
		}
		p.arrived = meta.Sequence.Consumer
		return fetched{msg: msg, meta: meta}, nil
	}
}

// errQuiet is a wait for a delivery that ended with nothing arrived before
// its quiet did, while the read's own context goes on.
var errQuiet = errors.New("no delivery yet")

// within is [pulling.next], waited for no longer than quiet: [errQuiet] where
// the quiet ended it and ctx goes on.
func (p *pulling) within(ctx context.Context, quiet time.Duration) (fetched, error) {
	soon, cancel := context.WithTimeout(ctx, quiet)
	defer cancel()
	d, err := p.next(soon)
	if errors.Is(err, context.DeadlineExceeded) && soon.Err() != nil && ctx.Err() == nil {
		return fetched{}, errQuiet
	}
	return d, err
}

// quietSays is what a consumer's own info says of a reader that heard nothing
// from it for a quiet ([pulling.quiet]).
type quietSays int

const (
	// crossing: the consumer delivered what the reader has not had yet, or
	// holds the reader's pull with something to deliver — what the reader
	// waits for is on its way.
	crossing quietSays = iota
	// unpulled: the consumer holds messages, holds no pull, and delivered
	// nothing the reader has not had — the reader's pull never reached it.
	unpulled
	// drained: the consumer holds nothing more, and the reader has had all
	// it delivered.
	drained
)

// quiet asks the consumer what it says of a reader that heard nothing from it
// for a quiet, and answers its info with that — asked again while nobody
// answers, as every read of a leader is ([jsprovision.ReadWith]): only the
// consumer's leader answers its info, and a member that still believes in one
// its group just lost drops the request without a word.
func (p *pulling) quiet(ctx context.Context) (quietSays, *jetstream.ConsumerInfo, error) {
	info, err := jsprovision.ReadWith(ctx, p.timing,
		func(ctx context.Context) (*jetstream.ConsumerInfo, error) { return p.cons.Info(ctx) }, nil)
	switch {
	case err != nil:
		return 0, nil, err
	case info.Delivered.Consumer > p.arrived:
		return crossing, info, nil
	case info.Delivered.Consumer < p.arrived:
		return 0, nil, fmt.Errorf("the consumer counts %d deliveries, and its reader took %d",
			info.Delivered.Consumer, p.arrived)
	case info.NumPending == 0:
		return drained, info, nil
	case info.NumWaiting > 0:
		return crossing, info, nil
	}
	return unpulled, info, nil
}

// owed is how many deliveries the consumer info describes still owes its
// reader: what it holds, and what it delivered that has not arrived.
func (p *pulling) owed(info *jetstream.ConsumerInfo) uint64 {
	return info.NumPending + (info.Delivered.Consumer - p.arrived)
}

// firstPullRemakes is how many times a copy read makes its consumer again
// after one never got its first pull, before it asks that consumer once more
// on the same inbox and waits for the delivery like any other.
//
// THREE, internal/objstore/natsobj's own number for the same drop, for its
// reason: a cluster dropped a first pull one read in twenty-five at the worst
// measured, and a pull made again was dropped as often as a first one, so
// after three one read in four hundred thousand would still be waiting — and a
// consumer still without its pull after three is behind a link slow enough
// to hold the pull itself back, where a fourth consumer only adds a create
// while the pull is on its way.
const firstPullRemakes = 3

// leaderCarries reports whether a stream leader's read of a message of
// messageBytes, header block and payload, reaches this queue's connection —
// see this file's head. Always, unless a leaf's link stands between the
// connection and the leader ([link.leaf], decided where the connection was
// made); and across one, only where the leader's reply ([jsapi.ReplyBytes])
// is no larger than what the leaf's server holds a message to — past it, it
// closes its whole link on the reply — and lands inside the ceiling every
// read is held to at the link's pace: the read term, the reply's crossing
// ([jsapi.Crossing]) and no more than the queue's lookup ceiling
// ([jsprovision.LookupBudget]).
func (q *Queue) leaderCarries(messageBytes int) bool {
	if !q.link.leaf {
		return true
	}
	t := q.provisioning()
	return int64(jsapi.ReplyBytes(messageBytes)) <= q.nc.MaxPayload() &&
		t.ReadTerm+jsapi.Crossing(messageBytes, q.link.pace) <= t.Lookup
}

// copyRead reads one stream's messages through consumers placed on the
// server that leads it — see this file's head.
type copyRead struct {
	q      *Queue
	stream string
	// leading names the server that leads the stream, and how many
	// replicas the stream keeps, as the leader states them
	// ([jsapi.Leader.Leading]).
	leading func(ctx context.Context) (string, int, error)
	// pause is how long a read waits before each consumer it makes past
	// its first draw ([placementPause]).
	pause time.Duration
}

// copiesOf is a copy read of stream, its leader named by leader.
func (q *Queue) copiesOf(stream string, leader *jsapi.Leader) *copyRead {
	return &copyRead{q: q, stream: stream, leading: leader.Leading, pause: placementPause}
}

// copyAt is what the stream holds at one sequence.
type copyAt struct {
	held bool
	// subject is the message's, which never changes.
	subject string
	// bytes is the message as it was stored, its header block and its
	// payload — what a read of it carries raw, counted from its headers'
	// delivery (a few bytes over, for the size the broker states beside
	// them).
	bytes int
}

// replyRoom is the most a delivery's reply subject takes beside the stream's
// name, as the broker counts a delivery against a pull's byte bound
// (nats-server v2.15.2, consumer.go: the subject, the reply, the header block
// and the payload): `$JS.ACK.`, the JetStream domain (the embedded fleet's is
// jsapi.Domain) and the account's eight-character hash, the consumer's name —
// twenty-two characters, as the client names a consumer it makes — and five
// numbers of at most twenty digits, with the dots between: about a hundred and
// sixty bytes. A quarter kibibyte is that with room.
const replyRoom = 256

// delivered is the most the broker counts a delivery of a's message of stream
// as against a pull's byte bound: the message as its probe counted it, its
// subject and its reply ([replyRoom]). A later message on the subject larger
// than that by more than the room is refused by the bound; one inside the
// room is a few hundred bytes over the message, carried and discarded.
func (a copyAt) delivered(stream string) int {
	return a.bytes + len(a.subject) + len(stream) + replyRoom
}

// stored is one message as the leader's server delivered it, raw.
type stored struct {
	subject string
	header  nats.Header
	data    []byte
	time    time.Time
}

// at is what the stream holds at seq, read from its headers alone — none of
// its payload — on the leader's own copy.
func (c *copyRead) at(ctx context.Context, seq uint64) (copyAt, error) {
	var found copyAt
	err := c.read(ctx, jetstream.ConsumerConfig{
		Description:   fmt.Sprintf("a read of what %s holds at %d, headers only", c.stream, seq),
		DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:   seq,
		// NO PAYLOAD CROSSES: the broker delivers the message's headers
		// and states its payload's size in place of it — whatever subject
		// it is on, since this read is unfiltered to see that.
		HeadersOnly: true,
	}, 0, 0, func(d fetched) error {
		if early := atOrPast(d, seq); early != nil || d.meta.Sequence.Stream > seq {
			// PAST SEQ: the leader holds no message there.
			return early
		}
		size, sizeErr := strconv.Atoi(d.msg.Header.Get(nats.MsgSize))
		if sizeErr != nil {
			return fmt.Errorf("the message at %d was delivered without the size of its "+
				"payload: %w", seq, sizeErr)
		}
		found = copyAt{held: true, subject: d.msg.Subject,
			bytes: (&nats.Msg{Header: d.msg.Header}).Size() + size}
		return nil
	})
	return found, err
}

// message is the message at seq, which a probe of the same stream found there
// as at says, read RAW from the leader's own copy, with held false where the
// leader has removed it since — the probe's word is never taken for the
// payload's. Its consumer is filtered to the message's subject, so a message
// removed since hands over at most the next on that subject in its place,
// never another subject's — and that one only where it is no larger than this
// one ([copyAt.delivered]): one larger is answered with the pull's byte bound,
// and the sequence is probed again, so a message still there is never
// answered as gone on the bound's word. Its read waits for the time the
// message takes to cross the link at its pace ([copyRead.crossing]).
func (c *copyRead) message(ctx context.Context, seq uint64, at copyAt) (stored, bool, error) {
	var (
		got  stored
		held bool
	)
	err := c.read(ctx, jetstream.ConsumerConfig{
		Description:   fmt.Sprintf("a read of the message %s holds at %d", c.stream, seq),
		FilterSubject: at.subject,
		DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:   seq,
	}, c.crossing(at.bytes), at.delivered(c.stream), func(d fetched) error {
		if early := atOrPast(d, seq); early != nil || d.meta.Sequence.Stream > seq {
			return early
		}
		got = stored{subject: d.msg.Subject, header: d.msg.Header, data: d.msg.Data,
			time: d.meta.Timestamp}
		held = true
		return nil
	})
	if !errors.Is(err, errPastMaxBytes) {
		return got, held, err
	}
	// THE NEXT ON ITS SUBJECT IS LARGER THAN THE MESSAGE THE PROBE FOUND, so
	// the bound held it back — which says the message is gone only while
	// the leader's copy says so too.
	again, err := c.at(ctx, seq)
	switch {
	case err != nil:
		return stored{}, false, err
	case again.held:
		return stored{}, false, fmt.Errorf("the broker held back the message at %d, %d bytes, "+
			"under a pull bounded at %d", seq, again.bytes, at.delivered(c.stream))
	}
	return stored{}, false, nil
}

// crossing is how long a message of messageBytes takes to cross this
// connection raw at its pace ([jsapi.Sending], [link.pace]): what a read of
// it waits for beside the ceiling ([copyRead.bounded]).
func (c *copyRead) crossing(messageBytes int) time.Duration {
	return jsapi.Sending(messageBytes, c.q.link.pace)
}

// last is the newest sequence the stream holds on subject, with false where it
// holds none: from the headers of the leader's newest message there, none of
// its payload.
func (c *copyRead) last(ctx context.Context, subject string) (uint64, bool, error) {
	var newest uint64
	err := c.read(ctx, jetstream.ConsumerConfig{
		Description:   fmt.Sprintf("a read of the newest message on %s, headers only", subject),
		FilterSubject: subject,
		// THE NEWEST ON THE SUBJECT, as the leader's copy holds it when the
		// consumer is made — which counts it, or counts nothing there.
		DeliverPolicy: jetstream.DeliverLastPolicy,
		HeadersOnly:   true,
	}, 0, 0, func(d fetched) error {
		newest = d.meta.Sequence.Stream
		return nil
	})
	return newest, newest > 0, err
}

// atOrPast refuses a delivery before seq: a consumer begun at seq never
// delivers one, so it is a consumer that did not begin where it was asked.
func atOrPast(d fetched, seq uint64) error {
	if d.meta.Sequence.Stream < seq {
		return fmt.Errorf("a consumer begun at %d delivered %d, before it", seq, d.meta.Sequence.Stream)
	}
	return nil
}

// bounded is ctx for one copy read of a message taking crossing to cross raw:
// its caller's own, where it set a deadline; and where it set none, ended at
// the queue's lookup ceiling ([jsprovision.LookupBudget]) and that crossing —
// see this file's head.
func (c *copyRead) bounded(ctx context.Context, crossing time.Duration) (context.Context, context.CancelFunc) {
	if _, set := ctx.Deadline(); set {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.q.provisioning().Lookup+crossing)
}

// errUnpulled is a consumer whose first pull never reached it, which the read
// makes again ([copyRead.first]).
var errUnpulled = errors.New("the consumer never got its first pull")

// read makes a consumer of the stream as cfg says — one replica, in memory,
// acknowledging nothing ([Queue.throwaway]) — on the server that leads the
// stream ([copyRead.onLeader]), and hands take its first delivery: what it
// was made to read. A consumer that counted nothing to deliver when it was
// made — nothing at or past where it begins, on the leader's copy, matching
// its filter — answers that without a pull, and take is never called.
//
// Its delivery is pulled no larger than maxBytes where that is not zero, and
// the whole read is bounded as [copyRead.bounded] says for a message taking
// crossing to cross. A consumer whose first pull a cluster dropped is made
// again ([firstPullRemakes]).
func (c *copyRead) read(ctx context.Context, cfg jetstream.ConsumerConfig, crossing time.Duration,
	maxBytes int, take func(fetched) error) error {

	ctx, cancel := c.bounded(ctx, crossing)
	defer cancel()
	for remade := 0; ; remade++ {
		cons, drop, err := c.onLeader(ctx, cfg)
		if err != nil {
			return err
		}
		got, err := c.first(ctx, cons, maxBytes, remade < firstPullRemakes)
		drop()
		switch {
		case errors.Is(err, errUnpulled):
			continue
		case err != nil:
			return err
		case got == nil:
			return nil
		}
		return take(*got)
	}
}

// first is the first message cons delivers, pulled once on an inbox of its own
// for as long as ctx allows, or nil where it has nothing to deliver — and
// [errUnpulled], where remake allows it, for a consumer whose pull never
// reached it: see this file's head.
func (c *copyRead) first(ctx context.Context, cons jetstream.Consumer, maxBytes int,
	remake bool) (*fetched, error) {

	if cons.CachedInfo().NumPending == 0 {
		return nil, nil
	}
	p, err := c.q.pulls(c.stream, cons)
	if err != nil {
		return nil, err
	}
	defer p.stop()
	// THE PULL WAITS FOR AS LONG AS THE READ DOES, which ctx always bounds
	// ([copyRead.bounded]).
	deadline, _ := ctx.Deadline()
	if err = p.ask(1, time.Until(deadline), maxBytes); err != nil {
		return nil, err
	}
	got, err := p.within(ctx, letterQuiet)
	if errors.Is(err, errQuiet) {
		says, _, qerr := p.quiet(ctx)
		switch {
		case errors.Is(qerr, jetstream.ErrConsumerNotFound):
			// GONE before it delivered anything — as good as one that
			// never had the pull.
			says = unpulled
		case qerr != nil:
			return nil, qerr
		}
		switch {
		case says == drained:
			// REMOVED since the consumer counted it: nothing there any
			// more.
			return nil, nil
		case says == unpulled && remake:
			return nil, errUnpulled
		case says == unpulled:
			// THE REMAKES ARE SPENT: asked once more, on the same inbox,
			// and waited for whole.
			if err = p.ask(1, time.Until(deadline), maxBytes); err != nil {
				return nil, err
			}
		}
		got, err = p.next(ctx)
	}
	switch {
	case errors.Is(err, errExpired), errors.Is(err, context.DeadlineExceeded):
		return nil, fmt.Errorf("the consumer of %s delivered nothing that arrived before the "+
			"read's deadline: %w", c.stream, err)
	case err != nil:
		return nil, err
	}
	return &got, nil
}

// placementPause is how long a read waits before each consumer it makes past
// its first draw — as many as the stream keeps replicas, made back to back —
// whose placements all missed the stream's leader.
//
// ONE SECOND, nats-server's raft heartbeat (raft.go's hbInterval): the broker
// prefers the replicas its metadata leader counts as caught up — an
// acknowledgement since that leader's election, and a word from it within
// three heartbeats — so a stream leader passed over for that is counted again
// within a heartbeat of answering, and a consumer made sooner is placed by the
// same count as the one before.
const placementPause = time.Second

// onLeader makes a consumer of the stream as cfg says on the server that leads
// it: made again, the broker placing each afresh among the stream's replicas,
// until one is placed there — the first draw of them back to back, every one
// after a [copyRead.pause] past the last, for up to [placements] of them,
// past which the read fails rather than read a copy that is not the leader's,
// as an outage the caller may ask again after (queue.ErrUnavailable).
func (c *copyRead) onLeader(ctx context.Context, cfg jetstream.ConsumerConfig) (jetstream.Consumer, func(), error) {
	leader, replicas, err := c.leading(ctx)
	if err != nil {
		return nil, nil, err
	}
	// A READER'S THROWAWAY, as a listing's is: one replica, in memory.
	cfg.Replicas, cfg.MemoryStorage = 1, true
	tries := placements(replicas)
	for try := range tries {
		if try >= max(replicas, 1) {
			if err := paused(ctx, c.pause); err != nil {
				return nil, nil, err
			}
		}
		cons, drop, makeErr := c.q.throwaway(ctx, c.stream, cfg)
		if makeErr != nil {
			return nil, nil, makeErr
		}
		if hosted(cons.CachedInfo(), leader) {
			return cons, drop, nil
		}
		drop()
	}
	return nil, nil, fmt.Errorf("%w: the broker placed none of %d consumers on %s, the server "+
		"leading %s", queue.ErrUnavailable, tries, leader, c.stream)
}

// paused waits d, or until ctx ends.
func paused(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// hosted reports whether the consumer info describes is one the stream's
// leader runs: its own create answer names the server it runs on, and a
// consumer on a server that is not clustered names none — that server holds
// the stream's one copy, so it leads the stream.
func hosted(info *jetstream.ConsumerInfo, leader string) bool {
	return info != nil && (info.Cluster == nil || info.Cluster.Leader == leader)
}

// unplacedOdds is the most often a read in a healthy cluster may give up on
// placing its consumer on the stream's leader: once in a million reads.
const unplacedOdds = 1e-6

// placements is how many consumers a read makes before it gives up on one
// placed on the leader of a stream keeping replicas copies: enough that a
// healthy cluster's broker, picking uniformly among the replicas it counts as
// caught up — every live one there — misses the leader every time at most
// [unplacedOdds] of the time: 35 at three replicas, 62 at five, and one where
// the stream keeps one, which is the leader. Past the first replicas of them
// each waits a metadata heartbeat ([placementPause]), so a stream leader the
// broker passes over until it is counted caught up again has 32 heartbeats to
// be at three replicas, and 57 at five. Each costs a consumer made and
// deleted, a few milliseconds of the metadata leader's: a read in a healthy
// cluster makes as many as the stream keeps copies, on average.
func placements(replicas int) int {
	if replicas <= 1 {
		return 1
	}
	miss := float64(replicas-1) / float64(replicas)
	return int(math.Ceil(math.Log(unplacedOdds) / math.Log(miss)))
}
