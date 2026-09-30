package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// Asker is what a router needs from the queue: the ephemeral request/reply
// verb, one subject per serving node.
type Asker interface {
	Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error)
}

// Placement is how the router learns who serves a partition. Declared here, by
// the consumer: under layout 0 the engine's answer is every live data node
// (partmap.Whole over the watched presence view), and under a layout with an
// estate map it is the map's view (partmap.View).
//
// ASKED ON EVERY REQUEST, so every answer comes from memory — a watched view,
// never a listing of the fleet per call.
type Placement interface {
	// Layout is the layout the fleet runs, or an error when that is not
	// known — never a layout standing in for "could not say".
	Layout() (statelog.Layout, error)

	// Serving is p's serving holders in the order the view ranks them, and
	// the map epoch they were read at (0 where there is no map). An error is
	// UNKNOWN, never "no holder": an empty list is a partition nobody
	// serves.
	Serving(p statelog.PartitionID) (nodes []string, epoch uint64, err error)

	// Refresh reads the placement from its source NOW, for a router told by
	// a server whose epoch is newer than the view's that it routed by an
	// old map. It must not wait for a watch to deliver.
	Refresh(ctx context.Context) error

	// Unanswered tells the placement a node it named did not answer, so a
	// node that left on a clean stop is gone from the next answer rather
	// than from the one a heartbeat later. It must not block.
	Unanswered(node string)
}

// LocalBackends is this node's own backends, per partition it SERVES.
type LocalBackends interface {
	// For is the backend over p's copy on this node, or false when this
	// node does not serve p right now: it does not hold p, has not finished
	// joining it, or has begun to leave it ([statelog.Holding], the answer
	// the write authority's gate 3 reads).
	//
	// It takes a context because what a copy may answer is read from the
	// log's broker ([Backend.Answers]); an implementation answers from a
	// verdict it refreshes, never with a broker round trip per request.
	For(ctx context.Context, p statelog.PartitionID) (Backend, bool)
}

// RouterOptions are a router's dependencies.
type RouterOptions struct {
	// Self is this node, which asks — named on every request so a serving
	// node's log says who it answered, and never asked over the wire.
	Self string

	Queue     Asker
	Placement Placement

	// Local is this node's own backends, nil on a node that holds no
	// partition at all.
	Local LocalBackends

	// Session is THIS NODE's floors ([Session]): one per node, shared with
	// whatever else observes a position a seat must read past.
	Session *Session

	// Seams are this node's own, partition-free: supplied to every
	// operation this node answers in-process.
	Seams ServerSeams

	// Now is the clock the suspect cooldown and the admission cache read,
	// injected for tests. Nil is the wall clock.
	Now func() time.Time
}

// Router routes every estate operation to a node that serves the partition it
// addresses: this node, in-process, where it serves it; otherwise the
// partition's holders over the broker, one subject per node.
//
// # One per node, on every node
//
// A node without the `data` role holds nothing and asks for everything; a data
// node answers what it serves and asks for the rest — which under layout 0 is
// nothing, and under a partitioned layout is most of the estate. Both are the
// same router, so a tool behaves identically on either, and the path a data
// node takes to its own copy is the path it takes to a peer's, floors included.
//
// # The order holders are asked in
//
// This node first, where it serves the partition. Then the node that answered
// last for THIS partition (sticky: its applier is the one most likely to have
// this node's writes), then a rendezvous order that spreads askers across the
// holders, with every node that recently went silent last. A node that ran
// nothing is passed over by every class of operation; what may be repeated
// after a node MAY have run it is the class's ([opClass]).
type Router struct {
	self      string
	queue     Asker
	placement Placement
	local     LocalBackends
	session   *Session
	seams     ServerSeams
	now       func() time.Time

	// readBudget and writeBudget are [readAttempt] and [writeAttempt],
	// held so a test can shorten them rather than wait out a dead node.
	readBudget, writeBudget time.Duration

	mu sync.Mutex
	// sticky is, per partition, the node that answered last — asked
	// first next time.
	sticky map[statelog.PartitionID]string
	// suspect is when each node that went unanswered may be asked first
	// again.
	suspect map[string]time.Time
	// admitted caches a REMOTE holder's answer to the admission question,
	// per partition — see [Router.Serves].
	admitted map[statelog.PartitionID]admission
}

// admission is one cached answer to [Router.Serves].
type admission struct {
	at             time.Time
	tracker, pages bool
	err            error
}

