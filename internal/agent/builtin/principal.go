package builtin

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tracker"
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
// tracker's four kinds. The credential is recorded again as OperatorID, the
// login it acted under, so "what did this credential do" is a question an
// audit answers without reasoning about kinds — for a bound person too, whose
// author is the seat.
func ActorOf(p iam.Principal) Actor {
	a := iam.ActorFor(p)
	return Actor{
		Handle: a.Name, Kind: tracker.AuthorKind(a.Kind), OperatorID: p.Login,
	}
}

// PageActorOf is [ActorOf] for the knowledge base, which records the SAME
// author under the SAME name — the audit feed reads both histories side by
// side, and one person under two names there is two people.
//
// AN ERROR FOR THE ONE KIND THE KNOWLEDGE BASE HAS NO WORD FOR: the engine's
// own principal. Nothing writes a page as the engine, and recording such a
// write as an operator or a person would put somebody's name on housekeeping.
func PageActorOf(p iam.Principal) (pages.Actor, error) {
	a := iam.ActorFor(p)
	kind := pages.AuthorKind(a.Kind)
	if !kind.Valid() {
		return pages.Actor{}, fmt.Errorf("builtin: a %s cannot author a page — "+
			"the knowledge base records agents, people and operators", a.Kind)
	}
	return pages.Actor{Handle: a.Name, Kind: kind, OperatorID: p.Login}, nil
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
func PrincipalPageActor(ctx context.Context, _ *turnctx.Turn) (pages.Actor, error) {
	p, err := resolvedPrincipal(ctx)
	if err != nil {
		return pages.Actor{}, err
	}
	return PageActorOf(p)
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
