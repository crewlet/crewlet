package estate

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// SubjectPrefix prefixes every serving node's subject.
const SubjectPrefix = "crewlet.estate"

// Subject is the subject a serving node answers every operation on but a
// push's proposal, which it answers on [ProposalSubject].
//
// The node id is ESCAPED as one subject token: `node.id` admits a dot, and a
// raw dot would split it into a subject another node's escaped id could match.
func Subject(nodeID string) string {
	return SubjectPrefix + "." + coord.DocumentKey(nodeID)
}

// ProposalSubject is the subject a serving node answers proposals on, a push's
// and a compaction's: its [Subject] plus one token, so no node's [Subject] is
// another's, an escaped id being exactly one token.
func ProposalSubject(nodeID string) string { return laneProposals.subject(nodeID) }

// lane is one of a serving node's registrations ([Serve]): the subject it
// answers on, and how many of its requests it answers at once.
//
// SEPARATE, because a request past a registration's slots waits behind every
// answer holding one ([queue.EventQueue.Serve]), so an answer that WAITS must
// never hold the slots answers done now need: on one registration, a burst of
// queued pushes held every slot, starved reads past [ReadAttempt] and got the
// node suspected fleet-wide. So proposals (a push's, a compaction's) have a
// lane as wide as the committer and its two sibling writers hold at once
// ([proposalAnswers]), and a Gitlet node's git reads ([GitReadAttempt]) and
// engine writes ([GitWriteAttempt]) a third and a fourth ([gitLanes]). A lane
// answers only the operations declared on it ([op.answeredOn]).
type lane string

const (
	// laneShared answers every operation but a proposal and a git read, on
	// [Subject].
	laneShared lane = ""

	// laneProposals answers proposals alone, on [ProposalSubject]: a push's, a
	// compaction's, an expunge record's, a change's and a merge queue's, and the
	// writes of a change's conversation and a repository's checks, each waiting
	// its change's or repository's turn on the same node ([proposalAnswers]).
	laneProposals lane = "proposals"

	// laneGitReads answers a repository's git reads alone ([GitCopy]) — on
	// the node's subject with one more token, as the proposals' lane is —
	// and only on a node that serves git ([ServerOptions.Git]).
	laneGitReads lane = "git"

	// laneGitWrites answers a repository's ENGINE WRITES alone
	// ([opCodeGitWrite]), on the node's subject plus one token, only on a node
	// that serves git. Apart from the reads because a write holds its slot
	// through every wait it makes (its copy's sync, build, upload, proposal and
	// its push's row, up to [GitWriteAttempt]), so a burst of writes on the
	// reads' lane would hold the slots every read on the node waits for. It
	// holds at most [gitWritesHeld] and answers the next at once; the seat
	// git-work share, never this lane, bounds the git a node's writes run
	// (BenchmarkEngineWrite, internal/gitlet/host).
	laneGitWrites lane = "git-writes"
)

// gitWritesHeld is how many engine writes a Gitlet node HOLDS at once for its
// peers on [laneGitWrites], each up to [GitWriteAttempt]: queue.ConcurrentAnswers,
// since a held write mostly waits on no git and the seat git-work share
// (gitlet.SeatWorkWrite) bounds its build, so this bounds only what writes hold
// ([lane.answers] has the measured figure). A write past them is answered AT
// ONCE, before decoding: `behind` at its share (gitlet.UnreadyShare), which the
// walk moves on from without demoting the node ([Router.routeGit]). Left to
// wait, it sits in the broker client's buffer, which nats.go drops past 64 MiB
// (DefaultSubPendingBytesLimit) unheard ([queue.EventQueue.Serve]) — some
// sixty-four writes of gitlet.MaxCommitPayloadBytes — and its asker suspects a
// healthy home for [suspectFor]. This node's own seats' writes are asked in
// process and hold none of these.
const gitWritesHeld = queue.ConcurrentAnswers

// lanes is every registration EVERY serving node makes, the shared one first.
var lanes = []lane{laneShared, laneProposals}

// gitLanes is every registration a node that serves GIT makes beside [lanes]
// ([ServerOptions.Git]), and no other: a node without the gitlet role would
// only answer `no_backend` there, a goroutine per slot for what the presence
// view already says. Registered from boot, before Gitlet is built, so askers
// hear `no_backend` rather than an unregistered subject's silence, read as a
// node gone; and both or neither ([Serve]'s EVERY LANE OR NONE), since a node
// answering reads and no writes is heard as gone by every write's walk.
var gitLanes = []lane{laneGitReads, laneGitWrites}

// subject is the subject node answers this lane on.
func (l lane) subject(node string) string {
	if l == laneShared {
		return Subject(node)
	}
	return Subject(node) + "." + string(l)
}

