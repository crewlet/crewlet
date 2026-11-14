package scaleway_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/scaleway"
	"github.com/crewlet/crewlet/internal/setup"
)

// THE CONNECT FORM SAYS WHAT THE KEY CAN DO, ONCE: its caution names the
// key's reach and asks for a short expiry, the steps name the three sets, and
// every value is held to Scaleway's shape.
func TestTheConnectFormSaysWhatTheKeyReaches(t *testing.T) {
	t.Parallel()
	reqs := map[string]setup.Requirement{}
	for _, r := range scaleway.Requirements(nil, nil) {
		reqs[r.Field] = r
	}
	guide := scaleway.Guide()
	if guide.Caution == nil {
		t.Fatal("the form carries no caution for a key that administers IAM")
	}
	for _, want := range []string{
		"mint keys for, and grant anything to, any application", "no condition can limit it",
		"expiry short", "rotate it",
	} {
		if !strings.Contains(guide.Caution.Body, want) {
			t.Errorf("the caution does not say %q: %q", want, guide.Caution.Body)
		}
	}
	var granted bool
	for _, step := range guide.Prepare {
		if strings.Join(step.Grants, ",") == "IAMApplicationManager,IAMPolicyManager,ProjectReadOnly" {
			granted = true
		}
	}
	if !granted {
		t.Errorf("no step grants the three sets the provisioner needs: %+v", guide.Prepare)
	}
	secret := reqs["secret_key"]
	if secret.Kind != setup.KindSecret || secret.SecretName != scaleway.ProvisionerSecret || !secret.Checkable ||
		strings.Join(secret.CheckWith, ",") != "organization_id,access_key" || secret.Shape != setup.ShapeUUID {
		t.Errorf("secret key = %+v", secret)
	}
	if reqs["organization_id"].Shape != setup.ShapeUUID || reqs["access_key"].Shape != setup.ShapeScalewayAccessKey {
		t.Error("an identifier is not held to its shape")
	}
	if !strings.Contains(reqs["access_key"].More, "IAMApplicationManager, IAMPolicyManager and ProjectReadOnly") {
		t.Errorf("the access key does not name the three sets: %q", reqs["access_key"].More)
	}
}

// A PASTE THAT IS NOT SCALEWAY'S SHAPE IS REFUSED AT ITS FIELD, without the
// value quoted back; a reference is never judged.
func TestAPasteIsHeldToScalewaysShape(t *testing.T) {
	t.Parallel()
	reqs := map[string]setup.Requirement{}
	for _, r := range scaleway.Requirements(nil, nil) {
		reqs[r.Field] = r
	}
	for _, tc := range []struct {
		field, value string
		ok           bool
	}{
		{"organization_id", testOrg, true},
		{"organization_id", "acme", false},
		{"access_key", provAccess, true},
		{"access_key", "SCW123", false},
		{"access_key", "scwprovisionerkey001", false},
		{"secret_key", provSecret, true},
		{"secret_key", "not-a-uuid-at-all", false},
		{"secret_key", "${SCALEWAY_SECRET_KEY}", true},
	} {
		problem := reqs[tc.field].ValueProblem(tc.value)
		if (problem == "") != tc.ok {
			t.Errorf("%s %q: problem %q", tc.field, tc.value, problem)
		}
		if problem != "" && strings.Contains(problem, tc.value) {
			t.Errorf("%s: the refusal quotes the value", tc.field)
		}
	}
}

// WHAT IS STORED IS SAID AS HELD, whether written or referenced.
func TestTheFormReadsWhatTheDocumentHolds(t *testing.T) {
	t.Parallel()
	block := &config.Scaleway{Enabled: true, Atlas: true, Provisioning: &config.ScalewayProvisioning{
		OrganizationID: testOrg, AccessKey: provAccess, SecretKey: "${" + scaleway.ProvisionerSecret + "}",
	}}
	resolve := func(name string) (string, bool) { return provSecret, name == scaleway.ProvisionerSecret }
	for _, r := range scaleway.Requirements(block, resolve) {
		if !r.Present || (r.Resolved != nil && !*r.Resolved) {
			t.Errorf("%s: present %v, resolved %v", r.Field, r.Present, r.Resolved)
		}
	}
}

// SEE MORE IN ATLAS IS ONE OPTIONAL SWITCH, OFF UNLESS SOMEBODY TURNS IT ON:
// the form shows it, never requires it, offers it off, names every permission
// set the application would hold and says how to undo it.
//
// Mutation: require the switch or default it on, and connecting Scaleway
// makes an application nobody asked for.
func TestSeeMoreInAtlasIsOneOptionalSwitch(t *testing.T) {
	t.Parallel()
	var atlas setup.Requirement
	for _, r := range scaleway.Requirements(&config.Scaleway{Enabled: true}, nil) {
		if r.Field == "atlas" {
			atlas = r
		}
	}
	if atlas.Kind != setup.KindToggle || atlas.ConfigPath != "integrations.scaleway.atlas" ||
		atlas.Required || atlas.Hidden || atlas.Connect || atlas.Default != "false" || atlas.Present {
		t.Fatalf("the switch: %+v", atlas)
	}
	for _, set := range scaleway.AtlasSets() {
		if !strings.Contains(atlas.More, set) {
			t.Errorf("the switch does not name %s", set)
		}
	}
	if !strings.Contains(atlas.More, "Turn this off to delete") {
		t.Errorf("the switch does not say how to undo it: %q", atlas.More)
	}
}
