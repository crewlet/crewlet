package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// completeGoogleCloud is an enabled block with every requirement answered.
const completeGoogleCloud = `    enabled: true
    provisioning:
      provisioner: crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com
      provider: projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine
      federation_key: "${GOOGLE_CLOUD_FEDERATION_KEY}"`

// A COMPLETE BLOCK VALIDATES, and so does one whose provider is not set yet:
// the connect form mints the key before the provider holding its public half
// can exist.
func TestACompleteGoogleCloudBlockValidates(t *testing.T) {
	t.Parallel()
	if err := validateIntegrationDoc(t, "google_cloud", completeGoogleCloud); err != nil {
		t.Fatalf("a complete Google Cloud block was refused: %v", err)
	}
	if err := validateIntegrationDoc(t, "google_cloud", "    enabled: false"); err != nil {
		t.Fatalf("a switched-off Google Cloud block was refused: %v", err)
	}
	early := "    enabled: true\n    provisioning: {provisioner: crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com, " +
		"federation_key: \"${K}\"}"
	if err := validateIntegrationDoc(t, "google_cloud", early); err != nil {
		t.Fatalf("a block awaiting its provider was refused: %v", err)
	}
	ref := "    enabled: true\n    provisioning: {provisioner: \"${GCP_PROV}\", provider: \"${GCP_PROVIDER}\", federation_key: \"${K}\"}"
	if err := validateIntegrationDoc(t, "google_cloud", ref); err != nil {
		t.Fatalf("a provisioner and provider named by reference were refused: %v", err)
	}
}

// AN ENABLED BLOCK NAMES ITS PROVISIONER AND KEY, and a provider it names is a
// provider's resource name, each refusal naming the field an operator edits.
func TestGoogleCloudRefusesAnIncompleteOrMalformedBlock(t *testing.T) {
	t.Parallel()
	const prov = "crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com"
	for _, tc := range []struct {
		name, body, path string
	}{
		{"no provisioning", "    enabled: true", "integrations.google_cloud.provisioning"},
		{"no provisioner", "    enabled: true\n    provisioning: {federation_key: \"${K}\"}",
			"integrations.google_cloud.provisioning.provisioner"},
		{"a person's email as the provisioner", "    enabled: true\n    provisioning: {provisioner: ops@acme.example, " +
			"federation_key: \"${K}\"}", "integrations.google_cloud.provisioning.provisioner"},
		{"a pool's name as the provider", "    enabled: true\n    provisioning: {provisioner: " + prov +
			", provider: projects/123/locations/global/workloadIdentityPools/crewlet, federation_key: \"${K}\"}",
			"integrations.google_cloud.provisioning.provider"},
		{"a project ID in the provider", "    enabled: true\n    provisioning: {provisioner: " + prov +
			", provider: projects/acme/locations/global/workloadIdentityPools/crewlet/providers/engine, federation_key: \"${K}\"}",
			"integrations.google_cloud.provisioning.provider"},
		{"no federation key", "    enabled: true\n    provisioning: {provisioner: " + prov + "}",
			"integrations.google_cloud.provisioning.federation_key"},
	} {
		err := validateIntegrationDoc(t, "google_cloud", tc.body)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: %v does not name %s", tc.name, err, tc.path)
		}
	}
}

// THE IDENTITY PROJECT IS THE PROVISIONER'S OWN, read from its email.
func TestTheIdentityProjectIsTheProvisionersProject(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		email, project string
		ok             bool
	}{
		{"crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com", "acme-crewlet-id", true},
		{" crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com ", "acme-crewlet-id", true},
		{"crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com.example", "", false},
		{"123456789-compute@developer.gserviceaccount.com", "", false},
		{"Crewlet@acme-crewlet-id.iam.gserviceaccount.com", "", false},
	} {
		project, ok := config.GoogleCloudServiceAccount(tc.email)
		if project != tc.project || ok != tc.ok {
			t.Errorf("%q = %q, %v; want %q, %v", tc.email, project, ok, tc.project, tc.ok)
		}
	}
}

