package queries_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// THE OVERVIEW IS EVERY AGENT SEAT, IN HANDLE ORDER, each handed whole — its
// agent id and its handle — so the holder counts its rows.
func TestTheMemoryOverviewHandsEverySeatWhole(t *testing.T) {
	t.Parallel()
	roster := &org.Organization{Name: "Acme", Roles: []*org.Role{
		{Name: "Money", DeclaredHandle: "money"},
		{Name: "Chief", DeclaredHandle: "chief"},
	}}
	company := func() (*config.Company, *org.Organization) {
		return &config.Company{Name: "Acme"}, roster
	}
	memory := &stubMemory{}
	s := queries.Sources{Company: company, Memory: memory, Chart: flatChart{}}
	if _, err := askTranscripts(t, s, "memory_overview", nil); err != nil {
		t.Fatalf("memory_overview: %v", err)
	}
	chief, _ := roster.AgentIDFor(roster.Role("chief"))
	if len(memory.seats) != 2 || memory.seats[0].Handle != "chief" ||
		memory.seats[0].ID != chief || memory.seats[1].Handle != "money" {
		t.Errorf("the holders were asked about %+v, want chief then money, each "+
			"with its agent id", memory.seats)
	}
}

// THE OVERVIEW LISTS THE SEATS THE CALLER MAY READ, AND NO OTHERS — each
// decided by the trail's own verb ([authz.ActionSeatTrailRead]), exactly as
// that seat's memory is: every agent seat for an admin, the seats a lead leads
// for them, and an empty list — never a refusal — for somebody who leads none.
// A seat this node cannot decide makes the answer unavailable rather than
// shorter.
//
// It was decided ONCE, on no seat at all, which the owner-or-lead rule refuses
// as naming nobody — so the list answered nobody, an admin included.
//
// Mutation: decide once on an empty owner, and the admin's row goes red; skip
// the decision, and the stranger is shown every seat.
func TestMemoryOverviewListsOnlyTheSeatsTheCallerMayRead(t *testing.T) {
	t.Parallel()
	roster := &org.Organization{Name: "Acme", Roles: []*org.Role{
		{Name: "Money", DeclaredHandle: "money"},
		{Name: "Chief", DeclaredHandle: "chief"},
		{Name: "Lead", DeclaredHandle: "lead", Kind: org.KindHuman},
	}}
	company := func() (*config.Company, *org.Organization) {
		return &config.Company{Name: "Acme"}, roster
	}
	for _, c := range []struct {
		name  string
		ctx   context.Context
		chart authz.Chart
		want  []string
		err   error
	}{
		{"an admin", asGrants(t, "", iam.GrantAdmin), flatChart{},
			[]string{"chief", "money"}, nil},
		{"the lead of one seat, holding no permission", asSeat(t, "lead"),
			leadsChart{lead: "lead", report: "money"}, []string{"money"}, nil},
		{"somebody leading nobody, holding every app", asSeat(t, "pat", iam.Apps...),
			flatChart{}, nil, nil},
		{"a node that cannot read its chart", asSeat(t, "lead"), authz.NoChart{},
			nil, queries.ErrUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			memory := &stubMemory{}
			r := queries.NewRegistry()
			queries.Register(r, queries.Sources{Company: company, Memory: memory, Chart: c.chart})
			_, err := r.Answer(c.ctx, "memory_overview", nil)
			if c.err != nil {
				if !errors.Is(err, c.err) {
					t.Fatalf("answered %v, want %v", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("memory_overview: %v", err)
			}
			var got []string
			for _, seat := range memory.seats {
				got = append(got, seat.Handle)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("the holders were asked about %v, want %v", got, c.want)
			}
		})
	}
}
