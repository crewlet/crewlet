package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The write half of the GitLab client: what a provisioning run needs and the
// engine never calls.
//
// Kept beside the read client rather than in its own package because they
// share the base URL, the PRIVATE-TOKEN header and the error shape — and a
// second client would be a second place for those to drift, on the surface
// where drifting means authenticating as the wrong account.

// send performs a request with a JSON body, decoding a JSON response.
func (c *Client) send(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("gitlab: encode %s: %w", path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+APIPath+path, reader)
	if err != nil {
		return fmt.Errorf("gitlab: %s: %w", path, err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("gitlab: %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &APIError{
			Method: method, Path: path, Status: resp.StatusCode,
			Detail: strings.TrimSpace(string(detail)),
		}
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("gitlab: decode %s: %w", path, err)
	}
	return nil
}

// APIError is a refused call, carrying enough to decide what to do about it.
//
// THE STATUS IS THE POINT. A provisioning run treats 409 (already exists) as
// success and 403 (not permitted) as fatal, and a flat error string would
// make both a substring match against whatever wording the instance
// happens to use — which differs by GitLab version and by locale.
type APIError struct {
	Method string
	Path   string
	Status int
	Detail string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gitlab: %s %s: %d: %s", e.Method, e.Path, e.Status, e.Detail)
}

// Conflict reports a call refused because the thing already exists.
func (e *APIError) Conflict() bool { return e.Status == http.StatusConflict }

// Forbidden reports a credential that is not permitted to do this.
func (e *APIError) Forbidden() bool {
	return e.Status == http.StatusForbidden || e.Status == http.StatusUnauthorized
}

// User is a GitLab account as a provisioning run needs it.
type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Email    string `json:"email"`
}

// UserByUsername finds an account, reporting whether it exists.
//
// BY USERNAME, which is the only stable identifier a provisioning run can
// derive from a seat handle: ids are assigned by the instance and emails are
// editable by the account itself.
func (c *Client) UserByUsername(ctx context.Context, username string) (User, bool, error) {
	var out []User
	err := c.get(ctx, "/users", url.Values{"username": {username}}, &out)
	if err != nil {
		return User{}, false, err
	}
	for _, u := range out {
		// EXACT MATCH, because /users?username= is a filter rather than a
		// lookup on some versions and returns prefix matches — under
		// which `crewlet-swe` would find `crewlet-swe-old` and the run
		// would mint a token for the wrong account.
		if u.Username == username {
			return u, true, nil
		}
	}
	return User{}, false, nil
}

// CreateServiceAccount creates a group service account.
//
// A SERVICE ACCOUNT, not a user: it consumes no licence seat, cannot sign
// in, and is owned by the group rather than by a person — which is what
// makes it removable when a seat is decommissioned without touching
// anybody's real account.
func (c *Client) CreateServiceAccount(ctx context.Context, groupID int, name, username, email string) (User, error) {
	var out User
	err := c.send(ctx, http.MethodPost,
		"/groups/"+strconv.Itoa(groupID)+"/service_accounts",
		map[string]string{"name": name, "username": username, "email": email}, &out)
	return out, err
}

// Group is a GitLab group.
type Group struct {
	ID       int    `json:"id"`
	FullPath string `json:"full_path"`
}

// GroupByPath resolves a group by its path.
func (c *Client) GroupByPath(ctx context.Context, path string) (Group, bool, error) {
	var out Group
	// URL-ESCAPED, because a nested group's path contains slashes and an
	// unescaped one addresses a different endpoint entirely.
	err := c.get(ctx, "/groups/"+url.PathEscape(path), nil, &out)
	if isNotFound(err) {
		return Group{}, false, nil
	}
	if err != nil {
		return Group{}, false, err
	}
	return out, true, nil
}

// AddGroupMember adds an account to a group at an access level.
func (c *Client) AddGroupMember(ctx context.Context, groupID, userID, accessLevel int) error {
	err := c.send(ctx, http.MethodPost, "/groups/"+strconv.Itoa(groupID)+"/members",
		map[string]int{"user_id": userID, "access_level": accessLevel}, nil)
	if isConflict(err) {
		// ALREADY A MEMBER is success. The run is a reconcile, and a
		// second run must not fail on what the first one did.
		return nil
	}
	return err
}

// AddProjectMember adds an account to a project at an access level.
func (c *Client) AddProjectMember(ctx context.Context, project string, userID, accessLevel int) error {
	err := c.send(ctx, http.MethodPost,
		"/projects/"+url.PathEscape(project)+"/members",
		map[string]int{"user_id": userID, "access_level": accessLevel}, nil)
	if isConflict(err) {
		return nil
	}
	return err
}

