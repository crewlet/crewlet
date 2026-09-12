package datadog_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
)

// orphansFinding pulls the one advisory this file is about off a result.
func orphansFinding(t *testing.T, res *datadog.Result) (integration.Finding, bool) {
	t.Helper()
	for _, f := range res.Findings() {
		if f.Kind == integration.FindingRegistrationOrphaned {
			return f, true
		}
	}
	return integration.Finding{}, false
}

// ACCOUNTS THIS ENGINE MADE AND NO LONGER MANAGES ARE REPORTED.
//
// A live organization accumulates them: a seat renamed, a handle changed, an
// older naming scheme. Measured on one — 36 disabled accounts under
// `agent-cs-…@agents.crewlet.invalid`, matching nothing a current pass would
// ask for. They are absent from the plan by construction, so no seat's result
// mentioned them and the card read Ready over an organization full of them.
//
// REPORTED, NEVER TOUCHED. An account is a colleague at Datadog with history
// attached, so removing one because a handle changed is not a decision a timer
// makes — which is also why this is an ADVISORY: nothing is broken, and
// nothing this engine runs will ever change it.
func TestServiceAccountsNoSeatClaimsAreReported(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)

	// The organization holds the seat's own account, one from a naming
	// scheme this engine no longer derives, and a PERSON.
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[
			{"id":"u1","attributes":{"email":"crewlet-sre@agents.test.invalid","service_account":true}},
			{"id":"u2","attributes":{"email":"agent-cs-old@agents.test.invalid","service_account":true,"disabled":true}},
			{"id":"u3","attributes":{"email":"jane@acme.example","service_account":false}}
		]}`)
	}
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(
				`{"data":{"id":"k1","attributes":{"key":"fresh","name":"crewlet"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"k1","attributes":{"name":"crewlet"}}]}`))
	}

	s := newSink()
	s.held["SRE_DD_KEY"] = "already-held"
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	f, found := orphansFinding(t, res)
	if !found {
		t.Fatalf("findings = %v, none of them naming the accounts no seat "+
			"claims: 36 of them read as a healthy organization", res.Findings())
	}
	if !strings.Contains(f.Detail, "agent-cs-old@agents.test.invalid") {
		t.Errorf("the finding does not name the account to act on:\n%s", f.Detail)
	}
	// NOT THE SEAT'S OWN, which is the working agent.
	if strings.Contains(f.Detail, "crewlet-sre@agents.test.invalid") {
		t.Errorf("the finding names a seat's own account, so an operator is "+
			"told to clean up the identity of a working agent:\n%s", f.Detail)
	}
	// AND NOT A PERSON. ListServiceAccounts already drops them, and this is
	// the clause that keeps it true here.
	if strings.Contains(f.Detail, "jane@acme.example") {
		t.Errorf("the finding names a person's account:\n%s", f.Detail)
	}
	// AN ADVISORY, so the surface is not held out of Ready by it.
	if phase, _ := f.Kind.Verdict(); phase != integration.PhaseReady {
		t.Errorf("the advisory reports phase %s: nothing is broken and nothing "+
			"this engine runs will change it", phase)
	}
}

// AND A CONVERGED ORGANIZATION REPORTS NOTHING.
//
// The clause that keeps the advisory from being permanent noise on every
// healthy company: every account at the domain belongs to a seat.
func TestAConvergedOrganizationReportsNoOrphans(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[
			{"id":"u1","attributes":{"email":"crewlet-sre@agents.test.invalid","service_account":true}}
		]}`)
	}
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(
		w http.ResponseWriter, _ *http.Request,
	) {
		_, _ = w.Write([]byte(`{"data":[{"id":"k1","attributes":{"name":"crewlet"}}]}`))
	}

	s := newSink()
	s.held["SRE_DD_KEY"] = "already-held"
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if f, found := orphansFinding(t, res); found {
		t.Errorf("a converged organization reported %q", f.Detail)
	}
}

// A COMPANY'S OWN DOMAIN IS NOT ENOUGH ON ITS OWN.
//
// `email_domain` may be a real domain the company owns, where its own people
// have addresses — and Datadog's user filter is a free-text substring match,
// so a listing under such a domain carries them. A `.invalid` domain is
// conclusive, because RFC 2606 reserves it precisely so nothing can deliver
// there: no person has a mailbox at one. Anywhere else the `crewlet-` prefix
// is the marker, and an address without it is somebody's.
func TestARealDomainNeedsTheCrewletPrefix(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[
			{"id":"u1","attributes":{"email":"crewlet-sre@acme.example","service_account":true}},
			{"id":"u2","attributes":{"email":"crewlet-gone@acme.example","service_account":true}},
			{"id":"u3","attributes":{"email":"build-robot@acme.example","service_account":true}}
		]}`)
	}
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(
		w http.ResponseWriter, _ *http.Request,
	) {
		_, _ = w.Write([]byte(`{"data":[{"id":"k1","attributes":{"name":"crewlet"}}]}`))
	}

	cfg := &config.Datadog{Provisioning: &config.DatadogProvisioning{
		Site: "datadoghq.com", EmailDomain: "acme.example",
	}}
	plan := &provision.Plan{}
	plan.Add(provision.Seat{
		Handle: "sre", Role: "SRE", TokenVar: "SRE_DD_KEY",
		Email: "crewlet-sre@acme.example",
	})

	s := newSink()
	s.held["SRE_DD_KEY"] = "already-held"
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfg, Plan: plan, Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f, found := orphansFinding(t, res)
	if !found {
		t.Fatalf("findings = %v, none naming the orphan", res.Findings())
	}
	if !strings.Contains(f.Detail, "crewlet-gone@acme.example") {
		t.Errorf("the finding does not name this engine's own leftover:\n%s", f.Detail)
	}
	if strings.Contains(f.Detail, "build-robot@acme.example") {
		t.Errorf("the finding names a service account somebody else made at a "+
			"domain this company owns:\n%s", f.Detail)
	}
}
