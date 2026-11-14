package jetstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
)

// THE FAILURE MARKS: which of a subscription's messages FAILED AS THEMSELVES
// at a delivery, kept on the broker beside the deliveries it counts.
//
// # Why the broker, and not the attachment
//
// Where a message whose budget a return spent is kept is decided by whether it
// failed as itself at ANY of its deliveries ([queue.RetiredTo]), not by the
// return that spent it: a hand-back, or a node's own failure, judges nothing
// of the message. And one message's deliveries go to every member of its group
// — the broker hands each return to whichever member asks next — so the member
// whose return spends the budget is rarely the one that saw the failures.
// [attachment.failures] is one attachment's, and resets with it; the broker's
// delivery count says how many times a message was delivered and nothing of
// how each ended. So the half the broker does not keep is kept beside it: a
// MARK per message that failed, written by the member whose handler failed it,
// and read by the member that retires it.
//
// # One subject per message, one message per subject
//
// A mark is an empty message on a subject of its own ([failureSubject]), in a
// stream that keeps one message per subject and keeps it for the event
// stream's age ([streamFailures]), so whether a message is marked is one read
// of the newest message on its subject — the stream leader's, so a mark
// written a moment before by another member is never missed for a replica that
// had not heard of it. The subject names the pair, the stream, the message's
// sequence there and the broker's instant for it, so the message a stream
// deleted and made again stores at a reused sequence is never taken for it.
//
// # What a mark costs, and what its absence costs
//
// One write, at the first failure of a message on a member — every later
// failure there finds it written ([attachment.failed]) — and one read when a
// return that is no failure of its own spends a budget, which is rare; and the
// mark is deleted once the member that wrote it settles the message, or the
// member that retires it, or by the stream's age where another member did. A mark that could not be written or
// read is a message kept with the unhandled deliveries though it failed: the
// SAFE DIRECTION, a copy its reader handles again and finds failing, never a
// healthy delivery kept with the poison nobody replays.

// streamFailures is the stream the failure marks are kept in.
const streamFailures = "CREWLET_FAILED"

// failurePrefix is the namespace every failure mark's subject lies under.
const failurePrefix = "failed."

// failureSubject is the subject the failure mark of the message md describes,
// delivered to the subscription key, is kept on: its pair, its stream, its
// sequence there and the broker's instant for it, digested — a pair is two
// arbitrary strings, which no subject token carries verbatim — behind NULs,
// which neither half of the pair can hold.
func failureSubject(key attachKey, md *jetstream.MsgMetadata) string {
	sum := sha256.Sum256([]byte(key.topic + "\x00" + key.group + "\x00" + md.Stream + "\x00" +
		strconv.FormatUint(md.Sequence.Stream, 10) + "\x00" +
		strconv.FormatInt(md.Timestamp.UnixNano(), 10)))
	return failurePrefix + hex.EncodeToString(sum[:16])
}

// failureMarks is the stream the marks are kept in as this client reads and
// deletes them: its leader's reads, and a handle to delete a mark through, each
// opened once.
type failureMarks struct {
	mu     sync.Mutex
	leader *jsapi.Leader
	stream jetstream.Stream
}

// markFailure writes the mark of a message that failed as itself on subject
// ([failureSubject]) — acknowledged, as every write of this client's is
// ([Queue.write]) — and answers the sequence the broker stored it at, which
// is what deletes it ([Queue.clearFailure]).
func (q *Queue) markFailure(ctx context.Context, subject string) (uint64, error) {
	if _, err := q.streamFor(ctx, subject); err != nil {
		return 0, err
	}
	ack, err := q.write(ctx, &nats.Msg{Subject: subject})
	if err != nil {
		return 0, fmt.Errorf("mark %s: %w", subject, q.brokerFailed(ctx, err))
	}
	return ack.Sequence, nil
}

// failureMarked reports whether subject holds a mark, as the stream's leader
// holds it, and the sequence it is stored at.
func (q *Queue) failureMarked(ctx context.Context, subject string) (uint64, bool, error) {
	leader, err := q.failureLeader(ctx, subject)
	if err != nil {
		return 0, false, err
	}
	msg, err := leader.Last(ctx, subject)
	switch {
	case errors.Is(err, jsapi.ErrNoMessage):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("read %s: %w", subject, q.brokerFailed(ctx, err))
	}
	return msg.Sequence, true, nil
}

// clearFailure deletes the mark stored at seq: its message is settled, and
// nothing will read the mark again. BY ITS SEQUENCE, the one way this package
// removes a message, never by its subject (TestNothingPurgesPerSubject).
func (q *Queue) clearFailure(ctx context.Context, seq uint64) error {
	st, err := q.failureStream(ctx)
	if err != nil {
		return err
	}
	if err := st.DeleteMsg(ctx, seq); err != nil {
		return fmt.Errorf("delete failure mark %d: %w", seq, q.brokerFailed(ctx, err))
	}
	return nil
}

// failureLeader is the marks stream's leader reads, opened once.
func (q *Queue) failureLeader(ctx context.Context, subject string) (*jsapi.Leader, error) {
	stream, err := q.streamFor(ctx, subject)
	if err != nil {
		return nil, err
	}
	q.marks.mu.Lock()
	defer q.marks.mu.Unlock()
	if q.marks.leader == nil {
		leader, err := jsapi.NewLeader(q.js, stream)
		if err != nil {
			return nil, fmt.Errorf("jetstream: read the failure marks: %w", err)
		}
		q.marks.leader = leader
	}
	return q.marks.leader, nil
}

// failureStream is a handle to the marks stream, opened once.
func (q *Queue) failureStream(ctx context.Context) (jetstream.Stream, error) {
	stream, err := q.streamFor(ctx, failurePrefix+"stream")
	if err != nil {
		return nil, err
	}
	q.marks.mu.Lock()
	defer q.marks.mu.Unlock()
	if q.marks.stream == nil {
		st, err := q.js.Stream(ctx, stream)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", stream, q.brokerFailed(ctx, err))
		}
		q.marks.stream = st
	}
	return q.marks.stream, nil
}
