package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/rendezvous"
	"github.com/crewlet/crewlet/internal/signedask"
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

	// Unanswered tells the placement a node it named did not answer — through
	// a wait of the router's own, never one its caller ended ([Router.ask]) —
	// so a node that left on a clean stop is gone from the next answer rather
	// than from the one a heartbeat later. It must not block.
	Unanswered(node string)
}

// LocalBackends is this data node's own copy of the estate.
type LocalBackends interface {
	// For is the backend over this node's copy of DOMAIN, or false while
	// that copy is OUT OF SERVICE: wrong rather than behind, so the node
	// answers none of that domain's operations from it while its seats'
	// calls on it go to the other data nodes. Another domain's fault is not
	// this one's — every other domain's copy keeps serving (ADR-0049).
	// [SeatCopy] asks for the copy a SEAT is served by.
	//
	// The backend's [Backend.Answers] is that domain's own.
	//
	// It takes a context because what a copy may answer is read from the
	// log's broker; an implementation answers from a verdict it refreshes,
	// never with a broker round trip per request.
	For(ctx context.Context, domain string) (Backend, bool)
}

// RouterOptions are a router's dependencies.
type RouterOptions struct {
	// Self is this node, which asks — named on every request so a serving
	// node's log says who it answered, and never asked over the wire.
	Self string

	Queue     Asker
	Placement Placement

	// Git is who serves GIT: every live node with the `git` role, the
	// engine's from the same watched presence view as Placement, and its
	// error UNKNOWN, never "no git node". Asked by every operation on a
	// repository's git objects ([op.keyedOnGit], ADR-0059), and — as ONE
	// listing of the git nodes beside what each advertises it can give its
	// background work ([GitPlacement.Listing]) — by [Router.Maintainer],
	// which places a repository's maintenance by it. Required: a router
	// built without one would answer every git read as a fleet with no git
	// node in it, a wiring mistake that reads as a deployment fact.
	Git GitPlacement

	// LocalGit is this node's own git half, resolved PER REQUEST ([LocalGit]:
	// the code host is built at boot or at the first apply that sets
	// code.native, after the router) — nil on a node without the `git`
	// role, which serves none.
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
// scale: one attempt's budget on one data node for a read and for a write, one
// git read's and one engine write's on one git node, and the admission
// question's whole budget.
//
// [Production] is the value every node runs, and nothing in the engine builds
// another. A suite that drives the router from outside this package builds its
// own: one value rather than three fields set from inside the package, because
// a test in another package can reach no unexported field, and the jsprovision
// idiom (jsprovision.Timing) is the tree's one shape for a set of durations a
// test scales.
//
// WHAT A SHORTER TIMING BUYS is the cost of a WEDGED node — one that takes a
// request and never answers it, which its asker waits out to the end of the
// attempt — and of an admission question no data node answers. A node that is
// GONE costs no attempt at any timing: nothing serves its subject any more,
// and the broker's ask ends at once.
//
// THE SERVING NODE FITS ITS WAITS INTO THE ATTEMPT, rather than the attempt
// having to outlast them: every wait a node makes before it answers another —
// for the asker's floor, for a linearizable read's barrier and the position it
// established, for a write's own record — ends code.ReplyMargin short of the
// attempt's end ([statelog.WithReplyMargin]), so a node that runs out of time
// says so (`behind`, `no_quorum`, `pending`) while its asker is still
// listening. A shorter attempt therefore costs only that a node behind by more
// than it leaves says so sooner; it never reads an HONEST node as a wedged one,
// which is what an attempt sized below the waits used to do — the asker had
// stopped listening when a lagging but healthy node answered, asked it last for
// suspectFor and told the placement it went unanswered. Nor could an attempt be
// sized to outlast the waits instead: at their whole budgets they are two
// [statelog.ReadBudget]s and two [statelog.DefaultBarrierBudget]s (a barrier
// may first wait out one already in flight), fourteen seconds, past production's
// own read attempt and a figure no test could scale.
//
// What [NewRouter] refuses, naming the field and the arithmetic, is an attempt
// that cannot do its job at all:
//
//   - A read attempt no longer than [minReadAttempt], the reply's margin: the
//     serving node would have no time left in it to wait for anything or to
//     run a query, so every read it is asked comes back refused or late. The
//     same holds for a git read's attempt on a git node.
//   - A write attempt no longer than [minWriteAttempt]: the serving node's
//     floor wait at its whole budget, then a whole code.CommitBudget and
//     code.ReplyMargin. The attempt's end travels to a repository's committer
//     as the push's deadline, and a committer starts a batch only while a
//     budget and the margin are left before it — so under the bound a push
//     whose committer first waited out its floor is answered busy, undecided,
//     and under the committer's two terms alone EVERY push asked of another
//     node's committer is. A suite that needs a wedged committer to cost less
//     has to shorten the committer's budget with it, which is the committer's
//     to expose rather than the router's to assume.
//   - A git write attempt no longer than [minGitWriteAttempt]: the proposal
//     the write makes needs a whole [minWriteAttempt] left after its upload,
//     and the git node is served until a reply's margin before the attempt
//     ends, so under the bound every engine write is answered busy before
//     its upload — "a git write's attempt must hold the proposal it makes".
//
// The admission budget bounds a floorless question ([opPing]), so it has only
// to be a positive duration.
//
// A FIELD LEFT ZERO IS REFUSED BY NAME as well, never read as "the default",
// because each would be read as something nobody chose: a zero attempt is no
// bound of the router's own ([attemptContext] takes it as the caller's
// deadline, unchanged), so a request from a caller with none would wait on a
// wedged data node for ever rather than move on to the next; and a zero
// admission budget is a deadline already past, so every placement sweep would
// withhold every seat's claim — a wiring mistake that reads as a fleet with no
// data node in it. A negative field is a deadline already past for any of
// them.
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
	// GitReadAttempt bounds one git read's wait on one git node — see
	// [GitReadAttempt]. Longer than [minReadAttempt].
	GitReadAttempt time.Duration
	// GitWriteAttempt bounds one engine write's wait on one git node, its
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

// minReadAttempt is what every read attempt must be longer than: the reply's
// margin, code.ReplyMargin — the answer's trip back, which a serving node
// leaves between the end of every wait it makes and its asker's deadline
// ([statelog.WithReplyMargin]). An attempt no longer than that leaves the node
// nothing to wait or run a query in; what an attempt holds beyond it is what
// the serving node's waits and its query share. See [Timing].
const minReadAttempt = code.ReplyMargin

// minWriteAttempt is what every write attempt must be longer than: the serving
// node's floor wait at its whole budget ([statelog.ReadBudget]), then a whole
// code.CommitBudget and code.ReplyMargin, which a repository's committer must
// still find before a push's deadline to start the push's batch at all.
//
// It is what a PUSH needs, and no bound on how long any write's serving node
// takes: a walking gesture — a merge, a move — appends once per object it
// walks and waits between them, which is what production's write attempt is
// sized for (writeAttempt), and every one of those waits ends inside whatever
// attempt the write is given, a reply's margin short of it. See [Timing].
//
// IT IS code.MinProposeAttempt, the domain's one sum of those three terms,
// because the code host holds an engine write to it too — refusing to upload
// a write whose caller left less than it for the proposal after the upload —
// and two sums would let the two disagree on what a proposal needs.
const minWriteAttempt = code.MinProposeAttempt

// minGitWriteAttempt is what every git write attempt must be longer than: the
// proposal the write makes ([minWriteAttempt]) and the reply's margin. The git
// node is served until a reply's margin before the attempt ends
// (code.AnswerBy), and it uploads a write's bundle only while the proposal's
// whole attempt is still left after the upload (internal/code/host's deadline
// rule) — so an attempt no longer than the two together leaves no write any
// time to sync, build or upload in, and answers every one busy before its
// upload. See [Timing].
const minGitWriteAttempt = minWriteAttempt + code.ReplyMargin

// refusal is why a router may not be built with t, naming the first field it
// refuses, or nil.
func (t Timing) refusal() error {
	const production = "Production() is the value a node runs"
	switch {
	case t.ReadAttempt <= minReadAttempt:
		return fmt.Errorf("estate: a router's Timing.ReadAttempt is %v, and it must be longer "+
			"than %v — the answer's trip back (code.ReplyMargin), which a serving node leaves "+
			"between the end of its waits and its asker's deadline, so an attempt no longer "+
			"leaves it no time to wait for anything or to run the read; %s", t.ReadAttempt,
			minReadAttempt, production)
	case t.WriteAttempt <= minWriteAttempt:
		return fmt.Errorf("estate: a router's Timing.WriteAttempt is %v, and it must be longer "+
			"than %v — the serving node's wait for the asker's floor (statelog.ReadBudget, %v), "+
			"then a batch (code.CommitBudget, %v) and its answer (code.ReplyMargin, %v), which a "+
			"repository's committer must still have before a push's deadline or it answers the "+
			"push busy, undecided; %s", t.WriteAttempt, minWriteAttempt, statelog.ReadBudget,
			code.CommitBudget, code.ReplyMargin, production)
	case t.AdmissionAsk <= 0:
		return fmt.Errorf("estate: a router's Timing.AdmissionAsk is %v, and it must be a "+
			"positive duration — a zero or negative budget is a deadline already past, which "+
			"withholds every seat's claim; %s", t.AdmissionAsk, production)
	case t.GitReadAttempt <= minReadAttempt:
		return fmt.Errorf("estate: a router's Timing.GitReadAttempt is %v, and it must be longer "+
			"than %v — the answer's trip back (code.ReplyMargin), which a git node leaves between "+
			"the end of its waits and its asker's deadline, so an attempt no longer leaves it no "+
			"time to sync its copy or run the read; %s", t.GitReadAttempt, minReadAttempt, production)
	case t.GitWriteAttempt <= minGitWriteAttempt:
		return fmt.Errorf("estate: a router's Timing.GitWriteAttempt is %v, and it must be longer "+
			"than %v — a git write's attempt must hold the proposal it makes: the git node uploads a "+
			"write only while the proposal's own attempt (code.MinProposeAttempt, %v) is left after "+
			"the upload, and answers its asker the reply's trip (code.ReplyMargin, %v) before the "+
			"attempt ends, so an attempt no longer answers every write busy before its upload; %s",
			t.GitWriteAttempt, minGitWriteAttempt, minWriteAttempt, code.ReplyMargin, production)
	}
	return nil
}

// Router routes every estate operation to a data node: this node, in-process,
// where it holds the estate and its copy serves; otherwise the other data
// nodes over the broker, one subject per node.
//
// # One per node, on every node
//
// A node without the `data` role holds nothing and asks for everything; a data
// node answers its own seats from its own copy, and asks a peer only while its
// copy of the operation's domain is out of service (ADR-0049) or cannot take
// the request. Both are the same
// router, so a tool behaves identically on either, and the path a data node
// takes to its own copy is the path it takes to a peer's, floors included.
//
// # The order data nodes are asked in
//
// This node first, where its copy serves. Then the node that answered the
// operation's domain last (sticky, per domain: its applier of that domain is
// the one most likely to have this node's writes), then [rendezvous.Order]
// keyed on this node's own id — with every node that recently answered that
// its copy of the domain is out of service after the rest, and every node
// that recently went silent last ([Router.order]). The rendezvous order picks
// an asker's FIRST data node and every node it fails over to, spread evenly
// across the data nodes —
// so the askers of a node that goes silent scatter over the survivors rather
// than fall on one neighbour — but not where an asker STAYS: it keeps asking
// a domain's sticky node until that node goes silent or says its copy of the
// domain is out of service. So a data node that joins is
// asked only by askers with no sticky node yet or whose sticky node goes
// silent, and after a rolling restart each restarted node's askers stay where
// they failed over to. A node that ran nothing is passed over by every class
// of operation; what may be repeated after a node MAY have run it is the
// class's ([opClass]).
//
// A KEYED operation — a push — keeps the demotions and nothing else of this
// order: it is asked in [rendezvous.Order] of its KEY over every data node,
// this one at its place, with no sticky node ([Router.keyedOrder]) — and with
// one demotion of its own, since no sticky node moves it off a node that
// refused: a node that ran nothing for it is asked last for that operation
// ([Router.markDeclined]).
//
// A node that said inside [suspectFor] that it is BEHIND THE COMPANY'S
// CONFIGURATION is not asked in either order at all: it is asked only as the
// walk's last resort, after every copy that only lags, told at once to answer
// anyway ([Router.markBehindConfig]), for every operation but admission's
// ungated ping.
//
// A GIT operation ([op.keyedOnGit]) is asked of the GIT nodes instead, in the
// keyed order over [RouterOptions.Git] — the repository's home first, this
// node at its place where its git half is built — with the keyed order's
// demotions and none of the data walk's preferences ([Router.routeGit]).
type Router struct {
	self      string
	queue     Asker
	placement Placement
	local     LocalBackends
	session   *Session
	seams     ServerSeams
	env       *Envelopes
	now       func() time.Time

	// gitPlacement is who serves git, and in one listing what each git
	// node advertises ([RouterOptions.Git]), and localGit this node's own
	// git half, nil on a node without the git role
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
	// behindConfig is when each node that answered `behind_config` may be
	// asked an operation the preference holds back ahead of the last resort
	// again ([Router.markBehindConfig]). Per NODE, because the answer is:
	// whether a node serves the company's configuration is the node's
	// posture, the same for every copy it holds and every operation.
	behindConfig map[string]time.Time
	// admitted caches a REMOTE node's answer to the admission question —
	// see [Router.Serves] — nil until one has been asked.
	admitted *admission

	// codeWrites is how far this node's own writes reached in each
	// repository ([CodeSeam.FloorOf]), under a lock of its own: the code
	// host's writes and reads touch nothing else of the router's state.
	codeWrites repoFloors
}

// nodeCopy is one node's copies of what an operation reads ([opSpec.copies]).
type nodeCopy struct{ node, copies string }

// nodeOp is one node's serving of one operation, by its name.
type nodeOp struct{ node, op string }

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
	case opts.Git == nil:
		return nil, errors.New("estate: a router needs the git placement it asks a repository's git " +
			"reads by — every live node with the git role; one with none to name answers an empty " +
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
		behindConfig: map[string]time.Time{},
	}, nil
}

