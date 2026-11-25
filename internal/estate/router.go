package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/rendezvous"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Asker is what a router needs from the queue: the ephemeral request/reply
// verb, one subject per serving node.
type Asker interface {
	Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error)
}

// Placement is how the router learns which data nodes to ask: the engine's
// answer is every live data node, from its watched view of the presence leases.
// It is asked on every request, so it answers from memory, never a listing of
// the fleet per call.
type Placement interface {
	// Holders is every data node that holds the estate, in no particular
	// order. An error is UNKNOWN, never "no holder": an empty list is a
	// fleet with no data node in it.
	Holders() ([]string, error)

	// Unanswered tells the placement a node it named went silent through a wait
	// of the router's own, never one its caller ended ([Router.ask]), so a node gone
	// on a clean stop leaves the next answer rather than one a heartbeat later. It
	// must not block.
	Unanswered(node string)
}

// LocalBackends is this data node's own copy of the estate.
type LocalBackends interface {
	// For is the backend over this node's copy of domain, or false while that
	// copy is OUT OF SERVICE (wrong rather than behind): the node answers none of
	// that domain's operations from it, while every other domain's copy keeps
	// serving (ADR-0079). [SeatCopy] asks for the copy a seat is served by. The
	// backend's [Backend.Answers] is that domain's own.
	//
	// It takes a context because the verdict is read from the log's broker; an
	// implementation answers from a verdict it refreshes, never with a broker round
	// trip per request.
	For(ctx context.Context, domain string) (Backend, bool)
}

// RouterOptions are a router's dependencies.
type RouterOptions struct {
	// Self is this node, which asks — named on every request so a serving
	// node's log says who it answered, and never asked over the wire.
	Self string

	Queue     Asker
	Placement Placement

	// Git is who serves git: every live node with the `gitlet` role, from the
	// same watched presence view as Placement, its error UNKNOWN, never "no Gitlet
	// node". Every operation on a repository's git objects asks it
	// ([op.keyedOnGit], ADR-0089), and [Router.Maintainer] places maintenance by
	// its [GitPlacement.Listing]. Required: without one every git read would answer
	// as a fleet with no Gitlet node, a wiring mistake that reads as a deployment
	// fact.
	Git GitPlacement

	// LocalGit is this node's own git half, resolved PER REQUEST ([LocalGit]),
	// since Gitlet may be built after the router (at the first apply that sets
	// code.backend: gitlet); nil on a node without the `gitlet` role.
	LocalGit LocalGit

	// Local is this node's own copy, nil on a node without the `data`
	// role, which holds none.
	Local LocalBackends

	// Session is THIS NODE's floors ([Session]): one per node, shared with
	// whatever else observes a position a seat must read past.
	Session *Session

	// Seams are this node's own: supplied to every operation this node
	// answers in-process.
	Seams ServerSeams

	// Envelopes sign every request this node sends and verify every reply
	// it is sent — required: see [Envelopes].
	Envelopes *Envelopes

	// Now is the clock the suspect cooldown, the admission cache and a
	// silent node's wait ([Router.ask]) read, injected for tests. Nil is the
	// wall clock.
	Now func() time.Time

	// Timing is how long the router waits on a data node: [Production] on
	// every node. Every field is required, and no attempt may end before the
	// serving node can answer ([Timing]).
	Timing Timing
}

// Timing is every duration the router waits on that a test must be able to
// scale: one attempt on one data node for a read and for a write, one git read
// and one engine write on one Gitlet node, and the admission question's whole
// budget. [Production] is the value every node runs; a suite in another package
// builds its own, one value in the jsprovision.Timing shape, since it can set
// no unexported field.
//
// A serving node ends every wait a reply's margin short of the attempt
// ([statelog.WithReplyMargin]), so a shorter attempt makes a lagging node say so
// sooner and never reads it as wedged. [NewRouter] refuses by name a field left
// zero, never read as a default, or too short to do its job ([minReadAttempt],
// [minWriteAttempt], [minGitWriteAttempt]): package doc, "The router's timing".
type Timing struct {
	// ReadAttempt bounds one read's wait on one data node — see
	// [ReadAttempt]. Longer than [minReadAttempt].
	ReadAttempt time.Duration
	// WriteAttempt bounds one write's wait on one data node, for a caller
	// with no deadline of its own — see writeAttempt. Longer than
	// [minWriteAttempt].
	WriteAttempt time.Duration
	// AdmissionAsk bounds the remote half of the admission question as a
	// whole — see admissionAsk. Positive.
	AdmissionAsk time.Duration
	// GitReadAttempt bounds one git read's wait on one Gitlet node — see
	// [GitReadAttempt]. Longer than [minReadAttempt].
	GitReadAttempt time.Duration
	// GitWriteAttempt bounds one engine write's wait on one Gitlet node, its
	// caller's deadline only ever shortening it — see [GitWriteAttempt].
	// Longer than [minGitWriteAttempt].
	GitWriteAttempt time.Duration
}

// Production is the timing every node runs: [ReadAttempt], writeAttempt,
// admissionAsk, [GitReadAttempt] and [GitWriteAttempt], whose reasons are
// written at their definitions.
func Production() Timing {
	return Timing{ReadAttempt: ReadAttempt, WriteAttempt: writeAttempt, AdmissionAsk: admissionAsk,
		GitReadAttempt: GitReadAttempt, GitWriteAttempt: GitWriteAttempt}
}

// GitWriteAttemptFor is an engine write's attempt on one Gitlet node by its
// kind: [Timing.GitWriteAttempt] with its build term (gitlet.WriteBuildDeadline)
// replaced by the kind's own (gitlet.EngineWriteKind.BuildDeadline): 102 s for
// a merge_change, 112 s for a rebase_change, 82 s for every other kind in
// production. The Gitlet node fits every stage of the write into its attempt
// (internal/gitlet/host's deadline rule), so a longer build needs its time
// before the upload and proposal, or every build near its bound is answered
// busy.
func (t Timing) GitWriteAttemptFor(kind gitlet.EngineWriteKind) time.Duration {
	return t.GitWriteAttempt - gitlet.WriteBuildDeadline + kind.BuildDeadline()
}

// minReadAttempt is what every read attempt must be longer than: the reply's
// margin, gitlet.ReplyMargin, which a serving node leaves between its last wait
// and its asker's deadline ([statelog.WithReplyMargin]). An attempt no longer
// leaves it nothing to wait or query in.
const minReadAttempt = gitlet.ReplyMargin

// minWriteAttempt is what every write attempt must be longer than: the serving
// node's floor wait at its whole budget ([statelog.ReadBudget]), then a whole
// gitlet.CommitBudget and gitlet.ReplyMargin, which a repository's committer
// must still find before a push's deadline to start its batch at all. It is what
// a PUSH needs, not a bound on any write: a walking gesture is what writeAttempt
// is sized for.
//
// It IS gitlet.MinProposeAttempt, because Gitlet holds an engine write's
// proposal to the same sum, and two sums could disagree.
const minWriteAttempt = gitlet.MinProposeAttempt

// minGitWriteAttempt is what every git write attempt must be longer than: the
// proposal the write makes ([minWriteAttempt]) and the reply's margin. The
// Gitlet node answers a reply's margin before the attempt ends (gitlet.AnswerBy)
// and uploads only while a whole proposal's attempt is left after the upload
// (internal/gitlet/host's deadline rule), so a shorter attempt answers every
// write busy before its upload.
const minGitWriteAttempt = minWriteAttempt + gitlet.ReplyMargin

