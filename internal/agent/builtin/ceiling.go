package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// WHO A SEAT'S TURN IS ACTING FOR: the requester ceiling ([authz.Ceiling]),
// so somebody who may not read a thing cannot have an agent read it out. Every
// call a seat makes in a turn somebody woke is decided against it at the one
// gate (authority.go): the app ([authz.DecideWithinApps], ADR-0109) and, where
// a family's objects carry one, the relation ([authz.DecideWithin];
// codetools.go). It is composed here once: per family, an unmapped sender's
// turn could be a stranger's to one family and nobody's to the next, which
// reads for the stranger all the agent may. It never widens down a hand-on: an
// a2a_ask, its answer and every write that wakes another seat carry it
// ([workingForOf], [writtenForTurn]), and the store refuses an agent's write
// stating nothing (types.WrittenFor). A vendor's webhook wake is its edge
// (docs/concepts/gitlet.md, "Reads made for somebody else").

// Requesters composes the principal a seat's turn is acting for.
type Requesters interface {
	// Requester is the seat handle's principal as the authority table
	// decides it: the person bound to a human seat, or the agent seat
	// itself. An error is UNKNOWN, never "nobody".
	Requester(ctx context.Context, handle string) (iam.Principal, error)

	// Credential is a credential acting as no seat, by its login, as THIS
	// node resolves it now (a `token:` login is this node's Tier A token, any
	// other a directory service account), and whether this node can place it:
	// false for a login neither names, one bound to a seat, or one whose stage
	// may not act. An error is UNKNOWN, never "nobody".
	Credential(ctx context.Context, login string) (iam.Principal, bool, error)
}

// errNoRequesters is a ceiling asked of a surface wired with no composer:
// a ceiling this node could not compose, never a turn nobody woke.
var errNoRequesters = errors.New("builtin: this surface composes nobody a turn is acting for")

// ceilingOf is who a seat's turn is acting for: nobody beyond the seat for a
// turn nobody woke, one its own act's outcome woke with itself as sender (a
// code host telling the actor its push failed) and a call outside any turn;
// else the known requester that woke it, never the seat itself
// ([credentialCeiling] for a credential acting as no seat), read before the
// senders since a person's message names both their seat and themselves; else
// a stranger, for any sender but the seat with no known requester, even beside
// a colleague's voice. An error is UNKNOWN, never read as a turn nobody woke,
// the one kind that bounds nothing.
func ceilingOf(ctx context.Context, turn *turnctx.Turn, seat string, r Requesters) (authz.Ceiling, error) {
	// The call's own where its gate bound one ([bindCeiling]).
	if bound, ok := ctx.Value(ceilingKey{}).(boundCeiling); ok {
		return bound()
	}
	return composeCeiling(ctx, turn, seat, r)
}

// boundCeiling is the requester ceiling of one call, composed at most once
// and only when a decision of the call first needs it ([bindCeiling]).
type boundCeiling func() (authz.Ceiling, error)

// ceilingKey is the context key a call's [boundCeiling] rides under.
type ceilingKey struct{}

// bindCeiling binds to a seat's call the ceiling of its turn: every decision
// the call makes (the gate's, every row the tool asks, a family's relation)
// reads one composition, made at most once and only when first needed, so a
// call deciding nothing reads nothing of the identity directory. Bound per
// call and never captured, as the principal is ([Authorizer]), so an
// administrator's revocation applies at the turn's next call (ADR-0109).
//
// A non-seat call binds nothing: a principal on their own credential is no
// deputy. A seat's call binds even with no composer: a turn somebody woke is
// then undecidable, never read as a turn nobody woke.
func bindCeiling(ctx context.Context, turn *turnctx.Turn, r Requesters) context.Context {
	p, how := iam.From(ctx)
	if how != iam.Resolved || p.Kind != iam.KindSeat {
		return ctx
	}
	// The turn's own seat, as every family names it to [ceilingOf].
	seat := p.Seat
	if handle := turn.Handle(); handle != "" {
		seat = handle
	}
	return context.WithValue(ctx, ceilingKey{}, boundCeiling(sync.OnceValues(func() (authz.Ceiling, error) {
		return composeCeiling(ctx, turn, seat, r)
	})))
}

// OutsideAnyTurn binds to ctx the ceiling of a seat's call outside any turn:
// nobody beyond the seat. A seat's decision with nothing bound is refused
// ([Decide]), so a path that skipped the gate is a visible refusal rather than
// every app granted; this is how a caller deciding with no turn on purpose
// says so ([Withheld], a case calling a tool directly).
func OutsideAnyTurn(ctx context.Context) context.Context {
	return context.WithValue(ctx, ceilingKey{}, boundCeiling(func() (authz.Ceiling, error) {
		return authz.Ceiling{Kind: authz.CeilingNone}, nil
	}))
}

// credentialCeiling is the ceiling of a turn a credential acting as no seat
// woke ([turnctx.Turn.RequesterLogin]): that credential, held to the apps both
// its writes carried AND this node resolves it to now; never admin
// (ADR-0109). Both, because Tier A is per node and grants change: the carried
// apps alone would act here on another node's wider grants, or on grants
// narrowed since; this node's alone would widen a token it grants more.
//
// A STRANGER, never nobody, where either is empty: a write carrying no apps,
// or a login this node cannot place (an unnamed Tier A token, a service
// account missing, removed or suspended, or a seat-bound credential, whose
// writes are the person's and never reach here).
func credentialCeiling(ctx context.Context, login string, carried []iam.Grant,
	r Requesters) (authz.Ceiling, error) {

	if len(carried) == 0 {
		return authz.Ceiling{Kind: authz.CeilingStranger}, nil
	}
	if r == nil {
		return authz.Ceiling{}, errNoRequesters
	}
	credential, known, err := r.Credential(ctx, login)
	switch {
	case err != nil:
		return authz.Ceiling{}, fmt.Errorf("builtin: compose requester %s: %w", login, err)
	case !known:
		return authz.Ceiling{Kind: authz.CeilingStranger}, nil
	}
	now := iam.AppsOf(credential.Grants)
	credential.Grants = slices.DeleteFunc(iam.AppsOf(carried), func(app iam.Grant) bool {
		return !slices.Contains(now, app)
	})
	return authz.Ceiling{Kind: authz.CeilingRequester, Principal: credential}, nil
}