// ReadAttempt bounds one read's wait on one node: [Production]'s
// [Timing.ReadAttempt].
//
// TEN SECONDS: every wait a serving node makes for a linearizable read at its
// whole budget — the asker's floor ([statelog.ReadBudget]), the read's barrier
// ([statelog.DefaultBarrierBudget]) and the barrier's position (ReadBudget
// again) — and the reply's margin on top (code.ReplyMargin): 2 + 5 + 2 + 1.
// So production cuts a healthy node's waits short only where its barrier
// first waits out one already in flight, and the node then says so in time
// ([statelog.WithReplyMargin]); a node that has said nothing at all in that
// long is one that is gone rather than slow, and the next one answers sooner
// than it would. TestProductionsReadAttemptHoldsALinearizableReadsWaits holds
// the sum. It is a ceiling under the caller's own deadline, never over it, and
// above [minReadAttempt], which [NewRouter] holds every timing to.
//
// THE CODE HOST'S LOCAL READS ARE BOUNDED BY THE SAME FIGURE — gitproc's
// ReadDeadline and SlotWait, one read attempt each — and since gitproc imports
// no engine package, TestGitprocsReadBoundsAreOneReadAttempt holds the
// equality from here.
const ReadAttempt = 10 * time.Second

// writeAttempt bounds one write's wait on one node, for a caller with no
// deadline of its own: [Production]'s [Timing.WriteAttempt], and read by
// nothing else, so a router waits only the timing it was built with.
//
// SIXTY SECONDS: a single append resolves within [statelog.DefaultResolveBudget]
// (five), and the longest gesture a seat's tools make — a merge or a move —
// appends once per object it walks and waits between them. A caller with a
// deadline is held to that instead.
//
// The attempt's end travels with the request ([request.Deadline]), and a
// repository's committer answers a push inside it: it starts a batch only
// while a code.CommitBudget and code.ReplyMargin are left before it, so the
// attempt, less the serving node's floor wait, has to hold more than both —
// [minWriteAttempt], which [NewRouter] holds every timing to — and what is left
// over, twenty-seven seconds, is how long a push may queue behind its
// repository's earlier ones.
const writeAttempt = 60 * time.Second

