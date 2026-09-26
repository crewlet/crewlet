package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
)

var log = logging.Get("estate")

// Server is what a queue answerer needs to register.
type Server interface {
	Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error)
}

// Resolve answers this node's backend for one request, and whether it has
// one at all.
type Resolve func() (Backend, bool)

// Serve makes this node answer for its estate on its own subject.
//
// EVERY DATA NODE SERVES, whether or not any stateless node exists yet: a
// stateless node joins without anybody reconfiguring the members, and an
// answerer registered later would leave its first seat's tools failing for
// however long that took.
func Serve(ctx context.Context, q Server, nodeID string, resolve Resolve) (queue.Unsubscribe, error) {
	if q == nil || resolve == nil {
		return nil, errors.New("estate: serve needs a queue and a backend")
	}
	if nodeID == "" {
		return nil, errors.New("estate: serve needs this node's id — it is the subject a stateless node asks")
	}
	return q.Serve(ctx, Subject(nodeID), func(ctx context.Context, raw []byte) ([]byte, error) {
		return answer(ctx, nodeID, resolve, raw), nil
	})
}

// answer runs one request and ALWAYS answers: a node that stayed silent
// would be indistinguishable from one that is down, and the asker would wait
// out its budget before trying the next — so every failure here, down to a
// request this build cannot decode, is an answer that says so.
func answer(ctx context.Context, nodeID string, resolve Resolve, raw []byte) []byte {
	out := reply{Node: nodeID}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		out.Err = encodeError(fmt.Errorf("estate: %s could not decode the request: %w", nodeID, err))
		return encodeReply(out)
	}
	spec, known := registry[req.Op]
	if !known {
		// A NEWER PEER'S OPERATION, which this build cannot run and
		// which no other node of this build can run either. Unserved
		// rather than failed, so a fleet mid-upgrade finds a node that
		// can.
		out.Unserved, out.Detail = unservedNoBackend,
			fmt.Sprintf("%s does not know the operation %q", nodeID, req.Op)
		return encodeReply(out)
	}
	if !req.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, req.Deadline)
		defer cancel()
	}
	b, ok := resolve()
	if !ok {
		out.Unserved, out.Detail = unservedNoBackend,
			nodeID+" runs no native backend yet"
		return encodeReply(out)
	}
	if !spec.ungated && b.Established != nil && !b.Established(ctx) {
		out.Unserved, out.Detail = unservedNotEstablished,
			nodeID+"'s copy of the estate is not established yet — the same gate that withholds seats from it"
		return encodeReply(out)
	}
	if spec.stream != "" && b.Committed != nil {
		for _, floor := range req.Floors {
			if floor.Stream != spec.stream || floor.Seq == 0 {
				continue
			}
			behind, obsolete := waitFloor(ctx, b, floor)
			if obsolete {
				out.Obsolete = append(out.Obsolete, floor.Stream)
				continue
			}
			if behind != nil {
				out.Unserved, out.Detail = unservedBehind, fmt.Sprintf(
					"%s has not applied %s within %s: %v", nodeID, floor,
					statelog.ReadBudget, behind)
				return encodeReply(out)
			}
		}
	}
	result, err := spec.serve(ctx, b, req.Actor, req.Args)
	if errors.Is(err, errNoHalf) {
		out.Unserved, out.Detail = unservedNoBackend, fmt.Sprintf(
			"%s runs no native backend for %s", nodeID, req.Op)
		return encodeReply(out)
	}
	if err != nil {
		out.Err = encodeError(err)
		return encodeReply(out)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		out.Err = encodeError(fmt.Errorf("estate: %s could not encode the answer to %s: %w",
			nodeID, req.Op, err))
		return encodeReply(out)
	}
	out.Result = encoded
	return encodeReply(out)
}

// waitFloor waits for this node to apply one floor, within the read budget.
//
// It answers the error to report as "behind", or obsolete when the floor
// names a generation this node's log has abandoned: a position nobody will
// ever reach again, which the asker should stop carrying rather than refuse
// every read on.
func waitFloor(ctx context.Context, b Backend, floor statelog.Position) (behind error, obsolete bool) {
	bounded, cancel := context.WithTimeout(ctx, statelog.ReadBudget)
	defer cancel()
	err := b.Committed(bounded, floor)
	switch {
	case err == nil:
		return nil, false
	case errors.Is(err, statelog.ErrWaitAbandoned):
		return nil, true
	default:
		return err, false
	}
}

// encodeReply never fails: every field of a reply is plain data, and an
// encoder that refused one would be the silent node answer exists to avoid.
func encodeReply(r reply) []byte {
	raw, err := json.Marshal(r)
	if err != nil {
		raw, _ = json.Marshal(reply{Node: r.Node, Err: &wireError{
			Message: fmt.Sprintf("estate: %s could not encode its reply: %v", r.Node, err),
		}})
	}
	if len(raw) > queue.MaxPayloadBytes {
		// TOO LARGE TO SEND, and said so rather than dropped: a reply the
		// broker refuses is a node the asker waits out and then counts as
		// down, while this one says what to narrow.
		raw, _ = json.Marshal(reply{Node: r.Node, Err: encodeError(fmt.Errorf(
			"estate: the answer is %d bytes, over the %d a message carries — "+
				"narrow the request: %w", len(raw), queue.MaxPayloadBytes, queue.ErrTooLarge))})
	}
	return raw
}

// deadlineOf is the deadline a request carries, or zero.
func deadlineOf(ctx context.Context) time.Time {
	d, _ := ctx.Deadline()
	return d
}
