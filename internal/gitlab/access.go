package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
)

// Per-seat access, in GitLab's own terms.
//
// A seat's access is stored as the integer GitLab's Members API takes
// (`access_level`), or as a custom role id (`member_role_id`) on Ultimate, and
// is shown under GitLab's own name for it. Nothing here translates to a
// Crewlet word: the only grouping the engine needs (is this an administration
// role, which token scopes does it get) is derived from GitLab's own order.

// Role is one of GitLab's default member roles an agent may hold.
type Role struct {
	// Level is GitLab's `access_level` integer.
	Level int
	// Name is GitLab's own name for the role.
	Name string
}

// The default roles GitLab offers a member, by `access_level`.
//
// Two are deliberately absent. Security Manager (25) is a beta introduced in
// GitLab 18.11 and disabled by default outside GitLab.com. Minimal Access (5)
// is valid only on a top-level group on Premium and Ultimate and grants
// nothing by itself, so an agent holding it can do no work.
// Source: https://docs.gitlab.com/api/members/ and
// https://docs.gitlab.com/user/permissions/
const (
	LevelGuest      = 10
	LevelPlanner    = 15
	LevelReporter   = 20
	LevelDeveloper  = 30
	LevelMaintainer = 40
	LevelOwner      = 50
)

// Roles is every default role a seat may hold, lowest first.
//
//nolint:gochecknoglobals // an immutable catalogue, not state
var Roles = []Role{
	{Level: LevelGuest, Name: "Guest"},
	{Level: LevelPlanner, Name: "Planner"},
	{Level: LevelReporter, Name: "Reporter"},
	{Level: LevelDeveloper, Name: "Developer"},
	{Level: LevelMaintainer, Name: "Maintainer"},
	{Level: LevelOwner, Name: "Owner"},
}

// RoleName is GitLab's name for a default role, and false for a level this
// build does not offer.
func RoleName(level int) (string, bool) {
	for _, role := range Roles {
		if role.Level == level {
			return role.Name, true
		}
	}
	return "", false
}

// Offered reports a level a seat may be given.
func Offered(level int) bool {
	_, ok := RoleName(level)
	return ok
}

// Administration reports a level that administers what it is a member of:
// Maintainer and Owner. A raise to one carries a warning and needs a fresh
// step-up (D10).
func Administration(level int) bool { return level >= LevelMaintainer }

// Read-only scopes cover the API and a clone; `api` is needed for any write,
// a push included. Source: https://docs.gitlab.com/user/profile/personal_access_tokens/
const (
	scopeAPI            = "api"
	scopeReadAPI        = "read_api"
	scopeReadRepository = "read_repository"
)

// ScopesFor is the token scopes a seat's level earns.
//
// THE ROLE DECIDES, so a token never carries more than its account may do:
// below Developer a member cannot push, and a token holding `api` would carry
// a write scope its role has no use for. A custom role follows the default
// role it is based on.
func ScopesFor(level int) []string {
	if level >= LevelDeveloper {
		return []string{scopeAPI}
	}
	return []string{scopeReadAPI, scopeReadRepository}
}

