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

// Placement is how the router learns which data nodes to ask. Declared here,
// by the consumer: the engine's answer is every live data node, from its
// watched view of the fleet's presence leases.
//
// ASKED ON EVERY REQUEST, so every answer comes from memory — a watched view,
// never a listing of the fleet per call.
type Placement interface {
	// Holders is every data node that holds the estate, in no particular
	// order. An error is UNKNOWN, never "no holder": an empty list is a
	// fleet with no data node in it.
	Holders() ([]string, error)

	// Unanswered tells the placement a node it named did not answer, so a
	// node that left on a clean stop is gone from the next answer rather
	// than from the one a heartbeat later. It must not block.
	Unanswered(node string)
}

// LocalBackends is this data node's own copy of the estate.
type LocalBackends interface {
	// For is the backend over this node's copy, or false when the copy is
	// OUT OF SERVICE right now: wrong rather than behind, so the node
	// answers nothing from it while its seats read the estate from the
	// other data nodes.
	//
	// It takes a context because what a copy may answer is read from the
	// log's broker ([Backend.Answers]); an implementation answers from a
	// verdict it refreshes, never with a broker round trip per request.
	For(ctx context.Context) (Backend, bool)
}

// RouterOptions are a router's dependencies.
type RouterOptions struct {
	// Self is this node, which asks — named on every request so a serving
	// node's log says who it answered, and never asked over the wire.
	Self string

	Queue     Asker
	Placement Placement

	// Local is this node's own copy, nil on a node without the `data`
	// role, which holds none.
	Local LocalBackends

	// Session is THIS NODE's floors ([Session]): one per node, shared with
	// whatever else observes a position a seat must read past.
	Session *Session

	// Seams are this node's own: supplied to every operation this node
	// answers in-process.
	Seams ServerSeams

	// Now is the clock the suspect cooldown and the admission cache read,
	// injected for tests. Nil is the wall clock.
	Now func() time.Time
}

// Router routes every estate operation to a data node: this node, in-process,
// where it holds the estate and its copy serves; otherwise the other data
// nodes over the broker, one subject per node.
//
// # One per node, on every node
//
// A node without the `data` role holds nothing and asks for everything; a data
// node answers its own seats from its own copy, and asks a peer only while
// that copy is out of service or cannot take the request. Both are the same
// router, so a tool behaves identically on either, and the path a data node
// takes to its own copy is the path it takes to a peer's, floors included.
//
// # The order data nodes are asked in
//
// This node first, where its copy serves. Then the node that answered last
// (sticky: its applier is the one most likely to have this node's writes),
// then a rendezvous order that spreads askers across the data nodes, with
// every node that recently went silent last. A node that ran nothing is
// passed over by every class of operation; what may be repeated after a node
// MAY have run it is the class's ([opClass]).
type Router struct {
	self      string
	queue     Asker
	placement Placement
	local     LocalBackends
	session   *Session
	seams     ServerSeams
	now       func() time.Time

	// readBudget and writeBudget are [ReadAttempt] and [writeAttempt], and
	// admissionBudget [admissionAsk], held so a test can shorten them
	// rather than wait out a dead node.
	readBudget, writeBudget, admissionBudget time.Duration

	mu sync.Mutex
	// sticky is the node that answered last — asked first next time.
	sticky string
	// suspect is when each node that went unanswered may be asked first
	// again.
	suspect map[string]time.Time
	// admitted caches a REMOTE node's answer to the admission question —
	// see [Router.Serves] — nil until one has been asked.
	admitted *admission
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
		readBudget: ReadAttempt, writeBudget: writeAttempt, admissionBudget: admissionAsk,
		suspect: map[string]time.Time{},
	}, nil
}

// ReadAttempt bounds one read's wait on one node.
//
// TEN SECONDS: a serving node may wait [statelog.ReadBudget] for the floor and
// again for the level a read asked for, and then runs the query — so a node
// that has not answered in several times that is one that is gone rather than
// slow, and the next one answers sooner than it would. It is a ceiling under
// the caller's own deadline, never over it.
const ReadAttempt = 10 * time.Second

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

// admissionTrust is how long a remote data node's answer to the admission
// question is trusted.
//
// FIVE SECONDS: the admission gate is asked on every placement sweep, and a
// fresh ask per sweep is a request to a data node per heartbeat per node for
// an answer that changes when a node joins or leaves — which the presence
// lease's own TTL already bounds at the same order. A LOCAL answer is never
// cached: it costs no request, and a node's own copy falling behind must
// withhold its next claim at once.
const admissionTrust = 5 * time.Second