// NewRouter builds a router.
func NewRouter(opts RouterOptions) (*Router, error) {
	switch {
	case opts.Queue == nil:
		return nil, errors.New("estate: a router needs the queue it asks over")
	case opts.Placement == nil:
		return nil, errors.New("estate: a router needs the placement it routes by")
	case opts.Session == nil:
		return nil, errors.New("estate: a router needs this node's session floors — " +
			"one per node, shared by everything that observes a write")
	case opts.Self == "":
		return nil, errors.New("estate: a router needs this node's id — it is " +
			"what a serving node's log names, and what the router never asks")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Router{
		self: opts.Self, queue: opts.Queue, placement: opts.Placement,
		local: opts.Local, session: opts.Session, seams: opts.Seams, now: now,
		readBudget: readAttempt, writeBudget: writeAttempt,
		sticky:   map[statelog.PartitionID]string{},
		suspect:  map[string]time.Time{},
		admitted: map[statelog.PartitionID]admission{},
	}, nil
}

// readAttempt bounds one read's wait on one node.
//
// TEN SECONDS: a serving node may wait [statelog.ReadBudget] for the floor and
// again for the level a read asked for, and then runs the query — so a node
// that has not answered in several times that is one that is gone rather than
// slow, and the next one answers sooner than it would. It is a ceiling under
// the caller's own deadline, never over it.
const readAttempt = 10 * time.Second

// writeAttempt bounds one write's wait on one node, for a caller with no
// deadline of its own.
//
// SIXTY SECONDS: a single append resolves within [statelog.DefaultResolveBudget]
// (five), and the longest gesture a seat's tools make — a merge or a move —
// appends once per object it walks and waits between them. A caller with a
// deadline is held to that instead.
const writeAttempt = 60 * time.Second

// suspectFor is how long a node that went unanswered is asked last.
//
// THIRTY SECONDS, which is the order of a presence lease's life: a node that
// died leaves the placement when its lease lapses, and until then every
// request that asked it first would wait out a whole attempt. Asked last
// instead, it costs one attempt per router rather than one per request.
const suspectFor = 30 * time.Second

// admissionTrust is how long a remote holder's answer to the admission
// question is trusted.
//
// FIVE SECONDS: the admission gate is asked on every placement sweep, and a
// fresh ask per sweep is a request to a data node per heartbeat per node for
// an answer that changes when a node joins or leaves — which the presence
// lease's own TTL already bounds at the same order. A LOCAL answer is never
// cached: it costs no request, and a node's own copy falling behind must
// withhold its next claim at once.
const admissionTrust = 5 * time.Second

// Session is this node's floors.
func (r *Router) Session() *Session { return r.session }

// Observe raises this node's floor to a position it has been told landed —
// every write's own, and anything a caller waits for.
func (r *Router) Observe(at statelog.Position) { r.session.Observe(at) }

// Await is [Router.Observe] in the shape the tool seams wait in.
func (r *Router) Await(ctx context.Context, at statelog.Position) error {
	return r.session.Await(ctx, at)
}

// exchange is one operation's two halves in its caller's types, boxed so the
// routing itself is not generic.
type exchange struct {
	// local runs the operation in-process on a backend this node serves.
	local func(ctx context.Context, b Backend) (any, error)

	// encoded is the arguments on the wire, encoded once.
	encoded json.RawMessage

	// decode is a remote answer's result, in the caller's type.
	decode func(raw json.RawMessage) (any, error)

	// resolve is the partition the arguments address under a layout —
	// asked again after a refresh, since a new map may be a new layout.
	resolve func(l statelog.Layout) (statelog.PartitionID, error)
}

// call runs one operation on a node that serves its partition.
func call[A, R any](ctx context.Context, r *Router, o op[A, R], actor *Actor, args A) (R, error) {
	var zero R
	spec := o.spec
	encoded, err := json.Marshal(args)
	if err != nil {
		return zero, fmt.Errorf("estate: %s: encode the arguments: %w", spec.name, err)
	}
	x := exchange{
		local: func(ctx context.Context, b Backend) (any, error) {
			return o.serve(ctx, b, actor, args)
		},
		encoded: encoded,
		decode: func(raw json.RawMessage) (any, error) {
			var out R
			if len(raw) == 0 {
				return out, nil
			}
			decodeErr := json.Unmarshal(raw, &out)
			return out, decodeErr
		},
	}
	var answer any
	if o.partitions == nil {
		answer, err = r.anyNode(ctx, spec, actor, x)
	} else {
		x.resolve = func(l statelog.Layout) (statelog.PartitionID, error) {
			parts, resolveErr := o.partitions(ctx, l, layoutResolver{layout: l}, args)
			switch {
			case resolveErr != nil:
				return statelog.PartitionID{}, resolveErr
			case len(parts) != 1:
				return statelog.PartitionID{}, fmt.Errorf("estate: %s addresses %d "+
					"partitions, and a single-partition operation addresses one",
					spec.name, len(parts))
			}
			return parts[0], nil
		}
		answer, err = r.route(ctx, spec, actor, x)
	}
	if out, ok := answer.(R); ok {
		return out, err
	}
	return zero, err
}

// held is an answer the router keeps while it asks another holder: one a
// holder gave that is not FINAL for its class ([opIdempotentWrite]'s
// `unvouched`), and is the answer if no holder gives a better one.
type held struct {
	value any
	err   error
}

// route runs one single-partition operation — this node first where it serves
// the partition, then the partition's holders in order.
func (r *Router) route(ctx context.Context, spec *opSpec, actor *Actor, x exchange) (any, error) {
	layout, err := r.placement.Layout()
	if err != nil {
		return nil, fmt.Errorf("estate: %s: read which layout the fleet runs: %w", spec.name, err)
	}
	p, err := x.resolve(layout)
	if err != nil {
		return nil, fmt.Errorf("estate: %s: %w", spec.name, err)
	}
	var (
		reasons  []string
		fallback *held
	)
	// THIS NODE FIRST, where it serves the partition — in-process, with
	// the node's floors enforced exactly as a remote holder enforces them.
	if r.local != nil {
		if b, ok := r.local.For(ctx, p); ok {
			b.ServerSeams = r.seams
			value, why, ran := r.runLocal(ctx, spec, b, layout, p, x)
			switch {
			case why != "":
				reasons = append(reasons, r.self+": "+why)
			case appendedNothing(ran):
				reasons = append(reasons, r.self+": "+ran.Error())
			case unvouched(spec, value, ran):
				fallback = &held{value: value, err: ran}
			default:
				return value, ran
			}
		}
	}
	nodes, epoch, err := r.placement.Serving(p)
	if err != nil {
		if fallback != nil {
			return fallback.value, fallback.err
		}
		return nil, fmt.Errorf("estate: %s: read who serves %s: %w", spec.name, p, err)
	}
	budget := r.readBudget
	if spec.class != opRead {
		// A WRITE takes the caller's deadline where there is one,
		// because a gesture the caller gave five minutes must not be
		// abandoned after one.
		budget = r.writeBudget
		if _, bounded := ctx.Deadline(); bounded {
			budget = 0
		}
	}
	tried := map[string]bool{r.self: true}
	refreshed := false
	for {
		restart := false
		for _, node := range r.order(p, nodes) {
			if tried[node] {
				continue
			}
			tried[node] = true
			floors := r.floorsFor(spec, layout, p)
			req := request{
				Op: spec.name, Args: x.encoded, Actor: actor,
				Partitions: []string{p.String()}, MapEpoch: epoch,
				Floors: floors, From: r.self,
			}
			rep, answered, err := r.ask(ctx, node, req, budget)
			if err != nil {
				return nil, fmt.Errorf("estate: %s: %w", spec.name, err)
			}
			if !answered {
				if spec.class == opOnceWrite {
					return nil, fmt.Errorf("%w (%s on %s, asked of %s)", ErrOutcomeUnknown,
						spec.name, p, node)
				}
				if ctx.Err() != nil {
					return nil, fmt.Errorf("estate: %s: %w", spec.name, ctx.Err())
				}
				reasons = append(reasons, node+": no answer")
				continue
			}
			r.session.Forget(named(floors, rep.Obsolete)...)
			if rep.Unserved == unservedNotHolder && rep.Epoch > epoch && !refreshed {
				// THE SERVER KNOWS A NEWER MAP. Read ours again,
				// resolve again, and walk the new holders — once:
				// a server that keeps outrunning a view just read is
				// one this request does not wait for.
				refreshed = true
				reasons = append(reasons, fmt.Sprintf("%s: %s (at map epoch %d, "+
					"this node routed by %d)", node, rep.Detail, rep.Epoch, epoch))
				fresh, freshEpoch, freshLayout, freshP, refreshErr := r.refresh(ctx, x, p)
				if refreshErr != nil {
					reasons = append(reasons, "refresh: "+refreshErr.Error())
					continue
				}
				nodes, epoch, layout, p = fresh, freshEpoch, freshLayout, freshP
				restart = true
				break
			}
			if rep.Unserved != "" {
				reasons = append(reasons, fmt.Sprintf("%s: %s", node, rep.Detail))
				continue
			}
			r.markAnswered(p, node)
			if rep.Err != nil {
				failure := decodeError(rep.Err)
				switch {
				case appendedNothing(failure):
					reasons = append(reasons, node+": "+failure.Error())
					continue
				case unvouched(spec, nil, failure):
					fallback = &held{err: failure}
					reasons = append(reasons, node+": unvouched")
					continue
				}
				return nil, failure
			}
			value, err := x.decode(rep.Result)
			if err != nil {
				return nil, fmt.Errorf("estate: %s: %s answered with a result this "+
					"build cannot decode: %w", spec.name, node, err)
			}
			if unvouched(spec, value, nil) {
				fallback = &held{value: value}
				reasons = append(reasons, node+": unvouched")
				continue
			}
			return value, nil
		}
		if !restart {
			break
		}
	}
	if fallback != nil {
		return fallback.value, fallback.err
	}
	return nil, &ErrPartitionUnserved{Partition: p.String(), Detail: strings.Join(reasons, "; ")}
}

// refresh reads the placement again after a server named a newer map, and
// answers what the operation resolves to under it.
func (r *Router) refresh(ctx context.Context, x exchange, was statelog.PartitionID) (
	nodes []string, epoch uint64, layout statelog.Layout, p statelog.PartitionID, err error) {

	if err = r.placement.Refresh(ctx); err != nil {
		return nil, 0, statelog.Layout{}, was, err
	}
	if layout, err = r.placement.Layout(); err != nil {
		return nil, 0, statelog.Layout{}, was, err
	}
	if p, err = x.resolve(layout); err != nil {
		return nil, 0, statelog.Layout{}, was, err
	}
	nodes, epoch, err = r.placement.Serving(p)
	return nodes, epoch, layout, p, err
}

// runLocal runs one operation in-process on a partition this node serves. why
// is set, and nothing ran, when the node's own copy could not take it — the
// same answers a remote holder gives, and the router moves on from each.
func (r *Router) runLocal(ctx context.Context, spec *opSpec, b Backend, layout statelog.Layout,
	p statelog.PartitionID, x exchange) (value any, why string, err error) {

	floors := r.floorsFor(spec, layout, p)
	reason, detail, obsolete := ready(ctx, r.self, spec, b, p, floors, streamsOf(layout, p))
	r.session.Forget(obsolete...)
	if reason != "" {
		return nil, detail, nil
	}
	value, err = x.local(ctx, b)
	switch {
	case errors.Is(err, errNoHalf):
		return nil, fmt.Sprintf("%s runs no native backend for %s", r.self, spec.name), nil
	case errors.Is(err, errNotAdmitting):
		return nil, fmt.Sprintf("%s's copy of %s admits no seat yet", r.self, p), nil
	}
	return value, "", err
}

// floorsFor is the floors a request for spec on p carries: this node's
// session on p's logs, or none for an operation that reads no log position.
func (r *Router) floorsFor(spec *opSpec, layout statelog.Layout, p statelog.PartitionID) []statelog.Position {
	if spec.floorless {
		return nil
	}
	return r.session.Floors(streamsOf(layout, p))
}

// ask sends one request to one node. answered is false for a node that said
// nothing — suspected, and reported to the placement — and err is set only
// where the request could not be made at all, which no other node would fare
// better with.
func (r *Router) ask(ctx context.Context, node string, req request, budget time.Duration) (
	reply, bool, error) {

	attempt, cancel := attemptContext(ctx, budget)
	defer cancel()
	req.Deadline = deadlineOf(attempt)
	raw, err := json.Marshal(req)
	if err != nil {
		return reply{}, false, fmt.Errorf("encode the request: %w", err)
	}
	replies, err := r.queue.Ask(attempt, Subject(node), raw, 1)
	if err != nil {
		// THE ASK COULD NOT BE MADE AT ALL — a queue that is not live,
		// a request over the size limit.
		return reply{}, false, err
	}
	if len(replies) == 0 {
		r.markSuspect(node)
		return reply{}, false, nil
	}
	var rep reply
	if err := json.Unmarshal(replies[0], &rep); err != nil {
		return reply{}, false, fmt.Errorf("%s answered something this build cannot decode: %w",
			node, err)
	}
	return rep, true, nil
}

// anyNode runs an operation that addresses no partition on the first data
// node that will: any node serving any partition of the fleet's layout. It is
// never answered in-process — the one such operation takes custody of a node's
// event records, which a node holding a store keeps itself.
func (r *Router) anyNode(ctx context.Context, spec *opSpec, actor *Actor, x exchange) (any, error) {
	layout, err := r.placement.Layout()
	if err != nil {
		return nil, fmt.Errorf("estate: %s: read which layout the fleet runs: %w", spec.name, err)
	}
	var nodes []string
	for _, p := range layout.Partitions() {
		holders, _, err := r.placement.Serving(p)
		if err != nil {
			return nil, fmt.Errorf("estate: %s: read who serves %s: %w", spec.name, p, err)
		}
		for _, n := range holders {
			if !slices.Contains(nodes, n) {
				nodes = append(nodes, n)
			}
		}
	}
	budget := r.readBudget
	if spec.class != opRead {
		budget = r.writeBudget
		if _, bounded := ctx.Deadline(); bounded {
			budget = 0
		}
	}
	var reasons []string
	for _, node := range r.order(statelog.PartitionID{}, nodes) {
		rep, answered, err := r.ask(ctx, node, request{
			Op: spec.name, Args: x.encoded, Actor: actor, From: r.self,
		}, budget)
		if err != nil {
			return nil, fmt.Errorf("estate: %s: %w", spec.name, err)
		}
		if !answered {
			if spec.class == opOnceWrite {
				return nil, fmt.Errorf("%w (%s, asked of %s)", ErrOutcomeUnknown, spec.name, node)
			}
			if ctx.Err() != nil {
				return nil, fmt.Errorf("estate: %s: %w", spec.name, ctx.Err())
			}
			reasons = append(reasons, node+": no answer")
			continue
		}
		if rep.Unserved != "" {
			reasons = append(reasons, fmt.Sprintf("%s: %s", node, rep.Detail))
			continue
		}
		r.markAnswered(statelog.PartitionID{}, node)
		if rep.Err != nil {
			return nil, decodeError(rep.Err)
		}
		value, err := x.decode(rep.Result)
		if err != nil {
			return nil, fmt.Errorf("estate: %s: %s answered with a result this "+
				"build cannot decode: %w", spec.name, node, err)
		}
		return value, nil
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("%w: no live node holds data, so %s has nothing to "+
			"ask — give a node the `data` role", ErrNoDataNode, spec.name)
	}
	return nil, fmt.Errorf("%w for %s: %v", ErrNoDataNode, spec.name, reasons)
}

// order is the order p's holders are asked in: sticky for p first, then
// rendezvous, with every suspect node last — and never this node, which the
// router asks in-process or not at all.
func (r *Router) order(p statelog.PartitionID, nodes []string) []string {
	nodes = slices.DeleteFunc(slices.Clone(nodes), func(n string) bool { return n == r.self || n == "" })
	r.mu.Lock()
	sticky := r.sticky[p]
	now := r.now()
	suspect := map[string]bool{}
	for n, until := range r.suspect {
		if now.Before(until) {
			suspect[n] = true
		} else {
			delete(r.suspect, n)
		}
	}
	r.mu.Unlock()
	slices.SortStableFunc(nodes, func(a, b string) int {
		// SUSPECT LAST, then STICKY FIRST, then rendezvous.
		if suspect[a] != suspect[b] {
			if suspect[a] {
				return 1
			}
			return -1
		}
		if (a == sticky) != (b == sticky) {
			if a == sticky {
				return -1
			}
			return 1
		}
		wa, wb := weight(r.self, a), weight(r.self, b)
		switch {
		case wa > wb:
			return -1
		case wa < wb:
			return 1
		}
		return 0
	})
	return nodes
}

// weight is a rendezvous score for one asker and one serving node.
func weight(asker, node string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(asker))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(node))
	return h.Sum64()
}

