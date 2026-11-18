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

// THE FAILURE MARKS: which of a subscription's messages failed AS THEMSELVES,
// kept on the broker. Where a spent message is kept turns on whether it failed
// as itself at ANY delivery ([queue.RetiredTo]), and the member that spends its
// budget rarely saw those failures: [attachment.failures] is one attachment's,
// and the delivery count says nothing of how each ended. A mark is an empty
// message on its own subject ([failureSubject]) in a stream keeping one per
// subject for the event stream's age ([streamFailures]), read off the leader so
// a mark another member just wrote is never missed. Written once per member
// ([attachment.failed]), read only when a return not failing as itself spends
// a budget, deleted when its writer or the retirer settles the message, else
// by age. A mark not written or read keeps a failed message with the unhandled
// deliveries: the SAFE direction, never a healthy one kept with the poison.

// streamFailures is the stream the failure marks are kept in.
const streamFailures = "CREWLET_FAILED"

// failurePrefix is the namespace every failure mark's subject lies under.
const failurePrefix = "failed."

// failureSubject is the subject of the mark for message md delivered to key:
// its pair, stream, stream sequence and broker instant (so a message stored
// again at a reused sequence is never taken for it), joined by NULs, which
// neither half of the pair can hold, and digested, since no subject token
// carries two arbitrary strings verbatim.
func failureSubject(key attachKey, md *jetstream.MsgMetadata) string {
	sum := sha256.Sum256([]byte(key.topic + "\x00" + key.group + "\x00" + md.Stream + "\x00" +
		strconv.FormatUint(md.Sequence.Stream, 10) + "\x00" +
		strconv.FormatInt(md.Timestamp.UnixNano(), 10)))
	return failurePrefix + hex.EncodeToString(sum[:16])
}

// failureMarks is this client's handles on the marks stream, each opened
// once: its leader's reads, and the stream to delete a mark through.
type failureMarks struct {
	mu     sync.Mutex
	leader *jsapi.Leader
	stream jetstream.Stream
}

// markFailure writes a mark on subject ([failureSubject]), acknowledged as
// every write is ([Queue.write]), and answers the sequence it is stored at,
// which is what deletes it ([Queue.clearFailure]).
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

// clearFailure deletes the mark stored at seq once its message is settled. By
// sequence, never by subject: this package removes a message no other way
// (TestNothingPurgesPerSubject).
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
