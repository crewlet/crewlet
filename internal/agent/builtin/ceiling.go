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

// WHO A SEAT'S TURN IS ACTING FOR — the requester ceiling ([authz.Ceiling]),
// which every call a seat makes inside a turn somebody woke is decided against
// at the one gate (authority.go): the app every app verb names, for every
// family alike ([authz.DecideWithinApps], ADR-0105), and the relation the
// families whose objects carry one decide beside it — the artifacts tools and
// their datastore's on the stored artifact ([authz.DecideWithin]), and the code
// tools on a repository's grant as well (codetools.go).
//
// # One composition, every family's
//
// An agent reads what it may read. Somebody who may not can still message an
// agent that may, and ask it to read the thing out — so inside a turn
// somebody woke, every family asks again for that somebody. WHO that is is one
// question with one answer, composed here once: were each family to compose
// its own, a turn woken by a sender nobody maps to would be a stranger's to
// one family and nobody's to the next, and the second would read for the
// stranger whatever the agent itself may.
//
// # The turn's, and no further
//
// The answer is read off what woke THIS turn (turnctx.Turn.Requester and
// Senders), so it holds this turn alone: a colleague the seat asks with
// a2a_ask answers in a turn the asking seat woke, decided for that seat and
// not for whoever woke it, and the answer wakes the seat in a turn of its own
// work. That hop is the ceiling's current edge: carrying a requester into the
// turns a turn sets off means every ask, answer and self-filed item carrying
// it, which crewlet#798's second piece does; until then the docs say where the
// bound ends (docs/concepts/gitlet.md, "Reads made for somebody else").

// Requesters composes the principal a seat's turn is acting for.
type Requesters interface {
	// Requester is the seat handle's principal as the authority table
	// decides it: the person bound to a human seat, or the agent seat
	// itself. An error is UNKNOWN, never "nobody".
	Requester(ctx context.Context, handle string) (iam.Principal, error)

	// Credential is a credential acting as no seat, by its login, as THIS
	// node resolves it now — a Tier A token of this node's own (a `token:`
	// login), or a service account of the identity directory (any other) —
	// and whether this node can place it at all: false for a login neither
	// names, one bound to a seat, and one whose stage may not act, which are
	// nobody the company knows as a requester. An error is UNKNOWN, never
	// "nobody".
	Credential(ctx context.Context, login string) (iam.Principal, bool, error)
}

// errNoRequesters is a ceiling asked of a surface that composes nobody a turn
// is acting for: a wiring that decided nothing, answered as a ceiling this
// node could not compose rather than a turn nobody woke.
var errNoRequesters = errors.New("builtin: this surface composes nobody a turn is acting for")

