package searchconsole

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// A SEAT IS OFFERED THE COMPANY'S SITES, AND NO OTHERS: the registered access
// vendor reaches exactly the sites the company narrows agents to, is open
// where it names none, and offers the three levels on the Search Console lane.
//
// Mutation: answer another list from the vendor's scope, and the Agents tab
// offers sites the company never named.
func TestASeatIsOfferedTheCompanysSites(t *testing.T) {
	t.Parallel()
	var vendor *googlegrowth.SeatAccess
	for _, v := range seataccess.Registered() {
		if sa, ok := v.(*googlegrowth.SeatAccess); ok && v.Kind() == Kind {
			vendor = sa
		}
	}
	if vendor == nil || vendor.Product != Kind {
		t.Fatal("no access vendor registered for Search Console")
	}
	sites := []string{"https://www.example.com/", "sc-domain:example.org"}
	c := &config.Company{Name: "Acme"}
	c.Integrations.SearchConsole = &config.SearchConsole{Enabled: true, Sites: sites,
		Approvers: []string{"founder"}}
	if got := vendor.Reach(c, nil, seataccess.LaneSearchConsole); got.Open || !slices.Equal(got.Scope, sites) {
		t.Errorf("a company naming its sites reaches %+v", got)
	}
	c.Integrations.SearchConsole.Sites = nil
	if got := vendor.Reach(c, nil, seataccess.LaneSearchConsole); !got.Open || len(got.Scope) != 0 {
		t.Errorf("a company naming no site reaches %+v", got)
	}
	if lanes := vendor.Catalog().Lanes; len(lanes) != 1 || lanes[0].Lane != seataccess.LaneSearchConsole ||
		len(vendor.FixedLevels(seataccess.LaneSearchConsole)) != 3 {
		t.Errorf("the vendor offers %+v", vendor.Catalog())
	}
}
