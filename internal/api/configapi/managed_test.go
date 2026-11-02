package configapi_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/iam"
)

// managedSurface is a node whose Tier A names writers as the company
// document's only writers, seeded with the org chart fixture so a revert has
// somewhere to go and a seat route has a seat to write.
func managedSurface(t *testing.T, writers ...string) (s *surface, first string) {
	t.Helper()
	s = newSurfaceWith(t, func(o *configapi.Options) {
		o.CompanyWriters = writers
	})
	first = s.seed(t, orgDoc)
	return s, first
}

// tierA is the principal the guard composes for the Tier A token id: its own
// credential, under its whole login, holding every grant and fresh for a
// step-up as every Tier A bearer is.
func tierA(id string) iam.Principal {
	return iam.Principal{
		ID: uuid.NewSHA1(auth.TokenNamespace, []byte(id)), Login: iam.TokenLogin(id),
		Kind: iam.KindMachine, Stage: iam.StageActive, Grants: iam.AllGrants,
		ReauthAt: time.Now().Add(time.Hour),
	}
}

// signedIn is a person at the dashboard holding every grant, through the
// session via names.
func signedIn(via string) iam.Principal {
	return iam.Principal{
		ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson, Seat: "cto",
		Stage: iam.StageActive, Grants: iam.AllGrants, Via: via,
		ReauthAt: time.Now().Add(time.Hour),
	}
}

// exchanged is the session a Tier A token's bearer was exchanged for: the
// token's own login, through a credential that is not the token.
func exchanged(id string) iam.Principal {
	p := tierA(id)
	p.Via = iam.SessionName("lineage-1")
	return p
}

// otherCredentials is every credential a managed document refuses when its
// writer is the token `operator`: another Tier A token, a person signed in,
// a person's machine token, and the writer's own token exchanged for a
// session — the credential, never the author, is what the list judges.
func otherCredentials() map[string]iam.Principal {
	return map[string]iam.Principal{
		"another Tier A token":            tierA("alice"),
		"a person's session":              signedIn(iam.SessionName("lineage-2")),
		"a person's machine token":        signedIn(iam.MachineTokenName("pat-1")),
		"the writer's token as a session": exchanged("operator"),
	}
}

func (s *surface) revisionCount(t *testing.T) int {
	t.Helper()
	rows, err := s.configs.List(t.Context(), 500, 0)
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	return len(rows)
}

// writePath is one way an HTTP caller changes the company document.
type writePath struct {
	method, path, body string
	headers            map[string]string
	// dryRun is a check, which stores nothing either way.
	dryRun bool
	// replaces is a write to a resource that already exists, answered 200
	// rather than the 201 of a write that creates a revision of the whole
	// document: the charter.
	replaces bool
}

// everyWritePath is every route on this surface that changes the document,
// and the dry run of each that has one. One entity kind stands for all four:
// they are one handler and one draft, mounted by one loop in Routes.
func everyWritePath(first string) map[string]writePath {
	summary := map[string]string{"X-Summary": "a change"}
	seat := `{"name":"CTO","handle":"cto","llm":"zulu","backstory":"edited"}`
	charter := `{"mission":"ship it","vision":"","policies":["One rule."]}`
	anyCharter := map[string]string{"If-Match": "*"}
	return map[string]writePath{
		"PUT /config":   {method: http.MethodPut, path: "/config", body: orgDoc, headers: summary},
		"PATCH /config": {method: http.MethodPatch, path: "/config", body: `{"mission":"ship it"}`, headers: summary},
		"PUT /config/roles/{id}": {
			method: http.MethodPut, path: "/config/roles/cto", body: seat, headers: summary,
		},
		"POST /config/revisions/{id}/revert": {
			method: http.MethodPost, path: "/config/revisions/" + first + "/revert",
		},
		"PUT /config?dry_run=true": {
			method: http.MethodPut, path: "/config?dry_run=true", body: orgDoc, dryRun: true,
		},
		"PATCH /config?dry_run=true": {
			method: http.MethodPatch, path: "/config?dry_run=true", body: `{"mission":"x"}`, dryRun: true,
		},
		"PUT /config/roles/{id}?dry_run=true": {
			method: http.MethodPut, path: "/config/roles/cto?dry_run=true", body: seat, dryRun: true,
		},
		"PUT /config/charter": {
			method: http.MethodPut, path: "/config/charter", body: charter,
			headers: anyCharter, replaces: true,
		},
		"PUT /config/charter?dry_run=true": {
			method: http.MethodPut, path: "/config/charter?dry_run=true", body: charter,
			headers: anyCharter, dryRun: true,
		},
	}
}

