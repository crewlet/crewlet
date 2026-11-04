package scaleway_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/scaleway"
)

// reconcile runs the company pass against the fake for the seats given.
func reconcile(t *testing.T, f *fakeScaleway, seats map[string]scaleway.Seat, inUse ...string) *scaleway.Result {
	t.Helper()
	client, err := scaleway.NewClient(scaleway.ClientOptions{OrganizationID: testOrg, SecretKey: provSecret, HTTP: f.server(t)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := scaleway.Reconcile(t.Context(), scaleway.Options{
		Client: client, AccessKey: provAccess, Company: "Acme", Seats: seats, InUse: inUse,
		Now: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

// findingsOf is each finding by its title.
func findingsOf(res *scaleway.Result) map[string]integration.Finding {
	out := map[string]integration.Finding{}
	for _, f := range res.Findings() {
		out[f.Title] = f
	}
	return out
}

// A CLEAN ORGANIZATION REPORTS NOTHING: the narrow provisioner, an agent
// application holding the engine's policy and its sealed key alone.
func TestACleanOrganizationReportsNothing(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.ScalewayLevelRead, projectWeb)
	w.sync()
	rec := w.record()
	access, _ := w.sink.Held(sealedAccess)
	res := reconcile(t, w.f, map[string]scaleway.Seat{
		"sre": {ApplicationID: rec.Account.ID, Name: rec.Account.Name, AccessKey: access},
	}, config.ScalewayLevelRead)
	if got := res.Findings(); len(got) != 0 {
		t.Errorf("findings = %+v", got)
	}
	w.f.onlyScaleway(t)
}

// WHAT CREWLET DID NOT MAKE IS REPORTED AND NEVER TOUCHED: a policy under
// another name on an agent's application, a key besides its sealed one, and
// an application named for the company that the ledger does not record.
func TestStrangersOnAgentsApplicationsAreReported(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.ScalewayLevelRead, projectWeb)
	w.sync()
	rec := w.record()
	access, _ := w.sink.Held(sealedAccess)
	org := testOrg
	w.f.edit(func(f *fakeScaleway) {
		f.policies["pol-stranger"] = &fakePolicy{ID: "pol-stranger", Name: "billing-for-sre", AppID: rec.Account.ID,
			CreatedAt: f.now, Rules: []scaleway.Rule{{PermissionSetNames: []string{"BillingManager"}, OrganizationID: &org}}}
		f.keys["SCWPERSONSKEY0000001"] = &fakeKey{Access: "SCWPERSONSKEY0000001", Secret: "00000000-0000-4000-8000-0000000000aa",
			AppID: rec.Account.ID, Description: "debugging", CreatedAt: f.now}
		f.apps["cccccccc-0000-4000-8000-00000000beef"] = &fakeApp{ID: "cccccccc-0000-4000-8000-00000000beef",
			Name: "crewlet-acme-ghost", Org: testOrg, CreatedAt: f.now}
		f.apps["cccccccc-0000-4000-8000-00000000cafe"] = &fakeApp{ID: "cccccccc-0000-4000-8000-00000000cafe",
			Name: "backups", Org: testOrg, CreatedAt: f.now}
	})
	writes := len(w.f.wrote())
	res := reconcile(t, w.f, map[string]scaleway.Seat{
		"sre": {ApplicationID: rec.Account.ID, Name: rec.Account.Name, AccessKey: access},
	})
	got := findingsOf(res)
	if f := got["Policies Crewlet did not write"]; f.Kind != integration.FindingGrantExcess ||
		!slices.Equal(f.Subjects, []string{"sre: billing-for-sre"}) {
		t.Errorf("foreign policies = %+v", f)
	}
	if f := got["API keys beside an agent's own"]; f.Kind != integration.FindingGrantExcess ||
		!slices.Equal(f.Subjects, []string{"sre: SCWPERSONSKEY0000001"}) {
		t.Errorf("extra keys = %+v", f)
	}
	if f := got["Agent applications with no seat"]; f.Kind != integration.FindingRegistrationOrphaned ||
		!slices.Equal(f.Subjects, []string{"crewlet-acme-ghost (cccccccc-0000-4000-8000-00000000beef)"}) {
		t.Errorf("orphans = %+v, want the company's unrecorded application and not the provisioner or another", f)
	}
	if after := w.f.wrote(); len(after) != writes {
		t.Errorf("the company pass wrote %q", after[writes:])
	}
}

// THE PROVISIONER'S OWN STATE IS REPORTED: a key about to expire, a set it
// lacks, a condition refusing key operations, and sets it does not need; a
// level the catalogue cannot grant blocks only while a seat holds it.
func TestTheProvisionersOwnStateIsReported(t *testing.T) {
	t.Parallel()
	f := newFakeScaleway()
	f.keys[provAccess].ExpiresAt = f.now.Add(5 * 24 * time.Hour)
	setRules(f,
		scalewayRule{sets: []string{"IAMApplicationManager"}, condition: `resource.name.startsWith("crewlet-")`},
		scalewayRule{sets: []string{"ProjectReadOnly", "BillingManager"}})
	delete(f.sets, "KubernetesFullAccess")
	got := findingsOf(reconcile(t, f, nil, config.ScalewayLevelRead))
	if e := got["Provisioner key expires soon"]; e.Kind != integration.FindingCredentialExpiring ||
		!e.ExpiresAt.Equal(f.now.Add(5*24*time.Hour)) || e.ActionURL != scaleway.APIKeysURL {
		t.Errorf("expiring = %+v", e)
	}
	if m := got["Provisioner is missing permission sets"]; !slices.Equal(m.Subjects, []string{"IAMPolicyManager"}) {
		t.Errorf("missing = %+v, want IAMPolicyManager alone: a conditioned key rule is its own finding", m)
	}
	if c := got["Provisioner cannot manage the agents' keys"]; c.Kind != integration.FindingGrantShort ||
		!strings.Contains(c.Detail, `resource.name.startsWith("crewlet-")`) {
		t.Errorf("condition = %+v", c)
	}
	if x := got["Provisioner holds more than it needs"]; !slices.Equal(x.Subjects, []string{"BillingManager"}) {
		t.Errorf("excess = %+v", x)
	}
	if g := got["Administer level unavailable"]; g.Kind != integration.FindingLevelUnavailable {
		t.Errorf("the gap on a level no seat holds = %+v, want the advisory", g)
	}
	for title, finding := range got {
		if strings.Contains(finding.Detail+finding.Remedy+title, "\u2014") {
			t.Errorf("%q carries an em dash", title)
		}
	}
}

// A REFUSED OR FOREIGN KEY IS THE CREDENTIAL'S: nothing else the pass could
// report would be trusted over it.
func TestARefusedProvisionerIsTheCredentialsError(t *testing.T) {
	t.Parallel()
	for name, broke := range map[string]func(f *fakeScaleway){
		"a deleted key":                      func(f *fakeScaleway) { delete(f.keys, provAccess) },
		"a user's key":                       func(f *fakeScaleway) { f.keys[provAccess].AppID, f.keys[provAccess].UserID = "", "user-1" },
		"no policy read":                     func(f *fakeScaleway) { f.refuse["policies"] = http.StatusForbidden },
		"another organization's application": func(f *fakeScaleway) { f.apps[provAppID].Org = otherOrg },
	} {
		f := newFakeScaleway()
		broke(f)
		client, _ := scaleway.NewClient(scaleway.ClientOptions{OrganizationID: testOrg, SecretKey: provSecret, HTTP: f.server(t)})
		_, err := scaleway.Reconcile(t.Context(), scaleway.Options{Client: client, AccessKey: provAccess, Company: "Acme"})
		if !errors.Is(err, integration.ErrCredentialRejected) {
			t.Errorf("%s: %v, want the credential rejected", name, err)
		}
		if err != nil && !strings.Contains(err.Error(), "integrations.scaleway.provisioning.secret_key") {
			t.Errorf("%s: %v does not name the field to change", name, err)
		}
	}
	f := newFakeScaleway()
	f.refuse["projects"] = http.StatusBadGateway
	client, _ := scaleway.NewClient(scaleway.ClientOptions{OrganizationID: testOrg, SecretKey: provSecret, HTTP: f.server(t)})
	_, err := scaleway.Reconcile(t.Context(), scaleway.Options{Client: client, AccessKey: provAccess, Company: "Acme"})
	if err == nil || errors.Is(err, integration.ErrCredentialRejected) {
		t.Errorf("a 502 reading projects: %v, want a fault the loop retries", err)
	}
}
