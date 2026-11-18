package jetstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// The dead-letter verbs, over `CREWLET_DLQ`: the one stream every
// subscription's budget writes to, kept by age (`stream.event_retention_hours`)
// and never by interest (package doc, "Dead letters"). An entry's keeper header
// rides in [letterHeader] and its body is the payload, so a reader that needs
// only what is kept reads no payload. A listing ([Queue.DeadLettersOn]) reads
// headers from whichever replica holds its consumer; the marks
// ([Queue.DeadLetterMarks]) are the leader's counts, which a reader holds a
// listing to; a frame ([Queue.DeadLetterFrame]) and a drop's check are the
// leader's, through its own copy across a leaf's link ([copyRead]). A keep's
// identity is its message id ([letterID]), so a second keep within the
// stream's duplicate window is answered "already kept".

// letterHeader is the header a dead letter's keeper's header rides in, beside
// the stream the keep expects (`Nats-Expected-Stream`).
const letterHeader = "Crewlet-Dead-Letter"

// letterPage is how many entries one pull of a listing asks for: 500, the
// client's default, stated rather than inherited. A listing holds headers,
// under 3 KiB an entry at worst, so a full pull is a few hundred KiB however
// large the frames, and the thousand entries a stalled roll can leave on one
// subject take two.
const letterPage = 500

// letterQuiet is how long a reader of a throwaway consumer (a listing, or a
// [copyRead]) hears nothing from its pull before it asks the consumer about
// itself ([jsapi.Pulls.Quiet]); a pull the consumer says it is answering, a
// whole message crossing a link among them, is waited on past it.
//
// One second, for internal/objstore/natsobj's quietWait reason: a cluster
// routinely drops a new consumer's first pull (sent before the reader's member
// hears who serves it), and a listing that waited out a long pull would wait
// for every such drop. Asked after a quiet second, the consumer says it
// delivered nothing, and the next pull reaches it.
const letterQuiet = time.Second

// KeepDeadLetter implements queue.EventQueue: body stored on subject in the
// dead-letter stream with header beside it, the broker's acknowledgement the
// proof it is durable — and a duplicate's acknowledgement, of a keep of id
// within the stream's duplicate window, the answer that it was already kept.
func (q *Queue) KeepDeadLetter(ctx context.Context, subject, id string, header, body []byte) (bool, error) {
	if err := deadLetterSubject(subject); err != nil {
		return false, err
	}
	if err := queue.LetterIdentity(id); err != nil {
		return false, fmt.Errorf("jetstream: keep a dead letter on %s: %w", subject, err)
	}
	if err := queue.LetterHeader(header); err != nil {
		return false, fmt.Errorf("jetstream: keep a dead letter on %s: %w", subject, err)
	}
	// The contract's ceiling, before the transport's: the twin refuses exactly
	// these, so the two agree on an entry whose header block would take it past
	// the transport ([queue.LetterFits]).
	if !queue.LetterFits(header, body) {
		return false, fmt.Errorf("keep a dead letter on %s: %d bytes of header and body exceed the "+
			"%d-byte limit less the %d a keep is framed in: %w", subject, len(header)+len(body),
			queue.MaxPayloadBytes, queue.LetterFraming, queue.ErrTooLarge)
	}
	stream, err := q.streamFor(ctx, subject)
	if err != nil {
		return false, err
	}
	// Waited for as long as its bytes take to cross where the caller set no
	// deadline ([Queue.write]): the request term plus, across a link, the whole
	// message ([letterMsg]) at the connection's pace. At the ceiling and the
	// slowest pace that is minutes, which the request term alone would fail as
	// an outage retried forever; lease renewals behind it on the link wait too
	// (crewlet/crewlet#1035, not fixed here). Classified on the caller's
	// context, so a budget this set and the broker outlasted reads as silence.
	ack, err := q.write(ctx, letterMsg(subject, stream, id, header, body))
	if err != nil {
		// Translated, as Publish does: nats.ErrMaxPayload is this backend's
		// word for the one refusal no retry changes, met only on a server
		// announcing less than the contract.
		if errors.Is(err, nats.ErrMaxPayload) {
			return false, fmt.Errorf("%s: %w: %w", tooLarge(q.nc, "keep a dead letter on", subject,
				len(header)+len(body)), queue.ErrTooLarge, err)
		}
		return false, fmt.Errorf("keep a dead letter on %s: %w", subject, q.brokerFailed(ctx, err))
	}
	return ack.Duplicate, nil
}

