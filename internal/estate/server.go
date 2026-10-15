package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
)

var log = logging.Get("estate")

// Server is what a queue answerer needs to register.
type Server interface {
	Serve(ctx context.Context, subject string, answers int, h queue.AnswerFunc) (queue.Unsubscribe, error)
}

// Serve makes this node answer the fleet on its own subjects, one a [lane],
// from its own copy of the estate — and `out_of_service` for the operations of
// a domain whose copy is wrong (ADR-0031). The returned withdrawal ends every
// lane.
//
// EVERY DATA NODE SERVES, whether or not any other node asks yet, and from
// BEFORE its runtime is up: a node joins without anybody reconfiguring the
// others, and an answerer registered later would leave the first seat that
// asked failing for however long that took. The backend is resolved per
// request, so one that arrives before the runtime is answered "not here"
// rather than refused for ever.
//
// EVERY LANE OR NONE: a lane that cannot be registered withdraws the ones
// before it, because a node that answered its reads and no proposal would be
// asked first for every push to the repositories it commits for, and cost
// each one a whole write attempt before the runner-up.
func Serve(ctx context.Context, q Server, self string, local LocalBackends,
	seams ServerSeams) (queue.Unsubscribe, error) {

	switch {
	case q == nil || local == nil:
		return nil, errors.New("estate: serve needs a queue and this node's backends")
	case self == "":
		return nil, errors.New("estate: serve needs this node's id — it is the subject other nodes ask")
	}
	srv := server{self: self, local: local, seams: seams}
	stops := make([]queue.Unsubscribe, 0, len(lanes))
	withdraw := func(ctx context.Context) error {
		var errs []error
		for _, stop := range slices.Backward(stops) {
			errs = append(errs, stop(ctx))
		}
		return errors.Join(errs...)
	}
	for _, l := range lanes {
		stop, err := q.Serve(ctx, l.subject(self), l.answers(),
			func(ctx context.Context, raw []byte) ([]byte, error) {
				return srv.answer(ctx, l, raw), nil
			})
		if err != nil {
			// A ROLLBACK, so it takes a context the failure cannot
			// have ended: each answer it waits for is bounded by its
			// own request's deadline.
			if undo := withdraw(context.WithoutCancel(ctx)); undo != nil {
				err = errors.Join(err, undo)
			}
			return nil, fmt.Errorf("estate: serve %s on %s: %w", l, l.subject(self), err)
		}
		stops = append(stops, stop)
	}
	return withdraw, nil
}

// server is one node's serving half.
type server struct {
	self  string
	local LocalBackends
	seams ServerSeams
}

// answer runs one request that arrived on lane l and ALWAYS answers: a node
// that stayed silent would be indistinguishable from one that is down, and the
// asker would wait out its budget before trying the next — so every failure
// here, down to a request this build cannot decode, is an answer that says so.
func (s server) answer(ctx context.Context, l lane, raw []byte) []byte {
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
	if spec.lane != l {
		// ANSWERED ONLY ON ITS OWN LANE, whatever the asker sent: run
		// here, a proposal would hold a slot every read waits for, or a
		// read one a proposal does — the separation is the server's to
		// keep, so it does not depend on every asker keeping it.
		// Unserved, like an operation this build does not know, so an
		// asker whose build declares the lane differently moves on to a
		// node that agrees with it.
		out.Unserved, out.Detail = unservedNoBackend, fmt.Sprintf(
			"%s answers %s on %s (%s), and it was asked on %s", s.self, req.Op, spec.lane,
			spec.lane.subject(s.self), l)
		return encodeReply(out)
	}
	if !req.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, req.Deadline)
		defer cancel()
		// EVERY WAIT ENDS A REPLY'S TRIP SHORT OF IT — the floor's below, and
		// a read's barrier and level and a write's resolution inside the
		// operation — so a node that runs out of time says so while its
		// asker is still listening. Run to the deadline itself, a wait's
		// honest `behind` reached the asker after it had given up on this
		// node, which then heard a lagging node as a wedged one: asked it
		// last for suspectFor and told the placement it went unanswered.
		// code.ReplyMargin is the one margin a serving node leaves, the
		// committer's between a batch and its asker's deadline included.
		ctx = statelog.WithReplyMargin(ctx, code.ReplyMargin)
	}
	b, wrong, unserved := unservedCopy(ctx, s.local, spec)
	if unserved {
		// NOTHING RAN, so every class moves on — a page write
		// included, whose never-repeat rule is about a request that
		// may have run.
		out.Unserved = unservedOutOfService
		out.Detail = outOfService(s.self, wrong)
		return encodeReply(out)
	}
	var spent statelog.Spent
	if req.Spent != nil {
		spent = *req.Spent
	}
	reason, detail, obsolete := ready(ctx, s.self, spec, b, req.Floors, req.AcceptLagging, spent)
	for _, gone := range obsolete {
		out.Obsolete = append(out.Obsolete, gone.Stream)
	}
	if reason != "" {
		out.Unserved, out.Detail = reason, detail
		return encodeReply(out)
	}
	b.ServerSeams = s.seams
	b.pageBytes = usagePageBytes
	var written *writtenItems
	if req.Actor != nil && req.Actor.Records {
		// THE ASKER'S TURN WANTS TO HEAR what this write committed to,
		// and its set is in another process: collect here, and answer it
		// beside whatever the operation answers.
		written = &writtenItems{}
		req.Actor.Provenance.Written = written
	}
	result, err := spec.serve(readSpent(ctx, spec, spent), b, req.Actor, req.Args)
	out.Written = written.list()
	switch {
	case errors.Is(err, errNoHalf):
		out.Unserved, out.Detail = unservedNoBackend, ranNothingDetail(s.self, req.Op, err)
		return encodeReply(out)
	case errors.Is(err, errNotAdmitting):
		out.Unserved, out.Detail = unservedNotEstablished, fmt.Sprintf(
			"%s's copy admits no seat yet — the same gate that withholds seats from it", s.self)
		return encodeReply(out)
	case err != nil:
		out.Err = encodeError(err)
		return encodeReply(out)
	}
	encoded, err := encodeResult(result)
	if err != nil {
		out.Err = encodeError(fmt.Errorf("estate: %s could not encode the answer to %s: %w",
			s.self, req.Op, err))
		return encodeReply(out)
	}
	out.Result = encoded
	return encodeReply(out)
}

