package expo_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// THE ROBOT TO CREATE IS NAMED BEFORE ANY PASS HAS SEEN IT: the seat's hand-in
// is the robot's name, its create step says it with no ledger record yet, and
// the offer carries the account a person switches to.
func TestAnExpoRobotIsNamedBeforeItExists(t *testing.T) {
	t.Parallel()
	vendor, ok := seataccess.Lookup(integration.KindExpo)
	if !ok {
		t.Fatal("the Expo vendor is not registered")
	}
	on := true
	company := &config.Company{Name: "Acme"}
	company.Integrations.Expo = &config.Expo{Enabled: true,
		Provisioning: &config.ExpoProvisioning{AccountName: "${EXPO_ACCOUNT}"}}
	company.Roles = []config.Role{{Name: "QA", Handle: "qa",
		Integrations: config.RoleIntegrations{Expo: &config.RoleExpo{Enabled: &on}}}}
	seat := seataccess.SeatRef{Handle: "qa", Name: "QA"}
	want := seataccess.Desired{Enabled: true, Level: config.ExpoLevelRunner}

	if got := seataccess.HandInOf(vendor, company, seat, seataccess.LaneExpo, want); got == nil ||
		got.Name != "crewlet-acme-qa" {
		t.Errorf("hand-in = %+v, want the robot's name", got)
	}

	steps, err := seataccess.Plan(vendor, seataccess.SeatInput{Seat: seat, Company: company,
		Want: map[seataccess.Lane]seataccess.Desired{seataccess.LaneExpo: want},
		Have: map[seataccess.Lane]seataccess.Observed{}})
	if err != nil {
		t.Fatal(err)
	}
	plan := steps[seataccess.LaneExpo]
	if len(plan) == 0 || plan[0].Verb != seataccess.VerbCreate || !strings.Contains(plan[0].Text, "crewlet-acme-qa") {
		t.Errorf("the create plan = %+v, want its first step naming the robot", plan)
	}

	sess, err := expo.OpenAccess(context.Background(), vendor, seataccess.OpenInput{Config: company,
		Env: config.NewResolver(config.MapSource{"EXPO_ACCOUNT": testAccountName})}, nil)
	if err != nil {
		t.Fatal(err)
	}
	o, err := sess.Options(context.Background(), seataccess.LaneExpo)
	if err != nil {
		t.Fatal(err)
	}
	if o.VendorAccount != testAccountName {
		t.Errorf("vendor account = %q, want the resolved account name, not its reference", o.VendorAccount)
	}
}
