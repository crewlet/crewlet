package builtin

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools/argval"
)

// operations is how a write tool's call names its OPERATION on the one surface
// whose callers have neither a turn nor a transport's key: the operator's
// assistant over MCP (package doc, "How a write is made once"). Every write
// family holds this one value, as a copy drifts on a check, not just a word;
// each keeps its derivation ([opIDFor] over [callOpKey]), a namespace fixed
// for the format's life.
//
// There each call IS an operation, minted and answered as `op_id`, or brought
// back for that same call ([callName]) under [statelog.CheckCallerOpID], so a
// repeat after an `unknown` is the same operation, never a second item. A call
// whose turn or request key (`Idempotency-Key`) names it is refused an `op_id`
// (a model could name another write's), not ignored, as if a promise kept.
type operations struct {
	// callerOperations is whether a caller with no turn holds each call's
	// operation id: every write answers it as `op_id`, and a call bringing it
	// back is that operation again.
	//
	// UNEXPORTED AND SET BY [OperatorTools] ALONE: a fact about the SURFACE,
	// not a setting. A seat's registry that took an id from its arguments
	// would let a model name another write's operation, answered as that write.
	callerOperations bool

	// now is the clock an operation is minted at; nil is the wall clock, which
	// [statelog.NewOpID] needs because the instant is compared with the
	// ledger's wall-clock watermark. The artifacts' writes hand theirs on
	// ([ArtifactDeps.Now], nil on a running node); the tracker's do not, as
	// [WorkDeps.Now] is the calendar a query's relative dates resolve against.
	now func() time.Time
}

// bind settles which operation a call is: one minted for it, or the `op_id` it
// brought back for exactly this call; none ("") where seeded (the call's turn
// or request key names it) or where this surface takes no `op_id`; or the
// refusal it is answered with, before anything is written.
//
// AN EMPTY `op_id` IS NONE: a model filling an optional field writes one.
func (o operations) bind(tool string, args map[string]any, seeded bool) (op, refusal string) {
	raw, brought := args[opIDArg]
	if text, _ := argval.String(raw); text == "" {
		brought = false
	}
	if !o.callerOperations || seeded {
		if brought {
			return "", fmt.Sprintf("%s takes no `op_id` here: these writes "+
				"derive their operation from the turn or the request that "+
				"carries them, so the same call made again is already the "+
				"same operation. Leave it out.", tool)
		}
		return "", ""
	}
	call := callName(tool, args)
	if !brought {
		return statelog.NewOpID(o.mintedAt(), call), ""
	}
	held, _ := argval.String(raw)
	if err := statelog.CheckCallerOpID(held); err != nil {
		return "", fmt.Sprintf("%s refused that `op_id`: %v", tool, err)
	}
	if !statelog.OpNamed(held, call) {
		return "", fmt.Sprintf("%s refused that `op_id`, and nothing was "+
			"written: it was answered for another call — another tool, or "+
			"this one with other arguments — and an op_id finishes only the "+
			"call it was answered for. To finish that call, send its "+
			"arguments unchanged with it; to make this one, leave `op_id` out.",
			tool)
	}
	return held, ""
}

// bindActor is [operations.bind] for actor's call, seeded wherever the actor
// carries the identity a derived id is built from ([Actor.OperationSeed]). It
// sets [Actor.Operation], which every write of the call derives its id from
// ([opIDFor], [commentID]), or returns the refusal. The tracker's and Gitlet's
// families both call it, so the seed is read one way for both.
func (o operations) bindActor(actor Actor, tool string, args map[string]any) (Actor, string) {
	op, refusal := o.bind(tool, args, actor.OperationSeed() != "")
	actor.Operation = op
	return actor, refusal
}

// mintedAt is the instant an operation is minted at — see [operations.now].
func (o operations) mintedAt() time.Time {
	if o.now == nil {
		return time.Now()
	}
	return o.now()
}

// param offers `op_id` in a write tool's schema on the surface whose callers
// bring one back, and nowhere else: described once for every family.
func (o operations) param(schema map[string]any) map[string]any {
	if !o.callerOperations {
		return schema
	}
	props, _ := schema["properties"].(map[string]any)
	props[opIDArg] = map[string]any{
		"type": "string",
		"description": "Leave it out for a new write. To finish one an earlier " +
			"answer from this tool left `unknown` or part of the way through, " +
			"send back the `op_id` that answer carried, unchanged, with exactly " +
			"the same arguments: the call is then that same operation, and is " +
			"answered with what landed. An op_id belongs to that one call: " +
			"sent with this tool and any other argument, or with another " +
			"tool, it is refused before anything is written.",
	}
	return schema
}

// opIDArg is the argument a caller on the operator's surface brings an
// operation back in, and the field every answer there carries it in.
const opIDArg = "op_id"

// callName is the name an operator's operation id carries: the tool and a
// digest of the call's arguments, `op_id` left out since the first call had none.
//
// THE ARGUMENTS ARE IN IT because an `op_id` promises nothing is made twice:
// steps are named by the objects written ([opIDFor]), so the id with other
// arguments would file a second item, or land unmatched steps as writes nothing
// authored. Refused whole before the first append, the id is that call or nothing.
//
// A DIGEST because an id is at most [statelog.MaxCallerOpIDBytes] in every row
// it writes; 16 hex digits of [turnctx.ArgsDigest] catch an accidental change,
// all it is for (a caller can mint any id: no credential). JOINED BY A DASH: a
// dot begins a step, and escaping it would hide the tool from a ledger reader.
func callName(tool string, args map[string]any) string {
	rest := make(map[string]any, len(args))
	for key, value := range args {
		if key != opIDArg {
			rest[key] = value
		}
	}
	return tool + "-" + turnctx.ArgsDigest(rest)[:callDigestLen]
}

// callDigestLen is how many hex digits of the arguments' digest an operator's
// operation id carries: 64 bits, far past any accidental collision, in an id
// of at most 76 bytes with the longest tool of any family that offers one
// (create_artifact_folder: 36 + 1 + 22 + 1 + 16) — well inside
// [statelog.MaxCallerOpIDBytes], and a name well inside one segment
// ([statelog.MaxOpSegmentBytes]).
const callDigestLen = 16
