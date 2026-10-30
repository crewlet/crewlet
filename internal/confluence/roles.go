package confluence

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Space access, the access a seat holds in Confluence, in Confluence's terms.
//
// A Confluence Cloud site grants access to a space in one of two ways, and
// which one applies is the site's, not this engine's, choice:
//
//   - Space roles (Atlassian's role-based access, a beta): Viewer,
//     Collaborator, Manager, Admin and any custom role, assigned per space.
//   - Space permissions by operation, on a site without the beta: View, Add
//     pages, Delete pages, Admin and so on, granted one by one per space.
//
// The engine reads which mode the site is in ([Client.SpaceRoleMode]) and
// manages the seat's access in that mode only.
// Sources: https://developer.atlassian.com/cloud/confluence/rest/v2/api-group-space-roles/
// and https://developer.atlassian.com/cloud/confluence/rest/v1/api-group-space-permissions/

// apiV2Path is the prefix of Confluence Cloud's v2 REST surface, under the
// same base the v1 surface ([APIPath]) is served from.
const apiV2Path = "/api/v2"

// v2PageSize is the largest page the v2 listings serve.
const v2PageSize = 250

// v2PageLimit bounds how far a listing follows its own cursor. A runaway
// guard rather than a ceiling: at 250 a page it covers far more than any
// site's roles or one space's grants.
const v2PageLimit = 40

// RoleMode is how a site grants access to its spaces.
type RoleMode string

// The modes Confluence reports from GET /space-role-mode.
const (
	// RoleModePreRoles is a site without role-based access: space
	// permissions by operation.
	RoleModePreRoles RoleMode = "PRE_ROLES"
	// RoleModeTransition is a site part-way through moving to roles. Roles
	// can be assigned, so the engine manages roles.
	RoleModeTransition RoleMode = "ROLES_TRANSITION"
	// RoleModeRoles is a site on role-based access.
	RoleModeRoles RoleMode = "ROLES"
)

// Roles reports a mode in which a seat's access is a space role.
func (m RoleMode) Roles() bool { return m == RoleModeRoles || m == RoleModeTransition }

// SpaceRoleMode reads how the site grants space access.
//
// A site that does not serve the endpoint (a 404, which is how an endpoint
// behind a beta the site has not joined answers) is on space permissions.
func (c *Client) SpaceRoleMode(ctx context.Context) (RoleMode, error) {
	var out struct {
		Mode RoleMode `json:"mode"`
	}
	err := c.do(ctx, http.MethodGet, apiV2Path+"/space-role-mode", nil, nil, &out)
	if Status(err) == http.StatusNotFound {
		return RoleModePreRoles, nil
	}
	if err != nil {
		return "", err
	}
	if out.Mode == "" {
		return RoleModePreRoles, nil
	}
	return out.Mode, nil
}

// SpaceRole is one role a space member may hold.
type SpaceRole struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Type is SYSTEM for Confluence's own roles and CUSTOM for the site's.
	Type string `json:"type"`
	// SpacePermissions are the operations the role grants, in the v2
	// surface's spelling: "read/space", "administer/space".
	SpacePermissions []string `json:"spacePermissions"`
}

// Administration reports a role that administers the spaces it is held in,
// which is Confluence's Admin and any custom role that carries the same
// permission. A raise to one needs a fresh step-up (D10).
func (r SpaceRole) Administration() bool {
	return slices.Contains(r.SpacePermissions, OperationAdminister.v2ID())
}

// SpaceRoles lists the site's space roles, live.
func (c *Client) SpaceRoles(ctx context.Context) ([]SpaceRole, error) {
	var out []SpaceRole
	err := c.walkV2(ctx, "/space-roles", url.Values{}, func(raw []byte) error {
		var page []SpaceRole
		if err := decodeResults(raw, &page); err != nil {
			return err
		}
		out = append(out, page...)
		return nil
	})
	return out, err
}

// SpaceRoleByID finds one role in a listing.
func SpaceRoleByID(roles []SpaceRole, id string) (SpaceRole, bool) {
	for _, role := range roles {
		if role.ID == id {
			return role, true
		}
	}
	return SpaceRole{}, false
}

// Space is a space as the access calls need it: v2 addresses a space by its
// numeric id and v1 by its key.
type Space struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

// SpaceByKey resolves a space key, reporting whether the site has it.
func (c *Client) SpaceByKey(ctx context.Context, key string) (Space, bool, error) {
	var out struct {
		Results []Space `json:"results"`
	}
	err := c.do(ctx, http.MethodGet, apiV2Path+"/spaces", url.Values{"keys": {key}}, nil, &out)
	if err != nil {
		return Space{}, false, err
	}
	for _, space := range out.Results {
		// EXACT, because a filter that matched loosely would hand a seat
		// another space's access.
		if strings.EqualFold(space.Key, key) {
			return space, true, nil
		}
	}
	return Space{}, false, nil
}

