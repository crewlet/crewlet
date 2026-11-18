package jetstream

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/queue"
)

// throwaway makes a consumer of stream, as cfg says, for its reader to read and
// delete, and answers it with the delete the reader defers. It acknowledges
// nothing, since a read must never change what the stream retains, and the
// broker reaps it [peekBackstop] after its reader stops pulling should the
// reader die first.
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
		// Its OWN bound, because WithoutCancel drops the deadline too.
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), peekCleanup)
		defer stop()
		_ = q.js.DeleteConsumer(cleanup, stream, cons.CachedInfo().Name)
	}, nil
}

// pulls opens a reader's pulls of cons, a consumer of stream, on an inbox of
// their own ([jsapi.Pulls]), the consumer's info asked at the queue's timing
// ([Queue.provisioning]).
func (q *Queue) pulls(stream string, cons jetstream.Consumer) (*jsapi.Pulls, error) {
	return jsapi.NewPulls(q.nc, q.API(), stream, cons, q.provisioning())
}

// errExpired is a pull the broker ended because its wait ran out with
// nothing more to deliver ([jsapi.Expired]).
var errExpired = errors.New("the consumer's pull expired")

// errPastMaxBytes is a pull the broker ended in place of a delivery larger
// than its byte bound ([jsapi.PastMaxBytes]).
var errPastMaxBytes = errors.New("the consumer's next message is larger than its pull's byte bound")

// pulled is the next delivery on p's inbox before ctx ends, or what the broker
// said of the pull instead: expired ([errExpired]), past its byte bound
// ([errPastMaxBytes]) or refused ([jsapi.Event.Refusal]). Heartbeats and
// spare-bytes counts say nothing a throwaway's reader acts on, and are passed
// over. The consumer's own count guards every delivery
// ([jsapi.ErrDeliveryLost]): a reader that went past a lost one would take the
// next for it.
func pulled(ctx context.Context, p *jsapi.Pulls) (jsapi.Event, error) {
	for {
		ev, err := p.Next(ctx)
		if err != nil {
			return jsapi.Event{}, err
		}
		switch ev.Kind {
		case jsapi.Delivered:
			return ev, nil
		case jsapi.Expired:
			return jsapi.Event{}, errExpired
		case jsapi.PastMaxBytes:
			return jsapi.Event{}, errPastMaxBytes
		case jsapi.Refused:
			return jsapi.Event{}, ev.Refusal()
		}
	}
}

// errQuiet is a wait for a delivery that ended with nothing arrived before
// its quiet did, while the read's own context goes on.
var errQuiet = errors.New("no delivery yet")

// within is [pulled], waited for no longer than quiet: [errQuiet] where the
// quiet ended it and ctx goes on.
func within(ctx context.Context, p *jsapi.Pulls, quiet time.Duration) (jsapi.Event, error) {
	soon, cancel := context.WithTimeout(ctx, quiet)
	defer cancel()
	d, err := pulled(soon, p)
	if errors.Is(err, context.DeadlineExceeded) && soon.Err() != nil && ctx.Err() == nil {
		return jsapi.Event{}, errQuiet
	}
	return d, err
}

// leaderCarries reports whether a stream leader's read of a message of
// messageBytes, header block and payload, reaches this queue's connection
// (package doc, "A message too large for a leader's reply is read raw"):
// always, unless a leaf's link stands between ([link.leaf]); across one, only
// where the leader's reply ([jsapi.ReplyBytes]) is within what the leaf's
// server holds a message to, past which it closes its whole link.
//
// NEVER BY HOW LONG THE REPLY TAKES TO CROSS: the leader's read is given its
// reply's arrival beyond the read term and the lookup ceiling
// ([jsapi.Leader.Carrying], [DomainLog.At]), so a reply the leaf's server
// carries lands however slow the link.
func (q *Queue) leaderCarries(messageBytes int) bool {
	return !q.link.leaf || int64(jsapi.ReplyBytes(messageBytes)) <= q.nc.MaxPayload()
}

