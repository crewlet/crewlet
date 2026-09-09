package jetstream

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

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

	// q is the queue this log was opened on, held for the ONE thing a
	// bare JetStream context cannot do: ensure a durable consumer in a
	// way that survives a peer creating the same name at the same instant.
	// See [Queue.ensureDurableConsumer].
	q *Queue

	// state is a SECOND handle, used by [DomainLog.Stats] and by nothing
	// else, and stateMu serialises the callers that share it.
	//
	// # Why the state read cannot go through the handle above
	//
	// The vendored stream handle is not safe for concurrent use, and the
	// shared mutable state is exactly one field: `Info` caches its reply
	// into `s.info` with no lock (stream.go:498), and `getMsg` — which is
	// what `GetMsg` and `GetLastMsgForSubject` are — READS `s.info.Config`
	// on the same handle (stream.go:577). So one goroutine asking for the
	// log's size races every goroutine reading a record by sequence or by
	// subject, on a DomainLog that is shared by design: the applier's
	// readiness check, the publisher's expectation read and the retention
	// tick all hold this one.
	//
	// A SECOND HANDLE RATHER THAN ONE MUTEX OVER ALL OF IT, because the
	// callers are not alike. `LastSeq` is on the WRITE path — every
	// conditional append that loses its expectation reads it — and
	// serialising that behind a state read would put a round trip's
	// latency on a hot path to protect a field it never touches. Nothing
	// calls `Info` on the shared handle, so its `s.info` is written once
	// at open and read immutably thereafter.
	//
	// The mutex is still needed HERE, because two state reads share this
	// handle. Its callers are the readiness check, the trim tick and the
	// operator report — none of them hot.
	state   jetstream.Stream
	stateMu sync.Mutex
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
	// THE SECOND HANDLE, opened here rather than lazily: a lazy one would
	// need its own lock to build, and the whole point of the split is that
	// the shared handle never takes one.
	state, err := q.js.Stream(ctx, stream)
	if err != nil {
		return nil, fmt.Errorf("jetstream: open the log %q's state reader: %w",
			stream, err)
	}
	return &DomainLog{js: q.js, stream: s, state: state, name: stream, q: q}, nil
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

// End is the stream's own last sequence, across every subject.
//
// SEPARATE FROM [DomainLog.LastSeq], which is per subject, because the two
// answer different questions: a writer forming an expectation wants its own
// subject's last message, and a reader establishing how far the log goes wants
// the stream's. Reading one for the other is how a lag figure comes back as
// zero on a busy company because the subject a node happened to ask about is
// quiet.
func (l *DomainLog) End(ctx context.Context) (uint64, error) {
	_, last, err := l.Bounds(ctx)
	return last, err
}

// Bounds is BOTH ends of the log: the first sequence still held and the last
// one written.
//
// # Why the first end is not a detail
//
// A consumer whose position falls below the stream's first sequence is CLAMPED
// UPWARD by the server with no error at all, and then reports itself perfectly
// caught up — over a hole. The only thing that can notice is a comparison
// against this number, so a readiness check that read only the end would call
// such a node established for ever.
//
// ONE ROUND TRIP for both, because they are one answer: read separately, a
// caller can pair a first sequence from before a trim with a last sequence
// from after it and compute a window neither describes.
func (l *DomainLog) Bounds(ctx context.Context) (first, last uint64, err error) {
	// THROUGH [DomainLog.Stats], so the pairing rule above has one
	// implementation: a second read of the same stream info is a second
	// place for a caller to end up holding two ends from two instants.
	stats, err := l.Stats(ctx)
	if err != nil {
		return 0, 0, err
	}
	return stats.FirstSeq, stats.LastSeq, nil
}

