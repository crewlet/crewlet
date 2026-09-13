package datadog_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
	// scheme this engine no longer derives, a DISABLED one from the same
	// scheme, and a PERSON.
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[
			{"id":"u1","attributes":{"email":"crewlet-sre@agents.test.invalid","service_account":true}},
			{"id":"u2","attributes":{"email":"agent-cs-old@agents.test.invalid","service_account":true}},
			{"id":"u4","attributes":{"email":"agent-cs-off@agents.test.invalid","service_account":true,"disabled":true}},
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
	// THE LIST IS THE SUBJECTS, and the sentence carries only the count —
	// so a card laying both out does not show a short list twice.
	if !slices.Contains(f.Subjects, "agent-cs-old@agents.test.invalid") {
		t.Errorf("the finding does not name the account to act on: %v", f.Subjects)
	}
	// NOT THE SEAT'S OWN, which is the working agent.
	if slices.Contains(f.Subjects, "crewlet-sre@agents.test.invalid") {
		t.Errorf("the finding names a seat's own account, so an operator is "+
			"told to clean up the identity of a working agent: %v", f.Subjects)
	}
	// AND NOT A PERSON. ListServiceAccounts already drops them, and this is
	// the clause that keeps it true here.
	if slices.Contains(f.Subjects, "jane@acme.example") {
		t.Errorf("the finding names a person's account: %v", f.Subjects)
	}
	// AND NOT A DISABLED ONE, which is already where the note is asking it
	// to get to. See [TestADisabledOrphanIsAlreadyWhereTheNoteAsks].
	if slices.Contains(f.Subjects, "agent-cs-off@agents.test.invalid") {
		t.Errorf("the finding names an account that is already disabled: %v",
			f.Subjects)
	}
	if strings.Contains(f.Detail, "@") {
		t.Errorf("the sentence splices an address into itself, so a card "+
			"rendering it beside the list shows it twice:\n%s", f.Detail)
	}
	// AND WHAT TO DO IS ITS OWN FIELD.
	if f.Remedy == "" {
		t.Error("the advisory says what is there and not what to do about it")
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

// A DISABLED ORPHAN IS ALREADY WHERE THE NOTE IS ASKING IT TO GET TO.
//
// The advisory's own remedy is "disable or delete the ones you do not want",
// so an account that is already disabled is asking an operator for work
// somebody has done. And mostly THIS ENGINE had done it: a `remove_seats`
// disconnect disables the accounts it removes, so the ordinary
// reconnect-with-a-smaller-roster cycle turned every correct teardown into a
// row on the card. That is where the 36 measured `agent-cs-…` accounts came
// from — every one of them inert, and the pile of them burying whatever else
// the card had to say.
//
// It also makes the instruction work. The filter looked only at the address,
// so an operator who read the note and disabled an account watched the finding
// come back unchanged on the next tick and learned that the card does not
// respond to what it asks for. Now either half of "disable or delete" clears
// it.
//
// The enabled one in the same organization still reports, which is the half
// that must not go with it: that account can still act.
func TestADisabledOrphanIsAlreadyWhereTheNoteAsks(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[
			{"id":"u1","attributes":{"email":"crewlet-sre@agents.test.invalid","service_account":true}},
			{"id":"u2","attributes":{"email":"agent-cs-gone@agents.test.invalid","service_account":true,"disabled":true}},
			{"id":"u3","attributes":{"email":"agent-cs-live@agents.test.invalid","service_account":true}}
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
	f, found := orphansFinding(t, res)
	if !found {
		t.Fatalf("findings = %v, none naming the account that can still act",
			res.Findings())
	}
	if want := []string{"agent-cs-live@agents.test.invalid"}; !slices.Equal(f.Subjects, want) {
		t.Errorf("subjects = %v, want exactly %v: a disabled account has "+
			"reached the end state this advisory asks for", f.Subjects, want)
	}
	// THE SENTENCE SAYS SO, because a count that silently means something
	// narrower than it reads is how an operator concludes the card is wrong
	// about their organization.
	if !strings.Contains(f.Detail, "1 enabled service account ") {
		t.Errorf("the detail does not say the count is of enabled accounts, "+
			"in the singular:\n%s", f.Detail)
	}
	if strings.Contains(f.Detail, "(s)") {
		t.Errorf("the count wears a parenthetical plural:\n%s", f.Detail)
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
	if !slices.Contains(f.Subjects, "crewlet-gone@acme.example") {
		t.Errorf("the finding does not name this engine's own leftover: %v",
			f.Subjects)
	}
	if slices.Contains(f.Subjects, "build-robot@acme.example") {
		t.Errorf("the finding names a service account somebody else made at a "+
			"domain this company owns: %v", f.Subjects)
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

// A KEY THIS ENGINE CANNOT READ IS REPLACED, NOT REPORTED AT AN OPERATOR.
//
// Datadog serves a key's value once, so a key on a seat's own service account
// with nothing sealed for the seat is a value NOBODY in the company holds: the
// agent authenticates from the ${VAR} and the ${VAR} is empty. The pass used
// to stop the whole surface on Action required and tell a person to "delete
// the key at Datadog and run this again" — an API call it is itself
// authenticated for, holding the very credentials it listed the key with.
// Measured on a live company: the Datadog card sat at Action required over one
// seat, indefinitely, with nothing retrying and nothing broken but this.
//
// The engine's own teardown already settles the question the refusal was
// asking: revokeAppKeys takes EVERY key on an agent's account, because "the
// account exists solely because this engine created it". Both cannot be right.
func TestAnUnreadableKeyIsReplacedRatherThanReported(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true}}]}`)
	}
	var deleted []string
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(
				`{"data":{"id":"k2","attributes":{"key":"fresh-value","name":"crewlet"}}}`))
			return
		}
		_, _ = w.Write([]byte(
			`{"data":[{"id":"k1","attributes":{"name":"crewlet"}}]}`))
	}
	reg.handle["/api/v2/service_accounts/u1/application_keys/k1"] = func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.Method == http.MethodDelete {
			deleted = append(deleted, "k1")
		}
		w.WriteHeader(http.StatusNoContent)
	}

	// THE SINK HOLDS NOTHING, which is the whole premise: the value is
	// gone and the key it belongs to cannot be recovered from anywhere.
	s := newSink()
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Seats) != 1 {
		t.Fatalf("seats = %+v", res.Seats)
	}
	if res.Seats[0].Err != nil {
		t.Fatalf("the seat was refused rather than repaired: %v", res.Seats[0].Err)
	}
	if !slices.Equal(deleted, []string{"k1"}) {
		t.Errorf("deleted %v, want the unreadable key gone before a new one "+
			"is minted: two keys where one is dead is the state this repairs",
			deleted)
	}
	if !res.Seats[0].KeyMinted {
		t.Error("no replacement was minted, so the seat still cannot act")
	}
	if got := s.held["SRE_DD_KEY"]; got != "fresh-value" {
		t.Errorf("sealed %q, want the replacement recorded", got)
	}
	// AND THE CARD IS CLEAN. The whole point is that nothing is left for a
	// person to do.
	for _, f := range res.Findings() {
		if f.Subject == "sre" {
			t.Errorf("the repaired seat still reports %q", f.Detail)
		}
	}
}

// AND A KEY THIS ENGINE DID NOT WRITE IS LEFT ALONE.
//
// The repair is narrower than the teardown's sweep, and deliberately: a
// teardown must leave NO live credential on a decommissioned account, so a key
// it cannot attribute is exactly the hazard it exists to remove. A repair only
// has to give this seat a working credential, and minting is ADDITIVE — so it
// can spare a key somebody else put there and still fix the seat, which the
// teardown has no way to do.
func TestARepairSparesAKeyThisEngineDidNotWrite(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true}}]}`)
	}
	var deleted []string
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(
				`{"data":{"id":"k9","attributes":{"key":"fresh-value","name":"crewlet"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[
			{"id":"k1","attributes":{"name":"crewlet"}},
			{"id":"k2","attributes":{"name":"terraform"}}
		]}`))
	}
	for _, id := range []string{"k1", "k2"} {
		reg.handle["/api/v2/service_accounts/u1/application_keys/"+id] = func(
			w http.ResponseWriter, r *http.Request,
		) {
			if r.Method == http.MethodDelete {
				deleted = append(deleted, id)
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}

	s := newSink()
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !slices.Equal(deleted, []string{"k1"}) {
		t.Errorf("deleted %v, want only the key this engine's own mint names",
			deleted)
	}
	if res.Seats[0].Err != nil || !res.Seats[0].KeyMinted {
		t.Errorf("the seat was not repaired: err=%v minted=%v",
			res.Seats[0].Err, res.Seats[0].KeyMinted)
	}
}

// A FAILURE THIS ENGINE CAUSED DOES NOT SEND SOMEBODY TO DATADOG.
//
// Every seat failure carried the console link once, on the reasoning that each
// is something a person settles there. An unreadable secret store is not: it
// is this engine's own fault, on this engine's own side, and a link to
// somebody else's user administration under it costs the trip and teaches an
// operator that the link means nothing.
func TestAStoreFailureLinksNowhereAtTheVendor(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true}}]}`)
	}
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(
		w http.ResponseWriter, _ *http.Request,
	) {
		_, _ = w.Write([]byte(`{"data":[{"id":"k1","attributes":{"name":"crewlet"}}]}`))
	}

	s := newSink()
	s.readErr = errors.New("seal: keyring unavailable")
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
		if f.ActionURL != "" {
			t.Errorf("a store failure this engine owns links to %q", f.ActionURL)
		}
	}
	if !found {
		t.Fatalf("findings = %v, none about the seat", res.Findings())
	}
}

// AND THE VENDOR'S LINK IS THE PAGE, NOT A GUESSED FILTER.
//
// It carried `?filter=disabled` on the theory that a re-enable starts from the
// disabled list — a parameter nothing here established Datadog honours, on a
// link that serves several failures having nothing to do with a disabled
// account. So it pointed at a filter that HIDES the account an operator came
// to look at, which costs exactly the trip the link was added to save.
func TestTheVendorLinkIsThePageItself(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true,
			"disabled":true,"title":""}}]}`)
	}

	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: newSink(),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, f := range res.Findings() {
		if f.Subject != "sre" {
			continue
		}
		if strings.Contains(f.ActionURL, "?") {
			t.Errorf("action_url = %q, want the user administration itself: a "+
				"query nothing has established the vendor honours can hide "+
				"the account somebody followed the link to find", f.ActionURL)
		}
	}
}