// letterMsg is the message a keep of id publishes: body as its payload, header
// in [letterHeader] where there is one, [letterID] as its message id, and the
// stream it was resolved to as the one the broker must store it in, so a
// broker whose streams are arranged differently refuses the entry rather than
// store it where no listing reads. Both headers are set here, not through
// publish options, so this message is all that crosses the wire: what the
// framing's test measures against [queue.LetterFraming] and the keep's budget
// counts ([carried]).
func letterMsg(subject, stream, id string, header, body []byte) *nats.Msg {
	msg := &nats.Msg{Subject: subject, Data: body, Header: nats.Header{}}
	msg.Header.Set(jetstream.ExpectedStreamHeader, stream)
	msg.Header.Set(jetstream.MsgIDHeader, letterID(subject, id))
	if len(header) > 0 {
		msg.Header.Set(letterHeader, string(header))
	}
	return msg
}

// letterID is the message id a keep of id on subject carries: the identity
// scoped to its subject, since the broker collapses a repeated id across the
// one dead-letter stream and one identity on two subjects is two entries; and
// digested, so a keep's framing stays inside [queue.LetterFraming] whatever
// the identity's length and no identity byte reaches a header the transport
// rewrites ([queue.LetterHeader]). A subject holds no NUL, so the digested
// pair is unambiguous.
func letterID(subject, id string) string {
	sum := sha256.Sum256([]byte(subject + "\x00" + id))
	return hex.EncodeToString(sum[:])
}

// DeadLettersOn implements queue.EventQueue: every entry subject holds, oldest
// first, by its header alone — read through a consumer of its own that
// delivers no payload, from the replica the broker places it on.
func (q *Queue) DeadLettersOn(ctx context.Context, subject string) ([]queue.Kept, error) {
	letters, err := q.deadLetters(ctx, subject)
	if err != nil {
		return nil, err
	}
	cons, drop, err := q.throwaway(ctx, letters.stream, jetstream.ConsumerConfig{
		Description:   "a listing of the dead letters on " + subject + ", headers only",
		FilterSubject: subject,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		// No payload crosses: the broker sends each entry's headers and
		// the payload's size instead.
		HeadersOnly: true,
		// A reader's throwaway, as a peek's is.
		Replicas:      1,
		MemoryStorage: true,
	})
	if err != nil {
		return nil, fmt.Errorf("list the dead letters on %s: %w", subject, q.brokerFailed(ctx, err))
	}
	defer drop()
	kept, err := letters.headers(ctx, q, cons)
	if err != nil {
		return nil, fmt.Errorf("list the dead letters on %s: %w", subject, q.brokerFailed(ctx, err))
	}
	return kept, nil
}

