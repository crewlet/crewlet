package jira

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Project roles, the access a seat holds in Jira, in Jira's own terms.
//
// A project role is defined once for the site and held per project, and what
// it may do is set by each project's permission scheme. So the engine reads
// the site's roles live, stores the role's id, and manages only the seat's
// membership of that role in each project the company lists. It never edits
// a permission scheme.
// Source: https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-project-roles/
// and https://developer.atlassian.com/cloud/jira/platform/rest/v3/api-group-project-role-actors/

// ProjectRole is one of the site's project roles.
type ProjectRole struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Admin is Jira's own flag for the role that administers a project.
	Admin bool `json:"admin"`
}

// administratorsRole is the name Jira gives the built-in role that
// administers a company-managed project. Read together with [ProjectRole.Admin],
// which Jira sets on a team-managed project's Administrator role.
const administratorsRole = "Administrators"

// Administration reports a role that administers the projects it is held in.
// A raise to one carries a warning and needs a fresh step-up (D10).
func (r ProjectRole) Administration() bool {
	return r.Admin || strings.EqualFold(strings.TrimSpace(r.Name), administratorsRole)
}

// ProjectRoles lists the site's project roles, live.
func (c *Client) ProjectRoles(ctx context.Context) ([]ProjectRole, error) {
	var out []ProjectRole
	if err := c.api(ctx, http.MethodGet, "/role", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ProjectRoleByID finds one role in a listing.
func ProjectRoleByID(roles []ProjectRole, id int64) (ProjectRole, bool) {
	for _, role := range roles {
		if role.ID == id {
			return role, true
		}
	}
	return ProjectRole{}, false
}

// roleActor is one member of a project role in one project.
type roleActor struct {
	Type      string `json:"type"`
	ActorUser *struct {
		AccountID string `json:"accountId"`
	} `json:"actorUser"`
}

// userActorType is how Jira marks an actor that is an account rather than a
// group. Only accounts are read: a seat's membership is always direct, and a
// group's members are the group's business.
const userActorType = "atlassian-user-role-actor"

// ProjectRoleMembers is the accounts that hold one role in one project
// directly, by account id.
func (c *Client) ProjectRoleMembers(ctx context.Context, project string, roleID int64) ([]string, error) {
	var out struct {
		Actors []roleActor `json:"actors"`
	}
	err := c.api(ctx, http.MethodGet, roleActorsPath(project, roleID), nil, nil, &out)
	if err != nil {
		return nil, err
	}
	members := make([]string, 0, len(out.Actors))
	for _, actor := range out.Actors {
		if actor.Type != userActorType || actor.ActorUser == nil {
			continue
		}
		if id := strings.TrimSpace(actor.ActorUser.AccountID); id != "" {
			members = append(members, id)
		}
	}
	return members, nil
}

// AddProjectRoleMember makes an account hold a role in a project.
//
// The caller reads [Client.ProjectRoleMembers] first and adds only an account
// that is absent, because Jira refuses a second add with a 400 rather than
// treating it as done.
func (c *Client) AddProjectRoleMember(ctx context.Context, project string, roleID int64, accountID string) error {
	return c.api(ctx, http.MethodPost, roleActorsPath(project, roleID), nil,
		map[string][]string{"user": {accountID}}, nil)
}

// RemoveProjectRoleMember ends an account's hold on a role in a project. An
// account that no longer holds it is the state asked for, so a 404 is success.
func (c *Client) RemoveProjectRoleMember(ctx context.Context, project string, roleID int64, accountID string) error {
	err := c.api(ctx, http.MethodDelete, roleActorsPath(project, roleID),
		url.Values{"user": {accountID}}, nil, nil)
	if Status(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// roleActorsPath addresses one role in one project.
func roleActorsPath(project string, roleID int64) string {
	return "/project/" + url.PathEscape(project) + "/role/" + strconv.FormatInt(roleID, 10)
}
