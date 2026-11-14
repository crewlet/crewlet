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

// The dead-letter verbs, over the one dead-letter stream every subscription's
// budget already writes to: `CREWLET_DLQ`, kept by age — the event stream's own
// bound, `stream.event_retention_hours` — and never by interest, so an entry is
// there whether or not anybody reads it until the age lets it go.
//
// # An entry is a message whose header says what it is
//
// The keeper's header rides as one header of the message, [letterHeader], and
// its body is the message's payload. That split is what lets every reader that
// only needs to know WHAT is kept read no payload at all: a wake feed's entry
// is a record's whole sealed frame, as large as the transport carries, and the
// alarm's reading lists the feeds' subjects on the beat of every data node.
// Read whole, a stalled roll that dead-lettered thousands of records would have
// had each data node read every frame of them every beat.
//
// # Three reads, each the cheapest that is honest for its reader
//
//   - A LISTING ([Queue.DeadLettersOn]) is one ephemeral consumer that
//     delivers HEADERS ONLY — the broker strips the payload and states its
//     size instead (nats-server v2.15.2, server/consumer.go:5508-5512 and
//     5774-5798, `convertToHeadersOnly`) — filtered to the subject, read
//     until it says nothing is pending, and deleted. The pinned server has no
//     headers-only read of a single message: the leader's message read
//     offers `no_hdr` alone (server/jetstream_api.go:825-847,
//     JSApiMsgGetRequest), the opposite, and a direct get always sends the
//     payload (server/stream.go:6439). So a listing reads the copy the
//     broker places its consumer on, one replica of the stream chosen among
//     those it counts as up (server/jetstream_cluster.go:12339-12387,
//     createGroupForConsumer), which may be behind the stream's leader: it
//     can miss an entry kept a moment ago or still show one dropped a moment
//     ago, and it never hides one the replica holds.
//   - THE MARKS ([Queue.DeadLetterMarks]) are the LEADER's: one
//     `STREAM.INFO` with a subjects filter, which only the leader answers
//     ([jsapi.Leader.SubjectsSince]) and which counts each subject and states
//     the stream's last sequence. A reader that must know its listing is
//     current holds it to them — the alarm's reading lists again only when
//     they moved, and takes a listing as current only when its counts are
//     the marks' (internal/engine's deadLetterWatch).
//   - A FRAME ([Queue.DeadLetterFrame]) is the one read that moves a payload,
//     made by the one reader that acts on it — a feed's replay — and never
//     reports an entry gone because a replica had not heard of it. It is the
//     leader's message read by sequence, `STREAM.MSG.GET` ([jsapi.Leader]),
//     wherever that read's reply reaches this node: on a member's own
//     connection, and across a link no leaf stands on, given there the time
//     the largest entry's reply takes to cross. ACROSS A LEAF'S LINK an entry
//     may be too large for that reply to cross it — the reply is JSON, the
//     entry base64-encoded inside it, and a leaf's server closes its whole
//     link on a message past the transport — so it is found from its headers
//     first, through a consumer on the leader's own copy ([copyRead.at]),
//     read with the leader's message read where its own reply crosses the
//     link, and otherwise read RAW through such a consumer
//     ([copyRead.message]): the leader's answer either way, as copyread.go
//     says.
//
// # One entry for one identity
//
// A keep names the identity its keeper derives from what it keeps
// ([queue.EventQueue.KeepDeadLetter]), and the message carries it as its
// `Nats-Msg-Id` ([letterID]): the stream's duplicate window is
// [queue.DeadLetterWindow], so the broker stores a second keep of one identity
// within it as nothing and acknowledges it as a duplicate, which is the
// contract's "already kept". The window is the stream's, set where the stream
// is declared (engineStreams), and a retention shorter than it is one the
// broker refuses the stream for (Tier A refuses it first).
//
// THE DELIVERY BUDGET'S DEAD LETTER NAMES ONE TOO ([Queue.deadLetter]), on
// either of its pair's subjects, poison's or the unhandled deliveries'
// ([queue.RetiredTo]): the message the broker stored, its stream, its sequence
// and its instant ([letterOf]). A message whose keep failed goes back for its
// next delivery to keep — the broker's backstop stands past the budget for
// that ([keepRetries]) — and a keep that stored its entry and lost its answer
// failed as far as its keeper heard, so the identity is what makes the keep
// made again come to nothing. A copy too large for the identity's header
// beside it is kept with none, and twice at worst.
//
// A DROP ([Queue.DropDeadLetter]) asks which subject the sequence is on before
// it deletes it, and the delete is the leader's. Wherever the leader's read
// reaches this node the check is that read, so it moves the payload of the
// one entry it removes: the pinned server has no read of a message's subject
// without it (above). ACROSS A LEAF'S LINK it reads the entry's HEADERS
// ALONE, through a consumer on the leader's own copy ([copyRead.at]) — never a
// replica's, where an entry not yet heard of would read as already gone and
// the drop answer done having removed nothing.

