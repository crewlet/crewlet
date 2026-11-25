package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// completeGoogleCloud is an enabled block with every requirement answered.
const completeGoogleCloud = `    enabled: true
    provisioning:
      identity_project: acme-crewlet-id
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
	early := "    enabled: true\n    provisioning: {identity_project: acme-crewlet-id, " +
		"federation_key: \"${K}\"}"
	if err := validateIntegrationDoc(t, "google_cloud", early); err != nil {
		t.Fatalf("a block awaiting its provider was refused: %v", err)
	}
	ref := "    enabled: true\n    provisioning: {identity_project: \"${GCP_IDENTITY}\", provider: \"${GCP_PROVIDER}\", " +
		"federation_key: \"${K}\"}"
	if err := validateIntegrationDoc(t, "google_cloud", ref); err != nil {
		t.Fatalf("an identity project and provider named by reference were refused: %v", err)
	}
}

// AN ENABLED BLOCK NAMES ITS IDENTITY PROJECT AND KEY, and a provider it names
// is a provider's resource name, each refusal naming the field an operator
// edits.
func TestGoogleCloudRefusesAnIncompleteOrMalformedBlock(t *testing.T) {
	t.Parallel()
	const prov = "identity_project: acme-crewlet-id"
	for _, tc := range []struct {
		name, body, path string
	}{
		{"no provisioning", "    enabled: true", "integrations.google_cloud.provisioning"},
		{"no identity project", "    enabled: true\n    provisioning: {federation_key: \"${K}\"}",
			"integrations.google_cloud.provisioning.identity_project"},
		{"a project's name as its ID", "    enabled: true\n    provisioning: {identity_project: Acme Crewlet, " +
			"federation_key: \"${K}\"}", "integrations.google_cloud.provisioning.identity_project"},
		{"a domain-scoped project", "    enabled: true\n    provisioning: {identity_project: \"example.com:acme\", " +
			"federation_key: \"${K}\"}", "integrations.google_cloud.provisioning.identity_project"},
		{"a pool's name as the provider", "    enabled: true\n    provisioning: {" + prov +
			", provider: projects/123/locations/global/workloadIdentityPools/crewlet, federation_key: \"${K}\"}",
			"integrations.google_cloud.provisioning.provider"},
		{"a project ID in the provider", "    enabled: true\n    provisioning: {" + prov +
			", provider: projects/acme/locations/global/workloadIdentityPools/crewlet/providers/engine, federation_key: \"${K}\"}",
			"integrations.google_cloud.provisioning.provider"},
		{"no federation key", "    enabled: true\n    provisioning: {" + prov + "}",
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

// THE PROVISIONER IS CREWLET'S NAME IN THE IDENTITY PROJECT, which a person
// names alone.
func TestTheIdentityProjectIsTheProvisionersProject(t *testing.T) {
	t.Parallel()
	if got := config.GoogleCloudProvisioner("acme-crewlet-id"); got != "crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com" {
		t.Errorf("the provisioner of acme-crewlet-id is %q", got)
	}
	g := &config.GoogleCloud{Provisioning: &config.GoogleCloudProvisioning{IdentityProject: " acme-crewlet-id "}}
	if project, ok := g.Identity(); !ok || project != "acme-crewlet-id" {
		t.Errorf("identity %q, %v", project, ok)
	}
	g.Provisioning.IdentityProject = "${GCP_IDENTITY}"
	if _, ok := g.Identity(); ok {
		t.Error("a reference read as a project ID")
	}
}

// A SERVICE ACCOUNT'S EMAIL NAMES ITS PROJECT, and nothing else does.
func TestAServiceAccountsEmailNamesItsProject(t *testing.T) {
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

// A FORM OPENED AND CLOSED STARTS NOTHING: the key Crewlet made when it opened
// is no connection to pause or finish, and anything a person saved is.
func TestTheKeyAloneIsNotAStartedConnection(t *testing.T) {
	t.Parallel()
	key := "${GOOGLE_CLOUD_FEDERATION_KEY}"
	for _, tc := range []struct {
		in   *config.GoogleCloud
		want bool
	}{
		{nil, false},
		{&config.GoogleCloud{Provisioning: &config.GoogleCloudProvisioning{FederationKey: key}}, false},
		{&config.GoogleCloud{Provisioning: &config.GoogleCloudProvisioning{FederationKey: key, IdentityProject: "acme-crewlet-id"}}, true},
		{&config.GoogleCloud{Provisioning: &config.GoogleCloudProvisioning{FederationKey: key, AtlasRead: true}}, true},
		{&config.GoogleCloud{Enabled: true, Provisioning: &config.GoogleCloudProvisioning{FederationKey: key}}, true},
	} {
		if got := tc.in.Started(); got != tc.want {
			t.Errorf("%+v: started %v, want %v", tc.in, got, tc.want)
		}
	}
}
