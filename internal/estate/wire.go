package estate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// SubjectPrefix prefixes every serving node's subject.
const SubjectPrefix = "crewlet.estate"

// Subject is the subject a serving node answers every operation on but a
// push's proposal, which it answers on [ProposalSubject].
//
// THE NODE ID IS ESCAPED as a subject token, because a node id may carry a dot
// (`node.id` admits one), and a raw dot would split it into two tokens — a
// subject some other node's escaped id could collide with.
func Subject(nodeID string) string {
	return SubjectPrefix + "." + coord.DocumentKey(nodeID)
}

// ProposalSubject is the subject a serving node answers proposals on, a push's
// and a compaction's — its node's subject with one more token, so no node's
// [Subject] is ever another's: an escaped id is always exactly one token.
func ProposalSubject(nodeID string) string { return laneProposals.subject(nodeID) }

// lane is one of a serving node's registrations ([Serve]): the subject it
// answers on, and how many of its requests it answers at once.
//
// SEPARATE, because an answer that WAITS must never hold a slot an answer that
// does not is waiting for — a registration's slots are its own, and a request
// past them waits behind every answer holding one ([queue.EventQueue.Serve]).
// A push's proposal waits: queued behind its repository's earlier pushes,
// then for its batch, for as long as its asker's whole attempt — and a
// compaction's waits in the same queue. Every other operation is a read or a
// write done now. On one registration, sixty-four queued pushes held every
// slot the node had — it answered no tracker read, no page write and no git
// fetch's code.resolve until a batch ended; a reader
// gave up after [ReadAttempt] and marked it suspect for every domain; and an
// election stalling the code log stalled every committer with sixty-four
// pushes in flight, spreading the suspicion across the fleet — while the
// committer's queue, sized to take a burst whole, could never fill. So
// proposals have a lane of their own, as wide as the committer's queue
// ([proposalAnswers]), and a lane answers only the operations declared on it
// ([op.answeredOn]). A git node's reads of a repository by object id wait
// too — on a sync of the node's copy and a git process of up to
// [GitReadAttempt] — so they have a third lane, and its engine writes, which
// wait on far more again ([GitWriteAttempt]), a fourth: both registered only
// where the node serves git ([gitLanes]).
type lane string

const (
	// laneShared answers every operation but a proposal and a git read, on
	// [Subject].
	laneShared lane = ""

	// laneProposals answers proposals alone — a push's, a compaction's, an
	// expunge record's, a change's and a merge queue's, a write of a change's
	// conversation, which waits its change's turn on the same node, and a
	// write of a repository's checks, which waits its repository's turn
	// there — on [ProposalSubject]. Each kind that waits is bounded by its writer, which
	// answers past its bound busy at once, and the lane is as wide as their
	// bounds together ([proposalAnswers]).
	laneProposals lane = "proposals"

	// laneGitReads answers a repository's git reads alone ([GitCopy]) — on
	// the node's subject with one more token, as the proposals' lane is —
	// and only on a node that serves git ([ServerOptions.Git]).
	laneGitReads lane = "git"

	// laneGitWrites answers a repository's ENGINE WRITES alone
	// ([opCodeGitWrite]) — on the node's subject with one more token — and,
	// as the reads' lane, only on a node that serves git. Apart from the
	// reads, because a write holds its answer slot through every wait it
	// makes — the sync of its copy, its build, its bundle's upload, its
	// proposal and the wait for its push's row, up to [GitWriteAttempt] —
	// where a read holds one through a sync and a git process: the
	// proposals' lesson again, a slot held through a wait is one every
	// other request of its registration waits behind, so a burst of writes
	// on the reads' lane would hold the slots every seat's read of every
	// repository on the node is answered through. It HOLDS at most
	// [gitWritesHeld] writes at once, and answers the next at once. What
	// bounds the git a node's writes run is the seat git-work share, never
	// this lane: BenchmarkEngineWrite's sixty-four writes at once
	// (internal/code/host) were all in flight with none queued for a slot,
	// every write past the share's wait answered unready rather than held —
	// so a write waits on the share or nothing.
	laneGitWrites lane = "git-writes"
)

