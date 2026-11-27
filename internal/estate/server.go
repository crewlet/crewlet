package estate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

var log = logging.Get("estate")

// Server is what a queue answerer needs to register.
type Server interface {
	Serve(ctx context.Context, subject string, answers int, h queue.AnswerFunc) (queue.Unsubscribe, error)
}

// ServerOptions are a serving node's dependencies.
type ServerOptions struct {
	// Self is this node — the subjects it answers on, one a [lane], and the
	// name its replies carry.
	Self string

	// Local is this node's own copy of the estate.
	Local LocalBackends

	// Git is this node's git half, resolved per request ([LocalGit]), on a
	// node with the `gitlet` role (each holds data in this release), and nil
	// on every other. Where set, the node also registers the git lanes from
	// boot ([gitLanes]), each answering `no_backend` until Gitlet is built.
	Git LocalGit

	// Seams are this node's own: supplied to every operation it answers.
	Seams ServerSeams

	// Envelopes verify every request this node is sent and sign every
	// reply it answers with — required: see [Envelopes].
	Envelopes *Envelopes

	// Now is this node's clock, injected for tests: the one a request's
	// deadline is admitted on. Nil is the wall clock.
	Now func() time.Time

	// Answers is the node's answer budget a read answering a page or a
	// capped list holds its weight of until its reply is written
	// ([answerShape.weight]). Nil is a budget of this server's own.
	Answers *queue.AnswerBudget
}

