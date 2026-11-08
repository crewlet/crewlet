package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
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
// agent by the apps the requester uses (ADR-0103), by their RELATIONS to what
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
type turnRequesters struct{ engine *Engine }

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
			p.Grants = appsOf(h.Grants)
		}
		return p, nil
	}
	// A SEAT NOBODY HOLDS IN THE DIRECTORY is a person the chart knows by
	// their contacts alone: their seat's shares are theirs, and no app is.
	return p, nil
}

// appsOf is the apps a person's grants let them use: every app admin includes
// where they hold admin (iam.Expand), and never admin itself, which no ceiling
// carries, nor a name this build does not know, which grants nothing.
func appsOf(grants []iam.Grant) []iam.Grant {
	return slices.DeleteFunc(iam.Expand(grants), func(g iam.Grant) bool { return !g.IsApp() })
}
