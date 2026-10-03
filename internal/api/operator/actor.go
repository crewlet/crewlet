package operator

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// WHO A CALL HERE WRITES AS, AND UNDER WHICH OPERATION.
//
// # The principal, and nothing a call can choose
//
// Every actor this surface hands a tool is [builtin.PrincipalActor]'s of the
// request's principal — the one conversion the tools make for any caller with
// no turn — which is [iam.ActorFor]'s: a person the identity directory binds
// to a seat writes AS that seat, with kind `human`; a credential nobody is
// bound through writes under its whole login (`token:ops`, colon and all),
// with kind `operator`; and the credential the call came through is recorded
// beside either. The binding is the DIRECTORY'S, resolved by the request guard
// before any handler ran, so it is one value on the context for the whole call:
// the actor, the dispatch's audit record and every log line read that value,
// and a rebind or a chart rename landing while the call runs changes the next
// request rather than splitting this one between two people.
//
// The alternative — asking the caller to name a seat to act as — was rejected:
// it lets anybody with a credential write as anybody, and a tracker whose
// author field can be chosen by the writer is not an audit trail.

// keyKey is the context key [withKey] stores under. An unexported type, so no
// other package can write or shadow it.
type keyKey struct{}

// withKey marks ctx as carrying the operation ONE call's transport named.
//
// A TRANSPORT SETS IT, through [Call.Key], and never a tool argument: the
// operation decides which writes are one, and a caller who could put it in the
// arguments of an ordinary call could choose which operation their write
// collapses into.
func withKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, keyKey{}, key)
}

// keyFrom is the operation [withKey] stored, or "".
func keyFrom(ctx context.Context) string {
	key, _ := ctx.Value(keyKey{}).(string)
	return key
}

// workActor is [builtin.WorkDeps.Actor] for this surface: the request's
// principal, written down by [builtin.PrincipalActor], carrying the call's
// operation and the instant it was minted at — which every id the tools derive
// from it carries, and which the ledger vouches for a retry by.
func workActor(ctx context.Context, turn *turnctx.Turn) (builtin.Actor, error) {
	actor, err := builtin.PrincipalActor(ctx, turn)
	if err != nil {
		return builtin.Actor{}, err
	}
	if key := keyFrom(ctx); key != "" {
		actor.WorkKey = key
		actor.WorkSince, _ = statelog.OpMintedAt(key)
	}
	return actor, nil
}

// pageActor is [workActor] for the knowledge base, which binds the call's
// operation to what each write says itself ([pages.Actor.OpKey]).
func pageActor(ctx context.Context, turn *turnctx.Turn) (pages.Actor, error) {
	actor, err := builtin.PrincipalPageActor(ctx, turn)
	if err != nil {
		return pages.Actor{}, err
	}
	actor.OpKey = keyFrom(ctx)
	return actor, nil
}
