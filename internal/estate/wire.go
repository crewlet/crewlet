package estate

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// SubjectPrefix prefixes every serving node's subject.
const SubjectPrefix = "crewlet.estate"

// Subject is the one subject a serving node answers on.
//
// THE NODE ID IS ESCAPED as a subject token, because a node id may carry a dot
// (`node.id` admits one), and a raw dot would split it into two tokens — a
// subject some other node's escaped id could collide with.
func Subject(nodeID string) string {
	return SubjectPrefix + "." + coord.DocumentKey(nodeID)
}

// Actor is who a write is attributed to, as it crosses the wire.
//
// The tracker's rule is that a writer acts as exactly one party, derived from
// an identity the caller cannot choose per call. On a stateless node that
// identity is the seat its tool surface bound; the serving node derives its
// writer from what arrives here and from nothing else, so the history row
// names the same author either node would have written.
type Actor struct {
	Handle     string             `json:"handle"`
	Kind       tracker.AuthorKind `json:"kind"`
	Provenance tracker.Provenance `json:"provenance"`
}

// opClass is what a failover may do with an operation that went unanswered.
//
// EVERY CLASS MOVES ON FROM A NODE THAT RAN NOTHING — one that answered it does
// not serve the partition, has no backend for it, is not serving yet or is
// behind the caller's floor, and a write refused by the write authority's gate
// 3 (`not_holder`, `holding_unknown`), which appends nothing. What differs is
// what may be repeated once a node MAY have run it.
type opClass int

const (
	// opRead runs on any serving holder of its partition and changes
	// nothing: an unanswered one is asked of the next.
	opRead opClass = iota

	// opIdempotentWrite carries an operation id the caller minted once, and
	// the ledger answers a repeat with the first copy's result — so an
	// unanswered one is asked of the next serving holder under the same id.
	// So is one a holder answered `unvouched` ([statelog.Result.Unvouched]):
	// that holder's ledger cannot say whether the operation landed, and
	// another holder's may, so the answer is not final until every holder
	// has been asked.
	opIdempotentWrite

	// opOnceWrite has no id a caller holds, so a repeat is a second write.
	// An unanswered one is reported as [ErrOutcomeUnknown] and never asked
	// again.
	opOnceWrite

	// opGatherRead addresses SEVERAL partitions and is answered per
	// partition as [opRead] is — each by any serving holder of it — with
	// one rule of its own: a partition is never asked again of a holder
	// that already failed it in this gather. See gather.go.
	//
	// A slice asked at `linearizable` is the BARRIER half of a gather: its
	// holder appends a barrier on the partition's log of the operation's
	// own domain, applies through it, and answers at or after it — the cut
	// it established. It fails over exactly as any slice does, so it is the
	// same class asked at a level ([request.Level]), not a class of its
	// own: a class is what a failover may do, and nothing differs.
	opGatherRead
)

// request is one operation, as the asking node sends it.
type request struct {
	Op    string          `json:"op"`
	Args  json.RawMessage `json:"args,omitempty"`
	Actor *Actor          `json:"actor,omitempty"`

	// Partitions are the partitions this request addresses AT THIS NODE —
	// the one a single-partition operation resolved to, by the asker's
	// layout. Empty for an operation that addresses none ([opSpec.partitions]
	// nil), and on a request from a build that predates partitions, which
	// the serving node resolves by its own layout instead.
	Partitions []string `json:"partitions,omitempty"`

	// MapEpoch is the estate map's epoch the asker routed by, 0 where there
	// is no map (layout 0). It is what a `not_holder` is weighed against on
	// the asking side, and the serving node names it in its refusal.
	MapEpoch uint64 `json:"map_epoch,omitempty"`

	// Floors are the positions the serving node must have applied before it
	// runs the operation — the asking node's session, on the logs of the
	// partition asked for. See the package doc.
	Floors []statelog.Position `json:"floors,omitempty"`

	// Deadline is the asker's, so the serving node stops working on an
	// answer nobody is waiting for.
	Deadline time.Time `json:"deadline,omitzero"`

	// From is the asking node, for the serving node's log.
	From string `json:"from,omitempty"`

	// AcceptLagging is the asker's LAST RESORT: every holder whose copy does
	// not lag its logs has run nothing, so this one runs the operation
	// although its copy does ([unservedLagging]) — held to the floors above
	// and to the read's own level, which are what make its answer sound. An
	// older build ignores it and refuses again, which is the answer it gave
	// before.
	AcceptLagging bool `json:"accept_lagging,omitempty"`

	// Slices asks a gather operation for each of Partitions' own answer
	// ([reply.Parts]) rather than the operation's whole one — the asker
	// holds every partition's and merges them. A gather that addresses ONE
	// partition is a single-partition read and never sets it, which is
	// also what an older build's asker sends: the serving node answers
	// such a request whole, exactly as before gathers.
	Slices bool `json:"slices,omitempty"`

	// Level is the level each partition of a gather slice is read at
	// ([statelog.GatherLevel]), set where the operation reads a log at a
	// level and empty where it does not.
	Level statelog.ReadLevel `json:"level,omitempty"`

	// Cursors are a paged gather's per-partition cursors, by partition id:
	// each partition resumes from its own, which only it can read.
	Cursors map[string]string `json:"cursors,omitempty"`
}

