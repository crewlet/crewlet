package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

// HOW A MACHINE TOKEN BECOMES A PRINCIPAL: the guard's THIRD credential arm.
//
// A Tier A token is the DEPLOYMENT's credential and a cookie is a PERSON's
// session; a machine token (`cwl_pat_…`) is the third shape — a person's own
// assistant acting as them, or a service account the company declared —
// minted through `/iam/credentials` and presented as a bearer on every request.
//
// # Nothing here decides anything either
//
// What the rows mean is internal/iam/credential's table ([credential.CheckToken]),
// over ONE snapshot the identity reader takes; who the owner acts as is the
// seat table every other credential goes through ([session.ResolveSeat]). This
// file reads the bearer, asks both, and composes the principal — as the owner,
// carrying the grants the token was minted with that the owner STILL holds,
// cut to this node's ceiling on this request, and reaching no further than
// either of them.
//
// # A node that cannot tell says so
//
// UNKNOWN IS 503 AND NEVER 401, for the session table's reason: a pipeline
// that reads 401 discards a credential that was fine, and a node behind the
// identity log — including one that has not yet applied the very mint the
// token names — cannot establish that anything is wrong with it.

// TokenDirectory is the one read a machine token needs.
//
// DEFINED HERE, by the caller, and one method wide. The identity reader
// answers it in one snapshot; AN ERROR IS THE UNKNOWN ARM, always.
type TokenDirectory interface {
	MachineToken(ctx context.Context, id string) (credential.TokenRow, error)
}

// Tokens is the guard's machine-token arm.
//
// A VALUE BUILT ONCE AT WIRING and read on every request, with no cache, for
// the reason [Sessions] carries none: the read is a keyed lookup of a local
// replicated row, and a cache would be a second idea of which tokens are live
// in front of the one a revocation has to reach.
type Tokens struct {
	directory TokenDirectory
	chart     session.Chart
	now       func() time.Time
}

// TokensDeps is what the arm is built from.
type TokensDeps struct {
	// Directory reads one token and its owner. REQUIRED.
	Directory TokenDirectory

	// Chart resolves a bound owner's seat. REQUIRED, and the ZERO VALUE of
	// the engine's adapter is what a node with no chart domain passes: it
	// answers every seat question UNKNOWN, never the seatless arm.
	Chart session.Chart

	// Now is the clock. Nil takes UTC wall time.
	Now func() time.Time
}

// NewTokens builds the arm, or says which half is missing.
func NewTokens(deps TokensDeps) (*Tokens, error) {
	switch {
	case deps.Directory == nil:
		return nil, errors.New("auth: the token arm needs the identity " +
			"directory — a token's secret says nothing until a row says whose " +
			"it is and whether it is still live")
	case deps.Chart == nil:
		return nil, errors.New("auth: the token arm needs a chart seam; a nil " +
			"one would resolve every bound owner as seatless")
	}
	t := &Tokens{directory: deps.Directory, chart: deps.Chart, now: deps.Now}
	if t.now == nil {
		t.now = func() time.Time { return time.Now().UTC() }
	}
	return t, nil
}

// WithTokens installs the machine-token arm and returns the guard for
// chaining.
//
// CALLED ONCE, AT WIRING TIME, for [Guard.BindSeats]' reason. Nil is a guard
// that resolves no machine token — what a suite stands up — and a token-shaped
// bearer there is refused like any bearer it does not hold. A running node is
// never that: `crewlet run` installs the arm on every node, and on one running
// no identity domain its read answers "cannot say", so a token minted
// elsewhere in the fleet is a 503 there rather than a 401.
func (g *Guard) WithTokens(t *Tokens) *Guard {
	g.machine = t
	return g
}

// PresentedToken is the id of the machine token this request carried as its
// bearer, if it carried one.
//
// ASKED BY THE ROUTES THAT MUST KNOW: minting a token from a token would let
// whoever holds one extend it for ever, a year at a time, with nobody present,
// and a token may manage none of its owner's proof.
//
// READ OFF THE PRINCIPAL'S [iam.Principal.Via], which is the one place the
// arm records it: a second carrier on the context beside it was a second
// answer to "through what", and the audit trail and these refusals would have
// asked different ones.
func PresentedToken(ctx context.Context) (string, bool) {
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return "", false
	}
	id, ok := strings.CutPrefix(principal.Via, iam.MachineTokenPrefix)
	return id, ok && id != ""
}

// errTokenUnavailable is the reason attached to a request whose machine token
// this node could not check.
var errTokenUnavailable = errors.New("auth: this node could not establish " +
	"whether this token is live")