// SameScopes reports two scope sets that grant the same thing, in any order.
func SameScopes(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

// Access is what one membership holds.
type Access struct {
	// Level is GitLab's `access_level`. For a custom role it is the role's
	// base level, which GitLab requires alongside it.
	Level int
	// MemberRoleID is a custom role on Ultimate, or zero for a default role.
	MemberRoleID int
}

// Equal reports two memberships that grant the same thing.
func (a Access) Equal(b Access) bool {
	return a.Level == b.Level && a.MemberRoleID == b.MemberRoleID
}

// addBody is the POST body that creates a membership. A custom role is sent
// only where one is wanted: an instance below Ultimate does not know the
// field, and a zero would ask for a role that does not exist.
func (a Access) addBody(userID int) map[string]any {
	body := map[string]any{"user_id": userID, "access_level": a.Level}
	if a.MemberRoleID != 0 {
		body["member_role_id"] = a.MemberRoleID
	}
	return body
}

// editBody is the PUT body that moves a membership. A default role sends
// `member_role_id: null` explicitly, which is how GitLab clears a custom role
// (https://docs.gitlab.com/api/members/, "Edit a member"); an instance that
// does not serve custom roles ignores a field it never declared.
func (a Access) editBody() map[string]any {
	body := map[string]any{"access_level": a.Level, "member_role_id": nil}
	if a.MemberRoleID != 0 {
		body["member_role_id"] = a.MemberRoleID
	}
	return body
}

// RemoveGroupMember ends an account's direct membership of a group.
//
// A membership already gone is the state asked for, so a 404 is success.
// Subgroup and project memberships are left to their own calls: the pass
// removes exactly the memberships it decided to remove.
func (c *Client) RemoveGroupMember(ctx context.Context, groupID, userID int) error {
	err := c.send(ctx, http.MethodDelete,
		"/groups/"+strconv.Itoa(groupID)+"/members/"+strconv.Itoa(userID)+
			"?skip_subresources=true", nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// RemoveProjectMember ends an account's direct membership of a project.
func (c *Client) RemoveProjectMember(ctx context.Context, project string, userID int) error {
	err := c.send(ctx, http.MethodDelete,
		"/projects/"+url.PathEscape(project)+"/members/"+strconv.Itoa(userID), nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// UserByID reads one account, reporting whether it exists.
//
// By id, which is what the account ledger records: a username can be changed
// at GitLab and an id cannot.
func (c *Client) UserByID(ctx context.Context, userID int) (User, bool, error) {
	var out User
	err := c.get(ctx, "/users/"+strconv.Itoa(userID), nil, &out)
	if isNotFound(err) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	return out, true, nil
}

// BlockUser stops an account from signing in or using any token.
//
// AN INSTANCE ADMINISTRATOR ROUTE (https://docs.gitlab.com/api/user_moderation/).
// A group Owner, which is every gitlab.com deployment, is refused with a 403;
// the caller decides from the mode whether to ask at all. GitLab also answers
// 403 for an account blocked through LDAP, which this engine never unblocks.
func (c *Client) BlockUser(ctx context.Context, userID int) error {
	return c.send(ctx, http.MethodPost, "/users/"+strconv.Itoa(userID)+"/block", nil, nil)
}

// UnblockUser lets a blocked account sign in again. Administrator only, like
// [Client.BlockUser], and only ever called for an account the ledger records
// this engine as having blocked (D3).
func (c *Client) UnblockUser(ctx context.Context, userID int) error {
	return c.send(ctx, http.MethodPost, "/users/"+strconv.Itoa(userID)+"/unblock", nil, nil)
}

// MemberRole is a custom role, as GitLab lists it.
type MemberRole struct {
	ID              int    `json:"id"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	BaseAccessLevel int    `json:"base_access_level"`
}

// MemberRoles lists the custom roles a seat may be given, live.
//
// Custom roles are an Ultimate feature with two homes: on GitLab.com they
// belong to the top-level group (`GET /groups/:id/member_roles`, group Owner),
// and on a self-managed instance to the instance (`GET /member_roles`,
// administrator). Source: https://docs.gitlab.com/api/member_roles/
//
// available is false where the instance does not serve them to this
// credential (a 403 or 404: not Ultimate, or not permitted), which the picker
// reads as "default roles only" rather than as a failure. Any other refusal is
// an error.
func (c *Client) MemberRoles(ctx context.Context, groupID int, instance bool) ([]MemberRole, bool, error) {
	path := "/groups/" + strconv.Itoa(groupID) + "/member_roles"
	if instance {
		path = "/member_roles"
	}
	var out []MemberRole
	err := c.get(ctx, path, nil, &out)
	switch Status(err) {
	case 0:
		if err != nil {
			return nil, false, err
		}
		return out, true, nil
	case http.StatusForbidden, http.StatusNotFound:
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("gitlab: list custom roles: %w", err)
}

// MemberRoleByID finds one custom role in a listing.
func MemberRoleByID(roles []MemberRole, id int) (MemberRole, bool) {
	for _, role := range roles {
		if role.ID == id {
			return role, true
		}
	}
	return MemberRole{}, false
}
