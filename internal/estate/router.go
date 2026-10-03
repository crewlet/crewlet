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
	// joining it, has begun to leave it ([statelog.Holding], the answer the
	// write authority's gate 3 reads), or its copy of p is wrong.
	//
	// THREE-VALUED, as gate 3 is: an error is a node that cannot TELL
	// whether it serves p — the holding answer could not be read — which is
	// neither a yes nor a no. A server answers it `holding_unknown` rather
	// than `not_holder`, whose map epoch would send an asker to re-read its
	// map over a question about this node, and the router moves on from it
	// either way. (The contract's `For(p) (Backend, bool)` collapsed the
	// two; see the package doc.)
	//
	// It takes a context because what a copy may answer is read from the
	// log's broker ([Backend.Answers]); an implementation answers from a
	// verdict it refreshes, never with a broker round trip per request.
	For(ctx context.Context, p statelog.PartitionID) (Backend, bool, error)

	// CPUs is the places a gather's query of these copies takes ([CPUs]):
	// this NODE's, never nil, and the SAME on every call — the server
	// answering other nodes' batches ([Serve]) and the router answering
	// this node's own gathers in-process both take them, and handed two,
	// each would run a query per CPU beside the other's.
	CPUs() *CPUs
}

// RouterOptions are a router's dependencies.
type RouterOptions struct {
	// Self is this node, which asks — named on every request so a serving
	// node's log says who it answered, and never asked over the wire.
	Self string

	Queue     Asker
	Placement Placement

	// Local is this node's own backends, nil on a node that holds no
	// partition at all — and the CPUs a gather's in-process queries take
	// ([LocalBackends.CPUs]), which are the ones this node's server
	// answers batches under.
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

	// cpus is local's ([LocalBackends.CPUs]), nil where local is: the
	// places this node's in-process gather queries take.
	cpus *CPUs

	// readBudget and writeBudget are [ReadAttempt] and [writeAttempt],
	// appendBudget [AppendAttempt], and admissionBudget [admissionAsk],
	// held so a test can shorten them rather than wait out a dead node.
	readBudget, writeBudget, appendBudget, admissionBudget time.Duration

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
	var cpus *CPUs
	if opts.Local != nil {
		if cpus = opts.Local.CPUs(); cpus == nil {
			return nil, errors.New("estate: a router over this node's own copies needs " +
				"the CPUs their queries take, and LocalBackends.CPUs answered none — " +
				"return the node's one CPUs, the one its server answers batches under")
		}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Router{
		self: opts.Self, queue: opts.Queue, placement: opts.Placement,
		local: opts.Local, cpus: cpus, session: opts.Session, seams: opts.Seams, now: now,
		readBudget: ReadAttempt, writeBudget: writeAttempt, appendBudget: AppendAttempt,
		admissionBudget: admissionAsk,
		sticky:          map[statelog.PartitionID]string{},
		suspect:         map[string]time.Time{},
		admitted:        map[statelog.PartitionID]admission{},
	}, nil
}

// ReadAttempt bounds one read's wait on one node — a single-partition read's,
// and a gather batch's however many partitions it carries.
//
// TEN SECONDS: a serving node may wait [statelog.ReadBudget] for the floor and
// again for the level a read asked for, and then runs the query — so a node
// that has not answered in several times that is one that is gone rather than
// slow, and the next one answers sooner than it would. It is a ceiling under
// the caller's own deadline, never over it.
//
// ONE ATTEMPT FOR A WHOLE BATCH TOO, because a batch's waits are concurrent —
// every partition's floor and barrier at once, only the queries a CPU's worth
// at a time ([CPUs]) — and its holder answers [batchMargin] before the
// attempt ends with whatever it finished, naming the rest unfinished for the
// next attempt. A budget scaled to the batch would instead wait out a holder
// that is gone for as long as its batch was large.
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

// admissionAsk bounds the REMOTE half of the admission question as a whole —
// every holder it asks, together — where [ReadAttempt] bounds only each one.
//
// FIVE SECONDS, what a node holding no data waited before the router, and for
// its reason: admission is asked on every placement sweep, synchronously — the
// first one at boot, before the host starts — and each holder that is listed
// but silent (wedged, cut off, restarting while its presence lease runs out)
// would otherwise cost the sweep a whole read attempt, ten seconds apiece, with
// its claims and its sheds waiting behind it. A holder that has not answered in
// five is not one a seat should attach to on this sweep: the router has marked
// it suspect, and the next sweep — a heartbeat later — asks the others first.
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
	// local runs the operation in-process on a backend this node serves.
	local func(ctx context.Context, b Backend) (any, error)

	// encoded is the arguments on the wire, encoded once.
	encoded json.RawMessage

	// decode is a remote answer's result, in the caller's type.
	decode func(raw json.RawMessage) (any, error)

	// resolve is the partition the arguments address under a layout —
	// asked again after a refresh, since a new map may be a new layout.
	resolve func(l statelog.Layout) (statelog.PartitionID, error)

	// answered is told the partition the answer came from and the cut it
	// was read at, once a holder's answer is the one returned — for an
	// operation whose answer reports its coverage ([opSpec.covered]). Nil
	// for every other.
	answered func(p statelog.PartitionID, at []statelog.Position)

	// from is told the node whose answer is the one returned — this node's
	// own id for one answered in-process — for a caller that reports WHO
	// did what it asked ([Router.Gate]). Nil for every other; never told
	// where no holder answered at all.
	from func(node string)

	// written is the asking turn's [tracker.WriteLog], which every holder
	// that RAN the operation remotely answers into ([reply.Written]) — the
	// one an answer kept is not the only one that committed, since a walk
	// asks the next holder after an unvouched one. Nil where the actor
	// holds none; an in-process run reports into it directly.
	written tracker.WriteLog
}