// principalUser is the principal type of an account.
const principalUser = "USER"

type principal struct {
	PrincipalType string `json:"principalType"`
	PrincipalID   string `json:"principalId"`
}

type roleAssignment struct {
	Principal principal `json:"principal"`
	RoleID    string    `json:"roleId,omitempty"`
}

// SpaceRoleOf is the role an account holds directly in a space, and false
// where it holds none.
func (c *Client) SpaceRoleOf(ctx context.Context, spaceID, accountID string) (string, bool, error) {
	params := url.Values{"principal-id": {accountID}, "principal-type": {principalUser}}
	var role string
	err := c.walkV2(ctx, "/spaces/"+url.PathEscape(spaceID)+"/role-assignments", params,
		func(raw []byte) error {
			var page []roleAssignment
			if err := decodeResults(raw, &page); err != nil {
				return err
			}
			for _, assignment := range page {
				if assignment.Principal.PrincipalType == principalUser &&
					assignment.Principal.PrincipalID == accountID && role == "" {
					role = assignment.RoleID
				}
			}
			return nil
		})
	if err != nil {
		return "", false, err
	}
	return role, role != "", nil
}

// SetSpaceRole assigns an account a role in a space, replacing any it held.
// An empty roleID removes the account's assignment, which is how Confluence
// spells a removal: an entry with no `roleId`.
func (c *Client) SetSpaceRole(ctx context.Context, spaceID, accountID, roleID string) error {
	body := []roleAssignment{{
		Principal: principal{PrincipalType: principalUser, PrincipalID: accountID},
		RoleID:    roleID,
	}}
	return c.do(ctx, http.MethodPost,
		apiV2Path+"/spaces/"+url.PathEscape(spaceID)+"/role-assignments", nil, body, nil)
}

// Operation is one space permission on a site without role-based access.
type Operation struct {
	Key    string `json:"key"`
	Target string `json:"target"`
}

// ID is the operation as the company document stores it: "read:space".
func (o Operation) ID() string { return o.Key + ":" + o.Target }

// v2ID is the operation as Confluence's v2 surface spells it inside a space
// role: "read/space".
func (o Operation) v2ID() string { return o.Key + "/" + o.Target }

// Name is Confluence's own name for the operation, as its space permissions
// screen shows it.
func (o Operation) Name() string {
	for _, op := range Operations {
		if op.Operation == o {
			return op.Name
		}
	}
	return o.ID()
}

// OperationAdminister is the space permission that administers a space.
//
//nolint:gochecknoglobals // an immutable catalogue entry, not state
var OperationAdminister = Operation{Key: "administer", Target: "space"}

// OperationRead is the space permission every other one requires. Confluence
// removes all of an account's permissions in a space when this one goes.
//
//nolint:gochecknoglobals // an immutable catalogue entry, not state
var OperationRead = Operation{Key: "read", Target: "space"}

// NamedOperation is one offered operation with Confluence's name for it.
type NamedOperation struct {
	Operation
	Name string
}

// Operations is every space permission a seat may be granted, under
// Confluence's names. The pairs are the ones Confluence's v1 surface accepts
// (SpacePermissionRequest in the published reference).
//
//nolint:gochecknoglobals // an immutable catalogue, not state
var Operations = []NamedOperation{
	{OperationRead, "View"},
	{Operation{"delete", "space"}, "Delete own"},
	{Operation{"create", "page"}, "Add pages"},
	{Operation{"archive", "page"}, "Archive pages"},
	{Operation{"delete", "page"}, "Delete pages"},
	{Operation{"create", "blogpost"}, "Add blog posts"},
	{Operation{"delete", "blogpost"}, "Delete blog posts"},
	{Operation{"create", "comment"}, "Add comments"},
	{Operation{"delete", "comment"}, "Delete comments"},
	{Operation{"create", "attachment"}, "Add attachments"},
	{Operation{"delete", "attachment"}, "Delete attachments"},
	{Operation{"restrict_content", "space"}, "Add or delete restrictions"},
	{Operation{"export", "space"}, "Export space"},
	{OperationAdminister, "Admin"},
}

// OperationByID finds an offered operation by its "key:target" id.
func OperationByID(id string) (Operation, bool) {
	for _, op := range Operations {
		if op.ID() == strings.TrimSpace(id) {
			return op.Operation, true
		}
	}
	return Operation{}, false
}

// OperationsLevel is a set of operations as one stored level: each id, in
// the catalogue's order, separated by a space. The catalogue's order rather
// than the alphabet's, so View reads first wherever the set is shown.
func OperationsLevel(ops []Operation) string {
	var ids []string
	for _, op := range Operations {
		if slices.Contains(ops, op.Operation) {
			ids = append(ids, op.ID())
		}
	}
	return strings.Join(ids, " ")
}

