package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// completeScaleway is an enabled block with every requirement answered.
const completeScaleway = `    enabled: true
    provisioning:
      organization_id: 11111111-2222-4333-8444-555555555555
      access_key: SCWABCDEFGHIJ0123456
      secret_key: "${SCALEWAY_SECRET_KEY}"`

// A COMPLETE BLOCK VALIDATES, so the refusals below are requirements rather
// than a refusal of everything.
func TestACompleteScalewayBlockValidates(t *testing.T) {
	t.Parallel()
	if err := validateIntegrationDoc(t, "scaleway", completeScaleway); err != nil {
		t.Fatalf("a complete Scaleway block was refused: %v", err)
	}
	if err := validateIntegrationDoc(t, "scaleway", "    enabled: false"); err != nil {
		t.Fatalf("a switched-off Scaleway block was refused: %v", err)
	}
	ref := "    enabled: true\n    provisioning: {organization_id: \"${SCW_ORG}\", access_key: \"${SCW_KEY}\", " +
		"secret_key: \"${S}\"}"
	if err := validateIntegrationDoc(t, "scaleway", ref); err != nil {
		t.Fatalf("an organization and access key named by reference were refused: %v", err)
	}
}

// AN ENABLED BLOCK NAMES ITS ORGANIZATION AND BOTH HALVES OF ITS KEY, each
// refusal naming the field an operator edits.
func TestScalewayRefusesAnIncompleteOrMalformedBlock(t *testing.T) {
	t.Parallel()
	const org, key = "11111111-2222-4333-8444-555555555555", "SCWABCDEFGHIJ0123456"
	for _, tc := range []struct {
		name, body, path string
	}{
		{"no provisioning", "    enabled: true", "integrations.scaleway.provisioning"},
		{"no organization", "    enabled: true\n    provisioning: {access_key: " + key + ", secret_key: \"${S}\"}",
			"integrations.scaleway.provisioning.organization_id"},
		{"an organization that is not a UUID", "    enabled: true\n    provisioning: {organization_id: acme, access_key: " +
			key + ", secret_key: \"${S}\"}", "integrations.scaleway.provisioning.organization_id"},
		{"no access key", "    enabled: true\n    provisioning: {organization_id: " + org + ", secret_key: \"${S}\"}",
			"integrations.scaleway.provisioning.access_key"},
		{"a secret pasted as the access key", "    enabled: true\n    provisioning: {organization_id: " + org +
			", access_key: " + org + ", secret_key: \"${S}\"}", "integrations.scaleway.provisioning.access_key"},
		{"no secret key", "    enabled: true\n    provisioning: {organization_id: " + org + ", access_key: " + key + "}",
			"integrations.scaleway.provisioning.secret_key"},
	} {
		err := validateIntegrationDoc(t, "scaleway", tc.body)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: %v does not name %s", tc.name, err, tc.path)
		}
	}
}

// A SEAT NAMES A LEVEL OF THE CLOSED SET AND ITS PROJECTS: an empty level reads
// as read, a seat that is on with no project is refused at its projects, and
// each project is a project ID, once.
func TestAScalewaySeatNamesItsLevelAndProjects(t *testing.T) {
	t.Parallel()
	const project = "88271f50-64e5-4ab8-b7e9-abd58127d46a"
	doc := func(block string) string {
		return "name: Acme\nintegrations:\n  scaleway:\n" + completeScaleway +
			"\nroles:\n  - {name: SRE, handle: sre, integrations: {scaleway: {" + block + "}}}\n"
	}
	for _, level := range config.ScalewayLevels() {
		c, err := config.ParseCompany([]byte(doc("enabled: true, level: " + level + ", projects: [" + project + "]")))
		if err != nil {
			t.Fatalf("%s was refused: %v", level, err)
		}
		if got := c.Roles[0].Integrations.Scaleway.LevelOrDefault(); got != level {
			t.Errorf("level = %q, want %s", got, level)
		}
	}
	c, err := config.ParseCompany([]byte(doc("enabled: true, projects: [" + project + "]")))
	if err != nil {
		t.Fatalf("a seat naming no level was refused: %v", err)
	}
	if got := c.Roles[0].Integrations.Scaleway.LevelOrDefault(); got != config.ScalewayLevelRead {
		t.Errorf("an unset level = %q, want read", got)
	}
	for _, tc := range []struct{ name, block, path string }{
		{"an unknown level", "enabled: true, level: admin, projects: [" + project + "]",
			"roles[0].integrations.scaleway.level"},
		{"on with no project", "enabled: true", "roles[0].integrations.scaleway.projects"},
		{"a project that is not an ID", "enabled: true, projects: [production]",
			"roles[0].integrations.scaleway.projects[0]"},
		{"a project listed twice", "enabled: true, projects: [" + project + ", " + strings.ToUpper(project) + "]",
			"roles[0].integrations.scaleway.projects[1]"},
	} {
		if _, err := config.ParseCompany([]byte(doc(tc.block))); err == nil || !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: %v, want a refusal naming %s", tc.name, err, tc.path)
		}
	}
	if _, err := config.ParseCompany([]byte(doc("enabled: false"))); err != nil {
		t.Errorf("a seat that is off with no project was refused: %v", err)
	}
}

// THE ENGINE HOLDS A SEAT'S KEY ONLY WHERE IT HOLDS THE PROVISIONER: an enabled
// block with provisioning, and nowhere else. The secret key is the credential,
// the access key beside it.
func TestScalewaySeatCredentialsAreTheEnginesOnlyWithAProvisioner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		scw  *config.Scaleway
		want config.SeatCredentialMode
	}{
		{"absent", nil, config.SeatCredentialExternal},
		{"switched off", &config.Scaleway{Provisioning: &config.ScalewayProvisioning{}}, config.SeatCredentialExternal},
		{"no provisioning", &config.Scaleway{Enabled: true}, config.SeatCredentialExternal},
		{"provisioned", &config.Scaleway{Enabled: true, Provisioning: &config.ScalewayProvisioning{}}, config.SeatCredentialEngine},
	} {
		c := &config.Company{}
		c.Integrations.Scaleway = tc.scw
		if got := c.SeatCredentialMode("scaleway", false); got != tc.want {
			t.Errorf("%s: mode = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := config.SeatSealedFields("scaleway"); len(got) != 2 || got[0] != "SECRET_KEY" || got[1] != "ACCESS_KEY" {
		t.Errorf("sealed fields = %v, want [SECRET_KEY ACCESS_KEY]", got)
	}
	if !config.SeatScopeRequired("scaleway") {
		t.Error("an agent on Scaleway is not required to name a project")
	}
}