// At reads one record back by sequence.
//
// # Why a domain needs this at all
//
// Almost nothing reads a log by position: a consumer is how records are
// delivered, and a consumer is what the apply loop uses. What needs this is
// the small set of paths that address the log directly rather than following
// it — a reanchor establishing what the old stream's head was, a snapshot's
// verification, and any tool that has a position and needs the record at it.
//
// It reports (false, nil) for a sequence the stream no longer holds, which is
// an ordinary answer rather than an error: a trimmed record is exactly what
// the retention gate exists to produce, and a caller distinguishing "gone" from
// "the broker is unreachable" is the whole reason it is not one error value.
func (l *DomainLog) At(ctx context.Context, seq uint64) (subject string,
	payload []byte, storedAt time.Time, ok bool, err error) {

	msg, err := l.stream.GetMsg(ctx, seq)
	switch {
	case errors.Is(err, jetstream.ErrMsgNotFound):
		return "", nil, time.Time{}, false, nil
	case err != nil:
		return "", nil, time.Time{}, false,
			fmt.Errorf("jetstream: read %q at %d: %w", l.name, seq, err)
	}
	return msg.Subject, msg.Data, msg.Time, true, nil
}

// LogStats is everything the retention gate reads about a log, in ONE answer.
//
// # Why it is one call and not four
//
// [DomainLog.Bounds] already gives the reason for two of them, and the trim
// needs four: it divides bytes by the ceiling to publish headroom, compares
// the first sequence against what it is about to remove, and reports the last
// one as the fleet's head. Read separately, a tick can pair a byte count from
// before a purge with a first sequence from after it and publish a headroom
// figure describing no state the stream was ever in — which is the number an
// operator watches approach a threshold.
type LogStats struct {
	// FirstSeq and LastSeq are the log's two ends.
	FirstSeq, LastSeq uint64

	// Messages is how many records survive between them, which is not
	// LastSeq − FirstSeq once anything has been purged.
	Messages uint64

	// Bytes is what the stream holds and MaxBytes the ceiling the BROKER
	// is enforcing — read from the stream's own configuration rather than
	// from the Tier A field, because Tier A is per node and takes effect
	// at restart: between an edit and a restart the field names a ceiling
	// nothing is applying, and this is the number an operator divides by.
	//
	// MaxBytes is zero when the stream declares no ceiling, which is a
	// real setting rather than a full log.
	Bytes, MaxBytes uint64

	// CreatedAt is the broker's own creation instant for the stream, which
	// is what detects a recreated one.
	CreatedAt time.Time
}

// Stats reads the log's ends, its size and its ceiling in one round trip.
func (l *DomainLog) Stats(ctx context.Context) (LogStats, error) {
	l.stateMu.Lock()
	info, err := l.state.Info(ctx)
	l.stateMu.Unlock()
	if err != nil {
		return LogStats{}, fmt.Errorf("jetstream: read %q's state: %w", l.name, err)
	}
	stats := LogStats{
		FirstSeq:  info.State.FirstSeq,
		LastSeq:   info.State.LastSeq,
		Messages:  info.State.Msgs,
		Bytes:     info.State.Bytes,
		CreatedAt: info.Created,
	}
	if info.Config.MaxBytes > 0 {
		stats.MaxBytes = uint64(info.Config.MaxBytes)
	}
	return stats, nil
}

// Purge removes every record BELOW upTo, which is the exclusive sequence the
// retention gate concluded may go.
//
// # Why nothing here re-decides
//
// The six terms are a minimum taken somewhere else, deliberately: the gate is
// a policy with an inversion in it that three readers got backwards, and a
// policy that can only be exercised through a live broker is one nobody
// checks. This call is the effect, and its only judgement is the one thing the
// arithmetic cannot know — that removing nothing is not worth a round trip.
//
// The broker's own semantics are what make the exclusive bound safe: a purge
// at sequence N leaves N itself, so a caller that has established every node
// committed THROUGH N − 1 can pass N without arithmetic of its own.
func (l *DomainLog) Purge(ctx context.Context, upTo uint64) error {
	if upTo == 0 {
		return nil
	}
	if err := l.stream.Purge(ctx, jetstream.WithPurgeSequence(upTo)); err != nil {
		return fmt.Errorf("jetstream: purge %q below %d: %w", l.name, upTo, err)
	}
	return nil
}
