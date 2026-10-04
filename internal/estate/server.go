package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
)

var log = logging.Get("estate")

// Server is what a queue answerer needs to register.
type Server interface {
	Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error)
}

// Serve makes this node answer the fleet on its own subject, from its own copy
// of the estate — and `out_of_service` while that copy is wrong.
//
// EVERY DATA NODE SERVES, whether or not any other node asks yet, and from
// BEFORE its runtime is up: a node joins without anybody reconfiguring the
// others, and an answerer registered later would leave the first seat that
// asked failing for however long that took. The backend is resolved per
// request, so one that arrives before the runtime is answered "not here"
// rather than refused for ever.
func Serve(ctx context.Context, q Server, self string, local LocalBackends,
	seams ServerSeams) (queue.Unsubscribe, error) {

	switch {
	case q == nil || local == nil:
		return nil, errors.New("estate: serve needs a queue and this node's backends")
	case self == "":
		return nil, errors.New("estate: serve needs this node's id — it is the subject other nodes ask")
	}
	srv := server{self: self, local: local, seams: seams}
	return q.Serve(ctx, Subject(self), func(ctx context.Context, raw []byte) ([]byte, error) {
		return srv.answer(ctx, raw), nil
	})
}

// server is one node's serving half.
type server struct {
	self  string
	local LocalBackends
	seams ServerSeams
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
	b, serves := s.local.For(ctx)
	if !serves {
		// NOTHING RAN, so every class moves on — a page write
		// included, whose never-repeat rule is about a request that
		// may have run.
		out.Unserved = unservedOutOfService
		out.Detail = fmt.Sprintf("%s's copy of the estate is out of service — "+
			"wrong rather than behind, so it answers nothing until it recovers", s.self)
		return encodeReply(out)
	}
	reason, detail, obsolete := ready(ctx, s.self, spec, b, req.Floors, req.AcceptLagging)
	for _, gone := range obsolete {
		out.Obsolete = append(out.Obsolete, gone.Stream)
	}
	if reason != "" {
		out.Unserved, out.Detail = reason, detail
		return encodeReply(out)
	}
	b.ServerSeams = s.seams
	var written *writtenItems
	if req.Actor != nil && req.Actor.Records {
		// THE ASKER'S TURN WANTS TO HEAR what this write committed to,
		// and its set is in another process: collect here, and answer it
		// beside whatever the operation answers.
		written = &writtenItems{}
		req.Actor.Provenance.Written = written
	}
	result, err := spec.serve(ctx, b, req.Actor, req.Args)
	out.Written = written.list()
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

// ready is the two gates a data node's copy puts in front of an operation —
// the copy answering requests, and the asker's floors on the log of the
// operation's own domain reached ([opSpec.floorStream]) — and the same two
// whether the asker is another node or this one's own router. reason is empty
// when the operation may run; obsolete is every floor on a generation the log
// has abandoned, which the asker stops carrying.
//
// THE FIRST GATE IS A PREFERENCE, and acceptLagging is the asker saying it has
// no better node to ask: a copy that lags its logs is a worse choice than one
// that does not, never a wrong one — the floors below and the read's own level
// hold what it answers to what the caller must see. See [Router.route].
//
// A FLOOR ON ANOTHER DOMAIN'S LOG IS NOT WAITED FOR, though the estate carries
// every domain's log: no operation reads another domain's rows — each domain's
// tables are written by its own applier alone — so a floor there buys
// nothing, and costs a refusal whenever THAT log's applier lags. The seat a
// page comment woke would otherwise hold every tracker read and write its node
// routes until the pages applier reached the comment, and be refused them
// `behind` while that applier was faulted. This node's own router never sends
// one; the check is the server's own, so the rule does not depend on every
// asker keeping it.
func ready(ctx context.Context, self string, spec *opSpec, b Backend, floors []statelog.Position,
	acceptLagging bool) (reason unservedReason, detail string, obsolete []statelog.Position) {

	if !spec.ungated && !acceptLagging && b.Answers != nil && !b.Answers(ctx) {
		return unservedLagging, fmt.Sprintf("%s's copy of the estate lags its logs — it "+
			"has not drained them since it started, or has fallen past the snapshot "+
			"slack of their ends — so it answers only when no data node whose copy "+
			"does not lag takes the request", self), nil
	}
	if spec.floorStream == "" || b.Committed == nil {
		return "", "", nil
	}
	for _, floor := range floors {
		if floor.Seq == 0 || floor.Stream != spec.floorStream {
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

// writtenItems is the [tracker.WriteLog] a served write reports into on behalf
// of an asking node's turn — see [reply.Written]. A walking gesture's writer
// may report one item more than once; the set keeps it once, in the order it
// was first reported.
type writtenItems struct {
	mu    sync.Mutex
	items []types.WorkItem
}

// Add implements [tracker.WriteLog].
func (w *writtenItems) Add(item types.WorkItem) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, held := range w.items {
		if held.Backend == item.Backend && held.ID == item.ID {
			return
		}
	}
	w.items = append(w.items, item)
}

// list is what was reported, or nil where nothing asked for it.
func (w *writtenItems) list() []types.WorkItem {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.items)
}