// A MANAGED DOCUMENT REFUSES EVERY OTHER CREDENTIAL ON EVERY PATH ONTO IT —
// with a 403 that names who manages it, the credential it refused and what to
// do — and stores nothing.
func TestAManagedDocumentRefusesEveryOtherCredential(t *testing.T) {
	t.Parallel()
	for who, caller := range otherCredentials() {
		for name, tc := range everyWritePath("placeholder") {
			t.Run(who+"/"+name, func(t *testing.T) {
				t.Parallel()
				refusedOnEveryPath(t, caller, name, tc)
			})
		}
	}
}

func refusedOnEveryPath(t *testing.T, caller iam.Principal, name string, tc writePath) {
	t.Helper()
	s, first := managedSurface(t, "operator")
	path := strings.Replace(tc.path, "placeholder", first, 1)
	before := s.revisionCount(t)

	res := doAs(t, s, caller, tc.method, path, tc.body, tc.headers)
	if res.Code != http.StatusForbidden {
		t.Fatalf("%s = %d, want 403: %s", name, res.Code, res.Body)
	}
	var body struct {
		Error     string   `json:"error"`
		Detail    string   `json:"detail"`
		Hint      string   `json:"hint"`
		ManagedBy []string `json:"managed_by"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error != "config_managed" || !slices.Equal(body.ManagedBy, []string{"operator"}) {
		t.Errorf("refusal = %+v, want config_managed naming operator", body)
	}
	credential := strconv.Quote(iam.ActorFor(caller).OperatorID)
	if !strings.Contains(body.Detail, credential) || !strings.Contains(body.Hint, "company_writers") {
		t.Errorf("the refusal does not name the credential %s or how to take the "+
			"document back: %+v", credential, body)
	}
	if got := s.revisionCount(t); got != before {
		t.Errorf("a refused write stored a revision: %d -> %d", before, got)
	}
}

// THE REFUSAL COMES FIRST: it depends on the credential alone, so a
// non-writer is answered it before the request is read — not told to fix a
// body, add a summary or name a revision that exists, when no fix would let
// the write through.
func TestAManagedRefusalComesBeforeTheRequestIsRead(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]writePath{
		"PUT /config with a body that is not a company": {
			method: http.MethodPut, path: "/config", body: "{not json", headers: map[string]string{"X-Summary": "x"},
		},
		"PATCH /config with no summary": {
			method: http.MethodPatch, path: "/config", body: `{"mission":"x"}`,
		},
		"PUT /config/roles/{id} with no summary": {
			method: http.MethodPut, path: "/config/roles/cto", body: `{"name":"CTO"}`,
		},
		"revert to a revision that does not exist": {
			method: http.MethodPost, path: "/config/revisions/no-such-revision/revert",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, _ := managedSurface(t, "operator")
			res := doAs(t, s, tierA("alice"), tc.method, tc.path, tc.body, tc.headers)
			if res.Code != http.StatusForbidden || !strings.Contains(res.Body.String(), `"config_managed"`) {
				t.Fatalf("%s as alice = %d %s, want 403 config_managed", name, res.Code, res.Body)
			}
			// THE WRITER IS ANSWERED ON ITS REQUEST, so the refusal above is
			// the credential's and not the request's.
			if res := doAs(t, s, tierA("operator"), tc.method, tc.path, tc.body, tc.headers); res.Code == http.StatusForbidden {
				t.Errorf("%s as the writer = 403: %s", name, res.Body)
			}
		})
	}
}

// AND THE LISTED CREDENTIAL WRITES ON EVERY ONE OF THEM, while a deployment
// that lists nobody lets every credential write exactly as before.
func TestAListedWriterAndAnUnmanagedDocumentWrite(t *testing.T) {
	t.Parallel()
	for _, posture := range []struct {
		name    string
		writers []string
		caller  iam.Principal
	}{
		{"the listed writer", []string{"operator"}, tierA("operator")},
		{"any token when nobody is listed", nil, tierA("alice")},
		{"a person when nobody is listed", nil, signedIn(iam.SessionName("lineage-2"))},
	} {
		for name, tc := range everyWritePath("placeholder") {
			t.Run(posture.name+"/"+name, func(t *testing.T) {
				t.Parallel()
				s, first := managedSurface(t, posture.writers...)
				path := strings.Replace(tc.path, "placeholder", first, 1)
				before := s.revisionCount(t)

				res := doAs(t, s, posture.caller, tc.method, path, tc.body, tc.headers)
				want := http.StatusCreated
				if tc.dryRun || tc.replaces {
					want = http.StatusOK
				}
				if res.Code != want {
					t.Fatalf("%s as %s = %d, want %d: %s", name, posture.name, res.Code, want, res.Body)
				}
				grew := s.revisionCount(t) - before
				if (tc.dryRun && grew != 0) || (!tc.dryRun && grew != 1) {
					t.Errorf("%s stored %d revisions", name, grew)
				}
			})
		}
	}
}

// A RELOAD IS NOT A CHANGE TO THE COMPANY, and a managed document keeps it
// open to every credential the authority table admits to it: it re-publishes
// the active revision's own bytes, and it is how a credential a person rotated
// — a leaked key, now — reaches the running seats when the managing system
// cannot know it has to.
func TestAReloadOfAManagedDocumentIsOpenToEveryCredential(t *testing.T) {
	t.Parallel()
	for who, caller := range otherCredentials() {
		t.Run(who, func(t *testing.T) {
			t.Parallel()
			s, first := managedSurface(t, "operator")
			before := s.activeDocument(t)
			res := doAs(t, s, caller, http.MethodPost, "/config/reload", "", nil)
			if res.Code != http.StatusCreated {
				t.Fatalf("POST /config/reload = %d: %s", res.Code, res.Body)
			}
			if after := s.activeDocument(t); after != before {
				t.Errorf("a reload changed the document:\n%s\n%s", before, after)
			}
			by := iam.ActorFor(caller)
			active, _, err := s.active(t.Context())
			if err != nil || active.ID == first || active.CreatedBy != by.Name ||
				active.OperatorID != by.OperatorID {
				t.Errorf("the reload is not a new revision by %+v: %+v, %v", by, active, err)
			}
		})
	}
}

// A LEAD IS REFUSED TOO, INSIDE THEIR OWN SUBTREE. The org chart's routes
// admit a unit's lead on what their write changes, and a managed document
// answers that edit exactly as it answers everybody else's: the managing
// system's next reconcile would overwrite it all the same.
func TestALeadIsRefusedAManagedDocumentInsideTheirOwnSubtree(t *testing.T) {
	t.Parallel()
	edit := `{"name":"SRE","handle":"sre","llm":"zulu","goal":"edited"}`
	for _, tc := range []struct {
		name    string
		writers []string
		want    int
	}{
		{"managed", []string{"operator"}, http.StatusForbidden},
		{"edited by people", nil, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSurfaceWith(t, func(o *configapi.Options) { o.CompanyWriters = tc.writers })
			s.seed(t, leadDoc)
			res := doAs(t, s, platformLead(), http.MethodPut, "/config/roles/sre",
				edit, map[string]string{"X-Summary": "a lead's edit"})
			if res.Code != tc.want {
				t.Fatalf("a lead's edit of their own report = %d, want %d: %s",
					res.Code, tc.want, res.Body)
			}
			if tc.want == http.StatusForbidden && !strings.Contains(res.Body.String(), `"config_managed"`) {
				t.Errorf("the lead was refused for something else: %s", res.Body)
			}
		})
	}
}

// THE PROGRAMMATIC WRITES ARE THE SAME RULE: /setup reaches the document
// through Apply and ApplyEntity as the operator who made the request, and is
// refused alike — while the engine's own writes, which present no credential,
// are not judged by a list of credentials.
func TestTheProgrammaticWritesAreJudgedByTheirAuthor(t *testing.T) {
	t.Parallel()
	seat := []byte(`{"name":"CTO","handle":"cto","llm":"zulu","backstory":"edited"}`)
	writer := iam.TokenLogin("operator")
	for _, tc := range []struct {
		by      iam.Actor
		refused bool
	}{
		{iam.ActorFor(tierA("alice")), true},
		{iam.ActorFor(signedIn(iam.SessionName("lineage-2"))), true},
		// THE CREDENTIAL, NEVER THE AUTHOR: the writer's own name through
		// another credential is another credential.
		{iam.Actor{Name: writer, Kind: iam.ActorOperator,
			OperatorID: iam.SessionName("lineage-1")}, true},
		{iam.ActorFor(tierA("operator")), false},
		{iam.Actor{Name: "node-a", Kind: iam.ActorSystem}, false},
	} {
		s, _ := managedSurface(t, "operator")
		_, applyErr := s.svc.Apply(t.Context(), configapi.ApplyRequest{
			Patch: []byte(`{"mission":"x"}`), Summary: "s", By: tc.by,
		})
		_, entityErr := s.svc.ApplyEntity(t.Context(), configapi.ApplyEntityRequest{
			Kind: "roles", ID: "cto", Body: seat, Summary: "s", By: tc.by,
		})
		for what, err := range map[string]error{"Apply": applyErr, "ApplyEntity": entityErr} {
			var managed *configapi.ManagedError
			if got := errors.As(err, &managed); got != tc.refused {
				t.Errorf("%s as %+v: refused=%v (err %v), want %v", what, tc.by, got, err, tc.refused)
			}
		}
		if got := s.svc.Authorize(tc.by) != nil; got != tc.refused {
			t.Errorf("Authorize(%+v) refused=%v, want %v", tc.by, got, tc.refused)
		}
	}
}
