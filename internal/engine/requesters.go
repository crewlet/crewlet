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
// authority table decides it — the requester ceiling every family of tools
// that reads or writes for somebody decides again (builtin.Requesters): the
// artifacts tools and their datastore's, and the code tools. ONE value for all
// of them, so a requester is one principal whichever tool reads for it.
//
// AN AGENT SEAT is the seat principal its own turn acts as (every app, never
// admin). A PERSON'S SEAT is the person bound to it, at their stage, holding
// every app and never admin: the routed identity directory answers who holds
// the seat and at what stage — that ONE seat's holder, asked by its handle —
// and never the grants (they are decided on the node that composes a
// session), so the ceiling bounds an agent by the requester's RELATIONS to
// what it reads — an artifact's shares, units and leads, a repository's grant
// for a seat — and by whether they may act at all. Admin is
// never part of a ceiling: an administrator who asks an agent for somebody's
// private artifact asks it as a colleague.
type turnRequesters struct{ engine *Engine }

func (r turnRequesters) Requester(ctx context.Context, handle string) (iam.Principal, error) {
	seat, found, err := SeatViewOf(r.engine).Seat(ctx, handle)
	switch {
	case err != nil:
		return iam.Principal{}, err
	case !found:
		return iam.Principal{}, fmt.Errorf("engine: the organisation holds no seat %q", handle)
	}
	p := iam.Principal{Seat: seat.Handle, Position: seat.Unit, Stage: iam.StageActive,
		Grants: slices.Clone(iam.Apps)}
	if org.RoleKind(seat.Kind) != org.KindHuman {
		p.Kind = iam.KindSeat
		return p, nil
	}
	p.Kind = iam.KindPerson
	if r.engine.router == nil {
		return iam.Principal{}, errors.New("engine: this node reaches no identity directory")
	}
	// ONE SEAT'S HOLDER, never the directory: this runs on every call of a
	// tool that reads or writes for somebody, so it is the routed point read
	// (two seeks on the data node that answers), whose cost does not grow
	// with the people the company has hired or ever removed.
	h, held, err := r.engine.router.Identity().SeatHolder(ctx, seat.Handle)
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
		return p, nil
	}
	// A SEAT NOBODY HOLDS IN THE DIRECTORY is a person the chart knows by
	// their contacts alone: their seat's shares are theirs.
	return p, nil
}