// admissionAsk bounds the REMOTE half of the admission question as a whole —
// every data node it asks, together — where [ReadAttempt] bounds only each one.
//
// FIVE SECONDS, what a node holding no data waited before the router, and for
// its reason: admission is asked on every placement sweep, synchronously — the
// first one at boot, before the host starts — and each data node that is
// listed but silent (wedged, cut off, restarting while its presence lease runs
// out) would otherwise cost the sweep a whole read attempt, ten seconds apiece,
// with its claims and its sheds waiting behind it. A node that has not
// answered in five is not one a seat should attach to on this sweep: the
// router has marked it suspect, and the next sweep — a heartbeat later — asks
// the others first.
const admissionAsk = 5 * time.Second

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
	// local runs the operation in-process on this node's copy.
	local func(ctx context.Context, b Backend) (any, error)

	// encoded is the arguments on the wire, encoded once.
	encoded json.RawMessage

	// decode is a remote answer's result, in the caller's type.
	decode func(raw json.RawMessage) (any, error)

	// written is the asking turn's [tracker.WriteLog], which every node
	// that RAN the operation remotely answers into ([reply.Written]) — the
	// one an answer kept is not the only one that committed, since a walk
	// asks the next node after an unvouched one. Nil where the actor holds
	// none; an in-process run reports into it directly.
	written tracker.WriteLog
}

// call runs one operation on a data node whose copy serves.
func call[A, R any](ctx context.Context, r *Router, o op[A, R], actor *Actor, args A) (R, error) {
	var zero R
	spec := o.spec
	var written tracker.WriteLog
	if actor != nil && actor.Provenance.Written != nil {
		// THE TURN'S SET OF ITEMS IT WROTE is in this process, so a node
		// that runs the write elsewhere is asked to name them instead —
		// on a copy, so the caller's actor is never changed under it.
		written = actor.Provenance.Written
		asked := *actor
		asked.Records = true
		actor = &asked
	}
	if o.repeatable != nil && o.repeatable(args) {
		// A KEYED ONCE-WRITE is repeated as an idempotent write is: a copy
		// of its spec, for this call alone ([op.repeatableWhen]).
		keyed := *spec
		keyed.class = opIdempotentWrite
		spec = &keyed
	}
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
		written: written,
	}
	answer, err := r.route(ctx, spec, actor, x)
	if out, ok := answer.(R); ok {
		return out, err
	}
	return zero, err
}

// ErrUnserved is an operation no data node ran: every node the asker's view
// named — this node among them where it holds the estate — answered that its
// copy is out of service, could not run it, was behind, or did not answer at
// all.
//
// NEVER A FALSE "NOT FOUND". An operation nobody answered for is refused
// saying so, because a read reported as empty would say the company has none
// of what it asked about.
type ErrUnserved struct {
	// Detail is who was asked and what each said, in order.
	Detail string
}

func (e *ErrUnserved) Error() string {
	if e.Detail == "" {
		return "estate: no data node serves the estate right now"
	}
	return "estate: no data node serves the estate right now: " + e.Detail
}

// held is an answer the router keeps while it asks another node: one a node
// gave that is not FINAL for its class ([opIdempotentWrite]'s `unvouched`),
// and is the answer if no node gives a better one.
type held struct {
	value any
	err   error
}

// walk is one request's account of the nodes it asked: why each ran nothing,
// the answer it keeps while it asks the next, and the copies it passed over
// for LAGGING their logs, which it comes back to last.
type walk struct {
	reasons  []string
	fallback *held

	// lagging is every node that answered [unservedLagging], and
	// laggingHere whether this node's own copy did.
	lagging     []string
	laggingHere bool
}

func (w *walk) note(format string, args ...any) {
	w.reasons = append(w.reasons, fmt.Sprintf(format, args...))
}