// CreateToken mints a personal access token for a service account.
//
// The value is returned ONCE, by GitLab, and never again — which is why the
// sink is written through rather than batched: between minting and recording
// there is a window where the only copy of a live credential is in this
// process's memory.
func (c *Client) CreateToken(ctx context.Context, userID int, name string, scopes []string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.send(ctx, http.MethodPost,
		"/users/"+strconv.Itoa(userID)+"/personal_access_tokens",
		map[string]any{"name": name, "scopes": scopes}, &out)
	if err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("gitlab: the instance minted a token for %s and "+
			"returned no value; it exists and cannot be recovered — revoke it "+
			"in the account's settings", name)
	}
	return out.Token, nil
}

// RevokeTokens removes every token on an account.
//
// THE ROLLBACK PATH. A run that cannot record what it minted revokes it, and
// revoking by account rather than by id is deliberate: the id of a token
// whose creation response was lost is exactly what is not available.
func (c *Client) RevokeTokens(ctx context.Context, userID int) error {
	var tokens []struct {
		ID int `json:"id"`
	}
	if err := c.get(ctx, "/personal_access_tokens",
		url.Values{"user_id": {strconv.Itoa(userID)}}, &tokens); err != nil {
		return err
	}
	var failures []string
	for _, t := range tokens {
		if err := c.send(ctx, http.MethodDelete,
			"/personal_access_tokens/"+strconv.Itoa(t.ID), nil, nil); err != nil {
			failures = append(failures, strconv.Itoa(t.ID))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("gitlab: these tokens on user %d could not be revoked "+
			"and are still live — remove them by hand: %s",
			userID, strings.Join(failures, ", "))
	}
	return nil
}

// Hook is a registered webhook.
type Hook struct {
	ID  int    `json:"id"`
	URL string `json:"url"`
}

// GroupHooks lists a group's webhooks.
func (c *Client) GroupHooks(ctx context.Context, groupID int) ([]Hook, error) {
	var out []Hook
	err := c.get(ctx, "/groups/"+strconv.Itoa(groupID)+"/hooks", nil, &out)
	return out, err
}

// CreateGroupHook registers a webhook on a group.
//
// EVERY EVENT THE PARSER UNDERSTANDS, and no more: a hook subscribed to
// something nothing routes is delivery this engine answers with a 200 and
// drops, which looks from the instance's side like a healthy integration.
func (c *Client) CreateGroupHook(ctx context.Context, groupID int, target, secret string) (Hook, error) {
	var out Hook
	err := c.send(ctx, http.MethodPost, "/groups/"+strconv.Itoa(groupID)+"/hooks",
		hookBody(target, secret), &out)
	return out, err
}

// UpdateGroupHook re-points an existing hook, which is what a rotation of the
// signing secret needs.
func (c *Client) UpdateGroupHook(ctx context.Context, groupID, hookID int, target, secret string) error {
	return c.send(ctx, http.MethodPut,
		"/groups/"+strconv.Itoa(groupID)+"/hooks/"+strconv.Itoa(hookID),
		hookBody(target, secret), nil)
}

// hookBody is the subscription every crewlet hook carries.
func hookBody(target, secret string) map[string]any {
	return map[string]any{
		"url": target,
		// The Standard-Webhooks signing token. GitLab sends it back as a
		// header the engine verifies an HMAC with; the weaker plain-token
		// scheme is deliberately unsupported.
		"token": secret,
		// The four the parser routes. Push is absent on purpose: nothing
		// routes it, and subscribing would spend delivery on events this
		// engine answers 200 and drops.
		"issues_events":         true,
		"merge_requests_events": true,
		"note_events":           true,
		"pipeline_events":       true,
		// TLS verification stays ON. A provisioner that turned it off to
		// make a self-signed development instance work would leave it off
		// in production, where the hook carries a signing secret.
		"enable_ssl_verification": true,
	}
}

// isNotFound reports an error that is a 404 — "not there yet", which for a
// reconcile is an ordinary answer rather than a failure.
func isNotFound(err error) bool {
	var apiErr *APIError
	return asAPIError(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// isConflict reports an error that is a 409.
func isConflict(err error) bool {
	var apiErr *APIError
	return asAPIError(err, &apiErr) && apiErr.Conflict()
}

// asAPIError unwraps to an APIError.
func asAPIError(err error, target **APIError) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*APIError); ok {
		*target = e
		return true
	}
	return false
}
