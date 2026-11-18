package operator

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
)

// WHO A CALL HERE WRITES AS, AND UNDER WHICH OPERATION.
//
// # The principal, and nothing a call can choose
//
// Every actor handed a tool is [builtin.PrincipalActor]'s of the request's
// principal, [iam.ActorFor]'s: a person the directory binds to a seat writes AS
// it (kind `human`); a credential bound to nobody writes under its whole login
// (`token:ops`, kind `operator`); the credential used is recorded beside either.
// The guard resolves it once, so a rebind mid-call changes the next request,
// not this one. A caller naming a seat to act as is refused by design: an
// author field the writer chooses is not an audit trail.

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
// operation through [builtin.Actor.Keyed] — dated by the key's own instant,
// which every id the tools derive from it carries and the ledger vouches for a
// retry by. A key with no instant fails the call as a fault: the transport
// admits only a key the grammar minted (internal/api/opkey), so one reaching
// here is this surface's own mistake, never the caller's.
func workActor(ctx context.Context, turn *turnctx.Turn) (builtin.Actor, error) {
	actor, err := builtin.PrincipalActor(ctx, turn)
	if err != nil {
		return builtin.Actor{}, err
	}
	return actor.Keyed(keyFrom(ctx))
}

// pageActor is [workActor] for the knowledge base, which binds the call's
// operation to what each write says itself ([doclet.Actor.OpKey]).
func pageActor(ctx context.Context, turn *turnctx.Turn) (doclet.Actor, error) {
	actor, err := builtin.PrincipalPageActor(ctx, turn)
	if err != nil {
		return doclet.Actor{}, err
	}
	actor.OpKey = keyFrom(ctx)
	return actor, nil
}

// chatActor is [pageActor] for chat, which binds the call's operation to what
// each gesture says ([chatlet.Actor.OpKey]), as the knowledge base does.
//
// A CREDENTIAL ACTING AS ITSELF CONVERTS, to [chatlet.AuthorOperator], rather
// than failing here: who may SPEAK is the authority table's (every chat verb
// is seated) and the store's (it refuses that kind every gesture said in a
// room), and a conversion that refused it would be a third answer to the
// question, worded where neither of those two can be read.
func chatActor(ctx context.Context, turn *turnctx.Turn) (chatlet.Actor, error) {
	actor, err := builtin.PrincipalChatActor(ctx, turn)
	if err != nil {
		return chatlet.Actor{}, err
	}
	actor.OpKey = keyFrom(ctx)
	return actor, nil
}