// ceilingOf is who a seat's turn is acting for: nobody beyond the seat for a
// turn nobody woke, for one the outcome of its own act woke and for a call
// outside any turn; the requester a known principal woke it as — never the
// seat itself — whether a seat's or a credential's acting as no seat
// ([credentialCeiling]); or a stranger, a sender the turn names with no
// requester the company knows behind it. A KNOWN REQUESTER IS READ BEFORE THE SENDERS: a
// person's message carries both their seat and their own identity as its
// sender. An error is a requester this node could not compose, which is
// UNKNOWN: never read as a turn nobody woke, which is the one kind that bounds
// nothing.
//
// THE SEAT'S OWN VOICE IS NO STRANGER'S. The outcome of a seat's own act — a
// workflow or a pipeline its own push failed, which a code host tells the
// actor of — wakes it with itself as the sender, and that is its own work.
// Every OTHER sender of a turn no known requester woke is a stranger, beside
// the seat's own voice or a colleague's: a thread somebody the company does
// not know started is theirs, whoever else speaks in it.
func ceilingOf(ctx context.Context, turn *turnctx.Turn, seat string, r Requesters) (authz.Ceiling, error) {
	// THE CALL'S OWN, where its gate bound one ([bindCeiling]): composed
	// once for every decision the call makes, whichever family asks.
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

// bindCeiling binds to one call's context the ceiling of the turn it is made
// in, for a seat's call: every decision the call makes — the gate's of the
// tool's own row, every row the tool asks inside itself, and the relation a
// family decides for the requester — reads the one composition, made at most
// once and only when a decision first needs it, so a call that decides no app
// and no relation for anybody reads nothing of the identity directory. It is
// bound per call, as the principal already is ([Authorizer]), and never
// captured: the next call composes afresh, so an administrator's revocation
// applies at the turn's next call (ADR-0105).
//
// A call that is not a seat's binds nothing: a principal acting through their
// own credential is no deputy. A seat's call binds even with no composer to
// ask: a turn nobody woke is still nobody's, and one somebody woke is then a
// ceiling this node cannot compose — undecidable for every decision that
// needs it, never read as a turn nobody woke.
func bindCeiling(ctx context.Context, turn *turnctx.Turn, r Requesters) context.Context {
	p, how := iam.From(ctx)
	if how != iam.Resolved || p.Kind != iam.KindSeat {
		return ctx
	}
	// THE TURN'S OWN SEAT, whose voice is never its requester, as every
	// family names it when it asks [ceilingOf] itself.
	seat := p.Seat
	if handle := turn.Handle(); handle != "" {
		seat = handle
	}
	return context.WithValue(ctx, ceilingKey{}, boundCeiling(sync.OnceValues(func() (authz.Ceiling, error) {
		return composeCeiling(ctx, turn, seat, r)
	})))
}

// OutsideAnyTurn binds to ctx the ceiling of a call a seat makes outside any
// turn: nobody beyond the seat itself. A seat's decision with nothing bound is
// refused ([Decide]) — the closed end, so a path that skipped the gate is a
// refusal somebody sees rather than every app granted in silence — and this is
// how a caller that decides a seat's call with no turn on purpose says so:
// what a seat is offered before any call ([Withheld]), and a case that calls a
// tool directly.
func OutsideAnyTurn(ctx context.Context) context.Context {
	return context.WithValue(ctx, ceilingKey{}, boundCeiling(func() (authz.Ceiling, error) {
		return authz.Ceiling{Kind: authz.CeilingNone}, nil
	}))
}

// credentialCeiling is the ceiling of a turn a credential acting as no seat
// woke ([turnctx.Turn.RequesterLogin]): that credential, using the apps its
// writes carried AND the apps this node resolves it to now, whichever is
// narrower — never admin, and no app either side lacks (ADR-0105).
//
// BOTH, because each alone is wrong. The carried apps are what the node that
// took the write resolved, and Tier A is per node: read alone, a token this
// node's operator narrowed would act here on another node's wider grants. The
// running node's resolution is what the credential is now: read alone, a
// write recorded before the credential was narrowed would carry the old
// grants, and a token another node gave wider grants would be widened here.
//
// A STRANGER, never nobody, where either answer is empty: a write that
// carries no apps (an older build's), and a login this node cannot place — a
// Tier A token its own configuration does not name, a service account the
// directory does not hold or holds removed or suspended, or a credential bound
// to a seat, whose writes are the person's and never reach here.
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

// composeCeiling is [ceilingOf] composed from the turn itself.
func composeCeiling(ctx context.Context, turn *turnctx.Turn, seat string, r Requesters) (authz.Ceiling,
	error) {

	if turn == nil {
		return authz.Ceiling{Kind: authz.CeilingNone}, nil
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
	if slices.ContainsFunc(turn.Senders, func(s types.CanonicalIdentity) bool {
		// AN EMPTY HANDLE IS NOBODY'S, never the seat's: a seat the
		// caller named as "" must not make every unknown sender its own.
		handle := strings.TrimSpace(s.Handle)
		return handle == "" || handle != seat
	}) {
		return authz.Ceiling{Kind: authz.CeilingStranger}, nil
	}
	return authz.Ceiling{Kind: authz.CeilingNone}, nil
}
