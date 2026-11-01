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

// WHO A WRITE MADE THROUGH A CREDENTIAL IS RECORDED AS.
//
// # One conversion, for every surface that has a request rather than a turn
//
// A seat's writes are attributed from its TURN ([actorFor]); everybody else's
// — the operator's assistant over MCP, a person at the HTTP write surface —
// from the PRINCIPAL the request resolved to. There are two such surfaces and
// one answer, so the answer lives here, beside the Actor it produces, rather
// than in either of them.
//
// # And the answer is [iam.ActorFor]'s
//
// A person bound to a seat writes AS THAT SEAT, with kind `human`; a
// credential nobody is bound through writes as its own LOGIN, with kind
// `operator`. The seat half is the one that was missing, and its absence was
// not cosmetic: the operator surface recorded every person under their TOKEN
// id, so the tracker's own "do not wake the author" rule compared a seat
// handle against a token name, never matched, and every write a person made
// woke them about it — the assignee editing their own task was paged for
// having done so.
//
// The login half is the WHOLE login, `token:ops` and never `ops`. The colon is
// what keeps it out of the seat namespace (a seat handle cannot carry one), so
// stripping it — which is what the bare token id did — hands a machine a name a
// seat can also hold, and every own-record rule that compares names would then
// admit a credential into the record of the seat that shares its spelling.

// ActorOf is how a principal is written down on a work record.
//
// TOTAL, as [iam.ActorFor] is: every principal comes back as one of the
// tracker's four kinds. The credential is recorded again as OperatorID —
// [iam.ActorFor]'s own: the machine token a principal acts through
// (`pat:<id>`), or the login it acted under — so "what did this credential
// do" is a question an audit answers without reasoning about kinds, for a
// bound person too, whose author is the seat. It used to be the login alone,
// which recorded a person's own token as the person: every item their
// assistant filed was indistinguishable from one they filed themselves.
func ActorOf(p iam.Principal) Actor {
	a := iam.ActorFor(p)
	return Actor{
		Handle: a.Name, Kind: worklet.AuthorKind(a.Kind), OperatorID: a.OperatorID,
	}
}

// PageActorOf is [ActorOf] for the knowledge base, which records the SAME
// author under the SAME name — the audit feed reads both histories side by
// side, and one person under two names there is two people.
//
// AN ERROR FOR THE ONE KIND THE KNOWLEDGE BASE HAS NO WORD FOR: the engine's
// own principal. Nothing writes a page as the engine, and recording such a
// write as an operator or a person would put somebody's name on housekeeping.
func PageActorOf(p iam.Principal) (doclet.Actor, error) {
	a := iam.ActorFor(p)
	kind := doclet.AuthorKind(a.Kind)
	if !kind.Valid() {
		return doclet.Actor{}, fmt.Errorf("builtin: a %s cannot author a page; "+
			"Doclet records agents, people and operators", a.Kind)
	}
	return doclet.Actor{Handle: a.Name, Kind: kind, OperatorID: a.OperatorID}, nil
}

// ChatActorOf is [ActorOf] for chat, which records the SAME author under the
// SAME name, for [PageActorOf]'s reason: one person under two names across
// the histories an audit reads side by side is two people.
//
// AN ERROR FOR THE ENGINE'S OWN PRINCIPAL, although chat HAS a word for it
// ([chatlet.AuthorSystem]): that kind is the engine NARRATING a room — somebody
// joined, the room was archived — which the store writes from its own
// records, never a tool call. A tool call made as the engine would put a
// narration where a speaker belongs, a line nobody said and nobody can answer.
//
// A CREDENTIAL ACTING AS ITSELF CONVERTS, to [chatlet.AuthorOperator]: it is a
// real author of what is not said in a room's voice, and the store refuses it
// every gesture that is ([chatlet.Actor] says which), naming the remedy.
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
// AN ERROR RATHER THAN A FALLBACK NAME when nobody resolved: this is reached
// inside a tool call, where there is a caller waiting for an answer and the
// honest one is that the write did not happen. A row written as "anonymous"
// would be a write nobody made.
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