// unservedReason is why a node answered without running an operation.
type unservedReason string

const (
	// unservedNoBackend: this node runs no native backend for the op's
	// half — the company is on a vendor for it, or the runtime is not up.
	unservedNoBackend unservedReason = "no_backend"

	// unservedNotEstablished: this node's copy admits no seat yet — the
	// strict gate admission's ping asks ([Backend.Admits]).
	unservedNotEstablished unservedReason = "not_established"

	// unservedLagging: this node's copy lags its logs ([Backend.Answers]
	// false) — not drained since its appliers started, or past the snapshot
	// slack of their ends. A worse holder rather than none: the asker comes
	// back to it with [request.AcceptLagging] when no holder whose copy does
	// not lag runs the operation.
	unservedLagging unservedReason = "lagging"

	// unservedBehind: this node could not reach the caller's floor within
	// the read budget.
	unservedBehind unservedReason = "behind"

	// unservedNotHolder: this node does not serve the partition asked for —
	// it never held it, has begun to leave it, has not finished joining it,
	// or has stopped serving it on a fault. The reply carries the node's own
	// map epoch ([reply.Epoch]), which is what tells the asker whether ITS
	// view or the server's is the stale one.
	unservedNotHolder unservedReason = "not_holder"

	// unservedHoldingUnknown: this node cannot tell whether it serves the
	// partition — the answer gate 3 reads could not be read. Not
	// `not_holder`: it carries no epoch, since the asker's map is not what
	// is in question, and the asker moves on without reading its map again.
	unservedHoldingUnknown unservedReason = "holding_unknown"

	// unservedOverflow: a gather slice this node ANSWERED and could not
	// fit in the reply beside the slices before it — the reply has a
	// ceiling ([queue.MaxPayloadBytes]) and a batch of partitions can
	// outgrow it. Not a failure of this node, so the asker asks it again
	// for the partitions that overflowed; a slice that does not fit even
	// alone is answered as an error naming its size instead, so every
	// reply answers at least one partition and the batch always shrinks.
	unservedOverflow unservedReason = "overflow"
)

// reply is what a serving node answers.
type reply struct {
	// Node is who answered.
	Node string `json:"node"`

	// Unserved is set when the node did NOT run the operation, and says
	// why. Nothing was executed, so every class may move on.
	Unserved unservedReason `json:"unserved,omitempty"`
	Detail   string         `json:"detail,omitempty"`

	// Epoch is the SERVER's estate-map epoch, set with a `not_holder`: newer
	// than the asker's view says the asker is routing by an old map, and
	// not newer says the server is the one behind (a joiner not yet
	// serving) or on its way out.
	Epoch uint64 `json:"epoch,omitempty"`

	// Result is the operation's answer, and Err its failure. At most one.
	Result json.RawMessage `json:"result,omitempty"`
	Err    *wireError      `json:"error,omitempty"`

	// Obsolete names the streams whose floor this node could never reach —
	// a position on a generation the log has since abandoned — so the
	// asker drops them rather than carrying a floor nobody can satisfy.
	Obsolete []string `json:"obsolete,omitempty"`

	// At is where each of the partition's logs was when a read whose
	// answer reports its coverage began ([statelog.Coverage.At]) — a lower
	// bound on what the answer holds. Empty for every other operation, and
	// from an older build, whose answer then states no cut.
	At []statelog.Position `json:"at,omitempty"`

	// Parts is a gather's answer per partition, one per partition the
	// request named, when it asked for [request.Slices].
	Parts []partReply `json:"parts,omitempty"`
}

