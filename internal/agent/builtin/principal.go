package builtin

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/worklet"
)

// WHO A WRITE MADE THROUGH A CREDENTIAL IS RECORDED AS. A seat's writes are
// attributed from its TURN ([actorFor]); the two request surfaces (the
// operator's assistant over MCP, a person at the HTTP write surface) from the
// PRINCIPAL, through this one conversion, which is [iam.ActorFor]'s.
//
// A person bound to a seat writes AS THAT SEAT, kind `human`, so the tracker's
// "do not wake the author" rule recognises them. A credential nobody is bound
// through writes as its WHOLE login, kind `operator`: `token:ops`, never `ops`,
// because the colon keeps it out of the seat namespace, and a stripped name
// would pass every own-record rule of the seat that shares its spelling.

// ActorOf is how a principal is written down on a work record.
//
// TOTAL, as [iam.ActorFor] is: every principal comes back as one of the
// tracker's four kinds. OperatorID records the credential again (the machine
// token acted through, `pat:<id>`, else the login), so an audit answers "what
// did this credential do" without reasoning about kinds, and a bound person's
// assistant's writes stay distinguishable from their own.
func ActorOf(p iam.Principal) Actor {
	a := iam.ActorFor(p)
	return Actor{
		Handle: a.Name, Kind: worklet.AuthorKind(a.Kind), OperatorID: a.OperatorID,
		Apps: iam.ActorApps(p),
	}
}

// PageActorOf is [ActorOf] for the knowledge base, under the SAME name: the
// audit feed reads both histories side by side, and one person under two
// names there is two people.
//
// AN ERROR FOR THE ENGINE'S OWN PRINCIPAL, which Doclet has no kind for:
// recording it as an operator or a person would put somebody's name on
// housekeeping.
func PageActorOf(p iam.Principal) (doclet.Actor, error) {
	a := iam.ActorFor(p)
	kind := doclet.AuthorKind(a.Kind)
	if !kind.Valid() {
		return doclet.Actor{}, fmt.Errorf("builtin: a %s cannot author a page; "+
			"Doclet records agents, people and operators", a.Kind)
	}
	return doclet.Actor{Handle: a.Name, Kind: kind, OperatorID: a.OperatorID,
		Apps: iam.ActorApps(p)}, nil
}

// ChatActorOf is [ActorOf] for chat, under the SAME name, for [PageActorOf]'s
// reason.
//
// AN ERROR FOR THE ENGINE'S OWN PRINCIPAL, although chat has
// [chatlet.AuthorSystem]: that kind is the engine NARRATING a room from its own
// records, and a tool call made as it would put a line nobody said where a
// speaker belongs. A credential acting as itself converts to
// [chatlet.AuthorOperator]; the store refuses it every gesture in a room's
// voice ([chatlet.Actor] says which).
func ChatActorOf(p iam.Principal) (chatlet.Actor, error) {
	a := iam.ActorFor(p)
	kind := chatlet.AuthorKind(a.Kind)
	if !kind.Valid() || kind == chatlet.AuthorSystem {
		return chatlet.Actor{}, fmt.Errorf("builtin: a %s cannot author a chat "+
			"write — the engine narrates a room from its own records, and chat "+
			"records agents, people and operators", a.Kind)
	}
	return chatlet.Actor{Handle: a.Name, Kind: kind, OperatorID: a.OperatorID}, nil
}

// PrincipalActor is [WorkDeps.Actor] for a surface whose caller is on the
// request's context.
//
// AN ERROR RATHER THAN A FALLBACK NAME when nobody resolved: the caller is
// told the write did not happen, where a row written as "anonymous" would be
// a write nobody made.
func PrincipalActor(ctx context.Context, _ *turnctx.Turn) (Actor, error) {
	p, err := resolvedPrincipal(ctx)
	if err != nil {
		return Actor{}, err
	}
	return ActorOf(p), nil
}

// PrincipalPageActor is [PrincipalActor] for [PageDeps.Actor].
func PrincipalPageActor(ctx context.Context, _ *turnctx.Turn) (doclet.Actor, error) {
	p, err := resolvedPrincipal(ctx)
	if err != nil {
		return doclet.Actor{}, err
	}
	return PageActorOf(p)
}

// PrincipalChatActor is [PrincipalActor] for [ChatDeps.Actor].
func PrincipalChatActor(ctx context.Context, _ *turnctx.Turn) (chatlet.Actor, error) {
	p, err := resolvedPrincipal(ctx)
	if err != nil {
		return chatlet.Actor{}, err
	}
	return ChatActorOf(p)
}

// resolvedPrincipal is the request's principal, or why there is none to write
// as.
func resolvedPrincipal(ctx context.Context) (iam.Principal, error) {
	p, how := iam.From(ctx)
	if how != iam.Resolved {
		return iam.Principal{}, fmt.Errorf("builtin: this request carries "+
			"nobody to attribute a write to (%s)", how)
	}
	return p, nil
}
