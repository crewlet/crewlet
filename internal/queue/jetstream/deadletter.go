package jetstream

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
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
//   - A FRAME ([Queue.DeadLetterFrame]) is the leader's message read by
//     sequence, `STREAM.MSG.GET` ([jsapi.Leader]): the one read that moves a
//     payload, made by the one reader that acts on it — a feed's replay — and
//     asked of the leader, so an entry is never reported gone because a
//     replica had not heard of it.
//
// A DROP ([Queue.DropDeadLetter]) asks the leader which subject the sequence
// is on before it deletes it — the same message read, so it moves the payload
// of the one entry it removes: the pinned server has no read of a message's
// subject without it (above), and a consumer's would answer from a replica,
// where an entry not yet heard of reads as already gone and the drop would
// answer done having removed nothing.

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

// letterQuiet is how long one pull of a listing waits for its entries before
// the listing asks its consumer what it still holds.
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
// proof it is durable.
func (q *Queue) KeepDeadLetter(ctx context.Context, subject string, header, body []byte) error {
	if err := deadLetterSubject(subject); err != nil {
		return err
	}
	if err := queue.LetterHeader(header); err != nil {
		return fmt.Errorf("jetstream: keep a dead letter on %s: %w", subject, err)
	}
	// THE CONTRACT'S CEILING FOR A KEEP, before the transport's: the twin
	// refuses exactly these, so the two never disagree about an entry whose
	// header block would take it past the transport ([queue.LetterFraming]).
	if size := len(header) + len(body); size+queue.LetterFraming > queue.MaxPayloadBytes {
		return fmt.Errorf("keep a dead letter on %s: %d bytes of header and body exceed the "+
			"%d-byte limit less the %d a keep is framed in: %w", subject, size,
			queue.MaxPayloadBytes, queue.LetterFraming, queue.ErrTooLarge)
	}
	stream, err := q.streamFor(ctx, subject)
	if err != nil {
		return err
	}
	// WAITED FOR AS LONG AS ITS BYTES TAKE TO CROSS, where its caller set no
	// deadline: the request term, and across a link the time the entry takes
	// at the slowest pace allowed ([jsapi.WriteBound]) — counted as the
	// contract bounds it, its header and body and the framing a keep is held
	// to beside them ([queue.LetterFraming]). An entry at the ceiling is
	// about four minutes at that pace, and held to the request term alone it
	// failed as an outage its keeper retried for ever while the link carried
	// each copy. Classified on the CALLER's context, so a bound this set and
	// the broker outlasted reads as the silence it is.
	sending, stop := jsapi.Writing(ctx, q.js, len(header)+len(body)+queue.LetterFraming, q.linkPace())
	defer stop()
	if _, err := q.js.PublishMsg(sending, letterMsg(subject, stream, header, body)); err != nil {
		// TRANSLATED, for Publish's reason: too large is the one refusal
		// no retry changes, and nats.ErrMaxPayload is this backend's
		// private word for it — met here only on a server announcing less
		// than the contract.
		if errors.Is(err, nats.ErrMaxPayload) {
			return fmt.Errorf("%s: %w: %w", tooLarge(q.nc, "keep a dead letter on", subject,
				len(header)+len(body)), queue.ErrTooLarge, err)
		}
		return fmt.Errorf("keep a dead letter on %s: %w", subject, q.brokerFailed(ctx, err))
	}
	return nil
}

// letterMsg is the message a keep publishes: body as its payload, header in
// [letterHeader] where there is one, and the stream it was resolved to as the
// one the broker must store it in.
//
// EXPECTED ON THE STREAM IT WAS RESOLVED TO (`Nats-Expected-Stream`, the
// header jetstream.WithExpectStream writes), so a broker whose streams were
// arranged differently — a namespace another stream also claims — refuses the
// entry rather than storing it where no listing reads. Written here rather than
// through the option so the message this returns is the whole of what crosses
// the wire, which the framing's test measures against [queue.LetterFraming].
func letterMsg(subject, stream string, header, body []byte) *nats.Msg {
	msg := &nats.Msg{Subject: subject, Data: body, Header: nats.Header{}}
	msg.Header.Set(jetstream.ExpectedStreamHeader, stream)
	if len(header) > 0 {
		msg.Header.Set(letterHeader, string(header))
	}
	return msg
}

// DeadLettersOn implements queue.EventQueue: every entry subject holds, oldest
// first, by its header alone — read through a consumer of its own that
// delivers no payload, from the replica the broker places it on.
func (q *Queue) DeadLettersOn(ctx context.Context, subject string) ([]queue.Kept, error) {
	letters, err := q.deadLetters(ctx, subject)
	if err != nil {
		return nil, err
	}
	q.noteConsumerProposal()
	cons, err := q.js.CreateConsumer(ctx, letters.stream, jetstream.ConsumerConfig{
		Description:   "a listing of the dead letters on " + subject + ", headers only",
		FilterSubject: subject,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		AckPolicy:     jetstream.AckNonePolicy,
		// NO PAYLOAD CROSSES: the broker delivers each entry's headers,
		// the keeper's among them, and the payload's size in place of the
		// payload.
		HeadersOnly: true,
		// A READER'S THROWAWAY, as a peek's is: one replica, in memory,
		// reaped if this listing dies before it deletes it.
		Replicas:          1,
		MemoryStorage:     true,
		InactiveThreshold: peekBackstop,
	})
	if err != nil {
		return nil, fmt.Errorf("list the dead letters on %s: %w", subject, q.brokerFailed(ctx, err))
	}
	defer func() {
		// WithoutCancel, for peek's reason: a cleanup that inherits the
		// cancellation it is cleaning up after does nothing at all.
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), peekCleanup)
		defer stop()
		_ = q.js.DeleteConsumer(cleanup, letters.stream, cons.CachedInfo().Name)
	}()
	kept, err := letters.headers(ctx, cons)
	if err != nil {
		return nil, fmt.Errorf("list the dead letters on %s: %w", subject, q.brokerFailed(ctx, err))
	}
	return kept, nil
}