// call runs one operation on a node that serves its partition.
func call[A, R any](ctx context.Context, r *Router, o op[A, R], actor *Actor, args A) (R, error) {
	return callFrom(ctx, r, o, actor, args, nil)
}

// callFrom is [call], telling from which node gave the answer it returns
// ([exchange.from]).
func callFrom[A, R any](ctx context.Context, r *Router, o op[A, R], actor *Actor, args A,
	from func(node string)) (R, error) {
	var zero R
	spec := o.spec
	var written tracker.WriteLog
	if actor != nil && actor.Provenance.Written != nil {
		// THE TURN'S SET OF ITEMS IT WROTE is in this process, so a holder
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
		from:    from,
		written: written,
	}
	var answer any
	if o.addr.partitions == nil {
		answer, err = r.anyNode(ctx, spec, actor, x)
	} else {
		x.resolve = func(l statelog.Layout) (statelog.PartitionID, error) {
			parts, resolveErr := o.addr.partitions(ctx, l, layoutResolver{layout: l}, args)
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
		var cov statelog.Coverage
		if o.cover != nil {
			// ONE PARTITION, ANSWERED: what a single-partition read covers
			// is the partition it was read from, at the cut its holder
			// measured before the read began.
			x.answered = func(p statelog.PartitionID, at []statelog.Position) {
				cov = statelog.Coverage{Addressed: 1, Answered: []string{p.String()}, At: cutOf(at)}
			}
		}
		answer, err = r.route(ctx, spec, actor, x)
		if out, ok := answer.(R); ok && err == nil && o.cover != nil {
			o.cover(&out, cov)
			return out, nil
		}
	}
	if out, ok := answer.(R); ok {
		return out, err
	}
	return zero, err
}

// cutOf is a cut of positions, or nil for none.
func cutOf(at []statelog.Position) statelog.Cut {
	if len(at) == 0 {
		return nil
	}
	cut := make(statelog.Cut, len(at))
	for _, pos := range at {
		cut[pos.Stream] = pos
	}
	return cut
}

// held is an answer the router keeps while it asks another holder: one a
// holder gave that is not FINAL for its class ([opIdempotentWrite]'s
// `unvouched`), and is the answer if no holder gives a better one.
type held struct {
	value any
	err   error

	// node is the holder that gave it — this node's own id for its own
	// copy's answer ([exchange.from]).
	node string
}

// walk is one request's account of the holders it asked: why each ran
// nothing, the answer it keeps while it asks the next, and the copies it
// passed over for LAGGING their logs, which it comes back to last.
type walk struct {
	reasons  []string
	fallback *held

	// lagging is every holder that answered [unservedLagging], and
	// laggingHere the partition this node's own copy lagged on, if it did.
	lagging     []string
	laggingHere *statelog.PartitionID
}

func (w *walk) note(format string, args ...any) {
	w.reasons = append(w.reasons, fmt.Sprintf(format, args...))
}

// route runs one single-partition operation — this node first where it serves
// the partition, then the partition's holders in order, and LAST the copies
// that answered that they lag their logs.
//
// # A copy that lags is a worse holder, never no holder
//
// [Backend.Answers] says whether a copy is level, or drained and within the
// snapshot slack of its logs' ends — a copy that is not is one a request should
// not choose while another holder's is. But it is the copy's DISTANCE from its
// log, not its correctness: the floors a request carries and the level a read
// asks for are what hold an answer to what the caller must see, and the write
// authority decides from its own snapshot and lets the broker arbitrate. So a
// copy that lags is asked again, told to take the request anyway
// ([request.AcceptLagging]), once every holder whose copy answers has run
// nothing — rather than refused as though the partition had no server. A
// single data node whose applier a burst put a thousand records behind refused
// every call its own seats made until it caught up; a fleet the same burst put
// behind together refused every node's.
func (r *Router) route(ctx context.Context, spec *opSpec, actor *Actor, x exchange) (any, error) {
	layout, err := r.placement.Layout()
	if err != nil {
		return nil, fmt.Errorf("estate: %s: read which layout the fleet runs: %w", spec.name, err)
	}
	p, err := x.resolve(layout)
	if err != nil {
		return nil, fmt.Errorf("estate: %s: %w", spec.name, err)
	}
	w := &walk{}
	// THIS NODE FIRST, where it serves the partition — in-process, with
	// the node's floors enforced exactly as a remote holder enforces them.
	if value, final, ranErr := r.tryLocal(ctx, spec, layout, p, x, false, w); final {
		return value, ranErr
	}
	budget := r.budgetFor(ctx, spec)
	nodes, epoch, err := r.placement.Serving(p)
	if err != nil {
		// A PLACEMENT THAT CANNOT SAY WHO SERVES p names no peer to ask,
		// and takes nothing from this node's own copy, which the walk
		// passed over above only for LAGGING: this node knows it holds p
		// without asking anybody, so that copy is still the worse holder
		// it always is rather than no holder. Returned here before it was
		// asked, a single data node whose copy a burst put behind refused
		// every call its own seats made for as long as the presence view
		// could not answer — while it kept those seats, rightly: a
		// partition a node answers from its own copy is one it needs no
		// view to route.
		if value, final, ranErr := r.lastResort(ctx, spec, actor, x, layout, p, 0, budget, w); final {
			return value, ranErr
		}
		if w.fallback != nil {
			return x.kept(w.fallback)
		}
		if len(w.reasons) > 0 {
			return nil, fmt.Errorf("estate: %s: read who serves %s: %w (%s)", spec.name, p, err,
				strings.Join(w.reasons, "; "))
		}
		return nil, fmt.Errorf("estate: %s: read who serves %s: %w", spec.name, p, err)
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
			rep, answered, err := r.askFor(ctx, spec, actor, x, layout, p, epoch, node, budget, false)
			if err != nil {
				return nil, err
			}
			if !answered {
				if err := r.silent(ctx, spec, p, node, w); err != nil {
					return nil, err
				}
				continue
			}
			if rep.Unserved == unservedNotHolder && rep.Epoch > epoch && !refreshed {
				// THE SERVER KNOWS A NEWER MAP. Read ours again,
				// resolve again, and walk the new holders — once:
				// a server that keeps outrunning a view just read is
				// one this request does not wait for.
				refreshed = true
				w.note("%s: %s (at map epoch %d, this node routed by %d)", node, rep.Detail,
					rep.Epoch, epoch)
				fresh, freshEpoch, freshLayout, freshP, refreshErr := r.refresh(ctx, x, p)
				if refreshErr != nil {
					w.note("refresh: %v", refreshErr)
					continue
				}
				moved := freshP != p
				nodes, epoch, layout, p = fresh, freshEpoch, freshLayout, freshP
				if moved {
					// A NEW PARTITION, which this node may serve
					// although it did not serve the old one.
					if value, final, ranErr := r.tryLocal(ctx, spec, layout, p, x, false, w); final {
						return value, ranErr
					}
				}
				restart = true
				break
			}
			if value, final, ranErr := r.settle(spec, x, p, node, rep, w); final {
				return value, ranErr
			}
		}
		if !restart {
			break
		}
	}
	if value, final, ranErr := r.lastResort(ctx, spec, actor, x, layout, p, epoch, budget, w); final {
		return value, ranErr
	}
	if w.fallback != nil {
		return x.kept(w.fallback)
	}
	return nil, &ErrPartitionUnserved{Partition: p.String(), Detail: strings.Join(w.reasons, "; ")}
}