// headers is every entry cons delivers, in order, until it says nothing is
// pending: pulls of [letterPage] on one inbox held for the whole listing
// ([jsapi.Pulls]), each waited on for [letterQuiet], so an entry delivered to
// an expired pull still arrives in its place. One missing from a listing reads
// to its reader as gone.
//
// The consumer's own count decides the end, never a quiet pull: after a pull
// that brought nothing the consumer is asked ([jsapi.Pulls.Quiet]), and the
// listing ends where it holds and owes nothing more, else pulls again, for at
// most [jsprovision.LookupBudget] of silence, past which it fails rather than
// answer short.
func (s letterStream) headers(ctx context.Context, q *Queue, cons jetstream.Consumer) ([]queue.Kept, error) {
	pending := cons.CachedInfo().NumPending
	if pending == 0 {
		return nil, nil
	}
	p, err := q.pulls(s.stream, cons)
	if err != nil {
		return nil, err
	}
	defer p.Stop()
	var (
		out []queue.Kept
		// heard is when an entry last arrived.
		heard = time.Now()
	)
	for pending > 0 {
		asked := int(min(pending, letterPage))
		if err := p.Ask(asked, 0, letterQuiet, 0); err != nil {
			return nil, err
		}
		took := 0
		for took < asked && pending > 0 {
			d, err := within(ctx, p, letterQuiet)
			if errors.Is(err, errQuiet) || errors.Is(err, errExpired) {
				break
			}
			if err != nil {
				return nil, err
			}
			out = append(out, s.kept(d.Meta.Sequence.Stream, d.Meta.Timestamp, d.Msg.Header))
			pending = d.Meta.NumPending
			took++
		}
		if took > 0 {
			heard = time.Now()
			continue
		}
		// Asked at the read term alone: header deliveries cross the link
		// in a fraction of it even at the floor.
		says, info, err := p.Quiet(ctx, 0)
		switch {
		case err != nil:
			return nil, err
		case says == jsapi.Drained:
			return out, nil
		}
		if pending = p.Owed(info); time.Since(heard) >= jsprovision.LookupBudget {
			return nil, fmt.Errorf("the listing's consumer delivered nothing for %v while it "+
				"owed %d entries", jsprovision.LookupBudget, pending)
		}
	}
	return out, nil
}

// DeadLetterFrame implements queue.EventQueue: the payload of the entry seq,
// as the stream's leader holds it, once the leader says seq holds an entry of
// subject.
func (q *Queue) DeadLetterFrame(ctx context.Context, subject string, seq uint64) ([]byte, bool, error) {
	if seq == 0 {
		return nil, false, fmt.Errorf("jetstream: read a dead letter on %s: sequence 0 names no entry", subject)
	}
	letters, err := q.deadLetters(ctx, subject)
	if err != nil {
		return nil, false, err
	}
	if letters.copies == nil {
		return letters.frame(ctx, q, letters.leader, subject, seq)
	}
	// ACROSS A LEAF'S LINK: what the sequence holds is found from its
	// headers, on the leader's own copy, before anything is read whole.
	at, err := letters.copies.at(ctx, seq)
	switch {
	case err != nil:
		return nil, false, fmt.Errorf("read dead letter %d on %s: %w", seq, subject, q.brokerFailed(ctx, err))
	case !at.held:
		return nil, false, nil
	case at.subject != subject:
		return nil, false, fmt.Errorf("jetstream: dead letter %d is on %s, not %s; nothing was read",
			seq, at.subject, subject)
	case q.leaderCarries(at.bytes):
		// ITS LEADER'S REPLY CROSSES THE LINK, held to no more than the
		// leaf's server carries, so the leader answers, given the time
		// that reply takes to arrive ([jsapi.Leader.Carrying]).
		return letters.frame(ctx, q, letters.carrying(q, at.bytes), subject, seq)
	}
	msg, held, err := letters.copies.message(ctx, seq, at)
	if err != nil {
		return nil, false, fmt.Errorf("read dead letter %d on %s: %w", seq, subject, q.brokerFailed(ctx, err))
	}
	return msg.data, held, nil
}

// carrying is the stream leader's read of an entry of entryBytes on q's
// connection: presumed lost only once its reply has had time to arrive across
// the connection at its pace ([jsapi.Leader.Carrying], [link.pace]).
func (s letterStream) carrying(q *Queue, entryBytes int) *jsapi.Leader {
	return s.leader.Carrying(entryBytes, q.link.pace)
}