// suspectFor is how long a node that went unanswered is asked last.
//
// THIRTY SECONDS, which is the order of a presence lease's life: a node that
// died leaves the placement when its lease lapses, and until then every
// request that asked it first would wait out a whole attempt. Asked last
// instead, it costs one attempt per router rather than one per request.
//
// AND IT IS HOW LONG A NODE THAT ANSWERED `out_of_service` FOR A DOMAIN IS
// ASKED LAST FOR THAT DOMAIN ([Router.markOutOfService]). A copy goes out of
// service for minutes at the least — until an eviction is lifted, a snapshot
// is adopted, an upgrade decodes what it held — so asked first, it cost every
// operation on the domain a round trip that ran nothing; asked last, a copy
// that recovers inside the window is asked last meanwhile, never not asked.
// One window rather than two, because the remedy is the same — a node not
// worth asking first — and a second number would be a second opinion about
// how long that lasts.
//
// AND HOW LONG A NODE THAT RAN NOTHING FOR A KEYED OPERATION IS ASKED LAST FOR
// IT ([Router.markDeclined]), for the same reason again: what makes a
// committer refuse — a maintenance window, a stop, an eviction, a log it is
// behind — lasts minutes, not one request.
//
// AND HOW LONG A NODE BEHIND THE COMPANY'S CONFIGURATION IS ASKED ONLY AS THE
// LAST RESORT ([Router.markBehindConfig]). That lasts longer still — the
// control plane moves a node back only at its next posture decision after an
// apply succeeds, and a stuck node stays until a person acts — so the window
// is how long a router goes on asking a node that has since become current
// last, and once a window, the refusal's round trip a router pays to learn
// that the node is still behind.
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
// every data node it asks, together — where [ReadAttempt] bounds only each one:
// [Production]'s [Timing.AdmissionAsk], and read by nothing else.
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

	// key is the arguments' routing key for a KEYED operation
	// ([op.keyedBy], [op.keyedOnGit]), and empty for every other: the walk
	// asks the nodes in its rendezvous order rather than this node first.
	key string

	// home is the same operation with its arguments stamped as asked of the
	// walk's effective home ([op.keyedOnGit]) — the one attempt of a git walk
	// that may fill a copy — while this exchange's own are stamped not: nil
	// for every operation but a git one.
	home *exchange
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
	var key string
	if o.key != nil {
		if key = o.key(args); key == "" {
			return zero, fmt.Errorf("estate: %s is asked of the node its key prefers, and "+
				"its arguments name no key", spec.name)
		}
	}
	var (
		x   exchange
		err error
	)
	if o.home == nil {
		if x, err = exchangeOf(spec, o, actor, args, written, key); err != nil {
			return zero, err
		}
	} else {
		// A GIT OPERATION IS STAMPED ON EVERY ATTEMPT: home for the walk's
		// effective home, not for every node after it — whatever the
		// caller set, since only the walk knows which node it asks first.
		home, homeErr := exchangeOf(spec, o, actor, o.home(args, true), written, key)
		if homeErr != nil {
			return zero, homeErr
		}
		if x, err = exchangeOf(spec, o, actor, o.home(args, false), written, key); err != nil {
			return zero, err
		}
		x.home = &home
	}
	answer, err := r.route(ctx, spec, actor, x)
	if out, ok := answer.(R); ok {
		return out, err
	}
	return zero, err
}