// route runs one operation — this node first where its copy serves, then the
// other data nodes in order, and LAST the copies that answered that they lag
// their logs.
//
// # A copy that lags is a worse node to ask, never no node
//
// [Backend.Answers] says whether a copy is level, or drained and within the
// snapshot slack of its logs' ends — a copy that is not is one a request should
// not choose while another node's is. But it is the copy's DISTANCE from its
// log, not its correctness: the floors a request carries and the level a read
// asks for are what hold an answer to what the caller must see, and the write
// authority decides from its own snapshot and lets the broker arbitrate. So a
// copy that lags is asked again, told to take the request anyway
// ([request.AcceptLagging]), once every node whose copy answers has run
// nothing — rather than refused as though the estate had no server. A single
// data node whose applier a burst put a thousand records behind refused every
// call its own seats made until it caught up; a fleet the same burst put
// behind together refused every node's.
func (r *Router) route(ctx context.Context, spec *opSpec, actor *Actor, x exchange) (any, error) {
	w := &walk{}
	// THIS NODE FIRST, where its copy serves — in-process, with the node's
	// floors enforced exactly as a remote node enforces them.
	if value, final, ranErr := r.tryLocal(ctx, spec, x, false, w); final {
		return value, ranErr
	}
	budget := r.budgetFor(ctx, spec)
	nodes, err := r.placement.Holders()
	if err != nil {
		// A PLACEMENT THAT CANNOT SAY WHO HOLDS THE ESTATE names no peer
		// to ask, and takes nothing from this node's own copy, which the
		// walk passed over above only for LAGGING: this node knows it
		// holds the estate without asking anybody, so that copy is still
		// the worse node to ask it always is rather than no node. Returned
		// here before it was asked, a single data node whose copy a burst
		// put behind refused every call its own seats made for as long as
		// the presence view could not answer — while it kept those seats,
		// rightly: a node that answers from its own copy needs no view to
		// route.
		if value, final, ranErr := r.lastResort(ctx, spec, actor, x, budget, w); final {
			return value, ranErr
		}
		if w.fallback != nil {
			return w.fallback.value, w.fallback.err
		}
		if len(w.reasons) > 0 {
			return nil, fmt.Errorf("estate: %s: read who holds the estate: %w (%s)", spec.name, err,
				strings.Join(w.reasons, "; "))
		}
		return nil, fmt.Errorf("estate: %s: read who holds the estate: %w", spec.name, err)
	}
	for _, node := range r.order(nodes) {
		rep, answered, err := r.askFor(ctx, spec, actor, x, node, budget, false)
		if err != nil {
			return nil, err
		}
		if !answered {
			if err := r.silent(ctx, spec, node, w); err != nil {
				return nil, err
			}
			continue
		}
		if value, final, ranErr := r.settle(spec, x, node, rep, w); final {
			return value, ranErr
		}
	}
	if value, final, ranErr := r.lastResort(ctx, spec, actor, x, budget, w); final {
		return value, ranErr
	}
	if w.fallback != nil {
		return w.fallback.value, w.fallback.err
	}
	return nil, &ErrUnserved{Detail: strings.Join(w.reasons, "; ")}
}

// lastResort asks the copies the walk passed over for lagging their logs —
// this node's own first, where it lagged, then each data node that said so —
// telling each to take the request anyway. final is set when one of them gives
// the answer.
func (r *Router) lastResort(ctx context.Context, spec *opSpec, actor *Actor, x exchange,
	budget time.Duration, w *walk) (value any, final bool, err error) {

	if w.laggingHere {
		w.laggingHere = false
		if value, final, ranErr := r.tryLocal(ctx, spec, x, true, w); final {
			return value, true, ranErr
		}
	}
	lagging := w.lagging
	w.lagging = nil
	for _, node := range lagging {
		rep, answered, askErr := r.askFor(ctx, spec, actor, x, node, budget, true)
		if askErr != nil {
			return nil, true, askErr
		}
		if !answered {
			if silentErr := r.silent(ctx, spec, node, w); silentErr != nil {
				return nil, true, silentErr
			}
			continue
		}
		if value, final, ranErr := r.settle(spec, x, node, rep, w); final {
			return value, true, ranErr
		}
	}
	return nil, false, nil
}

// tryLocal runs the operation on this node's own copy, where it serves. final
// is set when that is the answer; otherwise the walk says why not, and the
// router asks the other data nodes.
func (r *Router) tryLocal(ctx context.Context, spec *opSpec, x exchange, acceptLagging bool,
	w *walk) (value any, final bool, err error) {

	if r.local == nil {
		return nil, false, nil
	}
	b, ok := r.local.For(ctx)
	if !ok {
		return nil, false, nil
	}
	b.ServerSeams = r.seams
	value, refusal, why, ran := r.runLocal(ctx, spec, b, x, acceptLagging)
	switch {
	case why != "":
		if refusal == unservedLagging {
			w.laggingHere = true
		}
		w.note("%s: %s", r.self, why)
	case unvouched(spec, value, ran):
		w.fallback = &held{value: value, err: ran}
	default:
		return value, true, ran
	}
	return nil, false, nil
}