// frame is the entry at seq on subject as leader reads it from the stream's
// leader, refused where seq is another subject's entry.
func (s letterStream) frame(ctx context.Context, q *Queue, leader *jsapi.Leader, subject string,
	seq uint64) ([]byte, bool, error) {

	msg, err := leader.At(ctx, seq)
	switch {
	case errors.Is(err, jsapi.ErrNoMessage):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("read dead letter %d on %s: %w", seq, subject, q.brokerFailed(ctx, err))
	case msg.Subject != subject:
		return nil, false, fmt.Errorf("jetstream: dead letter %d is on %s, not %s; nothing was read",
			seq, msg.Subject, subject)
	}
	return msg.Data, true, nil
}

// DeadLetterMarks implements queue.EventQueue: how many entries every subject
// under under holds, and the stream's last sequence, as its LEADER states
// them in one listing of the subjects under under.
func (q *Queue) DeadLetterMarks(ctx context.Context, under string) (queue.Marks, error) {
	letters, err := q.deadLetters(ctx, under)
	if err != nil {
		return queue.Marks{}, err
	}
	marks := queue.Marks{Counts: map[string]uint64{}}
	marks.Last, err = letters.leader.SubjectsSince(ctx, under+".>", func(page map[string]uint64) error {
		maps.Copy(marks.Counts, page)
		return nil
	})
	if err != nil {
		return queue.Marks{}, fmt.Errorf("count the dead letters under %s: %w", under, q.brokerFailed(ctx, err))
	}
	return marks, nil
}

// DropDeadLetter implements queue.EventQueue: the entry seq deleted from the
// dead-letter stream, once its leader says seq holds an entry of subject.
//
// The entry's message id stays in the duplicate window, so a keep of its
// identity inside [queue.DeadLetterWindow] is answered "already kept" while the
// stream runs: in nats-server v2.15.2 deleteMsg and eraseMsg never touch the
// window, which ages an id out only (purgeMsgIds), and every copy, so any new
// leader, records the id of each message it applies. The window is rebuilt
// from stored messages (rebuildDedupe) on recovery at a restart and on
// prepareForWALReplay, and a snapshot catch-up never sends a deleted message
// (processCatchupMsg), so after any of those a keep stores a new entry.
func (q *Queue) DropDeadLetter(ctx context.Context, subject string, seq uint64) error {
	if seq == 0 {
		return fmt.Errorf("jetstream: drop a dead letter on %s: sequence 0 names no entry", subject)
	}
	letters, err := q.deadLetters(ctx, subject)
	if err != nil {
		return err
	}
	gone, err := letters.gone(ctx, q, subject, seq)
	if err != nil || gone {
		return err
	}
	st, err := q.js.Stream(ctx, letters.stream)
	if err != nil {
		return fmt.Errorf("drop dead letter %d on %s: %w", seq, subject, q.brokerFailed(ctx, err))
	}
	deleteErr := st.DeleteMsg(ctx, seq)
	if deleteErr == nil {
		return nil
	}
	// The broker refuses to delete a message already gone: a peer may have
	// dropped it first, the outcome this call asked for. Asked again rather
	// than read off the refusal, which the client folds into one sentinel with
	// every other, and of the leader's copy, the one that refused.
	if gone, err = letters.gone(ctx, q, subject, seq); err == nil && gone {
		return nil
	}
	return fmt.Errorf("drop dead letter %d on %s: %w", seq, subject, q.brokerFailed(ctx, deleteErr))
}

// letterStream is the dead-letter stream as one verb reads it: its name, a
// reader of its leader, how long it keeps an entry, and — where an entry may
// be too large for its leader's reply to reach this node — a reader of its
// copies.
type letterStream struct {
	stream string
	// leader reads the stream's leader: given the largest entry's reply
	// arrival where it reads every entry, and none where copies do, each
	// entry's read then given its own ([letterStream.carrying]).
	leader *jsapi.Leader
	// keep is the stream's own age bound, zero for a stream kept by none.
	keep time.Duration
	// copies reads an entry through a consumer on the leader's own copy
	// where a leaf's link stands between the connection and the leader,
	// and is nil wherever the leader's read reaches it whole
	// ([Queue.leaderCarries]).
	copies *copyRead
}

