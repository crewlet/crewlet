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
// engine mints on that save, then the provider the key's block prints, required
// to finish and never holding a save, which is the one field the Test checks,
// with the other two.
func TestTheConnectFormIsOnePassInOrder(t *testing.T) {
	t.Parallel()
	reqs := googlecloud.Requirements(testCompany, nil, nil, nil)
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
	if !provider.Required || provider.PrintedBy != "federation_key" || !provider.Checkable ||
		!slices.Equal(provider.CheckWith, []string{"provisioner", "federation_key"}) {
		t.Errorf("the provider is %+v, want it required, printed by the key's block and checked with the other two",
			provider)
	}
	if !reqs[4].Hidden || reqs[4].Default != "true" {
		t.Errorf("enabled is %+v, want hidden and on", reqs[4])
	}
	block := &config.GoogleCloud{Enabled: true, Provisioning: &config.GoogleCloudProvisioning{
		Provisioner: provisioner, FederationKey: "${" + googlecloud.FederationKeySecret + "}",
	}}
	held := func(name string) (string, bool) { return "a key", name == googlecloud.FederationKeySecret }
	if got := googlecloud.Requirements(testCompany, block, nil, held)[2]; !got.Required || got.PrintedBy == "" {
		t.Errorf("once the key is held, the provider is %+v", got)
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
	reqs := googlecloud.Requirements(testCompany, nil, nil, nil)
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
	for _, r := range googlecloud.Requirements(testCompany, block, nil, nil) {
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

// THE BLOCK SAYS WHICH SAVED ANSWERS IT IS DRAWN FROM, and exactly those: a
// form hides it while one of them has an unsaved change. Mutation: leave
// atlas_read out of drawn_from, or add a field the block ignores, and this fails.
func TestTheBlockIsDrawnFromExactlyTheFieldsItNames(t *testing.T) {
	t.Parallel()
	_, pemText := fakeKey(t)
	base := config.GoogleCloudProvisioning{
		Provisioner: provisioner, FederationKey: "${" + googlecloud.FederationKeySecret + "}", Provider: testProvider,
	}
	resolve := func(name string) (string, bool) { return pemText, name == googlecloud.FederationKeySecret }
	block := func(p config.GoogleCloudProvisioning) setup.Requirement {
		for _, r := range googlecloud.Requirements(testCompany, &config.GoogleCloud{Enabled: true, Provisioning: &p}, nil, resolve) {
			if r.Public != nil {
				return r
			}
		}
		t.Fatal("no row carries the block")
		return setup.Requirement{}
	}
	plain := block(base)
	changed := map[string]config.GoogleCloudProvisioning{}
	other := base
	other.Provisioner = "crewlet-provisioner@acme-other-id.iam.gserviceaccount.com"
	changed["provisioner"] = other
	other = base
	other.AtlasRead = true
	changed["atlas_read"] = other
	other = base
	other.Provider = "projects/999/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine"
	changed["provider"] = other
	var drawn []string
	for _, field := range []string{"provisioner", "provider", "atlas_read"} {
		if block(changed[field]).Public.Text != plain.Public.Text {
			drawn = append(drawn, field)
		}
	}
	if !slices.Equal(drawn, []string{"provisioner", "atlas_read"}) ||
		!slices.Equal(plain.Public.DrawnFrom, []string{"provisioner", "atlas_read"}) {
		t.Errorf("the block changes with %v and names %v", drawn, plain.Public.DrawnFrom)
	}
}