// headers is every entry cons delivers, in order, until it says nothing is
// pending — read a pull of [letterPage] at a time, each waited for no longer
// than [letterQuiet].
//
// THE CONSUMER'S OWN COUNT DECIDES THE END, never a quiet pull: a pull that
// brought nothing asks the consumer, which ends the listing where it holds
// nothing more and has delivered nothing this listing missed, and pulls again
// where it still holds entries — for as long as the server keeps a silence
// ([jsprovision.LookupBudget]), past which the listing fails rather than
// answer short. A delivery the consumer counted and this listing never
// received — a gap in its delivery count — fails the listing too: an entry
// missing from a listing reads to its reader as one that is gone.
func (s letterStream) headers(ctx context.Context, cons jetstream.Consumer) ([]queue.Kept, error) {
	pending := cons.CachedInfo().NumPending
	var (
		out []queue.Kept
		// delivered is the consumer's count of the last delivery taken,
		// and heard when one last arrived.
		delivered uint64
		heard     = time.Now()
	)
	for pending > 0 {
		// A PULL IS BOUNDED BY ITS OWN WAIT, never the caller's context —
		// the client takes one or the other (nats.go v1.55.0,
		// jetstream/pull.go:889-891) — so the caller's is asked before
		// each: a listing whose caller went stops within one quiet pull.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := cons.Fetch(int(min(pending, letterPage)), jetstream.FetchMaxWait(letterQuiet))
		if err != nil {
			return nil, err
		}
		got := 0
		for msg := range batch.Messages() {
			meta, metaErr := msg.Metadata()
			if metaErr != nil {
				return nil, fmt.Errorf("an entry was delivered with no position this build "+
					"can read: %w", metaErr)
			}
			if meta.Sequence.Consumer != delivered+1 {
				return nil, fmt.Errorf("the listing's consumer counted delivery %d after %d: "+
					"an entry it delivered never arrived", meta.Sequence.Consumer, delivered)
			}
			delivered = meta.Sequence.Consumer
			out = append(out, s.kept(meta.Sequence.Stream, meta.Timestamp, msg.Headers()))
			got++
			if pending = meta.NumPending; pending == 0 {
				break
			}
		}
		if err = batch.Error(); err != nil {
			return nil, err
		}
		if got > 0 {
			heard = time.Now()
			continue
		}
		info, err := cons.Info(ctx)
		if err != nil {
			return nil, err
		}
		if info.Delivered.Consumer != delivered {
			return nil, fmt.Errorf("the listing's consumer delivered %d entries and %d arrived",
				info.Delivered.Consumer, delivered)
		}
		if pending = info.NumPending; pending > 0 && time.Since(heard) >= jsprovision.LookupBudget {
			return nil, fmt.Errorf("the listing's consumer delivered nothing for %v while it "+
				"held %d entries", jsprovision.LookupBudget, pending)
		}
	}
	return out, nil
}

// DeadLetterFrame implements queue.EventQueue: the payload of the entry seq,
// read from the stream's leader, once the leader says seq holds an entry of
// subject.
func (q *Queue) DeadLetterFrame(ctx context.Context, subject string, seq uint64) ([]byte, bool, error) {
	if seq == 0 {
		return nil, false, fmt.Errorf("jetstream: read a dead letter on %s: sequence 0 names no entry", subject)
	}
	letters, err := q.deadLetters(ctx, subject)
	if err != nil {
		return nil, false, err
	}
	msg, err := letters.leader.At(ctx, seq)
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
	// Asked again of the leader rather than read off the refusal's words,
	// which the client folds into one sentinel with every other refusal.
	if gone, err = letters.gone(ctx, q, subject, seq); err == nil && gone {
		return nil
	}
	return fmt.Errorf("drop dead letter %d on %s: %w", seq, subject, q.brokerFailed(ctx, deleteErr))
}

// letterStream is the dead-letter stream as one verb reads it: its name, a
// reader of its leader and how long it keeps an entry.
type letterStream struct {
	stream string
	leader *jsapi.Leader
	// keep is the stream's own age bound, zero for a stream kept by none.
	keep time.Duration
}

// deadLetters opens subject's stream for a dead-letter verb: refused for a
// subject outside the namespace, provisioned through the one door every verb
// passes ([Queue.streamFor]), and read from its leader with a term that covers
// an entry as large as the transport carries where the connection crosses a
// link ([Queue.recordCrossing]'s reason).
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
	if q.acrossLink() {
		leader = leader.Carrying(int(q.nc.MaxPayload()), objstore.MiBPace)
	}
	return letterStream{stream: stream, leader: leader, keep: spec.maxAge}, nil
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
// entry is another subject's.
func (s letterStream) gone(ctx context.Context, q *Queue, subject string, seq uint64) (bool, error) {
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
