package cloudflare_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/setup"
)

// check is a Test-button press with what the form holds, against a company
// naming one zone the fake account has.
func check(values map[string]string) setup.Check {
	return setup.Check{Field: "api_token", Value: func(field string) string { return values[field] },
		Zones: []string{zoneExampleName}}
}

func probe(t *testing.T, f *fakeCloudflare, values map[string]string) (setup.Verdict, error) {
	t.Helper()
	return cloudflare.Probe(f.server(t))(t.Context(), check(values))
}

var typed = map[string]string{"account_id": testAccount, "api_token": provisioner}

// A WORKING PROVISIONER IS SAID TO WORK, with what it reads and when it lapses,
// and the check writes nothing.
func TestTheProbePassesAWorkingProvisioner(t *testing.T) {
	t.Parallel()
	f := newFakeCloudflare()
	f.provisionerExpires = "2027-03-01T00:00:00Z"
	v, err := probe(t, f, typed)
	if err != nil || v.Outcome != setup.VerifyWorks {
		t.Fatalf("probe = %+v, %v", v, err)
	}
	for _, want := range []string{"3 zones", "2027-03-01"} {
		if !strings.Contains(v.Detail, want) {
			t.Errorf("verdict %q does not say %q", v.Detail, want)
		}
	}
	if len(f.wrote()) != 0 {
		t.Errorf("the check wrote %v", f.wrote())
	}
	f.onlyCloudflare(t)
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
		{"a group the token does not hold", func(f *fakeCloudflare) {
			own := f.provisionerOwn[0]
			own.PermissionGroups = slices.DeleteFunc(slices.Clone(own.PermissionGroups),
				func(g cloudflare.GroupRef) bool { return g.ID == groupID("Cache Purge") })
			f.provisionerOwn = []cloudflare.Policy{own}
		}, typed, setup.VerifyRefused, `missing "Cache Purge"`},
		{"a zone the account lacks", func(f *fakeCloudflare) {
			f.zones = slices.DeleteFunc(f.zones, func(z cloudflare.Zone) bool { return z.Name == zoneExampleName })
		}, typed, setup.VerifyRefused, `no zone named "example.com"`},
		{"Cloudflare down", func(f *fakeCloudflare) { f.refuse["verify"] = http.StatusServiceUnavailable }, typed,
			setup.VerifyUnreachable, "could not say"},
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
		if v.Outcome != tc.want || !strings.Contains(v.Detail, tc.says) {
			t.Errorf("%s: %s %q, want %s saying %q", tc.name, v.Outcome, v.Detail, tc.want, tc.says)
		}
		if strings.Contains(v.Detail, provisioner) || strings.Contains(v.Detail, "fake-other") {
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