// Serve makes this node answer the fleet on its own subjects, one a [lane],
// from its own copy of the estate, and `out_of_service` for the operations of
// a domain whose copy is wrong (ADR-0079). The returned withdrawal ends every
// lane.
//
// Every data node serves from BEFORE its runtime is up, the backend resolved
// per request: a node joins without reconfiguring the others, and the first
// seat to ask hears "not here" rather than failing until a late registration.
//
// EVERY LANE OR NONE: a node answering reads but no proposal would cost every
// push it commits a whole write attempt, and a Gitlet node missing its git
// reads or engine writes ([gitLanes]) would be a silent home to every walk.
func Serve(ctx context.Context, q Server, opts ServerOptions) (queue.Unsubscribe, error) {
	switch {
	case q == nil || opts.Local == nil:
		return nil, errors.New("estate: serve needs a queue and this node's backends")
	case opts.Self == "":
		return nil, errors.New("estate: serve needs this node's id — it is the subject other nodes ask")
	case opts.Envelopes == nil:
		return nil, errNoEnvelopes
	}
	if opts.Answers == nil {
		opts.Answers = queue.NewAnswerBudget(nil)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	// ONE SERVER FOR EVERY LANE, so one guard remembers every request id
	// the node has taken whichever lane it arrived on: a replay sent to
	// another lane's subject is the same replay.
	srv := server{self: opts.Self, local: opts.Local, git: opts.Git, seams: opts.Seams,
		env: opts.Envelopes, now: now, seen: signedask.NewGuard(), answers: opts.Answers,
		writes: make(chan struct{}, gitWritesHeld)}
	registered := lanes
	if opts.Git != nil {
		registered = slices.Concat(lanes, gitLanes)
	}
	stops := make([]queue.Unsubscribe, 0, len(registered))
	withdraw := func(ctx context.Context) error {
		var errs []error
		for _, stop := range slices.Backward(stops) {
			errs = append(errs, stop(ctx))
		}
		return errors.Join(errs...)
	}
	for _, l := range registered {
		stop, err := q.Serve(ctx, l.subject(opts.Self), l.answers(),
			func(ctx context.Context, raw []byte) ([]byte, error) {
				return srv.answer(ctx, l, raw), nil
			})
		if err != nil {
			// A ROLLBACK, so it takes a context the failure cannot
			// have ended: each answer it waits for is bounded by its
			// own asker's wait, counted from its arrival.
			if undo := withdraw(context.WithoutCancel(ctx)); undo != nil {
				err = errors.Join(err, undo)
			}
			return nil, fmt.Errorf("estate: serve %s on %s: %w", l, l.subject(opts.Self), err)
		}
		stops = append(stops, stop)
	}
	return withdraw, nil
}

// server is one node's serving half.
type server struct {
	self  string
	local LocalBackends
	git   LocalGit
	seams ServerSeams
	env   *Envelopes
	now   func() time.Time
	seen  *signedask.Guard
	// answers is the node's answer budget ([ServerOptions.Answers]).
	answers *queue.AnswerBudget

	// writes is the [gitWritesHeld] engine writes this node holds for its
	// peers: a slot taken by every write it decodes and runs, until it is
	// answered, and none left the next is answered at once ([server.answerGit]).
	writes chan struct{}
}

// answer runs one request that arrived on lane l and ALWAYS answers, every
// failure included: a silent node is indistinguishable from a down one, and
// its asker would wait out its budget before trying the next.
func (s server) answer(ctx context.Context, l lane, raw []byte) []byte {
	out := reply{Node: s.self}
	// THE REQUEST'S ARRIVAL at this process, stamped by the queue before any
	// wait for a lane's slot (queue.Arrived), on the runtime clock every wait
	// for it is counted on; the node's own clock admits it.
	arrived := queue.Arrived(ctx)
	admitted := s.env.admit(raw, s.seen, s.now())
	switch admitted.Disposition {
	case signedask.Tampered:
		// NOT THE FLEET'S, so nothing in it is believed, not even the id a
		// reply would name. At ERROR: no holder will ever accept it, so it is
		// somebody outside the fleet, or a node with a foreign keyring.
		log.ErrorContext(ctx, "estate_request_refused", "node", s.self,
			"disposition", string(admitted.Disposition), "detail", admitted.Why+"; nothing in it ran")
		out.Unserved, out.Detail = unservedUnauthenticated, fmt.Sprintf(
			"%s refused a request that is not signed by this fleet", s.self)
		return s.encodeReply(out)
	case signedask.KeyUnknown:
		// A FACT ABOUT THIS NODE, which an operator changes by adding the
		// key: answered "did not run", naming the id the asker sent, so
		// it asks a node that holds the key.
		log.WarnContext(ctx, "estate_request_refused", "node", s.self,
			"disposition", string(admitted.Disposition), "claimed_from", admitted.Request.From,
			"held", s.env.heldKeys(), "detail", admitted.Why+"; nothing in it ran")
		out.Request, out.Unserved, out.Detail = admitted.ID, unservedUnauthenticated, fmt.Sprintf(
			"%s does not hold the key the request is signed under (it holds %s)",
			s.self, words.And(s.env.heldKeys()))
		return s.encodeReply(out)
	case signedask.Undecodable:
		out.Err = encodeError(fmt.Errorf("estate: %s could not decode a request the "+
			"fleet signed: %s", s.self, admitted.Why))
		return s.encodeReply(out)
	case signedask.Stale:
		log.WarnContext(ctx, "estate_request_refused", "node", s.self,
			"from", admitted.Request.From, "op", admitted.Request.Op, "detail", admitted.Why)
		out.Request, out.Unserved, out.Detail = admitted.ID, unservedStale,
			fmt.Sprintf("%s: %s", s.self, admitted.Why)
		return s.encodeReply(out)
	}
	req := admitted.Request
	out.Request = req.ID
	due, nobody := signedask.Due(arrived, req.BudgetMS, time.Now())
	if nobody != "" {
		log.WarnContext(ctx, "estate_request_refused", "node", s.self,
			"from", req.From, "op", req.Op, "detail", nobody)
		out.Unserved, out.Detail = unservedStale, fmt.Sprintf("%s: %s", s.self, nobody)
		return s.encodeReply(out)
	}
	spec, known := registry[req.Op]
	if !known {
		// AN OPERATION THIS BUILD DOES NOT DECLARE, which no node of
		// this build can run either. Unserved rather than failed: it
		// ran nothing, and a node that can is the one to ask.
		out.Unserved, out.Detail = unservedNoBackend,
			fmt.Sprintf("%s does not know the operation %q", s.self, req.Op)
		return s.encodeReply(out)
	}
	if spec.lane != l {
		// ANSWERED ONLY ON ITS OWN LANE, kept here rather than trusted to
		// every asker: on the wrong lane a proposal holds a slot every read
		// waits for, or the reverse. Unserved, so an asker whose build
		// declares the lane differently moves on.
		out.Unserved, out.Detail = unservedNoBackend, fmt.Sprintf(
			"%s answers %s on %s (%s), and it was asked on %s", s.self, req.Op, spec.lane,
			spec.lane.subject(s.self), l)
		return s.encodeReply(out)
	}
	if spec.git {
		// THE GIT ARM, before the copy's verdict and the readiness gates,
		// none of which applies to a git operation ([GitCopy]). It reads the
		// inputs itself, past the writes it holds.
		return s.answerGit(ctx, spec, req, due, out)
	}
	// INPUTS READ WHOLE OR NOT RUN (package doc, "A request is read whole,
	// or runs nowhere"), ahead of the floors, so a node that refuses anyway
	// does not first wait out a read budget.
	args, actor, refused, err := inputs(spec, req)
	switch {
	case err != nil:
		out.Err = encodeError(err)
		return s.encodeReply(out)
	case refused != nil:
		return s.refuseUnknown(ctx, out, req, refused)
	}
	// THE ASKER'S WAIT, COUNTED FROM ARRIVAL ON THIS NODE'S CLOCK, bounds
	// everything run for it ([request.BudgetMS]): its deadline, the asker's
	// instant, only admitted it, and read on this clock it cut the work short
	// by however far this node stood ahead.
	ctx, cancel := context.WithDeadline(ctx, due)
	defer cancel()
	// EVERY WAIT ENDS gitlet.ReplyMargin SHORT OF IT (the floor's below, a
	// read's barrier and level, a write's resolution), so `behind` reaches the
	// asker while it listens; heard late, a lagging node is taken for a wedged
	// one, asked last for suspectFor. It is the one margin a serving node
	// leaves, the committer's included (package doc, "Read-your-writes: ONE
	// floor table per node, consulted for local reads too").
	ctx = statelog.WithReplyMargin(ctx, gitlet.ReplyMargin)
	b, wrong, unserved := unservedCopy(ctx, s.local, spec)
	if unserved {
		// NOTHING RAN, so every class moves on, a page write included:
		// its never-repeat rule is about a request that may have run.
		out.Unserved = unservedOutOfService
		out.Detail = outOfService(s.self, wrong)
		return s.encodeReply(out)
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
		return s.encodeReply(out)
	}
	// A PAGE OR A CAPPED LIST HOLDS ITS WEIGHT of the node's answer budget
	// from before it reads until its reply is written; a node whose budget
	// stays full answers behind, as one that cannot answer inside its budget.
	if weight := spec.answer.weight(); weight > 0 {
		release, holdErr := s.answers.Acquire(ctx, queue.AnswerEstate, weight)
		if holdErr != nil {
			out.Unserved, out.Detail = unservedBehind, fmt.Sprintf("%s: %v", s.self, holdErr)
			return s.encodeReply(out)
		}
		defer release()
	}
	b.ServerSeams = s.seams
	b.verifiers = s.env.verifiers
	b.pageBytes = usagePageBytes
	var written *writtenItems
	if actor != nil && actor.Records {
		// THE ASKER'S TURN WANTS TO HEAR what this write committed to, and
		// its set is in another process: collect here, answered beside the
		// operation's result.
		written = &writtenItems{}
		actor.Provenance.Written = written
	}
	result, err := spec.run(readSpent(ctx, spec, spent), b, actor, args)
	out.Written, out.Committed = written.list(), written.committed()
	switch {
	case errors.Is(err, errNoHalf):
		out.Unserved, out.Detail = unservedNoBackend, ranNothingDetail(s.self, req.Op, err)
		return s.encodeReply(out)
	case errors.Is(err, errNotAdmitting):
		out.Unserved, out.Detail = unservedNotEstablished, fmt.Sprintf(
			"%s's copy admits no seat yet — the same gate that withholds seats from it", s.self)
		return s.encodeReply(out)
	case errors.Is(err, errDecidesNoGrant):
		// THE HARD ARM, even when told to answer anyway ([op.judgesGrant]):
		// nothing ran, and the node decides no grant from a replaced chart.
		out.Unserved, out.Detail = unservedBehindConfig, decidesNoGrant(s.self)
		return s.encodeReply(out)
	}
	return s.answered(out, spec, result, err)
}

// inputs decodes req's actor and its arguments, in the operation's own type,
// under the wire's rule ([decodeInput]). refused names every key either holds
// that this build does not declare, and nothing is to run; err is either
// failing to decode for any other reason.
func inputs(spec *opSpec, req request) (args any, actor *Actor, refused *ErrUnknownKeys, err error) {
	var unknown []string
	if len(req.Actor) > 0 && !bytes.Equal(req.Actor, []byte("null")) {
		actor = &Actor{}
		held, decodeErr := decodeInput(req.Actor, actor)
		if decodeErr != nil {
			return nil, nil, nil, fmt.Errorf("estate: %s: decode the actor: %w", spec.name, decodeErr)
		}
		unknown = prefixed("actor", held)
	}
	args, held, err := spec.decode(req.Args)
	if err != nil {
		return nil, nil, nil, err
	}
	if unknown = append(unknown, prefixed("args", held)...); len(unknown) > 0 {
		return nil, nil, &ErrUnknownKeys{Op: spec.name, Keys: unknown}, nil
	}
	return args, actor, nil, nil
}

// refuseUnknown answers a request whose inputs hold keys this build does not
// declare ([inputs]): refused, naming them, nothing run, so the asking router
// asks a data node of the writer's build ([Router.settle]).
//
// LOGGED HERE at WARN, like the other refusals an operator fixes on this node,
// because during an upgrade the operator looks at the node still on the older
// build; the asker's walk names what every node said only where none served.
func (s server) refuseUnknown(ctx context.Context, out reply, req request, refused *ErrUnknownKeys) []byte {
	refused.Nodes = []string{s.self}
	log.WarnContext(ctx, "estate_request_refused", "node", s.self, "disposition", "unknown_keys",
		"from", req.From, "op", req.Op, "keys", words.And(asSaid(refused.Keys)),
		"detail", "a node of a build that declares those keys asked for it and this build does not, so "+
			"nothing of it ran rather than run without them; a data node of the asker's build serves "+
			"it — finish the upgrade")
	out.Err = encodeError(refused)
	return s.encodeReply(out)
}

// answered is out carrying what an operation answered: its failure, crossing
// as itself ([encodeError]), or its result encoded ([encodeResult]).
func (s server) answered(out reply, spec *opSpec, result any, err error) []byte {
	if err != nil {
		out.Err = encodeError(err)
		return s.encodeReply(out)
	}
	encoded, err := encodeResult(spec, result)
	if err != nil {
		out.Err = encodeError(fmt.Errorf("estate: %s could not encode the answer to %s: %w",
			s.self, spec.name, err))
		return s.encodeReply(out)
	}
	out.Result = encoded
	return s.encodeReply(out)
}

// jsonAppender is an answer that writes its own encoding — a usage page, which
// holds the encoding [usage.Fill] measured each of its rows by
// ([usage.Page.AppendJSON]) — exactly as [json.Marshal] would.
type jsonAppender interface {
	AppendJSON(dst []byte) ([]byte, error)
}

// encodeResult is an operation's answer as the reply carries it: its own
// encoding where it holds one (a usage page's rows, encoded once already to
// cut the page at its budget); the inputs' rule where a caller carries it into
// another request ([op.answeredWhole]), so a key a newer build adds is on it
// only with a value; json.Marshal's otherwise.
func encodeResult(spec *opSpec, result any) ([]byte, error) {
	if a, ok := result.(jsonAppender); ok {
		return a.AppendJSON(nil)
	}
	if spec.whole {
		return encodeInput(result)
	}
	return json.Marshal(result)
}

// ready is the two gates a data node puts before an operation, for a peer and
// its own router alike: no better node left to ask (a preference, [worse];
// acceptLagging is the asker saying it has none, see [Router.route]), and the
// asker's floors on the operation's own domain's log reached
// ([opSpec.floorStream]). reason is empty when it may run; obsolete is every
// floor on an abandoned generation, which the asker stops carrying.
//
// A floor on ANOTHER domain's log is skipped here rather than trusted to every
// asker (package doc, "Read-your-writes: ONE floor table per node, consulted
// for local reads too"). One already waited out on an earlier node
// ([statelog.Spent.Waited]) is only checked, so a walk past N nodes behind one
// floor waits one read budget, not N.
func ready(ctx context.Context, self string, spec *opSpec, b Backend, floors []statelog.Position,
	acceptLagging bool, spent statelog.Spent) (reason unservedReason, detail string, obsolete []statelog.Position) {

	if !spec.ungated && !acceptLagging {
		if reason, why := worse(ctx, self, spec, b); reason != "" {
			return reason, why, nil
		}
	}
	if spec.floorStream == "" || b.Reached == nil {
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

// worse is why this node is a WORSE NODE TO ASK for spec than another data
// node, or empty: confirmed behind the company's configuration
// ([Backend.Current], [unservedBehindConfig]), or its copy of the operation's
// domain lags its log ([Backend.Answers], [unservedLagging]), both said where
// both hold, under the code that lasts and ranks last ([Router.lastResort]).
//
// Preferences, never refusals: either answer is sound, only worse, and a fleet
// whose only data node lags or failed an apply would refuse every call. An
// operation that judges a grant is refused by its own hard arm
// ([op.judgesGrant]). See the package doc, "A node behind the company's
// configuration is a worse node too".
func worse(ctx context.Context, self string, spec *opSpec, b Backend) (unservedReason, string) {
	var (
		reason unservedReason
		why    []words.Said
	)
	if b.Current != nil && !b.Current() {
		reason = unservedBehindConfig
		why = append(why, words.Said(fmt.Sprintf("%s is behind the company's configuration — the "+
			"control plane has confirmed it behind the revision the fleet activated, so "+
			"the chart, the units and the limits it would answer by are the ones the "+
			"fleet replaced", self)))
	}
	if b.Answers != nil && !b.Answers(ctx) {
		if reason == "" {
			reason = unservedLagging
		}
		why = append(why, words.Said(fmt.Sprintf("%s's copy of %s lags its log — it has not drained "+
			"it since it started, or has fallen past the snapshot slack of its end", self,
			spec.domain)))
	}
	if reason == "" {
		return "", ""
	}
	return reason, words.And(why) + " — so it answers only when no data " +
		"node that is a better one to ask takes the request"
}

// waitFloor waits for this node to apply floor within the read budget and,
// asked across the wire, within what the asker left ([statelog.WithReplyMargin]);
// where the request already waited it out (waited), it only checks. It answers
// the bound it waited within and the error to report as "behind", or obsolete
// for a floor on a generation this node's log abandoned, which nobody will
// reach and the asker stops carrying.
func waitFloor(ctx context.Context, b Backend, floor statelog.Position, waited bool) (
	bound statelog.WaitBound, obsolete bool, behind error) {

	budget := statelog.ReadBudget
	if waited {
		budget = 0
	}
	bounded, cancel, bound := statelog.WaitWithin(ctx, budget)
	defer cancel()
	err := b.Reached(bounded, floor)
	switch {
	case err == nil:
		return bound, false, nil
	case errors.Is(err, statelog.ErrWaitAbandoned):
		return bound, true, nil
	default:
		return bound, false, err
	}
}

// readSpent is ctx carrying what the request already paid on the nodes asked
// before this one ([statelog.WithSpent]), for a READ only: a write's waits are
// its write authority's own, which nothing a read paid elsewhere describes.
func readSpent(ctx context.Context, spec *opSpec, spent statelog.Spent) context.Context {
	if spec.class != opRead {
		return ctx
	}
	return statelog.WithSpent(ctx, spent)
}

// encodeReply signs a reply, and never fails: every field of a reply is plain
// data, and an encoder that refused one would be the silent node answer exists
// to avoid.
func (s server) encodeReply(r reply) []byte {
	raw := s.env.sealReply(r)
	if len(raw) > queue.MaxPayloadBytes {
		// TOO LARGE TO SEND, and said so: a reply the broker refuses is a
		// node the asker waits out and counts as down. Measured SEALED, as
		// the broker carries it.
		raw = s.env.sealReply(reply{Node: r.Node, Request: r.Request, Err: encodeError(fmt.Errorf(
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

// writtenItems is the [worklet.WriteLog] a served write reports into on behalf
// of an asking node's turn — see [reply.Written]. A walking gesture's writer
// may report one item more than once; the set keeps it once, in the order it
// was first reported.
type writtenItems struct {
	mu      sync.Mutex
	items   []types.WorkItem
	changes []types.CommittedChange
}

// Commit implements [worklet.WriteLog].
func (w *writtenItems) Commit(c types.CommittedChange) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !slices.Contains(w.changes, c) {
		w.changes = append(w.changes, c)
	}
}

// committed is the changes reported, or nil where nothing asked for them.
func (w *writtenItems) committed() []types.CommittedChange {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.changes)
}

// Add implements [worklet.WriteLog].
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