// refusal is why a router may not be built with t, naming the first field it
// refuses, or nil.
func (t Timing) refusal() error {
	const production = "Production() is the value a node runs"
	switch {
	case t.ReadAttempt <= minReadAttempt:
		return fmt.Errorf("estate: a router's Timing.ReadAttempt is %v, and it must be longer "+
			"than %v — the answer's trip back (gitlet.ReplyMargin), which a serving node leaves "+
			"between the end of its waits and its asker's deadline, so an attempt no longer "+
			"leaves it no time to wait for anything or to run the read; %s", t.ReadAttempt,
			minReadAttempt, production)
	case t.WriteAttempt <= minWriteAttempt:
		return fmt.Errorf("estate: a router's Timing.WriteAttempt is %v, and it must be longer "+
			"than %v — the serving node's wait for the asker's floor (statelog.ReadBudget, %v), "+
			"then a batch (gitlet.CommitBudget, %v) and its answer (gitlet.ReplyMargin, %v), which a "+
			"repository's committer must still have before a push's deadline or it answers the "+
			"push busy, undecided; %s", t.WriteAttempt, minWriteAttempt, statelog.ReadBudget,
			gitlet.CommitBudget, gitlet.ReplyMargin, production)
	case t.AdmissionAsk <= 0:
		return fmt.Errorf("estate: a router's Timing.AdmissionAsk is %v, and it must be a "+
			"positive duration — a zero or negative budget is a deadline already past, which "+
			"withholds every seat's claim; %s", t.AdmissionAsk, production)
	case t.GitReadAttempt <= minReadAttempt:
		return fmt.Errorf("estate: a router's Timing.GitReadAttempt is %v, and it must be longer "+
			"than %v — the answer's trip back (gitlet.ReplyMargin), which a Gitlet node leaves between "+
			"the end of its waits and its asker's deadline, so an attempt no longer leaves it no "+
			"time to sync its copy or run the read; %s", t.GitReadAttempt, minReadAttempt, production)
	case t.GitWriteAttempt <= minGitWriteAttempt:
		return fmt.Errorf("estate: a router's Timing.GitWriteAttempt is %v, and it must be longer "+
			"than %v — a git write's attempt must hold the proposal it makes: the Gitlet node uploads a "+
			"write only while the proposal's own attempt (gitlet.MinProposeAttempt, %v) is left after "+
			"the upload, and answers its asker the reply's trip (gitlet.ReplyMargin, %v) before the "+
			"attempt ends, so an attempt no longer answers every write busy before its upload; %s",
			t.GitWriteAttempt, minGitWriteAttempt, minWriteAttempt, gitlet.ReplyMargin, production)
	}
	return nil
}

// Router routes every estate operation to a data node: this node, in-process,
// where its copy of the operation's domain serves; otherwise the other data
// nodes over the broker, one subject per node. One runs on every node, so a
// tool behaves identically with or without the `data` role, and a data node's
// path to its own copy is its path to a peer's, floors included.
//
// The order nodes are asked in is [Router.order]'s, a keyed operation's
// [Router.keyedOrder]'s and a git operation's [Router.routeGit]'s; what may be
// repeated after a node MAY have run it is its class's ([opClass]). A node
// behind the company's configuration is asked only as the walk's last resort
// ([Router.markBehindConfig]). Package doc, "One node per request, never a
// scatter".
type Router struct {
	self      string
	queue     Asker
	placement Placement
	local     LocalBackends
	session   *Session
	seams     ServerSeams
	env       *Envelopes
	now       func() time.Time

	// gitPlacement is who serves git, and in one listing what each Gitlet
	// node advertises ([RouterOptions.Git]), and localGit this node's own
	// git half, nil on a node without the gitlet role
	// ([RouterOptions.LocalGit]).
	gitPlacement GitPlacement
	localGit     LocalGit

	// timing is how long each attempt waits ([RouterOptions.Timing]).
	timing Timing

	mu sync.Mutex
	// sticky is, per set of copies an operation reads ([opSpec.copies]) —
	// one domain's for nearly every operation — the node that answered such
	// an operation last, asked first for one next time.
	sticky map[string]string
	// suspect is when each node that went unanswered may be asked first
	// again.
	suspect map[string]time.Time
	// passedOver is when each node that answered `out_of_service` for a
	// set of copies may be asked ahead of the rest for it again.
	passedOver map[nodeCopy]time.Time
	// declined is when each node that ran nothing for a KEYED operation —
	// answered `no_backend` — may be asked ahead of the rest for it again.
	declined map[nodeOp]time.Time
	// unread is, per node and KEYED operation, the keys a request was refused for
	// because the node's build does not declare them ([ErrUnknownKeys]), and until
	// when a request carrying one asks it last ([Router.markUnread]); never the
	// whole operation.
	unread map[nodeOp]unreadKeys
	// behindConfig is until when each node that answered `behind_config` is asked
	// only as the last resort ([Router.markBehindConfig]). Per NODE: serving the
	// company's configuration is the node's posture, not a copy's.
	behindConfig map[string]time.Time
	// admitted caches a REMOTE node's answer to the admission question —
	// see [Router.Serves] — nil until one has been asked.
	admitted *admission

	// codeWrites is how far this node's own writes reached in each
	// repository ([CodeSeam.FloorOf]), under a lock of its own: Gitlet's
	// writes and reads touch nothing else of the router's state.
	codeWrites repoFloors
}

// nodeCopy is one node's copies of what an operation reads ([opSpec.copies]).
type nodeCopy struct{ node, copies string }

// nodeOp is one node's serving of one operation, by its name.
type nodeOp struct{ node, op string }

// unreadKeys is what one node refused requests of one operation for — the keys
// its build does not declare, each by the path its refusal named
// ([ErrUnknownKeys.Keys]) — and until when it is remembered.
type unreadKeys struct {
	until time.Time
	keys  []string
}

// admission is one cached answer to [Router.Serves].
type admission struct {
	at     time.Time
	served Served
	err    error
}

// NewRouter builds a router.
func NewRouter(opts RouterOptions) (*Router, error) {
	switch {
	case opts.Queue == nil:
		return nil, errors.New("estate: a router needs the queue it asks over")
	case opts.Placement == nil:
		return nil, errors.New("estate: a router needs the placement it routes by")
	case opts.Git == nil:
		return nil, errors.New("estate: a router needs the git placement it asks a repository's git " +
			"reads by — every live node with the gitlet role; one with none to name answers an empty " +
			"list, never a missing placement")
	case opts.Session == nil:
		return nil, errors.New("estate: a router needs this node's session floors — " +
			"one per node, shared by everything that observes a write")
	case opts.Self == "":
		return nil, errors.New("estate: a router needs this node's id — it is " +
			"what a serving node's log names, and what the router never asks")
	case opts.Envelopes == nil:
		return nil, errNoEnvelopes
	}
	if err := opts.Timing.refusal(); err != nil {
		return nil, err
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Router{
		self: opts.Self, queue: opts.Queue, placement: opts.Placement,
		local: opts.Local, session: opts.Session, seams: opts.Seams, env: opts.Envelopes, now: now,
		gitPlacement: opts.Git, localGit: opts.LocalGit, timing: opts.Timing,
		sticky: map[string]string{}, suspect: map[string]time.Time{},
		passedOver: map[nodeCopy]time.Time{}, declined: map[nodeOp]time.Time{},
		unread: map[nodeOp]unreadKeys{}, behindConfig: map[string]time.Time{},
	}, nil
}

// ReadAttempt bounds one read's wait on one node ([Timing.ReadAttempt]), under
// the caller's own deadline: FIFTEEN SECONDS, a linearizable read's waits at
// their whole budgets (TestProductionsReadAttemptHoldsALinearizableReadsWaits).
// The barrier on a log several members store, 10 s, waits out one election at
// the pinned nats-server v2.15.3: 9 s maxElectionTimeoutDefault
// (server/raft.go:329) and the new leader's first 1 s heartbeat (raft.go:331).
// The read's own 5 s is argued, not measured: the floor's and the position's
// [statelog.ReadBudget] (2 s each, the read's promise, refused rather than
// grown) and gitlet.ReplyMargin (1 s, two orders over the answer's trip and
// clock skew). A node silent that long is gone rather than slow; gitproc's
// ReadDeadline and SlotWait fit inside one (TestAGitReadFitsInsideOneReadAttempt).
const ReadAttempt = 15 * time.Second

// writeAttempt bounds one write's wait on one node for a caller with no
// deadline of its own: [Production]'s [Timing.WriteAttempt], read by nothing
// else.
//
// SIXTY SECONDS: one append resolves within [statelog.DefaultResolveBudget]
// (five), and the longest gesture, a merge or a move, appends once per object
// it walks. A repository's committer starts a push's batch only while a
// gitlet.CommitBudget and gitlet.ReplyMargin are left before the attempt's end
// ([request.Deadline]), so past those and the floor wait ([minWriteAttempt]) a
// push may queue twenty-seven seconds behind its repository's earlier ones.
const writeAttempt = 60 * time.Second

// suspectFor is how long a node that went unanswered is asked last.
//
// THIRTY SECONDS, the order of a presence lease's life: a dead node leaves the
// placement when its lease lapses, and asked last meanwhile it costs one
// attempt per router rather than one per request.
//
// It is the one window for every other demotion too — a copy `out_of_service`
// ([Router.markOutOfService]), a keyed operation's `no_backend`
// ([Router.markDeclined]), `behind_config` ([Router.markBehindConfig]) —
// because each condition lasts minutes rather than one request and the remedy
// is the same: a node not worth asking first. A second number would be a
// second opinion about how long that lasts.
const suspectFor = 30 * time.Second

// admissionTrust is how long a remote data node's answer to the admission
// question is trusted. FIVE SECONDS: it is asked on every placement sweep, and
// its answer changes only as nodes join or leave, which the presence lease's
// TTL bounds at the same order. A LOCAL answer is never cached: it costs no
// request, and a node's own copy falling behind must withhold its next claim at
// once.
const admissionTrust = 5 * time.Second

// admissionAsk bounds the REMOTE half of the admission question as a whole,
// every data node it asks together, where [ReadAttempt] bounds each one:
// [Production]'s [Timing.AdmissionAsk], read by nothing else.
//
// FIVE SECONDS: admission is asked synchronously on every placement sweep, the
// first before the host starts, and each listed but silent data node would
// otherwise cost the sweep a whole read attempt, its claims and sheds waiting
// behind it. A node silent for five is marked suspect, and the next sweep asks
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

	// unread is the keys a serving node of this build would refuse the encoded
	// arguments for ([decodeInput], [inputs]); [unreadHere] uses it to answer an
	// in-process refusal as the wire would.
	unread func() ([]string, error)

	// written is the asking turn's [worklet.WriteLog], which every node that RAN
	// the operation remotely answers into ([reply.Written]): a walk asks past an
	// unvouched node, so the answer kept is not the only commit. Nil where the
	// actor holds none; an in-process run reports into it directly.
	written worklet.WriteLog

	// key is a KEYED operation's routing key ([op.keyedBy], [op.keyedOnGit]),
	// empty for every other: the walk asks in the key's rendezvous order rather
	// than this node first.
	key string

	// home is this exchange stamped as asked of the walk's effective home
	// ([op.keyedOnGit]), the one git attempt that may fill a copy, while this one
	// is stamped not. Nil for every operation but a git one.
	home *exchange

	// attempt is this exchange's own attempt on one node, read off its
	// arguments ([op.attemptBy]) — an engine write's, by its kind — and nil
	// for every exchange whose attempt is its class's ([Router.wholeAttempt]).
	attempt func(Timing) time.Duration

	// here asks THIS node's copy first even while it lags its logs — see
	// [callHere]. False for every other call, which passes over a lagging
	// copy of its own for one that does not lag.
	here bool
}

