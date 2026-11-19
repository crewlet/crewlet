package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

// HOW A MACHINE TOKEN BECOMES A PRINCIPAL: the guard's THIRD credential arm.
// A `cwl_pat_…` token, minted through `/iam/credentials`, is a person's
// assistant acting as them or a declared service account.
//
// Nothing here decides anything: the rows' meaning is internal/iam/credential's
// ([credential.CheckToken]) over ONE snapshot, the owner's seat is
// [session.ResolveSeat]'s. This file composes the owner's principal with the
// minted grants the owner STILL holds under this node's admin posture.
// UNKNOWN IS 503 AND NEVER 401, for the session table's reason: a pipeline
// reading 401 discards a good credential, and a node behind the identity log
// (one that has not applied the very mint) cannot establish anything is wrong.

// TokenDirectory is the one read a machine token needs.
//
// Defined by the caller, one method wide, answered in one snapshot; AN ERROR IS
// THE UNKNOWN ARM, always.
//
// It is handed the PRESENTED TOKEN, not its id, because the rows may be another
// node's: a node with no identity rows asks a data node, and what crosses back
// carries no verifier, only a MAC the presented secret is checked against where
// it was presented (internal/estate's TokenFacts). So the row
// [credential.CheckToken] reads holds the verifier only for the presented
// token. A surface holding only the token's NAME ([Lineages]'s view ticket)
// hands it by id and gets no verifier, which [credential.CheckHeld] never reads.
type TokenDirectory interface {
	MachineToken(ctx context.Context, presented credential.Token) (credential.TokenRow, error)
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

	// Chart resolves a bound owner's seat. REQUIRED, and the engine's
	// adapter on a node running no company yet answers every seat question
	// UNKNOWN, never the seatless arm.
	Chart session.Chart

	// Now is the clock. Nil takes UTC wall time.
	Now func() time.Time
}

