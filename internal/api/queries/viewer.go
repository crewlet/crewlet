// Who the caller is, which is the question every personal surface rests on.

package queries

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// The two refusals a personal question makes, told apart because the remedies
// are different: one is a line of company configuration, the other authority.
var (
	errNoSeat = fmt.Errorf("%w: this credential is not bound to a seat — bind "+
		"the person to one in the org chart, or name a handle", ErrBadParams)
	errNotYours = fmt.Errorf("%w: reading another seat's record needs the "+
		"lead relation or people:manage", ErrUnauthorized)
)

// viewer answers who this caller is.
//
// THE PRINCIPAL AND NOTHING ELSE. It used to walk a chain of two — Tier A's
// `api.auth.tokens` mapped a credential to an operator id, and a field on a
// seat's contact block named one of those ids — which was the only binding
// this engine had between a credential and a seat. The identity estate
// replaced it: a person is a ROW, a session resolves to that row, and the seat
// they hold is on the chart. Nothing here derives anything any more.
//
// AN UNBOUND CREDENTIAL IS AN ORDINARY STATE, so this answers the login with
// no seat rather than an error — the screen then says what to bind, which is a
// different thing from a screen that looks broken.
func (s Sources) viewer(ctx context.Context, _ Params) (any, error) {
	principal, how := iam.From(ctx)
	if how == iam.Unknown {
		return nil, unresolved(ctx, "viewer")
	}
	out := map[string]any{
		"login": principal.Login,
		// WHAT THIS CALLER MAY ASK, answered once so a screen can draw a
		// locked row rather than discovering the refusal per question.
		"grants": grantNames(principal),
		// THE SEAT'S OWN FIELDS, empty for a credential that holds none.
		// `kind` is the SEAT's — human or agent — and never the
		// principal's: the two are different vocabularies, and a screen
		// reading "person" where the chart says "human" would be reading
		// a word this company never wrote.
		"handle": "",
		"name":   "",
		"kind":   "",
	}
	seat := s.seatOf(principal)
	if seat == nil {
		return out, nil
	}
	out["handle"] = seat.Handle()
	out["name"] = seat.Name
	// The zero value is an agent, which is what config.Role's own `kind`
	// defaults to — reporting "" would make the common case look unset.
	if kind := seat.Kind; kind != "" {
		out["kind"] = string(kind)
	} else {
		out["kind"] = string(org.KindAgent)
	}
	return out, nil
}

// grantNames renders a principal's capabilities for the screen.
func grantNames(p iam.Principal) []string {
	out := make([]string, 0, len(p.Grants))
	for _, g := range p.Grants {
		out = append(out, string(g))
	}
	return out
}

// seatOf resolves the caller's own seat, or nil.
func (s Sources) seatOf(p iam.Principal) *org.Role {
	if p.Seat == "" || s.Company == nil {
		return nil
	}
	company, roster := s.Company()
	if company == nil || roster == nil {
		return nil
	}
	// EVERY SEAT AND NOT ONLY THE AGENTS: a person holds a HUMAN seat,
	// which is the whole case this resolves.
	return roster.SeatByHandle(p.Seat)
}

// viewerHandle is the handle a personal question answers for when the caller
// named none, and the authority check when they named one.
//
// THE SCOPE RULE, in one place because four questions share it — and it is the
// authority TABLE's rule rather than a second copy of it: the caller reads
// their own record, whoever leads them, or anybody's with people:manage, which
// is exactly [authz.ClassOwnOrLead]. It used to be "your own, or ANY operator
// credential for anybody else's", which made every token in Tier A a reader of
// every seat's inbox.
//
// Returns the handle to read and an error to refuse with.
func (s Sources) viewerHandle(ctx context.Context, asked string) (string, error) {
	principal, how := iam.From(ctx)
	if how == iam.Unknown {
		return "", unresolved(ctx, "viewer")
	}
	own := principal.Seat
	if asked == "" {
		if own == "" {
			// NOT AN AUTHORIZATION FAILURE. Nobody was refused: there is
			// no person to answer about, and the remedy is a binding in
			// the org chart rather than a different credential.
			return "", errNoSeat
		}
		return own, nil
	}
	if asked == own {
		return asked, nil
	}
	d := authz.Decide(ctx, principal, authz.ActionPersonRead,
		authz.Object{Kind: authz.KindPerson, Owner: asked}, s.Chart)
	switch {
	case d.Unknown():
		// THIS NODE COULD NOT TELL, which a surface renders as 503 and
		// never as a refusal: a node behind the chart log telling a lead
		// they lead nobody sends them to ask for authority they hold.
		return "", fmt.Errorf("%w: this node cannot say who leads %s yet: %w",
			ErrUnavailable, asked, d.Err)
	case !d.Allowed:
		return "", errNotYours
	}
	return asked, nil
}

// viewerPins is the authority rule over the viewer a view STRIP is
// personalised by.
//
// The same check as [Sources.viewerHandle] over a different ABSENCE, which is
// why the named case delegates rather than restating it. A personal question
// is ABOUT somebody, so naming nobody with no seat to fall back on has no
// answer and [errNoSeat] says so. A view strip is about a CONTAINER and the
// viewer only decides whose pins order it, so naming nobody is the SHARED
// strip — a real answer, the documented meaning of an empty
// [tracker.ViewQuery.Viewer], and the one an unbound caller must keep getting,
// because the sidebar and the board ask for exactly that.
func (s Sources) viewerPins(ctx context.Context, asked string) (string, error) {
	if asked == "" {
		return "", nil
	}
	return s.viewerHandle(ctx, asked)
}
