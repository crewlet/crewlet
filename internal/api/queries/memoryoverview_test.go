package queries_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
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
