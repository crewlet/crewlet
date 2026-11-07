package seataccess

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
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

// expoCompany is a company that provisions Expo, so the engine seals its
// agents' credentials there, with QA on or off at it as enabled says; external
// is the same company handing its agents' credentials in instead.
func expoCompany(enabled *bool, external bool) *config.Company {
	c := &config.Company{
		Integrations: config.Integrations{Expo: &config.Expo{Enabled: true,
			Provisioning: &config.ExpoProvisioning{AccountName: "acme"}}},
		Roles: []config.Role{{Name: "QA", Handle: "qa",
			Integrations: config.RoleIntegrations{Expo: &config.RoleExpo{Enabled: enabled, Level: "viewer"}}}},
	}
	if external {
		c.Integrations.Expo.Provisioning = nil
	}
	return c
}

// AN ON SEAT'S BOX HOLDS EVERY CREDENTIAL ITS LANE'S TOOL READS, each set to
// the seat's own sealed name, with no line in the document; an off seat's, a
// seat whose credentials are handed in from outside Crewlet, and a lane the
// catalog does not list hold none. Mutation: drop the on check, or the mode
// check, and this goes red.
func TestAnOnSeatsBoxHoldsEveryCredentialItsToolReads(t *testing.T) {
	t.Parallel()
	on, off := true, false
	c := expoCompany(&on, false)
	got := laneCredentials(c, twoKeys, &c.Roles[0], LaneExpo)
	want := map[string]string{"CLI_SECRET_KEY": "${EXPO_SECRET_KEY_QA}", "CLI_ACCESS_KEY": "${EXPO_ACCESS_KEY_QA}"}
	if !maps.Equal(got, want) {
		t.Fatalf("credentials = %v, want %v", got, want)
	}
	for name, c := range map[string]*config.Company{
		"off":      expoCompany(&off, false),
		"external": expoCompany(&on, true),
	} {
		if got := laneCredentials(c, twoKeys, &c.Roles[0], LaneExpo); got != nil {
			t.Errorf("%s: credentials = %v, want none", name, got)
		}
	}
	if got := laneCredentials(c, twoKeys, &c.Roles[0], LaneDatadog); got != nil {
		t.Errorf("a lane the catalog does not list has credentials %v", got)
	}
	if got := laneCredentials(c, twoKeys, nil, LaneExpo); got != nil {
		t.Errorf("no seat has credentials %v", got)
	}
}

// defaulter is a vendor whose box CLI reads settings beside its credentials,
// credentials a CLI beside its tool reads, and files the engine writes for it.
type defaulter struct {
	cat   Catalog
	vars  map[string]string
	creds map[string]string
	files []BoxFile
}

func (d defaulter) Kind() integration.Kind { return d.cat.Kind }
func (d defaulter) Catalog() Catalog       { return d.cat }
func (defaulter) Steps(SeatRef, Lane, Op, Desired, Observed) []Step {
	return nil
}
func (defaulter) Open(context.Context, OpenInput) (Session, error) { return nil, nil }
func (d defaulter) BoxDefaults(*config.Company, *config.Role) BoxSet {
	return BoxSet{Env: d.vars, Credentials: d.creds, Files: d.files}
}