// gitWritesHeld is how many engine writes a git node HOLDS at once for its
// peers on its writes' lane ([laneGitWrites]), each through every wait it
// makes, up to [GitWriteAttempt]; a write a peer asks past them is answered AT
// ONCE, before its arguments are decoded or its git half asked: `behind`, at
// its share (code.UnreadyShare), which its walk moves on from without demoting
// the node — the next git node holding a copy builds it, or the walk ends
// unserved, saying why ([Router.routeGit]).
//
// SIXTY-FOUR, queue.ConcurrentAnswers, the lane's width before it answered at
// once: what a held write waits on is mostly no git at all — its upload, its
// proposal, its push's row — while its build takes a unit of the node's seat
// git-work share (code.SeatWorkWrite) and gives it back before it lands, so
// the share, never this bound, bounds the git a node's writes run, and this
// bounds what they hold ([lane.answers] states the figure, measured).
//
// ANSWERED AT ONCE, NEVER LEFT TO WAIT FOR A SLOT, because a request a
// registration has no slot for waits in its broker client's buffer, and the
// broker client drops one past that buffer's bound, unheard by its asker
// ([queue.EventQueue.Serve]): nats.go holds a subscription's pending messages
// to 64 MiB (its DefaultSubPendingBytesLimit) and drops the rest as a slow
// consumer, which the node logs naming no subject. A write is up to a
// mebibyte as the wire spells it (code.MaxCommitPayloadBytes), so some
// sixty-four waiting behind a full lane — 128 large writes in flight against
// one git node — overflowed it, and the asker of a dropped one heard
// nothing for its whole attempt, then took the healthy node for a gone one:
// asked it last for every operation for [suspectFor], so every repository it
// is home to sent its reads and writes to a runner-up that downloaded a copy
// of each. Answered at once, the asker hears the node is busy within the
// milliseconds an admission takes.
//
// This node's own seats' writes are asked in process and hold none of these:
// they are bounded by their turns, and hold nothing a peer's request does.
const gitWritesHeld = queue.ConcurrentAnswers

// lanes is every registration EVERY serving node makes, the shared one first.
var lanes = []lane{laneShared, laneProposals}

// gitLanes is every registration a node that serves GIT makes beside [lanes]
// ([ServerOptions.Git]), and no other node: a node without the git role has no
// git half, and a lane it answered only `no_backend` on would still cost each
// of its slots a goroutine for nothing a git walk could not learn from the
// presence view. A git node registers them from boot, before its code host is
// built, so its askers hear `no_backend` rather than the silence of an
// unregistered subject, which they would read as a node gone and suspect for
// every operation — and registers both or neither ([Serve]'s EVERY LANE OR
// NONE): a git node answering its reads and no write would be heard as gone by
// every write's walk, no answerer on its subject, and so asked last for every
// operation and reported to both placements while it served.
var gitLanes = []lane{laneGitReads, laneGitWrites}

// subject is the subject node answers this lane on.
func (l lane) subject(node string) string {
	if l == laneShared {
		return Subject(node)
	}
	return Subject(node) + "." + string(l)
}