// answers is how many of this lane's requests a node answers at once. The git
// reads' lane is as wide as the shared one: it only SEPARATES git reads, and
// the seat git-work share (gitlet.GitShare) bounds the git running, in-process
// reads included, answering `share` at once where a narrower lane would leave
// reads waiting unseen in the broker's buffer.
//
// The writes' lane answers [gitWritesHeld] held writes and queue.ConcurrentAnswers
// more, refused `share` at once. MEASURED (TestWhatAHeldGitWriteHolds,
// TestAnEngineWriteTheRuleAdmitsCrossesTheWireAtItsBound): a held slot ~3 MiB
// at gitlet.MaxCommitPayloadBytes (3.02 payloads), ~200 MiB for sixty-four, its
// pack on disk; an at-once answer ~2 MiB for ~31 ms (-race, two cores). Past
// both, only ~192 such writes at one node overflow its client, unheard.
func (l lane) answers() int {
	switch l {
	case laneProposals:
		return proposalAnswers
	case laneGitWrites:
		return gitWritesHeld + queue.ConcurrentAnswers
	}
	return queue.ConcurrentAnswers
}

// String names the lane in a refusal.
func (l lane) String() string {
	if l == laneShared {
		return "the shared lane"
	}
	return "the " + string(l) + " lane"
}

// Actor is who a write is attributed to, as it crosses the wire: the author
// (Handle), its sort (Kind), and in its provenance the CREDENTIAL it was made
// through ([worklet.Provenance.OperatorID]), as [iam.ActorFor] writes it.
//
// A writer acts as exactly one party, derived from an identity the caller
// cannot choose per call (the seat bound into a turn, or the principal a
// request resolved to). The serving node derives its writer from this alone,
// so the history row names the author either node would have, and it arrives
// in a request the fleet SIGNED ([Envelopes]), so no party on the broker can
// name one.
type Actor struct {
	Handle string             `json:"handle"`
	Kind   worklet.AuthorKind `json:"kind"`

	// Provenance is the credential, turn and chain behind the write, and whom
	// it is written for. Left off the wire only where all of it is zero
	// ([encodeInput]); an older build sends an agent's empty provenance as no
	// member at all, so the actor's own decoder states whom its writes are for
	// either way ([Actor.UnmarshalJSONFrom]).
	Provenance worklet.Provenance `json:"provenance"`

	// Records asks the serving node to answer which work items the write
	// committed to ([reply.Written]), because the asker's provenance holds
	// a turn's [worklet.WriteLog], a set in the asking process that no
	// wire carries. Set by the router itself wherever that log is present.
	Records bool `json:"records,omitempty"`
}

// UnmarshalJSONFrom decodes the actor of a request this node serves, and
// states whom its writes are for WHATEVER ARRIVED ([types.WrittenFor.Received]):
// an older node sends an agent's empty provenance as no member at all, and
// stated only where a provenance arrived, such a write would state nothing,
// which the tracker refuses for good. No node asks a write whose actor may
// write nothing ([actorWrites]).
//
// THROUGH THE CALLER'S OWN DECODER, so under its options: on the wire
// ([decodeInput]) an undeclared member anywhere in it refuses the request,
// named at its path; an UnmarshalJSON would re-decode under encoding/json's
// defaults, which drop it silently. No encoder of its own: its fields' plain
// encoding is what every wire gate reads (wirerule_test.go's inputTypes).
func (a *Actor) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type wire Actor
	if err := jsonv2.UnmarshalDecode(dec, (*wire)(a)); err != nil {
		// UNWRAPPED, against the %w rule: json/v2 passes a method's
		// *jsonv2.SemanticError through as is and wraps any other error in
		// one placed where the decoder stopped, and [decodeInput] names the
		// refused member from the first SemanticError, so returned bare it
		// keeps the member's real path.
		return err
	}
	a.Provenance.WrittenFor = a.Provenance.WrittenFor.Received()
	return nil
}

// actorWrites is why an actor named who, of kind, whose writes are for
// writtenFor, may write nothing ([types.WrittenFor.For]): an agent's actor
// that states nobody, or a carrier on any other kind's (ADR-0109). Nil where it
// may write.
//
// EVERY WRITE'S ACTOR IS HELD TO IT BEFORE IT IS ASKED ANYWHERE, this node's
// own copy included, so every node answers alike: a tracker write's by
// [callAs], a page or chat write's by its [op.refusedBeforeAsking], a code
// write's by [codeWrites]. A serving node cannot check it: it reads an absent
// carrier and an older node's unsent one alike as stated nobody, and the woken
// turn would work for the writing seat, every app. Each domain's writer keeps
// its own check, for a write that reaches it another way.
func actorWrites(who string, kind iam.ActorKind, writtenFor types.WrittenFor) error {
	if _, err := writtenFor.For(kind); err != nil {
		return fmt.Errorf("estate: a write as %q is asked of no node: %w", who, err)
	}
	return nil
}

// codeWrites is the check every code write is held to before it is asked
// anywhere ([op.refusedBeforeAsking]): that the asker its request holds (no
// code operation runs as an actor) may state whom the write is for
// ([actorWrites], by gitlet.Principal.WriterKind). Six writes declare it: a
// push, a change's lifecycle and conversation, a merge queue's write, a merge
// and an engine write. The deciding node holds the rest of the carrier's rule
// (gitlet's writtenForUnder).
func codeWrites(asker gitlet.Principal, writtenFor types.WrittenFor) error {
	return actorWrites(asker.Actor(), asker.WriterKind(), writtenFor)
}

