package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// Serve makes this node answer the fleet on its own subject: every partition
// it serves, from its own copy, and `not_holder` for every one it does not.
//
// EVERY DATA NODE SERVES, whether or not any other node asks yet, and from
// BEFORE its runtime is up: a node joins without anybody reconfiguring the
// others, and an answerer registered later would leave the first seat that
// asked failing for however long that took. The backend is resolved per
// request, so one that arrives before the runtime is answered "not here"
// rather than refused for ever.
func Serve(ctx context.Context, q Server, self string, local LocalBackends,
	placement Placement, seams ServerSeams) (queue.Unsubscribe, error) {

	switch {
	case q == nil || local == nil:
		return nil, errors.New("estate: serve needs a queue and this node's backends")
	case placement == nil:
		return nil, errors.New("estate: serve needs the placement — a `not_holder` " +
			"names this node's map epoch, which the asker weighs its own against")
	case self == "":
		return nil, errors.New("estate: serve needs this node's id — it is the subject other nodes ask")
	}
	srv := server{self: self, local: local, placement: placement, seams: seams}
	return q.Serve(ctx, Subject(self), func(ctx context.Context, raw []byte) ([]byte, error) {
		return srv.answer(ctx, raw), nil
	})
}

// server is one node's serving half.
type server struct {
	self      string
	local     LocalBackends
	placement Placement
	seams     ServerSeams
}

// answer runs one request and ALWAYS answers: a node that stayed silent
// would be indistinguishable from one that is down, and the asker would wait
// out its budget before trying the next — so every failure here, down to a
// request this build cannot decode, is an answer that says so.
func (s server) answer(ctx context.Context, raw []byte) []byte {
	out := reply{Node: s.self}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		out.Err = encodeError(fmt.Errorf("estate: %s could not decode the request: %w", s.self, err))
		return encodeReply(out)
	}
	spec, known := registry[req.Op]
	if !known {
		// A NEWER PEER'S OPERATION, which this build cannot run and
		// which no other node of this build can run either. Unserved
		// rather than failed, so a fleet mid-upgrade finds a node that
		// can.
		out.Unserved, out.Detail = unservedNoBackend,
			fmt.Sprintf("%s does not know the operation %q", s.self, req.Op)
		return encodeReply(out)
	}
	if !req.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, req.Deadline)
		defer cancel()
	}
	b := Backend{}
	if spec.partitions != nil {
		p, err := s.partitionOf(ctx, spec, req)
		if err != nil {
			out.Err = encodeError(fmt.Errorf("estate: %s: %w", req.Op, err))
			return encodeReply(out)
		}
		served, serves := s.local.For(ctx, p)
		if !serves {
			// NOTHING RAN, so every class moves on — a page write
			// included, whose never-repeat rule is about a request
			// that may have run. The epoch is THIS node's, which is
			// what tells the asker whose view is stale.
			_, epoch, _ := s.placement.Serving(p)
			out.Unserved, out.Epoch = unservedNotHolder, epoch
			out.Detail = fmt.Sprintf("%s does not serve %s (asked at map epoch %d)",
				s.self, p, req.MapEpoch)
			return encodeReply(out)
		}
		b = served
		layout, err := s.placement.Layout()
		if err != nil {
			out.Err = encodeError(fmt.Errorf("estate: %s: read which layout the fleet runs: %w",
				req.Op, err))
			return encodeReply(out)
		}
		reason, detail, obsolete := ready(ctx, s.self, spec, b, p, req.Floors, streamsOf(layout, p))
		for _, gone := range obsolete {
			out.Obsolete = append(out.Obsolete, gone.Stream)
		}
		if reason != "" {
			out.Unserved, out.Detail = reason, detail
			return encodeReply(out)
		}
	}
	b.ServerSeams = s.seams
	result, err := spec.serve(ctx, b, req.Actor, req.Args)
	switch {
	case errors.Is(err, errNoHalf):
		out.Unserved, out.Detail = unservedNoBackend, fmt.Sprintf(
			"%s runs no native backend for %s", s.self, req.Op)
		return encodeReply(out)
	case errors.Is(err, errNotAdmitting):
		out.Unserved, out.Detail = unservedNotEstablished, fmt.Sprintf(
			"%s's copy admits no seat yet — the same gate that withholds seats from it", s.self)
		return encodeReply(out)
	case err != nil:
		out.Err = encodeError(err)
		return encodeReply(out)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		out.Err = encodeError(fmt.Errorf("estate: %s could not encode the answer to %s: %w",
			s.self, req.Op, err))
		return encodeReply(out)
	}
	out.Result = encoded
	return encodeReply(out)
}

// partitionOf is the one partition a request addresses at this node: the one
// it names, or — for a request from a build that predates partitions, which
// names none — the one the operation's arguments resolve to under this node's
// layout.
func (s server) partitionOf(ctx context.Context, spec *opSpec, req request) (statelog.PartitionID, error) {
	var parts []statelog.PartitionID
	if len(req.Partitions) > 0 {
		for _, name := range req.Partitions {
			p, err := statelog.ParsePartitionID(name)
			if err != nil {
				return statelog.PartitionID{}, fmt.Errorf("the request names %q: %w", name, err)
			}
			parts = append(parts, p)
		}
	} else {
		layout, err := s.placement.Layout()
		if err != nil {
			return statelog.PartitionID{}, fmt.Errorf("read which layout the fleet runs: %w", err)
		}
		if parts, err = spec.partitions(ctx, layout, layoutResolver{layout: layout}, req.Args); err != nil {
			return statelog.PartitionID{}, err
		}
	}
	if len(parts) != 1 {
		return statelog.PartitionID{}, fmt.Errorf("the request addresses %d partitions, and "+
			"%s addresses one", len(parts), spec.name)
	}
	return parts[0], nil
}

// ready is the two gates a node that serves p puts in front of an operation —
// its copy answering at all, and the asker's floors on p's logs reached — and
// the same two whether the asker is another node or this one's own router.
// reason is empty when the operation may run; obsolete is every floor on a
// generation the log has abandoned, which the asker stops carrying.
func ready(ctx context.Context, self string, spec *opSpec, b Backend, p statelog.PartitionID,
	floors []statelog.Position, streams []string) (reason unservedReason, detail string,
	obsolete []statelog.Position) {

	if !spec.ungated && b.Answers != nil && !b.Answers(ctx) {
		return unservedNotEstablished, fmt.Sprintf("%s's copy of %s answers no request "+
			"yet — it is behind its logs and has not drained them since it started, "+
			"or has fallen past the snapshot slack of their ends", self, p), nil
	}
	if spec.floorless || b.Committed == nil {
		return "", "", nil
	}
	for _, floor := range floors {
		// A FLOOR ON ANOTHER PARTITION'S LOG is one this node cannot
		// reach by applying p, and is never waited for here.
		if floor.Seq == 0 || !slices.Contains(streams, floor.Stream) {
			continue
		}
		behind, gone := waitFloor(ctx, b, floor)
		if gone {
			obsolete = append(obsolete, floor)
			continue
		}
		if behind != nil {
			return unservedBehind, fmt.Sprintf("%s has not applied %s within %s: %v",
				self, floor, statelog.ReadBudget, behind), obsolete
		}
	}
	return "", "", obsolete
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