// answers is how many of this lane's requests a node answers at once.
//
// The git reads' lane is as wide as the shared one, queue.ConcurrentAnswers:
// the lane SEPARATES git reads from the slots the node answers its replicated
// reads and pushes' resolves through, and what bounds the git work RUNNING is
// the node's seat git-work share (code.GitShare, half its read lane), which
// holds this node's own seats' in-process reads too — a lane bound would hold
// only the remote askers, and a narrower one would only leave reads waiting
// unseen in the broker's buffer where the share answers them `share` at once.
//
// The git writes' lane answers the [gitWritesHeld] writes it holds and
// queue.ConcurrentAnswers more, which it answers AT ONCE: a write past the
// held ones, refused `share` before its arguments are decoded — work done
// now, as wide as every registration answering work done now — so a burst
// past the held writes is answered rather than left waiting in the broker
// client's buffer.
//
// What a full writes' lane holds is MEASURED (TestWhatAHeldGitWriteHolds,
// TestAnEngineWriteTheRuleAdmitsCrossesTheWireAtItsBound): a held write's
// slot holds its request as the broker delivered it — the payload, at most
// code.MaxCommitPayloadBytes as JSON spells it, and some 7 KiB beside it at
// the longest the engine stamps the rest — the request as admitted, its
// arguments copied out of it, and the write decoded, which the git half holds
// to its end: about 3 MiB a slot at the bound, three payloads and what is
// beside them (2.96 and 3.09 payloads measured), so about 200 MiB for the
// sixty-four; an at-once answer holds the first two, some 2 MiB, for the
// milliseconds its admission takes — 31 ms measured, under -race on two
// cores; and a held write's staged pack, at most code.MaxWritePackBytes, is
// on the cache's volume rather than in memory. Past both, a request waits in
// the broker client's buffer only behind the at-once answers ahead of it,
// each a few milliseconds' work, so the buffer fills only under some
// sixty-four more mebibyte writes inside those milliseconds — some 192 at one
// git node at once, where 128 in flight overflowed it before — and past that
// a write is still dropped unheard ([queue.EventQueue.Serve]).
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
// (Handle), what sort of author it is (Kind), and in its provenance the
// CREDENTIAL the write was made through ([tracker.Provenance.OperatorID]) —
// the triple every record carries, as [iam.ActorFor] writes it — beside the
// turn and chain that produced it.
//
// The tracker's rule is that a writer acts as exactly one party, derived from
// an identity the caller cannot choose per call: the seat bound into a turn,
// or the principal a request resolved to. The serving node derives its writer
// from what arrives here and from nothing else, so the history row names the
// same author either node would have written — and what arrives is the actor
// inside a request the fleet SIGNED ([Envelopes]), so no party on the broker
// can name one.
type Actor struct {
	Handle     string             `json:"handle"`
	Kind       tracker.AuthorKind `json:"kind"`
	Provenance tracker.Provenance `json:"provenance"`

	// Records asks the serving node to answer which work items the write
	// committed to ([reply.Written]), because the asker's provenance holds
	// a turn's [tracker.WriteLog] — a set in the asking process that no
	// wire carries. Set by the router itself wherever that log is present.
	Records bool `json:"records,omitempty"`
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
	// the ledger answers a repeat with the first copy's result — so an
	// unanswered one is asked of the next data node under the same id. So
	// is one a node answered `unvouched` ([statelog.Result.Unvouched]):
	// that node's ledger cannot say whether the operation landed, and
	// another node's may, so the answer is not final until every node has
	// been asked.
	opIdempotentWrite

	// opOnceWrite has no id a caller holds, so a repeat is a second write.
	// An unanswered one is reported as [ErrOutcomeUnknown] and never asked
	// again.
	opOnceWrite
)

// request is one operation, as the asking node sends it — signed whole
// ([Envelopes]), so every field below is one the fleet wrote.
type request struct {
	// ID is this request's own: minted per ask, so the same operation asked
	// of two nodes is two requests. A serving node takes an id once
	// ([signedask.Guard]) and its reply names it ([reply.Request]).
	ID string `json:"id"`

	Op    string          `json:"op"`
	Args  json.RawMessage `json:"args,omitempty"`
	Actor *Actor          `json:"actor,omitempty"`

	// Floors are the positions the serving node must have applied before it
	// runs the operation — the asking node's session, on the log of the
	// operation's own domain. See the package doc.
	Floors []statelog.Position `json:"floors,omitempty"`

	// Deadline is the asker's, so the serving node stops working on an
	// answer nobody is waiting for — and REQUIRED, because it is also how
	// long a serving node remembers [request.ID]: a request that arrives
	// after it is refused unrun, so a replay of it is refused either way.
	Deadline time.Time `json:"deadline"`

	// From is the asking node, for the serving node's log.
	From string `json:"from,omitempty"`

	// AcceptLagging is the asker's LAST RESORT: every data node that is not a
	// worse one to ask has run nothing, so this one runs the operation
	// although its copy lags its logs or the node is behind the company's
	// configuration ([unservedLagging]) — held to the floors above and to the
	// read's own level, which are what make its answer sound. Never an
	// operation that JUDGES A GRANT ([op.judgesGrant]): a node behind the
	// configuration refuses one whatever the asker says, since being asked
	// last makes no grant judged from the chart the fleet replaced sound.
	AcceptLagging bool `json:"accept_lagging,omitempty"`

	// Spent is what the request already paid for on the nodes the asker
	// asked before this one ([statelog.Spent]): the read index one of them
	// established, which this node's linearizable read waits for rather than
	// append a barrier of its own; every position one of them waited its
	// whole bound for, which this node waits for no longer; every log a
	// barrier ran out on, which it appends no second barrier to. Absent on a
	// request's first ask.
	Spent *statelog.Spent `json:"spent,omitempty"`
}