// askFor asks one data node to run the operation, carrying this node's floor
// on the operation's log, and stops carrying every floor the node says is on a
// generation its log abandoned. err is set only where the request could not be
// made at all.
func (r *Router) askFor(ctx context.Context, spec *opSpec, actor *Actor, x exchange,
	node string, budget time.Duration, acceptLagging bool) (reply, bool, error) {

	floors := r.floorsFor(spec)
	rep, answered, err := r.ask(ctx, node, request{
		Op: spec.name, Args: x.encoded, Actor: actor,
		Floors: floors, From: r.self, AcceptLagging: acceptLagging,
	}, budget)
	if err != nil {
		return reply{}, false, fmt.Errorf("estate: %s: %w", spec.name, err)
	}
	if answered {
		r.session.Forget(named(floors, rep.Obsolete)...)
		x.record(rep)
	}
	return rep, answered, nil
}

// silent is what a node that did not answer costs the walk: nothing more for
// a class that may be repeated, the request itself for a once-write — which
// that node may have run — and the caller's own ending.
func (r *Router) silent(ctx context.Context, spec *opSpec, node string, w *walk) error {
	if spec.class == opOnceWrite {
		return fmt.Errorf("%w (%s, asked of %s)", ErrOutcomeUnknown, spec.name, node)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("estate: %s: %w", spec.name, ctx.Err())
	}
	w.note("%s: no answer", node)
	return nil
}

// settle is what one node's reply decides: final when it is the answer, and
// otherwise noted on the walk — a node that ran nothing, or an answer not final
// for its class, kept in case no node gives a better one.
func (r *Router) settle(spec *opSpec, x exchange, node string, rep reply, w *walk) (
	value any, final bool, err error) {

	if rep.Unserved != "" {
		if rep.Unserved == unservedLagging {
			w.lagging = append(w.lagging, node)
		}
		w.note("%s: %s", node, rep.Detail)
		return nil, false, nil
	}
	r.markAnswered(node)
	if rep.Err != nil {
		failure := decodeError(rep.Err)
		if unvouched(spec, nil, failure) {
			w.fallback = &held{err: failure}
			w.note("%s: unvouched", node)
			return nil, false, nil
		}
		return nil, true, failure
	}
	value, err = x.decode(rep.Result)
	if err != nil {
		return nil, true, fmt.Errorf("estate: %s: %s answered with a result this build "+
			"cannot decode: %w", spec.name, node, err)
	}
	if unvouched(spec, value, nil) {
		w.fallback = &held{value: value}
		w.note("%s: unvouched", node)
		return nil, false, nil
	}
	return value, true, nil
}

// record adds what a node's write committed to into the asking turn's own
// set ([exchange.written]).
func (x exchange) record(rep reply) {
	if x.written == nil {
		return
	}
	for _, item := range rep.Written {
		x.written.Add(item)
	}
}

// budgetFor is one attempt's budget on one data node: [ReadAttempt] for a read,
// and for a write [writeAttempt] — or, where the caller has a deadline, that
// deadline, since a gesture the caller gave five minutes must not be
// abandoned after one.
func (r *Router) budgetFor(ctx context.Context, spec *opSpec) time.Duration {
	if spec.class == opRead {
		return r.readBudget
	}
	if _, bounded := ctx.Deadline(); bounded {
		return 0
	}
	return r.writeBudget
}

// runLocal runs one operation in-process on this node's own copy. why is set,
// and nothing ran, when the copy could not take it — the same answers a remote
// node gives, and the router moves on from each — with refusal the reason a
// remote node would have answered.
func (r *Router) runLocal(ctx context.Context, spec *opSpec, b Backend, x exchange,
	acceptLagging bool) (value any, refusal unservedReason, why string, err error) {

	reason, detail, obsolete := ready(ctx, r.self, spec, b, r.floorsFor(spec), acceptLagging)
	r.session.Forget(obsolete...)
	if reason != "" {
		return nil, reason, detail, nil
	}
	value, err = x.local(ctx, b)
	switch {
	case errors.Is(err, errNoHalf):
		return nil, unservedNoBackend, fmt.Sprintf("%s runs no native backend for %s",
			r.self, spec.name), nil
	case errors.Is(err, errNotAdmitting):
		return nil, unservedNotEstablished, fmt.Sprintf("%s's copy admits no seat yet",
			r.self), nil
	}
	return value, "", "", err
}

