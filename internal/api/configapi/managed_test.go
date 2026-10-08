package configapi_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/store"
)

// managedSurface is a node whose Tier A names `operator` as the company
// document's only writer, seeded with the fixture company and one later
// revision, so a revert has somewhere to go.
func managedSurface(t *testing.T, writers ...string) (s *surface, first string) {
	t.Helper()
	s = newSurfaceWith(t, func(o *configapi.Options) {
		o.Bootstrap.API.Auth.CompanyWriters = writers
	})
	first = s.seed(t, companyDoc, nil)
	return s, first
}

// as sends a request carrying the operator id the guard would have attached.
func (s *surface) as(t *testing.T, operator, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req = req.WithContext(auth.WithOperator(req.Context(), operator))
	res := httptest.NewRecorder()
	s.mux.ServeHTTP(res, req)
	return res
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
}

// everyWritePath is every route on this surface that changes the document,
// and the dry run of each that has one. One entity kind stands for all four:
// they are one handler and one draft, mounted by one loop in Routes.
func everyWritePath(first string) map[string]writePath {
	summary := map[string]string{"X-Summary": "a change"}
	seat := `{"name":"CTO","handle":"cto","llm":"zulu","backstory":"edited"}`
	return map[string]writePath{
		"PUT /config":   {method: http.MethodPut, path: "/config", body: companyDoc, headers: summary},
		"PATCH /config": {method: http.MethodPatch, path: "/config", body: `{"mission":"ship it"}`, headers: summary},
		"PUT /config/roles/{id}": {
			method: http.MethodPut, path: "/config/roles/cto", body: seat, headers: summary,
		},
		"POST /config/revisions/{id}/revert": {
			method: http.MethodPost, path: "/config/revisions/" + first + "/revert",
		},
		"PUT /config?dry_run=true": {
			method: http.MethodPut, path: "/config?dry_run=true", body: companyDoc, dryRun: true,
		},
		"PATCH /config?dry_run=true": {
			method: http.MethodPatch, path: "/config?dry_run=true", body: `{"mission":"x"}`, dryRun: true,
		},
		"PUT /config/roles/{id}?dry_run=true": {
			method: http.MethodPut, path: "/config/roles/cto?dry_run=true", body: seat, dryRun: true,
		},
	}
}

// A MANAGED DOCUMENT REFUSES EVERY OTHER CREDENTIAL ON EVERY PATH ONTO IT —
// with a 403 that names who manages it and what to do — and stores nothing.
func TestAManagedDocumentRefusesEveryOtherCredential(t *testing.T) {
	t.Parallel()
	for name, tc := range everyWritePath("placeholder") {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, first := managedSurface(t, "operator")
			path := strings.Replace(tc.path, "placeholder", first, 1)
			before := s.revisionCount(t)

			res := s.as(t, "alice", tc.method, path, tc.body, tc.headers)
			if res.Code != http.StatusForbidden {
				t.Fatalf("%s as alice = %d, want 403: %s", name, res.Code, res.Body)
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
			if !strings.Contains(body.Detail, `"alice"`) || !strings.Contains(body.Hint, "company_writers") {
				t.Errorf("the refusal does not say whose credential or how to take the document back: %+v", body)
			}
			if got := s.revisionCount(t); got != before {
				t.Errorf("a refused write stored a revision: %d -> %d", before, got)
			}
		})
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
			res := s.as(t, "alice", tc.method, tc.path, tc.body, tc.headers)
			if res.Code != http.StatusForbidden || !strings.Contains(res.Body.String(), `"config_managed"`) {
				t.Fatalf("%s as alice = %d %s, want 403 config_managed", name, res.Code, res.Body)
			}
			// THE WRITER IS ANSWERED ON ITS REQUEST, so the refusal above is
			// the credential's and not the request's.
			if res := s.as(t, "operator", tc.method, tc.path, tc.body, tc.headers); res.Code == http.StatusForbidden {
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
		name     string
		writers  []string
		operator string
	}{
		{"the listed writer", []string{"operator"}, "operator"},
		{"any token when nobody is listed", nil, "alice"},
	} {
		for name, tc := range everyWritePath("placeholder") {
			t.Run(posture.name+"/"+name, func(t *testing.T) {
				t.Parallel()
				s, first := managedSurface(t, posture.writers...)
				path := strings.Replace(tc.path, "placeholder", first, 1)
				before := s.revisionCount(t)

				res := s.as(t, posture.operator, tc.method, path, tc.body, tc.headers)
				want := http.StatusCreated
				if tc.dryRun {
					want = http.StatusOK
				}
				if res.Code != want {
					t.Fatalf("%s as %s = %d, want %d: %s", name, posture.operator, res.Code, want, res.Body)
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
// open to every credential: it re-publishes the active revision's own bytes,
// and it is how a credential a person rotated — a leaked key, now — reaches
// the running seats when the managing system cannot know it has to.
func TestAReloadOfAManagedDocumentIsOpenToEveryCredential(t *testing.T) {
	t.Parallel()
	s, first := managedSurface(t, "operator")
	before := s.activeDocument(t)
	res := s.as(t, "alice", http.MethodPost, "/config/reload", "", nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("POST /config/reload as alice = %d: %s", res.Code, res.Body)
	}
	if after := s.activeDocument(t); after != before {
		t.Errorf("a reload changed the document:\n%s\n%s", before, after)
	}
	active, _, err := s.configs.Active(t.Context())
	if err != nil || active.ID == first || active.CreatedBy != "alice" {
		t.Errorf("the reload is not a new revision by alice: %+v, %v", active, err)
	}
}

// THE PROGRAMMATIC WRITES ARE THE SAME RULE: /setup reaches the document
// through Apply and ApplyEntity as the operator who made the request, and is
// refused alike — while the engine's own writes, which present no credential,
// are not judged by a list of credentials.
func TestTheProgrammaticWritesAreJudgedByTheirAuthor(t *testing.T) {
	t.Parallel()
	seat := []byte(`{"name":"CTO","handle":"cto","llm":"zulu","backstory":"edited"}`)
	for _, tc := range []struct {
		author  store.Author
		refused bool
	}{
		{store.Author{Name: "alice", Kind: store.AuthorOperator}, true},
		{store.Author{Name: "operator", Kind: store.AuthorOperator}, false},
		{store.Author{Name: "reconcile loop", Kind: store.AuthorNode}, false},
	} {
		s, _ := managedSurface(t, "operator")
		_, applyErr := s.svc.Apply(t.Context(), configapi.ApplyRequest{
			Patch: []byte(`{"mission":"x"}`), Summary: "s", Author: tc.author,
		})
		_, entityErr := s.svc.ApplyEntity(t.Context(), configapi.ApplyEntityRequest{
			Kind: "roles", ID: "cto", Body: seat, Summary: "s", Author: tc.author,
		})
		for what, err := range map[string]error{"Apply": applyErr, "ApplyEntity": entityErr} {
			var managed *configapi.ManagedError
			if got := errors.As(err, &managed); got != tc.refused {
				t.Errorf("%s as %+v: refused=%v (err %v), want %v", what, tc.author, got, err, tc.refused)
			}
		}
		if got := s.svc.Authorize(tc.author) != nil; got != tc.refused {
			t.Errorf("Authorize(%+v) refused=%v, want %v", tc.author, got, tc.refused)
		}
	}
}
