package builtin

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools/argval"
)

// operations is how a write tool's call names its OPERATION on the one surface
// whose callers have neither a turn nor a transport's key to name it by — the
// operator's assistant over MCP — and why a call that has either is refused an
// `op_id`. The package doc's "How a write is made once" is the identity every
// write derives from; this is the part of it a caller can bring.
//
// # One value, because every family of write tools takes one rule
//
// The tracker's writes, the artifacts' and the datastore's each offer `op_id`
// on that surface and refuse it everywhere else, and two of them used to keep a
// copy of this rule each: the artifacts' copy had already reworded every
// refusal it gave and the argument's own description, and a copy is where the
// next drift is a CHECK rather than a word — one that took a brought-back
// `op_id` without holding it to its call would run another call under an
// operation promised to make nothing twice. So the families hold this value,
// and what stays each family's own is only what a turn's call or a request
// DERIVES from what it carries ([opIDFor] over [callOpKey], the one derivation
// a turn's call takes), because each derivation's namespace is fixed for the
// life of the format.
//
// # Why a caller with no turn needs one at all
//
// A seat's writes derive their ids from its turn, so the same call made again
// — a re-run, or a repeat after an `unknown` — is the same operation, and the
// ledger answers what landed. The operator's assistant has no turn: every call
// minted every id afresh, so the one thing an `unknown` or a gesture stopped
// part of the way through asks for — the same operation again — was
// unreachable. A create repeated filed a second item, and an update
// conditioned on a version was refused as stale by its own first copy.
//
// So each call there IS an operation: one minted for it and answered as
// `op_id`, or the one the caller brought back, held to the rule every surface
// that takes one holds it to ([statelog.CheckCallerOpID]). Every write the
// call makes derives from it.
//
// # Why a brought-back id must come with its own call
//
// The id is minted NAMING the call — the tool and a digest of its arguments
// ([callName]) — and brought back it is accepted only with that call. With any
// other it is refused before a single write: see [callName] for the second
// items and the half-applied gestures a looser rule let through.
//
// # Why a call that has a turn or a key is refused one
//
// Because the turn is already the identity, and an id a model could name
// would be answered as whichever write it named, whatever this call asked for.
// Refused rather than ignored: ignored, it reads as a promise the call keeps.
// The same holds for a request carrying its own operation key — the act
// transport's and the HTTP write surface's `Idempotency-Key` — which is its
// identity exactly as a turn is.
type operations struct {
	// callerOperations is whether a caller with no turn holds the id of the
	// operation each call is: every write answers with it as `op_id`, and a
	// call that brings it back is that operation again rather than a new
	// one.
	//
	// UNEXPORTED AND SET BY [OperatorTools] ALONE, for every family it
	// builds, because it is a fact about the SURFACE rather than a setting:
	// the operator's MCP is the one caller with no turn to derive an
	// operation from, and a seat's registry that took an id from its
	// arguments would let a model name another write's operation — answered
	// as that write, whatever it asked for.
	callerOperations bool

	// now is the clock an operation is minted at, and nil is the wall clock,
	// which [statelog.NewOpID] asks of the node minting the id: the instant is
	// compared with the ledger's watermark, read off the wall. The artifacts'
	// writes hand theirs on ([ArtifactDeps.Now], nil on a running node and
	// pinned by their cases); the tracker's do not, because [WorkDeps.Now]
	// is the calendar a query's relative dates resolve against, which no
	// operation is minted on.
	now func() time.Time
}

// bind settles which operation a call is: the one minted for it, or the
// `op_id` it brought back for exactly this call; none ("") where seeded — the
// call's turn or its request's own key already names it — or where this
// surface takes no `op_id`; or the refusal it is answered with instead, before
// anything is written.
//
// AN EMPTY `op_id` IS NONE — it names no operation, and a model filling an
// optional field writes one.
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

// bindActor settles which operation actor's call is, or answers the refusal:
// [operations.bind], seeded wherever the actor already carries the identity a
// derived id is built from ([Actor.OperationSeed]) — a turn's, or a request's
// own key. What it settles is the actor's [Actor.Operation], which every write
// the call makes derives its id from ([opIDFor], [commentID]). The families
// whose writes are named by [opIDFor] — the tracker's and the code host's —
// each call this, so the seed is read one way for both.
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

// param offers `op_id` in a write tool's schema, on the surface whose callers
// bring one back and nowhere else — described once, for every tool of every
// family that offers it.
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

// callName is the name an operator's operation id carries — minted, or brought
// back: the tool, and a digest of the arguments of the call it is for.
//
// # Why the arguments are in the operation
//
// Because an `op_id` promises that nothing is made twice, and a brought-back
// id with other arguments cannot keep that promise any other way. Its steps
// are named by the objects the call writes ([opIDFor]), so the same id sent
// with another project derived another operation and filed a second item; and
// named without them, the steps that DID match were answered as the first
// call's while the ones that did not — a dependency mirror on a blocker the
// first call never named, a subtask re-parented onto another canonical item,
// a tag declared that the first call never asked for — landed as a write
// nothing had authored. Refused whole, before the first append, none of those
// can happen: the id is that call, or it is nothing.
//
// A DIGEST, not the arguments, because an id is at most
// [statelog.MaxCallerOpIDBytes] and travels in every record and ledger row its
// steps write; sixteen hex digits of [turnctx.ArgsDigest] tell an accidental
// change apart with certainty, which is all this check is for — a caller can
// mint any id that carries an instant anyway, so this is not a credential.
// The `op_id` itself is left out of it, since the first call carried none.
//
// JOINED BY A DASH, because the name is one segment of the grammar and a dot
// begins a step: joined by one, the name was escaped whole and an operator
// reading the ledger could no longer see which tool an id was minted for.
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
