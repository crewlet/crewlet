package plane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The write half of the Plane client: what a provisioning run needs and the
// engine never calls.
//
// Beside the read client rather than in its own package, for the reason the
// GitLab half states — they share the base URL, the X-API-Key header and the
// error shape, and a second client would be a second place for those to
// drift on the surface where drifting means authenticating as the wrong
// account.

// APIError is a refused call, carrying enough to decide what to do about it.
//
// # The status IS the answer, and here more than anywhere
//
// Plane's capability preflight is a METHOD PROBE: it asks a route that
// exists but refuses the method, and reads 405 as "present", 404 as "absent"
// and 403 as "present, but this credential is not an administrator". Those
// three are one flat string away from indistinguishable, and the string
// differs by fork, by version and by DRF locale.
type APIError struct {
	Method string
	Path   string
	Status int
	Detail string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("plane: %s %s: %d: %s", e.Method, e.Path, e.Status, e.Detail)
}

// Status reads the HTTP status off an error, or 0 where there is none.
//
// Zero for a transport failure, deliberately: "the request never landed" is
// not a status, and a probe that read it as one would decide a capability
// question from a dropped connection.
func Status(err error) int {
	var api *APIError
	if errors.As(err, &api) {
		return api.Status
	}
	return 0
}

// send performs a request with a JSON body, decoding a JSON response.
func (c *Client) send(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("plane: encode %s: %w", path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+APIPath+path, reader)
	if err != nil {
		return fmt.Errorf("plane: %s: %w", path, err)
	}
	req.Header.Set("X-API-Key", c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("plane: %s: %w", path, err)
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
		return fmt.Errorf("plane: decode %s: %w", path, err)
	}
	return nil
}

// ws builds a workspace-scoped path.
//
// Every resource Plane exposes is under the workspace slug, and the slug is
// operator-supplied — so it is escaped once, here, rather than concatenated
// at two dozen call sites of which one would eventually forget.
func (c *Client) ws(suffix string) string {
	return "/workspaces/" + url.PathEscape(c.workspace) + suffix
}

// ---- identity ---------------------------------------------------------- //

// Account is a Plane user as a provisioning run needs it.
//
// Username is the fork's addition to the member row and the whole basis of
// re-run safety: it is the only field a run can derive from a seat handle
// before the account exists. An id is assigned by the instance and an email
// is editable by the account.
type Account struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	// Role is the workspace role INT — 20 admin, 15 member, 5 guest.
	Role int `json:"role"`
	// IsBot marks a service account. Read for the decommission guard: the
	// run must never touch a human who happens to match a derived name.
	IsBot bool `json:"is_bot"`
	// Token is the plaintext credential, returned by the create call and
	// by nothing else, ever.
	Token string `json:"token"`
}

// Me resolves the calling credential.
//
// The first call any run makes, because it is the one that separates "this
// token is wrong" from every later 403 — which would otherwise all read as a
// missing capability.
func (c *Client) Me(ctx context.Context) (Account, error) {
	var out Account
	err := c.get(ctx, "/users/me/", nil, &out)
	return out, err
}