// copyRead reads one stream's messages raw, through consumers placed on the
// server that leads it, where a leader's JSON reply would close a leaf's link
// (package doc, "A message too large for a leader's reply is read raw").
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
	// bytes is the message as stored, header block and payload — what a raw
	// read carries — counted from its headers' delivery (a few bytes over,
	// for the size header the broker adds).
	bytes int
}

// replyRoom is the most a delivery's reply subject takes beside the stream's
// name in the broker's count against a pull's byte bound (nats-server v2.15.2,
// consumer.go: subject, reply, header block and payload): `$JS.ACK.`, the
// JetStream domain (jsapi.Domain on the embedded fleet), the account's
// eight-character hash, the client's twenty-two-character consumer name and
// five numbers of up to twenty digits, with dots: about 160 bytes, plus room.
const replyRoom = 256

// delivered is the most the broker counts a delivery of a's message of stream
// as against a pull's byte bound: the message as probed, its subject and its
// reply ([replyRoom]). A later message on the subject larger by more than the
// room is refused by the bound; one inside it is carried and discarded.
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
	}, 0, 0, func(d jsapi.Event) error {
		if early := atOrPast(d, seq); early != nil || d.Meta.Sequence.Stream > seq {
			// PAST SEQ: the leader holds no message there.
			return early
		}
		size, sizeErr := strconv.Atoi(d.Msg.Header.Get(nats.MsgSize))
		if sizeErr != nil {
			return fmt.Errorf("the message at %d was delivered without the size of its "+
				"payload: %w", seq, sizeErr)
		}
		found = copyAt{held: true, subject: d.Msg.Subject,
			bytes: (&nats.Msg{Header: d.Msg.Header}).Size() + size}
		return nil
	})
	return found, err
}

// message is the message at seq, which a probe ([copyRead.at]) found as at
// says, read RAW from the leader's own copy, with held false where the leader
// has removed it since; the probe's word is never taken for the payload's. Its
// consumer is filtered to the message's subject and bounded at
// [copyAt.delivered], so a removed message hands over at most the next on that
// subject no larger than it; a larger one is answered with the byte bound and
// the sequence probed again, so a message still there never reads as gone.
// The read waits for the message's crossing ([copyRead.crossing]).
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
	}, c.crossing(at.bytes), at.delivered(c.stream), func(d jsapi.Event) error {
		if early := atOrPast(d, seq); early != nil || d.Meta.Sequence.Stream > seq {
			return early
		}
		got = stored{subject: d.Msg.Subject, header: d.Msg.Header, data: d.Msg.Data,
			time: d.Meta.Timestamp}
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
	}, 0, 0, func(d jsapi.Event) error {
		newest = d.Meta.Sequence.Stream
		return nil
	})
	return newest, newest > 0, err
}

// atOrPast refuses a delivery before seq: a consumer begun at seq never
// delivers one, so it is a consumer that did not begin where it was asked.
func atOrPast(d jsapi.Event, seq uint64) error {
	if d.Meta.Sequence.Stream < seq {
		return fmt.Errorf("a consumer begun at %d delivered %d, before it", seq, d.Meta.Sequence.Stream)
	}
	return nil
}

