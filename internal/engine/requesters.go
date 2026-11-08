package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// turnRequesters composes the principal a seat's turn is acting for, as the
// authority table decides it — the requester ceiling every call a seat makes
// inside a turn somebody woke is decided against (builtin.Requesters): the app
// every app verb asks, and the relation the artifacts tools, their
// datastore's and the code tools ask beside it. ONE value for all of them, so
// a requester is one principal whichever tool decides for it.
//
// AN AGENT SEAT is the seat principal its own turn acts as (every app, never
// admin). A PERSON'S SEAT is the person bound to it, at their stage, holding
// the apps their own grants name and never admin: the routed identity
// directory answers who holds the seat, at what stage and with what grants —
// that ONE seat's requester, asked by its handle — so the ceiling bounds an
// agent by the apps the requester uses (ADR-0108), by their RELATIONS to what
// it reads — an artifact's shares, units and leads, a repository's grant for a
// seat — and by whether they may act at all. Admin is never part of a
// ceiling: an administrator who asks an agent for somebody's private artifact
// asks it as a colleague, and uses every app because admin includes them.
//
// A PERSON NO DIRECTORY ROW BINDS — a seat the chart knows by its contacts
// alone, a removal — holds no app: a permission is a person's (ADR-0033), and
// a seat nobody holds has nobody to hold one. An administrator creates the
// person on the seat, with the apps they use, to have agents act for them; the
// create lands them active, and they need never sign in.
//
// A CREDENTIAL ACTING AS NO SEAT is the machine principal THIS node resolves
// its login to now ([turnRequesters.Credential]), holding the apps its grants
// name and never admin — which the ceiling narrows again to the apps its
// write carried.
type turnRequesters struct{ engine *Engine }

// Credential is a credential acting as no seat, by its login, as THIS node
// resolves it now (builtin.Requesters.Credential): a machine principal holding
// the apps its grants name, never admin, and whether this node can place it.
//
// BY ITS LOGIN'S NAMESPACE, and only there. A `token:` login is a Tier A token
// of this node's own configuration and nothing else: a token another node
// declares is nobody here, and a directory row that happens to carry the same
// login — a binding the identity directory enrols under it — never stands in
// for the configuration. That row is read for one fact alone, whether it binds
// the token to a seat, which makes the token's writes the person's: such a
// login never reaches here as a credential acting as itself, so it is placed
// as nobody. Any other login is a service account of the identity directory
// and nothing else: active or suspended, never removed, and bound to no seat.
//
// THE DECLARATION'S APPS, NOT THE ADMIN POSTURE'S CLAMP: the posture takes
// admin alone away, which no ceiling carries, so the apps are the same on
// every node — one that serves no API, whose zero posture confers nothing on
// a request, included.
//
// Not placed — false with no error — for a login neither names, a token bound
// to a seat, and a service account removed, suspended, bound or not a machine:
// nobody the company knows as a requester, whom the turn is worked for as a
// stranger. An error is a directory this node could not read or vouch for,
// which is UNKNOWN: never a stranger, whom a lagging copy could otherwise make
// of an account still active, nor a credential it might have removed.
func (r turnRequesters) Credential(ctx context.Context, login string) (iam.Principal, bool, error) {
	dir, ok := r.engine.identityDirectory()
	if !ok {
		return iam.Principal{}, false, errNoDirectory
	}
	if _, routed := dir.(routedIdentity); !routed {
		// ONE KEYED READ OF THIS NODE'S OWN STORE, bounded as a request's
		// is ([requestReadBudget]); a routed one is bounded by the router.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestReadBudget)
		defer cancel()
	}
	return credentialOf(ctx, dir, r.engine.tierATokens, login)
}

