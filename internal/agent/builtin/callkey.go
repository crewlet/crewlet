package builtin

import (
	"fmt"
	"strconv"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
)

// callOpKey is the operation key every write one tool call makes is derived
// from (package doc, "How a write is made once"): in a turn, the turn's
// ([Actor.callKey]); outside one, `sent`, the key the caller's transport put on
// the actor, already scoped by internal/api/opkey; or "", and every write is
// fresh. ONE derivation for every domain, because what makes a call one
// operation is a fact about the turn, not the store: a copy per domain is free
// to disagree. `family` is the tools the call is counted among, declared by
// their domain ([chatlet.CallFamily]), nil the tool alone: a join after a
// leave must not take the first join's key. The tracker's tools derive their
// own ids from the key (package doc, "What the tracker's tools derive from a
// call's key").
func callOpKey(turn *turnctx.Turn, tool string, family []string,
	args map[string]any, sent string) string {

	return turnIdentity(turn).callKey(tool, family, args, sent)
}

// callKey is [callOpKey] over the identity an [Actor] carries, and the one
// place the key is derived. A request's key is the call's as sent
// ([Actor.keyedRequest]), the key [PageDeps.Actor] and [ChatDeps.Actor] hand
// on as `sent`, so a request's tracker and page writes share it. A turn's key
// carries what keeps a different write from being answered as an earlier
// one's retry: the instant the unit of work began ([Actor.OperationSince]),
// never the call's, which reads a re-run after a ledger loss as new and
// publishes it again; the arguments' digest AS TYPED, so two different calls
// are two, even two that resolve alike (a page by id and by title); the repeat
// count across the family ([turnctx.CallLog.OrdinalAmong]), so a call after a
// different one is new, a count of zero adding nothing; and the tool, since
// the count is kept per tool.
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