// opClass is what a failover may do with an operation that went unanswered.
//
// EVERY CLASS MOVES ON FROM A NODE THAT RAN NOTHING — one whose copy is out of
// service, that runs no backend for it, is not serving yet or is behind the
// caller's floor. What differs is what may be repeated once a node MAY have
// run it.
type opClass int

const (
	// opRead runs on any data node whose copy serves and changes nothing:
	// an unanswered one is asked of the next.
	opRead opClass = iota

	// opIdempotentWrite carries an operation id the caller minted once, and
	// the ledger answers a repeat with the first copy's result, so an
	// unanswered one, or one a node answered `unvouched`
	// ([statelog.Result.Unvouched]: its ledger cannot say whether it landed,
	// another's may), is asked of the next data node under the same id. One
	// no node ran while some never answered is [ErrOutcomeUnknown], never
	// [ErrUnserved]: the silent one may have run it.
	opIdempotentWrite

	// opOnceWrite has no id a caller holds, so a repeat is a second write.
	// An unanswered one is reported as [ErrOutcomeUnknown] and never asked
	// again.
	opOnceWrite
)

// opOrigin is who originates a write: a SURFACE, on somebody's behalf, or the
// ENGINE, recording what its own runs did. A different question from the
// class: it says who may make the write at all (package doc, "Who originates a
// write: a surface, or the engine").
type opOrigin int

const (
	// originSurface is a write a surface makes on somebody's behalf. The
	// zero value, because every write but the engine's own record is one.
	originSurface opOrigin = iota

	// originEngine is a write the engine makes of its own accord, through a
	// facade only the engine is handed ([ExecutorWrites]): the executor
	// log's, which records what a seat's box did.
	originEngine
)

// request is one operation, as the asking node sends it — signed whole
// ([Envelopes]), so every field below is one the fleet wrote.
type request struct {
	// ID is this request's own: minted per ask, so the same operation asked
	// of two nodes is two requests. A serving node takes an id once
	// ([signedask.Guard]) and its reply names it ([reply.Request]).
	ID string `json:"id"`

	Op string `json:"op"`

	// Args and Actor are the operation's INPUTS, its arguments and the actor
	// it runs as, crossing under the wire's rule ([encodeInput],
	// [decodeInput]): a key only where it carries a value, and a key the
	// serving build does not declare refuses the request. The rest of the
	// envelope is the router's own protocol, read as encoding/json always has.
	Args  json.RawMessage `json:"args,omitempty"`
	Actor json.RawMessage `json:"actor,omitempty"`

	// Floors are the positions the serving node must have applied before it
	// runs the operation — the asking node's session, on the log of the
	// operation's own domain. See the package doc.
	Floors []statelog.Position `json:"floors,omitempty"`

	// Deadline is the asker's instant, which binds and admits the request
	// (internal/signedask) and nothing else — REQUIRED, because it is also
	// how long a serving node remembers [request.ID]: a request that arrives
	// after it is refused unrun, so a replay of it is refused either way.
	Deadline time.Time `json:"deadline"`

	// BudgetMS is how long the asker waits for the reply
	// (signedask.WaitMS), which the serving node counts from the request's
	// arrival on its own clock to bound everything it runs for it
	// (signedask.Due), so two clocks standing apart neither cut the work
	// short nor let it run past its asker. REQUIRED: a request carrying none
	// runs nothing, as an older unreleased build gets no compatibility; an
	// older serving node ignores the field and serves as it always did.
	BudgetMS int64 `json:"budget_ms,omitzero"`

	// From is the asking node, for the serving node's log.
	From string `json:"from,omitempty"`

	// AcceptLagging is the asker's LAST RESORT: every better data node ran
	// nothing, so this one runs the operation although its copy lags or it is
	// behind the company's configuration ([unservedLagging]), held to the
	// floors and the read's own level. Never for an operation that JUDGES A
	// GRANT ([op.judgesGrant]), which a node behind the configuration refuses
	// whatever the asker says.
	AcceptLagging bool `json:"accept_lagging,omitempty"`

	// Spent is what the request already paid for on the nodes asked before
	// ([statelog.Spent]): a read index one established, which this node's
	// linearizable read waits for instead of a barrier of its own; positions
	// one waited its whole bound for, waited for no longer here; logs a
	// barrier ran out on, given no second barrier. Absent on a first ask.
	Spent *statelog.Spent `json:"spent,omitempty"`
}

// Binding is the request's id and deadline, as its envelope binds them
// ([signedask.Request]).
func (r request) Binding() (string, time.Time) { return r.ID, r.Deadline }

// Writes is true for every operation but a declared read, the git arm's
// writes included: a write replayed at a node that restarted would run again
// as the actor it names (internal/signedask). An operation this build does not
// declare runs nothing, and is counted a write so it is never taken unkept.
func (r request) Writes() bool {
	spec, known := registry[r.Op]
	return !known || spec.class != opRead
}

// unservedReason is why a node answered without running an operation.
type unservedReason string