// partReply is one partition's answer within a gather batch: what [reply]
// says about a whole request, said about one of its partitions.
type partReply struct {
	Partition string `json:"partition"`

	// Unserved is set when this node did not run the read for this
	// partition — the same reasons a whole request is refused for, and
	// [unservedOverflow] — and says why. Epoch is this node's map epoch,
	// set with `not_holder`.
	Unserved unservedReason `json:"unserved,omitempty"`
	Epoch    uint64         `json:"epoch,omitempty"`
	Detail   string         `json:"detail,omitempty"`

	// Result is the partition's answer and Err its failure. At most one.
	Result json.RawMessage `json:"result,omitempty"`
	Err    *wireError      `json:"error,omitempty"`

	// At is where each of the partition's logs was when its read began —
	// or the barriers it was read after, at `linearizable`. Empty for a
	// read that is not of a log's rows.
	At []statelog.Position `json:"at,omitempty"`
}

// opSpec is one operation's declaration: what it is called, how a failover
// treats it, which partitions it addresses, and its server half.
type opSpec struct {
	name   string
	class  opClass
	args   reflect.Type
	result reflect.Type

	// partitions resolves a request's arguments, decoded, to the partitions
	// it addresses under a layout — one for a single-partition operation,
	// every one a gather reads for a gather — or is nil for an operation
	// that addresses none (the node's own event log), which any data node
	// answers. A floor's stream is derived from the partition's log of the
	// operation's domain, so an operation names no stream of its own.
	partitions func(ctx context.Context, l statelog.Layout, r Resolver,
		raw json.RawMessage) ([]statelog.PartitionID, error)

	// domain is the domain whose log the operation depends on in each
	// partition it addresses: the only log its floors are on
	// ([opSpec.floorStreams], [address]).
	domain string

	// actor says whether the operation acts AS somebody, which a request
	// then has to name.
	actor bool

	// ungated operations run on a node whose copy is not serving yet: they
	// touch only what the node keeps for itself, or ask the gate
	// themselves.
	ungated bool

	// floorless operations carry no session floor, because what they read
	// is not the log's rows at a position — see [opWorkSearch].
	floorless bool

	// covered operations' answers report what they covered
	// ([statelog.Coverage]): the partitions they addressed and the cut each
	// was read at, which the serving node measures before it runs the read
	// ([reply.At]). Every gather is; a single-partition read whose answer
	// a gather will one day assemble is declared so ([op.covered]).
	covered bool

	// serve runs a single-partition operation on p — the zero partition
	// for one that addresses none. Nil for a gather, which [opSpec.whole]
	// answers instead.
	serve func(ctx context.Context, b Backend, p statelog.PartitionID, actor *Actor,
		raw json.RawMessage) (any, error)

	// part is a gather's per-partition answer type; slice is its server
	// half for one partition of several — that partition's answer before
	// any merge, with the positions its logs were at ([partReply.At]) —
	// and whole is its answer when it addresses ONE partition, which is a
	// single-partition read: the slice and the merge of that one slice.
	// All three nil for every other operation.
	part  reflect.Type
	slice func(ctx context.Context, b Backend, s sliceAsk, raw json.RawMessage) (
		any, []statelog.Position, error)
	whole func(ctx context.Context, b Backend, s sliceAsk, raw json.RawMessage) (
		any, []statelog.Position, error)
}