// carrying matches a refusal's keys against this request (its arguments as
// the wire carries them, and its actor) for a keyed order passing over a node
// that refused requests carrying them ([Router.markUnread], [carries]). The
// request is decoded once, on the first question, and never where no node is
// remembered.
func (x exchange) carrying(actor *Actor) func(keys []string) bool {
	var (
		read bool
		doc  map[string]any
	)
	return func(keys []string) bool {
		if !read {
			read, doc = true, map[string]any{}
			// WHAT DOES NOT DECODE CARRIES NOTHING: no request this
			// router encoded fails to, and a match that missed only asks a
			// node first that refuses again ([carries]).
			if args, err := looseDoc(x.encoded); err == nil {
				doc["args"] = args
			}
			if actor != nil {
				if encoded, err := encodeInput(actor); err == nil {
					if a, err := looseDoc(encoded); err == nil {
						doc["actor"] = a
					}
				}
			}
		}
		return slices.ContainsFunc(keys, func(key string) bool { return carries(doc, key) })
	}
}

// call runs one operation on a data node whose copy serves.
func call[A, R any](ctx context.Context, r *Router, o op[A, R], actor *Actor, args A) (R, error) {
	return callAs(ctx, r, o, actor, args, false)
}

// callHere is [call] asked of THIS node's own copy first, even while it lags
// its logs, and of the other data nodes only where it cannot answer.
//
// For a caller deciding about a record this node's applier has just committed,
// which its own copy holds by construction and a peer may not: the ordinary
// walk would pass the lagging copy over during the very burst that made it
// lag, a round trip to a node that may lack the record. An out-of-service copy
// is still passed over: lag is distance, and a wrong copy is never an answer.
func callHere[A, R any](ctx context.Context, r *Router, o op[A, R], actor *Actor, args A) (R, error) {
	return callAs(ctx, r, o, actor, args, true)
}

// callAs is [call] and [callHere], here saying which.
func callAs[A, R any](ctx context.Context, r *Router, o op[A, R], actor *Actor, args A,
	here bool) (R, error) {
	var zero R
	spec := o.spec
	if spec.actor && actor != nil {
		// AN ACTOR THAT MAY WRITE NOTHING IS REFUSED BEFORE IT IS ASKED ANYWHERE
		// ([actorWrites]), here because [op.refusedBeforeAsking] sees only the
		// arguments; a read that runs as an actor (tracker.view_prior) included.
		if err := actorWrites(actor.Handle, iam.ActorKind(actor.Kind), actor.Provenance.WrittenFor); err != nil {
			return zero, err
		}
	}
	if spec.asked != nil {
		// REFUSED BEFORE IT IS ASKED ANYWHERE ([op.refusedBeforeAsking]),
		// in process as routed, so the answer is the same from every node.
		if err := spec.asked(args); err != nil {
			return zero, err
		}
	}
	var written worklet.WriteLog
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
	var key string
	if o.key != nil {
		if key = o.key(args); key == "" {
			return zero, fmt.Errorf("estate: %s is asked of the node its key prefers, and "+
				"its arguments name no key", spec.name)
		}
	}
	x, err := exchangesOf(spec, o, actor, args, written, key)
	if err != nil {
		return zero, err
	}
	x.here = here
	answer, err := r.route(ctx, spec, actor, x)
	if out, ok := answer.(R); ok {
		return out, err
	}
	return zero, err
}

// exchangesOf is what one call of o over args asks: its exchange — and, for a
// git operation, the same stamped as asked of the walk's effective home — each
// with the attempt its arguments declare ([op.attemptBy]).
func exchangesOf[A, R any](spec *opSpec, o op[A, R], actor *Actor, args A, written worklet.WriteLog,
	key string) (exchange, error) {

	var (
		x   exchange
		err error
	)
	if o.home == nil {
		if x, err = exchangeOf(spec, o, actor, args, written, key); err != nil {
			return exchange{}, err
		}
	} else {
		// A GIT OPERATION IS STAMPED ON EVERY ATTEMPT: home for the walk's
		// effective home, not for every node after it — whatever the
		// caller set, since only the walk knows which node it asks first.
		home, homeErr := exchangeOf(spec, o, actor, o.home(args, true), written, key)
		if homeErr != nil {
			return exchange{}, homeErr
		}
		if x, err = exchangeOf(spec, o, actor, o.home(args, false), written, key); err != nil {
			return exchange{}, err
		}
		x.home = &home
	}
	if o.attempt != nil {
		x.attempt = func(t Timing) time.Duration { return o.attempt(args, t) }
		if x.home != nil {
			x.home.attempt = x.attempt
		}
	}
	return x, nil
}

// exchangeOf is one operation's two halves over args, in its caller's types:
// run in-process on a backend, or encoded once to be asked of another node and
// its answer decoded.
func exchangeOf[A, R any](spec *opSpec, o op[A, R], actor *Actor, args A, written worklet.WriteLog,
	key string) (exchange, error) {

	// UNDER THE WIRE'S RULE FOR AN OPERATION'S INPUTS ([encodeInput]): a
	// field at its zero stays off, so a data node of a build that does not
	// declare it serves a write that does not use it.
	encoded, err := encodeInput(args)
	if err != nil {
		return exchange{}, fmt.Errorf("estate: %s: encode the arguments: %w", spec.name, err)
	}
	return exchange{
		key: key,
		local: func(ctx context.Context, b Backend) (any, error) {
			return o.serve(ctx, b, actor, args)
		},
		encoded: encoded,
		decode: func(raw json.RawMessage) (any, error) {
			var out R
			if len(raw) == 0 {
				return out, nil
			}
			if spec.whole {
				// AN ANSWER A CALLER CARRIES INTO A WRITE is read whole
				// or not at all ([op.answeredWhole]).
				unknown, decodeErr := decodeInput(raw, &out)
				switch {
				case decodeErr != nil:
					return out, decodeErr
				case len(unknown) > 0:
					var zero R
					return zero, &ErrUnknownKeys{Op: spec.name, Keys: prefixed("result", unknown),
						Answer: true}
				}
			} else if decodeErr := json.Unmarshal(raw, &out); decodeErr != nil {
				return out, decodeErr
			}
			// A PEER'S CAVEATS, rebuilt at this one boundary: no caveat
			// decodes from JSON anywhere else (internal/caveat's peer.go).
			return out, caveat.RebuildFromPeer(raw, &out)
		},
		unread: func() ([]string, error) {
			var in A
			return decodeInput(encoded, &in)
		},
		written: written,
	}, nil
}