const (
	// unservedNoBackend: this node runs no native backend for the op's half
	// (a vendor serves it, or the runtime is not up), OR a half it runs
	// DECLINED before running anything ([declined], [ranNothing]): a code half
	// refusing gitlet.ErrNotServing, or an identity directory asked to MAC a
	// machine token's answer under a key it lacks ([opIdentityMachineToken]).
	// Nothing ran, so every class moves on; Detail carries the half's own
	// cause ([ranNothingDetail]).
	unservedNoBackend unservedReason = "no_backend"

	// unservedNotEstablished: this node's copy admits no seat yet — the
	// strict gate admission's ping asks ([Backend.Admits]).
	unservedNotEstablished unservedReason = "not_established"

	// unservedLagging: this node's copy lags its logs ([Backend.Answers]
	// false: not drained since its appliers started, or past the snapshot
	// slack of their ends) — a WORSE NODE TO ASK ([worse]) rather than none:
	// the asker comes back to it with [request.AcceptLagging] when no data
	// node that is a better one runs the operation.
	unservedLagging unservedReason = "lagging"

	// unservedBehindConfig: this node is confirmed behind the company's
	// configuration ([Backend.Current] false), a worse node asked only after
	// every copy that merely lags ([Router.lastResort]); Detail names the
	// configuration, and the copy's lag where both hold. A reason of its own
	// because it LASTS until the node's apply succeeds, so askers remember it
	// ([Router.markBehindConfig]).
	//
	// It is the HARD ARM's answer too ([op.judgesGrant], [decidesNoGrant]),
	// and a seat's git read from such a Gitlet node (gitlet.UnreadyBehindConfig,
	// [reply.Unready]), remembered for its data operations too while the git
	// walk moves on ([Router.routeGit]). See the package doc, "A node behind the
	// company's configuration is a worse node too".
	unservedBehindConfig unservedReason = "behind_config"

	// unservedBehind: this node could not reach the caller's floor within
	// the read budget — or, for a git read ([GitCopy]), its git half could
	// not answer inside its budget for a reason about the node
	// (gitlet.ErrGitUnready: its copy still syncing, its share full, no chart
	// to judge a seat by, a failure of its own, or no copy held and no fill
	// asked of it), which another Gitlet node may answer; the reply's
	// [reply.Unready] then says which.
	unservedBehind unservedReason = "behind"

	// unservedOutOfService: this data node's copy of the operation's domain
	// is out of service — WRONG rather than behind (an applier halted, the
	// node evicted, its rows below the log), so it answers none of that
	// domain's operations until the copy recovers, while every other
	// domain's copy keeps serving and its own seats' calls on this one go to
	// the other data nodes ([LocalBackends.For], ADR-0079).
	unservedOutOfService unservedReason = "out_of_service"

	// unservedUnauthenticated: the request's envelope does not verify on
	// this node — signed under a key it does not hold, or not signed by
	// the fleet at all ([Envelopes]) — so nothing in it was believed and
	// nothing ran. A node that holds the key is asked next.
	unservedUnauthenticated unservedReason = "unauthenticated"

	// unservedStale: the request is not one this node may take now — it
	// names no id or no deadline, its deadline had passed when it arrived,
	// it carries a wait that is no duration ([request.BudgetMS]), or this
	// node has already taken its id ([signedask.Guard]). Nothing ran.
	unservedStale unservedReason = "stale"

	// unservedUnrecorded: the request is a write and this node's own file
	// would not keep its id ([signedask.Unrecorded]), so it ran nothing
	// rather than run a write a replay after a restart could run again.
	// Every class moves on.
	unservedUnrecorded unservedReason = "unrecorded"
)

// outOfService says which of node's copies is out of service, in the words a
// walk's note and a peer's reply carry — naming the node itself, as every
// other refusal a node gives does ("<node>'s copy of <domain> lags its log").
func outOfService(node, domain string) string {
	if domain == SeatCopy {
		return node + "'s copy of a domain seat admission depends on is out of service — " +
			"wrong rather than behind"
	}
	return node + "'s copy of " + domain + " is out of service — wrong rather than behind, " +
		"so it answers none of " + domain + "'s operations until it recovers"
}

// reply is what a serving node answers — signed whole, like the request.
type reply struct {
	// Node is who answered.
	Node string `json:"node"`

	// Request is the id of the request this answers ([request.ID]), so an
	// asker believes a reply only to the question it asked. Empty only
	// where the request could not be read at all.
	Request string `json:"request,omitempty"`

	// Unserved is set when the node did NOT run the operation, and says
	// why. Nothing was executed, so every class may move on.
	Unserved unservedReason `json:"unserved,omitempty"`
	Detail   string         `json:"detail,omitempty"`

	// Unready is, for a GIT operation the git half could not answer
	// (gitlet.ErrGitUnready: `behind`, or `behind_config` for that reason),
	// the reason it gave, which tells the walk whether the one node it lets
	// fill a copy still is that one (gitlet.UnreadyReason.PassesFill). Empty
	// on every other reply, and from a build that sends none: the walk then
	// keeps its filler ([Router.routeGit]).
	Unready gitlet.UnreadyReason `json:"unready,omitempty"`

	// Result is the operation's answer, and Err its failure. At most one.
	Result json.RawMessage `json:"result,omitempty"`
	Err    *wireError      `json:"error,omitempty"`

	// Obsolete names the streams whose floor this node could never reach —
	// a position on a generation the log has since abandoned — so the
	// asker drops them rather than carrying a floor nobody can satisfy.
	Obsolete []string `json:"obsolete,omitempty"`

	// Written is every work item the operation's writes COMMITTED to on this
	// node, for a request whose actor asked ([Actor.Records]): what a writer
	// in the asking process would have reported into the turn's
	// [worklet.WriteLog]. Reported whatever the answer, an error included,
	// since a walking gesture's earlier steps committed either way.
	Written []types.WorkItem `json:"written,omitempty"`

	// Committed is the kinds of change those writes were, for the asking
	// turn's outcome ([worklet.WriteLog.Commit]).
	Committed []types.CommittedChange `json:"committed,omitempty"`
}