// A VENDOR'S BOX DEFAULTS REACH ONLY A SEAT WITH ITS LANE ON, sorted: the
// credentials its tool reads and those beside it first, then its settings. A
// vendor whose lane the seat does not have on contributes none.
func TestBoxDefaultsReachOnlyASeatWithItsLaneOn(t *testing.T) {
	t.Parallel()
	on, off := true, false
	cli := defaulter{cat: twoKeys, vars: map[string]string{"CLI_ZONE": "fr-par-1", "CLI_ORG": "${ORG_ID}"},
		creds: map[string]string{"GIT_TOKEN": "${EXPO_GIT_TOKEN_QA}"}}
	server := defaulter{cat: Catalog{Kind: integration.KindDatadog, Credentials: []string{"APP_KEY"},
		Lanes: []LaneSpec{{Lane: LaneDatadog}}}, vars: map[string]string{"NEVER": "x"}}
	vendors := []Vendor{cli, server}

	got := boxDefaults(expoCompany(&on, false), "qa", vendors)
	want := []BoxDefault{
		{Kind: integration.KindExpo, Env: "CLI_ACCESS_KEY", Value: "${EXPO_ACCESS_KEY_QA}", Credential: true},
		{Kind: integration.KindExpo, Env: "CLI_SECRET_KEY", Value: "${EXPO_SECRET_KEY_QA}", Credential: true},
		{Kind: integration.KindExpo, Env: "GIT_TOKEN", Value: "${EXPO_GIT_TOKEN_QA}", Credential: true},
		{Kind: integration.KindExpo, Env: "CLI_ORG", Value: "${ORG_ID}"},
		{Kind: integration.KindExpo, Env: "CLI_ZONE", Value: "fr-par-1"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("defaults = %v, want %v", got, want)
	}
	if got := boxDefaults(expoCompany(&off, false), "qa", vendors); got != nil {
		t.Errorf("a seat with the lane off got %v", got)
	}
	if got := boxDefaults(expoCompany(&on, false), "sre", vendors); got != nil {
		t.Errorf("a seat the company does not have got %v", got)
	}
	if got := boxDefaults(nil, "qa", vendors); got != nil {
		t.Errorf("no company got %v", got)
	}
}

// A SEAT'S BOX ON THE WIRE lists the credentials its lane's tool reads, marked
// as the tool's, then every other variable Crewlet sets there by name, a
// credential beside the tool among them, while the lane is on, and nothing for
// a seat that is off or a lane with none of them. Mutation: list an off seat's
// credentials, or mark the beside-the-tool one as the tool's, and this goes red.
func TestABoxListsItsToolsCredentialsThenTheRestWhileOn(t *testing.T) {
	t.Parallel()
	on, off := true, false
	cli := defaulter{cat: twoKeys, vars: map[string]string{"CLI_ZONE": "fr-par-1", "CLI_ORG": "${ORG_ID}"},
		creds: map[string]string{"GIT_TOKEN": "${EXPO_GIT_TOKEN_QA}"}}
	c := expoCompany(&on, false)
	got := boxOf(c, cli, LaneExpo, &c.Roles[0])
	want := []BoxLine{
		{Name: "CLI_ACCESS_KEY", Value: "${EXPO_ACCESS_KEY_QA}", Tool: true},
		{Name: "CLI_SECRET_KEY", Value: "${EXPO_SECRET_KEY_QA}", Tool: true},
		{Name: "CLI_ORG", Value: "${ORG_ID}"},
		{Name: "CLI_ZONE", Value: "fr-par-1"},
		{Name: "GIT_TOKEN", Value: "${EXPO_GIT_TOKEN_QA}"},
	}
	if got == nil || !slices.Equal(got.Env, want) {
		t.Fatalf("box = %+v, want %v", got, want)
	}
	offCompany := expoCompany(&off, false)
	if got := boxOf(offCompany, cli, LaneExpo, &offCompany.Roles[0]); got != nil {
		t.Errorf("a seat with the lane off: box = %+v, want none", got)
	}
	server := defaulter{cat: Catalog{Kind: integration.KindDatadog, Credentials: []string{"APP_KEY"},
		Lanes: []LaneSpec{{Lane: LaneDatadog}}}}
	if got := boxOf(c, server, LaneDatadog, &c.Roles[0]); got != nil {
		t.Errorf("a lane whose tool is a server has a box %+v", got)
	}
}

// A VENDOR'S FILES ARE THE ENGINE'S LINES ON THE WIRE, by path, each with its
// reference as configured and whether the poll keeps it current, for every
// lane of a seat that has one on: a lane whose credential is issued at use and
// reads no box variable included. A box of files alone still sends its
// variables as a list. Nothing is listed while the seat's lane is off.
func TestABoxListsTheEnginesFilesWhileOn(t *testing.T) {
	t.Parallel()
	on, off := true, false
	company := func(enabled *bool) *config.Company {
		return &config.Company{Roles: []config.Role{{Name: "QA", Handle: "qa",
			Integrations: config.RoleIntegrations{Datadog: &config.RoleDatadog{Enabled: enabled, Role: "a"}}}}}
	}
	cli := defaulter{cat: issuedOnly, files: []BoxFile{
		{Path: ".config/crewlet/token", Ref: "${DATADOG_ACCESS_TOKEN_QA}", Rewritten: true},
		{Path: ".config/crewlet/a-setting", Ref: "${DATADOG_SITE}"},
	}}
	onCompany := company(&on)
	got := boxOf(onCompany, cli, LaneDatadog, &onCompany.Roles[0])
	want := []BoxFileLine{
		{Path: ".config/crewlet/a-setting", Value: "${DATADOG_SITE}"},
		{Path: ".config/crewlet/token", Value: "${DATADOG_ACCESS_TOKEN_QA}", Rewritten: true},
	}
	if got == nil || !slices.Equal(got.Files, want) {
		t.Fatalf("box = %+v, want the files %v", got, want)
	}
	wire, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(wire), `"env":[]`) || !strings.Contains(string(wire), `"files":[`) {
		t.Errorf("a box of files alone is sent as %s", wire)
	}
	offCompany := company(&off)
	if got := boxOf(offCompany, cli, LaneDatadog, &offCompany.Roles[0]); got != nil {
		t.Errorf("a seat with the lane off: box = %+v, want none", got)
	}
	defaults := boxDefaults(onCompany, "qa", []Vendor{cli})
	if len(defaults) != 2 || defaults[0].File == nil || defaults[0].File.Path != ".config/crewlet/a-setting" ||
		defaults[0].Kind != integration.KindDatadog {
		t.Errorf("defaults = %+v, want the vendor's two files, by path", defaults)
	}
}