// attemptContext is one attempt's context: the caller's, capped by budget.
//
// A ZERO budget is the caller's own deadline, unchanged.
func attemptContext(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); budget == 0 || (ok && time.Until(deadline) < budget) {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, budget)
}

func (r *Router) markSuspect(node string) {
	r.mu.Lock()
	r.suspect[node] = r.now().Add(suspectFor)
	for p, n := range r.sticky {
		if n == node {
			delete(r.sticky, p)
		}
	}
	r.mu.Unlock()
	// OUTSIDE THE LOCK: the placement is somebody else's, and a request
	// path must not hold this router's mutex across it.
	r.placement.Unanswered(node)
}

func (r *Router) markAnswered(p statelog.PartitionID, node string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sticky[p] = node
	delete(r.suspect, node)
}

// appendedNothing reports a write the write authority refused at gate 3 — this
// node does not serve the log's partition (`not_holder`), or could not tell
// whether it does (`holding_unknown`). Nothing was appended under the
// operation's id, so every class moves on to the next holder with it.
func appendedNothing(err error) bool {
	var refusal *statelog.Unavailable
	return errors.As(err, &refusal) && (refusal.Reason == statelog.ReasonNotHolder ||
		refusal.Reason == statelog.ReasonHoldingUnknown)
}

// unvouched reports an idempotent write's answer that is NOT final: an
// `unknown` the answering holder's operation ledger could not vouch for
// ([statelog.Result.Unvouched]), or a walking gesture that stopped at such a
// step ([tracker.ErrStepUnvouched]). Another holder's ledger may hold the row
// this one lost, so the router asks it, under the same operation id, before
// reporting the outcome unknown. Never true for another class: a read has no
// ledger, and a once-write is never repeated.
func unvouched(spec *opSpec, value any, err error) bool {
	if spec.class != opIdempotentWrite {
		return false
	}
	if err != nil {
		return errors.Is(err, tracker.ErrStepUnvouched)
	}
	res, ok := resultIn(value)
	return ok && res.Outcome == statelog.OutcomeUnknown && res.Unvouched
}