// Members lists the workspace's members.
//
// ALL OR NOTHING. Every create-or-reuse decision keys off this
// enumeration, so a truncated list would have the run create a second
// account for a seat that already has one — and mint it a credential the
// config's `${VAR}` then holds instead of the live one.
func (c *Client) Members(ctx context.Context) ([]Account, error) {
	var payload json.RawMessage
	if err := c.get(ctx, c.ws("/members/"), nil, &payload); err != nil {
		return nil, err
	}
	var out []Account
	if err := json.Unmarshal(payload, &out); err == nil {
		return out, nil
	}
	// The endpoint serves a bare array on the builds this targets, but a
	// cursor envelope costs one struct to tolerate and the alternative is
	// an empty member list read as "no accounts exist yet".
	var envelope struct {
		Results []Account `json:"results"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("plane: members: %w", err)
	}
	return envelope.Results, nil
}

// ---- service accounts -------------------------------------------------- //

// CreateAccount creates a service account and returns it WITH its first
// token.
//
// # The role is always named
//
// The endpoint defaults to admin when the field is omitted, which is a
// silent privilege escalation for any caller that forgets — so this one
// takes the role as a required argument and never omits the field.
//
// # The username echo has to be checked by the caller
//
// An instance carrying only the upstream service-accounts API ignores a
// caller-chosen username and generates a synthetic `svc_<uuid>` instead.
// The account is created either way, so the failure is not visible here —
// it is visible on the NEXT run, which cannot find the account it made and
// creates another. [Reconcile] compares the echo for exactly that reason.
func (c *Client) CreateAccount(ctx context.Context, username, display string, role int) (Account, error) {
	body := map[string]any{
		"name": display, "role": roleName(role),
		"username": username, "display_name": display,
		"description": "Crewlet agent seat",
	}
	var out Account
	if err := c.send(ctx, http.MethodPost, c.ws("/service-accounts/"), body, &out); err != nil {
		return Account{}, err
	}
	if strings.TrimSpace(out.ID) == "" {
		return Account{}, fmt.Errorf("plane: creating %s returned no account id", username)
	}
	return out, nil
}

// DeleteAccount decommissions a service account.
//
// A cascade on the instance's side: it deactivates every token, drops every
// membership and deactivates the user, keeping the row so past activity
// still attributes. The guard that makes it safe is the instance's, not
// ours — it refuses with 400 for anything that is not a service account.
func (c *Client) DeleteAccount(ctx context.Context, accountID string) error {
	path := c.ws("/service-accounts/" + url.PathEscape(accountID) + "/")
	err := c.send(ctx, http.MethodDelete, path, nil, nil)
	if Status(err) == http.StatusNotFound {
		// Unknown or already decommissioned. Both are the state the
		// caller asked for, and a re-run must not fail on the second.
		return nil
	}
	return err
}

// Token is one of an account's API tokens, as the list endpoint serves it.
//
// Never the value: the plaintext is returned by the mint call alone, so a
// run that fails to persist what it minted cannot go back and read it.
type Token struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Active bool   `json:"is_active"`
	// ExpiresAt is when it dies, or the zero time for never. Read rather
	// than ignored because an expired row can still arrive with
	// is_active true — the flag records revocation, not the calendar —
	// and a run that trusted the flag would leave a seat holding a
	// credential that stopped working at midnight.
	ExpiresAt time.Time `json:"expired_at"`
	// Value is the plaintext, present on a mint response only.
	Value string `json:"token"`
}

// Usable reports a token that would still authenticate at the given instant.
func (t Token) Usable(now time.Time) bool {
	return t.Active && (t.ExpiresAt.IsZero() || t.ExpiresAt.After(now))
}

// Tokens lists an account's tokens.
func (c *Client) Tokens(ctx context.Context, accountID string) ([]Token, error) {
	path := c.ws("/service-accounts/" + url.PathEscape(accountID) + "/tokens/")
	var payload json.RawMessage
	if err := c.get(ctx, path, nil, &payload); err != nil {
		return nil, err
	}
	var out []Token
	if err := json.Unmarshal(payload, &out); err == nil {
		return out, nil
	}
	var envelope struct {
		Results []Token `json:"results"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("plane: tokens: %w", err)
	}
	return envelope.Results, nil
}

// MintToken creates an API token for an account, returning its plaintext.
//
// # A zero expiry OMITS the field
//
// On this endpoint an absent `expired_at` means the token never expires,
// which is what `token_expiry_days: 0` is documented to mean. Sending an
// explicit null means the same thing, so omission is simply the canonical
// spelling — and sending a computed zero-day instant would mean a token that
// expired the moment it was minted.
func (c *Client) MintToken(ctx context.Context, accountID, label string, expiry time.Time) (Token, error) {
	body := map[string]any{"label": label, "description": "Minted by crewlet plane provision"}
	if !expiry.IsZero() {
		body["expired_at"] = expiry.UTC().Format(time.RFC3339)
	}
	path := c.ws("/service-accounts/" + url.PathEscape(accountID) + "/tokens/")
	var out Token
	if err := c.send(ctx, http.MethodPost, path, body, &out); err != nil {
		return Token{}, err
	}
	if strings.TrimSpace(out.Value) == "" {
		// A response with no plaintext is worse than a refusal: the token
		// exists on the instance and nothing can ever read it, so the run
		// must stop rather than record an empty credential.
		return Token{}, fmt.Errorf("plane: minting %q returned no token value", label)
	}
	return out, nil
}

