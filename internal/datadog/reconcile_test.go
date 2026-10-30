package datadog_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/integration"
)

func cfgWith() *config.Datadog {
	return &config.Datadog{
		Provisioning: &config.DatadogProvisioning{
			Site: "datadoghq.com", EmailDomain: "agents.test.invalid",
		},
	}
}

// orgOK is the verify call every pass starts with.
func orgOK(reg *region) {
	reg.handle["/api/v1/org"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"orgs":[{"name":"Acme","public_id":"p1"}]}`))
	}
}

// A CANCELLED PASS RAISES RATHER THAN REPORTING A CONVERGED COMPANY.
//
// The two are opposite claims to the reconcile loop: an error is a fault it
// retries, and an empty findings list is a statement that everything is fine.
// A node shutting down would otherwise record Datadog ready on its way out,
// and the next node to hold the duty trusts that for a full settled interval.
// [integrationtest] drives the same clause; this pins what makes it true,
// which that clause cannot see. The pass consults the context ITSELF, before
// anything else it could answer from — rather than relying on the credential
// probe below to fail because a transport happened to honour cancellation.
func TestACancelledPassRaisesRatherThanReportingHealth(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	noSeats(reg)
	held := &stored{}
	serveWebhook(reg, held)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := datadog.Reconcile(ctx, hookOptions(t, reg, base))
	if err == nil && len(res.Findings()) == 0 {
		t.Fatal("a cancelled pass reported a converged integration")
	}
	if err == nil {
		t.Fatalf("a cancelled pass answered with findings rather than a fault: %+v",
			res.Findings())
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancellation itself", err)
	}
	// AND IT ASKED DATADOG NOTHING. Not an efficiency: a pass that reaches
	// the network before it reads the context is one whose answer depends
	// on the transport it was built with, and the arm ABOVE that probe
	// returns integration.ErrNotConfigured — which the loop reads as
	// "forget this surface's status row", so a node draining against a
	// company mid-edit would delete the fleet's Datadog status on its way
	// out.
	if len(reg.calls) != 0 {
		t.Errorf("a cancelled pass made %v; it observed nothing and must say so "+
			"without asking", reg.calls)
	}
	// AND IT DOES NOT ACCUSE THE OPERATOR'S KEY. A cancellation says
	// nothing about the credential, and reporting one as refused — or even
	// as unverifiable — sends somebody to rotate a pair that works.
	if errors.Is(err, integration.ErrCredentialRejected) {
		t.Errorf("a cancelled pass was classified as a credential rejection: %v", err)
	}
	if strings.Contains(err.Error(), "integrations.datadog.provisioning") {
		t.Errorf("a cancelled pass sent somebody to look at the organization "+
			"credential pair, which it never asked about: %v", err)
	}
}

// A REFUSED ORGANIZATION CREDENTIAL IS A REJECTION, so the surface reads
// Failed and points at the operator rather than sitting in a retry.
func TestRefusedOrgCredentialsAreReportedAsARejection(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	reg.handle["/api/v1/org"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["Forbidden"]}`))
	}
	_, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Creds: pair,
	})
	if err == nil {
		t.Fatal("a refused credential was not reported")
	}
	if !strings.Contains(err.Error(), "was refused") {
		t.Errorf("err = %v", err)
	}
	if !errors.Is(err, integration.ErrCredentialRejected) {
		t.Errorf("a 403 was not classified as a rejection: %v", err)
	}
}

// AND ANYTHING ELSE IS NOT A REFUSAL. A 404, a 500 or a timeout says nothing
// about the key, and reporting one as refused sends an operator to rotate a
// credential that works. This is not hypothetical: the verify call named a
// route Datadog does not serve, and every pass reported the keys rejected.
func TestANonRefusalDoesNotAccuseTheCredential(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	reg.handle["/api/v1/org"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"errors":["upstream"]}`))
	}
	_, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Creds: pair,
	})
	if err == nil {
		t.Fatal("a 500 was not reported at all")
	}
	if !strings.Contains(err.Error(), "could not be verified") {
		t.Errorf("err = %v, want it to stop short of claiming a refusal", err)
	}
	if errors.Is(err, integration.ErrCredentialRejected) {
		t.Errorf("a 500 was classified as a credential rejection: %v", err)
	}
}

// THE COMPANY'S PASS NEVER ACTS ON AN AGENT'S CREDENTIAL. Each seat's key is
// the access sync's alone: it mints, seals, rotates and revokes it. A second
// writer here deleted keys it could not read and minted its own, revoking the
// key the sync had sealed, so this pass is held to the webhook and the
// advisory: over an organization whose agents hold keys it touches no key and
// no account.
func TestTheCompanyPassNeverTouchesAnAgentsKey(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"id":"u1","attributes":{"email":"crewlet-sre@agents.test.invalid","service_account":true}},
			{"id":"u2","attributes":{"email":"crewlet-dba@agents.test.invalid","service_account":true,"disabled":true}}
		]}`))
	}
	held := &stored{}
	serveWebhook(reg, held)
	opts := hookOptions(t, reg, base)
	opts.Managed = []string{"u1", "u2"}
	if _, err := datadog.Reconcile(context.Background(), opts); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, call := range reg.calls {
		if strings.Contains(call, "/application_keys") || strings.Contains(call, "/service_accounts") ||
			strings.Contains(call, "/roles/") ||
			(strings.Contains(call, "/api/v2/users/") && !strings.HasPrefix(call, http.MethodGet)) {
			t.Errorf("the company's pass acted on an agent's account or key: %s", call)
		}
	}
}
