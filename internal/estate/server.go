package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
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
	// node with the `git` role — every one of which holds data in this
	// release — and nil on every other. Where it is set the node registers
	// the git lane too ([gitLanes]), from boot: until its code host is built
	// the lane answers `no_backend`.
	Git LocalGit

	// Seams are this node's own: supplied to every operation it answers.
	Seams ServerSeams

	// Envelopes verify every request this node is sent and sign every
	// reply it answers with — required: see [Envelopes].
	Envelopes *Envelopes

	// Now is the clock a request's deadline is judged on, injected for
	// tests. Nil is the wall clock.
	Now func() time.Time
}

// Serve makes this node answer the fleet on its own subjects, one a [lane],
// from its own copy of the estate — and `out_of_service` for the operations of
// a domain whose copy is wrong (ADR-0049). The returned withdrawal ends every
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
// each one a whole write attempt before the runner-up — and a git node that
// answered no git read would be every repository's silent home, as one that
// answered no engine write would be to every write's walk. A node serving git
// ([ServerOptions.Git]) registers the git lanes, the reads' and the writes'
// ([gitLanes]), beside the others.
func Serve(ctx context.Context, q Server, opts ServerOptions) (queue.Unsubscribe, error) {
	switch {
	case q == nil || opts.Local == nil:
		return nil, errors.New("estate: serve needs a queue and this node's backends")
	case opts.Self == "":
		return nil, errors.New("estate: serve needs this node's id — it is the subject other nodes ask")
	case opts.Envelopes == nil:
		return nil, errNoEnvelopes
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	// ONE SERVER FOR EVERY LANE, so one guard remembers every request id
	// the node has taken whichever lane it arrived on: a replay sent to
	// another lane's subject is the same replay.
	srv := server{self: opts.Self, local: opts.Local, git: opts.Git, seams: opts.Seams,
		env: opts.Envelopes, now: now, seen: signedask.NewGuard()}
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
			// own request's deadline.
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
}

// answer runs one request that arrived on lane l and ALWAYS answers: a node
// that stayed silent would be indistinguishable from one that is down, and the
// asker would wait out its budget before trying the next — so every failure
// here, down to a request this build cannot verify or decode, is an answer
// that says so.
func (s server) answer(ctx context.Context, l lane, raw []byte) []byte {
	out := reply{Node: s.self}
	admitted := s.env.admit(raw, s.seen, s.now())
	switch admitted.Disposition {
	case signedask.Tampered:
		// NOT THE FLEET'S, so nothing in it is believed — not even the id
		// a reply would name, which is why this one names none. At ERROR:
		// no holder will ever accept it, so it is somebody on the broker
		// who is not the fleet, or a node whose keyring is not the fleet's.
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
			"%s does not hold the key the request is signed under (it holds %v)",
			s.self, s.env.heldKeys())
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
		return s.encodeReply(out)
	}
	if spec.git {
		// THE GIT ARM, after the envelope's admission and the lane check
		// and before the copy's verdict and the readiness gates, none of
		// which applies to a git operation ([GitCopy]).
		return s.answerGit(ctx, spec, req, out)
	}
	// AN ADMITTED REQUEST ALWAYS CARRIES ITS DEADLINE, under the signature
	// ([signedask.Channel.Admit] refuses one without), so it is the bound
	// on everything run for it.
	ctx, cancel := context.WithDeadline(ctx, req.Deadline)
	defer cancel()
	// EVERY WAIT ENDS A REPLY'S TRIP SHORT OF IT — the floor's below, and a
	// read's barrier and level and a write's resolution inside the operation
	// — so a node that runs out of time says so while its asker is still
	// listening. Run to the deadline itself, a wait's honest `behind`
	// reached the asker after it had given up on this node, which then heard
	// a lagging node as a wedged one: asked it last for suspectFor and told
	// the placement it went unanswered. code.ReplyMargin is the one margin a
	// serving node leaves, the committer's between a batch and its asker's
	// deadline included.
	ctx = statelog.WithReplyMargin(ctx, code.ReplyMargin)
	b, wrong, unserved := unservedCopy(ctx, s.local, spec)
	if unserved {
		// NOTHING RAN, so every class moves on — a page write
		// included, whose never-repeat rule is about a request that
		// may have run.
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
	b.ServerSeams = s.seams
	b.verifiers = s.env.verifiers
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
		return s.encodeReply(out)
	case errors.Is(err, errNotAdmitting):
		out.Unserved, out.Detail = unservedNotEstablished, fmt.Sprintf(
			"%s's copy admits no seat yet — the same gate that withholds seats from it", s.self)
		return s.encodeReply(out)
	case errors.Is(err, errDecidesNoGrant):
		// THE HARD ARM, answered whether or not the asker said to answer
		// anyway ([op.judgesGrant]): nothing ran, and the node decides no
		// grant from the chart the fleet replaced.
		out.Unserved, out.Detail = unservedBehindConfig, decidesNoGrant(s.self)
		return s.encodeReply(out)
	}
	return s.answered(out, req.Op, result, err)
}