var resultType = reflect.TypeFor[statelog.Result]()

// resultIn is the write outcome an answer carries: the answer itself, or the
// first [statelog.Result] among its fields, embedded ones searched first and
// depth first — every write answer here either is one or embeds one.
func resultIn(value any) (statelog.Result, bool) {
	v := reflect.ValueOf(value)
	for v.IsValid() && v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return statelog.Result{}, false
		}
		v = v.Elem()
	}
	return findResult(v)
}

func findResult(v reflect.Value) (statelog.Result, bool) {
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return statelog.Result{}, false
	}
	if v.Type() == resultType {
		return v.Interface().(statelog.Result), true
	}
	for i := range v.NumField() {
		if f := v.Type().Field(i); f.Anonymous && f.IsExported() {
			if res, ok := findResult(v.Field(i)); ok {
				return res, true
			}
		}
	}
	return statelog.Result{}, false
}

// Serves answers the admission question for a seat on this node: whether p's
// copy that will answer the seat's reads admits one now, and which halves it
// runs natively.
//
// ASKED OF THE COPY THAT WILL SERVE THE SEAT. Where this node serves p, that
// is its own copy — the router asks it first for every read — so the answer is
// the local one and is never passed over for a peer's: a node whose own copy
// is one record behind withholds its claims, because a seat attaching to it
// would act on rows that are behind. Where it does not — a node without data,
// or one whose copy of p is faulted — it is the first holder that answers, and
// that answer is trusted for [admissionTrust].
func (r *Router) Serves(ctx context.Context, p statelog.PartitionID) (tracker, pages bool, err error) {
	if r.local != nil {
		if b, ok := r.local.For(ctx, p); ok {
			if b.Admits != nil && !b.Admits(ctx) {
				return false, false, nil
			}
			return b.Tracker != nil, b.Pages != nil, nil
		}
	}
	r.mu.Lock()
	cached, ok := r.admitted[p]
	r.mu.Unlock()
	if ok && r.now().Sub(cached.at) < admissionTrust {
		return cached.tracker, cached.pages, cached.err
	}
	out, err := call(ctx, r, opPing, nil, pingArgs{Partition: p.String()})
	r.mu.Lock()
	r.admitted[p] = admission{at: r.now(), tracker: out.Tracker, pages: out.Pages, err: err}
	r.mu.Unlock()
	return out.Tracker, out.Pages, err
}
