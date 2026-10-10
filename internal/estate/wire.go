package estate

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
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

// request is one operation, as the asking node sends it.
type request struct {
	Op    string          `json:"op"`
	Args  json.RawMessage `json:"args,omitempty"`
	Actor *Actor          `json:"actor,omitempty"`

	// Floors are the positions the serving node must have applied before it
	// runs the operation — the asking node's session, on the log of the
	// operation's own domain. See the package doc.
	Floors []statelog.Position `json:"floors,omitempty"`

	// Deadline is the asker's, so the serving node stops working on an
	// answer nobody is waiting for.
	Deadline time.Time `json:"deadline,omitzero"`

	// From is the asking node, for the serving node's log.
	From string `json:"from,omitempty"`

	// AcceptLagging is the asker's LAST RESORT: every data node whose copy
	// does not lag its logs has run nothing, so this one runs the operation
	// although its copy does ([unservedLagging]) — held to the floors above
	// and to the read's own level, which are what make its answer sound.
	AcceptLagging bool `json:"accept_lagging,omitempty"`
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
	// slack of their ends. A worse node to ask rather than none: the asker
	// comes back to it with [request.AcceptLagging] when no data node whose
	// copy does not lag runs the operation.
	unservedLagging unservedReason = "lagging"

	// unservedBehind: this node could not reach the caller's floor within
	// the read budget.
	unservedBehind unservedReason = "behind"

	// unservedOutOfService: this data node's copy of the operation's domain
	// is out of service — WRONG rather than behind (an applier halted, the
	// node evicted, its rows below the log), so it answers none of that
	// domain's operations until the copy recovers, while every other
	// domain's copy keeps serving and its own seats' calls on this one go to
	// the other data nodes ([LocalBackends.For], ADR-0031).
	unservedOutOfService unservedReason = "out_of_service"
)

// outOfService says which copy of this node's is out of service, in the words
// a walk's note and a peer's reply carry.
func outOfService(domain string) string {
	if domain == SeatCopy {
		return "its copy of a domain seat admission depends on is out of service — " +
			"wrong rather than behind"
	}
	return "its copy of " + domain + " is out of service — wrong rather than behind, " +
		"so it answers none of " + domain + "'s operations until it recovers"
}

// reply is what a serving node answers.
type reply struct {
	// Node is who answered.
	Node string `json:"node"`

	// Unserved is set when the node did NOT run the operation, and says
	// why. Nothing was executed, so every class may move on.
	Unserved unservedReason `json:"unserved,omitempty"`
	Detail   string         `json:"detail,omitempty"`

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

// opSpec is one operation's declaration: what it is called, how a failover
// treats it, which log its floor is on, and its server half.
type opSpec struct {
	name   string
	class  opClass
	args   reflect.Type
	result reflect.Type

	// domain is the domain whose COPY the operation reads — the verdict the
	// router and the server ask before running it here ([LocalBackends.For],
	// ADR-0031) — or [SeatCopy] for admission's ping. floorStream is that
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

	// ungated operations run on a node whose copy lags its logs: they ask
	// the copy's own gate themselves ([opPing]).
	ungated bool

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
// [SeatCopy] for one that asks about the copy a seat is served by.
// Package-level, at init: a duplicate name is a build that cannot serve, so it
// panics rather than silently shadowing, and so does a domain this package
// knows no log for, whose copy no verdict judges and whose floor no log holds.
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
	if domain != SeatCopy {
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
		if spec.actor && (who == nil || who.Handle == "") {
			var zero R
			return zero, fmt.Errorf("estate: %s acts as somebody and the request "+
				"names nobody — a write whose author was not stated is not an "+
				"audit trail", name)
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
// (ADR-0031). Routed on its own domain alone, it answered from a copy the node
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

// ungated declares that the serving gate does not hold this operation back —
// see [opSpec.ungated].
func (o op[A, R]) ungated() op[A, R] {
	o.spec.ungated = true
	return o
}

// repeatableWhen declares a once-write that is IDEMPOTENT for the calls whose
// arguments say so: a page write that carries the caller's own key
// ([pages.CallKey]) derives its operation id — and a create its page's id —
// from that key rather than minting one per call, so a repeat under it is the
// same operation and the ledger answers it with the first copy's outcome. Such
// a call fails over as an [opIdempotentWrite] does; one without a key is still
// never repeated ([opOnceWrite]). A property of the ASKING side, since the
// class is what the asker may do with an unanswered request, and the server
// runs the operation the same way either way.
func (o op[A, R]) repeatableWhen(keyed func(A) bool) op[A, R] {
	if o.spec.class != opOnceWrite {
		panic(fmt.Sprintf("estate: %s is not a once-write, so nothing about its "+
			"arguments can make it repeatable", o.spec.name))
	}
	o.repeatable = keyed
	return o
}