// lastResort asks the copies the walk passed over for lagging their logs —
// this node's own first, where it lagged on p, then each holder that said so —
// telling each to take the request anyway. final is set when one of them gives
// the answer.
func (r *Router) lastResort(ctx context.Context, spec *opSpec, actor *Actor, x exchange,
	layout statelog.Layout, p statelog.PartitionID, epoch uint64, budget time.Duration,
	w *walk) (value any, final bool, err error) {

	if here := w.laggingHere; here != nil && *here == p {
		w.laggingHere = nil
		if value, final, ranErr := r.tryLocal(ctx, spec, layout, p, x, true, w); final {
			return value, true, ranErr
		}
	}
	lagging := w.lagging
	w.lagging = nil
	for _, node := range lagging {
		rep, answered, askErr := r.askFor(ctx, spec, actor, x, layout, p, epoch, node, budget, true)
		if askErr != nil {
			return nil, true, askErr
		}
		if !answered {
			if silentErr := r.silent(ctx, spec, p, node, w); silentErr != nil {
				return nil, true, silentErr
			}
			continue
		}
		if value, final, ranErr := r.settle(spec, x, p, node, rep, w); final {
			return value, true, ranErr
		}
	}
	return nil, false, nil
}

// tryLocal runs the operation on this node's own copy of p, where it serves p.
// final is set when that is the answer; otherwise the walk says why not, and
// the router asks the partition's holders.
func (r *Router) tryLocal(ctx context.Context, spec *opSpec, layout statelog.Layout,
	p statelog.PartitionID, x exchange, acceptLagging bool, w *walk) (value any, final bool, err error) {

	if r.local == nil {
		return nil, false, nil
	}
	b, ok, unknown := r.local.For(ctx, p)
	if unknown != nil {
		w.note("%s: cannot tell whether it serves %s: %v", r.self, p, unknown)
	}
	if !ok {
		return nil, false, nil
	}
	b.ServerSeams = r.seams
	value, at, refusal, why, ran := r.runLocal(ctx, spec, b, layout, p, x, acceptLagging)
	switch {
	case why != "":
		if refusal == unservedLagging {
			w.laggingHere = &p
		}
		w.note("%s: %s", r.self, why)
	case appendedNothing(ran):
		w.note("%s: %v", r.self, ran)
	case unvouched(spec, value, ran):
		w.fallback = &held{value: value, err: ran, node: r.self}
	default:
		if ran == nil && x.answered != nil {
			x.answered(p, at)
		}
		x.tell(r.self)
		return value, true, ran
	}
	return nil, false, nil
}