// RevokeToken deactivates one token.
func (c *Client) RevokeToken(ctx context.Context, accountID, tokenID string) error {
	path := c.ws("/service-accounts/" + url.PathEscape(accountID) +
		"/tokens/" + url.PathEscape(tokenID) + "/")
	err := c.send(ctx, http.MethodDelete, path, nil, nil)
	if Status(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// CreateProject creates a project.
//
// The creating credential becomes its admin, which is what lets the same
// run add every seat to it immediately afterwards.
func (c *Client) CreateProject(ctx context.Context, name, identifier string) (Project, error) {
	var out Project
	body := map[string]any{"name": name, "identifier": identifier}
	if err := c.send(ctx, http.MethodPost, c.ws("/projects/"), body, &out); err != nil {
		return Project{}, err
	}
	if strings.TrimSpace(out.ID) == "" {
		return Project{}, fmt.Errorf("plane: creating project %s returned no id", identifier)
	}
	return out, nil
}

// ---- memberships ------------------------------------------------------- //

// AddProjectMember adds an existing workspace member to a project.
//
// # A duplicate is success
//
// Adding a member twice violates a unique constraint the instance maps to a
// generic 400 — generic because any integrity error from that view produces
// the same body. So a 400 or a 409 whose text names a duplicate is read as
// "already a member", and anything else is raised: a run that swallowed
// every 400 would report a seat joined to a project it cannot see.
func (c *Client) AddProjectMember(ctx context.Context, projectID, accountID string, role int) error {
	path := c.ws("/projects/" + url.PathEscape(projectID) + "/members/")
	body := map[string]any{"member": accountID, "role": role}
	err := c.send(ctx, http.MethodPost, path, body, nil)
	if err == nil || isDuplicate(err) {
		return nil
	}
	return err
}

// isDuplicate reports an error that unambiguously names an existing row.
func isDuplicate(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	if api.Status == http.StatusConflict {
		return true
	}
	if api.Status != http.StatusBadRequest {
		return false
	}
	detail := strings.ToLower(api.Detail)
	return strings.Contains(detail, "already exist") || strings.Contains(detail, "duplicate")
}

// ---- webhooks ---------------------------------------------------------- //

// Webhook is a workspace webhook.
//
// SecretKey is present on the create response and on nothing else — not the
// list, not the retrieve, not the update. It is generated by the instance
// and cannot be written, so rotating it means deleting the webhook and
// making another.
type Webhook struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	SecretKey string `json:"secret_key"`
	IsActive  bool   `json:"is_active"`

	Project      bool `json:"project"`
	Issue        bool `json:"issue"`
	Module       bool `json:"module"`
	Cycle        bool `json:"cycle"`
	IssueComment bool `json:"issue_comment"`
	// Page is the fork's addition. An instance without it drops the field
	// silently rather than refusing, which is why the caller checks the
	// echo instead of the status.
	Page bool `json:"page"`
}

// Webhooks lists the workspace's webhooks.
func (c *Client) Webhooks(ctx context.Context) ([]Webhook, error) {
	var out []Webhook
	if err := c.get(ctx, c.ws("/webhooks/"), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateWebhook creates one and returns it WITH its generated secret.
//
// # It returns what it created even when it then refuses it
//
// A response with no secret is a failure — the value is served once and this
// was the once, so nothing could ever verify a delivery from it. But the
// webhook EXISTS by then, and a caller handed a bare error would leave it
// on the workspace signing deliveries with a key nobody holds, which reads
// as a broken integration rather than an absent one. So the hook comes back
// beside the error, for the caller to undo.
func (c *Client) CreateWebhook(ctx context.Context, target string) (Webhook, error) {
	var out Webhook
	if err := c.send(ctx, http.MethodPost, c.ws("/webhooks/"), webhookBody(target), &out); err != nil {
		return Webhook{}, err
	}
	if strings.TrimSpace(out.SecretKey) == "" {
		return out, fmt.Errorf("plane: creating the webhook returned no " +
			"secret — it is served once, at creation, so there is nothing to capture")
	}
	return out, nil
}

// UpdateWebhook reconciles an existing webhook's toggles.
//
// It cannot return the secret and cannot set one. That asymmetry is the
// whole reason the reconcile treats a pre-existing webhook differently from
// one it made: the toggles converge, the secret does not.
func (c *Client) UpdateWebhook(ctx context.Context, id, target string) (Webhook, error) {
	var out Webhook
	path := c.ws("/webhooks/" + url.PathEscape(id) + "/")
	if err := c.send(ctx, http.MethodPatch, path, webhookBody(target), &out); err != nil {
		return Webhook{}, err
	}
	return out, nil
}

// DeleteWebhook removes one.
func (c *Client) DeleteWebhook(ctx context.Context, id string) error {
	err := c.send(ctx, http.MethodDelete, c.ws("/webhooks/"+url.PathEscape(id)+"/"), nil, nil)
	if Status(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// webhookBody is the entity set the engine's parser actually routes.
//
// ISSUE, ISSUE_COMMENT and PAGE, and deliberately not module or cycle: the
// parser routes work items, their comments and pages, and a delivery for an
// entity nothing routes is a signed request the engine verifies, stores and
// then drops. `project` is on because a project's creation is what the
// project cache learns identifiers from.
func webhookBody(target string) map[string]any {
	return map[string]any{
		"url": target, "is_active": true,
		"project": true, "issue": true, "issue_comment": true, "page": true,
		"module": false, "cycle": false,
	}
}

// roleName maps the workspace role int to the string the create endpoint
// takes.
//
// TWO SPELLINGS OF ONE THING, which is the instance's choice rather than
// ours: the account create endpoint takes a string and every membership
// endpoint takes the int. Keeping the int as the canonical form and
// translating at the one call site that needs a word is the direction that
// cannot silently mean `admin` — an unmapped int falls to the least
// privilege, never the most.
func roleName(role int) string {
	switch role {
	case RoleAdmin:
		return "admin"
	case RoleMember:
		return "member"
	default:
		return "guest"
	}
}

// The workspace role ints Plane stores.
const (
	RoleAdmin  = 20
	RoleMember = 15
	RoleGuest  = 5
)