// Answers is the id of the request this reply answers ([signedask.Reply]).
func (r reply) Answers() string { return r.Request }

// opSpec is one operation's declaration: what it is called, how a failover
// treats it, which log its floor is on, and its server half.
type opSpec struct {
	name   string
	class  opClass
	args   reflect.Type
	result reflect.Type

	// domain is the domain whose COPY the operation reads, whose verdict is
	// asked before running it here ([LocalBackends.For], ADR-0079), or
	// [SeatCopy] for admission's ping. floorStream is that domain's log, the
	// one its floors are on ([Router.floorsFor], [ready]), or empty for an
	// operation that carries none ([op.floorless]), which a gate over the
	// registry names.
	domain      string
	floorStream string

	// alsoReads is every OTHER domain whose rows the operation reads
	// ([op.alsoReads]): its verdict is asked too, and a copy of any of them
	// out of service runs none of it here. Its floor and its domain stay
	// the one above.
	alsoReads []string

	// copies is what the router keys where it STAYS and whom it passes over
	// by: the domain plus every domain also read, since a node that ran an
	// operation over both copies is the one to ask for that operation, not
	// for either domain's alone ([Router.order]).
	copies string

	// actor says whether the operation acts AS somebody, which a request
	// then has to name.
	actor bool

	// origin is who originates a write ([opOrigin]): a surface unless the
	// declaration says the engine ([op.byEngine]).
	origin opOrigin

	// ungated operations run on a node that is a worse one to ask ([worse]):
	// they ask the copy's own gate themselves ([opPing]).
	ungated bool

	// git is an operation asked of the GITLET nodes ([op.keyedOnGit]): its
	// domain is [GitCopy], so it reads no domain's copy, carries no floor
	// and meets no lag gate; the router walks the repository's git order for
	// it ([Router.routeGit]) and a serving node answers it from its git half.
	git bool

	// judges is the predicate an operation that JUDGES A GRANT from the
	// serving node's chart declared over its decoded arguments
	// ([op.judgesGrant]), nil for every other: where it holds on a node
	// behind the company's configuration, the operation is answered
	// [errDecidesNoGrant] before it runs, however it was asked.
	judges func(args any) bool

	// asked is the check the ASKING node makes of the arguments before the
	// request runs anywhere ([op.refusedBeforeAsking]), nil where none is
	// declared. On the declaration, as judges is, so a gate over the
	// registry can ask it of every operation.
	asked func(args any) error

	// lane is the registration a serving node answers the operation on —
	// the shared one, the zero lane, unless it is declared on another
	// ([op.answeredOn]) — and the one subject the router asks it on.
	lane lane

	// whole is an operation whose ANSWER a caller carries into another request
	// ([op.answeredWhole]): the serving node encodes it under the wire's
	// rule for an operation's inputs, and the asking router decodes it
	// refusing a key this build does not declare ([ErrUnknownKeys]).
	whole bool

	// answer is how large the operation's answer can grow ([answerShape]):
	// declared on every read, and one value on every write.
	answer answerShape

	// decode is the server half's first step: the request's arguments,
	// decoded under the wire's rule ([decodeInput]) into the operation's own
	// type — boxed — or every member they hold that the type does not
	// declare, by its path, and nothing run.
	decode func(raw json.RawMessage) (args any, unknown []string, err error)

	// run is the server half's second: the decoded arguments run on this
	// node's backend, as the actor the request names.
	run func(ctx context.Context, b Backend, actor *Actor, args any) (any, error)
}

// op is a typed handle on one registered operation: its declaration, and its
// two halves in the caller's own types, which the router calls in-process
// where this node's copy serves — no encoding, and an error keeps its own
// identity rather than the wire's rebuilt one.
type op[A, R any] struct {
	spec  *opSpec
	serve func(ctx context.Context, b Backend, actor *Actor, args A) (R, error)

	// repeatable reports a ONCE-WRITE whose arguments name the caller's own
	// idempotency key, and so may be asked again under it — see
	// [op.repeatableWhen]. Nil for every other operation.
	repeatable func(A) bool

	// key names the node an operation PREFERS: the arguments' routing key (a
	// push's repository), over which the router orders every data node by
	// [rendezvous.Order], this one included ([Router.keyedOrder]). Nil where
	// any data node answers equally: asked of this node first, then of the
	// node that answered last. See [op.keyedBy].
	key func(A) string

	// home stamps a GIT operation's arguments with whether the node asked
	// is its walk's effective home ([op.keyedOnGit]), nil for every other.
	home func(A, bool) A

	// attempt is a git write's attempt on one Gitlet node FOR ITS ARGUMENTS
	// ([op.attemptBy]) — an engine write's, which follows its kind's build
	// (Timing.GitWriteAttemptFor) — and nil for every operation whose
	// attempt is its class's alone ([Router.wholeAttempt]).
	attempt func(A, Timing) time.Duration
}