// deadLetters opens subject's stream for a dead-letter verb: refused outside
// the namespace, provisioned through [Queue.streamFor], and read from its
// leader, and from its copies as well where a leaf's link stops a leader's
// reply as large as the transport ([Queue.leaderCarries]).
func (q *Queue) deadLetters(ctx context.Context, subject string) (letterStream, error) {
	if err := deadLetterSubject(subject); err != nil {
		return letterStream{}, err
	}
	stream, err := q.streamFor(ctx, subject)
	if err != nil {
		return letterStream{}, err
	}
	spec, err := specForSubject(subject, q.cfg.EventRetention)
	if err != nil {
		return letterStream{}, err
	}
	leader, err := jsapi.NewLeader(q.js, stream)
	if err != nil {
		return letterStream{}, fmt.Errorf("jetstream: read the dead letters on %s: %w", subject, err)
	}
	letters := letterStream{stream: stream, leader: leader, keep: spec.maxAge}
	if !q.leaderCarries(int(q.nc.MaxPayload())) {
		// ACROSS A LEAF'S LINK: an entry is found from its headers, and
		// read by its own size ([Queue.DeadLetterFrame]).
		letters.copies = q.copiesOf(stream, leader)
		return letters, nil
	}
	// Every other connection reads every entry from the leader, given the
	// arrival of a reply as large as the transport ([jsapi.Leader.Carrying]):
	// its crossing at the connection's pace across a link no leaf stands on,
	// and on a member's own connection the time to build and decode it. Under
	// load the bare read term re-asked an eleven-megabyte reply while the
	// leader was still building the first, each re-ask another whole reply,
	// until its bound ran out unanswered (crewlet/crewlet#1153).
	letters.leader = leader.Carrying(int(q.nc.MaxPayload()), q.link.pace)
	return letters, nil
}

// kept is the entry stored at seq at stored, with headers, as the contract
// lists it: the keeper's header where it kept one, and nothing of its payload.
func (s letterStream) kept(seq uint64, stored time.Time, headers nats.Header) queue.Kept {
	k := queue.Kept{Seq: seq, StoredAt: stored.UTC()}
	if s.keep > 0 {
		k.Expires = k.StoredAt.Add(s.keep)
	}
	if header := headers.Get(letterHeader); header != "" {
		k.Header = []byte(header)
	}
	return k
}

// gone reports whether seq holds no entry any more, and refuses a seq whose
// entry is another subject's — across a leaf's link from the entry's headers
// alone, through a consumer on the leader's own copy ([copyRead.at]).
func (s letterStream) gone(ctx context.Context, q *Queue, subject string, seq uint64) (bool, error) {
	if s.copies != nil {
		at, err := s.copies.at(ctx, seq)
		switch {
		case err != nil:
			return false, fmt.Errorf("read dead letter %d on %s: %w", seq, subject, q.brokerFailed(ctx, err))
		case at.held && at.subject != subject:
			return false, fmt.Errorf("jetstream: dead letter %d is on %s, not %s; nothing was dropped",
				seq, at.subject, subject)
		}
		return !at.held, nil
	}
	msg, err := s.leader.At(ctx, seq)
	switch {
	case errors.Is(err, jsapi.ErrNoMessage):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("read dead letter %d on %s: %w", seq, subject, q.brokerFailed(ctx, err))
	case msg.Subject != subject:
		return false, fmt.Errorf("jetstream: dead letter %d is on %s, not %s; nothing was dropped",
			seq, msg.Subject, subject)
	}
	return false, nil
}

// deadLetterSubject reports why subject cannot hold a dead letter, or nil when
// it can: a literal subject inside the dead-letter namespace. The contract's
// rule ([queue.EventQueue.KeepDeadLetter]); the memory twin states the same one
// and the conformance suite holds the two to one answer.
func deadLetterSubject(subject string) error {
	if !strings.HasPrefix(subject, topics.DeadLetterPrefix) {
		return fmt.Errorf("jetstream: %q is not a dead-letter subject (topics.DeadLetter names one)", subject)
	}
	return queue.LiteralSubject(subject)
}
