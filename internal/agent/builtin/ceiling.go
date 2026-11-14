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
// family alike ([authz.DecideWithinApps], ADR-0109), and the relation the
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
// # The turn's, and every turn it hands on to
//
// The answer is read off what woke THIS turn (turnctx.Turn.WorkingFor, then
// Requester and Senders). A colleague the seat asks with a2a_ask answers in a
// turn worked for whoever this turn works for ([types.WorkingFor], composed
// from this call's own ceiling by [workingForOf]), and the answer back wakes
// the seat in a turn worked for them too, so a chain of asks never widens.
// So does a WRITE that wakes another seat on the engine's own surfaces (an
// item filed or assigned, a work-item comment, a page or a comment on one, a
// Chatlet message, and on Gitlet a change, a review, a comment, a push, a
// merge or an enqueue): every actor a seat's turn builds states whom it
// writes for ([writtenForTurn], from the same binding), and so does every
// request a code tool forms from its caller's actor; the store, or the node
// deciding a code write, refuses an agent's write that states nothing
// (types.WrittenFor), and the record carries it to the turn its wake starts,
// as do the engine's records about a seat's earlier push on Gitlet (a
// revision's facts, a failed check of its head, a landing its home found),
// read back from the push's rows, and what Gitlet's merge queue records about
// a seat's earlier enqueue (its landing, an ejection and a close it could not
// make), read back from the entry's row — whose close of a landed change's
// item is decided within the same ceiling, composed by [WorkingForCeiling].
// The ceiling's edge is what carries no requester: a vendor's surface, whose
// wake comes back by webhook; the docs say where the bound ends
// (docs/concepts/gitlet.md, "Reads made for somebody else").

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
// applies at the turn's next call (ADR-0109).
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
// narrower — never admin, and no app either side lacks (ADR-0109).
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

// CeilingOfTurn is the requester ceiling of turn, composed as every tool
// call of it composes its own: for a decision the engine makes for the turn
// before any call, such as what its prompt may show it (the atlas's entries
// its work names), held to the one composer the gate uses.
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
	// WHOEVER A HAND-ON CARRIED, read first: a colleague's ask or the answer
	// to this seat's own works for whoever the handing turn worked for,
	// through every colleague it passes, and never for the seat that asked
	// or answered.
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
		// AN EMPTY HANDLE IS NOBODY'S, never the seat's: a seat the
		// caller named as "" must not make every unknown sender its own.
		handle := strings.TrimSpace(s.Handle)
		return handle == "" || handle != seat
	})
}

// WorkingForCeiling is the ceiling of a turn a hand-on woke, from whoever it
// carried ([types.WorkingFor]), composed again on this node: a seat requester
// as this node's directory holds them now, so a revocation since the hand-on
// applies (and the seat itself, carried back to it, is its own work); a
// credential narrowed again by this node's resolution ([credentialCeiling]),
// never widened; a stranger as one. A kind this build does not know is a
// stranger's, the closed end.
//
// EXPORTED FOR THE ONE WRITE THE ENGINE MAKES FOR A TURN AFTER IT ENDED: the
// merge queue's close of a landed change's work item, the enqueuing turn's
// write made late, which internal/engine decides within the ceiling whom the
// enqueue was made for composes now — this composition, never a second one,
// so the close and every call of a turn read one requester from one carrier
// alike. A requester whose seat the organisation no longer holds fails here
// with the composer's own error wrapped (%w), which that caller tells from a
// read that failed.
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

// workingForOf is who a turn's work handed on to a colleague works for
// ([types.WorkingFor]), on an ask and on every write the turn makes
// ([writtenForTurn]), from the turn's own ceiling, which the caller reads
// from the call's binding ([ceilingOf]): nil for a turn that works for its own
// seat; the requester's seat for a person or a colleague seat; the credential
// with the apps the ceiling holds it to, already narrowed by this node; a
// stranger for a stranger, and for any ceiling this build cannot read.
//
// FROM THE BOUND CEILING AND NOTHING ELSE, so the carrier is the same rule
// every family decides the turn's own calls by and cannot drift from it, and
// nothing the model writes reaches it.
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