// bounded is ctx for one copy read of a message taking crossing to cross raw:
// the caller's own where it set a deadline, so a caller that goes stops the
// read; otherwise ended at the lookup ceiling ([jsprovision.LookupBudget]) plus
// that crossing, since a copy read is several requests and a delivery may wait
// on the link behind a whole message.
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
// again ([jsapi.FirstPullRemakes]).
func (c *copyRead) read(ctx context.Context, cfg jetstream.ConsumerConfig, crossing time.Duration,
	maxBytes int, take func(jsapi.Event) error) error {

	ctx, cancel := c.bounded(ctx, crossing)
	defer cancel()
	for remade := 0; ; remade++ {
		cons, drop, err := c.onLeader(ctx, cfg)
		if err != nil {
			return err
		}
		got, err := c.first(ctx, cons, crossing, maxBytes, remade < jsapi.FirstPullRemakes)
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
// [errUnpulled], where remake allows it, for a consumer whose first pull a
// cluster dropped. crossing is how long the message takes to cross raw, which
// the consumer's answer to a quiet pull may wait behind ([jsapi.Pulls.Quiet]).
func (c *copyRead) first(ctx context.Context, cons jetstream.Consumer, crossing time.Duration,
	maxBytes int, remake bool) (*jsapi.Event, error) {

	if cons.CachedInfo().NumPending == 0 {
		return nil, nil
	}
	p, err := c.q.pulls(c.stream, cons)
	if err != nil {
		return nil, err
	}
	defer p.Stop()
	// THE PULL WAITS FOR AS LONG AS THE READ DOES, which ctx always bounds
	// ([copyRead.bounded]).
	deadline, _ := ctx.Deadline()
	if err = c.ask(p, deadline, maxBytes); err != nil {
		return nil, err
	}
	got, err := within(ctx, p, letterQuiet)
	if errors.Is(err, errQuiet) {
		// ITS ANSWER BEHIND THE MESSAGE: what the consumer delivered
		// before the probe crosses the link ahead of the probe's answer.
		says, _, qerr := p.Quiet(ctx, crossing)
		switch {
		case errors.Is(qerr, jetstream.ErrConsumerNotFound):
			// GONE before it delivered anything — as good as one that
			// never had the pull.
			says = jsapi.Unpulled
		case qerr != nil:
			return nil, qerr
		}
		switch {
		case says == jsapi.Drained:
			// REMOVED since the consumer counted it: nothing there any
			// more.
			return nil, nil
		case says == jsapi.Unpulled && remake:
			return nil, errUnpulled
		case says == jsapi.Unpulled:
			// THE REMAKES ARE SPENT: asked once more, on the same inbox,
			// and waited for whole.
			if err = c.ask(p, deadline, maxBytes); err != nil {
				return nil, err
			}
		}
		got, err = pulled(ctx, p)
	}
	switch {
	case errors.Is(err, errExpired), errors.Is(err, context.DeadlineExceeded):
		return nil, c.undelivered(err)
	case err != nil:
		return nil, err
	}
	return &got, nil
}

// ask asks p for the one delivery a copy read takes, no larger than maxBytes
// where that is not zero, held by the broker until deadline — the read's own —
// and fails the read as [copyRead.undelivered] where the deadline has passed,
// since a pull asked to wait no time at all would stand nowhere.
func (c *copyRead) ask(p *jsapi.Pulls, deadline time.Time, maxBytes int) error {
	expires := time.Until(deadline)
	if expires <= 0 {
		return c.undelivered(context.DeadlineExceeded)
	}
	return p.Ask(1, maxBytes, expires, 0)
}

// undelivered is err, the read's deadline passing before its delivery
// arrived, as the read's failure.
func (c *copyRead) undelivered(err error) error {
	return fmt.Errorf("the consumer of %s delivered nothing that arrived before the read's "+
		"deadline: %w", c.stream, err)
}

// placementPause is how long a read waits before each consumer it makes past
// its first draw (as many as the stream keeps replicas, back to back) once all
// missed the stream's leader. One second, nats-server's raft heartbeat
// (raft.go's hbInterval): the broker prefers replicas its metadata leader
// counts as caught up (acknowledged since its election and heard from within
// three heartbeats, raft.go's IsFollowerCaughtUp), so a stream leader passed over is counted again within
// a heartbeat, and a consumer made sooner is placed by the same count.
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
// broker picking uniformly among them (every live replica counts as caught up
// in a healthy cluster) misses the leader every time at most [unplacedOdds] of
// the time — 35 at three replicas, 62 at five, one at one. Past the first
// replicas each waits [placementPause], so a passed-over leader has 32
// heartbeats to be counted again at three replicas, 57 at five. Each costs a
// consumer made and deleted; a healthy read makes about replicas of them.
func placements(replicas int) int {
	if replicas <= 1 {
		return 1
	}
	miss := float64(replicas-1) / float64(replicas)
	return int(math.Ceil(math.Log(unplacedOdds) / math.Log(miss)))
}