// unreadHere answers this node's own copy refusing an in-process request for
// naming a value its build does not know (jsonv2.ErrUnknownName from the
// request's decoder, or gitlet.IsUnknownToThisBuild from a code write's
// Validate) as the wire answers the same request: the keys a serving node of
// this build refuses its arguments for ([exchange.unread]), with this node
// among the nodes that do not declare them. So the walk moves on to a node
// whose build knows the value (a run's row may hold one another build wrote)
// rather than ending here refusing the request as malformed. Nil for any other
// answer, and for a request that reads whole.
func unreadHere(spec *opSpec, x exchange, node string, err error) *ErrUnknownKeys {
	if _, unknown := unknownMember(err); !unknown || x.unread == nil {
		return nil
	}
	keys, decodeErr := x.unread()
	if decodeErr != nil || len(keys) == 0 {
		// A REQUEST THIS BUILD CANNOT READ BACK, or one that reads whole, names no
		// key a peer of this build would refuse, so this node's answer stands.
		//nolint:nilerr // Deliberate: see the paragraph above.
		return nil
	}
	return &ErrUnknownKeys{Op: spec.name, Keys: prefixed("args", keys), Nodes: []string{node}}
}

// ErrUnserved is an operation no node ran: every data node the asker's view
// named, this node among them where it holds the estate, answered that its
// copy of the domain is out of service, could not run it or was behind, or did
// not answer; for a GIT operation ([op.keyedOnGit]), every Gitlet node.
//
// It claims that nothing ran, never of a write somebody may have run: a walk
// that sent a write and heard no answer is [ErrOutcomeUnknown] instead
// ([walk.unserved]), so only a read or a git walk ends here with a silent node
// ([ErrUnserved.Silent]). Nor is it ever a false "not found": a read reported
// empty would say the company has none of what it asked about.
type ErrUnserved struct {
	// Detail is who was asked and what each said, in order.
	Detail string

	// NodeSilent records that a node the walk asked went silent or gave a reply
	// that did not verify ([Router.unheard]); read it through [ErrUnserved.Silent].
	// Exported so it crosses the wire: a walk on the far side of a second hop says
	// the same of its own.
	NodeSilent bool

	// Git records that the walk asked the Gitlet nodes ([Router.routeGit]), so
	// the refusal names them: worded as a data walk's, it would send its reader
	// to the data nodes, which were serving.
	Git bool
}

// Silent reports whether a node the walk asked went silent rather than
// answering that it would not run the operation; a reply this node could not
// believe is no such answer.
//
// Without one, NOTHING RAN the operation, and a caller with a write may say so:
// Gitlet tells a person "no data node took the push", which a re-run lands.
// With one it may not: a committer that appended and lost its reply has run
// the push, and the re-run would report it already up to date.
func (e *ErrUnserved) Silent() bool { return e.NodeSilent }

func (e *ErrUnserved) Error() string {
	what := "estate: no data node serves the estate right now"
	if e.Git {
		what = "estate: no Gitlet node serves the repository's git work right now"
	}
	if e.Detail == "" {
		return what
	}
	return what + ": " + e.Detail
}

// held is an answer the router keeps while it asks another node: one a node
// gave that is not FINAL for its class ([opIdempotentWrite]'s `unvouched`),
// and is the answer if no node gives a better one.
type held struct {
	value any
	err   error
	// node is the data node whose refusal err is, or empty for one this
	// node's own copy gave: [walk.heldAnswer] names it in the log when the
	// refusal becomes the answer.
	node string
}

// heldAnswer is the answer the walk kept, now that no node gave a better one.
// A peer's refusal is logged as it becomes the answer ([peerRefused]), here
// rather than where it was kept, since a refusal another answer replaced was
// never anybody's answer.
func (w *walk) heldAnswer(ctx context.Context, spec *opSpec) (any, error) {
	if w.fallback.node != "" && w.fallback.err != nil {
		peerRefused(ctx, spec, w.fallback.node, w.fallback.err)
	}
	return w.fallback.value, w.fallback.err
}

// peerRefused logs the data node whose condition failure is, as it is handed
// back as this node's answer, at the level the surface logs the condition at
// ([statelog.ConditionLevel]). This is the one place that knows whose it was:
// the error carries the refusal and not the node, so an administrator sent to
// "the logs" would otherwise check the node they asked rather than the one
// that refused.
func peerRefused(ctx context.Context, spec *opSpec, node string, failure error) {
	if errors.Is(failure, statelog.ErrUnavailable) {
		log.Log(ctx, statelog.ConditionLevel(failure, slog.LevelDebug),
			"estate_peer_refused", "node", node, "op", spec.name,
			"error", failure.Error())
	}
}

// walk is one request's account of the nodes it asked: why each ran nothing,
// the answer it keeps while it asks the next, and the nodes it passed over as
// WORSE ones to ask ([worse]), which it comes back to last.
type walk struct {
	reasons  []string
	fallback *held

	// silent is set once a node the walk asked gave no reply this node
	// believes — it said nothing, or what it said did not verify
	// ([Router.unheard]): a walk that ends unserved then may have had the
	// operation run by that node ([ErrUnserved.Silent]).
	silent bool

	// lagging is every node that answered that its copy ONLY lags its logs
	// ([unservedLagging]), and behindConfig every node behind the company's
	// configuration (it answered so, or [Router.remembersBehind]), each in the
	// order the walk reached it, this node by its own id where its copy answered so.
	//
	// TWO LISTS, because [Router.lastResort] asks the first before the second: a
	// lagging copy answers under the current configuration, a node behind it under
	// the replaced one. One list, ordered by when the router learned of each, made
	// the preference hang on the router's memory and sent a repository's pushes to
	// two committers. Within each the walk's order stands, so a keyed operation's
	// last resort never jumps among nodes of the same standing.
	lagging, behindConfig []string

	// unanswered is every node a repeatable WRITE was sent to that gave no
	// answer this node believes — one that may have run it. See
	// [walk.unserved].
	unanswered []string

	// spent is what the request already paid for on the nodes it asked (a
	// barrier's read index, every position waited out for its whole bound, every
	// log a barrier ran out on), handed to every node asked after them
	// ([statelog.Spent]), so a walk of N nodes appends at most one barrier and
	// waits at most one bound per position.
	spent statelog.Spent

	// unknown is what every node whose build could not read the request
	// whole refused ([ErrUnknownKeys]) — the keys, and the nodes — or nil
	// where none did. Each ran nothing, so the walk moved on from it; where
	// no node serves the request, this is the answer ([walk.unserved]).
	unknown *ErrUnknownKeys
}

// unreadBy is node refusing the request for keys its build does not declare —
// or, where it answered for a walk of its own, for keys the data nodes it asked
// do not ([ErrUnknownKeys.Nodes]): it ran nothing, so the walk keeps what it
// refused and asks the next node.
func (w *walk) unreadBy(node string, refused *ErrUnknownKeys) {
	if w.unknown == nil {
		w.unknown = &ErrUnknownKeys{Op: refused.Op}
	}
	nodes := refused.Nodes
	if len(nodes) == 0 {
		nodes = []string{node}
	}
	for _, n := range nodes {
		if !slices.Contains(w.unknown.Nodes, n) {
			w.unknown.Nodes = append(w.unknown.Nodes, n)
		}
	}
	for _, key := range refused.Keys {
		if !slices.Contains(w.unknown.Keys, key) {
			w.unknown.Keys = append(w.unknown.Keys, key)
		}
	}
	w.note("%s: its build does not declare %s, so it ran nothing", node, words.And(asSaid(refused.Keys)))
}

// refusedOr is unserved, the account of a walk no node served — or, where a
// node refused the request for keys its build does not declare, that refusal
// with the account beside it ([ErrUnknownKeys.Unserved]), so the caller is told
// what would serve it: a data node of the writer's build.
func (w *walk) refusedOr(unserved *ErrUnserved) error {
	if w.unknown == nil {
		return unserved
	}
	refused := *w.unknown
	refused.Unserved = unserved
	return &refused
}

// paid adds what one node's refusal of a read had paid for to what the walk
// carries.
func (w *walk) paid(err error) {
	var refused *statelog.Refused
	if errors.As(err, &refused) {
		w.spent = w.spent.Merge(refused.Spent)
	}
}

// waitedOut is a node behind the floors it was handed ([unservedBehind]):
// its one way to say so is a floor wait that ran its whole bound, so every
// node asked after it waits for none of them.
func (w *walk) waitedOut(floors []statelog.Position) {
	w.spent = w.spent.Merge(statelog.Spent{Waited: floors})
}

func (w *walk) note(format string, args ...any) {
	w.reasons = append(w.reasons, fmt.Sprintf(format, args...))
}