// letterHeader is the header a dead letter's keeper's header rides in, beside
// the stream the keep expects (`Nats-Expected-Stream`).
const letterHeader = "Crewlet-Dead-Letter"

// letterPage is how many entries one pull of a listing asks for.
//
// FIVE HUNDRED, the client's own default, stated rather than inherited: what a
// listing holds is headers — a few hundred bytes an entry, under three
// kibibytes for a wake feed's at its worst — so a full pull is a few hundred
// kibibytes however large the frames behind them, and the thousand entries a
// stalled roll can leave on one subject take two.
const letterPage = 500

// letterQuiet is how long a reader of a throwaway consumer — a listing, or a
// copy read ([copyRead]) — hears nothing from its pull before it asks the
// consumer what it says of itself ([pulling.quiet]); a pull the consumer
// says it is answering, a whole message crossing a link among them, is
// waited on past it.
//
// ONE SECOND, internal/objstore/natsobj's quietWait, for its reason: a cluster
// drops a new consumer's FIRST PULL routinely — the member that made the
// consumer answers its creation before the reader's member hears that the
// consumer's member serves its pulls, and a pull sent in that gap goes nowhere
// — measured there at up to one read in twenty-five on a loaded three-member
// fleet. A listing that waited out a long pull for nothing would wait for
// every one of those. Asked after a quiet second instead, the consumer says it
// still holds its entries and delivered none, and the next pull reaches it.
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
	// THE CONTRACT'S CEILING FOR A KEEP, before the transport's: the twin
	// refuses exactly these, so the two never disagree about an entry whose
	// header block would take it past the transport ([queue.LetterFits]).
	if !queue.LetterFits(header, body) {
		return false, fmt.Errorf("keep a dead letter on %s: %d bytes of header and body exceed the "+
			"%d-byte limit less the %d a keep is framed in: %w", subject, len(header)+len(body),
			queue.MaxPayloadBytes, queue.LetterFraming, queue.ErrTooLarge)
	}
	stream, err := q.streamFor(ctx, subject)
	if err != nil {
		return false, err
	}
	// WAITED FOR AS LONG AS ITS BYTES TAKE TO CROSS, where its caller set no
	// deadline: the request term, and across a link the time the entry takes
	// at the connection's pace, each time it is sent ([Queue.write]) —
	// counted off the message as it is sent, its header block and its body
	// ([letterMsg]). An entry at the ceiling is about four minutes at the
	// slowest pace allowed, and held to the request term alone it failed as an
	// outage its keeper retried for ever while the link carried each copy. For
	// those minutes everything the node sends behind it on the link waits too,
	// its lease renewals among them (crewlet/crewlet#1035, which this does not
	// fix). Classified on the CALLER's context, so a budget this set and the
	// broker outlasted reads as the silence it is.
	ack, err := q.write(ctx, letterMsg(subject, stream, id, header, body))
	if err != nil {
		// TRANSLATED, for Publish's reason: too large is the one refusal
		// no retry changes, and nats.ErrMaxPayload is this backend's
		// private word for it — met here only on a server announcing less
		// than the contract.
		if errors.Is(err, nats.ErrMaxPayload) {
			return false, fmt.Errorf("%s: %w: %w", tooLarge(q.nc, "keep a dead letter on", subject,
				len(header)+len(body)), queue.ErrTooLarge, err)
		}
		return false, fmt.Errorf("keep a dead letter on %s: %w", subject, q.brokerFailed(ctx, err))
	}
	return ack.Duplicate, nil
}

