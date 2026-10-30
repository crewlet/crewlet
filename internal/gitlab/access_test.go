package gitlab_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlab"
)

// routed is a fake GitLab answering by "METHOD /path" and recording every
// request with its decoded body.
type routed struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []routedCall
	answer map[string]routedAnswer
}

type routedCall struct {
	method, path, query string
	body                map[string]any
}

type routedAnswer struct {
	status int
	body   string
}

func newRouted(t *testing.T, answers map[string]routedAnswer) *routed {
	t.Helper()
	r := &routed{answer: answers}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		r.mu.Lock()
		r.calls = append(r.calls, routedCall{req.Method, req.URL.Path, req.URL.RawQuery, body})
		r.mu.Unlock()
		a, ok := r.answer[req.Method+" "+req.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"404 Not Found"}`))
			return
		}
		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(a.body))
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *routed) only(t *testing.T) routedCall {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != 1 {
		t.Fatalf("calls = %d, want 1: %+v", len(r.calls), r.calls)
	}
	return r.calls[0]
}

// THE OFFERED ROLES ARE GITLAB'S, UNDER GITLAB'S NAMES, and two are left out
// on purpose (D6): Security Manager is a beta off by default, and Minimal
// Access grants an agent nothing.
func TestTheOfferedRolesAreGitLabsOwnLessTheTwoThatGrantAnAgentNothingUseful(t *testing.T) {
	t.Parallel()
	want := map[int]string{
		10: "Guest", 15: "Planner", 20: "Reporter", 30: "Developer", 40: "Maintainer", 50: "Owner",
	}
	for level, name := range want {
		got, ok := gitlab.RoleName(level)
		if !ok || got != name {
			t.Errorf("RoleName(%d) = %q, %v; want %q", level, got, ok, name)
		}
	}
	for _, level := range []int{0, 5, 25, 60} {
		if gitlab.Offered(level) {
			t.Errorf("level %d is offered", level)
		}
	}
	if !slices.IsSortedFunc(gitlab.Roles, func(a, b gitlab.Role) int { return a.Level - b.Level }) {
		t.Error("Roles are not in GitLab's order, least access first")
	}
}

// TOKEN SCOPES FOLLOW THE ROLE (D6): below Developer an account cannot push,
// so its token must not carry `api`.
func TestTokenScopesFollowTheRole(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		level int
		want  []string
	}{
		{gitlab.LevelGuest, []string{"read_api", "read_repository"}},
		{gitlab.LevelPlanner, []string{"read_api", "read_repository"}},
		{gitlab.LevelReporter, []string{"read_api", "read_repository"}},
		{gitlab.LevelDeveloper, []string{"api"}},
		{gitlab.LevelMaintainer, []string{"api"}},
		{gitlab.LevelOwner, []string{"api"}},
	} {
		if got := gitlab.ScopesFor(tc.level); !slices.Equal(got, tc.want) {
			t.Errorf("ScopesFor(%d) = %v, want %v", tc.level, got, tc.want)
		}
	}
	if !gitlab.SameScopes([]string{"read_repository", "read_api"}, []string{"read_api", "read_repository"}) {
		t.Error("the same scopes in another order compare unequal")
	}
	if gitlab.SameScopes([]string{"api"}, []string{"read_api"}) {
		t.Error("different scopes compare equal")
	}
}

func TestMaintainerAndOwnerAreAdministration(t *testing.T) {
	t.Parallel()
	for _, role := range gitlab.Roles {
		want := role.Level >= gitlab.LevelMaintainer
		if got := gitlab.Administration(role.Level); got != want {
			t.Errorf("Administration(%s) = %v, want %v", role.Name, got, want)
		}
	}
}

// A CUSTOM ROLE RIDES ALONGSIDE ITS BASE LEVEL on an add, and only when one
// is wanted: an instance below Ultimate does not know the field.
func TestAddingAMemberSendsACustomRoleOnlyWhenOneIsWanted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		access gitlab.Access
		role   any
	}{
		{"default role", gitlab.Access{Level: 20}, nil},
		{"custom role", gitlab.Access{Level: 20, MemberRoleID: 7}, float64(7)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := newRouted(t, map[string]routedAnswer{
				"POST /api/v4/projects/acme/api/members": {http.StatusCreated, `{}`},
			})
			if err := client(t, fake.server.URL).AddProjectMember(
				context.Background(), "acme/api", 42, tc.access); err != nil {
				t.Fatal(err)
			}
			call := fake.only(t)
			if call.body["user_id"] != float64(42) || call.body["access_level"] != float64(20) {
				t.Fatalf("body = %v", call.body)
			}
			got, present := call.body["member_role_id"]
			if tc.role == nil && present {
				t.Fatalf("member_role_id sent for a default role: %v", got)
			}
			if tc.role != nil && got != tc.role {
				t.Fatalf("member_role_id = %v, want %v", got, tc.role)
			}
		})
	}
}

// AN EDIT TO A DEFAULT ROLE CLEARS A CUSTOM ONE, which GitLab spells as an
// explicit null. Omitting it would leave a custom role a document no longer
// asks for.
func TestEditingAMemberToADefaultRoleClearsTheCustomRole(t *testing.T) {
	t.Parallel()
	fake := newRouted(t, map[string]routedAnswer{
		"PUT /api/v4/groups/9/members/42": {http.StatusOK, `{}`},
	})
	if err := client(t, fake.server.URL).SetGroupMember(
		context.Background(), 9, 42, gitlab.Access{Level: 30}); err != nil {
		t.Fatal(err)
	}
	call := fake.only(t)
	role, present := call.body["member_role_id"]
	if !present || role != nil {
		t.Fatalf("member_role_id = %v (present %v), want an explicit null", role, present)
	}
	if call.body["access_level"] != float64(30) {
		t.Fatalf("access_level = %v", call.body["access_level"])
	}
}

func TestAMembershipListingCarriesItsCustomRole(t *testing.T) {
	t.Parallel()
	fake := newRouted(t, map[string]routedAnswer{
		"GET /api/v4/groups/9/members": {http.StatusOK, `[
			{"id": 1, "username": "crewlet-sre", "access_level": 20, "member_role": {"id": 7, "name": "Auditor"}},
			{"id": 2, "username": "crewlet-swe", "access_level": 30, "member_role": null}
		]`},
	})
	members, err := client(t, fake.server.URL).GroupMembers(context.Background(), 9)
	if err != nil {
		t.Fatal(err)
	}
	if got := members[0].Access(); !got.Equal(gitlab.Access{Level: 20, MemberRoleID: 7}) {
		t.Errorf("custom member access = %+v", got)
	}
	if got := members[1].Access(); !got.Equal(gitlab.Access{Level: 30}) {
		t.Errorf("default member access = %+v", got)
	}
}

// REMOVING A MEMBERSHIP THAT IS ALREADY GONE IS THE STATE ASKED FOR, and the
// group removal keeps the account's project memberships: the pass removes
// exactly what it decided to.
func TestRemovingAMembershipIsIdempotentAndLeavesSubresources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRouted(t, map[string]routedAnswer{
		"DELETE /api/v4/groups/9/members/42": {http.StatusNoContent, ``},
	})
	c := client(t, fake.server.URL)
	if err := c.RemoveGroupMember(ctx, 9, 42); err != nil {
		t.Fatal(err)
	}
	if call := fake.only(t); call.query != "skip_subresources=true" {
		t.Fatalf("query = %q, want skip_subresources=true", call.query)
	}
	if err := c.RemoveProjectMember(ctx, "acme/api", 42); err != nil {
		t.Fatalf("a membership already gone failed the removal: %v", err)
	}
}

func TestBlockAndUnblockReachTheAdministratorRoutesAndKeepTheRefusal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRouted(t, map[string]routedAnswer{
		"POST /api/v4/users/42/block":   {http.StatusCreated, `true`},
		"POST /api/v4/users/42/unblock": {http.StatusForbidden, `{"message":"403 Forbidden"}`},
	})
	c := client(t, fake.server.URL)
	if err := c.BlockUser(ctx, 42); err != nil {
		t.Fatal(err)
	}
	err := c.UnblockUser(ctx, 42)
	if gitlab.Status(err) != http.StatusForbidden {
		t.Fatalf("unblock refusal = %v, want the 403 kept", err)
	}
}

func TestUserByIDReportsAnAccountThatIsGone(t *testing.T) {
	t.Parallel()
	fake := newRouted(t, map[string]routedAnswer{
		"GET /api/v4/users/42": {http.StatusOK, `{"id": 42, "username": "crewlet-sre", "state": "blocked"}`},
	})
	c := client(t, fake.server.URL)
	user, found, err := c.UserByID(context.Background(), 42)
	if err != nil || !found || !user.Blocked() {
		t.Fatalf("UserByID = %+v, %v, %v", user, found, err)
	}
	if _, found, err := c.UserByID(context.Background(), 43); err != nil || found {
		t.Fatalf("a deleted account read as %v, %v", found, err)
	}
}

// CUSTOM ROLES HAVE TWO HOMES and an instance that serves neither is a
// default-roles-only instance, not a failure.
func TestCustomRolesAreReadLiveFromTheirHomeAndAbsenceIsNotAFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := newRouted(t, map[string]routedAnswer{
		"GET /api/v4/groups/9/member_roles": {http.StatusOK,
			`[{"id": 7, "name": "Auditor", "base_access_level": 20}]`},
		"GET /api/v4/member_roles": {http.StatusForbidden, `{"message":"403 Forbidden"}`},
	})
	c := client(t, fake.server.URL)
	roles, available, err := c.MemberRoles(ctx, 9, false)
	if err != nil || !available || len(roles) != 1 || roles[0].BaseAccessLevel != 20 {
		t.Fatalf("group roles = %+v, %v, %v", roles, available, err)
	}
	if role, ok := gitlab.MemberRoleByID(roles, 7); !ok || role.Name != "Auditor" {
		t.Fatalf("MemberRoleByID = %+v, %v", role, ok)
	}
	if _, available, err := c.MemberRoles(ctx, 9, true); err != nil || available {
		t.Fatalf("an instance that refuses custom roles read as %v, %v", available, err)
	}
}

func TestACustomRoleListingThatFailsIsAnError(t *testing.T) {
	t.Parallel()
	fake := newRouted(t, map[string]routedAnswer{
		"GET /api/v4/groups/9/member_roles": {http.StatusBadGateway, `bad gateway`},
	})
	if _, _, err := client(t, fake.server.URL).MemberRoles(context.Background(), 9, false); err == nil {
		t.Fatal("a 502 read as default roles only")
	}
}
