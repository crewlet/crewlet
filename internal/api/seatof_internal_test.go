package api

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// THE PUBLISHED COMPANY'S WORD ON A SEAT, by its handle.
//
// An open socket acting as a seat is decided against the company just
// published by this one read: a seat absent, or no longer a human seat, ends it
// 4403. So the read must find a seat by its handle, say an agent seat is not a
// human one, and find nothing for a handle no seat answers to — the display
// name included — or in no company at all.
func TestSeatOfReadsThePublishedRoster(t *testing.T) {
	t.Parallel()
	roster := &org.Organization{Roles: []*org.Role{
		{Name: "Ana Lee", DeclaredHandle: "ana", Kind: org.KindHuman},
		{Name: "Triage", DeclaredHandle: "triage", Kind: org.KindAgent},
	}}
	company := func() (*config.Company, *org.Organization) {
		return &config.Company{}, roster
	}
	for _, c := range []struct {
		name  string
		found bool
		want  stream.SeatState
	}{
		{"ana", true, stream.SeatState{Human: true}},
		{"triage", true, stream.SeatState{}},
		{"ana-lee", false, stream.SeatState{}},
		{"nobody", false, stream.SeatState{}},
	} {
		got, found := seatOf(company, c.name)
		if found != c.found || got != c.want {
			t.Errorf("seatOf(%q) = %+v, %v; want %+v, %v", c.name, got, found,
				c.want, c.found)
		}
	}
	none := func() (*config.Company, *org.Organization) { return nil, nil }
	if _, found := seatOf(none, "ana"); found {
		t.Error("a node with no published company found a seat")
	}
}