// refusedBeforeAsking declares a check the ASKING node makes of the arguments
// before the request runs anywhere, in process or routed, refusing with
// check's error. A property of the asking side, like [op.keyedBy].
//
// For a value the wire does not carry as itself: a zero field, or a pointer at
// a value its type's IsZero calls zero, crosses as nothing ([encodeInput]), so
// a writer refusing it where it runs answers by which node asked (the wire
// gate's [zeroThroughPointer], wirerule_test.go, names each such check). And
// for an actor that may write nothing ([actorWrites], [codeWrites]), which a
// serving node cannot tell from an older build's unsent carrier: every such
// write declares it, as TestEveryOperationHoldingAPageOrChatActorRefusesOneThatCannotWrite
// and TestEveryCodeWriteRefusesAnAgentsRequestThatStatesNobody hold.
func (o op[A, R]) refusedBeforeAsking(check func(A) error) op[A, R] {
	name := o.spec.name
	o.spec.asked = func(args any) error {
		a, ok := args.(A)
		if !ok {
			return fmt.Errorf("estate: %s is asked with arguments of type %T", name, args)
		}
		return check(a)
	}
	return o
}

// attemptBy declares a git write whose attempt on one Gitlet node follows its
// arguments: of becomes the walk's attempt in place of the class's
// [Timing.GitWriteAttempt] — what it waits on each node, and how long a node's
// silence must last to be evidence it is gone. Only a git write takes one.
func (o op[A, R]) attemptBy(of func(A, Timing) time.Duration) op[A, R] {
	if !o.spec.git || o.spec.class == opRead {
		panic(fmt.Sprintf("estate: %s declares an attempt of its own, which only a git write takes", o.spec.name))
	}
	o.attempt = of
	return o
}

// registry is every operation this build serves, keyed by name.
//
// ONE TABLE FOR BOTH HALVES: the client asks through the value the server
// dispatches on, so every operation it can send is one the server knows, and
// the wire gate checks a new operation's types the moment it is declared.
var registry = map[string]*opSpec{}

// SurfaceWriteDomains is every domain a SURFACE writes through this router, by
// its state-log name, sorted, read off the operation table so a new surface
// write's domain is in it at once.
//
// EXPORTED for the gates covering every domain a surface writes:
// internal/agent/builtin's WriterClass and ObjectArgs declare each, and one
// missing there answers a person's content refusal as a fault. The engine's
// own writes ([originEngine]) are not in it: no surface makes one, so none has
// a refusal of them to class or an object of them to name.
func SurfaceWriteDomains() []string {
	seen := map[string]bool{}
	for _, spec := range registry {
		if !spec.surfaceWrite() {
			continue
		}
		domain := spec.domain
		if domain == GitCopy {
			// A GIT WRITE lands on the code log, as a push Gitlet
			// proposes ([opCodeGitWrite]), so its refusals are the code
			// domain's: no state log is named "git".
			domain = codeDomain
		}
		seen[domain] = true
	}
	return slices.Sorted(maps.Keys(seen))
}

// surfaceWrite reports a write a surface originates ([originSurface]), on
// any domain but [SeatCopy], which no write is about: what
// [SurfaceWriteDomains] collects.
func (s *opSpec) surfaceWrite() bool {
	return s.class != opRead && s.domain != SeatCopy && s.origin == originSurface
}