// Binding is the request's id and deadline, as its envelope binds them
// ([signedask.Request]).
func (r request) Binding() (string, time.Time) { return r.ID, r.Deadline }

// unservedReason is why a node answered without running an operation.
type unservedReason string

const (
	// unservedNoBackend: this node runs no native backend for the op's
	// half — the company is on a vendor for it, or the runtime is not up —
	// OR a half it does run DECLINED before it ran anything ([declined],
	// [ranNothing]): a code half refusing code.ErrNotServing — a
	// committer stopping, a node in a maintenance mode that publishes
	// nothing, a refusal about this node, a half with no reader — or an
	// identity directory asked to MAC a machine token's answer under a key
	// the node does not hold ([opIdentityMachineToken]). Either way
	// nothing ran, so every class moves on; the reply's Detail then carries
	// the half's own cause ([ranNothingDetail]), so a node that has the
	// backend and answered no_backend says why.
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
	// configuration ([Backend.Current] false) — a worse node to ask as a
	// lagging copy is, and asked again the same way, told to answer anyway,
	// when no better node runs the operation: after every copy that only
	// lags, whose answer is held to the current configuration where this
	// node's is decided under the one the fleet replaced
	// ([Router.lastResort]). The Detail names the configuration, and the
	// copy's lag too where both hold.
	//
	// A REASON OF ITS OWN BECAUSE IT LASTS. A lag closes as the applier
	// catches up; a node stays behind its configuration until its apply
	// succeeds — a stuck one until a person acts — so the asker REMEMBERS
	// this answer ([Router.markBehindConfig]) and asks the node only as the
	// last resort meanwhile. Folded into `lagging`, which nobody remembers,
	// every request any router made sent it a refusal's round trip first —
	// a push its whole proposal — for as long as it stayed behind.
	//
	// AND IT IS THE HARD ARM'S ANSWER TOO: an operation that judges a grant
	// from the chart ([op.judgesGrant]) is answered `behind_config` by such a
	// node even when it is asked last, told to answer anyway — the Detail
	// then says it decides no grant ([decidesNoGrant]) — so the last resort
	// meets the refusal again and the walk ends unserved. So is a SEAT'S git
	// read on such a git node, whose git half judges no seat
	// (code.UnreadyBehindConfig, [reply.Unready]): the posture is the
	// node's, so every asker remembers it for the node's data operations as
	// well, while the git walk, which takes no lag gate, moves on from it as
	// from any git node that ran nothing — and lets the next one fill a copy
	// ([Router.routeGit]).
	unservedBehindConfig unservedReason = "behind_config"

	// unservedBehind: this node could not reach the caller's floor within
	// the read budget — or, for a git read ([GitCopy]), its git half could
	// not answer inside its budget for a reason about the node
	// (code.ErrGitUnready: its copy still syncing, its share full, no chart
	// to judge a seat by, a failure of its own, or no copy held and no fill
	// asked of it), which another git node may answer; the reply's
	// [reply.Unready] then says which.
	unservedBehind unservedReason = "behind"

	// unservedOutOfService: this data node's copy of the operation's domain
	// is out of service — WRONG rather than behind (an applier halted, the
	// node evicted, its rows below the log), so it answers none of that
	// domain's operations until the copy recovers, while every other
	// domain's copy keeps serving and its own seats' calls on this one go to
	// the other data nodes ([LocalBackends.For], ADR-0049).
	unservedOutOfService unservedReason = "out_of_service"

	// unservedUnauthenticated: the request's envelope does not verify on
	// this node — signed under a key it does not hold, or not signed by
	// the fleet at all ([Envelopes]) — so nothing in it was believed and
	// nothing ran. A node that holds the key is asked next.
	unservedUnauthenticated unservedReason = "unauthenticated"

	// unservedStale: the request is not one this node may take now — it
	// names no id or no deadline, its deadline had passed when it arrived,
	// or this node has already taken its id ([signedask.Guard]). Nothing ran.
	unservedStale unservedReason = "stale"
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

	// Unready is, for a GIT operation the node's git half could not answer
	// (code.ErrGitUnready, answered `behind`, or `behind_config` for that
	// reason), the reason it gave — which the walk reads to know whether
	// the one node it lets fill a copy is still that one
	// (code.UnreadyReason.PassesFill), since `behind` alone says the node
	// ran nothing and not whether a copy is coming there. Empty on every
	// other reply, and from a build that sends none: the walk then keeps its
	// filler, as it did before the reason crossed ([Router.routeGit]).
	Unready code.UnreadyReason `json:"unready,omitempty"`

	// Result is the operation's answer, and Err its failure. At most one.
	Result json.RawMessage `json:"result,omitempty"`
	Err    *wireError      `json:"error,omitempty"`

	// Obsolete names the streams whose floor this node could never reach —
	// a position on a generation the log has since abandoned — so the
	// asker drops them rather than carrying a floor nobody can satisfy.
	Obsolete []string `json:"obsolete,omitempty"`

	// Written is every work item the operation's writes COMMITTED to on
	// this node, for a request whose actor asked ([Actor.Records]) — what a
	// writer in the asking process would have reported into the turn's
	// [tracker.WriteLog] itself. Reported whatever the answer, an error
	// included, since a walking gesture's earlier steps committed whether
	// or not a later one failed.
	Written []types.WorkItem `json:"written,omitempty"`
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

	// domain is the domain whose COPY the operation reads — the verdict the
	// router and the server ask before running it here ([LocalBackends.For],
	// ADR-0049) — or [SeatCopy] for admission's ping. floorStream is that
	// domain's log, the one log its floors are on ([Router.floorsFor],
	// [ready]), or empty for an operation that carries no floor
	// ([op.floorless]): what it reads is not the log's rows at a position
	// (see [opWorkSearch]) or it asks about the copy itself ([opPing]). A
	// gate over the registry names every such operation.
	domain      string
	floorStream string

	// alsoReads is every OTHER domain whose rows the operation reads
	// ([op.alsoReads]): its verdict is asked too, and a copy of any of them
	// out of service runs none of it here. Its floor and its domain stay
	// the one above.
	alsoReads []string

	// copies is what the router keys where it STAYS and whom it passes
	// over by: the domain, and every domain the operation also reads —
	// since a node that serves one of them may not serve the other, and a
	// node that ran an operation over both copies is the one to ask for
	// that operation, not for either domain's alone ([Router.order]).
	copies string

	// actor says whether the operation acts AS somebody, which a request
	// then has to name.
	actor bool

	// ungated operations run on a node that is a worse one to ask ([worse]):
	// they ask the copy's own gate themselves ([opPing]).
	ungated bool

	// git is an operation asked of the GIT nodes ([op.keyedOnGit]): its
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

	// lane is the registration a serving node answers the operation on —
	// the shared one, the zero lane, unless it is declared on another
	// ([op.answeredOn]) — and the one subject the router asks it on.
	lane lane

	// serve is the server half: the request's arguments, decoded, run on
	// this node's backend.
	serve func(ctx context.Context, b Backend, actor *Actor, raw json.RawMessage) (any, error)
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

	// key names the node an operation PREFERS: the arguments' routing key —
	// a push's repository — over which the router orders every data node by
	// [rendezvous.Order], this one included ([Router.keyedOrder]). Nil for
	// every operation any data node answers equally, which is asked of this
	// node first and of the node that answered last. See [op.keyedBy].
	key func(A) string

	// home stamps a GIT operation's arguments with whether the node asked
	// is its walk's effective home ([op.keyedOnGit]), nil for every other.
	home func(A, bool) A

	// attempt is a git write's attempt on one git node FOR ITS ARGUMENTS
	// ([op.attemptBy]) — an engine write's, which follows its kind's build
	// (Timing.GitWriteAttemptFor) — and nil for every operation whose
	// attempt is its class's alone ([Router.wholeAttempt]).
	attempt func(A, Timing) time.Duration
}

