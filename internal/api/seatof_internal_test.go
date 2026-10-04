package api

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// THE PUBLISHED COMPANY'S WORD ON A SEAT names it by its identity and its
// handle now, under any name it answers to.
//
// An open socket acting as a seat is decided against the company just
// published by this one read: a seat absent, or no longer a human seat, ends it
// 4403, and one that answers to another handle sends it back to its handshake.
// So the read must find a renamed seat under the handle it was created under
// and under one it gave up — reporting the handle it holds now, which is how
// the socket tells a rename from a seat held as it was — say an agent seat is
// not a human one, and find nothing in a company that holds no such seat or in
// no company at all.
func TestSeatOfReadsThePublishedRoster(t *testing.T) {
	t.Parallel()
	roster := &org.Organization{Roles: []*org.Role{
		{Name: "Ana Lee", DeclaredHandle: "ana-lee", Kind: org.KindHuman,
			OriginHandle: "ana", FormerHandles: []string{"ana-l"}},
		{Name: "Triage", DeclaredHandle: "triage", Kind: org.KindAgent},
	}}
	company := func() (*config.Company, *org.Organization) {
		return &config.Company{}, roster
	}
	renamed := stream.SeatState{Origin: "ana", Handle: "ana-lee", Human: true}
	for _, c := range []struct {
		name  string
		found bool
		want  stream.SeatState
	}{
		{"ana-lee", true, renamed},
		{"ana", true, renamed},
		{"ana-l", true, renamed},
		{"triage", true, stream.SeatState{Origin: "triage", Handle: "triage"}},
		{"nobody", false, stream.SeatState{}},
	} {
		got, found := seatOf(company, c.name)
		if found != c.found || got != c.want {
			t.Errorf("seatOf(%q) = %+v, %v; want %+v, %v", c.name, got, found,
				c.want, c.found)
		}
	}
	none := func() (*config.Company, *org.Organization) { return nil, nil }
	if _, found := seatOf(none, "ana-lee"); found {
		t.Error("a node with no published company found a seat")
	}
}
