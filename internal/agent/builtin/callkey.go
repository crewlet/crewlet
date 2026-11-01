package builtin

import (
	"fmt"
	"strconv"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
)

// callOpKey is the operation key every write one tool call makes is derived
// from — the package doc's one rule, "How a write is made once" — for every
// domain these tools write: the knowledge base and chat, whose stores derive
// each write's id from the key the caller carries ([doclet.Actor.OpKey],
// [chatlet.Actor.OpKey]) by the key, the verb, the object and a digest of what
// the write says; the tracker, whose ids these tools derive from it
// themselves ([opIDFor], [commentID]); and the artifacts and their
// datastores, whose writers derive every record's id from the key a call is
// made under ([ArtifactDeps.operation]). So a key names a CALL, and what it is
// handed to names each gesture the call makes:
//
//   - IN A TURN, a key derived from the turn's seed ([turnKey]), the instant
//     it began ([Actor.OperationSince]), the tool, a digest of the call's own
//     arguments ([turnctx.ArgsDigest]) and this call's repeat count in the run
//     ([turnctx.CallLog.OrdinalAmong]) — see [Actor.callKey] for what each
//     part buys;
//   - OUTSIDE ONE, `sent` — the key the caller's transport put on the actor
//     ([PageDeps.Actor], [ChatDeps.Actor]), as it was sent: it already names
//     one request, held to the operation grammar and scoped to the principal
//     that sent it by internal/api/opkey;
//   - or none, and every write is minted fresh.
//
// # One derivation, every domain
//
// ONE FUNCTION rather than one per domain, because what makes a turn's call
// one operation is a fact about the TURN — its seed, its instant, its call
// log — and not about the store the call reaches. A copy per domain is the
// same rule spelled twice and free to disagree, which is the shape
// internal/whsec and internal/textcut exist because of — and the tracker's
// tools carried such a copy, which named no tool. The tool is part of every
// identity, so two tools' calls never derive one another's keys.
//
// # The digest, beside the store's parts
//
// THE DIGEST IS THE CALL'S OWN, beside the parts the store adds, because the
// two answer different questions and the count needs both. The repeat count
// compares the arguments AS TYPED, skipping every earlier call that was this
// same call, while the store's parts are what the call RESOLVED to — a page
// id, a room id, a digest of the body. A page or a room can be named two ways
// (an id, or a title or name), so two calls typed differently resolve alike,
// and a count they happen to share handed the later one the earlier one's id:
// "ok" by id, the same again, "ok" by title, "other" by id and "ok" by id once
// more gave the last call the title call's count, so it was answered with that
// comment's success and nothing was written.
//
// # The family
//
// `family` is the tools this call is COUNTED AMONG, declared by the domain
// that owns them ([chatlet.CallFamily]) — nil counts the tool alone. Two tools
// that write one state in opposite directions need it: a seat that joined a
// room, left it and joined again made its second join after no call of its
// OWN tool that asked anything different, so a count kept per tool handed it
// the first join's key and the first join's success while the seat stayed
// outside. See [turnctx.CallLog].
//
// # What the tracker's tools derive from it
//
// The tracker's store is handed the ids its writes are published under, so its
// tools derive them from this key rather than handing it on, each under a
// namespace of its own ([opIDNamespace], [commentNamespace]) that keeps the two
// apart from each other and from every store's derivation over the same key —
// FIXED for the life of the format, since a new one makes every retry that
// straddles the change a second write:
//
//   - THE OPERATION ID ([opIDFor]) is NAMED `<verb>-<object>`, where the
//     stores' ids are named by the verb alone ([statelog.KeyedOpID]),
//     because a call under an operator's operation ([Actor.Operation]) derives
//     each write as a STEP of that operation, and there the step's name is the
//     write's whole identity within the call: one call writes many records,
//     often of one verb on several objects — a dependency on each of two
//     tasks — so the name must carry the object. Every other arm names its id
//     the same, so a write reads alike in the ledger whichever arm minted it.
//     The object goes into the name through [statelog.OpNamePart], so the
//     name is one segment of the grammar whatever the object holds — a
//     file's path holds the dot that begins a step.
//   - THE COMMENT'S ID ([commentID]) is a UUIDv5 rather than an operation id,
//     because a tracker comment's id is the primary key of its row, apart
//     from the operation that writes it — a comment rides its task's record,
//     and the writer answers a post its row already holds from that row —
//     where a page's comment IS its operation ([statelog.KeyedObjectOpID]).
//     So it is derived from the key as the work began, a rebase put aside
//     ([Actor.asBegun]): a rebase re-mints operations for a ledger that
//     forgot them, and the row has not forgotten the comment.
//   - THE IDS A CREATE FILES UNDER ([createdTaskID], [createdViewID]) are
//     UUIDv5s over the create's operation as the work began, for the
//     comment's reason: each is the subject its create arbitrates on and the
//     key of its row, and the tracker answers a create whose row is already
//     there from that row.
//
// Both put the call's digest in beside the key, as the stores bind what a
// write says: a request's key is its call's key as sent, and names the
// request rather than what it asked — the same key sent with other arguments
// is another write. In a turn the digest is already in the key and adds
// nothing.
func callOpKey(turn *turnctx.Turn, tool string, family []string,
	args map[string]any, sent string) string {

	return turnIdentity(turn).callKey(tool, family, args, sent)
}

