package jetstream

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"
)

// DomainLog is one state-log domain's append surface: a conditional publish
// that returns the acknowledgement the ordinary publish path discards, and
// the last-message query a rejection is discriminated with.
//
// # Why this is not Queue.Publish
//
// [Queue.Publish] throws the PubAck away, and for its own traffic that is
// right: an event's sequence is nobody's business. For a state log the
// sequence inside it is the object's new version, the applier's checkpoint
// and the next writer's expectation, all at once — so the framework
// publishes through here instead, on the same connection and with the same
// credentials.
type DomainLog struct {
	js     jetstream.JetStream
	stream jetstream.Stream
	name   string
}

// DomainLog opens the append surface for a stream this node has provisioned.
//
// It resolves the stream ONCE, at open, rather than per call: the
// last-message query needs a stream handle, and looking one up on every
// rejection would put an extra round trip on the path that is already the
// slowest one the write authority has.
func (q *Queue) DomainLog(ctx context.Context, stream string) (*DomainLog, error) {
	s, err := q.js.Stream(ctx, stream)
	if err != nil {
		return nil, fmt.Errorf("jetstream: open the log %q: %w", stream, err)
	}
	return &DomainLog{js: q.js, stream: s, name: stream}, nil
}

// Append publishes one record.
//
// expect is the per-subject last sequence the append is conditional on, and
// NIL means no expectation header at all — which is correct only where the
// records commute. A pointer rather than a sentinel because zero is a real
// expectation with a specific meaning, "this subject holds nothing", and it
// is the one branch where being wrong costs a silent lost update.
//
// msgID empty sends no message id, which the read barrier requires: a
// repeated id inside the duplicate window is served out of the window with no
// quorum round trip, so an acknowledgement carrying one would prove nothing
// about the log's end.
func (l *DomainLog) Append(ctx context.Context, subject, msgID string, expect *uint64, body []byte) (uint64, bool, error) {
	opts := make([]jetstream.PublishOpt, 0, 2)
	if msgID != "" {
		opts = append(opts, jetstream.WithMsgID(msgID))
	}
	if expect != nil {
		opts = append(opts, jetstream.WithExpectLastSequencePerSubject(*expect))
	}
	ack, err := l.js.Publish(ctx, subject, body, opts...)
	if err != nil {
		return 0, false, err
	}
	return ack.Sequence, ack.Duplicate, nil
}

// LastSeq answers the last sequence on a subject, reporting false with a nil
// error when the subject holds no message.
//
// "No message here" is a FACT and not a failure: it is what tells a trimmed
// anchor from a lost race, and the two have opposite remedies. Returning the
// broker's not-found as an error would make every caller re-derive that
// distinction, and the first one to forget it would take the trimmed-anchor
// branch on a broker that was merely unreachable.
func (l *DomainLog) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
	msg, err := l.stream.GetLastMsgForSubject(ctx, subject)
	if err != nil {
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("jetstream: last message on %q: %w", subject, err)
	}
	return msg.Sequence, true, nil
}