// A SEAT NAMES A LEVEL OF THE CLOSED SET AND ITS PROJECTS: an empty level reads
// as viewer, a seat that is on with no project is refused at its projects, and
// each project is a project ID, once.
func TestAGoogleCloudSeatNamesItsLevelAndProjects(t *testing.T) {
	t.Parallel()
	doc := func(block string) string {
		return "name: Acme\nintegrations:\n  google_cloud:\n" + completeGoogleCloud +
			"\nroles:\n  - {name: SRE, handle: sre, integrations: {google_cloud: {" + block + "}}}\n"
	}
	for _, level := range config.GoogleCloudLevels() {
		c, err := config.ParseCompany([]byte(doc("enabled: true, level: " + level + ", projects: [acme-web]")))
		if err != nil {
			t.Fatalf("%s was refused: %v", level, err)
		}
		if got := c.Roles[0].Integrations.GoogleCloud.LevelOrDefault(); got != level {
			t.Errorf("level = %q, want %s", got, level)
		}
	}
	c, err := config.ParseCompany([]byte(doc("enabled: true, projects: [acme-web, example.com:acme-legacy]")))
	if err != nil {
		t.Fatalf("a seat naming no level, or a domain-scoped project, was refused: %v", err)
	}
	if got := c.Roles[0].Integrations.GoogleCloud.LevelOrDefault(); got != config.GoogleCloudLevelViewer {
		t.Errorf("an unset level = %q, want viewer", got)
	}
	for _, tc := range []struct{ name, block, path string }{
		{"an unknown level", "enabled: true, level: owner, projects: [acme-web]",
			"roles[0].integrations.google_cloud.level"},
		{"on with no project", "enabled: true", "roles[0].integrations.google_cloud.projects"},
		{"a display name as a project", "enabled: true, projects: [Acme Web]",
			"roles[0].integrations.google_cloud.projects[0]"},
		{"a project number as a project", "enabled: true, projects: [\"123456789012\"]",
			"roles[0].integrations.google_cloud.projects[0]"},
		{"a project listed twice", "enabled: true, projects: [acme-web, acme-web]",
			"roles[0].integrations.google_cloud.projects[1]"},
	} {
		if _, err := config.ParseCompany([]byte(doc(tc.block))); err == nil || !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: %v, want a refusal naming %s", tc.name, err, tc.path)
		}
	}
	if _, err := config.ParseCompany([]byte(doc("enabled: false"))); err != nil {
		t.Errorf("a seat that is off with no project was refused: %v", err)
	}
}

// THE ENGINE HOLDS A SEAT'S TOKEN ONLY WHERE IT HOLDS THE PROVISIONER: an
// enabled block with provisioning, and nowhere else. The token is issued at
// use, the one value a seat's tool reads.
func TestGoogleCloudSeatCredentialsAreTheEnginesOnlyWithAProvisioner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		gc   *config.GoogleCloud
		want config.SeatCredentialMode
	}{
		{"absent", nil, config.SeatCredentialExternal},
		{"switched off", &config.GoogleCloud{Provisioning: &config.GoogleCloudProvisioning{}}, config.SeatCredentialExternal},
		{"no provisioning", &config.GoogleCloud{Enabled: true}, config.SeatCredentialExternal},
		{"provisioned", &config.GoogleCloud{Enabled: true, Provisioning: &config.GoogleCloudProvisioning{}}, config.SeatCredentialEngine},
	} {
		c := &config.Company{}
		c.Integrations.GoogleCloud = tc.gc
		if got := c.SeatCredentialMode("google_cloud", false); got != tc.want {
			t.Errorf("%s: mode = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := config.SeatSealedFields("google_cloud"); len(got) != 1 || got[0] != "ACCESS_TOKEN" {
		t.Errorf("seat fields = %v, want [ACCESS_TOKEN]", got)
	}
	if !config.SeatScopeRequired("google_cloud") {
		t.Error("an agent on Google Cloud is not required to name a project")
	}
}

// THE ATLAS'S READER IS ONE SWITCH, OFF UNLESS WRITTEN: it names no account,
// since the account is the identity project's crewlet-atlas.
func TestGoogleCloudAtlasReadIsOffUnlessWritten(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		extra string
		want  bool
	}{{"", false}, {"\n      atlas_read: true", true}, {"\n      atlas_read: false", false}} {
		c, err := config.ParseCompany([]byte("name: Acme\nintegrations:\n  google_cloud:\n" + completeGoogleCloud + tc.extra + "\n"))
		if err != nil {
			t.Fatalf("%q: %v", tc.extra, err)
		}
		if got := c.Integrations.GoogleCloud.Provisioning.AtlasRead; got != tc.want {
			t.Errorf("%q reads atlas_read %v, want %v", tc.extra, got, tc.want)
		}
	}
}
