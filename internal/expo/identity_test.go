package expo_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seatname"
)

// EXPO'S ROBOT IS THE AGENT'S OWN IDENTITY on the one pattern every vendor
// shares: offered, never the company's, named by the shared rule, and set up
// with Expo's own labels at the level's role, never Admin.
func TestAnExpoRobotIsTheAgentsOwnIdentity(t *testing.T) {
	t.Parallel()
	v, ok := seataccess.Lookup(integration.KindExpo)
	if !ok {
		t.Fatal("Expo registers no access vendor")
	}
	spec, _ := v.Catalog().Lane(seataccess.LaneExpo)
	if !spec.Identity.Offered() || spec.Identity.Company {
		t.Fatalf("Expo's identity spec: %+v", spec.Identity)
	}
	guider, ok := v.(seataccess.IdentityGuider)
	if !ok {
		t.Fatal("Expo words no set-up sheet")
	}
	c := &config.Company{Name: "Example Co", Integrations: config.Integrations{Expo: &config.Expo{
		Provisioning: &config.ExpoProvisioning{AccountName: "example-org"}}}}
	g := guider.IdentityGuide(c, seataccess.SeatRef{Handle: "qa"}, seataccess.LaneExpo,
		seataccess.Desired{Enabled: true, Level: config.ExpoLevelDeveloper})
	if g.Name != seatname.For(seatname.Expo, "Example Co", "qa") || g.Role != "Developer" || g.Never != "Admin" ||
		g.Back.Kind != seataccess.BackToken || g.Link != nil || !strings.Contains(g.Steps[0].Text.URLs()[0], "example-org") {
		t.Fatalf("guide: %+v", g)
	}
	if err := g.Check(); err != nil {
		t.Errorf("the guide is not of the one form: %v", err)
	}
	id := seataccess.IdentityOf(v, c, seataccess.SeatRef{Handle: "qa"}, seataccess.LaneExpo,
		seataccess.Desired{Enabled: true}, seataccess.Observed{}, nil, time.Now())
	if id == nil || id.Status != seataccess.IdentityNone || !id.SetUp || id.UseCompany || id.Guide == nil {
		t.Fatalf("a robot not yet handed in: %+v", id)
	}
}
