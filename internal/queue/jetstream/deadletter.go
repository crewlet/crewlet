package jetstream

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// The dead-letter verbs, over the one dead-letter stream every subscription's
// budget already writes to: `CREWLET_DLQ`, kept by age — the event stream's own
// bound, `stream.event_retention_hours` — and never by interest, so an entry is
// there whether or not anybody reads it until the age lets it go.
//
// # Read from the stream's LEADER, one entry at a time
//
// A listing is read through [jsapi.Leader] — a `STREAM.MSG.GET` per entry,
// asked of the leader and asked again while nobody answers — rather than
// through a throwaway consumer ([Queue.peek]) or a direct get. A direct get is
// answered by any replica, one that is behind included, and the reader of this
// listing acts on an ABSENCE: it counts an entry that vanished between two
// listings as let go by the retention, so a replica that had not heard of an
// entry would report it expired. A consumer is a metadata proposal per
// listing, and the listing runs on every alarm beat of every data node. One
// leader read per entry costs a round trip each instead, which a dead-letter
// subject affords: a healthy fleet holds no entry at all, and the first one
// raises an alarm.

// KeepDeadLetter implements queue.EventQueue: body stored on subject in the
// dead-letter stream, the broker's acknowledgement the proof it is durable.
func (q *Queue) KeepDeadLetter(ctx context.Context, subject string, body []byte) error {
	if err := deadLetterSubject(subject); err != nil {
		return err
	}
	stream, err := q.streamFor(ctx, subject)
	if err != nil {
		return err
	}
	// EXPECTED ON THE STREAM IT WAS RESOLVED TO, so a broker whose streams
	// were arranged differently — a namespace another stream also claims —
	// refuses the entry rather than storing it where no listing reads.
	if _, err := q.js.Publish(ctx, subject, body, jetstream.WithExpectStream(stream)); err != nil {
		// TRANSLATED, for Publish's reason: too large is the one refusal
		// no retry changes, and nats.ErrMaxPayload is this backend's
		// private word for it.
		if errors.Is(err, nats.ErrMaxPayload) {
			return fmt.Errorf("%s: %w: %w", tooLarge(q.nc, "keep a dead letter on", subject, len(body)),
				queue.ErrTooLarge, err)
		}
		return fmt.Errorf("keep a dead letter on %s: %w", subject, q.brokerFailed(ctx, err))
	}
	return nil
}

// DeadLettersOn implements queue.EventQueue: every entry subject holds, oldest
// first, read from the stream's leader.
func (q *Queue) DeadLettersOn(ctx context.Context, subject string) ([]queue.Kept, error) {
	letters, err := q.deadLetters(ctx, subject)
	if err != nil {
		return nil, err
	}
	var out []queue.Kept
	for from := uint64(0); ; {
		msg, err := letters.leader.Next(ctx, subject, from)
		if errors.Is(err, jsapi.ErrNoMessage) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list the dead letters on %s: %w", subject, q.brokerFailed(ctx, err))
		}
		out = append(out, letters.kept(msg))
		from = msg.Sequence + 1
	}
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

// kept is one stored message as the contract lists it.
func (s letterStream) kept(msg jsapi.Message) queue.Kept {
	k := queue.Kept{Seq: msg.Sequence, StoredAt: msg.Time.UTC(), Body: msg.Data}
	if s.keep > 0 {
		k.Expires = k.StoredAt.Add(s.keep)
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
