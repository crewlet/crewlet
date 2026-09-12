package setupapi_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
)

// A DISCONNECT NAMES WHAT IT LEAVES BEHIND, ON THE PATH THE DASHBOARD USES.
//
// The store is the company's, so a credential an operator may be sharing with
// another deployment is not something a Disconnect button removes. What the
// answer owes them is the list — and the list existed only on the FORCE path,
// which nothing presses: the ordinary disconnect returned
// `{key, removed, disconnecting, remove_seats, detail}` and named nothing at
// all. Measured: seven credentials survived a disconnect and not one of them
// was named.
//
// It has to be computed HERE, before the intent is recorded, because every
// name in it is derived from a `${VAR}` in the company document and both paths
// end with that block gone.
func TestAnOrdinaryDisconnectNamesTheCredentialsItLeaves(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	res := s.do(t, http.MethodPut, "/config", identityDoc,
		map[string]string{"X-Summary": "a provisioned company"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}

	// WITH A STATUS STORE, because the ordinary path records the intent on
	// the fleet row and refuses without one.
	s.withPass(t, &recordingPass{kind: integration.KindAtlassian})
	out := s.do(t, http.MethodDelete, "/setup/integrations/atlassian",
		`{"remove_seats": true}`, nil)
	if out.Code != http.StatusAccepted {
		t.Fatalf("disconnect = %d: %s", out.Code, out.Body)
	}
	body := decode(t, out)
	orphans, _ := body["orphaned_secrets"].([]any)

	// THE ORGANIZATION'S OWN KEY, which the old list would have named.
	if !slices.Contains(orphans, any("ORG_KEY")) {
		t.Errorf("orphaned_secrets = %v, want the organization key named", orphans)
	}
	// AND THE SEAT'S, which it never did. A pass mints a token under the
	// name that seat's own mcp_env points at, and Atlassian seals the
	// ADDRESS beside it because its products authenticate
	// base64(address:token) — so a disconnect orphans two values per agent
	// and named neither.
	for _, want := range []string{"SRE_ATLASSIAN", "SRE_EMAIL"} {
		if !slices.Contains(orphans, any(want)) {
			t.Errorf("orphaned_secrets = %v, want the seat's %s named", orphans, want)
		}
	}
}

// AND IT NAMES ONLY WHAT IS THERE.
//
// A list to act on has to be a list of things that exist: telling an operator
// to unset a variable nobody ever set is a step they cannot take, over an
// optional credential this company chose not to use.
func TestADisconnectNamesNoCredentialNobodySet(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	res := s.do(t, http.MethodPut, "/config", identityDoc,
		map[string]string{"X-Summary": "a provisioned company"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}

	s.withPass(t, &recordingPass{kind: integration.KindGitLab})
	out := s.do(t, http.MethodDelete, "/setup/integrations/gitlab", `{}`, nil)
	if out.Code != http.StatusAccepted {
		t.Fatalf("disconnect = %d: %s", out.Code, out.Body)
	}
	orphans, _ := decode(t, out)["orphaned_secrets"].([]any)

	// The routing token is optional and this company never set one.
	if slices.Contains(orphans, any("GITLAB_ROUTING_TOKEN")) {
		t.Errorf("orphaned_secrets = %v, naming a credential nobody stored", orphans)
	}
	// What it DID set is still named.
	if !slices.Contains(orphans, any("GL_SIGN")) {
		t.Errorf("orphaned_secrets = %v, want the signing secret named", orphans)
	}
}

// A SEAT WHOSE ACCOUNT WAS REMOVED STOPS READING AS SATISFIED, AND STOPS
// NAMING THE ACCOUNT.
//
// The roster reads a seat's credential slot out of the company document and
// resolves it. That is honest — provided something clears the store when the
// account goes. Nothing did: a disconnect that deleted every agent's service
// account left every agent's credential sealed and resolving, so the roster
// went on reporting the seat satisfied, next to the address of an account that
// no longer existed.
//
// This asserts the roster through the same resolution the screen uses, over a
// store the teardown has emptied for that seat.
func TestASeatWhoseCredentialWentWithItsAccountIsNotSatisfied(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	res := s.do(t, http.MethodPut, "/config", identityDoc,
		map[string]string{"X-Summary": "a provisioned company"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}
	// A provisioned seat: the pass sealed both slots.
	for name, value := range map[string]string{
		"SRE_ATLASSIAN": "atlassian-token",
		"SRE_EMAIL":     "crewlet-sre-lead@acme.invalid",
	} {
		if err := s.vault.Set(t.Context(), name, value, "op", "test", pinned); err != nil {
			t.Fatalf("seal %s: %v", name, err)
		}
	}
	if seat := seatRow(t, s, "jira", "sre-lead"); seat["satisfied"] != true {
		t.Fatalf("precondition: a provisioned seat reads as %v", seat["satisfied"])
	}

	// WHAT THE TEARDOWN NOW DOES: the accounts went, so their credentials
	// went with them.
	s.vault.forget("SRE_ATLASSIAN", "SRE_EMAIL")

	seat := seatRow(t, s, "jira", "sre-lead")
	if seat["satisfied"] == true {
		t.Error("the seat still reads as satisfied over an account that was removed")
	}
	// AND THE ADDRESS GOES WITH IT. The identity column is the account
	// Atlassian assigned, read back through the resolver — so a deleted
	// account's address is exactly what it kept showing.
	if detail, _ := seat["detail"].(string); strings.Contains(detail, "acme.invalid") {
		t.Errorf("detail = %q, still naming the removed account's address", detail)
	}
}

// seatRow reads one agent's roster entry off a tool's setup answer.
func seatRow(t *testing.T, s *surface, kind, handle string) map[string]any {
	t.Helper()
	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/"+kind, "", nil))
	rows, _ := state["seats"].([]any)
	for _, row := range rows {
		seat, _ := row.(map[string]any)
		if seat["handle"] == handle {
			return seat
		}
	}
	t.Fatalf("no roster row for %s on %s", handle, kind)
	return nil
}

// A SEAT IS NOT SATISFIED ON AN INTEGRATION THE COMPANY DOES NOT HAVE.
//
// The roster answered about the CREDENTIAL and nothing else, so a seat whose
// `${VAR}` resolved read as satisfied over a surface that had been
// disconnected out of the document entirely — with the detail naming the
// config path the value sits at, which is a fact about YAML rather than a
// state. Measured after a Datadog disconnect: the block gone, every agent
// still shown ready on it.
func TestASeatIsNotSatisfiedOnASurfaceTheCompanyDoesNotDeclare(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	res := s.do(t, http.MethodPut, "/config", identityDoc,
		map[string]string{"X-Summary": "a provisioned company"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}
	for name, value := range map[string]string{
		"SRE_ATLASSIAN": "atlassian-token",
		"SRE_EMAIL":     "crewlet-sre-lead@acme.invalid",
	} {
		if err := s.vault.Set(t.Context(), name, value, "op", "test", pinned); err != nil {
			t.Fatalf("seal %s: %v", name, err)
		}
	}
	if seat := seatRow(t, s, "jira", "sre-lead"); seat["satisfied"] != true {
		t.Fatalf("precondition: a provisioned seat reads as %v", seat["satisfied"])
	}

	// THE INTEGRATION GOES, and the seat's sealed credentials stay — which
	// is what a disconnect that keeps the accounts deliberately leaves.
	drop := s.do(t, http.MethodPatch, "/config", `{"integrations":{"jira":null}}`,
		map[string]string{
			"Content-Type": "application/merge-patch+json", "X-Summary": "disconnect jira",
		})
	if drop.Code != http.StatusCreated {
		t.Fatalf("drop = %d: %s", drop.Code, drop.Body)
	}

	seat := seatRow(t, s, "jira", "sre-lead")
	if seat["satisfied"] == true {
		t.Error("the seat reads as satisfied on an integration this company " +
			"no longer declares, so nothing reads what it holds")
	}
	detail, _ := seat["detail"].(string)
	if !strings.Contains(detail, "connected") {
		t.Errorf("detail = %q, want the sentence to be about the integration "+
			"rather than about a variable", detail)
	}
}

// A SEAT THE LOOP HAS A FINDING ABOUT IS NOT SATISFIED, whatever the store says.
//
// The roster reads the document and the sealed store, so `satisfied` means "a
// credential is sealed where this app looks for one" — a real fact and not the
// one a green row is read as. A key deleted at the third-party app leaves the
// pointer resolving perfectly while every call the agent makes is refused, and
// the roster went on reporting it ready. The loop is the only thing that has
// asked the vendor, so its answer is folded in here.
func TestASeatTheLoopFoundBrokenIsNotSatisfied(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	status, _ := s.withPass(t, &recordingPass{})
	res := s.do(t, http.MethodPut, "/config", identityDoc,
		map[string]string{"X-Summary": "a provisioned company"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}
	for name, value := range map[string]string{
		"SRE_ATLASSIAN": "atlassian-token",
		"SRE_EMAIL":     "crewlet-sre-lead@acme.invalid",
	} {
		if err := s.vault.Set(t.Context(), name, value, "op", "test", pinned); err != nil {
			t.Fatalf("seal %s: %v", name, err)
		}
	}
	if seat := seatRow(t, s, "jira", "sre-lead"); seat["satisfied"] != true {
		t.Fatalf("precondition: a provisioned seat reads as %v", seat["satisfied"])
	}

	// WHAT A PASS FOUND AT THE VENDOR: the account is there and holds no
	// usable credential. Nothing about the document or the store changed.
	mustSaveStatus(t, status, integration.State{
		Kind:   integration.KindJira,
		Report: integration.Report{Phase: integration.PhaseProvisioning, Actor: integration.ActorEngine},
		Findings: []integration.Finding{{
			Kind: integration.FindingIdentityMissing, Subject: "sre-lead",
			Detail: "sre-lead's account holds no application key",
		}},
	})

	seat := seatRow(t, s, "jira", "sre-lead")
	if seat["satisfied"] == true {
		t.Error("the seat reads as satisfied over a credential the loop found broken")
	}
	if detail, _ := seat["detail"].(string); !strings.Contains(detail, "no application key") {
		t.Errorf("detail = %q, want the loop's own sentence", detail)
	}
}

// AND AN ADVISORY LEAVES IT ALONE. Two kinds are phase-ready by definition —
// a permission wider than the role asked for, a registration this engine no
// longer manages but which still delivers — and reporting either as a broken
// agent would contradict the card's own tag.
func TestAnAdvisoryFindingLeavesASeatSatisfied(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	status, _ := s.withPass(t, &recordingPass{})
	res := s.do(t, http.MethodPut, "/config", identityDoc,
		map[string]string{"X-Summary": "a provisioned company"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}
	for name, value := range map[string]string{
		"SRE_ATLASSIAN": "atlassian-token",
		"SRE_EMAIL":     "crewlet-sre-lead@acme.invalid",
	} {
		if err := s.vault.Set(t.Context(), name, value, "op", "test", pinned); err != nil {
			t.Fatalf("seal %s: %v", name, err)
		}
	}
	mustSaveStatus(t, status, integration.State{
		Kind:   integration.KindJira,
		Report: integration.Report{Phase: integration.PhaseReady, Actor: integration.ActorAdmin},
		Findings: []integration.Finding{{
			Kind: integration.FindingGrantExcess, Subject: "sre-lead",
			Detail: "sre-lead holds more access than its role asks for",
		}},
	})

	if seat := seatRow(t, s, "jira", "sre-lead"); seat["satisfied"] != true {
		t.Errorf("an advisory un-satisfied a working seat: %v", seat["satisfied"])
	}
}

// mustSaveStatus seeds the fleet row a pass would have written.
func mustSaveStatus(t *testing.T, status *statusStore, state integration.State) {
	t.Helper()
	if err := status.SaveIntegration(t.Context(), state); err != nil {
		t.Fatalf("seed the status row: %v", err)
	}
}