// unserved is the answer to a walk no node served: the estate unserved, or,
// where a write was sent to a node that never answered, [ErrOutcomeUnknown].
//
// The difference is whether anybody may have run it. Nodes that each said "I
// did not run it" ran nothing, and the caller may write again. A node that did
// not answer may have committed the write and lost its acknowledgement, the
// case an [opIdempotentWrite]'s operation id exists for, so "unserved" would
// invite a retry under a FRESH id: a second write. Unknown is the honest
// answer, and the same operation id the safe retry.
func (w *walk) unserved(spec *opSpec) error {
	detail := strings.Join(w.reasons, "; ")
	if len(w.unanswered) > 0 {
		return fmt.Errorf("%w (%s, asked of %s: %s)", ErrOutcomeUnknown, spec.name,
			strings.Join(w.unanswered, ", "), detail)
	}
	return w.refusedOr(&ErrUnserved{Detail: detail, NodeSilent: w.silent})
}

// askFailed is the answer to a walk whose next request could not be made at
// all: that failure — unless a repeatable write was already SENT to a node
// that never answered, which may have run it. Then the walk's answer is
// unknown, whatever stopped it asking the next one ([walk.unserved]).
func (w *walk) askFailed(spec *opSpec, err error) error {
	if len(w.unanswered) == 0 {
		return err
	}
	return fmt.Errorf("%w (%s, asked of %s): %w", ErrOutcomeUnknown, spec.name,
		strings.Join(w.unanswered, ", "), err)
}

// route runs one operation: this node first where its copy serves, then the
// other data nodes in order, and last the nodes that answered that they are a
// worse one to ask ([worse]): a copy that lags its logs, then a node behind
// the company's configuration, the latter remembered for [suspectFor]
// ([Router.lastResort], [Router.markBehindConfig]). Each is still asked, told
// to take the request anyway, rather than refused as though the estate had no
// server: package doc, "A domain's copy that is WRONG is out of service; one
// that LAGS is a worse node" and "A node behind the company's configuration is
// a worse node too".
//
// A KEYED operation ([op.keyedBy]) asks this node at its place in the key's
// order ([Router.keyedOrder]) rather than first; every other step is the same.
func (r *Router) route(ctx context.Context, spec *opSpec, actor *Actor, x exchange) (any, error) {
	if spec.git {
		return r.routeGit(ctx, spec, actor, x)
	}
	w := &walk{}
	// THIS NODE FIRST where its copy serves, in-process and held to the floors a
	// remote node enforces, even while it lags where the caller asked for this
	// node's copy ([callHere]). A keyed operation asks it at its place, below.
	if x.key == "" {
		if value, final, ranErr := r.tryLocal(ctx, spec, x, x.here, w); final {
			return value, ranErr
		}
	}
	budget := r.budgetFor(ctx, spec, x)
	nodes, err := r.placement.Holders()
	if err != nil {
		// A PLACEMENT THAT CANNOT SAY WHO HOLDS THE ESTATE names no peer, and takes
		// nothing from this node's own copy, passed over above only for lagging: it is
		// still the worse node to ask rather than no node, or a single data node whose
		// copy a burst put behind would refuse its own seats for as long as the
		// presence view could not answer. A KEYED operation has not asked it yet: with
		// no node to order, its own copy is the one left.
		if x.key != "" {
			if value, final, ranErr := r.tryLocal(ctx, spec, x, false, w); final {
				return value, ranErr
			}
		}
		if value, final, ranErr := r.lastResort(ctx, spec, actor, x, budget, w); final {
			return value, ranErr
		}
		if w.fallback != nil {
			return w.heldAnswer(ctx, spec)
		}
		if len(w.reasons) > 0 {
			return nil, fmt.Errorf("estate: %s: read who holds the estate: %w (%s)", spec.name, err,
				strings.Join(w.reasons, "; "))
		}
		return nil, fmt.Errorf("estate: %s: read who holds the estate: %w", spec.name, err)
	}
	for _, node := range r.orderFor(spec, x.key, nodes, x.carrying(actor)) {
		if node == r.self {
			// THIS NODE'S TURN IN A KEYED ORDER: in-process, held to the
			// same floors and the same verdict a remote node is.
			if value, final, ranErr := r.tryLocal(ctx, spec, x, false, w); final {
				return value, ranErr
			}
			continue
		}
		if r.remembersBehind(spec, node) {
			// IT WOULD REFUSE AN ORDINARY ASK, as it did inside the
			// window, so it is sent none: the last resort asks it, told
			// to answer anyway, once every other node has run nothing.
			w.behindConfig = append(w.behindConfig, node)
			w.note("%s: said within %v that it is behind the company's configuration, "+
				"so it is asked only when no other data node takes the request", node,
				suspectFor)
			continue
		}
		rep, got, err := r.askFor(ctx, spec, actor, x, node, budget, false, w.spent)
		if err != nil {
			return nil, w.askFailed(spec, err)
		}
		if !got.answered() {
			if err := r.unheard(ctx, spec, node, got, w); err != nil {
				return nil, err
			}
			continue
		}
		if value, final, ranErr := r.settle(ctx, spec, x, node, rep, w); final {
			return value, ranErr
		}
	}
	if value, final, ranErr := r.lastResort(ctx, spec, actor, x, budget, w); final {
		return value, ranErr
	}
	if w.fallback != nil {
		return w.heldAnswer(ctx, spec)
	}
	// WHO WAS ASKED AND WHAT EACH SAID GOES TO THE LOG HERE, the one place
	// that knows it: every reader of the answer tells its caller in fixed
	// words (internal/agent/builtin's Condition and Unanswered), because the
	// detail names nodes and their copies' faults, and a model's prompt or a
	// person's screen can act on neither.
	detail := strings.Join(w.reasons, "; ")
	if len(w.unanswered) > 0 {
		log.WarnContext(ctx, "estate_outcome_unknown", "op", spec.name,
			"unanswered", strings.Join(w.unanswered, ", "), "detail", detail)
	} else {
		log.WarnContext(ctx, "estate_unserved", "op", spec.name, "detail", detail)
	}
	return nil, w.unserved(spec)
}

