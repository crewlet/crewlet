package engine

import (
	"context"
	"errors"
	"slices"
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
// asks it: ONE seat's holder and their grants, by its handle, from holders.
// Its whole-directory read counts its callers and answers nothing, because a
// requester that reads it reads every binding and every tombstone the company
// ever wrote.
type oneSeatDirectory struct {
	holders map[string]iamdomain.Requester
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

func (d oneSeatDirectory) Requester(_ context.Context, seat string) (iamdomain.Requester, bool, error) {
	if d.err != nil {
		return iamdomain.Requester{}, false, d.err
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
	served.set(oneSeatDirectory{whole: whole, holders: map[string]iamdomain.Requester{
		"jane":  {SeatHolder: iamdomain.SeatHolder{Seat: "jane", Person: jane.String(), Stage: iam.StageActive}},
		"priya": {SeatHolder: iamdomain.SeatHolder{Seat: "priya", Person: priya.String(), Stage: iam.StageSuspended}},
		"omar":  {SeatHolder: iamdomain.SeatHolder{Seat: "omar", Person: uuid.NewString(), Removed: true}},
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

// A PERSON REQUESTER USES THE APPS THEIR GRANTS NAME, AND NO MORE (ADR-0103):
// every app admin includes where they hold admin, never admin itself — which
// no ceiling carries — and never a name this build does not know, which
// grants nothing; none at all at a stage that may not act, for a removed
// holder, or on a seat the directory binds to nobody, since a permission is a
// person's and a seat nobody holds has nobody to hold one. An agent seat uses
// every app. Composed with every app for every person, as it was, the ceiling
// held no app verb to anybody.
//
// Mutation: give every person every app (the composition before ADR-0103) —
// the worklet-only person reads as using Doclet.
func TestTheRequesterCarriesTheirApps(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{
		{Name: "Analyst", DeclaredHandle: "analyst"},
		{Name: "Jane", DeclaredHandle: "jane", Kind: org.KindHuman},
		{Name: "Priya", DeclaredHandle: "priya", Kind: org.KindHuman},
		{Name: "Omar", DeclaredHandle: "omar", Kind: org.KindHuman},
		{Name: "Noor", DeclaredHandle: "noor", Kind: org.KindHuman},
		{Name: "Lena", DeclaredHandle: "lena", Kind: org.KindHuman},
	}}
	o.Normalize()
	served := &directoryCopy{}
	served.set(oneSeatDirectory{whole: &atomic.Int64{}, holders: map[string]iamdomain.Requester{
		"jane": {SeatHolder: iamdomain.SeatHolder{Seat: "jane", Person: uuid.NewString(), Stage: iam.StageActive},
			Grants: []iam.Grant{iam.GrantWorklet, "a-later-build's-app"}},
		"priya": {SeatHolder: iamdomain.SeatHolder{Seat: "priya", Person: uuid.NewString(), Stage: iam.StageActive},
			Grants: []iam.Grant{iam.GrantAdmin}},
		"omar": {SeatHolder: iamdomain.SeatHolder{Seat: "omar", Person: uuid.NewString(), Stage: iam.StageSuspended},
			Grants: []iam.Grant{iam.GrantWorklet}},
		"noor": {SeatHolder: iamdomain.SeatHolder{Seat: "noor", Person: uuid.NewString(), Removed: true}},
	}})
	e := statelessWithDirectory(t, served)
	e.epoch.current.Store(&Company{Org: o})
	r := turnRequesters{engine: e}

	for _, tc := range []struct {
		seat string
		uses []iam.Grant
	}{
		{"analyst", iam.Apps},
		{"jane", []iam.Grant{iam.GrantWorklet}},
		{"priya", iam.Apps},
		{"omar", nil},
		{"noor", nil},
		{"lena", nil},
	} {
		p, err := r.Requester(t.Context(), tc.seat)
		if err != nil {
			t.Errorf("seat %s: %v", tc.seat, err)
			continue
		}
		for _, app := range iam.Apps {
			if got, want := p.Can(app), slices.Contains(tc.uses, app); got != want {
				t.Errorf("seat %s's requester uses %s: %v, want %v (grants %v)", tc.seat, app, got, want,
					p.Grants)
			}
		}
		if p.Can(iam.GrantAdmin) || slices.Contains(p.Grants, iam.GrantAdmin) {
			t.Errorf("seat %s's requester carries admin: %v", tc.seat, p.Grants)
		}
		if slices.ContainsFunc(p.Grants, func(g iam.Grant) bool { return !g.Valid() }) {
			t.Errorf("seat %s's requester carries a grant this build does not know: %v", tc.seat, p.Grants)
		}
	}
}
