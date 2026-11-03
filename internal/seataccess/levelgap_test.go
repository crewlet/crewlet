package seataccess_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// A GAP IS THE VENDOR'S FINDING ONLY FOR A LEVEL A SEAT THAT IS ON HOLDS, and
// otherwise the advisory on the same subject; its offer carries the reason on
// that level alone.
func TestALevelGapIsAFaultOnlyWhereASeatHoldsTheLevel(t *testing.T) {
	t.Parallel()
	gap := seataccess.LevelGap{
		Level: "configure", Name: "Configure", Reason: "The Configure level is unavailable until X",
		Remedy: "Add X", ActionURL: "https://dash.example.com/tokens", Missing: []string{"X"},
		Blocking: integration.Finding{Kind: integration.FindingGrantShort, Subject: "vendor level configure"},
	}
	note := seataccess.GapFindings([]seataccess.LevelGap{gap}, []string{"read"})
	if len(note) != 1 || note[0].Kind != integration.FindingLevelUnavailable ||
		note[0].Subject != "vendor level configure" || note[0].Detail != gap.Reason ||
		note[0].Remedy != "Add X" || !slices.Equal(note[0].Subjects, []string{"X"}) {
		t.Errorf("a level nobody holds = %+v, want the advisory", note)
	}
	if fault := seataccess.GapFindings([]seataccess.LevelGap{gap}, []string{"configure"}); len(fault) != 1 ||
		fault[0].Kind != integration.FindingGrantShort {
		t.Errorf("a level a seat holds = %+v, want the blocking finding", fault)
	}
	offer := seataccess.OfferGaps([]seataccess.Level{{ID: "read"}, {ID: "configure"}}, []seataccess.LevelGap{gap})
	if offer[0].Unavailable != "" || offer[1].Unavailable != gap.Reason {
		t.Errorf("offer = %+v, want the reason on configure alone", offer)
	}
}

// THE LEVELS IN USE are those of seats that are on, each once, an unset level
// read as the lane's default; a seat that is off holds none.
func TestTheLevelsInUseAreTheSeatsThatAreOn(t *testing.T) {
	t.Parallel()
	on, off := true, false
	company := &config.Company{Roles: []config.Role{
		{Name: "SRE", Integrations: config.RoleIntegrations{Cloudflare: &config.RoleCloudflare{Enabled: &on, Level: "operate"}}},
		{Name: "Ops", Integrations: config.RoleIntegrations{Cloudflare: &config.RoleCloudflare{Enabled: &on, Level: "operate"}}},
		{Name: "Reviewer", Integrations: config.RoleIntegrations{Cloudflare: &config.RoleCloudflare{Enabled: &on}}},
		{Name: "Edge", Integrations: config.RoleIntegrations{Cloudflare: &config.RoleCloudflare{Enabled: &off, Level: "configure"}}},
		{Name: "PM"},
	}}
	if got := seataccess.LevelsInUse(company, seataccess.LaneCloudflare); !slices.Equal(got, []string{"operate", "read"}) {
		t.Errorf("levels in use = %v, want operate and read", got)
	}
}
