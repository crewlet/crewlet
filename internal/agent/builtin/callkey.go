package builtin

import (
	"fmt"
	"strconv"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
)

// callOpKey is the operation key every write one tool call makes is derived
// from, for the domains whose store derives each write's id from a key the
// caller carries — the knowledge base ([doclet.Actor.OpKey]), chat
// ([chatlet.Actor.OpKey]) and the artifacts and their datastores
// ([ArtifactDeps.operation]) — the package doc's one rule, "How a write is
// made once". Each store derives a write's id from the key, the verb, the
// object and a digest of what the write says, so a key names a CALL and the
// store names each gesture the call makes:
//
//   - IN A TURN, a key derived from the turn's seed ([turnKey]), the instant
//     it began ([turnSince]), the tool, a digest of the call's own arguments
//     ([turnctx.ArgsDigest]) and this call's repeat count in the run
//     ([turnctx.CallLog.OrdinalAmong]) — so a re-run's write is the first
//     run's, and a page saved A, then B, then A again in one run is three
//     saves rather than the third collapsing into the first, which the
//     store's digest of what each save says would otherwise make it;
//   - OUTSIDE ONE, `sent` — the key the caller's transport put on the actor
//     ([PageDeps.Actor], [ChatDeps.Actor]), as it was sent: it already names
//     one request, held to the operation grammar and scoped to the principal
//     that sent it by internal/api/opkey;
//   - or none, and the store mints a fresh id for each write.
//
// # One derivation, every domain
//
// ONE FUNCTION rather than one per domain, because what makes a turn's call
// one operation is a fact about the TURN — its seed, its instant, its call
// log — and not about the store the call reaches. A copy per domain is the
// same rule spelled twice and free to disagree, which is the shape
// internal/whsec and internal/textcut exist because of. The tool is part of
// every identity, so two domains' calls never derive one another's keys.
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
// comment's success and nothing was written. The tracker's ids have always
// carried the digest ([opIDFor]).
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
func callOpKey(turn *turnctx.Turn, tool string, family []string,
	args map[string]any, sent string) string {

	seed := turnKey(turn)
	if seed == "" {
		return sent
	}
	identity := []string{callNamespace, seed, tool, turnctx.ArgsDigest(args)}
	if n := turn.CallLog().OrdinalAmong(family, tool, args); n > 0 {
		identity = append(identity, "repeat:"+strconv.Itoa(n))
	}
	return statelog.DeriveOpID(turnSince(turn), "", identity...)
}

// callNamespace scopes [callOpKey]'s derivation. FIXED for the life of the
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
