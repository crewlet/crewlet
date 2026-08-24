package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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

// Status reports the HTTP status a call was refused with, or 0 when the
// failure was not an API error.
//
// The same accessor the Plane and Mattermost clients export, for the same
// reason: a caller deciding what to do about a refusal needs the number, and
// three vendor packages spelling that three ways is three places to get it
// wrong.
func Status(err error) int {
	var e *APIError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
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

// ProjectExists reports whether a project path resolves on this instance.
//
// It answers a QUESTION rather than raising: a declared project that has
// been renamed, moved or never created is an ordinary state of a company's
// config, and the reconcile's answer to it is to drop that one and carry on.
// A 404 is therefore data here, and every other refusal is still an error.
func (c *Client) ProjectExists(ctx context.Context, project string) (bool, error) {
	err := c.get(ctx, "/projects/"+url.PathEscape(project), nil, nil)
	if err == nil {
		return true, nil
	}
	if Status(err) == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

// Token is a personal access token as the list endpoint serves it.
//
// Never the value: GitLab returns that from the mint call alone, so a run
// that fails to persist what it minted cannot go back and read it.
type Token struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Revoked bool   `json:"revoked"`
	Active  bool   `json:"active"`
	// ExpiresAt is a DATE, not a timestamp — GitLab serves "2026-01-31".
	//
	// Read for the record rather than for a decision: whether a token
	// still authenticates is answered by USING it, not by comparing this
	// to a clock. A calendar check here would be a second opinion that
	// can disagree with the instance — over a timezone, a grace period,
	// or a token revoked before its date.
	ExpiresAt Date `json:"expires_at"`
	// Value is the plaintext, present on a mint response only.
	Value string `json:"token"`
}

// Date is GitLab's bare `YYYY-MM-DD` expiry, which is not a timestamp and
// does not unmarshal as one.
//
// Its own type rather than a string, because the only question anyone asks
// of it is whether it has passed — and a string comparison would answer
// that correctly right up until somebody compared it to a timestamp.
type Date struct{ time.Time }

// UnmarshalJSON accepts a date, a null, or an empty string. An expiry GitLab
// did not set is the zero time, which reads as "never" — the same thing the
// API means by a null here.
func (d *Date) UnmarshalJSON(raw []byte) error {
	var text *string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	if text == nil || strings.TrimSpace(*text) == "" {
		d.Time = time.Time{}
		return nil
	}
	parsed, err := time.Parse(time.DateOnly, strings.TrimSpace(*text))
	if err != nil {
		// UNPARSEABLE IS NEVER-EXPIRES rather than an error: the only
		// consumer asks "has this passed", and refusing the whole
		// listing over a format nobody anticipated would break a run
		// that has nothing to do with expiry.
		d.Time = time.Time{}
		return nil
	}
	d.Time = parsed
	return nil
}

// Tokens lists an account's personal access tokens.
func (c *Client) Tokens(ctx context.Context, userID int) ([]Token, error) {
	var tokens []Token
	err := c.get(ctx, "/personal_access_tokens",
		url.Values{"user_id": {strconv.Itoa(userID)}}, &tokens)
	return tokens, err
}

// CreateToken mints a personal access token for a service account.
//
// The value is returned ONCE, by GitLab, and never again — which is why the
// sink is written through rather than batched: between minting and recording
// there is a window where the only copy of a live credential is in this
// process's memory.
func (c *Client) CreateToken(ctx context.Context, userID int, name string, scopes []string, expiry time.Time) (Token, error) {
	body := map[string]any{"name": name, "scopes": scopes}
	if !expiry.IsZero() {
		body["expires_at"] = expiry.UTC().Format(time.DateOnly)
	}
	var out Token
	err := c.send(ctx, http.MethodPost,
		"/users/"+strconv.Itoa(userID)+"/personal_access_tokens", body, &out)
	if err != nil {
		return Token{}, err
	}
	if out.Value == "" {
		return Token{}, fmt.Errorf("gitlab: the instance minted a token for %s and "+
			"returned no value; it exists and cannot be recovered — revoke it "+
			"in the account's settings", name)
	}
	return out, nil
}

// RevokeToken removes one token.
func (c *Client) RevokeToken(ctx context.Context, tokenID int) error {
	err := c.send(ctx, http.MethodDelete,
		"/personal_access_tokens/"+strconv.Itoa(tokenID), nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// RevokeTokens removes every token on an account.
//
// THE ROLLBACK PATH FOR AN ACCOUNT THIS RUN CREATED, and only that: nothing
// else has ever minted on it, so taking everything is taking exactly what
// this run caused. On an account that already existed the rollback revokes
// by id instead — sweeping it would take an administrator's own token with
// no way to tell that it had.
func (c *Client) RevokeTokens(ctx context.Context, userID int) error {
	tokens, err := c.Tokens(ctx, userID)
	if err != nil {
		return err
	}
	var failures []string
	for _, t := range tokens {
		if err := c.RevokeToken(ctx, t.ID); err != nil {
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

// GroupMembers lists a group's members.
//
// The enumeration decommission targets from: an account is "managed" only
// if it is in the group this company provisions into, so a service account
// somebody else made elsewhere on the instance is never a candidate.
func (c *Client) GroupMembers(ctx context.Context, groupID int) ([]User, error) {
	var out []User
	err := c.get(ctx, "/groups/"+strconv.Itoa(groupID)+"/members",
		url.Values{"per_page": {"100"}}, &out)
	return out, err
}

// DeleteServiceAccount removes a group service account.
//
// The group-scoped route rather than the instance-wide user delete, which
// needs an instance admin — and which would happily remove a person.
func (c *Client) DeleteServiceAccount(ctx context.Context, groupID, userID int) error {
	err := c.send(ctx, http.MethodDelete,
		"/groups/"+strconv.Itoa(groupID)+"/service_accounts/"+strconv.Itoa(userID),
		nil, nil)
	if isNotFound(err) {
		// Unknown or already removed. Both are the state the caller
		// asked for, and a re-run must not fail on the second.
		return nil
	}
	return err
}

// Hook is a registered webhook.
type Hook struct {
	ID  int    `json:"id"`
	URL string `json:"url"`

	// SigningTokenPresent is the ONLY thing GitLab will say about a hook's
	// signing token: the token itself is never returned, by design. It is
	// what lets a reconcile tell a hook that can verify from one that
	// cannot — the difference between an integration that works and one
	// that has been silently unauthenticated since it was created.
	//
	// Absent on a GitLab older than 19.0, where it decodes as false and a
	// reconcile then sets the token it could not confirm. That is the safe
	// direction: setting a token that was already right costs a write,
	// while skipping one that was missing costs every delivery.
	SigningTokenPresent bool `json:"signing_token_present"`
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

// ProjectHooks lists a project's webhooks.
func (c *Client) ProjectHooks(ctx context.Context, project string) ([]Hook, error) {
	var out []Hook
	err := c.get(ctx, "/projects/"+url.PathEscape(project)+"/hooks", nil, &out)
	return out, err
}

// CreateProjectHook registers a webhook on one project.
//
// The path for an instance whose tier has no group hooks — Premium on
// gitlab.com, absent from Community Edition — where this is the only way a
// hook exists at all. See [config.GroupWebhookMode].
func (c *Client) CreateProjectHook(ctx context.Context, project, target, secret string) (Hook, error) {
	var out Hook
	err := c.send(ctx, http.MethodPost, "/projects/"+url.PathEscape(project)+"/hooks",
		hookBody(target, secret), &out)
	return out, err
}

// UpdateProjectHook re-points an existing project hook.
func (c *Client) UpdateProjectHook(ctx context.Context, project string, hookID int, target, secret string) error {
	return c.send(ctx, http.MethodPut,
		"/projects/"+url.PathEscape(project)+"/hooks/"+strconv.Itoa(hookID),
		hookBody(target, secret), nil)
}

// hookBody is the subscription every crewlet hook carries.
// EVERY EVENT IS STATED, including the ones that are off.
//
// Omitting a field does not mean "off" — it means "whatever this GitLab
// version defaults it to", and `push_events` defaults to TRUE. Sending only
// the four the parser routes therefore subscribed every hook to every push
// on every repository, which the engine answers with a 200 and drops.
// Measured on a real instance: the hook came back `push_events: true` from a
// body that never mentioned push.
//
// So the list below is exhaustive over what GitLab's hook API accepts, and a
// future version that flips a default cannot quietly sign this deployment up
// for traffic nothing reads.
func hookBody(target, secret string) map[string]any {
	body := map[string]any{
		"url": target,
		// THE SIGNING TOKEN, and the field name is the whole feature.
		//
		// GitLab takes two different secrets on a hook and they are not
		// variants of one idea:
		//
		//   signing_token — an HMAC key. GitLab signs every delivery and
		//     sends webhook-id / webhook-timestamp / webhook-signature, so
		//     the receiver can verify the payload was not tampered with.
		//     Must be whsec_<base64> over a 32-byte key; never returned.
		//   token — a bearer string echoed back in plaintext as
		//     X-Gitlab-Token, which GitLab's own docs call "not
		//     recommended" and "weaker".
		//
		// This sent the minted whsec_ key in `token`. GitLab did exactly
		// what it was asked: it never signed, and it echoed a 32-byte HMAC
		// key back in cleartext on every delivery. The engine, verifying
		// signatures, then rejected everything — measured against a live
		// 19.3.0 instance, and misread at the time as GitLab not
		// supporting the scheme. It supports it from 19.1; the hook was
		// asked for the other one.
		"signing_token": secret,
		// AND THE PLAINTEXT FIELD IS EXPLICITLY CLEARED.
		//
		// Not merely "no longer set": a hook an older Crewlet created holds
		// the signing key in `token`, and an update that only writes
		// `signing_token` leaves it there — so GitLab goes on echoing a
		// live HMAC key in cleartext on every delivery, for ever, from a
		// hook that now also signs correctly and therefore never looks
		// wrong again.
		//
		// Sending the empty string is what removes it. Omitting the field
		// means "leave whatever is there", which is exactly the state that
		// needs clearing.
		"token": "",
		// TLS verification stays ON. A provisioner that turned it off to
		// make a self-signed development instance work would leave it off
		// in production, where the hook carries a signing secret.
		"enable_ssl_verification": true,
	}
	for _, event := range hookEvents {
		body[event] = routedEvents[event]
	}
	return body
}

// hookEvents is every subscription GitLab's hook API takes.
//
// Read off a real instance (19.3.0-ee) rather than off the reference docs,
// because the point of the list is that nothing is left to a default and a
// doc that lags the API would leave exactly the gap this exists to close. A
// name a given version does not know is ignored, so listing one that arrived
// later costs nothing and omitting one costs a subscription nobody chose.
var hookEvents = []string{
	"push_events",
	"tag_push_events",
	"issues_events",
	"confidential_issues_events",
	"merge_requests_events",
	"note_events",
	"confidential_note_events",
	"job_events",
	"pipeline_events",
	"wiki_page_events",
	"deployment_events",
	"feature_flag_events",
	"releases_events",
	"emoji_events",
	"milestone_events",
	"repository_update_events",
	"resource_access_token_events",
	"resource_deploy_token_events",
	"vulnerability_events",
}

// routedEvents are the ones the parser turns into a notification. Everything
// else in [hookEvents] is registered OFF.
//
// Emoji is the near miss and is deliberately absent: an award names a user
// and a target, but no party to notify — see the parser's own note.
var routedEvents = map[string]bool{
	"issues_events":         true,
	"merge_requests_events": true,
	"note_events":           true,
	"pipeline_events":       true,
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
