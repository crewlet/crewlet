package jira_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassianhost"
	"github.com/crewlet/crewlet/internal/jira"
)

// roleSite is a fake Jira Cloud site answering the project-role routes.
type roleSite struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []string
	bodies []map[string][]string
	answer map[string]struct {
		status int
		body   string
	}
}

func newRoleSite(t *testing.T) *roleSite {
	t.Helper()
	s := &roleSite{answer: map[string]struct {
		status int
		body   string
	}{}}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string][]string
		_ = json.Unmarshal(raw, &body)
		key := r.Method + " " + r.URL.Path
		s.mu.Lock()
		s.calls = append(s.calls, key+"?"+r.URL.RawQuery)
		s.bodies = append(s.bodies, body)
		a, ok := s.answer[key]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(a.body))
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *roleSite) on(method, path string, status int, body string) {
	s.answer[method+" "+path] = struct {
		status int
		body   string
	}{status, body}
}

func (s *roleSite) client(t *testing.T) *jira.Client {
	t.Helper()
	c, err := jira.NewClient(jira.ClientOptions{
		URL: s.server.URL, Email: "admin@example.com", Token: "fake-token",
		Declared: atlassianhost.Cloud,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTheSitesProjectRolesAreReadLiveAndTheAdministratorsAreKnown(t *testing.T) {
	t.Parallel()
	site := newRoleSite(t)
	site.on(http.MethodGet, "/rest/api/3/role", http.StatusOK, `[
		{"id": 10002, "name": "Administrators", "description": "Admins"},
		{"id": 10101, "name": "Developers"},
		{"id": 10102, "name": "Viewers"},
		{"id": 10200, "name": "Administrator", "admin": true}
	]`)
	roles, err := site.client(t).ProjectRoles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 4 {
		t.Fatalf("roles = %+v", roles)
	}
	admin := map[int64]bool{10002: true, 10101: false, 10102: false, 10200: true}
	for _, role := range roles {
		if role.Administration() != admin[role.ID] {
			t.Errorf("%s Administration() = %v", role.Name, role.Administration())
		}
	}
	if role, ok := jira.ProjectRoleByID(roles, 10101); !ok || role.Name != "Developers" {
		t.Fatalf("ProjectRoleByID = %+v, %v", role, ok)
	}
}

// ONLY ACCOUNTS, NEVER GROUPS: a seat's membership is direct, and a group's
// members are the group's business.
func TestARolesMembersAreItsDirectAccounts(t *testing.T) {
	t.Parallel()
	site := newRoleSite(t)
	site.on(http.MethodGet, "/rest/api/3/project/GOP/role/10101", http.StatusOK, `{"actors": [
		{"type": "atlassian-user-role-actor", "actorUser": {"accountId": "acc-sre"}},
		{"type": "atlassian-group-role-actor", "actorGroup": {"groupId": "g1"}},
		{"type": "atlassian-user-role-actor", "actorUser": {"accountId": "acc-swe"}}
	]}`)
	members, err := site.client(t).ProjectRoleMembers(context.Background(), "GOP", 10101)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(members, []string{"acc-sre", "acc-swe"}) {
		t.Fatalf("members = %v", members)
	}
}

func TestAddingAndRemovingARoleMemberNamesTheAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	site := newRoleSite(t)
	site.on(http.MethodPost, "/rest/api/3/project/GOP/role/10101", http.StatusOK, `{}`)
	site.on(http.MethodDelete, "/rest/api/3/project/GOP/role/10101", http.StatusNoContent, ``)
	c := site.client(t)
	if err := c.AddProjectRoleMember(ctx, "GOP", 10101, "acc-sre"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveProjectRoleMember(ctx, "GOP", 10101, "acc-sre"); err != nil {
		t.Fatal(err)
	}
	if got := site.bodies[0]["user"]; !slices.Equal(got, []string{"acc-sre"}) {
		t.Fatalf("add body user = %v", got)
	}
	if got := site.calls[1]; got != "DELETE /rest/api/3/project/GOP/role/10101?user=acc-sre" {
		t.Fatalf("remove = %q", got)
	}
}

// A MEMBER ALREADY GONE IS THE STATE ASKED FOR, while a refusal is kept with
// its status so the pass can name it.
func TestRemovingAGoneMemberSucceedsAndARefusalKeepsItsStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	site := newRoleSite(t)
	site.on(http.MethodPost, "/rest/api/3/project/GOP/role/10101", http.StatusForbidden,
		`{"errorMessages":["You do not have permission to administer this project."]}`)
	c := site.client(t)
	if err := c.RemoveProjectRoleMember(ctx, "GOP", 10101, "acc-sre"); err != nil {
		t.Fatalf("a 404 failed the removal: %v", err)
	}
	err := c.AddProjectRoleMember(ctx, "GOP", 10101, "acc-sre")
	if jira.Status(err) != http.StatusForbidden {
		t.Fatalf("refusal = %v, want the 403 kept", err)
	}
}
