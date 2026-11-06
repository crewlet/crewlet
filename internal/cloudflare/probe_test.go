package cloudflare_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/setup/setuptest"
)

// check is a Test-button press with what the form holds, against a company
// naming one zone the fake account has.
func check(values map[string]string) setup.Check {
	return setup.Check{Field: "api_token", Value: func(field string) string { return values[field] },
		Zones: []string{zoneExampleName}}
}

func probe(t *testing.T, f *fakeCloudflare, values map[string]string) (setup.Verdict, error) {
	t.Helper()
	v, err := cloudflare.Probe(f.server(t))(t.Context(), check(values))
	if err == nil {
		setuptest.Said(t, "the Cloudflare probe", v, values["api_token"])
	}
	return v, err
}

// said is everything a verdict tells a person.
func said(v setup.Verdict) string {
	return setuptest.Spoken(v)
}

var typed = map[string]string{"account_id": testAccount, "api_token": provisioner}

// A WORKING PROVISIONER IS SAID TO WORK, with what it reads and when it lapses,
// and the check writes nothing.
func TestTheProbePassesAWorkingProvisioner(t *testing.T) {
	t.Parallel()
	f := newFakeCloudflare()
	f.provisionerExpires = "2099-03-01T00:00:00Z"
	v, err := probe(t, f, typed)
	if err != nil || v.Outcome != setup.VerifyWorks {
		t.Fatalf("probe = %+v, %v", v, err)
	}
	wantFacts := []setup.Fact{{Label: "Zones", Value: "3"}, {Label: "Expires", Value: "2099-03-01"}}
	if !slices.Equal(v.Facts, wantFacts) || v.Warnings != nil {
		t.Errorf("verdict = %+v, want facts %+v and no warning", v, wantFacts)
	}
	if len(f.wrote()) != 0 {
		t.Errorf("the check wrote %v", f.wrote())
	}
	f.onlyCloudflare(t)
}

// A LEVEL THE TOKEN LEAVES UNAVAILABLE IS A WARNING WITH ITS REMEDY, and an
// expiry inside the warning window says to replace it. Mutation: drop a level
// gap's remedy, and its action is empty.
func TestAWorkingTokenWarnsWithItsActions(t *testing.T) {
	t.Parallel()
	f := newFakeCloudflare()
	withoutHeld("Cache Purge")(f)
	soon := time.Now().UTC().AddDate(0, 0, 3)
	f.provisionerExpires = soon.Format(time.RFC3339)
	v, err := probe(t, f, typed)
	if err != nil || v.Outcome != setup.VerifyWorks {
		t.Fatalf("probe = %+v, %v", v, err)
	}
	if v.Summary != "Works. The token is active." {
		t.Errorf("summary = %q", v.Summary)
	}
	if !slices.ContainsFunc(v.Warnings, func(w setup.Warning) bool {
		return strings.Contains(w.Title, "Operate level is unavailable") &&
			strings.HasPrefix(w.Action, "Edit the provisioner token at Cloudflare")
	}) {
		t.Errorf("warnings = %+v, want the level's gap with its remedy", v.Warnings)
	}
	if want := (setup.Warning{
		Title: "It expires soon.", Action: "Create its replacement before " + soon.Format(time.DateOnly) + ".",
	}); !slices.Contains(v.Warnings, want) {
		t.Errorf("warnings = %+v, want %+v", v.Warnings, want)
	}
}

