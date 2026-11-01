package datadog_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/integration"
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
// A live organization accumulates them: a seat removed or given another handle
// (a different handle is a different seat, ADR-0013), or an account made by
// hand at this engine's `.invalid` domain. They are absent from the plan by
// construction, so no seat's result mentioned them and the card read Ready
// over an organization full of them.
//
// REPORTED, NEVER TOUCHED. An account is a colleague at Datadog with history
// attached, so removing one because a handle changed is not a decision a timer
// makes — which is also why this is an ADVISORY: nothing is broken, and
// nothing this engine runs will ever change it.
func TestServiceAccountsNoSeatClaimsAreReported(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)

	// The organization holds the seat's own account, one made by hand at
	// the `.invalid` domain under a prefix this engine does not derive, a
	// DISABLED one like it, and a PERSON.
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[
			{"id":"u1","attributes":{"email":"crewlet-sre@agents.test.invalid","service_account":true}},
			{"id":"u2","attributes":{"email":"agent-cs-old@agents.test.invalid","service_account":true}},
			{"id":"u4","attributes":{"email":"agent-cs-off@agents.test.invalid","service_account":true,"disabled":true}},
			{"id":"u3","attributes":{"email":"jane@acme.example","service_account":false}}
		]}`)
	}

	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Creds: pair,
		Managed: []string{"crewlet-sre@agents.test.invalid"},
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

	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Creds: pair,
		Managed: []string{"crewlet-sre@agents.test.invalid"},
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

	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Creds: pair,
		Managed: []string{"crewlet-sre@agents.test.invalid"},
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

	cfg := &config.Datadog{Provisioning: &config.DatadogProvisioning{
		Site: "datadoghq.com", EmailDomain: "acme.example",
	}}
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfg, Creds: pair,
		Managed: []string{"crewlet-sre@acme.example"},
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

// AND THE VENDOR'S LINK IS THE PAGE, NOT A GUESSED FILTER.
//
// It carried `?filter=disabled` on the theory that a re-enable starts from the
// disabled list — a parameter nothing here established Datadog honours, on a
// link that serves several failures having nothing to do with a disabled
// account. So it pointed at a filter that HIDES the account an operator came
// to look at, which costs exactly the trip the link was added to save.
func TestTheVendorLinkIsThePageItself(t *testing.T) {
	t.Parallel()
	link := newRegion(t).client(t).UsersURL()
	if strings.Contains(link, "?") || !strings.HasSuffix(link, "/organization-settings/users") {
		t.Errorf("users link = %q, want the user administration itself: a query "+
			"nothing has established the vendor honours can hide the account "+
			"somebody followed the link to find", link)
	}
}

// AN ACCOUNT THE ACCESS SYNC MANAGES IS NEVER AN ORPHAN. This pass manages no
// agent's account, so judged against nothing it called every agent's live
// account an orphan; what the ledger and the document name is spared.
func TestAccountsTheAccessSyncManagesAreNotOrphans(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[
			{"id":"u1","attributes":{"email":"crewlet-sre@agents.test.invalid","service_account":true}},
			{"id":"u2","attributes":{"email":"crewlet-dba@agents.test.invalid","service_account":true}},
			{"id":"u3","attributes":{"email":"crewlet-gone@agents.test.invalid","service_account":true}}
		]}`)
	}
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Creds: pair,
		// One by the ledger's id, one by a document seat's address.
		Managed: []string{"u1", "Crewlet-DBA@agents.test.invalid"},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	f, found := orphansFinding(t, res)
	if !found || !slices.Equal(f.Subjects, []string{"crewlet-gone@agents.test.invalid"}) {
		t.Errorf("orphans = %+v, want only the account nothing manages", f.Subjects)
	}
}