// NewTokens builds the arm, or says which half is missing.
func NewTokens(deps TokensDeps) (*Tokens, error) {
	switch {
	case deps.Directory == nil:
		return nil, errors.New("auth: the token arm needs the identity " +
			"directory: a token's secret says nothing until a row says whose " +
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
// CALLED ONCE, AT WIRING TIME, for [Guard.BindSeats]' reason. Nil resolves no
// machine token (a suite's guard) and a token-shaped bearer is refused like any
// unheld one. A running node installs it: every API node holds the estate
// (ingress requires `data`, ADR-0025) and applies the identity directory from
// boot, so a token minted on a peer resolves here, and a node that has not yet
// applied its mint answers 503, not 401.
func (g *Guard) WithTokens(t *Tokens) *Guard {
	g.machine = t
	return g
}

// PresentedToken is the id of the machine token this request carried as its
// bearer, if it carried one.
//
// Asked by the routes that must know what the authority table cannot decide:
// minting a token from a token would let its holder extend it for ever with
// nobody present, and a password step-up has nobody to confirm. Gestures that
// need a person present are the TABLE's to refuse a token
// ([iam.Principal.MachineToken], read by internal/authz), never a route's.
//
// Read off [iam.Principal.Via], the one place the arm records it: a second
// carrier on the context would be a second answer to "through what".
func PresentedToken(ctx context.Context) (string, bool) {
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return "", false
	}
	return principal.MachineToken()
}

// errTokenUnavailable is the reason attached to a request whose machine token
// this node could not check.
var errTokenUnavailable = errors.New("auth: this node could not establish " +
	"whether this token is live")

// token resolves a request presenting a machine token.
func (g *Guard) token(r *http.Request, presented credential.Token) (*http.Request, *Refusal) {

	ctx := r.Context()
	now := g.machine.now()
	// THE TOKEN WHOLE, its secret included — see [TokenDirectory].
	row, err := g.machine.directory.MachineToken(ctx, presented)
	if err != nil {
		log.Log(ctx, UnreadLevel(ctx), "api_token_unavailable",
			"credential", presented.ID, "error", err)
		return r.WithContext(iam.WithUnresolved(ctx, errTokenUnavailable)), nil
	}
	check := credential.CheckToken(presented, row, now, statelog.StallGrace)
	switch check.Answer {
	case credential.TokenValid:
	case credential.TokenUnknown:
		log.Log(ctx, UnreadLevel(ctx), "api_token_unavailable",
			"credential", presented.ID, "detail", check.Detail)
		return r.WithContext(iam.WithUnresolved(ctx, errTokenUnavailable)), nil
	default:
		// PRESENT AND NO GOOD IS ANONYMOUS, and marked like any other
		// refused bearer, which a guarded route's refusal counts as a
		// failed attempt — the log carries which fact decided, the caller
		// one code for all of them.
		log.InfoContext(ctx, "api_token_refused",
			"credential", presented.ID, "detail", check.Detail)
		return r.WithContext(refusedCredential(iam.WithAnonymous(ctx))), nil
	}

	// A MACHINE TOKEN ENDS ON ITS OWN AT ITS EXPIRY — see [Lifetime].
	ctx = withLifetime(ctx, row.ExpiresAt)
	p, refusal, unresolved := g.machinePrincipal(ctx, presented.ID, row, now)
	if unresolved != nil {
		return r.WithContext(iam.WithUnresolved(ctx, unresolved)), nil
	}
	return r.WithContext(iam.WithPrincipal(ctx, p)), refusal
}

// machinePrincipal composes the principal a machine token whose rows checked
// out acts as: its owner, carrying the grants the mint conferred that the
// owner still holds, under this node's admin posture, as their seat where they
// are bound to one. refusal is a binding the chart will not let it act as,
// and unresolved the reason this node could not say.
//
// ONE COMPOSITION for a token presented as a bearer and a token a view ticket
// names ([Lineages]), so the two can never compose one token two ways.
func (g *Guard) machinePrincipal(ctx context.Context, id string, row credential.TokenRow,
	now time.Time) (p iam.Principal, refusal *Refusal, unresolved error) {

	owner := row.Owner
	ownerID, err := uuid.Parse(owner.ID)
	if err != nil {
		// A ROW THIS ENGINE WROTE carries a uuid it minted, so this is a
		// store that is not answering the question it was asked.
		log.WarnContext(ctx, "api_token_owner_unreadable",
			"credential", id, "error", err)
		return iam.Principal{}, nil, errTokenUnavailable
	}
	p = iam.Principal{
		ID: ownerID, Login: owner.Login, Kind: owner.Kind, Stage: owner.Stage,
		// THE MINT'S GRANTS THE OWNER STILL HOLDS, under this node's
		// admin posture on this request.
		Grants: g.admin.Effective(row.EffectiveGrants()),
		// AND THROUGH WHAT. The token acts as its owner, so without this
		// every write it made was recorded exactly as the owner's own —
		// and a token minted on somebody's account could file, edit and
		// close their work with no row saying a token was used. The
		// author stays the owner; the operator column names the token.
		Via: iam.MachineTokenName(id),
	}
	// STEPPED UP BY CONSTRUCTION, as every non-interactive credential is.
	//
	// WHAT KEEPS IT OFF THE GESTURES THAT NEED A PERSON PRESENT is neither a
	// proof's age nor a missing permission: a token minted with admin acts
	// with admin. Those gestures say so on their row in internal/authz, and
	// the table refuses the credential [iam.Principal.Via] names (read
	// through [iam.Principal.MachineToken]), which also closes the ones a
	// verb admits the owner to as THEMSELVES on no permission. And
	// internal/iamdomain refuses every administrative identity record whose
	// operator is a machine token, whatever the route decided, so a leaked
	// token never rewrites who may act.
	g.proof.stamp(&p, now)
	if owner.Seat == "" {
		return p, nil, nil
	}
	// A BOUND OWNER ACTS AS THEIR SEAT, through the table every other
	// credential's binding goes through — refused NAMING the seat once it
	// is gone, unknown where this node cannot say.
	binding := session.ResolveSeat(ctx, g.machine.chart, session.PersonRow{
		Found: true, Stage: owner.Stage, Login: owner.Login,
		Seat: owner.Seat,
	})
	switch binding.Answer() {
	case session.AnswerUnavailable:
		log.Log(ctx, UnreadLevel(ctx), "api_token_seat_unavailable",
			"credential", id, "seat", owner.Seat,
			"detail", binding.Detail, "error", errText(binding.Err))
		return iam.Principal{}, nil, errBindingUnavailable
	case session.AnswerServe:
		// THE KIND FOLLOWS THE SEAT, for [Guard.principalFor]'s reason:
		// [iam.ActorFor] splits a person on their seat and a machine has
		// no such split, so a write from a bound credential lands under
		// the seat.
		p.Kind, p.Seat = iam.KindPerson, binding.Handle()
		p.Position = binding.Seat.Unit
		return p, nil, nil
	}
	return p, seatRefusal(binding), nil
}
