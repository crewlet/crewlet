package seataccess

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// twoKeys is a box-CLI lane that reads two sealed values, as a cloud CLI reads
// an access key and a secret key.
var twoKeys = Catalog{
	Kind:        integration.KindExpo,
	Credentials: []string{"SECRET_KEY", "ACCESS_KEY"},
	Lanes: []LaneSpec{{Lane: LaneExpo, BoxEnv: []BoxVar{
		{Field: "SECRET_KEY", Env: "CLI_SECRET_KEY"}, {Field: "ACCESS_KEY", Env: "CLI_ACCESS_KEY"},
	}}},
}

// A BOX-CLI LANE NEEDS EVERY ONE OF ITS LINES, each set to exactly the seat's
// own sealed name, before the box counts as its tool.
func TestABoxReadsALaneOnlyWithEveryLine(t *testing.T) {
	t.Parallel()
	lines := boxLinesOf(twoKeys, LaneExpo, "qa")
	want := []BoxLine{
		{Name: "CLI_SECRET_KEY", Value: "${EXPO_SECRET_KEY_QA}", By: ByYou},
		{Name: "CLI_ACCESS_KEY", Value: "${EXPO_ACCESS_KEY_QA}", By: ByYou},
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
	role := func(env map[string]string) *config.Role {
		return &config.Role{Handle: "qa", Sandbox: &config.RoleSandbox{Env: env}}
	}
	for _, tc := range []struct {
		name string
		role *config.Role
		want bool
	}{
		{"both set", role(map[string]string{
			"CLI_SECRET_KEY": "${EXPO_SECRET_KEY_QA}", "CLI_ACCESS_KEY": "${EXPO_ACCESS_KEY_QA}"}), true},
		{"one missing", role(map[string]string{"CLI_SECRET_KEY": "${EXPO_SECRET_KEY_QA}"}), false},
		{"one pointing at another seat", role(map[string]string{
			"CLI_SECRET_KEY": "${EXPO_SECRET_KEY_QA}", "CLI_ACCESS_KEY": "${EXPO_ACCESS_KEY_SRE}"}), false},
		{"no box settings", &config.Role{Handle: "qa"}, false},
		{"no role", nil, false},
	} {
		if got := boxSets(tc.role, lines); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if boxSets(role(nil), nil) {
		t.Error("a lane with no lines counted a box as its tool")
	}
	if got := boxLinesOf(twoKeys, LaneDatadog, "qa"); got != nil {
		t.Errorf("a lane the catalog does not list has lines %v", got)
	}
}

// defaulter is a vendor whose box CLI reads settings beside its credentials.
type defaulter struct {
	cat  Catalog
	vars map[string]string
}

func (d defaulter) Kind() integration.Kind { return d.cat.Kind }
func (d defaulter) Catalog() Catalog       { return d.cat }
func (defaulter) Steps(SeatRef, Lane, Op, Desired, Observed) []Step {
	return nil
}
func (defaulter) Open(context.Context, OpenInput) (Session, error) { return nil, nil }
func (d defaulter) BoxDefaults(*config.Company, *config.Role) BoxSet {
	return BoxSet{Env: d.vars}
}

// A VENDOR'S BOX DEFAULTS REACH ONLY A SEAT WITH ITS BOX-CLI LANE ON, sorted,
// and a vendor whose lane reads no box variable contributes none.
func TestBoxDefaultsReachOnlyASeatWithItsLaneOn(t *testing.T) {
	t.Parallel()
	on, off := true, false
	company := func(enabled *bool) *config.Company {
		return &config.Company{Roles: []config.Role{{Name: "QA", Handle: "qa",
			Integrations: config.RoleIntegrations{Expo: &config.RoleExpo{Enabled: enabled, Level: "viewer"}}}}}
	}
	cli := defaulter{cat: twoKeys, vars: map[string]string{"CLI_ZONE": "fr-par-1", "CLI_ORG": "${ORG_ID}"}}
	server := defaulter{cat: Catalog{Kind: integration.KindDatadog, Credentials: []string{"APP_KEY"},
		Lanes: []LaneSpec{{Lane: LaneDatadog}}}, vars: map[string]string{"NEVER": "x"}}
	vendors := []Vendor{cli, server}

	got := boxDefaults(company(&on), "qa", vendors)
	want := []BoxDefault{
		{Kind: integration.KindExpo, Env: "CLI_ORG", Value: "${ORG_ID}"},
		{Kind: integration.KindExpo, Env: "CLI_ZONE", Value: "fr-par-1"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("defaults = %v, want %v", got, want)
	}
	if got := boxDefaults(company(&off), "qa", vendors); got != nil {
		t.Errorf("a seat with the lane off got %v", got)
	}
	if got := boxDefaults(company(&on), "sre", vendors); got != nil {
		t.Errorf("a seat the company does not have got %v", got)
	}
	if got := boxDefaults(nil, "qa", vendors); got != nil {
		t.Errorf("no company got %v", got)
	}
}

// A SEAT'S BOX ON THE WIRE lists the lines a person adds, then the engine's
// own settings while the lane is on, and nothing for a lane with neither.
func TestABoxListsAPersonsLinesThenTheEnginesWhileOn(t *testing.T) {
	t.Parallel()
	on, off := true, false
	company := func(enabled *bool) *config.Company {
		return &config.Company{Roles: []config.Role{{Name: "QA", Handle: "qa",
			Integrations: config.RoleIntegrations{Expo: &config.RoleExpo{Enabled: enabled, Level: "viewer"}}}}}
	}
	cli := defaulter{cat: twoKeys, vars: map[string]string{"CLI_ZONE": "fr-par-1", "CLI_ORG": "${ORG_ID}"}}
	got := boxOf(company(&on), cli, LaneExpo, "qa")
	want := []BoxLine{
		{Name: "CLI_SECRET_KEY", Value: "${EXPO_SECRET_KEY_QA}", By: ByYou},
		{Name: "CLI_ACCESS_KEY", Value: "${EXPO_ACCESS_KEY_QA}", By: ByYou},
		{Name: "CLI_ORG", Value: "${ORG_ID}", By: ByCrewlet},
		{Name: "CLI_ZONE", Value: "fr-par-1", By: ByCrewlet},
	}
	if got == nil || !slices.Equal(got.Env, want) {
		t.Fatalf("box = %+v, want %v", got, want)
	}
	if got := boxOf(company(&off), cli, LaneExpo, "qa"); got == nil || !slices.Equal(got.Env, want[:2]) {
		t.Errorf("a seat with the lane off: box = %+v, want only the lines a person adds", got)
	}
	server := defaulter{cat: Catalog{Kind: integration.KindDatadog, Credentials: []string{"APP_KEY"},
		Lanes: []LaneSpec{{Lane: LaneDatadog}}}}
	if got := boxOf(company(&on), server, LaneDatadog, "qa"); got != nil {
		t.Errorf("a lane whose tool is a server has a box %+v", got)
	}
}