// token resolves a request presenting a machine token.
func (g *Guard) token(r *http.Request, presented credential.Token,
	candidate string) (*http.Request, *Refusal) {

	ctx := r.Context()
	now := g.machine.now()
	row, err := g.machine.directory.MachineToken(ctx, presented.ID)
	if err != nil {
		log.WarnContext(ctx, "api_token_unavailable",
			"credential", presented.ID, "error", err)
		return r.WithContext(iam.WithUnresolved(ctx, errTokenUnavailable)), nil
	}
	check := credential.CheckToken(presented, row, now, statelog.StallGrace)
	switch check.Answer {
	case credential.TokenValid:
	case credential.TokenUnknown:
		log.WarnContext(ctx, "api_token_unavailable",
			"credential", presented.ID, "detail", check.Detail)
		return r.WithContext(iam.WithUnresolved(ctx, errTokenUnavailable)), nil
	default:
		// PRESENT AND NO GOOD IS ANONYMOUS, and marked like any other
		// refused bearer, which a guarded route's refusal counts as a
		// failed attempt — the log carries which fact decided, the caller
		// one code for all of them.
		log.InfoContext(ctx, "api_token_refused",
			"credential", presented.ID, "detail", check.Detail)
		return r.WithContext(refusedCredential(iam.WithAnonymous(ctx),
			candidate)), nil
	}

	owner := row.Owner
	id, err := uuid.Parse(owner.ID)
	if err != nil {
		// A ROW THIS ENGINE WROTE carries a uuid it minted, so this is a
		// store that is not answering the question it was asked.
		log.WarnContext(ctx, "api_token_owner_unreadable",
			"credential", presented.ID, "error", err)
		return r.WithContext(iam.WithUnresolved(ctx, errTokenUnavailable)), nil
	}
	p := iam.Principal{
		ID: id, Login: owner.Login, Kind: owner.Kind, Stage: owner.Stage,
		// THE MINT'S GRANTS THE OWNER STILL HOLDS, cut to this node's
		// ceiling on this request.
		Grants:    intersect(row.EffectiveGrants(), g.ceiling),
		Colleague: row.EffectiveColleague(),
		// AND THROUGH WHAT. The token acts as its owner, so without this
		// every write it made was recorded exactly as the owner's own —
		// and a token minted on somebody's account could file, edit and
		// close their work with no row saying a token was used. The
		// author stays the owner; the operator column names the token.
		Via: iam.MachineTokenName(presented.ID),
	}
	// STEPPED UP BY CONSTRUCTION FOR THE ORDINARY WINDOW, as every
	// non-interactive credential is — there is nothing else it could present
	// — and NEVER FOR THE SENSITIVE ONE, which is a person's to satisfy: see
	// [proofWindows.stampOrdinary]. The two grants that need a person
	// present are also never minted onto a token (internal/iamdomain) and
	// never carried by one (internal/iam/credential); this is the half that
	// holds where a verb admits the owner as themselves on no grant at all.
	g.proof.stampOrdinary(&p, now)
	if owner.Seat == "" {
		return r.WithContext(iam.WithPrincipal(ctx, p)), nil
	}
	// A BOUND OWNER ACTS AS THEIR SEAT, through the table every other
	// credential's binding goes through — followed through a rename,
	// refused NAMING the seat once it is gone, unknown where this node
	// cannot say.
	binding := session.ResolveSeat(ctx, g.machine.chart, session.PersonRow{
		Found: true, Stage: owner.Stage, Login: owner.Login,
		Seat: owner.Seat, SeatAt: owner.SeatAt,
	})
	switch binding.Answer() {
	case session.AnswerUnavailable:
		log.WarnContext(ctx, "api_token_seat_unavailable",
			"credential", presented.ID, "seat", owner.Seat,
			"detail", binding.Detail, "error", errText(binding.Err))
		return r.WithContext(iam.WithUnresolved(ctx, errBindingUnavailable)), nil
	case session.AnswerServe:
		// THE KIND FOLLOWS THE SEAT, for [Guard.principalFor]'s reason:
		// [iam.ActorFor] splits a person on their seat and a machine has
		// no such split, so a write from a bound credential lands under
		// the seat.
		p.Kind, p.Seat = iam.KindPerson, binding.Handle()
		p.SeatAt, p.Position = owner.SeatAt, binding.Seat.Unit
		return r.WithContext(iam.WithPrincipal(ctx, p)), nil
	}
	p.SeatAt = owner.SeatAt
	return r.WithContext(iam.WithPrincipal(ctx, p)), seatRefusal(binding)
}
