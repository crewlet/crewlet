package auth

import (
	"context"
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
)

// WHAT THIS BUILD'S GUARD ESTABLISHES, TRANSLATED INTO THE VOCABULARY THE
// AUTHORITY TABLE DECIDES IN.
//
// # Why a translation and not a resolution
//
// Tier A lists bearer tokens. A token has an id and a value and NOTHING ELSE
// — no capabilities, no person behind it, no lifecycle — and every route it
// opens, it opens completely: with one of these a caller writes the company
// document, the secret store, the tracker and the knowledge base. That is the
// posture this repository has today, and it is what [Principal] states.
//
// So the translation is deliberately blunt: a recognised token is a MACHINE
// principal holding every grant, because that is precisely what it can
// already do. Pretending otherwise — inventing a narrower set here — would
// make internal/authz refuse requests the guard beside it plainly allows, and
// the two would be a security decision made twice with different answers.
//
// # The one thing it is NOT blunt about
//
// A token bound to a chart seat acts AS THAT SEAT. The binding already exists
// (`contact.crewlet_operator_id` on a human seat) and already means this: a
// person working through the dashboard acts as themselves rather than as a
// credential. Carrying it here is what makes a LEAD relation reachable at
// all — without the seat handle every authority rule that asks "do you lead
// this" would fall through to the admin grant, and a founder would be
// indistinguishable from a CI pipeline in every audit row and every refusal.
//
// # What replaces it
//
// The identity work that gives a principal a real lifecycle, real grants and
// a session of its own. When that lands this file goes, together with
// [GuardedPrefixes] and the bearer comparison: they are one posture, not
// three, and retiring half of it would leave the narrower half deciding
// against the wider one.

// Principal is the acting party, as this build can establish it.
//
// AN UNAUTHENTICATED REQUEST IS NOT A PARTY. It comes back as the zero
// principal, whose stage refuses every action — which is the honest answer
// while anonymous reads exist: the caller presented nothing, so there is
// nobody for a rule to decide about. Routes that serve anonymously are the
// ones never mounted through the authority table.
//
// boundSeat maps an operator id to the seat handle bound to it, and answers
// empty for a credential nobody in the chart claims — which is an ORDINARY
// state rather than a misconfiguration: a pipeline, an automation, an
// administrator who is not a member of the company.
func Principal(ctx context.Context, boundSeat func(operatorID string) string) iam.Principal {
	id, ok := OperatorFrom(ctx)
	if !ok || id == "" {
		return iam.Principal{}
	}
	p := iam.Principal{
		Login: id,
		// A MACHINE UNTIL A SEAT SAYS OTHERWISE. The kind decides how a
		// write is attributed, and a bare credential is not a person
		// however much a person is holding it.
		Kind:   iam.KindMachine,
		Stage:  iam.StageActive,
		Grants: slices.Clone(iam.AllGrants),
	}
	if boundSeat == nil {
		return p
	}
	if handle := boundSeat(id); handle != "" {
		p.Kind, p.Seat = iam.KindPerson, handle
	}
	return p
}