// define declares an operation on the domain whose copy it reads and whose log
// its floor is on ([op.floorless] drops the floor), on [SeatCopy] for one
// about the copy a seat is served by, or on [GitCopy] for a Gitlet node's git
// half ([op.keyedOnGit]). At init: a duplicate name, or a domain with no known
// log, panics, since that build cannot serve.
//
// A FIELD'S ZERO MEANS WHAT AN OLDER BUILD DOES: a field of A, at any depth,
// crosses only where non-zero ([encodeInput]), so one a later build adds must
// mean at its zero what a build without it does (`Drop`, never `Keep`). A
// type's own IsZero decides that zero, through a pointer too, which then
// crosses as nil; the wire gate holds each to a reason (wirerule_test.go's
// zeroByMethod, zeroThroughPointer), else it is reshaped or refused ([op.refusedBeforeAsking]).
func define[A, R any](name string, class opClass, domain string, actor bool,
	serve func(ctx context.Context, b Backend, actor *Actor, args A) (R, error),
) op[A, R] {
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("estate: operation %q declared twice", name))
	}
	spec := &opSpec{
		name: name, class: class, actor: actor, domain: domain, copies: domain,
		args: reflect.TypeFor[A](), result: reflect.TypeFor[R](),
	}
	if class != opRead {
		spec.answer = answerShape{kind: answerOne}
	}
	if domain != SeatCopy && domain != GitCopy {
		stream, known := floorStreams[domain]
		if !known {
			panic(fmt.Sprintf("estate: operation %q reads the %q domain's copy, "+
				"which is no domain this package knows", name, domain))
		}
		spec.floorStream = stream
	}
	spec.decode = func(raw json.RawMessage) (any, []string, error) {
		var args A
		if len(raw) == 0 {
			return args, nil, nil
		}
		unknown, err := decodeInput(raw, &args)
		switch {
		case err != nil:
			return nil, nil, fmt.Errorf("estate: %s: decode the arguments: %w", name, err)
		case len(unknown) > 0:
			return nil, unknown, nil
		}
		return args, nil, nil
	}
	checked := func(ctx context.Context, b Backend, who *Actor, args A) (R, error) {
		var zero R
		if spec.actor && (who == nil || who.Handle == "") {
			return zero, fmt.Errorf("estate: %s acts as somebody and the request "+
				"names nobody — a write whose author was not stated is not an "+
				"audit trail", name)
		}
		// THE HARD ARM, after decoding and before anything runs, in process and
		// on the wire alike: a node behind the company's configuration judges
		// no grant from its chart, whatever the asker said ([op.judgesGrant]).
		if spec.judges != nil && b.Current != nil && !b.Current() && spec.judges(args) {
			return zero, errDecidesNoGrant
		}
		return serve(spec.answer.reading(ctx), b, who, args)
	}
	spec.run = func(ctx context.Context, b Backend, who *Actor, args any) (any, error) {
		return checked(ctx, b, who, args.(A))
	}
	registry[name] = spec
	return op[A, R]{spec: spec, serve: checked}
}

// floorless declares that this operation carries no floor: what it reads is
// not its domain's rows at a log position — an index maintained behind the
// applier, or the copy's own readiness — so no floor could hold its answer to
// anything. Its domain still names the copy it reads, and a wrong copy of that
// domain still runs none of it.
func (o op[A, R]) floorless() op[A, R] {
	o.spec.floorStream = ""
	return o
}

// alsoReads declares that this operation also reads other domains' rows (a
// page's backlinks name the tasks linking to it), so each one's verdict is
// asked before it runs here, and an out-of-service copy answers
// `out_of_service` naming it (ADR-0079). Its FLOOR stays on its own domain's
// log: a second floor would hold it to an applier its answer does not wait on.
func (o op[A, R]) alsoReads(domains ...string) op[A, R] {
	for _, d := range domains {
		if _, known := floorStreams[d]; !known || d == o.spec.domain {
			panic(fmt.Sprintf("estate: %s also reads %q, which is no other domain "+
				"this package knows", o.spec.name, d))
		}
	}
	o.spec.alsoReads = slices.Sorted(slices.Values(domains))
	o.spec.copies = strings.Join(append([]string{o.spec.domain}, o.spec.alsoReads...), "+")
	return o
}

// unservedCopy is the first copy op reads that local holds out of service —
// its own domain's, then each it also reads — or false where every one
// serves, with the backend over the operation's own domain.
func unservedCopy(ctx context.Context, local LocalBackends, spec *opSpec) (Backend, string, bool) {
	b, serves := local.For(ctx, spec.domain)
	if !serves {
		return Backend{}, spec.domain, true
	}
	for _, d := range spec.alsoReads {
		if _, serves := local.For(ctx, d); !serves {
			return Backend{}, d, true
		}
	}
	return b, "", false
}

// answeredOn declares the lane a serving node answers this operation on, and
// the one the router asks it on — see [lane]. Package-level, at init.
func (o op[A, R]) answeredOn(l lane) op[A, R] {
	switch {
	case !slices.Contains(lanes, l) && !slices.Contains(gitLanes, l):
		panic(fmt.Sprintf("estate: %s is answered on %s, which no node serves", o.spec.name, l))
	case slices.Contains(gitLanes, l) != o.spec.git:
		// A git lane answers git operations alone: only a node serving git
		// registers one, so a data operation there goes unanswered by a data
		// node without the gitlet role, and a git operation elsewhere would
		// hold that lane's slots.
		panic(fmt.Sprintf("estate: %s is answered on %s, and a git lane answers git operations alone",
			o.spec.name, l))
	}
	o.spec.lane = l
	return o
}

// ungated declares that the serving gate does not hold this operation back —
// see [opSpec.ungated].
func (o op[A, R]) ungated() op[A, R] {
	o.spec.ungated = true
	return o
}

// answeredWhole declares a READ whose answer a caller carries into another
// request through this router (a version's files a publish carries on, a
// page's cursor the next page asks by), so the answer crosses under the
// inputs' rule: the asking router reads it whole or refuses it
// ([ErrUnknownKeys], its Answer set) rather than silently drop what a newer
// data node said and send the rest on. A read only shown is not declared:
// dropping a field it shows loses nothing. Where one read is both, the carried
// one is its own operation ([opArtifactVersionCarried] beside
// [opArtifactVersion]). The wire gate is two-sided (answeredWholeBy): every
// declared read names the request it is carried into, and one no longer
// carried fails it.
func (o op[A, R]) answeredWhole() op[A, R] {
	if o.spec.class != opRead {
		panic(fmt.Sprintf("estate: %s is a write, and a write's answer is never refused: by the time it "+
			"arrives the write has run", o.spec.name))
	}
	o.spec.whole = true
	return o
}