// sliceAsk is what one partition's slice of a gather is asked under.
type sliceAsk struct {
	partition statelog.PartitionID
	layout    statelog.Layout

	// level is the gather's per-partition level, empty for an operation
	// that reads no log at one; floors are the asker's on the partition's
	// logs, already waited for.
	level  statelog.ReadLevel
	floors []statelog.Position

	// cursor is this partition's own cursor in a paged gather.
	cursor string
}

// op is a typed handle on one registered operation: its declaration, and its
// two halves in the caller's own types, which the router calls in-process
// where this node serves the partition — no encoding, and an error keeps its
// own identity rather than the wire's rebuilt one.
type op[A, R any] struct {
	spec  *opSpec
	serve func(ctx context.Context, b Backend, actor *Actor, args A) (R, error)
	addr  address[A]

	// cover sets what the answer covered on it, for an operation declared
	// [op.covered]; nil otherwise.
	cover func(*R, statelog.Coverage)
}

// partitionsFunc resolves an operation's arguments to the partitions it
// addresses under a layout. It may READ through the resolver first — a bare
// id resolving to the partition that holds it — and says so by calling it.
type partitionsFunc[A any] func(ctx context.Context, l statelog.Layout, r Resolver,
	args A) ([]statelog.PartitionID, error)

// registry is every operation this build serves, keyed by name.
//
// ONE TABLE FOR BOTH HALVES. The client asks an operation through the same
// value the server dispatches on, so an operation the client can send is by
// construction one the server knows — and the wire gate walks this table,
// so a new operation's types are checked the moment it is declared.
var registry = map[string]*opSpec{}

// define declares an operation. Package-level, at init: a duplicate name is a
// build that cannot serve, so it panics rather than silently shadowing. The
// zero address is an operation that addresses no partition
// ([opSpec.partitions]).
func define[A, R any](name string, class opClass, at address[A], actor bool,
	serve func(ctx context.Context, b Backend, actor *Actor, args A) (R, error),
) op[A, R] {
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("estate: operation %q declared twice", name))
	}
	spec := &opSpec{
		name: name, class: class, actor: actor, domain: at.domain,
		args: reflect.TypeFor[A](), result: reflect.TypeFor[R](),
	}
	partitions := at.partitions
	decode := func(raw json.RawMessage) (A, error) {
		var args A
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &args); err != nil {
				return args, fmt.Errorf("estate: %s: decode the arguments: %w", name, err)
			}
		}
		return args, nil
	}
	if partitions != nil {
		spec.partitions = func(ctx context.Context, l statelog.Layout, r Resolver,
			raw json.RawMessage) ([]statelog.PartitionID, error) {
			args, err := decode(raw)
			if err != nil {
				return nil, err
			}
			return partitions(ctx, l, r, args)
		}
	}
	checked := func(ctx context.Context, b Backend, who *Actor, args A) (R, error) {
		if spec.actor && (who == nil || who.Handle == "") {
			var zero R
			return zero, fmt.Errorf("estate: %s acts as somebody and the request "+
				"names nobody — a write whose author was not stated is not an "+
				"audit trail", name)
		}
		return serve(ctx, b, who, args)
	}
	spec.serve = func(ctx context.Context, b Backend, _ statelog.PartitionID, who *Actor,
		raw json.RawMessage) (any, error) {
		args, err := decode(raw)
		if err != nil {
			return nil, err
		}
		return checked(ctx, b, who, args)
	}
	registry[name] = spec
	return op[A, R]{spec: spec, serve: checked, addr: at}
}

// ungated declares that the establishment gate does not hold this operation
// back — see [opSpec.ungated].
func (o op[A, R]) ungated() op[A, R] {
	o.spec.ungated = true
	return o
}

// floorless declares that this operation carries no session floor — see
// [opSpec.floorless].
func (o op[A, R]) floorless() op[A, R] {
	o.spec.floorless = true
	return o
}

// covered declares that this operation's answer reports what it covered, and
// how: cover sets the coverage on the answer the router hands its caller —
// see [opSpec.covered].
func (o op[A, R]) covered(cover func(*R, statelog.Coverage)) op[A, R] {
	o.spec.covered = true
	o.cover = cover
	return o
}