// CeilingOfTurn is the requester ceiling of turn, by the one composer the
// gate uses, for a decision the engine makes before any call (such as which
// atlas entries its prompt may show).
func CeilingOfTurn(ctx context.Context, turn *turnctx.Turn, r Requesters) (authz.Ceiling, error) {
	seat := ""
	if turn != nil && turn.Seat != nil {
		seat = turn.Seat.Handle()
	}
	return composeCeiling(ctx, turn, seat, r)
}

// composeCeiling is [ceilingOf] composed from the turn itself.
func composeCeiling(ctx context.Context, turn *turnctx.Turn, seat string, r Requesters) (authz.Ceiling,
	error) {

	if turn == nil {
		return authz.Ceiling{Kind: authz.CeilingNone}, nil
	}
	// A hand-on's carrier first: an ask, or the answer to one, works for
	// whoever the handing turn did, never the seat that asked or answered.
	if w := turn.WorkingFor; w != nil {
		return WorkingForCeiling(ctx, *w, seat, r)
	}
	if handle := strings.TrimSpace(turn.Requester); handle != "" && handle != seat {
		if r == nil {
			return authz.Ceiling{}, errNoRequesters
		}
		requester, err := r.Requester(ctx, handle)
		if err != nil {
			return authz.Ceiling{}, fmt.Errorf("builtin: compose requester %s: %w", handle, err)
		}
		return authz.Ceiling{Kind: authz.CeilingRequester, Principal: requester}, nil
	}
	if login := strings.TrimSpace(turn.RequesterLogin); login != "" {
		return credentialCeiling(ctx, login, turn.RequesterApps, r)
	}
	if unknownSender(turn, seat) {
		return authz.Ceiling{Kind: authz.CeilingStranger}, nil
	}
	return authz.Ceiling{Kind: authz.CeilingNone}, nil
}

// unknownSender reports whether a sender of turn is somebody other than seat
// that the company does not know.
func unknownSender(turn *turnctx.Turn, seat string) bool {
	return slices.ContainsFunc(turn.Senders, func(s types.CanonicalIdentity) bool {
		// An empty handle is nobody's, never the seat's: a seat named ""
		// must not make every unknown sender its own.
		handle := strings.TrimSpace(s.Handle)
		return handle == "" || handle != seat
	})
}

// WorkingForCeiling is the ceiling of a turn a hand-on woke, composed again on
// this node from whoever it carried ([types.WorkingFor]): a seat requester as
// the directory holds them now, so a revocation since applies (the seat
// itself, carried back, is its own work); a credential narrowed again, never
// widened ([credentialCeiling]); a stranger, or a kind this build does not
// know, as a stranger.
//
// Exported for internal/engine's merge-queue close of a landed change's work
// item, a write made after the enqueuing turn ended, so it reads this same
// composition, never a second one. A requester whose seat the organisation no
// longer holds fails with the composer's error wrapped (%w), which that caller
// tells from a failed read.
func WorkingForCeiling(ctx context.Context, w types.WorkingFor, seat string, r Requesters) (authz.Ceiling,
	error) {

	switch w.Kind {
	case types.WorkingForSeat:
		handle := strings.TrimSpace(w.Seat)
		switch {
		case handle == "":
			return authz.Ceiling{Kind: authz.CeilingStranger}, nil
		case handle == seat:
			return authz.Ceiling{Kind: authz.CeilingNone}, nil
		case r == nil:
			return authz.Ceiling{}, errNoRequesters
		}
		requester, err := r.Requester(ctx, handle)
		if err != nil {
			return authz.Ceiling{}, fmt.Errorf("builtin: compose requester %s: %w", handle, err)
		}
		return authz.Ceiling{Kind: authz.CeilingRequester, Principal: requester}, nil
	case types.WorkingForCredential:
		return credentialCeiling(ctx, strings.TrimSpace(w.Login), w.Apps, r)
	}
	return authz.Ceiling{Kind: authz.CeilingStranger}, nil
}

// workingForOf is whom a turn's hand-on works for ([types.WorkingFor]), on an
// ask and every write ([writtenForTurn]): nil for the turn's own seat; the
// requester's seat; a credential with the apps already narrowed here; a
// stranger for a stranger or a ceiling this build cannot read. It reads the
// call's bound ceiling ([ceilingOf]) and nothing else, so it cannot drift
// from the rule every family decides by and nothing the model writes reaches
// it.
func workingForOf(c authz.Ceiling) *types.WorkingFor {
	switch c.Kind {
	case authz.CeilingNone:
		return nil
	case authz.CeilingRequester:
		p := c.Principal
		if p.Kind == iam.KindMachine {
			return &types.WorkingFor{Kind: types.WorkingForCredential, Login: p.Login,
				Apps: iam.AppsOf(p.Grants)}
		}
		if seat := strings.TrimSpace(p.Seat); seat != "" {
			return &types.WorkingFor{Kind: types.WorkingForSeat, Seat: seat}
		}
	}
	return &types.WorkingFor{Kind: types.WorkingForStranger}
}
