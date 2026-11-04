package scaleway_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/scaleway"
	"github.com/crewlet/crewlet/internal/setup"
)

// THE CONNECT FORM SAYS WHAT THE KEY CAN DO WHERE IT IS PASTED: it can mint
// keys for, and grant anything to, any application in the organization, so it
// asks for a short expiry and a rotation. Every value is held to Scaleway's
// shape, and the secret is checked as the access key it belongs to.
func TestTheConnectFormSaysWhatTheKeyReaches(t *testing.T) {
	t.Parallel()
	reqs := map[string]setup.Requirement{}
	for _, r := range scaleway.Requirements(nil, nil) {
		reqs[r.Field] = r
		for _, text := range []string{r.Label, r.Help, r.LinkText, r.Format} {
			if strings.Contains(text, "\u2014") {
				t.Errorf("%s carries an em dash: %q", r.Field, text)
			}
		}
	}
	secret := reqs["secret_key"]
	for _, want := range []string{
		"mint keys for, and grant anything to, any application", "no condition can limit it",
		"short expiry", "rotate it",
	} {
		if !strings.Contains(secret.Help, want) {
			t.Errorf("the secret key's help does not say %q", want)
		}
	}
	if secret.Kind != setup.KindSecret || secret.SecretName != scaleway.ProvisionerSecret || !secret.Checkable ||
		strings.Join(secret.CheckWith, ",") != "organization_id,access_key" || secret.Shape != setup.ShapeUUID {
		t.Errorf("secret key = %+v", secret)
	}
	if reqs["organization_id"].Shape != setup.ShapeUUID || reqs["access_key"].Shape != setup.ShapeScalewayAccessKey {
		t.Error("an identifier is not held to its shape")
	}
	if !strings.Contains(reqs["access_key"].Help, "IAMApplicationManager, IAMPolicyManager and ProjectReadOnly") {
		t.Errorf("the access key's help does not name the three sets: %q", reqs["access_key"].Help)
	}
	if s := scaleway.Summary(); strings.Contains(s, "\u2014") || !strings.Contains(s, "IAM application") {
		t.Errorf("summary = %q", s)
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
	block := &config.Scaleway{Enabled: true, Provisioning: &config.ScalewayProvisioning{
		OrganizationID: testOrg, AccessKey: provAccess, SecretKey: "${" + scaleway.ProvisionerSecret + "}",
	}}
	resolve := func(name string) (string, bool) { return provSecret, name == scaleway.ProvisionerSecret }
	for _, r := range scaleway.Requirements(block, resolve) {
		if !r.Present || (r.Resolved != nil && !*r.Resolved) {
			t.Errorf("%s: present %v, resolved %v", r.Field, r.Present, r.Resolved)
		}
	}
}