// askFor asks one holder to run the operation on p, carrying this node's
// floors on p's logs, and stops carrying every floor the holder says is on a
// generation its log abandoned. err is set only where the request could not
// be made at all.
func (r *Router) askFor(ctx context.Context, spec *opSpec, actor *Actor, x exchange,
	layout statelog.Layout, p statelog.PartitionID, epoch uint64, node string,
	budget time.Duration, acceptLagging bool) (reply, bool, error) {

	floors := r.floorsFor(spec, layout, p)
	rep, answered, err := r.ask(ctx, node, request{
		Op: spec.name, Args: x.encoded, Actor: actor,
		Partitions: []string{p.String()}, MapEpoch: epoch,
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

// silent is what a holder that did not answer costs the walk: nothing more
// for a class that may be repeated, the request itself for a once-write —
// which that holder may have run — and the caller's own ending.
func (r *Router) silent(ctx context.Context, spec *opSpec, p statelog.PartitionID, node string,
	w *walk) error {

	if spec.class == opOnceWrite {
		return fmt.Errorf("%w (%s on %s, asked of %s)", ErrOutcomeUnknown, spec.name, p, node)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("estate: %s: %w", spec.name, ctx.Err())
	}
	w.note("%s: no answer", node)
	return nil
}

// settle is what one holder's reply decides: final when it is the answer, and
// otherwise noted on the walk — a holder that ran nothing, a write gate 3
// refused, which appended nothing, or an answer not final for its class, kept
// in case no holder gives a better one.
func (r *Router) settle(spec *opSpec, x exchange, p statelog.PartitionID, node string, rep reply,
	w *walk) (value any, final bool, err error) {

	if rep.Unserved != "" {
		if rep.Unserved == unservedLagging {
			w.lagging = append(w.lagging, node)
		}
		w.note("%s: %s", node, rep.Detail)
		return nil, false, nil
	}
	r.markAnswered(p, node)
	if rep.Err != nil {
		failure := decodeError(rep.Err)
		switch {
		case appendedNothing(failure):
			w.note("%s: %v", node, failure)
			return nil, false, nil
		case unvouched(spec, nil, failure):
			w.fallback = &held{err: failure, node: node}
			w.note("%s: unvouched", node)
			return nil, false, nil
		}
		x.tell(node)
		return nil, true, failure
	}
	value, err = x.decode(rep.Result)
	if err != nil {
		x.tell(node)
		return nil, true, fmt.Errorf("estate: %s: %s answered with a result this build "+
			"cannot decode: %w", spec.name, node, err)
	}
	if unvouched(spec, value, nil) {
		w.fallback = &held{value: value, node: node}
		w.note("%s: unvouched", node)
		return nil, false, nil
	}
	if x.answered != nil {
		x.answered(p, rep.At)
	}
	x.tell(node)
	return value, true, nil
}

// record adds what a holder's write committed to into the asking turn's own
// set ([exchange.written]).
func (x exchange) record(rep reply) {
	if x.written == nil {
		return
	}
	for _, item := range rep.Written {
		x.written.Add(item)
	}
}

// tell tells the caller which node gave the answer the router returns, where
// it asked ([exchange.from]).
func (x exchange) tell(node string) {
	if x.from != nil {
		x.from(node)
	}
}

// kept is the answer a walk kept while it asked on ([held]), as the router
// returns it.
func (x exchange) kept(h *held) (any, error) {
	x.tell(h.node)
	return h.value, h.err
}

// budgetFor is one attempt's budget on one holder: [ReadAttempt] for a read,
// and for a write [writeAttempt] — or, where the caller has a deadline, that
// deadline, since a gesture the caller gave five minutes must not be
// abandoned after one.
//
// AN OPERATION THAT IS ONE APPEND ([opSpec.oneAppend]) is held to
// [AppendAttempt] whatever the caller's deadline: its whole work is bounded by
// the write authority's own budgets, so a holder that has not answered in that
// time is silent rather than slow — and waited on until the caller's deadline,
// one silent holder would take the whole call with it before the next was
// asked.
func (r *Router) budgetFor(ctx context.Context, spec *opSpec) time.Duration {
	if spec.class == opRead {
		return r.readBudget
	}
	if spec.oneAppend {
		return r.appendBudget
	}
	if _, bounded := ctx.Deadline(); bounded {
		return 0
	}
	return r.writeBudget
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
// same answers a remote holder gives, and the router moves on from each —
// with refusal the reason a remote holder would have answered.
func (r *Router) runLocal(ctx context.Context, spec *opSpec, b Backend, layout statelog.Layout,
	p statelog.PartitionID, x exchange, acceptLagging bool) (
	value any, at []statelog.Position, refusal unservedReason, why string, err error) {

	floors := r.floorsFor(spec, layout, p)
	reason, detail, obsolete := ready(ctx, r.self, spec, b, p, floors, spec.floorStreams(layout, p),
		acceptLagging)
	r.session.Forget(obsolete...)
	if reason != "" {
		return nil, nil, reason, detail, nil
	}
	if spec.covered && !spec.floorless {
		// BEFORE THE READ, so the cut is one the answer holds at least.
		at = appliedAt(b, streamsOf(layout, p))
	}
	value, err = x.local(ctx, b)
	switch {
	case errors.Is(err, errNoHalf):
		return nil, nil, unservedNoBackend, fmt.Sprintf("%s runs no native backend for %s",
			r.self, spec.name), nil
	case errors.Is(err, errNotAdmitting):
		return nil, nil, unservedNotEstablished, fmt.Sprintf("%s's copy of %s admits no seat yet",
			r.self, p), nil
	}
	return value, at, "", "", err
}

// floorsFor is the floors a request for spec on p carries: this node's
// session on the logs of the operation's own domain in p ([address]), or none
// for an operation that reads no log position.
func (r *Router) floorsFor(spec *opSpec, layout statelog.Layout, p statelog.PartitionID) []statelog.Position {
	return r.session.Floors(spec.floorStreams(layout, p))
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
	budget := r.budgetFor(ctx, spec)
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
		x.record(rep)
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
// ([statelog.Result.Unvouched]) on ANY step the answer reports — a walking
// gesture answers one outcome per step ([tracker.PlaceResult]), and a page
// write names its outcome rather than embedding it ([pages.Written]) — or a
// walking gesture that stopped at such a step ([tracker.ErrStepUnvouched]).
// Another holder's ledger may hold the row this one lost, so the router asks
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
		// A NODE THAT CANNOT TELL WHETHER IT SERVES p is not the copy
		// that will serve the seat — the router passes it over too — so
		// the answer is a holder's that can.
		if b, ok, _ := r.local.For(ctx, p); ok {
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
	asked, cancel := context.WithTimeout(ctx, r.admissionBudget)
	defer cancel()
	out, err := call(asked, r, opPing, nil, pingArgs{Partition: p.String()})
	r.mu.Lock()
	r.admitted[p] = admission{at: r.now(), tracker: out.Tracker, pages: out.Pages, err: err}
	r.mu.Unlock()
	return out.Tracker, out.Pages, err
}