// attemptBy declares a git write whose attempt on one git node follows its
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
// ONE TABLE FOR BOTH HALVES. The client asks an operation through the same
// value the server dispatches on, so an operation the client can send is by
// construction one the server knows — and the wire gate walks this table,
// so a new operation's types are checked the moment it is declared.
var registry = map[string]*opSpec{}

// define declares an operation, with the domain whose copy it reads — and
// whose log its floor is on, unless it is declared [op.floorless] — or
// [SeatCopy] for one that asks about the copy a seat is served by, or
// [GitCopy] for one a git node answers from its git half, which carries no
// floor at all ([op.keyedOnGit]). Package-level, at init: a duplicate name is
// a build that cannot serve, so it panics rather than silently shadowing, and
// so does a domain this package knows no log for, whose copy no verdict
// judges and whose floor no log holds.
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
	if domain != SeatCopy && domain != GitCopy {
		stream, known := floorStreams[domain]
		if !known {
			panic(fmt.Sprintf("estate: operation %q reads the %q domain's copy, "+
				"which is no domain this package knows", name, domain))
		}
		spec.floorStream = stream
	}
	decode := func(raw json.RawMessage) (A, error) {
		var args A
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &args); err != nil {
				return args, fmt.Errorf("estate: %s: decode the arguments: %w", name, err)
			}
		}
		return args, nil
	}
	checked := func(ctx context.Context, b Backend, who *Actor, args A) (R, error) {
		var zero R
		if spec.actor && (who == nil || who.Handle == "") {
			return zero, fmt.Errorf("estate: %s acts as somebody and the request "+
				"names nobody — a write whose author was not stated is not an "+
				"audit trail", name)
		}
		// THE HARD ARM, after the arguments are decoded and before anything
		// runs, on the in-process path and the wire's alike: a node behind
		// the company's configuration judges no grant from its chart,
		// whatever the asker said ([op.judgesGrant]).
		if spec.judges != nil && b.Current != nil && !b.Current() && spec.judges(args) {
			return zero, errDecidesNoGrant
		}
		return serve(ctx, b, who, args)
	}
	spec.serve = func(ctx context.Context, b Backend, who *Actor, raw json.RawMessage) (any, error) {
		args, err := decode(raw)
		if err != nil {
			return nil, err
		}
		return checked(ctx, b, who, args)
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

// alsoReads declares that this operation reads the rows of other domains as
// well as its own — a page's backlinks name the tasks that link to it — so
// the verdict of each is asked before it runs here, and a node whose copy of
// any of them is out of service answers `out_of_service` naming that copy,
// which sends the asker on to a node whose copies of all of them serve
// (ADR-0049). Routed on its own domain alone, it answered from a copy the node
// had stopped serving. Its FLOOR stays on its own domain's log: a second
// floor would hold it to an applier its answer does not wait on.
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
// the one the router asks it on — see [lane]. An operation whose answer waits
// on a queue of its own is answered on a lane of its own, so its slots are
// never the ones every other operation waits for. Package-level, at init.
func (o op[A, R]) answeredOn(l lane) op[A, R] {
	switch {
	case !slices.Contains(lanes, l) && !slices.Contains(gitLanes, l):
		panic(fmt.Sprintf("estate: %s is answered on %s, which no node serves", o.spec.name, l))
	case slices.Contains(gitLanes, l) != o.spec.git:
		// A GIT LANE ANSWERS GIT OPERATIONS AND NOTHING ELSE, since only a
		// node serving git registers one: a data operation declared on it
		// would be asked of a subject no data node without the git role
		// answers, and a git operation on another lane would hold a slot
		// that lane's answers wait for.
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

// repeatableWhen declares a once-write that is IDEMPOTENT for the calls whose
// arguments say so: a page write whose actor carries the caller's own
// operation key ([pages.Actor.OpKey]) — or a comment carrying its turn's
// ([pages.NewComment.TurnKey]) — derives its operation id, and a create its
// page's id, from that key and what the write says rather than minting one per
// call, so a repeat under it is the same operation and the ledger answers it
// with the first copy's outcome. Such a call fails over as an
// [opIdempotentWrite] does; one without a key is still never repeated
// ([opOnceWrite]). A property of the ASKING side, since the class is what the
// asker may do with an unanswered request, and the server runs the operation
// the same way either way.
func (o op[A, R]) repeatableWhen(keyed func(A) bool) op[A, R] {
	if o.spec.class != opOnceWrite {
		panic(fmt.Sprintf("estate: %s is not a once-write, so nothing about its "+
			"arguments can make it repeatable", o.spec.name))
	}
	o.repeatable = keyed
	return o
}

// keyedBy declares that the operation is asked of the data nodes in the
// rendezvous order of key(args) rather than this node first — see
// [Router.route]'s keyed walk and the package doc's "A push is asked of its
// repository's committer". The key travels with the call, never on the wire:
// it is a property of the ASKING side, like [op.repeatableWhen], since only
// the asker chooses whom to ask, and a serving node runs the operation the
// same way whoever chose it. Arguments that name no key are refused before
// any node is asked, because "no node preferred" would put one repository's
// pushes on as many committers as there are askers.
func (o op[A, R]) keyedBy(key func(A) string) op[A, R] {
	o.key = key
	return o
}

// keyedOnGit declares an operation on a repository's GIT OBJECTS, asked of the
// git nodes rather than the data nodes (ADR-0059): in [rendezvous.Order] of
// key(args) over every live git node — the repository's HOME first, its
// runner-up next — with a node that ran nothing for it asked last for
// [suspectFor] and one that went silent last of all, no sticky node, and this
// node at its place, in process, where it is a git node ([Router.routeGit]).
//
// home stamps each attempt's arguments with whether the node asked is the one
// node of the walk that may FILL a copy it does not hold: the walk's
// EFFECTIVE HOME — the first node of the order once its demotions are applied
// — and in its place the next node only where that one said no copy serving
// the read is coming there ([Router.routeGit]). Every other git node holding
// none answers at once that it holds none, so a read of a repository nobody
// holds costs the fleet one download at a time rather than one a git node.
// The walk calls it on every attempt, over whatever the caller set, so no
// caller chooses it.
//
// The operation must be defined on [GitCopy], which it reads instead of a
// domain's copy: no copy verdict holds it back, no floor is waited for at
// dispatch — its floor travels in its arguments, as code.GitQuery.Fresh, and
// is the git node's cache's sync level — and no lag gate applies, since a git
// node syncs its copy through the router from whichever data node serves.
// Package-level, at init, before [op.answeredOn] names its lane.
func (o op[A, R]) keyedOnGit(key func(A) string, home func(A, bool) A) op[A, R] {
	if o.spec.domain != GitCopy {
		panic(fmt.Sprintf("estate: %s is keyed on the git nodes, and reads the %q copy rather than %q",
			o.spec.name, o.spec.domain, GitCopy))
	}
	o.spec.git = true
	o.key = key
	o.home = home
	return o
}

// judgesGrant declares an operation that JUDGES A GRANT from the serving
// node's chart wherever pred holds of its decoded arguments — a read whose
// asker is a seat, whose grant on a repository is the chart's (ADR-0058). On
// a node CONFIRMED behind the company's configuration ([Backend.Current]
// false: shed or stuck, never lag alone) such an operation is answered
// [errDecidesNoGrant] before it runs — `behind_config`, saying it decides no
// grant ([decidesNoGrant]) — EVEN WHEN IT IS ASKED LAST, told to answer
// anyway: a revision the node failed to apply may be the one that revoked the
// grant, and being asked last makes no answer judged by the chart the fleet
// replaced sound. So the soft arm sends the walk to every current data node
// first, the last resort asks this node again, it refuses again, and the walk
// ends [ErrUnserved] — the safe answer where the only data node may be
// judging by a revoked grant.
//
// A DECLARATION, NOT A LIST: every operation on a domain's copy whose
// arguments carry a code.Principal named Asker must declare whether it judges
// one, which TestEveryOperationThatJudgesAGrantIsDeclared holds over the
// registry. A git operation is outside it: its node judges the grant and
// refuses a seat itself when behind its configuration (code.ErrGitUnready,
// `behind_config`), and the walk moves on from that as from any node that ran
// nothing.
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