// answered is out carrying what an operation answered: its failure, crossing
// as itself ([encodeError]), or its result encoded.
func (s server) answered(out reply, op string, result any, err error) []byte {
	if err != nil {
		out.Err = encodeError(err)
		return s.encodeReply(out)
	}
	encoded, err := encodeResult(result)
	if err != nil {
		out.Err = encodeError(fmt.Errorf("estate: %s could not encode the answer to %s: %w",
			s.self, op, err))
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
// encoding where it holds one, so a usage page's rows — encoded once already,
// to cut the page at its budget — are not encoded a second time to send it.
func encodeResult(result any) ([]byte, error) {
	if a, ok := result.(jsonAppender); ok {
		return a.AppendJSON(nil)
	}
	return json.Marshal(result)
}

// ready is the two gates a data node puts in front of an operation — no
// better node left to ask, and the asker's floors on the log of the
// operation's own domain reached ([opSpec.floorStream]) — and the same two
// whether the asker is another node or this one's own router. reason is empty when the
// operation may run; obsolete is every floor on a generation the log has
// abandoned, which the asker stops carrying.
//
// THE FIRST GATE IS A PREFERENCE, and acceptLagging is the asker saying it has
// no better node to ask: a node that is a worse choice than another is never a
// wrong one ([worse]). See [Router.route].
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

	if !spec.ungated && !acceptLagging {
		if reason, why := worse(ctx, self, spec, b); reason != "" {
			return reason, why, nil
		}
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

// worse is why this node is a WORSE NODE TO ASK for spec than another data
// node, or empty where it is not: the node is confirmed behind the company's
// configuration ([Backend.Current], [unservedBehindConfig]), or its copy of
// the operation's domain lags its log ([Backend.Answers], [unservedLagging]) —
// each said, where both hold.
//
// Both are preferences rather than refusals, and for one reason: what the
// node answers is sound, only worse than another's. A copy that lags is held
// to what the caller must see by the floors and the read's own level; a node
// behind the configuration answers from the chart, the units, the scope and
// the limits the fleet replaced, a stale value every reader of one already
// meets when a revision changes it. So each sends the walk to every other data
// node first, and is asked again, told to answer anyway, when none of them
// takes the request — never refused, which on a fleet whose only data node
// lags or failed an apply would refuse every call its seats make. The one
// exception is no preference: an operation that JUDGES A GRANT from the chart
// ([op.judgesGrant]) is refused by a node behind the configuration however it
// is asked, by the hard arm the operation's own declaration runs, since an
// answer under the chart the fleet replaced is the one thing being asked last
// cannot make sound.
//
// TWO REASON CODES, because the asker does one thing with either answer — asks
// the node last — and two more with the second: it asks it after every node
// whose copy only lags, which answers under the current configuration
// ([Router.lastResort]), and it REMEMBERS it, since a node stays behind its
// configuration until an apply succeeds where a lag closes as the applier
// catches up ([unservedBehindConfig]). Where both hold the answer is the one
// that lasts and ranks last, so the asker remembers it.
func worse(ctx context.Context, self string, spec *opSpec, b Backend) (unservedReason, string) {
	var (
		reason unservedReason
		why    []string
	)
	if b.Current != nil && !b.Current() {
		reason = unservedBehindConfig
		why = append(why, fmt.Sprintf("%s is behind the company's configuration — the "+
			"control plane has confirmed it behind the revision the fleet activated, so "+
			"the chart, the units and the limits it would answer by are the ones the "+
			"fleet replaced", self))
	}
	if b.Answers != nil && !b.Answers(ctx) {
		if reason == "" {
			reason = unservedLagging
		}
		why = append(why, fmt.Sprintf("%s's copy of %s lags its log — it has not drained "+
			"it since it started, or has fallen past the snapshot slack of its end", self,
			spec.domain))
	}
	if reason == "" {
		return "", ""
	}
	return reason, strings.Join(why, "; and ") + " — so it answers only when no data " +
		"node that is a better one to ask takes the request"
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

// encodeReply signs a reply, and never fails: every field of a reply is plain
// data, and an encoder that refused one would be the silent node answer exists
// to avoid.
func (s server) encodeReply(r reply) []byte {
	raw := s.env.sealReply(r)
	if len(raw) > queue.MaxPayloadBytes {
		// TOO LARGE TO SEND, and said so rather than dropped: a reply the
		// broker refuses is a node the asker waits out and then counts as
		// down, while this one says what to narrow. Measured as SEALED,
		// which is what the broker carries.
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