// ParseOperationsLevel reads a stored level back into its operations,
// refusing one Confluence does not grant to an account.
func ParseOperationsLevel(level string) ([]Operation, error) {
	var out []Operation
	for _, id := range strings.Fields(level) {
		op, ok := OperationByID(id)
		if !ok {
			return nil, fmt.Errorf("confluence: %q is not a space permission this build grants", id)
		}
		if !slices.Contains(out, op) {
			out = append(out, op)
		}
	}
	return out, nil
}

// SpacePermission is one permission an account holds in a space.
type SpacePermission struct {
	// ID is the permission's own id, which a removal names.
	ID        string
	Operation Operation
}

// SpacePermissionsOf lists the permissions an account holds directly in a
// space.
func (c *Client) SpacePermissionsOf(ctx context.Context, spaceID, accountID string) ([]SpacePermission, error) {
	all, err := c.SpacePermissions(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	return all[accountID], nil
}

// SpacePermissions lists every permission accounts hold directly in a space,
// by account id. Groups' grants are left out: a seat's access is always its
// own.
func (c *Client) SpacePermissions(ctx context.Context, spaceID string) (map[string][]SpacePermission, error) {
	out := map[string][]SpacePermission{}
	err := c.walkV2(ctx, "/spaces/"+url.PathEscape(spaceID)+"/permissions", url.Values{},
		func(raw []byte) error {
			var page []struct {
				ID        string `json:"id"`
				Principal struct {
					Type string `json:"type"`
					ID   string `json:"id"`
				} `json:"principal"`
				Operation struct {
					Key        string `json:"key"`
					TargetType string `json:"targetType"`
				} `json:"operation"`
			}
			if err := decodeResults(raw, &page); err != nil {
				return err
			}
			for _, row := range page {
				if row.Principal.Type != "user" || row.Principal.ID == "" {
					continue
				}
				out[row.Principal.ID] = append(out[row.Principal.ID], SpacePermission{
					ID:        row.ID,
					Operation: Operation{Key: row.Operation.Key, Target: row.Operation.TargetType},
				})
			}
			return nil
		})
	return out, err
}

// AddSpacePermission grants an account one operation in a space. v1 is the
// only surface that writes a space permission; it addresses the space by key.
func (c *Client) AddSpacePermission(ctx context.Context, spaceKey, accountID string, op Operation) error {
	body := map[string]any{
		"subject":   map[string]string{"type": "user", "identifier": accountID},
		"operation": op,
	}
	return c.api(ctx, http.MethodPost, "/space/"+url.PathEscape(spaceKey)+"/permission", nil, body, nil)
}

// RemoveSpacePermission revokes one permission by its id. One already gone is
// the state asked for, so a 404 is success.
func (c *Client) RemoveSpacePermission(ctx context.Context, spaceKey, permissionID string) error {
	err := c.api(ctx, http.MethodDelete,
		"/space/"+url.PathEscape(spaceKey)+"/permission/"+url.PathEscape(permissionID), nil, nil, nil)
	if Status(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// walkV2 follows a v2 listing's cursor to exhaustion, handing each page's raw
// body to visit.
//
// THE CURSOR, NOT THE LINK. `_links.next` is relative to the site and carries
// the /wiki prefix, while this client's base already ends in it on a site and
// does not on a gateway; re-issuing the same path with the link's cursor is
// correct on both.
func (c *Client) walkV2(ctx context.Context, path string, params url.Values, visit func([]byte) error) error {
	params.Set("limit", strconv.Itoa(v2PageSize))
	for range v2PageLimit {
		var raw rawPage
		if err := c.do(ctx, http.MethodGet, apiV2Path+path, params, nil, &raw); err != nil {
			return err
		}
		if err := visit(raw.Results); err != nil {
			return fmt.Errorf("confluence: decode %s: %w", path, err)
		}
		cursor, err := nextCursor(raw.Links.Next)
		if err != nil {
			return fmt.Errorf("confluence: read the next page of %s: %w", path, err)
		}
		if cursor == "" {
			return nil
		}
		params.Set("cursor", cursor)
	}
	return fmt.Errorf("confluence: the listing at %s did not end after %d pages", path, v2PageLimit)
}

// rawPage is one page of a v2 listing, with the rows left for the caller.
type rawPage struct {
	Results json.RawMessage `json:"results"`
	Links   struct {
		Next string `json:"next"`
	} `json:"_links"`
}

// decodeResults reads one page's rows. A page with no rows field is empty.
func decodeResults(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// nextCursor is the cursor a `_links.next` carries, or "" at the end.
func nextCursor(next string) (string, error) {
	if strings.TrimSpace(next) == "" {
		return "", nil
	}
	parsed, err := url.Parse(next)
	if err != nil {
		return "", err
	}
	cursor := parsed.Query().Get("cursor")
	if cursor == "" {
		return "", fmt.Errorf("the next link %q carries no cursor", next)
	}
	return cursor, nil
}
