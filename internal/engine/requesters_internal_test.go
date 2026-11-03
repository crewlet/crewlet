package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

// WHO A TURN IS ACTING FOR is the seat the chart holds — ONE composition, which
// the artifacts tools and the code tools both read: an agent seat as the seat
// principal, never admin, and a seat the chart does not hold is unknown rather
// than a guess.
func TestARequesterIsComposedFromTheRunningChart(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{
		{Name: "Analyst", DeclaredHandle: "analyst"},
		{Name: "Jane", DeclaredHandle: "jane", Kind: org.KindHuman},
	}}
	o.Normalize()
	e := &Engine{}
	e.epoch.current.Store(&Company{Org: o})
	r := turnRequesters{engine: e}

	p, err := r.Requester(t.Context(), "analyst")
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != iam.KindSeat || p.Seat != "analyst" || p.Can(iam.GrantAdmin) {
		t.Errorf("an agent requester composed as %+v", p)
	}
	if _, err := r.Requester(t.Context(), "nobody"); err == nil {
		t.Error("a seat the chart does not hold composed a principal")
	}
	// A PERSON'S SEAT needs the identity directory, and a node that reaches
	// none cannot say who holds it.
	if _, err := r.Requester(t.Context(), "jane"); err == nil {
		t.Error("a person's seat composed with no identity directory")
	}
}

// oneSeatDirectory is a data node's identity directory as a turn's requester
// asks it: ONE seat's holder, by its handle, from holders. Its whole-directory
// read counts its callers and answers nothing, because a requester that reads
// it reads every binding and every tombstone the company ever wrote.
type oneSeatDirectory struct {
	holders map[string]iamdomain.SeatHolder
	err     error
	whole   *atomic.Int64
}

func (d oneSeatDirectory) PersonByLoginVouched(context.Context, string) (iamdomain.Sighting,
	iamdomain.Vouch, error) {
	return iamdomain.Sighting{}, iamdomain.Vouch{}, errors.New("not asked here")
}

func (d oneSeatDirectory) SeatHolders(context.Context) ([]iamdomain.SeatHolder, error) {
	d.whole.Add(1)
	return nil, errors.New("the whole directory was read for one seat")
}

func (d oneSeatDirectory) SeatHolder(_ context.Context, seat string) (iamdomain.SeatHolder, bool, error) {
	if d.err != nil {
		return iamdomain.SeatHolder{}, false, d.err
	}
	h, held := d.holders[seat]
	return h, held, nil
}

func (d oneSeatDirectory) MachineToken(context.Context, string) (credential.TokenRow, error) {
	return credential.TokenRow{}, errors.New("not asked here")
}

// A PERSON'S SEAT IS COMPOSED FROM THAT SEAT'S HOLDER ALONE, asked of the data
// node by its handle: the person at their stage, a removed holder as nobody
// acting, a seat the directory binds to nobody as the chart's — and never by
// reading the whole directory, which a turn would otherwise pay on every call
// of a tool that reads or writes for somebody, at a cost growing with every
// person the company has hired or removed. An unreadable directory is
// unknown, never a seat nobody holds.
func TestAPersonRequesterIsComposedFromThatSeatsHolderAlone(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{
		{Name: "Jane", DeclaredHandle: "jane", Kind: org.KindHuman},
		{Name: "Priya", DeclaredHandle: "priya", Kind: org.KindHuman},
		{Name: "Omar", DeclaredHandle: "omar", Kind: org.KindHuman},
		{Name: "Lena", DeclaredHandle: "lena", Kind: org.KindHuman},
	}}
	o.Normalize()
	jane := uuid.Must(uuid.NewV7())
	priya := uuid.Must(uuid.NewV7())
	whole := &atomic.Int64{}
	served := &directoryCopy{}
	served.set(oneSeatDirectory{whole: whole, holders: map[string]iamdomain.SeatHolder{
		"jane":  {Seat: "jane", Person: jane.String(), Stage: iam.StageActive},
		"priya": {Seat: "priya", Person: priya.String(), Stage: iam.StageSuspended},
		"omar":  {Seat: "omar", Person: uuid.NewString(), Removed: true},
	}})
	e := statelessWithDirectory(t, served)
	e.epoch.current.Store(&Company{Org: o})
	r := turnRequesters{engine: e}

	for _, tc := range []struct {
		seat  string
		id    uuid.UUID
		stage iam.Stage
	}{
		{"jane", jane, iam.StageActive},
		{"priya", priya, iam.StageSuspended},
		{"omar", uuid.Nil, iam.StageRetired},
		{"lena", uuid.Nil, iam.StageActive},
	} {
		p, err := r.Requester(t.Context(), tc.seat)
		if err != nil {
			t.Errorf("seat %s: %v", tc.seat, err)
			continue
		}
		if p.Kind != iam.KindPerson || p.Seat != tc.seat || p.ID != tc.id ||
			p.Stage != tc.stage || p.Can(iam.GrantAdmin) {
			t.Errorf("seat %s composed as %+v, want the person %s at %s", tc.seat, p,
				tc.id, tc.stage)
		}
	}
	if n := whole.Load(); n != 0 {
		t.Errorf("composing four requesters read the whole directory %d times", n)
	}

	served.set(oneSeatDirectory{whole: whole, err: store.ErrNoEstate})
	if p, err := r.Requester(t.Context(), "jane"); !errors.Is(err, store.ErrNoEstate) {
		t.Errorf("an unreadable directory composed (%+v, %v), want its error", p, err)
	}
}