// callKey is [callOpKey] over the identity an [Actor] carries — a turn's seed,
// its instant and its call log, or a request's key — and the one place the
// key is derived.
//
// A REQUEST'S KEY IS THE CALL'S, AS SENT. The tracker's actor carries it as
// its work key with no run behind it ([Actor.keyedRequest]), and it is the
// very key [PageDeps.Actor] and [ChatDeps.Actor] hand the knowledge base and
// chat as `sent`, so a request's tracker write and its page write derive from
// one key, by one rule.
//
// # What each part of a turn's key buys
//
// THE INSTANT THE UNIT OF WORK BEGAN ([Actor.OperationSince]), never the
// call's. The state log refuses to decide a second time an operation its
// ledger cannot vouch for, and "cannot vouch" is "minted before this node's
// ledger may have lost rows"; an id that carried this CALL's instant read every
// re-run after a loss as minted after it, and published the operation again.
// Every operation id derived from the key carries it too, and so does a
// comment's id, from the key as the work began rather than as a rebase
// ([turnctx.Turn.RebasedTo]) re-minted it ([commentID]).
//
// WHAT THE CALL ASKS FOR. The unit of work and the tool say which call this is
// — and not which of two calls. A turn that moved a task to `in_progress` and
// later to `done`, commented on it twice, or set a queue and then corrected
// it, derived ONE id for both calls, and the second was the first one's retry
// as far as anything downstream could tell: inside the log's duplicate window
// the broker acknowledged it as the first record, the ledger answered
// `applied` at the first position, and the change the model had just asked
// for was dropped with a success. With the digest the same call repeated — a
// re-run, or an executor that asks twice — is still one operation, and two
// different calls are two. That is as true of a call that writes one SHARED
// object — a project's tags or policy, the workspace catalogue, a label
// declaration — as of a task: keyed on the object alone, a turn that declared
// a tag and then archived another was answered `applied` for the second
// without it landing.
//
// HOW MANY DIFFERENT CALLS CAME BEFORE IT. What a call asks for says which
// call it is and not WHEN, so a run that moved an item to `in_progress`, then
// to `done`, then back to `in_progress` derived one id for the first and third
// calls, and the third was answered as the first's retry — `applied`, with the
// item still `done`. So the key carries the call's repeat count in the run,
// across its family: the third call follows a different one and is a new
// operation, while a call repeated with nothing different in between keeps its
// count and its key. A re-run that makes the same calls in the same order
// reproduces every count, and a COUNT OF ZERO ADDS NOTHING, so a run's first
// call of each kind derives the key of the call alone.
//
// THE TOOL, because the count is kept per tool: two tools each making their
// first call with the same arguments both count zero, and without the tool in
// the key a write one makes and a write the other makes of the same verb on
// the same object were one operation.
func (a Actor) callKey(tool string, family []string, args map[string]any,
	sent string) string {

	if a.keyedRequest() {
		return a.WorkKey
	}
	seed := a.OperationSeed()
	if seed == "" {
		return sent
	}
	identity := []string{callNamespace, seed, tool, turnctx.ArgsDigest(args)}
	if n := a.Calls.OrdinalAmong(family, tool, args); n > 0 {
		identity = append(identity, "repeat:"+strconv.Itoa(n))
	}
	return statelog.DeriveOpID(a.OperationSince(), "", identity...)
}

// callNamespace scopes [Actor.callKey]'s derivation. FIXED for the life of the
// format: a new one would make every re-run turn's keyed write a second one.
const callNamespace = "crewlet.builtin.call"

// callRepeat says how this caller makes a keyed write again as the same
// operation, or "" where no repeat is the same operation: a turn's call made
// again unchanged, a request sent again under its key, and nothing for a call
// that carried no key at all.
func callRepeat(turn *turnctx.Turn, key, tool string) string {
	switch {
	case turnKey(turn) != "":
		return fmt.Sprintf("call %s again with exactly the same arguments, "+
			"before calling it with any others", tool)
	case key != "":
		return "send the same request again, unchanged"
	}
	return ""
}
