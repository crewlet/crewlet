package cloudflare_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// A 400 THAT REFUSES THE TOKEN IS THE TOKEN'S: Cloudflare's codes, chained
// ones included, say so where its status cannot, and the reconcile classes it
// as a rejected credential rather than a fault to retry.
func TestARefusedTokenIsReadFromCloudflaresCodes(t *testing.T) {
	t.Parallel()
	f := newFakeCloudflare()
	f.envelope["verify"] = malformedToken
	_, err := newClient(t, f, provisioner).Verify(t.Context())
	api, ok := errors.AsType[*cloudflare.APIError](err)
	if !ok || !slices.Equal(api.Codes, []int{6003, 6111}) || !cloudflare.TokenRefused(err) {
		t.Fatalf("Verify = %v (codes %v), want the token refused by codes 6003 and 6111", err, api.Codes)
	}
	if !strings.Contains(api.Detail, "Invalid format for Authorization header") {
		t.Errorf("the refusal %q leaves out the chained reason", api.Detail)
	}

	w := convergedCloudflare(t, t)
	w.f.set(func(f *fakeCloudflare) { f.envelope["verify"] = malformedToken })
	if _, err := w.Reconcile(t.Context()); !errors.Is(err, integration.ErrCredentialRejected) {
		t.Errorf("a refused token in the pass = %v, want a rejected credential", err)
	}

	f2 := newFakeCloudflare()
	f2.envelope["zones"] = fakeEnvelope{status: http.StatusBadRequest,
		errors: []cloudflare.Message{{Code: 1001, Message: "Invalid page size"}}}
	if _, err := newClient(t, f2, provisioner).Zones(t.Context()); cloudflare.TokenRefused(err) {
		t.Errorf("a 400 about the request was read as a refused token: %v", err)
	}
}

// THE TEST BUTTON SAYS WHAT A REFUSED TOKEN IS, never that the address is
// wrong: Cloudflare's address is fixed, so its 4xx refuses what was sent.
func TestTheProbeNeverBlamesTheAddress(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		envelope fakeEnvelope
		says     string
	}{
		{"a header Cloudflare cannot read", malformedToken, "This is not a valid Cloudflare API token"},
		{"an invalid token", fakeEnvelope{status: http.StatusBadRequest,
			errors: []cloudflare.Message{{Code: 1000, Message: "Invalid API Token"}}}, "This is not a valid Cloudflare API token"},
		{"another refusal", fakeEnvelope{status: http.StatusNotFound,
			errors: []cloudflare.Message{{Code: 7003, Message: "Could not route to the account"}}}, "Could not route to the account"},
	} {
		f := newFakeCloudflare()
		f.envelope["verify"] = tc.envelope
		v, err := probe(t, f, typed)
		if err != nil || v.Outcome != setup.VerifyRefused || !strings.Contains(v.Detail, tc.says) {
			t.Errorf("%s: %+v, %v; want refused saying %q", tc.name, v, err, tc.says)
		}
		if strings.Contains(v.Detail, "address") {
			t.Errorf("%s: %q blames the address", tc.name, v.Detail)
		}
	}
}

// A SEAT'S TOKEN REFUSED FOR A MALFORMED PROVISIONER names the provisioner as
// what to replace.
func TestARefusedProvisionerIsWhatTheSeatsFindingAsksToReplace(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.CloudflareLevelRead)
	w.f.envelope["create"] = fakeEnvelope{status: http.StatusBadRequest,
		errors: []cloudflare.Message{{Code: 1000, Message: "Invalid API Token"}}}
	report := w.sync()
	if len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Remedy, "Replace the provisioner token") {
		t.Fatalf("findings = %+v, want the provisioner named as what to replace", report.Findings)
	}
}