// EACH FAILURE NAMES WHAT TO CHANGE: the token, the permission a read needs, a
// group the catalogue lacks, or one the token does not hold itself.
func TestTheProbeNamesWhatIsWrong(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		arrange func(*fakeCloudflare)
		values  map[string]string
		want    setup.VerifyOutcome
		says    string
	}{
		{"a wrong token", nil, map[string]string{"account_id": testAccount, "api_token": "fake-other"},
			setup.VerifyRefused, "Cloudflare refused the token"},
		{"a disabled token", func(f *fakeCloudflare) { f.provisionerStatus = cloudflare.StatusDisabled }, typed,
			setup.VerifyRefused, "disabled"},
		{"no token rights", func(f *fakeCloudflare) { f.refuse["groups"] = http.StatusForbidden }, typed,
			setup.VerifyRefused, "Account API Tokens Write"},
		{"no zone rights", func(f *fakeCloudflare) { f.refuse["zones"] = http.StatusForbidden }, typed,
			setup.VerifyRefused, "Zone Read"},
		{"a group Cloudflare no longer lists", func(f *fakeCloudflare) {
			f.groups = slices.DeleteFunc(f.groups, func(g fakeGroup) bool { return g.Name == "DNS Read" })
		}, typed, setup.VerifyRefused, "DNS Read"},
		// A GROUP A HIGHER LEVEL GRANTS leaves that level unavailable and the
		// token working; one the default level or the token itself needs
		// leaves no agent anything.
		{"a group only higher levels grant", withoutHeld("Cache Purge"), typed, setup.VerifyWorks,
			"The Operate level is unavailable until the provisioner token holds Cache Purge: Purge"},
		{"a group the default level grants", withoutHeld("DNS Read"), typed, setup.VerifyRefused,
			"no agent can be given a level yet. | The Read level is unavailable until the provisioner token holds DNS: Read"},
		{"a group the token needs itself", withoutHeld("Account Settings Read"), typed, setup.VerifyRefused,
			"missing Account Settings: Read. | Crewlet needs it to manage the agents' tokens. Edit the token at Cloudflare"},
		{"a zone the account lacks", func(f *fakeCloudflare) {
			f.zones = slices.DeleteFunc(f.zones, func(z cloudflare.Zone) bool { return z.Name == zoneExampleName })
		}, typed, setup.VerifyRefused, `no zone named "example.com"`},
		{"Cloudflare down", func(f *fakeCloudflare) { f.refuse["verify"] = http.StatusServiceUnavailable }, typed,
			setup.VerifyUnreachable, "could not say"},
		// ITS OWN POLICY NOT SHOWN is an answer, so no gap is judged; any other
		// failure to read it is one.
		{"its own policy not shown", func(f *fakeCloudflare) { f.refuseID[provisionerID] = http.StatusForbidden },
			typed, setup.VerifyWorks, "Works. The token holds every permission an agent can be given. | Zones: 3"},
		{"its own policy unreadable", func(f *fakeCloudflare) { f.refuseID[provisionerID] = http.StatusBadGateway },
			typed, setup.VerifyUnreachable, "502 Bad Gateway"},
	} {
		f := newFakeCloudflare()
		if tc.arrange != nil {
			tc.arrange(f)
		}
		v, err := probe(t, f, tc.values)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if v.Outcome != tc.want || !strings.Contains(said(v), tc.says) {
			t.Errorf("%s: %s %q, want %s saying %q", tc.name, v.Outcome, said(v), tc.want, tc.says)
		}
		if strings.Contains(said(v), provisioner) || strings.Contains(said(v), "fake-other") {
			t.Errorf("%s: the verdict repeats the token", tc.name)
		}
	}
}

// INPUTS THE CHECK CANNOT BE MADE FROM ARE THE REQUEST'S FAULT, and a field it
// does not check is a wiring mistake.
func TestTheProbeRefusesWhatItCannotCheck(t *testing.T) {
	t.Parallel()
	f := newFakeCloudflare()
	for _, values := range []map[string]string{
		{"account_id": testAccount},
		{"api_token": provisioner, "account_id": "acme"},
	} {
		if _, err := probe(t, f, values); !errors.Is(err, setup.ErrUnusableInput) {
			t.Errorf("probe(%v) = %v, want unusable input", values, err)
		}
	}
	if _, err := cloudflare.Probe(nil)(t.Context(), setup.Check{Field: "account_id"}); !errors.Is(err, setup.ErrNotCheckable) {
		t.Errorf("checking the account ID alone = %v, want not checkable", err)
	}
}

// THE FORM AND THE PROBE AGREE: the checkable field is the one the probe
// checks, it is checked with the account ID, and every path is the document's.
func TestTheFormIsTheDocumentsAndTheProbes(t *testing.T) {
	t.Parallel()
	cf := &config.Cloudflare{Enabled: true, Provisioning: &config.CloudflareProvisioning{
		AccountID: testAccount, APIToken: "${CLOUDFLARE_API_TOKEN}"}}
	resolve := func(name string) (string, bool) { return "fake", name == "CLOUDFLARE_API_TOKEN" }
	byField := map[string]setup.Requirement{}
	for _, r := range cloudflare.Requirements(cf, resolve) {
		byField[r.Field] = r
		if !r.Present {
			t.Errorf("%s is answered in the document and reads absent", r.Field)
		}
	}
	token := byField["api_token"]
	if !token.Checkable || !slices.Equal(token.CheckWith, []string{"account_id"}) || token.SecretName != cloudflare.ProvisionerSecret {
		t.Errorf("api_token = %+v", token)
	}
	if byField["account_id"].ConfigPath != "integrations.cloudflare.provisioning.account_id" {
		t.Errorf("account_id path = %q", byField["account_id"].ConfigPath)
	}
	for _, r := range cloudflare.Requirements(nil, nil) {
		if r.Present {
			t.Errorf("%s reads present on a company with no block", r.Field)
		}
	}
}

// withoutHeld is a provisioner that holds every group but name.
func withoutHeld(name string) func(*fakeCloudflare) {
	return func(f *fakeCloudflare) {
		own := f.provisionerOwn[0]
		own.PermissionGroups = slices.DeleteFunc(slices.Clone(own.PermissionGroups),
			func(g cloudflare.GroupRef) bool { return g.ID == groupID(name) })
		f.provisionerOwn = []cloudflare.Policy{own}
	}
}