// floorsFor is the floors a request for spec carries: this node's session on
// the log of the operation's own domain ([opSpec.floorStream]), or none for an
// operation that reads no log position.
func (r *Router) floorsFor(spec *opSpec) []statelog.Position {
	if spec.floorStream == "" {
		return nil
	}
	return r.session.Floors([]string{spec.floorStream})
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

// order is the order the data nodes are asked in: sticky first, then
// rendezvous, with every suspect node last — and never this node, which the
// router asks in-process or not at all.
func (r *Router) order(nodes []string) []string {
	nodes = slices.DeleteFunc(slices.Clone(nodes), func(n string) bool { return n == r.self || n == "" })
	r.mu.Lock()
	sticky := r.sticky
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
	if r.sticky == node {
		r.sticky = ""
	}
	r.mu.Unlock()
	// OUTSIDE THE LOCK: the placement is somebody else's, and a request
	// path must not hold this router's mutex across it.
	r.placement.Unanswered(node)
}

func (r *Router) markAnswered(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sticky = node
	delete(r.suspect, node)
}

// unvouched reports an idempotent write's answer that is NOT final: an
// `unknown` the answering node's operation ledger could not vouch for
// ([statelog.Result.Unvouched]) on ANY step the answer reports — a walking
// gesture answers one outcome per step ([tracker.PlaceResult]), and a page
// write names its outcome rather than embedding it ([pages.Written]) — or a
// walking gesture that stopped at such a step ([tracker.ErrStepUnvouched]).
// Another node's ledger may hold the row this one lost, so the router asks
// it, under the same operation id, before reporting the outcome unknown. Never
// true for another class: a read has no ledger, and a once-write is never
// repeated.
func unvouched(spec *opSpec, value any, err error) bool {
	if spec.class != opIdempotentWrite {
		return false
	}
	if err != nil {
		return errors.Is(err, tracker.ErrStepUnvouched)
	}
	for _, res := range resultsIn(value) {
		if res.Outcome == statelog.OutcomeUnknown && res.Unvouched {
			return true
		}
	}
	return false
}

var resultType = reflect.TypeFor[statelog.Result]()

// resultsIn is every write outcome an answer carries: the answer itself, or
// each [statelog.Result] among its exported struct fields, embedded or named,
// depth first — every write answer here either is one, embeds one, or names
// one per step it took.
func resultsIn(value any) []statelog.Result {
	v := reflect.ValueOf(value)
	for v.IsValid() && v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	return findResults(v, nil)
}

func findResults(v reflect.Value, out []statelog.Result) []statelog.Result {
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return out
	}
	if v.Type() == resultType {
		return append(out, v.Interface().(statelog.Result))
	}
	for i := range v.NumField() {
		if v.Type().Field(i).IsExported() {
			out = findResults(v.Field(i), out)
		}
	}
	return out
}

// Serves answers the admission question for a seat on this node: whether the
// copy that will answer the seat's reads admits one now, and which halves it
// runs natively.
//
// ASKED OF THE COPY THAT WILL SERVE THE SEAT. Where this node's own copy
// serves, that is the one — the router asks it first for every read — so the
// answer is the local one and is never passed over for a peer's: a node whose
// own copy is one record behind withholds its claims, because a seat attaching
// to it would act on rows that are behind. Where it does not — a node without
// data, or one whose copy is out of service — it is the first data node that
// answers, and that answer is trusted for [admissionTrust].
func (r *Router) Serves(ctx context.Context) (tracker, pages bool, err error) {
	if r.local != nil {
		if b, ok := r.local.For(ctx); ok {
			if b.Admits != nil && !b.Admits(ctx) {
				return false, false, nil
			}
			return b.Tracker != nil, b.Pages != nil, nil
		}
	}
	r.mu.Lock()
	cached := r.admitted
	r.mu.Unlock()
	if cached != nil && r.now().Sub(cached.at) < admissionTrust {
		return cached.tracker, cached.pages, cached.err
	}
	asked, cancel := context.WithTimeout(ctx, r.admissionBudget)
	defer cancel()
	out, err := call(asked, r, opPing, nil, struct{}{})
	r.mu.Lock()
	r.admitted = &admission{at: r.now(), tracker: out.Tracker, pages: out.Pages, err: err}
	r.mu.Unlock()
	return out.Tracker, out.Pages, err
}
