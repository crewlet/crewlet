package builtin

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE KEY A KEYED WRITE IS DERIVED FROM, for the two domains whose stores take
// one — the wiki and chat ([statelog.CallKey]) — and the three questions every
// write tool over either asks of it: what the key is, whether the transport's
// operation was derived for this call, and how this caller repeats the write.
//
// ONE COPY OF EACH, over the transport's operation seam
// ([PageDeps.Operation], [ChatDeps.Operation]) rather than a method on either
// deps. The two used to carry byte-identical methods, and the two keys they
// built had already drifted once — chat's had the digest of the arguments and
// the wiki's did not — which is the shape internal/textcut and internal/whsec
// each record the cost of. What differs between the two domains is POLICY, and
// it is stated where each builds its key ([PageDeps.callKey],
// [ChatDeps.callKey]): which tools a repeat is counted among, and whether the
// key names its tool.

// transportOperation is the operation a transport named for this call, or "".
func transportOperation(ctx context.Context, operation func(context.Context) string) string {
	if operation == nil {
		return ""
	}
	return operation(ctx)
}

// callKeyOf is the key every write a call makes is derived from — the package
// doc's one rule, "How a write is made once", typed for the stores
// ([statelog.CallKey]):
//
//   - IN A TURN, the turn's seed ([turnKey]), the instant it began
//     ([turnSince]), this call's repeat count in the run, counted among
//     `family` ([turnctx.CallLog.OrdinalAmong]; nil is the tool alone), and a
//     digest of the arguments the count compared ([turnctx.ArgsDigest]);
//   - OUTSIDE ONE, the operation the transport named for this call, carrying
//     that operation's own instant — it already names the call, its arguments
//     included, so it takes no repeat count and no digest;
//   - or none, which the store mints afresh.
//
// `named` records the tool on the key, for a domain two of whose tools make one
// gesture — see [statelog.CallKey]'s doc.
//
// THE DIGEST TRAVELS WITH THE COUNT because the count compares the arguments
// as typed and the store's parts are what they resolved to: an object named by
// its id once and by its name the next time is two calls to one and one object
// to the other, and without the digest a count two such calls share hands the
// later one the earlier one's id.
func callKeyOf(ctx context.Context, turn *turnctx.Turn,
	operation func(context.Context) string, tool string, family []string,
	args map[string]any, named bool) statelog.CallKey {

	var key statelog.CallKey
	switch {
	case turnKey(turn) != "":
		key = statelog.CallKey{Seed: turnKey(turn), Since: turnSince(turn),
			Repeat: turn.CallLog().OrdinalAmong(family, tool, args),
			Args:   turnctx.ArgsDigest(args)}
	case transportOperation(ctx, operation) != "":
		op := transportOperation(ctx, operation)
		at, _ := statelog.OpMintedAt(op)
		key = statelog.CallKey{Seed: op, Since: at}
	default:
		return statelog.CallKey{}
	}
	if named {
		key.Tool = tool
	}
	return key
}

// foreignOperation refuses a write whose transport named an operation derived
// for ANOTHER call — [WorkDeps.bindOperation]'s rule, held on the wiki's and
// chat's tools for its reason: a check that exempted one source of operations
// would be where a transport that derived one wrong wrote anyway, and here it
// would write a page, a comment or a message under ids the other call derives.
// These tools take no `op_id`, so a transport's operation is the only one they
// meet. "" where the call has none, is a turn's (whose key wins — see
// [callKeyOf]), or names this call.
func foreignOperation(ctx context.Context, turn *turnctx.Turn,
	operation func(context.Context) string, tool string, args map[string]any) string {

	if turnKey(turn) != "" {
		return ""
	}
	op := transportOperation(ctx, operation)
	if op == "" || statelog.OpNamed(op, callName(tool, args)) {
		return ""
	}
	return foreignRequest(tool)
}

// keyedCaller is who made a keyed write, as far as repeating it goes — a turn,
// a caller whose transport named its operation, or neither — because what a
// repeat IS depends on where the key came from.
type keyedCaller struct {
	turn bool
	key  statelog.CallKey
}

// callerOf is the caller behind a call's key.
func callerOf(turn *turnctx.Turn, key statelog.CallKey) keyedCaller {
	return keyedCaller{turn: turnKey(turn) != "", key: key}
}

// sameCall says how this caller makes a write that ADDS something again as
// the SAME operation — [sameCall]'s rule for these keys — or "" where no
// repeat is: a turn's is the same call before any other to its tool; a
// request's is the request sent again; an operator's assistant over MCP names
// no operation for these tools, so its repeat is a new one, and this says so
// by saying nothing.
//
// # Why a turn's names no other tool
//
// Because it serves the tools that add — the wiki's writes, chat's POSTING
// tools ([chat.WriteTools]) and a room's create — and none of them is counted
// among a family ([chat.CallFamily]): nothing one of them makes undoes
// another, so the only call that moves one's count is a call to its own tool
// with other arguments. The tools that ARE counted across a pair —
// join_channel and leave_channel — state something rather than add it, and
// answer through [stated], whose repeat is offered rather than described:
// stating one membership again changes nothing, whichever operation the repeat
// is.
//
// A family tool that ever needed this sentence could not take it as written.
// [turnctx.CallLog.OrdinalAmong] counts EVERY call to another tool of the
// family, whatever its arguments, so "before calling join_channel or
// leave_channel with any others" would invite exactly the call that makes the
// repeat a new operation. It would have to name the other tool
// unconditionally, and a test holds that none of the tools it serves has a
// family.
func (k keyedCaller) sameCall(tool string) string {
	switch {
	case k.turn:
		return fmt.Sprintf("call %s again with exactly the same arguments, "+
			"before calling it with any others", tool)
	case k.key.String() != "":
		return "send the same request again, unchanged"
	}
	return ""
}
