package googlecloud_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/setup"
)

// THE CONNECT FORM IS ONE PASS: the identity project first, offered as a
// suggestion, the federation key the engine mints when the form opens, then
// the provider the key's block prints, required to finish and never holding a
// save, which is the one field the Test checks, with the other two.
func TestTheConnectFormIsOnePassInOrder(t *testing.T) {
	t.Parallel()
	reqs := googlecloud.Requirements(testCompany, nil, nil)
	var fields []string
	for _, r := range reqs {
		fields = append(fields, r.Field)
	}
	if !slices.Equal(fields, []string{"identity_project", "federation_key", "provider", "atlas_read", "enabled"}) {
		t.Fatalf("fields %v", fields)
	}
	project, key, provider := reqs[0], reqs[1], reqs[2]
	if project.Default != googlecloud.SuggestedProject(testCompany) || project.ValueProblem(project.Default) != "" {
		t.Errorf("the identity project offers %q, want a valid suggestion", project.Default)
	}
	if !key.Mintable || !key.MintFirst || key.Kind != setup.KindSecret || key.SecretName != googlecloud.FederationKeySecret {
		t.Errorf("the federation key is %+v, want a sealed value the engine mints when the form opens", key)
	}
	if !provider.Required || provider.PrintedBy != "federation_key" || !provider.Checkable ||
		!slices.Equal(provider.CheckWith, []string{"identity_project", "federation_key"}) {
		t.Errorf("the provider is %+v, want it required, printed by the key's block and checked with the other two",
			provider)
	}
	if !reqs[4].Hidden || reqs[4].Default != "true" {
		t.Errorf("enabled is %+v, want hidden and on", reqs[4])
	}
	block := &config.GoogleCloud{Enabled: true, Provisioning: &config.GoogleCloudProvisioning{
		IdentityProject: identity, FederationKey: "${" + googlecloud.FederationKeySecret + "}",
	}}
	held := func(name string) (string, bool) { return "a key", name == googlecloud.FederationKeySecret }
	if got := googlecloud.Requirements(testCompany, block, held)[2]; !got.Required || got.PrintedBy == "" {
		t.Errorf("once the key is held, the provider is %+v", got)
	}
	if summary := googlecloud.Guide().Summary; !strings.Contains(summary, "No service account key is made") {
		t.Errorf("the summary %q does not say no key is made", summary)
	}
}

// A PASTE IS HELD TO GOOGLE'S SHAPE before it is stored: the identity project
// is a project ID a service account can name, the provider a provider's full
// name, and a reference is a pointer, never judged.
func TestAPasteIsHeldToGooglesShape(t *testing.T) {
	t.Parallel()
	reqs := googlecloud.Requirements(testCompany, nil, nil)
	project, provider := reqs[0], reqs[2]
	for _, tc := range []struct {
		req   setup.Requirement
		value string
		ok    bool
	}{
		{project, identity, true},
		{project, "Acme Crewlet", false},
		{project, provisioner, false},
		{project, "example.com:acme-crewlet", false},
		{project, "${GCP_IDENTITY}", true},
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
		IdentityProject: identity, Provider: testProvider, FederationKey: "${" + googlecloud.FederationKeySecret + "}",
		AtlasRead: true,
	}}
	for _, r := range googlecloud.Requirements(testCompany, block, nil) {
		want := map[string]string{
			"identity_project": identity, "provider": testProvider,
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

// THE BLOCK SAYS WHICH ANSWERS IT IS DRAWN FROM, and exactly those: a form
// redraws it when one of them changes. Mutation: leave atlas_read out of
// drawn_from, or add a field the block ignores, and this fails.
func TestTheBlockIsDrawnFromExactlyTheFieldsItNames(t *testing.T) {
	t.Parallel()
	_, pemText := fakeKey(t)
	base := config.GoogleCloudProvisioning{
		IdentityProject: identity, FederationKey: "${" + googlecloud.FederationKeySecret + "}", Provider: testProvider,
	}
	resolve := func(name string) (string, bool) { return pemText, name == googlecloud.FederationKeySecret }
	block := func(p config.GoogleCloudProvisioning) setup.Requirement {
		for _, r := range googlecloud.Requirements(testCompany, &config.GoogleCloud{Enabled: true, Provisioning: &p}, resolve) {
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
	other.IdentityProject = "acme-other-id"
	changed["identity_project"] = other
	other = base
	other.AtlasRead = true
	changed["atlas_read"] = other
	other = base
	other.Provider = "projects/999/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine"
	changed["provider"] = other
	var drawn []string
	for _, field := range []string{"identity_project", "provider", "atlas_read"} {
		if block(changed[field]).Public.Text != plain.Public.Text {
			drawn = append(drawn, field)
		}
	}
	if !slices.Equal(drawn, []string{"identity_project", "atlas_read"}) ||
		!slices.Equal(plain.Public.DrawnFrom, []string{"identity_project", "atlas_read"}) {
		t.Errorf("the block changes with %v and names %v", drawn, plain.Public.DrawnFrom)
	}
}

// THE BLOCK FOLLOWS WHAT IS TYPED: drawn for the typed project and Atlas with
// the key Crewlet holds, live, and nothing for a project that is no ID, which
// says what to type instead.
func TestTheBlockIsDrawnFromWhatIsTyped(t *testing.T) {
	t.Parallel()
	_, pemText := fakeKey(t)
	form := map[string]string{"identity_project": "acme-ids-2", "atlas_read": "true", "federation_key": pemText}
	draw := func(field string) (*setup.PublicView, error) {
		return googlecloud.Draw(setup.Draw{Field: field, Company: testCompany, Value: func(f string) string { return form[f] }})
	}
	view, err := draw("federation_key")
	if err != nil || !view.Live || !strings.Contains(view.Text, "IDENTITY=acme-ids-2\n") ||
		!strings.Contains(view.Text, googlecloud.AtlasAccountID) ||
		!slices.Equal(view.DrawnFrom, []string{"identity_project", "atlas_read"}) {
		t.Fatalf("drawn %+v, %v", view, err)
	}
	form["identity_project"] = "Acme IDs"
	if _, err := draw("federation_key"); !errors.Is(err, setup.ErrUnusableInput) {
		t.Errorf("a name for an ID: %v, want what to type", err)
	}
	if _, err := draw("provider"); !errors.Is(err, setup.ErrNotDrawable) {
		t.Errorf("the provider: %v, want nothing drawn", err)
	}
}
