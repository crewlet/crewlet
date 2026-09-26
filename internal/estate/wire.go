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
type opClass int

const (
	// opRead runs anywhere and changes nothing: an unanswered one is asked
	// of the next node.
	opRead opClass = iota

	// opIdempotentWrite carries an operation id the caller minted once, and
	// the ledger answers a repeat with the first copy's result — so an
	// unanswered one is asked of the next node under the same id.
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
	// runs the operation — the asking node's session. See the package doc.
	Floors []statelog.Position `json:"floors,omitempty"`

	// Deadline is the asker's, so the serving node stops working on an
	// answer nobody is waiting for.
	Deadline time.Time `json:"deadline,omitzero"`

	// From is the asking node, for the serving node's log.
	From string `json:"from,omitempty"`
}

// unservedReason is why a node answered without running an operation.
type unservedReason string

const (
	// unservedNoBackend: this node runs no native backend for the op's
	// half — the company is on a vendor for it, or the runtime is not up.
	unservedNoBackend unservedReason = "no_backend"

	// unservedNotEstablished: this node's copy is not one a seat's tools
	// may read yet — the same gate that withholds seats from it.
	unservedNotEstablished unservedReason = "not_established"

	// unservedBehind: this node could not reach the caller's floor within
	// the read budget.
	unservedBehind unservedReason = "behind"
)

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
}

// opSpec is one operation's declaration: what it is called, how a failover
// treats it, which stream's floor applies, and its server half.
type opSpec struct {
	name   string
	class  opClass
	stream string
	args   reflect.Type
	result reflect.Type

	// actor says whether the operation acts AS somebody, which a request
	// then has to name.
	actor bool

	// ungated operations run on a node whose replicated estate is not
	// established: they touch only what the node keeps for itself.
	ungated bool

	serve func(ctx context.Context, b Backend, actor *Actor, raw json.RawMessage) (any, error)
}

// op is a typed handle on one registered operation.
type op[A, R any] struct{ spec *opSpec }

// registry is every operation this build serves, keyed by name.
//
// ONE TABLE FOR BOTH HALVES. The client asks an operation through the same
// value the server dispatches on, so an operation the client can send is by
// construction one the server knows — and the wire gate walks this table,
// so a new operation's types are checked the moment it is declared.
var registry = map[string]*opSpec{}

// define declares an operation. Package-level, at init: a duplicate name is a
// build that cannot serve, so it panics rather than silently shadowing.
func define[A, R any](name string, class opClass, stream string, actor bool,
	serve func(ctx context.Context, b Backend, actor *Actor, args A) (R, error),
) op[A, R] {
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("estate: operation %q declared twice", name))
	}
	spec := &opSpec{
		name: name, class: class, stream: stream, actor: actor,
		args: reflect.TypeFor[A](), result: reflect.TypeFor[R](),
	}
	spec.serve = func(ctx context.Context, b Backend, who *Actor, raw json.RawMessage) (any, error) {
		var args A
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &args); err != nil {
				return nil, fmt.Errorf("estate: %s: decode the arguments: %w", name, err)
			}
		}
		if spec.actor && (who == nil || who.Handle == "") {
			return nil, fmt.Errorf("estate: %s acts as somebody and the request "+
				"names nobody — a write whose author was not stated is not an "+
				"audit trail", name)
		}
		return serve(ctx, b, who, args)
	}
	registry[name] = spec
	return op[A, R]{spec: spec}
}

// defineUngated declares an operation the establishment gate does not hold
// back — see [opSpec.ungated].
func defineUngated[A, R any](name string, class opClass, stream string, actor bool,
	serve func(ctx context.Context, b Backend, actor *Actor, args A) (R, error),
) op[A, R] {
	o := define(name, class, stream, actor, serve)
	o.spec.ungated = true
	return o
}
