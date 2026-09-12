package datadog_test

import (
	"context"
	"encoding/json"
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

// A TEARDOWN OVER AN ALREADY-DISABLED ACCOUNT STILL MARKS IT.
//
// Both the marker and the disable used to sit behind `if !account.Disabled`,
// and the seat was reported removed either way — so a teardown that met an
// account already off wrote no marker and swore the work was done. There is
// no way back from that: every later connect takes the unmarked-disabled arm
// and refuses for ever, because the marker can now never appear.
//
// Reached by an ordinary disconnect twice, by a disconnect after somebody
// disabled the account by hand, and by a teardown retried after a partial
// failure. Measured: crewlet-sre-lead@agents.crewlet.invalid, disabled, title
// empty, the card at degraded/admin after six attempts.
func TestATeardownMarksAnAccountThatIsAlreadyDisabled(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)

	account := struct {
		disabled bool
		title    string
	}{disabled: true}
	var disables int
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true,
			"disabled":%t,"title":%q}}]}`, account.disabled, account.title)
	}
	reg.handle["/api/v2/users/u1"] = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			disables++
			account.disabled = true
		case http.MethodPatch:
			var body struct {
				Data struct{ Attributes map[string]any } `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if title, set := body.Data.Attributes["title"].(string); set {
				account.title = title
			}
		}
		_, _ = w.Write([]byte(`{"data":{"id":"u1"}}`))
	}
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(
		w http.ResponseWriter, _ *http.Request,
	) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}

	removed, err := datadog.Teardown(context.Background(), datadog.TeardownOptions{
		Client: reg.client(t), Config: cfgWith(), Creds: pair,
		Plan: planWith("sre"), RemoveSeats: true,
	})
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if account.title != datadog.DisconnectedTitle {
		t.Errorf("title = %q over an account that was already disabled: "+
			"nothing records that this engine decommissioned it, so every "+
			"later connect refuses for ever", account.title)
	}
	// AND IT IS NOT DISABLED AGAIN, because it already is: the request
	// would change nothing and cost one round trip per seat per retry.
	if disables != 0 {
		t.Errorf("%d disable(s) sent to an account that was already off", disables)
	}
	if len(removed.Accounts) != 1 {
		t.Errorf("removed = %+v, want the seat reported once it is marked "+
			"and off", removed.Accounts)
	}
}

// AND A CONNECT AFTER THAT GETS THE ACCOUNT BACK.
//
// The whole point of the marker: the cycle this leaves behind has to be one
// the operator can complete from the product, rather than one that needs
// somebody in Datadog's own UI.
func TestAConnectAfterADoubleDisconnectReEnablesTheAccount(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)

	account := struct {
		disabled bool
		title    string
	}{disabled: true, title: datadog.DisconnectedTitle}
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true,
			"disabled":%t,"title":%q}}]}`, account.disabled, account.title)
	}
	reg.handle["/api/v2/users/u1"] = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			var body struct {
				Data struct{ Attributes map[string]any } `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if off, set := body.Data.Attributes["disabled"].(bool); set {
				account.disabled = off
			}
			if title, set := body.Data.Attributes["title"].(string); set {
				account.title = title
			}
		}
		_, _ = w.Write([]byte(`{"data":{"id":"u1"}}`))
	}
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(
				`{"data":{"id":"k1","attributes":{"key":"fresh","name":"crewlet"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}

	s := newSink()
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if account.disabled {
		t.Error("the connect left the account disabled, so the cycle still " +
			"ends in Datadog's own UI or another dead account")
	}
	if account.title != "" {
		t.Errorf("title = %q after the connect: a live account still marked "+
			"disconnected would be re-enabled again on every pass", account.title)
	}
	for _, seat := range res.Seats {
		if seat.Err != nil {
			t.Errorf("%s: %v", seat.Handle, seat.Err)
		}
	}
}

// THE REFUSAL HANDS THE OPERATOR SOMEWHERE TO GO.
//
// "re-enable it at Datadog if that was not deliberate" is the right decision
// and was, on its own, an instruction with nothing behind it: the finding
// carried a kind, a subject and a sentence, and the screen had no re-enable
// affordance — so somebody reading it had to find the account themselves,
// over an engine that holds the credentials and is declining to use them on
// purpose.
func TestASeatRefusalNamesWhereToActOnIt(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	// AN ACCOUNT SOMEBODY ELSE DISABLED: off, and carrying no marker of
	// this engine's.
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true,
			"disabled":true,"title":""}}]}`)
	}

	s := newSink()
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var found bool
	for _, f := range res.Findings() {
		if f.Subject != "sre" {
			continue
		}
		found = true
		if f.ActionURL == "" {
			t.Errorf("the refusal says %q and offers nowhere to do it", f.Detail)
		}
		// THE CONSOLE, not the API host: they differ by a subdomain on
		// every region, and a link to the second opens JSON.
		if !strings.HasPrefix(f.ActionURL, "https://app.") {
			t.Errorf("action_url = %q, which is not the Datadog console", f.ActionURL)
		}
	}
	if !found {
		t.Fatalf("findings = %v, none about the disabled seat", res.Findings())
	}
}