// exchangeOf is one operation's two halves over args, in its caller's types:
// run in-process on a backend, or encoded once to be asked of another node and
// its answer decoded.
func exchangeOf[A, R any](spec *opSpec, o op[A, R], actor *Actor, args A, written tracker.WriteLog,
	key string) (exchange, error) {

	encoded, err := json.Marshal(args)
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
			decodeErr := json.Unmarshal(raw, &out)
			return out, decodeErr
		},
		written: written,
	}, nil
}

// ErrUnserved is an operation no data node ran: every node the asker's view
// named — this node among them where it holds the estate — answered that its
// copy of the operation's domain is out of service, could not run it, was
// behind, or did not answer at all. For a GIT operation ([op.keyedOnGit]) it
// is the same of the git nodes: none ran it.
//
// NEVER A FALSE "NOT FOUND". An operation nobody answered for is refused
// saying so, because a read reported as empty would say the company has none
// of what it asked about.
type ErrUnserved struct {
	// Detail is who was asked and what each said, in order.
	Detail string

	// NodeSilent records that a node the walk asked went SILENT, or gave a
	// reply that did not verify ([Router.unheard]) — read through
	// [ErrUnserved.Silent]. Exported so it crosses the wire with the rest of
	// the refusal: an estate nobody served on the far side of a second hop
	// says the same of its own walk.
	NodeSilent bool

	// Git records that the walk asked the GIT nodes ([Router.routeGit]), not
	// the data nodes, so the refusal names the nodes it asked: worded as
	// every data walk's, a git read the git nodes could not answer — its
	// home filling a copy, the rest holding none — sent whoever read it to
	// the data nodes, which were serving.
	Git bool
}

