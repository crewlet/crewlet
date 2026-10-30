package confluence_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassianhost"
	"github.com/crewlet/crewlet/internal/confluence"
)

// spaceSite is a fake Confluence Cloud answering by "METHOD /path?query".
type spaceSite struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []string
	bodies []any
	answer map[string]spaceAnswer
}

type spaceAnswer struct {
	status int
	body   string
}

func newSpaceSite(t *testing.T, answers map[string]spaceAnswer) *spaceSite {
	t.Helper()
	s := &spaceSite{answer: answers}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body any
		_ = json.Unmarshal(raw, &body)
		key := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		s.mu.Lock()
		s.calls = append(s.calls, key)
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()
		a, ok := s.answer[key]
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

func (s *spaceSite) client(t *testing.T) *confluence.Client {
	t.Helper()
	c, err := confluence.NewClient(confluence.ClientOptions{
		URL: s.server.URL, Email: "admin@example.com", Token: "fake-token",
		Declared: atlassianhost.Cloud,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestASiteWithoutTheRoleBetaIsOnSpacePermissions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	without := newSpaceSite(t, nil)
	mode, err := without.client(t).SpaceRoleMode(ctx)
	if err != nil || mode != confluence.RoleModePreRoles || mode.Roles() {
		t.Fatalf("a site without the endpoint = %q, %v", mode, err)
	}
	with := newSpaceSite(t, map[string]spaceAnswer{
		"GET /api/v2/space-role-mode": {http.StatusOK, `{"mode": "ROLES_TRANSITION"}`},
	})
	mode, err = with.client(t).SpaceRoleMode(ctx)
	if err != nil || !mode.Roles() {
		t.Fatalf("a site in transition = %q, %v; want roles", mode, err)
	}
}

func TestARefusedRoleModeIsAnErrorNotAMode(t *testing.T) {
	t.Parallel()
	site := newSpaceSite(t, map[string]spaceAnswer{
		"GET /api/v2/space-role-mode": {http.StatusUnauthorized, `{"message":"Unauthorized"}`},
	})
	if _, err := site.client(t).SpaceRoleMode(context.Background()); confluence.Status(err) != http.StatusUnauthorized {
		t.Fatalf("err = %v, want the 401 kept", err)
	}
}

// EVERY PAGE, by the cursor the next link carries rather than the link itself,
// whose /wiki prefix the client's base already holds.
func TestSpaceRolesAreReadToTheLastPage(t *testing.T) {
	t.Parallel()
	site := newSpaceSite(t, map[string]spaceAnswer{
		"GET /api/v2/space-roles?limit=250": {http.StatusOK, `{
			"results": [{"id": "r-viewer", "name": "Viewer", "type": "SYSTEM", "spacePermissions": ["read/space"]}],
			"_links": {"next": "/wiki/api/v2/space-roles?cursor=abc&limit=250"}}`},
		"GET /api/v2/space-roles?cursor=abc&limit=250": {http.StatusOK, `{
			"results": [{"id": "r-admin", "name": "Admin", "type": "SYSTEM",
				"spacePermissions": ["read/space", "administer/space"]}],
			"_links": {}}`},
	})
	roles, err := site.client(t).SpaceRoles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 2 {
		t.Fatalf("roles = %+v", roles)
	}
	viewer, _ := confluence.SpaceRoleByID(roles, "r-viewer")
	admin, _ := confluence.SpaceRoleByID(roles, "r-admin")
	if viewer.Administration() || !admin.Administration() {
		t.Fatalf("administration: viewer %v, admin %v", viewer.Administration(), admin.Administration())
	}
}

func TestASpaceKeyResolvesExactly(t *testing.T) {
	t.Parallel()
	site := newSpaceSite(t, map[string]spaceAnswer{
		"GET /api/v2/spaces?keys=ENG": {http.StatusOK,
			`{"results": [{"id": "65537", "key": "ENG"}]}`},
		"GET /api/v2/spaces?keys=OPS": {http.StatusOK, `{"results": []}`},
	})
	c := site.client(t)
	space, found, err := c.SpaceByKey(context.Background(), "ENG")
	if err != nil || !found || space.ID != "65537" {
		t.Fatalf("ENG = %+v, %v, %v", space, found, err)
	}
	if _, found, err := c.SpaceByKey(context.Background(), "OPS"); err != nil || found {
		t.Fatalf("a missing space = %v, %v", found, err)
	}
}

func TestASeatsSpaceRoleIsReadAndSetAndRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	site := newSpaceSite(t, map[string]spaceAnswer{
		"GET /api/v2/spaces/65537/role-assignments?limit=250&principal-id=acc-sre&principal-type=USER": {
			http.StatusOK, `{"results": [{"principal": {"principalType": "USER", "principalId": "acc-sre"},
				"roleId": "r-viewer"}]}`},
		"POST /api/v2/spaces/65537/role-assignments": {http.StatusOK, `{"results": []}`},
	})
	c := site.client(t)
	role, held, err := c.SpaceRoleOf(ctx, "65537", "acc-sre")
	if err != nil || !held || role != "r-viewer" {
		t.Fatalf("SpaceRoleOf = %q, %v, %v", role, held, err)
	}
	if err := c.SetSpaceRole(ctx, "65537", "acc-sre", "r-collab"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetSpaceRole(ctx, "65537", "acc-sre", ""); err != nil {
		t.Fatal(err)
	}
	set := site.bodies[1].([]any)[0].(map[string]any)
	if set["roleId"] != "r-collab" {
		t.Fatalf("set body = %v", set)
	}
	// A REMOVAL IS AN ENTRY WITH NO roleId, which is how Confluence spells it.
	removal := site.bodies[2].([]any)[0].(map[string]any)
	if _, present := removal["roleId"]; present {
		t.Fatalf("removal body carries a roleId: %v", removal)
	}
	if p := removal["principal"].(map[string]any); p["principalId"] != "acc-sre" || p["principalType"] != "USER" {
		t.Fatalf("removal principal = %v", p)
	}
}

// ONLY THIS ACCOUNT'S GRANTS, never a group's or another account's.
func TestSpacePermissionsAreTheAccountsOwn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	site := newSpaceSite(t, map[string]spaceAnswer{
		"GET /api/v2/spaces/65537/permissions?limit=250": {http.StatusOK, `{"results": [
			{"id": "p1", "principal": {"type": "user", "id": "acc-sre"}, "operation": {"key": "read", "targetType": "space"}},
			{"id": "p2", "principal": {"type": "group", "id": "acc-sre"}, "operation": {"key": "create", "targetType": "page"}},
			{"id": "p3", "principal": {"type": "user", "id": "acc-swe"}, "operation": {"key": "administer", "targetType": "space"}},
			{"id": "p4", "principal": {"type": "user", "id": "acc-sre"}, "operation": {"key": "create", "targetType": "page"}}
		]}`},
		"POST /rest/api/space/ENG/permission":      {http.StatusOK, `{"id": 9}`},
		"DELETE /rest/api/space/ENG/permission/p1": {http.StatusNoContent, ``},
	})
	c := site.client(t)
	held, err := c.SpacePermissionsOf(ctx, "65537", "acc-sre")
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 2 || held[0].Operation != confluence.OperationRead || held[1].Operation.ID() != "create:page" {
		t.Fatalf("held = %+v", held)
	}
	if err := c.AddSpacePermission(ctx, "ENG", "acc-sre", confluence.Operation{Key: "create", Target: "page"}); err != nil {
		t.Fatal(err)
	}
	body := site.bodies[1].(map[string]any)
	subject := body["subject"].(map[string]any)
	op := body["operation"].(map[string]any)
	if subject["type"] != "user" || subject["identifier"] != "acc-sre" || op["key"] != "create" || op["target"] != "page" {
		t.Fatalf("add body = %v", body)
	}
	if err := c.RemoveSpacePermission(ctx, "ENG", "p1"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveSpacePermission(ctx, "ENG", "gone"); err != nil {
		t.Fatalf("a permission already gone failed the removal: %v", err)
	}
}

// THE OPERATIONS ARE NAMED AS CONFLUENCE NAMES THEM, and each is a pair the
// v1 surface accepts.
func TestOperationsCarryConfluencesNames(t *testing.T) {
	t.Parallel()
	for id, name := range map[string]string{
		"read:space": "View", "create:page": "Add pages", "administer:space": "Admin",
		"restrict_content:space": "Add or delete restrictions",
	} {
		op, ok := confluence.OperationByID(id)
		if !ok || op.Name() != name {
			t.Errorf("%s = %+v %q, want %q", id, op, op.Name(), name)
		}
	}
	if _, ok := confluence.OperationByID("purge:space"); ok {
		t.Error("an operation Confluence does not grant per account is offered")
	}
	seen := map[string]bool{}
	for _, op := range confluence.Operations {
		if seen[op.ID()] {
			t.Errorf("%s is listed twice", op.ID())
		}
		seen[op.ID()] = true
	}
}

// A SET OF OPERATIONS IS ONE STORED LEVEL, sorted whatever order it was
// written in, and an operation Confluence does not grant is refused rather
// than dropped.
func TestASetOfOperationsRoundTripsAsOneLevel(t *testing.T) {
	t.Parallel()
	ops, err := confluence.ParseOperationsLevel("create:page  read:space create:page")
	if err != nil {
		t.Fatal(err)
	}
	if got := confluence.OperationsLevel(ops); got != "create:page read:space" {
		t.Fatalf("level = %q", got)
	}
	if _, err := confluence.ParseOperationsLevel("read:space purge:space"); err == nil {
		t.Fatal("an operation Confluence does not grant was accepted")
	}
}