// credentialOf is [turnRequesters.Credential] over its two seams: the identity
// directory this node reads, and this node's Tier A tokens with the grants each
// declares.
func credentialOf(ctx context.Context, dir bindingDirectory, tokens map[string][]iam.Grant,
	login string) (iam.Principal, bool, error) {

	if id, isToken := strings.CutPrefix(login, iam.TokenLoginPrefix); isToken {
		grants, declared := tokens[id]
		if !declared {
			return iam.Principal{}, false, nil
		}
		bound, err := boundRowOf(ctx, dir, login)
		switch {
		case err != nil:
			return iam.Principal{}, false, err
		case bound.Found:
			return iam.Principal{}, false, nil
		}
		return iam.Principal{Kind: iam.KindMachine, Login: login, Stage: iam.StageActive,
			Grants: iam.AppsOf(grants)}, true, nil
	}
	seen, vouch, err := dir.PersonByLoginVouched(ctx, login)
	switch {
	case err != nil:
		return iam.Principal{}, false, fmt.Errorf("engine: read the directory's row "+
			"for %s: %w", login, err)
	case seen.ID == "" || seen.Kind != iam.KindMachine || seen.Seat != "" || !seen.Stage.MayAct():
		return iam.Principal{}, false, nil
	case vouch.Lag > statelog.StallGrace:
		return iam.Principal{}, false, fmt.Errorf("engine: can't confirm what %s may "+
			"use: this node is %s behind on identity changes (limit %s)",
			login, vouch.Lag, statelog.StallGrace)
	case vouch.Deferred:
		return iam.Principal{}, false, fmt.Errorf("engine: can't confirm what %s may "+
			"use: this node holds an identity change it can't read", login)
	}
	p := iam.Principal{Kind: iam.KindMachine, Login: seen.Login, Stage: seen.Stage,
		Grants: iam.AppsOf(seen.Grants)}
	if id, err := uuid.Parse(seen.ID); err == nil {
		p.ID = id
	}
	return p, true, nil
}

// errNoSuchSeat is a requester's handle the organisation holds no seat for:
// a definitive answer, which a caller asking on somebody's behalf long after
// they asked (the merge queue's close, codequeue.go) tells apart from a read
// that failed.
var errNoSuchSeat = errors.New("engine: the organisation holds no seat")

func (r turnRequesters) Requester(ctx context.Context, handle string) (iam.Principal, error) {
	seat, found, err := SeatViewOf(r.engine).Seat(ctx, handle)
	switch {
	case err != nil:
		return iam.Principal{}, err
	case !found:
		return iam.Principal{}, fmt.Errorf("%w %q", errNoSuchSeat, handle)
	}
	p := iam.Principal{Seat: seat.Handle, Position: seat.Unit, Stage: iam.StageActive}
	if org.RoleKind(seat.Kind) != org.KindHuman {
		p.Kind, p.Grants = iam.KindSeat, slices.Clone(iam.Apps)
		return p, nil
	}
	p.Kind = iam.KindPerson
	if r.engine.router == nil {
		return iam.Principal{}, errors.New("engine: this node reaches no identity directory")
	}
	// ONE SEAT'S REQUESTER, never the directory: this runs at most once a
	// call of a tool that decides an app or a relation for somebody, so it is
	// the routed point read (two seeks and one document's decode on the data
	// node that answers), whose cost does not grow with the people the company
	// has hired or ever removed.
	h, held, err := r.engine.router.Identity().Requester(ctx, seat.Handle)
	switch {
	case err != nil:
		return iam.Principal{}, err
	case held && h.Removed:
		// THE PERSON WHO HELD IT WAS REMOVED, and nobody has held it
		// since: whoever speaks as the seat now acts as nobody.
		p.Stage = iam.StageRetired
		return p, nil
	case held:
		// THE PERSON BY THEIR ID AND SEAT: every artifact relation of a
		// person is decided on the seat they hold (a person's artifacts
		// are owned under it), so no login is needed for the ceiling.
		if id, err := uuid.Parse(h.Person); err == nil {
			p.ID = id
		}
		p.Stage = h.Stage
		if p.Stage.MayAct() {
			p.Grants = iam.AppsOf(h.Grants)
		}
		return p, nil
	}
	// A SEAT NOBODY HOLDS IN THE DIRECTORY is a person the chart knows by
	// their contacts alone: their seat's shares are theirs, and no app is.
	return p, nil
}