// byEngine declares a write the ENGINE originates ([originEngine]): kept out
// of [SurfaceWriteDomains] and reached only through a facade the engine alone
// is handed; the gate over the packages serving a person or a model holds the
// class. A read panics: the engine's reads are ordinary reads. One read's
// ANSWER is the engine's alone, chat.turn_thread's (whom each message was
// written for), held by that same gate rather than a declaration ([TurnThreads]).
func (o op[A, R]) byEngine() op[A, R] {
	if o.spec.class == opRead {
		panic(fmt.Sprintf("estate: %s is a read, and only a write has an originator: "+
			"a read the engine makes is an ordinary read", o.spec.name))
	}
	o.spec.origin = originEngine
	return o
}

// repeatableWhen declares a once-write IDEMPOTENT for the calls whose
// arguments say so: a page or chat write whose actor carries the caller's
// operation key ([doclet.Actor.OpKey], [chatlet.Actor.OpKey]) derives its
// operation id, and a create's or post's object id, from that key, so a repeat
// is the same operation the ledger answers with the first outcome, failing
// over as an [opIdempotentWrite]; one without a key stays [opOnceWrite]. A
// property of the ASKING side: the server runs it the same either way.
func (o op[A, R]) repeatableWhen(keyed func(A) bool) op[A, R] {
	if o.spec.class != opOnceWrite {
		panic(fmt.Sprintf("estate: %s is not a once-write, so nothing about its "+
			"arguments can make it repeatable", o.spec.name))
	}
	o.repeatable = keyed
	return o
}

// keyedBy declares that the operation is asked of the data nodes in the
// rendezvous order of key(args) rather than this node first ([Router.route];
// package doc, "A push is asked of its repository's committer"). The key never
// crosses the wire: only the asker chooses whom to ask. Arguments naming no
// key are refused before any node is asked, since "no node preferred" would
// spread one repository's pushes over as many committers as there are askers.
func (o op[A, R]) keyedBy(key func(A) string) op[A, R] {
	o.key = key
	return o
}

// keyedOnGit declares an operation on a repository's GIT OBJECTS, asked of the
// Gitlet nodes in [rendezvous.Order] of key(args), home first (ADR-0089,
// [Router.routeGit]; package doc, "A repository's git work is asked of its
// home Gitlet node"). Defined on [GitCopy]: no copy verdict, dispatch floor or
// lag gate holds it back, its floor travelling in its arguments
// (gitlet.GitQuery.Fresh). At init, before [op.answeredOn] names its lane.
//
// home stamps each attempt's arguments with whether the node asked is the one
// that may FILL a copy it does not hold: the walk's effective home, or the
// next node where that one said no copy is coming. Every other node holding
// none says so at once, so an unheld repository costs one download, not one
// per Gitlet node. The walk sets it on every attempt, so no caller chooses it.
func (o op[A, R]) keyedOnGit(key func(A) string, home func(A, bool) A) op[A, R] {
	if o.spec.domain != GitCopy {
		panic(fmt.Sprintf("estate: %s is keyed on the Gitlet nodes, and reads the %q copy rather than %q",
			o.spec.name, o.spec.domain, GitCopy))
	}
	o.spec.git = true
	o.key = key
	o.home = home
	return o
}

// judgesGrant declares an operation that JUDGES A GRANT from the serving
// node's chart wherever pred holds of its decoded arguments (a seat's read,
// whose grant on a repository is the chart's, ADR-0088). A node CONFIRMED
// behind the configuration ([Backend.Current] false) answers it
// [errDecidesNoGrant] before it runs, EVEN ASKED LAST: the revision it failed
// to apply may have revoked the grant, so the walk ends [ErrUnserved] (package
// doc, "A node behind the company's configuration is a worse node too").
//
// A DECLARATION, NOT A LIST: TestEveryOperationThatJudgesAGrantIsDeclared
// holds every operation on a domain's copy whose arguments carry a
// gitlet.Principal Asker to it. A git operation is outside it: its node
// refuses a seat itself when behind (gitlet.ErrGitUnready, `behind_config`).
func (o op[A, R]) judgesGrant(pred func(A) bool) op[A, R] {
	o.spec.judges = func(args any) bool {
		a, ok := args.(A)
		return ok && pred(a)
	}
	return o
}

// errDecidesNoGrant is an operation that judges a grant ([op.judgesGrant]),
// asked of a node confirmed behind the company's configuration: nothing ran.
// It never crosses the wire — the server and the router's own in-process run
// answer it `behind_config` with the hard arm's detail ([decidesNoGrant]) —
// so it is no sentinel of the wire's.
var errDecidesNoGrant = errors.New("estate: this node is behind the company's configuration and decides no grant")

// decidesNoGrant is what a node behind the company's configuration answers an
// operation that judges a grant with, naming itself.
func decidesNoGrant(self string) string {
	return fmt.Sprintf("%s is behind the company's configuration and decides no grant from the chart "+
		"the fleet replaced — the revision it failed to apply may be the one that revoked it — so it "+
		"runs none of it, even asked last", self)
}