// Silent reports whether a node the walk asked went silent rather than
// answering that it would not run the operation — a reply this node could not
// believe being no such answer.
//
// A walk in which nobody went silent is one in which NOTHING RAN the
// operation — every node answered that its copy was out of service, could not
// run it, or was behind — and a caller with a write may say so definitively:
// the code host tells a person "no data node took the push", which a re-run
// then lands. One in which a node went silent may not: a write whose node
// never answered may have run there — a committer that appended and lost its
// reply — and told "nothing took it", the person's re-run would report the
// push already up to date, a false negative about a push that landed.
func (e *ErrUnserved) Silent() bool { return e.NodeSilent }

func (e *ErrUnserved) Error() string {
	what := "estate: no data node serves the estate right now"
	if e.Git {
		what = "estate: no git node serves the repository's git work right now"
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
	// configuration — it answered so ([unservedBehindConfig]), or the router
	// remembers it did and did not ask it ([Router.remembersBehind]) — each
	// in the order the walk reached them, this node among them by its own id
	// where its own copy answered so.
	//
	// TWO LISTS, because the last resort asks the first before the second
	// ([Router.lastResort]): a copy that only lags answers under the current
	// configuration, a node behind it under the one the fleet replaced. One
	// list in the order the walk reached the refusals made that preference a
	// matter of WHEN the router learned of them — the request that learned
	// a node was behind asked it first wherever rendezvous ranked it first,
	// and every request the router remembered it for asked it last — so a
	// repository's pushes went to two committers in every window. Within each
	// list the walk's order stands: this node first for an operation any copy
	// answers equally, and the key's order for a keyed one, whose committer
	// the last resort must not jump among nodes of the same standing.
	lagging, behindConfig []string

	// spent is what the request has already paid for on the nodes it asked
	// — the read index one node's barrier established, every position one
	// node waited its whole bound for, every log a barrier ran out on — and
	// every node asked after them is handed it ([statelog.Spent]), so a walk
	// of N nodes appends at most one barrier and waits at most one bound
	// for a position rather than N of each.
	spent statelog.Spent
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

// route runs one operation — this node first where its copy serves, then the
// other data nodes in order, and LAST the nodes that answered that they are a
// worse one to ask: a copy that lags its logs, or a node behind the company's
// configuration ([worse]).
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
//
// A node confirmed behind the company's configuration is asked last by the
// same walk, and for the same reason: what it answers is sound — it supplies
// the configuration the fleet replaced, a value every reader meets anyway
// when a revision changes it — only worse than a current node's answer; and
// refused, a fleet whose only data node failed an apply would answer nothing.
// Unlike a lag, which the router forgets, its answer is REMEMBERED for
// [suspectFor], because it lasts: until the window ends the walk does not ask
// the node at all, and comes to it only as the last resort, told at once to
// answer anyway ([Router.markBehindConfig]) — one ask where every better node
// runs nothing, and none where one runs it. And it is the WORSE of the two:
// the last resort asks every copy that only lags first, whose answer is held
// to the current configuration, and a node behind the configuration after
// them ([walk.lagging]) — on the request that learns it as on every one
// after.
//
// # A keyed operation asks in its key's order
//
// An operation declared [op.keyedBy] asks this node at its PLACE in the
// key's order ([Router.keyedOrder]) rather than first, and never ahead of the
// node the key prefers; every other step of the walk — the floors, the
// verdict of the operation's own domain, the class rule for a node that may
// have run it, the lagging last resort — is the one above.
func (r *Router) route(ctx context.Context, spec *opSpec, actor *Actor, x exchange) (any, error) {
	if spec.git {
		return r.routeGit(ctx, spec, actor, x)
	}
	w := &walk{}
	// THIS NODE FIRST, where its copy serves — in-process, with the node's
	// floors enforced exactly as a remote node enforces them — for an
	// operation any copy answers equally. A keyed one asks it at its place
	// in the key's order, below.
	if x.key == "" {
		if value, final, ranErr := r.tryLocal(ctx, spec, x, false, w); final {
			return value, ranErr
		}
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
		// route. A KEYED operation has not asked it yet: with no node
		// named to order, its own copy is the one node left to ask.
		if x.key != "" {
			if value, final, ranErr := r.tryLocal(ctx, spec, x, false, w); final {
				return value, ranErr
			}
		}
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
	for _, node := range r.orderFor(spec, x.key, nodes) {
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
			return nil, err
		}
		if !got.answered() {
			if err := r.unheard(ctx, spec, node, got, w); err != nil {
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
	// WHO WAS ASKED AND WHAT EACH SAID GOES TO THE LOG HERE, the one place
	// that knows it: every reader of the refusal tells its caller in fixed
	// words (internal/agent/builtin's Condition), because the detail names
	// nodes and their copies' faults, and a model's prompt or a person's
	// screen can act on neither.
	detail := strings.Join(w.reasons, "; ")
	log.WarnContext(ctx, "estate_unserved", "op", spec.name, "detail", detail)
	return nil, &ErrUnserved{Detail: detail, NodeSilent: w.silent}
}

// lastResort asks the nodes the walk passed over as worse ones to ask, telling
// each to take the request anyway — this node's own in-process: every copy
// that only lags its logs FIRST, then every node behind the company's
// configuration, each in the order the walk reached it ([walk.lagging]). The
// order is the REASONS', so it is the same on every request whatever the
// router has heard: a copy that lags answers held to the caller's floors and
// under the current configuration, a node behind it under the configuration
// the fleet replaced. final is set when one of them gives the answer.
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
			return nil, true, askErr
		}
		if !got.answered() {
			if unheardErr := r.unheard(ctx, spec, node, got, w); unheardErr != nil {
				return nil, true, unheardErr
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
	switch {
	case why != "":
		// IN-PROCESS, so nothing is remembered: asking this node's own
		// copy again costs no round trip. It still waits its turn by its
		// REASON — a copy of its own behind the configuration is asked after
		// a peer's that only lags, because in-process saves a round trip and
		// never makes a decide under the replaced configuration sound.
		switch refusal {
		case unservedLagging:
			w.lagging = append(w.lagging, r.self)
		case unservedBehindConfig:
			w.behindConfig = append(w.behindConfig, r.self)
		case unservedBehind:
			w.waitedOut(r.floorsFor(spec))
		}
		w.note("%s: %s", r.self, why)
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

// refusedHere is a READ a copy refused for a reason about ITSELF — behind the
// barrier its linearizable read appended, stalled, holding a record it cannot
// decode, below the floor, evicted, cut off from its broker or from a quorum —
// which another data node may serve ([statelog.ReadRefusal.Elsewhere]). The
// node ran nothing, so the walk asks the next one and keeps the refusal as its
// answer only where no node does better. Returned as the answer instead, a
// git fetch through a data node whose applier fell a second behind was told
// 503 while two peers' copies could have served it, and so was every seat's
// linearizable read on that node.
//
// WHAT THE REFUSAL HAD PAID FOR GOES WITH THE READ ([statelog.Refused.Spent],
// [walk.spent]): the next node is asked at the read index the refusing one
// established and waits for no position it already waited out. Asked afresh,
// each node appended a barrier of its own and waited a whole read budget for
// it, so on a fleet one burst put behind together the one final `behind`
// cost a barrier and two seconds per data node — barriers every node's
// applier then applied, on the very log they were all behind on.
//
// A READ ONLY: a write is refused by the write authority's own answers
// (statelog.Unavailable), whose class rules say what may be repeated, and a
// read is safe to ask anywhere.
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
		Op: spec.name, Args: x.encoded, Actor: actor,
		Floors: floors, From: r.self, AcceptLagging: acceptLagging,
	}
	if !spent.IsZero() {
		req.Spent = &spent
	}
	rep, got, err := r.ask(ctx, node, spec.lane.subject(node), req, budget, r.wholeAttempt(spec))
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

	// unbelieved is why what came back is not believed, empty for a reply
	// that verified and answers this request ([Envelopes.openReply]).
	unbelieved string
}

// answered reports a reply this node believes.
func (h heard) answered() bool { return h.replied && h.unbelieved == "" }

// unheard is what a node that gave no reply this node believes costs the walk:
// nothing more for a class that may be repeated, the request itself for a
// once-write — which that node may have run, whatever it said or did not say
// — and the caller's own ending.
//
// A REPLY THAT IS NOT BELIEVED IS NO ANSWER, never an answer of "did not run":
// a refusal the fleet signed for another request is exactly what somebody
// would replay to make a once-write run twice. But the node that sent it is
// not SILENT — it is up, and its presence lease says so — so it is not
// suspected, which would only move it down the order for something its key
// configuration, not its health, explains. What the walk records of it is
// what it records of a silent node ([walk.silent]): the node may have run the
// operation, so a walk that ends unserved does not say that nothing ran it.
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
		return fmt.Errorf("estate: %s: %w", spec.name, ctx.Err())
	}
	w.silent = true
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
func (r *Router) settle(spec *opSpec, x exchange, node string, rep reply, w *walk) (
	value any, final bool, err error) {

	if rep.Unserved != "" {
		switch rep.Unserved {
		case unservedLagging:
			w.lagging = append(w.lagging, node)
		case unservedBehindConfig:
			// ASKED LAST, after every copy that only lags — and
			// remembered, because unlike a lag it lasts: forgotten, every
			// later request, keyed or not, sent the node a refusal's round
			// trip first — a push its whole proposal — for as long as it
			// stayed behind.
			w.behindConfig = append(w.behindConfig, node)
			r.markBehindConfig(node)
		case unservedBehind:
			w.waitedOut(r.floorsFor(spec))
		case unservedOutOfService:
			r.markOutOfService(node, spec.copies)
		case unservedNoBackend:
			// A KEYED ORDER HAS NO STICKY NODE to move the next request
			// off a node that ran nothing, so it is remembered: without
			// it, every later push to each repository the node ranks first
			// for sent it the whole proposal again — encoded, decoded and
			// refused — before the runner-up was asked, for as long as
			// the node refused (a maintenance window, a stop). An
			// unkeyed operation moves off it by its sticky node instead.
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
			w.fallback = &held{err: failure}
			w.paid(failure)
			w.note("%s: %v", node, failure)
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

// budgetFor is one attempt's budget on one node: the router's own attempt
// ([Router.wholeAttempt]) — or, for a write on a DATA node whose caller has a
// deadline, that deadline (zero), since a gesture the caller gave five
// minutes must not be abandoned after one.
//
// NEVER FOR A GIT WRITE, which keeps the router's own attempt whatever its
// caller allows ([GitWriteAttempt]): its git node fits every stage of it —
// the sync, the build, the upload and the proposal — into that attempt, so
// one silent through it is wedged rather than slow, and the walk's next git
// node builds the same push. Given a turn's deadline minutes away, a wedged
// home was waited on until the turn's tool call gave up, and its runner-up
// never asked.
func (r *Router) budgetFor(ctx context.Context, spec *opSpec) time.Duration {
	if _, bounded := ctx.Deadline(); bounded && spec.class != opRead && !spec.git {
		return 0
	}
	return r.wholeAttempt(spec)
}

// wholeAttempt is the router's own attempt on one node for spec's class:
// [Timing.ReadAttempt] for a read, [Timing.GitReadAttempt] for a git read on a
// git node, [Timing.GitWriteAttempt] for an engine write on one and
// [Timing.WriteAttempt] for a write on a data node — what a request waits
// where nothing shorter bounds it, and what a node's silence must last to be
// evidence it is gone rather than slow ([Router.ask]).
func (r *Router) wholeAttempt(spec *opSpec) time.Duration {
	switch {
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
// the subject of the lane its operation is answered on ([opSpec.lane]),
// waiting budget ([Router.budgetFor]). heard says whether anything came back
// and whether it is believed: one whose reply does not verify, or answers
// another request, is heard and not believed. err is set only where the
// request could not be made at all, which no other node would fare better
// with, or where the fleet signed a reply this build cannot decode.
//
// A NODE THAT SAID NOTHING IS SUSPECTED — asked last for [suspectFor] and
// reported to the placement — only where its silence is evidence it is gone
// rather than slow, which is a matter of whose wait ended: the ask ended with
// the attempt still live (nothing serves the node's subject), a budget of the
// router's own ran out on it ([errAttemptSpent]), or it stayed silent for a
// whole attempt of its class (whole) under a caller's longer deadline. Through
// either wait a node that is only behind has answered: it ends every wait it
// makes a reply's margin short of the deadline it was handed
// ([statelog.WithReplyMargin]) and says it is behind. A caller
// that hung up, or gave the request less than an attempt, has said nothing
// about the node it was asking; suspected for it, a healthy node was asked
// last by every request this router made for half a minute — and for a keyed
// operation that sends every repository it commits to the runner-up, whose
// committer then contends with it on the repository's subject — and dropped
// from the placement's answer. A node whose reply is not believed is not
// suspected either: it is up, and its key configuration rather than its
// health explains the reply ([Router.unheard]).
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
		return reply{}, heard{}, fmt.Errorf("%s answered something this build cannot decode: %w",
			node, err)
	}
	return rep, heard{replied: true, unbelieved: why}, nil
}

// order is the order the data nodes are asked in for spec, an operation any
// copy answers equally, reading [opSpec.copies] — its own domain's, and any
// other's it also reads: the STICKY node for those copies first, then the
// rest, then every node that answered `out_of_service` for them inside
// [suspectFor], and every SUSPECT node last — [rendezvous.Order] on this
// node's own id within each class, kept by a STABLE sort, so the classes only
// move nodes between them — and never this node, which the router asks
// in-process or not at all. A node the router remembers behind the company's
// configuration ([Router.markBehindConfig]) keeps its place, from which the
// walk hands it to the last resort unasked ([Router.classes]).
//
// # Per domain, because the appliers are
//
// A node is sticky for the reason its applier is the one most likely to have
// this node's writes, and each domain has its own applier — and since a fault
// takes only its own domain's copy out of service (ADR-0049), a node can serve
// one domain and not another. One sticky node for every domain made a router
// on a node without data, between a peer whose tracker copy was out and one
// whose knowledge base was, CHASE ITS TAIL: a tracker call asked the first,
// was answered by the second and made it sticky; the next page call asked the
// second, was answered by the first and made it sticky back — every call paid
// a round trip that ran nothing, and a third node serving both was never asked
// first at all.
func (r *Router) order(spec *opSpec, nodes []string) []string {
	// rendezvous.Order hands back a slice of its own, so the placement's
	// listing is never the one trimmed or sorted here; and removing members
	// from its answer leaves the rest in rendezvous order.
	nodes = slices.DeleteFunc(rendezvous.Order(r.self, nodes),
		func(n string) bool { return n == r.self || n == "" })
	class := r.classes(spec, false)
	slices.SortStableFunc(nodes, func(a, b string) int { return class(a) - class(b) })
	return nodes
}

// orderFor is the order the data nodes are asked in for one request:
// [Router.order] for an operation any copy answers equally — this node
// excluded, since it was asked first — and [Router.keyedOrder] for a keyed
// one, this node at its place where it holds the estate.
func (r *Router) orderFor(spec *opSpec, key string, nodes []string) []string {
	if key == "" {
		return r.order(spec, nodes)
	}
	return r.keyedOrder(spec, key, nodes, r.local != nil)
}

// keyedOrder is the order a KEYED operation, spec, asks nodes in:
// [rendezvous.Order] on the KEY over nodes — THIS ONE INCLUDED where self says
// it answers the operation (it holds the estate, or for a git operation its
// git half is built), since the node the key prefers may be this one, and an
// empty id never — with every node that answered `out_of_service` for the
// operation's copies or ran nothing for it inside [suspectFor] after the rest
// and every node that went silent inside it last, the key's order kept within
// each by a STABLE sort. A node the router remembers behind the company's
// configuration keeps its place in the key's order, from which the walk hands
// it to the last resort unasked, so every asker's last resort meets the nodes
// behind the configuration in the key's order ([Router.classes]). ONE order
// for a push's data nodes and a repository's git nodes, so the compactor's
// home and the reads' first node can never be computed two ways.
//
// NO STICKY NODE: which node answered last says nothing about where a
// repository's pushes should meet, and every node computing the same order
// from the same set is the whole point — a committer holding a repository's
// whole burst commits it in a few records, where two committers each holding
// part of it contend on its one subject and each round one loses costs it a
// re-decide and a re-append of its whole batch.
func (r *Router) keyedOrder(spec *opSpec, key string, nodes []string, self bool) []string {
	ordered := r.keyed(key, nodes, self)
	class := r.classes(spec, true)
	slices.SortStableFunc(ordered, func(a, b string) int { return class(a) - class(b) })
	return ordered
}

// keyed is the rendezvous half of every keyed order, before any memory of the
// router's demotes a node: [rendezvous.Order] on key over nodes — this one
// included where self says it answers, an empty id never, a node listed twice
// named once. ONE FUNCTION for [Router.keyedOrder], [Router.Home] and
// [Router.Maintainer], so the node a repository's git reads ask first on a
// quiet fleet, the node its own git work falls to and the node its
// maintenance is placed on where every node can hold the work are one
// computation and can never be two.
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

// classes is how the router's memory ranks each node for spec, lowest asked
// first: the STICKY node for its copies (never for a keyed order), then every
// other node, then every node that answered `out_of_service` for its copies —
// or, for a keyed operation, ran nothing for it — inside [suspectFor], then
// every SUSPECT node. It sweeps out every window that has lapsed. ONE reading
// of the memory for both orders, so a node demoted for the unkeyed walk is
// demoted alike for the keyed one.
//
// A node remembered behind the company's configuration HAS NO CLASS of its
// own: the walk never asks it from its place — it hands it straight to the
// last resort ([Router.remembersBehind]) — so its place decides only where it
// stands among the other nodes behind the configuration there, and that must
// be the place a router that has heard of none of them gives it. Demoted, a
// router that remembered one node behind and had not yet heard another asked
// the unheard one first; a router that had heard neither asked them in the
// key's order, and the two sent one repository's pushes to two committers.
// The memory is swept here with the rest.
func (r *Router) classes(spec *opSpec, keyed bool) func(node string) int {
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
	// OUTSIDE THE LOCK: the placements are somebody else's, and a request
	// path must not hold this router's mutex across them. BOTH OF THEM,
	// because a suspicion is the node's and never one placement's — a node
	// silent to a git read is as likely gone from the data nodes it may be
	// one of — and a placement that names it lists again only when told: a
	// git placement never told kept a dead git node in every repository's
	// order, asked last of each walk nobody else answered, until its own
	// view caught up.
	r.placement.Unanswered(node)
	r.gitPlacement.Unanswered(node)
}

// markOutOfService asks node last for [suspectFor] for the operations that read
// copies: one of its copies of them is out of service (ADR-0049), and every
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

// markBehindConfig remembers for [suspectFor] that node answered
// `behind_config`: until the window ends, every operation the preference holds
// back — every one but an ungated one — asks it only as the walk's last
// resort, after every copy that only lags, told at once to answer anyway
// ([Router.remembersBehind]), and a keyed one asks its runner-up instead.
//
// THE NODE, not a copy or an operation: whether it serves the company's
// configuration is its posture, the same for everything it holds, so one
// refusal tells the router about every domain and every repository it
// commits for.
//
// NOTHING BUT THE WINDOW ENDS IT. The one answer that could — the node running
// a held-back operation from an ordinary ask — is one the router does not
// send it while it remembers; the last resort's answer was given because it
// was told to answer anyway, and the ungated ping's skips the preference, so
// neither says the node is current again. A node current again is asked last
// until the window ends; one still behind costs a router one refusal a window.
// Its sticky places stay: while it is remembered the walk passes it to the
// last resort wherever it ranks, and once the window ends the node asked first
// is the one that learns whether it is still behind.
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
// data, or one whose copy of a domain that gates seats is out of service
// ([SeatCopy]) — it is the first data node that answers, and that answer is
// trusted for [admissionTrust]. A wrong copy of any other domain is not the
// seat's copy, and admission stays local over it (ADR-0049).
func (r *Router) Serves(ctx context.Context) (tracker, pages bool, err error) {
	if r.local != nil {
		if b, ok := r.local.For(ctx, SeatCopy); ok {
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
	asked, cancel := context.WithTimeoutCause(ctx, r.timing.AdmissionAsk, errAttemptSpent)
	defer cancel()
	out, err := call(asked, r, opPing, nil, struct{}{})
	r.mu.Lock()
	r.admitted = &admission{at: r.now(), tracker: out.Tracker, pages: out.Pages, err: err}
	r.mu.Unlock()
	return out.Tracker, out.Pages, err
}