// lastResort asks the nodes the walk passed over as worse ones, telling each
// to take the request anyway, this node's own in-process: every copy that only
// lags FIRST, then every node behind the company's configuration, each in the
// walk's order ([walk.lagging]), an order by REASON, the same whatever the
// router has heard. final is set when one of them gives the answer.
func (r *Router) lastResort(ctx context.Context, spec *opSpec, actor *Actor, x exchange,
	budget time.Duration, w *walk) (value any, final bool, err error) {

	// TAKEN AND CLEARED before anything is asked: a node that answers the
	// last resort with the same refusal again — one that does not honour
	// the say-so — is added back to a list nothing reads, never asked twice.
	worse := slices.Concat(w.lagging, w.behindConfig)
	w.lagging, w.behindConfig = nil, nil
	for _, node := range worse {
		if node == r.self {
			if value, final, ranErr := r.tryLocal(ctx, spec, x, true, w); final {
				return value, true, ranErr
			}
			continue
		}
		rep, got, askErr := r.askFor(ctx, spec, actor, x, node, budget, true, w.spent)
		if askErr != nil {
			return nil, true, w.askFailed(spec, askErr)
		}
		if !got.answered() {
			if unheardErr := r.unheard(ctx, spec, node, got, w); unheardErr != nil {
				return nil, true, unheardErr
			}
			continue
		}
		if value, final, ranErr := r.settle(ctx, spec, x, node, rep, w); final {
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
	b, wrong, unserved := unservedCopy(ctx, r.local, spec)
	if unserved {
		// SAID, like every other node that ran nothing: an estate nobody
		// serves names why this node's own copy was passed over, which
		// on a single data node is the whole of the answer.
		w.note("%s: %s", r.self, outOfService(r.self, wrong))
		return nil, false, nil
	}
	b.ServerSeams = r.seams
	b.verifiers = r.env.verifiers
	value, refusal, why, ran := r.runLocal(ctx, spec, b, x, acceptLagging, w.spent)
	unread := unreadHere(spec, x, r.self, ran)
	switch {
	case why != "":
		// IN-PROCESS, so nothing is remembered: asking again costs no round trip. It
		// still waits its turn by REASON: a copy behind the configuration is asked
		// after a peer's that only lags, since saving a round trip never makes a
		// decide under the replaced configuration sound.
		switch refusal {
		case unservedLagging:
			w.lagging = append(w.lagging, r.self)
		case unservedBehindConfig:
			w.behindConfig = append(w.behindConfig, r.self)
		case unservedBehind:
			w.waitedOut(r.floorsFor(spec))
		}
		w.note("%s: %s", r.self, why)
	case unread != nil:
		// A BUILD THAT DOES NOT KNOW A VALUE THE REQUEST NAMES RAN NOTHING, in process
		// as on the wire ([unreadHere], [Router.settle]): the walk asks the next node,
		// remembering nothing.
		w.unreadBy(r.self, unread)
	case unvouched(spec, value, ran):
		w.fallback = &held{value: value, err: ran}
	case refusedHere(spec, ran):
		w.fallback = &held{err: ran}
		w.paid(ran)
		w.note("%s: %v", r.self, ran)
	default:
		return value, true, ran
	}
	return nil, false, nil
}

// refusedHere is a READ a copy refused for a reason about ITSELF (behind its
// barrier, stalled, holding a record it cannot decode, below the floor,
// evicted, cut off from its broker or a quorum), which another data node may
// serve ([statelog.ReadRefusal.Elsewhere]). The node ran nothing, so the walk
// asks the next one and keeps the refusal only where no node does better.
//
// What the refusal paid for goes with the read ([statelog.Refused.Spent],
// [walk.spent]): the next node reads at the index the refusing one established
// rather than appending a barrier and waiting a read budget of its own.
//
// A read only: a write's repeats are its class's rules (statelog.Unavailable),
// and a read is safe to ask anywhere.
func refusedHere(spec *opSpec, err error) bool {
	var refused *statelog.Refused
	return spec.class == opRead && errors.As(err, &refused) && refused.Code.Elsewhere()
}

// askFor asks one data node to run the operation, carrying this node's floor
// on the operation's log, and stops carrying every floor the node says is on a
// generation its log abandoned. err is set only where the request could not be
// made at all.
func (r *Router) askFor(ctx context.Context, spec *opSpec, actor *Actor, x exchange,
	node string, budget time.Duration, acceptLagging bool, spent statelog.Spent) (reply, heard, error) {

	floors := r.floorsFor(spec)
	req := request{
		Op: spec.name, Args: x.encoded,
		Floors: floors, From: r.self, AcceptLagging: acceptLagging,
	}
	if actor != nil {
		// AN INPUT OF THE OPERATION, as its arguments are: it crosses under
		// the same rule, and the serving node reads it whole or refuses the
		// request ([inputs]).
		encoded, err := encodeInput(actor)
		if err != nil {
			return reply{}, heard{}, fmt.Errorf("estate: %s: encode the actor: %w", spec.name, err)
		}
		req.Actor = encoded
	}
	if !spent.IsZero() {
		req.Spent = &spent
	}
	rep, got, err := r.ask(ctx, node, spec.lane.subject(node), req, budget, r.wholeAttempt(spec, x))
	if err != nil {
		return reply{}, got, fmt.Errorf("estate: %s: %w", spec.name, err)
	}
	if got.answered() {
		r.session.Forget(named(floors, rep.Obsolete)...)
		x.record(rep)
	}
	return rep, got, nil
}

// heard is what came back from one node: nothing at all, a reply this node may
// believe, or one it may not — whose reason is the string.
type heard struct {
	// replied is whether anything came back.
	replied bool

	// unbelieved is why what came back is not believed — unsigned, signed
	// for another request, or undecodable — and empty for a reply that
	// verified, decoded and answers this request ([Envelopes.openReply]).
	unbelieved string
}

// answered reports a reply this node believes.
func (h heard) answered() bool { return h.replied && h.unbelieved == "" }

// unheard is what a node that gave no reply this node believes costs the
// walk: nothing more for a read; the request itself for a once-write, which the
// node may have run; and the caller's own ending.
//
// A REPEATABLE WRITE is asked of the next node under the same operation id and
// the node remembered ([walk.unanswered]), so a walk none answers ends unknown
// rather than unserved, as does one whose caller stopped listening, which says
// nothing about whether it ran.
//
// A REPLY NOT BELIEVED IS NO ANSWER, never "did not run": a refusal signed for
// another request is what somebody would replay to run a once-write twice. Its
// node is up, so not suspected, but recorded [walk.silent]: it may have run it.
func (r *Router) unheard(ctx context.Context, spec *opSpec, node string, h heard, w *walk) error {
	if h.replied {
		log.WarnContext(ctx, "estate_reply_refused", "node", node, "op", spec.name,
			"detail", h.unbelieved)
	}
	if spec.class == opOnceWrite {
		if h.replied {
			return fmt.Errorf("%w (%s, asked of %s: %s)", ErrOutcomeUnknown, spec.name,
				node, h.unbelieved)
		}
		return fmt.Errorf("%w (%s, asked of %s)", ErrOutcomeUnknown, spec.name, node)
	}
	if ctx.Err() != nil {
		if spec.class == opIdempotentWrite {
			w.unanswered = append(w.unanswered, node)
			return fmt.Errorf("%w (%s, asked of %s): %w", ErrOutcomeUnknown,
				spec.name, strings.Join(w.unanswered, ", "), ctx.Err())
		}
		return fmt.Errorf("estate: %s: %w", spec.name, ctx.Err())
	}
	w.silent = true
	if spec.class == opIdempotentWrite {
		w.unanswered = append(w.unanswered, node)
	}
	if h.replied {
		w.note("%s: %s", node, h.unbelieved)
		return nil
	}
	w.note("%s: no answer", node)
	return nil
}

// settle is what one node's reply decides: final when it is the answer, and
// otherwise noted on the walk — a node that ran nothing, or an answer not final
// for its class, kept in case no node gives a better one.
func (r *Router) settle(ctx context.Context, spec *opSpec, x exchange, node string,
	rep reply, w *walk) (value any, final bool, err error) {

	if rep.Unserved != "" {
		switch rep.Unserved {
		case unservedLagging:
			w.lagging = append(w.lagging, node)
		case unservedBehindConfig:
			// ASKED LAST, after every copy that only lags, and remembered, because
			// unlike a lag it lasts: forgotten, every later request would send it a
			// refusal's round trip first (for a push, its whole proposal).
			w.behindConfig = append(w.behindConfig, node)
			r.markBehindConfig(node)
		case unservedBehind:
			w.waitedOut(r.floorsFor(spec))
		case unservedOutOfService:
			r.markOutOfService(node, spec.copies)
		case unservedNoBackend:
			// A KEYED ORDER HAS NO STICKY NODE to move the next request off a node
			// that ran nothing, so it is remembered; otherwise every push the node ranks
			// first would send it the whole proposal again for as long as it refused. An
			// unkeyed operation moves off it by its sticky node.
			if x.key != "" {
				r.markDeclined(node, spec.name)
			}
		}
		w.note("%s: %s", node, rep.Detail)
		return nil, false, nil
	}
	var failure error
	if rep.Err != nil {
		failure = decodeError(rep.Err)
		if refusedHere(spec, failure) {
			// NOT ANSWERED-BY, so not the node asked first next time:
			// it ran nothing, and what made it refuse is its own.
			w.fallback = &held{err: failure, node: node}
			w.paid(failure)
			w.note("%s: %v", node, failure)
			return nil, false, nil
		}
		var refused *ErrUnknownKeys
		if errors.As(failure, &refused) {
			// A BUILD THAT COULD NOT READ THE REQUEST WHOLE RAN NOTHING, whatever the
			// class ([inputs]), so the next node is asked and this one is not made
			// sticky. A KEYED operation, with no sticky node to move off it, remembers
			// WHAT it refused ([Router.markUnread]): the node still serves every request
			// that does not carry those keys.
			w.unreadBy(node, refused)
			if x.key != "" {
				r.markUnread(node, spec.name, refused.Keys)
			}
			return nil, false, nil
		}
	}
	op := ""
	if x.key != "" {
		op = spec.name
	}
	r.markAnswered(node, spec.copies, op)
	if failure != nil {
		if unvouched(spec, nil, failure) {
			w.fallback = &held{err: failure, node: node}
			w.note("%s: unvouched", node)
			return nil, false, nil
		}
		peerRefused(ctx, spec, node, failure)
		return nil, true, failure
	}
	value, err = x.decode(rep.Result)
	var unread *ErrUnknownKeys
	if errors.As(err, &unread) {
		// AN ANSWER THIS BUILD CANNOT READ WHOLE ([op.answeredWhole]) is final: it
		// is what the copies hold, and another node's answer would not change what this
		// build declares.
		unread.Nodes = []string{node}
		return nil, true, unread
	}
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
	for _, c := range rep.Committed {
		x.written.Commit(c)
	}
}

// budgetFor is one attempt's budget on one node: the router's own attempt
// ([Router.wholeAttempt]), or for a write on a DATA node whose caller has a
// deadline, that deadline (zero), since a gesture given five minutes must not
// be abandoned after one.
//
// Never for a GIT write, which keeps the router's own attempt
// ([GitWriteAttempt]): its Gitlet node fits every stage into that attempt, so
// one silent through it is wedged rather than slow, and the walk's next Gitlet
// node builds the same push rather than the turn waiting out its deadline.
func (r *Router) budgetFor(ctx context.Context, spec *opSpec, x exchange) time.Duration {
	if _, bounded := ctx.Deadline(); bounded && spec.class != opRead && !spec.git {
		return 0
	}
	return r.wholeAttempt(spec, x)
}

// wholeAttempt is the router's own attempt on one node for spec: the attempt
// its arguments declare ([op.attemptBy]; an engine write's is
// [Timing.GitWriteAttemptFor]), else the [Timing] field for its class and
// placement. It is what a request waits where nothing shorter bounds it, and
// what a node's silence must last to be evidence it is gone ([Router.ask]).
func (r *Router) wholeAttempt(spec *opSpec, x exchange) time.Duration {
	switch {
	case x.attempt != nil:
		return x.attempt(r.timing)
	case spec.class == opRead && spec.git:
		return r.timing.GitReadAttempt
	case spec.class == opRead:
		return r.timing.ReadAttempt
	case spec.git:
		return r.timing.GitWriteAttempt
	}
	return r.timing.WriteAttempt
}

// runLocal runs one operation in-process on this node's own copy, held to
// what the walk has already paid for on the nodes it asked first (spent) as a
// remote node is. why is set, and nothing ran, when the copy could not take it
// — the same answers a remote node gives, and the router moves on from each —
// with refusal the reason a remote node would have answered.
func (r *Router) runLocal(ctx context.Context, spec *opSpec, b Backend, x exchange,
	acceptLagging bool, spent statelog.Spent) (value any, refusal unservedReason, why string, err error) {

	reason, detail, obsolete := ready(ctx, r.self, spec, b, r.floorsFor(spec), acceptLagging, spent)
	r.session.Forget(obsolete...)
	if reason != "" {
		return nil, reason, detail, nil
	}
	value, err = x.local(readSpent(ctx, spec, spent), b)
	switch {
	case errors.Is(err, errNoHalf):
		return nil, unservedNoBackend, ranNothingDetail(r.self, spec.name, err), nil
	case errors.Is(err, errNotAdmitting):
		return nil, unservedNotEstablished, fmt.Sprintf("%s's copy admits no seat yet",
			r.self), nil
	case errors.Is(err, errDecidesNoGrant):
		// THE HARD ARM, answered as a peer answers it — even on the last
		// resort's run, told to answer anyway ([op.judgesGrant]).
		return nil, unservedBehindConfig, decidesNoGrant(r.self), nil
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

// ask sends one request to one node, signed and bound to an id of its own, on
// its operation's lane subject ([opSpec.lane]), waiting budget
// ([Router.budgetFor]). heard says whether a reply came back and is believed;
// err only that the request could not be made, as it could be to no node.
//
// A SILENT NODE IS SUSPECTED ([Router.markSuspect]) only where its silence
// shows it gone rather than slow: nothing serves its subject, a budget of the
// router's own ran out ([errAttemptSpent]), or it stayed silent a whole
// attempt of its class (whole). A node only behind answers inside each
// ([statelog.WithReplyMargin]); a caller that hung up or gave less than an
// attempt says nothing about the node; and a node whose reply is not believed
// is up ([Router.unheard]).
func (r *Router) ask(ctx context.Context, node, subject string, req request,
	budget, whole time.Duration) (reply, heard, error) {

	attempt, cancel := attemptContext(ctx, budget)
	defer cancel()
	req.ID = signedask.NewID()
	req.Deadline = deadlineOf(attempt)
	sealed, err := r.env.sealRequest(req)
	if err != nil {
		return reply{}, heard{}, err
	}
	sent := r.now()
	replies, err := r.queue.Ask(attempt, subject, sealed, 1)
	if err != nil {
		// THE ASK COULD NOT BE MADE AT ALL — a queue that is not live,
		// a request over the size limit.
		return reply{}, heard{}, err
	}
	if len(replies) == 0 {
		if attempt.Err() == nil || errors.Is(context.Cause(attempt), errAttemptSpent) ||
			r.now().Sub(sent) >= whole {
			r.markSuspect(node)
		}
		return reply{}, heard{}, nil
	}
	rep, why, err := r.env.openReply(replies[0], req.ID)
	if err != nil {
		// A SIGNED REPLY THIS BUILD CANNOT DECODE IS NO ANSWER ([Router.unheard]),
		// never a request that could not be made: the node may have run it, so a
		// once-write is unknown and any other class asks the next node.
		return reply{}, heard{replied: true,
			unbelieved: "its reply cannot be decoded by this build: " + err.Error()}, nil
	}
	return rep, heard{replied: true, unbelieved: why}, nil
}

// order is the order the data nodes are asked in for spec, an unkeyed
// operation: the STICKY node for its copies ([opSpec.copies]; the last to
// answer, whose applier most likely has this node's writes) first, the rest,
// every node `out_of_service` for them inside [suspectFor], every SUSPECT node
// last ([Router.classes]), [rendezvous.Order] on this node's id within each
// class; never this node, asked in-process or not at all. Rendezvous scatters a silent node's askers over the survivors;
// the sticky node keeps an asker where it is, so a joining node is asked only
// by askers with none.
//
// Sticky PER set of copies, because each domain has its own applier and its
// own out-of-service verdict (ADR-0079): one sticky node for every domain made
// a router chase its tail between two peers each out of service for one domain.
func (r *Router) order(spec *opSpec, nodes []string) []string {
	// rendezvous.Order hands back a slice of its own, so the placement's
	// listing is never the one trimmed or sorted here; and removing members
	// from its answer leaves the rest in rendezvous order.
	nodes = slices.DeleteFunc(rendezvous.Order(r.self, nodes),
		func(n string) bool { return n == r.self || n == "" })
	class := r.classes(spec, false, nil)
	slices.SortStableFunc(nodes, func(a, b string) int { return class(a) - class(b) })
	return nodes
}

// orderFor is the order the data nodes are asked in for one request:
// [Router.order] for an operation any copy answers equally — this node
// excluded, since it was asked first — and [Router.keyedOrder] for a keyed
// one, this node at its place where it holds the estate. carrying says which
// of a refusal's keys the request carries ([exchange.carrying]), nil for none.
func (r *Router) orderFor(spec *opSpec, key string, nodes []string, carrying func([]string) bool) []string {
	if key == "" {
		return r.order(spec, nodes)
	}
	return r.keyedOrder(spec, key, nodes, r.local != nil, carrying)
}

// keyedOrder is the order a KEYED operation asks nodes in: [Router.keyed] on
// the key, this node included where self says it answers, then demoted by
// [Router.classes] (a node that answered `out_of_service`, ran nothing for the
// operation, or refused a key this request carries inside [suspectFor], after
// the rest; every silent node last), the key's order kept within each by a
// STABLE sort. ONE order for a push's data nodes and a repository's Gitlet
// nodes.
//
// NO STICKY NODE: every node computing the same order from the same set is the
// point. One committer holding a repository's whole burst commits it in a few
// records; two holding parts contend on its one subject, and each lost round
// costs a re-decide and a re-append of a whole batch.
func (r *Router) keyedOrder(spec *opSpec, key string, nodes []string, self bool,
	carrying func([]string) bool) []string {
	ordered := r.keyed(key, nodes, self)
	class := r.classes(spec, true, carrying)
	slices.SortStableFunc(ordered, func(a, b string) int { return class(a) - class(b) })
	return ordered
}

// keyed is the rendezvous half of every keyed order, before the router's
// memory demotes anybody: [rendezvous.Order] on key over nodes, this one
// included where self says it answers, an empty id never, a node listed twice
// once. ONE function for [Router.keyedOrder], [Router.Home] and
// [Router.Maintainer], so a repository's first reader, its git work's home and
// its maintenance node are one computation.
func (r *Router) keyed(key string, nodes []string, self bool) []string {
	candidates := slices.DeleteFunc(slices.Clone(nodes), func(n string) bool {
		return n == "" || n == r.self
	})
	if self {
		candidates = append(candidates, r.self)
	}
	// rendezvous.Order keeps duplicates, adjacent: a placement listing a
	// node twice names it once here.
	return slices.Compact(rendezvous.Order(key, candidates))
}

// classes ranks each node for spec by the router's memory, lowest asked first:
// the STICKY node for its copies (never in a keyed order), then the rest, then
// every node passed over inside [suspectFor] (`out_of_service` for its copies,
// or for a keyed operation `no_backend` or a refused key this request carries;
// carrying nil for none), then every SUSPECT node. It sweeps lapsed windows.
// ONE reading for both orders, so a demotion holds in both.
//
// A node remembered behind the configuration HAS NO CLASS: the walk hands it
// straight to the last resort ([Router.remembersBehind]), so its place only
// orders it among the others there, and must be the place a router that heard
// of none gives it, or two routers send one repository's pushes to two
// committers.
func (r *Router) classes(spec *opSpec, keyed bool, carrying func([]string) bool) func(node string) int {
	// MATCHED BEFORE THE LOCK, since matching decodes the request: a node
	// that refused a key this request carries is passed over for it.
	unread := map[string]bool{}
	if carrying != nil {
		for node, keys := range r.unreadFor(spec, keyed) {
			if carrying(keys) {
				unread[node] = true
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	sticky := ""
	if !keyed {
		sticky = r.sticky[spec.copies]
	}
	now := r.now()
	suspect := map[string]bool{}
	for n, until := range r.suspect {
		if now.Before(until) {
			suspect[n] = true
		} else {
			delete(r.suspect, n)
		}
	}
	passedOver := map[string]bool{}
	for c, until := range r.passedOver {
		switch {
		case !now.Before(until):
			delete(r.passedOver, c)
		case c.copies == spec.copies:
			passedOver[c.node] = true
		}
	}
	for d, until := range r.declined {
		switch {
		case !now.Before(until):
			delete(r.declined, d)
		case keyed && d.op == spec.name:
			passedOver[d.node] = true
		}
	}
	for node := range unread {
		passedOver[node] = true
	}
	for n, until := range r.behindConfig {
		if !now.Before(until) {
			delete(r.behindConfig, n)
		}
	}
	return func(n string) int {
		switch {
		case suspect[n]:
			return 3
		case passedOver[n]:
			return 2
		case sticky != "" && n == sticky:
			return 0
		}
		return 1
	}
}

// attemptContext is one attempt's context: the caller's, capped by budget —
// ended by [errAttemptSpent] where the cap is what ends it.
//
// A ZERO budget is the caller's own deadline, unchanged.
func attemptContext(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); budget == 0 || (ok && time.Until(deadline) < budget) {
		return context.WithCancel(ctx)
	}
	return context.WithTimeoutCause(ctx, budget, errAttemptSpent)
}

// errAttemptSpent is the cause a budget of the router's OWN ends a wait with —
// one attempt on one data node, or the admission question's whole — told apart
// from the caller's own ending ([Router.ask]), because a node's silence is
// evidence about the node only through a wait the router chose for it.
var errAttemptSpent = errors.New("estate: the router's own budget for the request ran out")

// markSuspect asks node last for every domain for [suspectFor], and takes away
// every domain's sticky place it held: a node that said nothing is no node to
// stay on.
func (r *Router) markSuspect(node string) {
	r.mu.Lock()
	r.suspect[node] = r.now().Add(suspectFor)
	for domain, sticky := range r.sticky {
		if sticky == node {
			delete(r.sticky, domain)
		}
	}
	r.mu.Unlock()
	// OUTSIDE THE LOCK: the placements are somebody else's. BOTH, because a
	// suspicion is the node's, not one placement's, and a placement drops a node
	// only when told: an untold git placement kept a dead Gitlet node in every
	// repository's order until its own view caught up.
	r.placement.Unanswered(node)
	r.gitPlacement.Unanswered(node)
}

// markOutOfService asks node last for [suspectFor] for the operations that read
// copies: one of its copies of them is out of service (ADR-0079), and every
// other place it holds stays — so does the node, which answered.
func (r *Router) markOutOfService(node, copies string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.passedOver[nodeCopy{node: node, copies: copies}] = r.now().Add(suspectFor)
	if r.sticky[copies] == node {
		delete(r.sticky, copies)
	}
}

// markDeclined asks node last for [suspectFor] for op, a KEYED operation it
// answered `no_backend` for: it ran nothing for it, and what made it refuse
// outlasts one request. Every other place it holds stays — so does the node,
// which answered.
func (r *Router) markDeclined(node, op string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.declined[nodeOp{node: node, op: op}] = r.now().Add(suspectFor)
}

// markUnread asks node last for [suspectFor] for every request of op, a KEYED
// operation, that carries one of keys, which its build does not declare
// ([ErrUnknownKeys]), and for no other: an older build serves every request
// that does not use what it lacks. Declined for the whole operation, one newer
// read moved every repository the node is home to onto a runner-up filling a
// copy for each, and one newer push gave its repositories a second committer.
// The keys join those refused before, inside one window.
func (r *Router) markUnread(node, op string, keys []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := nodeOp{node: node, op: op}
	held := r.unread[d]
	for _, key := range keys {
		if !slices.Contains(held.keys, key) {
			held.keys = append(held.keys, key)
		}
	}
	held.until = r.now().Add(suspectFor)
	r.unread[d] = held
}

// unreadFor is, for a keyed operation spec, every node remembered refusing a
// request of it for keys its build does not declare, and those keys — read
// under the lock and matched outside it ([Router.classes]), since matching
// decodes the request. It sweeps out every window that has lapsed.
func (r *Router) unreadFor(spec *opSpec, keyed bool) map[string][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	out := map[string][]string{}
	for d, held := range r.unread {
		switch {
		case !now.Before(held.until):
			delete(r.unread, d)
		case keyed && d.op == spec.name:
			out[d.node] = slices.Clone(held.keys)
		}
	}
	return out
}

// markBehindConfig remembers for [suspectFor] that node answered
// `behind_config`: until the window ends, every gated operation asks it only as
// the walk's last resort, after every copy that only lags, told at once to
// answer anyway ([Router.remembersBehind]), and a keyed one asks its runner-up
// instead. Per NODE, since its configuration is its posture for all it holds.
//
// NOTHING BUT THE WINDOW ENDS IT: the router sends no ordinary ask that could
// show the node current, and neither the last resort's answer nor the ungated
// ping's says so. A node current again is asked last until the window ends;
// one still behind costs one refusal a window. Its sticky places stay, so the
// first ask after the window learns whether it still is.
func (r *Router) markBehindConfig(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.behindConfig[node] = r.now().Add(suspectFor)
}

// remembersBehind reports whether the walk hands node straight to its last
// resort for spec rather than ask it: it answered `behind_config` inside
// [suspectFor] ([Router.markBehindConfig]), and spec is one the preference
// holds back at all — an ungated operation, admission's ping, is not, on the
// node that answers it or here.
func (r *Router) remembersBehind(spec *opSpec, node string) bool {
	if spec.ungated {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	until, ok := r.behindConfig[node]
	return ok && r.now().Before(until)
}

// markAnswered makes node the one asked first for the operations that read
// copies: it ran one, so its copies of them serve — and, where it ran the
// keyed operation op, it serves that again too.
func (r *Router) markAnswered(node, copies, op string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sticky[copies] = node
	delete(r.suspect, node)
	delete(r.passedOver, nodeCopy{node: node, copies: copies})
	if op != "" {
		delete(r.declined, nodeOp{node: node, op: op})
	}
}

// unvouched reports an idempotent write's answer that is NOT final: an
// `unknown` the answering node's ledger could not vouch for
// ([statelog.Result.Unvouched]) on any step the answer reports
// ([worklet.PlaceResult], [doclet.Written]), or a walking gesture stopped at
// such a step ([worklet.ErrStepUnvouched]). Another node's ledger may hold the
// row, so the router asks it under the same operation id first. Never true for
// another class: a read has no ledger, and a once-write is never repeated.
func unvouched(spec *opSpec, value any, err error) bool {
	if spec.class != opIdempotentWrite {
		return false
	}
	if err != nil {
		return errors.Is(err, worklet.ErrStepUnvouched)
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
// It asks that copy: this node's own where its [SeatCopy] serves, never passed
// over for a peer's, since a seat on a copy one record behind would act on
// rows that are behind; otherwise the first data node that answers, trusted
// for [admissionTrust]. A wrong copy of a domain that does not gate seats
// leaves admission local (ADR-0079). A copy that admits no seat answers the
// zero [Served]: it serves none of the halves.
func (r *Router) Serves(ctx context.Context) (Served, error) {
	if r.local != nil {
		if b, ok := r.local.For(ctx, SeatCopy); ok {
			if b.Admits != nil && !b.Admits(ctx) {
				return Served{}, nil
			}
			return b.served(), nil
		}
	}
	r.mu.Lock()
	cached := r.admitted
	r.mu.Unlock()
	if cached != nil && r.now().Sub(cached.at) < admissionTrust {
		return cached.served, cached.err
	}
	asked, cancel := context.WithTimeoutCause(ctx, r.timing.AdmissionAsk, errAttemptSpent)
	defer cancel()
	out, err := call(asked, r, opPing, nil, struct{}{})
	r.mu.Lock()
	r.admitted = &admission{at: r.now(), served: out, err: err}
	r.mu.Unlock()
	return out, err
}