// A FILE'S VARIABLE IS ONE OF THE ENGINE'S LINES ON THE WIRE, among its
// settings by name, holding the file's path from the box's home, since the
// launch joins it onto each box's own. A setting of the same name is the line
// instead, as it wins in the box, and a file naming no variable, such as one of
// literal text, adds none.
// Mutation: drop the line, or let it replace the setting, and this goes red.
func TestABoxListsTheVariableNamingAFilesPath(t *testing.T) {
	t.Parallel()
	on := true
	c := &config.Company{Roles: []config.Role{{Name: "QA", Handle: "qa",
		Integrations: config.RoleIntegrations{Datadog: &config.RoleDatadog{Enabled: &on, Role: "a"}}}}}
	settings := map[string]string{"CLI_ZONE": "fr-par-1", "CLI_SHADOWED": "a setting"}
	cli := defaulter{cat: issuedOnly, vars: settings, files: []BoxFile{
		{Path: ".config/crewlet/token", Ref: "${DATADOG_ACCESS_TOKEN_QA}", PathEnv: "CLI_TOKEN_FILE", Rewritten: true},
		{Path: ".config/crewlet/other", Ref: "${DATADOG_SITE}", PathEnv: "CLI_SHADOWED"},
		{Path: ".config/crewlet/config", Ref: "[default]\nregion = fr-par\n"},
	}}
	got := boxOf(c, cli, LaneDatadog, &c.Roles[0])
	want := []BoxLine{
		{Name: "CLI_SHADOWED", Value: "a setting"},
		{Name: "CLI_TOKEN_FILE", Value: "~/.config/crewlet/token"},
		{Name: "CLI_ZONE", Value: "fr-par-1"},
	}
	if got == nil || !slices.Equal(got.Env, want) {
		t.Fatalf("box = %+v, want the lines %v", got, want)
	}
	if len(got.Files) != 3 {
		t.Errorf("files = %+v, want all three of the vendor's", got.Files)
	}
	if len(settings) != 2 {
		t.Errorf("listing the box wrote into the vendor's own settings: %v", settings)
	}
}

// A VENDOR'S BOX DEFAULTS REACH A SEAT WITH ANY OF ITS LANES ON, whether that
// lane's tool reads a box variable or not, and no seat with every lane off:
// its settings and its files alike. Mutation: reach only a seat with a lane
// that reads a box variable on, and the seat on the other lane goes without.
func TestBoxDefaultsReachASeatWithAnyOfItsVendorsLanesOn(t *testing.T) {
	t.Parallel()
	file := BoxFile{Path: ".config/crewlet/token", Ref: "${ATLASSIAN_TOKEN_QA}", Rewritten: true}
	twoLanes := defaulter{cat: Catalog{Kind: integration.KindAtlassian, Credentials: []string{"TOKEN", "EMAIL"},
		Lanes: []LaneSpec{
			{Lane: LaneJira, BoxEnv: []BoxVar{{Field: "TOKEN", Env: "CLI_TOKEN"}}},
			{Lane: LaneConfluence},
		}}, vars: map[string]string{"CLI_SITE": "acme"}, files: []BoxFile{file}}
	on, off := true, false
	company := func(jira, confluence *bool) *config.Company {
		return &config.Company{Roles: []config.Role{{Name: "QA", Handle: "qa",
			Integrations: config.RoleIntegrations{
				Jira: &config.RoleJira{Enabled: jira}, Confluence: &config.RoleConfluence{Enabled: confluence},
			}}}}
	}
	for name, c := range map[string]*config.Company{
		"only the lane that reads no box variable on": company(&off, &on),
		"only the lane that reads one on":             company(&on, &off),
		"both on":                                     company(&on, &on),
	} {
		got := boxDefaults(c, "qa", []Vendor{twoLanes})
		if len(got) != 2 || got[0].Env != "CLI_SITE" || got[0].Value != "acme" ||
			got[1].File == nil || *got[1].File != file {
			t.Errorf("%s: defaults = %+v, want the setting and the file", name, got)
		}
	}
	if got := boxDefaults(company(&off, &off), "qa", []Vendor{twoLanes}); got != nil {
		t.Errorf("a seat with every lane off got %+v", got)
	}
}