// jsonAppender is an answer that writes its own encoding — a usage page, which
// holds the encoding [usage.Fill] measured each of its rows by
// ([usage.Page.AppendJSON]) — exactly as [json.Marshal] would.
type jsonAppender interface {
	AppendJSON(dst []byte) ([]byte, error)
}

// encodeResult is an operation's answer as the reply carries it: its own
// encoding where it holds one, so a usage page's rows — encoded once already,
// to cut the page at its budget — are not encoded a second time to send it.
func encodeResult(result any) ([]byte, error) {
	if a, ok := result.(jsonAppender); ok {
		return a.AppendJSON(nil)
	}
	return json.Marshal(result)
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
// every domain's log: no operation reads another domain's rows at a position —
// each domain's tables are written by its own applier alone, and the reads
// that cross into another's, a search's semantic half over the vectors and a
// page's backlinks over the tracker, are of indexes no position describes — so
// a floor there buys nothing, and costs a refusal whenever THAT log's applier
// lags. The seat a
// page comment woke would otherwise hold every tracker read and write its node
// routes until the pages applier reached the comment, and be refused them
// `behind` while that applier was faulted. This node's own router never sends
// one; the check is the server's own, so the rule does not depend on every
// asker keeping it.
//
// A FLOOR THE REQUEST ALREADY WAITED OUT on a node asked before this one
// ([statelog.Spent.Waited]) is not waited for again: this node serves if it
// has applied it and is behind at once if it has not, so a walk past N nodes
// behind one floor waits one read budget rather than N.
func ready(ctx context.Context, self string, spec *opSpec, b Backend, floors []statelog.Position,
	acceptLagging bool, spent statelog.Spent) (reason unservedReason, detail string, obsolete []statelog.Position) {

	if !spec.ungated && !acceptLagging && b.Answers != nil && !b.Answers(ctx) {
		return unservedLagging, fmt.Sprintf("%s's copy of %s lags its log — it has "+
			"not drained it since it started, or has fallen past the snapshot slack "+
			"of its end — so it answers only when no data node whose copy does not "+
			"lag takes the request", self, spec.domain), nil
	}
	if spec.floorStream == "" || b.Committed == nil {
		return "", "", nil
	}
	for _, floor := range floors {
		if floor.Seq == 0 || floor.Stream != spec.floorStream {
			continue
		}
		waited := spent.WaitedFor(floor)
		bound, gone, behind := waitFloor(ctx, b, floor, waited)
		if gone {
			obsolete = append(obsolete, floor)
			continue
		}
		if behind != nil {
			if waited {
				return unservedBehind, fmt.Sprintf("%s has not applied %s, which a node "+
					"asked before it already waited a whole bound for — so it waited for "+
					"nothing", self, floor), obsolete
			}
			return unservedBehind, fmt.Sprintf("%s has not applied %s within %s: %v",
				self, floor, bound, behind), obsolete
		}
	}
	return "", "", obsolete
}

// waitFloor waits for this node to apply one floor, within the read budget —
// and, on a request asked across the wire, within what its asker's attempt
// left ([statelog.WithReplyMargin]), so a node that runs out says it is behind
// while the asker is still listening — or not at all where the request
// already waited it out (waited), when it only checks.
//
// It answers the bound it waited within and the error to report as "behind",
// or obsolete when the floor names a generation this node's log has
// abandoned: a position nobody will ever reach again, which the asker should
// stop carrying rather than refuse every read on.
func waitFloor(ctx context.Context, b Backend, floor statelog.Position, waited bool) (
	bound statelog.WaitBound, obsolete bool, behind error) {

	budget := statelog.ReadBudget
	if waited {
		budget = 0
	}
	bounded, cancel, bound := statelog.WaitWithin(ctx, budget)
	defer cancel()
	err := b.Committed(bounded, floor)
	switch {
	case err == nil:
		return bound, false, nil
	case errors.Is(err, statelog.ErrWaitAbandoned):
		return bound, true, nil
	default:
		return bound, false, err
	}
}

// readSpent is ctx carrying what the request it runs has already paid for on
// the nodes asked before this one ([statelog.WithSpent]) — for a READ, whose
// barrier and waits are what was paid; a write's waits are its write
// authority's own, for its own record, and nothing a read refused elsewhere
// paid for describes them.
func readSpent(ctx context.Context, spec *opSpec, spent statelog.Spent) context.Context {
	if spec.class != opRead {
		return ctx
	}
	return statelog.WithSpent(ctx, spent)
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
