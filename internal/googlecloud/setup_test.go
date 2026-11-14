package googlecloud_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/setup"
)

// THE CONNECT FORM IS ONE PASS: the provisioner first, the federation key the
// engine mints on that save, then the provider, required only once the key is
// held, which is the one field the Test button checks, with the other two.
func TestTheConnectFormIsOnePassInOrder(t *testing.T) {
	t.Parallel()
	reqs := googlecloud.Requirements(testCompany, nil, nil)
	var fields []string
	for _, r := range reqs {
		fields = append(fields, r.Field)
	}
	if !slices.Equal(fields, []string{"provisioner", "federation_key", "provider", "atlas_read", "enabled"}) {
		t.Fatalf("fields %v", fields)
	}
	key, provider := reqs[1], reqs[2]
	if !key.Mintable || key.Kind != setup.KindSecret || key.SecretName != googlecloud.FederationKeySecret {
		t.Errorf("the federation key is %+v, want a sealed value the engine mints", key)
	}
	if provider.Required || !provider.Checkable || !slices.Equal(provider.CheckWith, []string{"provisioner", "federation_key"}) {
		t.Errorf("before the key, the provider is %+v, want it optional and checked with the other two", provider)
	}
	if !reqs[4].Hidden || reqs[4].Default != "true" {
		t.Errorf("enabled is %+v, want hidden and on", reqs[4])
	}
	block := &config.GoogleCloud{Enabled: true, Provisioning: &config.GoogleCloudProvisioning{
		Provisioner: provisioner, FederationKey: "${" + googlecloud.FederationKeySecret + "}",
	}}
	held := func(name string) (string, bool) { return "a key", name == googlecloud.FederationKeySecret }
	if got := googlecloud.Requirements(testCompany, block, held)[2]; !got.Required {
		t.Error("once the key is held, the provider is not required")
	}
	unheld := func(string) (string, bool) { return "", false }
	if got := googlecloud.Requirements(testCompany, block, unheld)[2]; got.Required {
		t.Error("a key reference that resolves to nothing made the provider required")
	}
	if summary := googlecloud.Guide().Summary; !strings.Contains(summary, "No service account key is made") {
		t.Errorf("the summary %q does not say no key is made", summary)
	}
}

// A PASTE IS HELD TO GOOGLE'S SHAPE before it is stored: the provisioner is a
// service account's email, the provider a provider's full name, and a
// reference is a pointer, never judged.
func TestAPasteIsHeldToGooglesShape(t *testing.T) {
	t.Parallel()
	reqs := googlecloud.Requirements(testCompany, nil, nil)
	prov, provider := reqs[0], reqs[2]
	for _, tc := range []struct {
		req   setup.Requirement
		value string
		ok    bool
	}{
		{prov, provisioner, true},
		{prov, "ops@acme.example", false},
		{prov, "${GCP_PROVISIONER}", true},
		{provider, testProvider, true},
		{provider, "crewlet-engine", false},
	} {
		if got := tc.req.ValueProblem(tc.value); (got == "") != tc.ok {
			t.Errorf("%s %q: %q", tc.req.Field, tc.value, got)
		}
	}
}

// THE FORM READS WHAT THE DOCUMENT HOLDS, each field from its own position.
func TestTheFormReadsWhatTheDocumentHolds(t *testing.T) {
	t.Parallel()
	block := &config.GoogleCloud{Enabled: true, Provisioning: &config.GoogleCloudProvisioning{
		Provisioner: provisioner, Provider: testProvider, FederationKey: "${" + googlecloud.FederationKeySecret + "}",
		AtlasRead: true,
	}}
	for _, r := range googlecloud.Requirements(testCompany, block, nil) {
		want := map[string]string{
			"provisioner": provisioner, "provider": testProvider,
			"federation_key": "${" + googlecloud.FederationKeySecret + "}", "atlas_read": "true", "enabled": "true",
		}[r.Field]
		if !r.Present || r.Stored != want {
			t.Errorf("%s: present %v, stored %q; want %q", r.Field, r.Present, r.Stored, want)
		}
		if !strings.HasPrefix(r.ConfigPath, "integrations.google_cloud.") {
			t.Errorf("%s writes %s", r.Field, r.ConfigPath)
		}
	}
}