// letterMsg is the message a keep of id publishes: body as its payload, header
// in [letterHeader] where there is one, the identity as its message id
// ([letterID]), and the stream it was resolved to as the one the broker must
// store it in.
//
// EXPECTED ON THE STREAM IT WAS RESOLVED TO (`Nats-Expected-Stream`, the
// header jetstream.WithExpectStream writes), so a broker whose streams were
// arranged differently — a namespace another stream also claims — refuses the
// entry rather than storing it where no listing reads. Written here rather than
// through the options, the message id's as much as the stream's, so the
// message this returns is the whole of what crosses the wire, which the
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
// SCOPED TO ITS SUBJECT, because every dead-letter subject is one stream and
// the broker collapses a repeated id across the whole of it — two keepers'
// things that share an identity on two subjects are two entries — and
// DIGESTED, so the header block a keep is framed in stays inside
// [queue.LetterFraming] whatever the identity's length, and no byte of an
// identity reaches a header the transport would rewrite ([queue.LetterHeader]
// says what it rewrites). A subject holds no NUL, so the pair it digests is
// one pair.
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
		// NO PAYLOAD CROSSES: the broker delivers each entry's headers,
		// the keeper's among them, and the payload's size in place of the
		// payload.
		HeadersOnly: true,
		// A READER'S THROWAWAY, as a peek's is: one replica, in memory.
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
// pending — asked a pull of [letterPage] at a time on one inbox the listing
// holds throughout ([pulling]), each waited for no longer than [letterQuiet],
// so an entry delivered to a pull that has expired still reaches the
// listing, in its place: the consumer's delivery count guards every entry,
// and one missing from a listing reads to its reader as one that is gone.
//
// THE CONSUMER'S OWN COUNT DECIDES THE END, never a quiet pull: a pull that
// brought nothing asks the consumer ([pulling.quiet]), which ends the listing
// where it holds nothing more and has delivered nothing this listing has not
// had, and is pulled again where it still holds entries or has delivered some
// still on their way — for as long as the server keeps a silence
// ([jsprovision.LookupBudget]), past which the listing fails rather than
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
	defer p.stop()
	var (
		out []queue.Kept
		// heard is when an entry last arrived.
		heard = time.Now()
	)
	for pending > 0 {
		asked := int(min(pending, letterPage))
		if err := p.ask(asked, letterQuiet, 0); err != nil {
			return nil, err
		}
		took := 0
		for took < asked && pending > 0 {
			d, err := p.within(ctx, letterQuiet)
			if errors.Is(err, errQuiet) || errors.Is(err, errExpired) {
				break
			}
			if err != nil {
				return nil, err
			}
			out = append(out, s.kept(d.meta.Sequence.Stream, d.meta.Timestamp, d.msg.Header))
			pending = d.meta.NumPending
			took++
		}
		if took > 0 {
			heard = time.Now()
			continue
		}
		says, info, err := p.quiet(ctx)
		switch {
		case err != nil:
			return nil, err
		case says == drained:
			return out, nil
		}
		if pending = p.owed(info); time.Since(heard) >= jsprovision.LookupBudget {
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
// THE DELETE LEAVES THE ENTRY'S MESSAGE ID IN THE STREAM'S DUPLICATE WINDOW,
// so a keep of its identity inside [queue.DeadLetterWindow] is answered a
// duplicate — already kept — while the stream runs: in nats-server v2.15.2 a
// delete (stream.go's deleteMsg, and eraseMsg, through which every copy
// applies a clustered one) removes the message from the store and never
// touches the window, which lets an id go only by its age (purgeMsgIds), and
// a leader elected since holds the id too, since every copy records the id of
// each message it applies. THE WINDOW IS REBUILT FROM THE MESSAGES STILL
// STORED (rebuildDedupe) when the stream is recovered at a restart and when it
// is reset to replay its log (prepareForWALReplay), and a copy caught up from
// a snapshot is never sent a deleted message (processCatchupMsg records the id
// of each message it is sent), so after any of those the identity is
// forgotten and a keep of it stores a new entry.
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
	// A PEER THAT DROPPED IT FIRST: the broker refuses to delete a message
	// that is no longer there, and that is the outcome this call asked for.
	// Asked again rather than read off the refusal's words, which the client
	// folds into one sentinel with every other refusal — of the leader's own
	// copy either way, the one that refused the delete.
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
	// leader reads the stream's leader: given the arrival of the largest
	// entry's reply wherever the leader's read reaches every entry, and none
	// where it reads through copies, whose read gives each entry its own
	// ([letterStream.carrying]).
	leader *jsapi.Leader
	// keep is the stream's own age bound, zero for a stream kept by none.
	keep time.Duration
	// copies reads an entry through a consumer on the leader's own copy
	// where a leaf's link stands between the connection and the leader,
	// and is nil wherever the leader's read reaches it whole
	// ([Queue.leaderCarries]).
	copies *copyRead
}

// deadLetters opens subject's stream for a dead-letter verb: refused for a
// subject outside the namespace, provisioned through the one door every verb
// passes ([Queue.streamFor]), and read from its leader — and, where a leaf's
// link stands between the connection and the leader, past which a leader's
// reply of an entry as large as the transport carries cannot reach this node
// ([Queue.leaderCarries]), from its copies as well.
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
	// EVERY OTHER CONNECTION reads every entry from the leader, given the
	// arrival of a reply as large as the transport carries
	// ([jsapi.Leader.Carrying]): its handling in the engine's own processes
	// on a member's own connection, which crosses nothing, and its crossing at
	// the connection's pace besides across a link no leaf stands on — a dial
	// to a broker somebody else runs, where nothing holds the reply to the
	// transport. On a member's own connection the reply was given the bare
	// read term, and under load an entry at the ceiling — its reply eleven
	// megabytes, built by the leader and decoded here, in this process — was
	// asked again at two seconds and at four while the leader was still
	// building the first, every re-ask another whole reply, and ran out its
	// bound with none of them answered (crewlet/crewlet#1153).
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
