package gitlab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
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
		// THROUGH [httpx.ReadRefusal] like the client's own read arm: a
		// raw body here put a proxy's HTML page into the error, cut at
		// 2 KiB with nothing to say so.
		return &APIError{
			Method: method, Path: path, Status: resp.StatusCode,
			Detail: httpx.ReadRefusal(resp),
		}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, httpx.MaxResponseBody)).Decode(out); err != nil {
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
// The same accessor every integration client exports, for the same reason: a
// caller deciding what a refusal MEANS needs the number, and the meaning is
// decided once, in [integration.Reject], rather than per integration.
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

const (
	// userPageSize is one page of an account listing. GitLab's own
	// ceiling for `per_page` is 100 and a larger value is silently
	// clamped, which would make a partial walk look complete.
	userPageSize = 100

	// userWalkCeiling bounds a full account walk. Every page is a request
	// against an instance an operator is waiting on, and an instance with
	// more managed service accounts than this is not one a single company
	// config describes — it is a prefix matching things it should not.
	userWalkCeiling = 5000
)

// User is a GitLab account as a provisioning run needs it.
type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	// State is GitLab's own word for whether the account may sign in —
	// active, blocked, deactivated, ldap_blocked, banned. It is served on
	// the listing and it is the only thing that distinguishes an account
	// this engine can mint into from one it must only report on: see
	// [User.Blocked].
	State string `json:"state"`

	// Bot is GitLab's mark of an account no person signs in to: a service
	// account, a project or group bot. GitLab serves it on the single-user
	// read ([Client.UserByID]); a listing omits it and decodes it as false.
	Bot bool `json:"bot"`
}

// Blocked reports an account GitLab will not let sign in.
//
// ANYTHING BUT active, rather than an enumeration of the states that mean
// no. GitLab has five and has added to them across versions — blocked,
// deactivated, ldap_blocked, banned — and each one refuses every token the
// account holds; a list would go stale silently, minting into whichever
// state was added last.
//
// AN EMPTY STATE IS NOT BLOCKED, which is the other half. The field is
// absent from some older listings and from the service-account creation
// response, and reading absence as "cannot sign in" would report every seat
// on such an instance as failed while provisioning nothing.
func (u User) Blocked() bool {
	return strings.TrimSpace(u.State) != "" && !strings.EqualFold(u.State, "active")
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
// NO EMAIL IS SENT, and the field is not a parameter: see the note in
// provision.go. A custom address is refused until it is confirmed, and every
// address this tool could derive is undeliverable by construction.
func (c *Client) CreateServiceAccount(ctx context.Context, groupID int, name, username string) (User, error) {
	var out User
	err := c.send(ctx, http.MethodPost,
		"/groups/"+strconv.Itoa(groupID)+"/service_accounts",
		map[string]string{"name": name, "username": username}, &out)
	return out, err
}

// CreateInstanceServiceAccount creates a service account that belongs to the
// INSTANCE rather than to a group.
//
// The self-managed path, and the difference is ownership rather than
// capability: the account is not a member of anything until this run adds it,
// so it can hold seats for several top-level groups and it survives a group
// being deleted. The cost is that it needs an INSTANCE ADMIN token — the
// group route is satisfied by a group Owner — and that nothing scopes a
// decommission sweep except the username prefix and the fact that the
// instance service-account listing holds no people.
// It sends no email either, for the reason [Client.CreateServiceAccount]
// gives: the confirmation rule is the account's, not the route's.
func (c *Client) CreateInstanceServiceAccount(ctx context.Context, name, username string) (User, error) {
	var out User
	err := c.send(ctx, http.MethodPost, "/service_accounts",
		map[string]string{"name": name, "username": username}, &out)
	return out, err
}

// ServiceAccounts lists the service accounts one owner holds: the group's
// (`GET /groups/:id/service_accounts`, an Owner's read) where groupID is
// set, the instance's (`GET /service_accounts`, an administrator's) where it
// is zero. Rows carry id, username and name.
//
// PAGED TO EXHAUSTION, like [Client.members]: an account past a truncated
// first page would read as not this owner's.
func (c *Client) ServiceAccounts(ctx context.Context, groupID int) ([]User, error) {
	path, owner := "/service_accounts", "this instance"
	if groupID != 0 {
		path, owner = "/groups/"+strconv.Itoa(groupID)+"/service_accounts", fmt.Sprintf("group %d", groupID)
	}
	var out []User
	for page := 1; ; page++ {
		var batch []User
		err := c.get(ctx, path, url.Values{
			"order_by": {"id"}, "sort": {"asc"},
			"per_page": {strconv.Itoa(userPageSize)},
			"page":     {strconv.Itoa(page)},
		}, &batch)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < userPageSize {
			return out, nil
		}
		if len(out) >= userWalkCeiling {
			return nil, fmt.Errorf(
				"gitlab: %s holds more than %d service accounts, which is not an "+
					"owner Crewlet provisions into: set integrations.gitlab.provisioning.group "+
					"to a group of the company's own (mode: group)", owner, userWalkCeiling)
		}
	}
}

// DeleteInstanceServiceAccount removes an instance service account.
//
// The symmetric route to the create, not the instance-wide user delete: the
// user route would remove a PERSON just as readily, and the only thing
// standing between a mistyped id and somebody's account would be this
// caller's own care.
func (c *Client) DeleteInstanceServiceAccount(ctx context.Context, userID int) error {
	err := c.send(ctx, http.MethodDelete,
		"/service_accounts/"+strconv.Itoa(userID), nil, nil)
	if isNotFound(err) {
		// Already gone, which is what a re-run finds.
		return nil
	}
	return err
}

// Group is a GitLab group.
type Group struct {
	ID       int    `json:"id"`
	FullPath string `json:"full_path"`

	// Plan is the subscription tier this group is on, and gitlab.com is the
	// only deployment that answers it: a self-managed instance sends no
	// such field, so an empty value there means "not said" rather than
	// "free". See [PaidPlan].
	Plan string `json:"plan"`
}

// Tier is what a deployment says about a group's subscription, and it is
// THREE-VALUED because "cannot tell" must not collapse into "paid" or "free".
//
// A self-managed instance answers with no plan at all, and reading silence as
// free would push it down a fallback it does not need. But GET /groups/:path
// on gitlab.com also omits `plan` for a FREE group, so silence read as paid
// takes the group hook that GitLab accepts and never delivers. Keeping the
// answers apart lets a caller ask elsewhere before concluding: [Client.TierOf].
type Tier string

const (
	// TierPaid serves the Premium features, group webhooks among them.
	TierPaid Tier = "paid"
	// TierFree is a deployment saying, in so many words, that group
	// webhooks here are accepted and never delivered.
	TierFree Tier = "free"
	// TierUnknown is nothing said. Treated as paid by every caller, which
	// is the self-managed default rather than a guess about gitlab.com.
	TierUnknown Tier = "unknown"
)

// tierOf reads one plan string, or [TierUnknown] where there is none.
func tierOf(plan string) Tier {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "":
		return TierUnknown
	case "free", "default":
		return TierFree
	default:
		return TierPaid
	}
}

// Tier is what the GROUP record says, which on gitlab.com is frequently
// nothing at all. See [Client.TierOf] for the answer a caller should act on.
func (g Group) Tier() Tier { return tierOf(g.Plan) }

// Namespace is the account a group belongs to, and the only place a
// gitlab.com group's tier can be relied on to appear.
//
// GET /groups/:path omits `plan` on the free tier — measured on a live
// gitlab.com group whose namespace answered `plan: "free"` for the same
// path in the same second. The group record is not lying; it simply does
// not carry the field unless the subscription is one, and "absent" is
// indistinguishable there from the self-managed silence that must keep
// reading as paid.
type Namespace struct {
	ID       int    `json:"id"`
	FullPath string `json:"full_path"`
	Plan     string `json:"plan"`
}

// NamespaceByPath resolves a namespace by its path.
func (c *Client) NamespaceByPath(ctx context.Context, path string) (Namespace, bool, error) {
	var out Namespace
	err := c.get(ctx, "/namespaces/"+url.PathEscape(path), nil, &out)
	if isNotFound(err) {
		return Namespace{}, false, nil
	}
	if err != nil {
		return Namespace{}, false, err
	}
	return out, true, nil
}

// TierOf is the subscription tier to ACT on, asked of both places that know.
//
// The group record is asked first: the caller already holds it and it is right
// whenever it answers. Only its silence costs a second request, and only on
// gitlab.com does that say anything; a self-managed instance stays
// [TierUnknown], which every caller reads as paid.
//
// A NAMESPACE THAT CANNOT BE READ IS NOT FREE. The error is returned, not
// folded into an answer: concluding "free" from a failed request would move a
// working group hook to per-project hooks over a blip, and back on the next
// pass, rewriting somebody's webhooks on a timer.
func (c *Client) TierOf(ctx context.Context, group Group) (Tier, error) {
	if t := group.Tier(); t != TierUnknown {
		return t, nil
	}
	path := strings.TrimSpace(group.FullPath)
	if path == "" {
		return TierUnknown, nil
	}
	ns, found, err := c.NamespaceByPath(ctx, path)
	if err != nil {
		return TierUnknown, fmt.Errorf("gitlab: read the tier of %s: %w", path, err)
	}
	if !found {
		return TierUnknown, nil
	}
	return tierOf(ns.Plan), nil
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
//
// A LAST RESORT RATHER THAN THE PASS'S FIRST MOVE. Per-seat access reads
// [Client.GroupMembers] once per pass and only calls this for an account the
// group does not have. The 409 arm below is what remains: two nodes
// reconciling one surface can both find the membership absent, and the loser
// must not fail on what the winner did.
func (c *Client) AddGroupMember(ctx context.Context, groupID, userID int, access Access) error {
	err := c.send(ctx, http.MethodPost, "/groups/"+strconv.Itoa(groupID)+"/members",
		access.addBody(userID), nil)
	if isConflict(err) {
		// ALREADY A MEMBER is success. The run is a reconcile, and a
		// second run must not fail on what the first one did.
		return nil
	}
	return err
}

// AddProjectMember adds an account to a project at an access level.
//
// The same race guard as [Client.AddGroupMember], reached the same way: only
// for an account [Client.ProjectMembers] did not report.
func (c *Client) AddProjectMember(ctx context.Context, project string, userID int, access Access) error {
	err := c.send(ctx, http.MethodPost,
		"/projects/"+url.PathEscape(project)+"/members",
		access.addBody(userID), nil)
	if isConflict(err) {
		return nil
	}
	return err
}

// SetGroupMember moves an existing membership to an access level.
//
// A second POST to /members answers 409 and changes nothing, and the add above
// treats that as success, so only this call changes an existing seat's role;
// without it the company document says maintainer, the instance keeps
// developer, and every pass reports converged.
//
// PUT is the route GitLab serves for an edit, and it 404s for a user with no
// DIRECT membership, so the caller decides from the listing rather than trying
// this first and falling back.
func (c *Client) SetGroupMember(ctx context.Context, groupID, userID int, access Access) error {
	return c.send(ctx, http.MethodPut,
		"/groups/"+strconv.Itoa(groupID)+"/members/"+strconv.Itoa(userID),
		access.editBody(), nil)
}

// SetProjectMember moves an existing project membership to an access level.
//
// The project counterpart of [Client.SetGroupMember], and it carried the same
// silent drift for the same reason.
func (c *Client) SetProjectMember(ctx context.Context, project string, userID int, access Access) error {
	return c.send(ctx, http.MethodPut,
		"/projects/"+url.PathEscape(project)+"/members/"+strconv.Itoa(userID),
		access.editBody(), nil)
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
	// Read for a FORECAST rather than for a decision: whether a token
	// still authenticates is answered by USING it, not by comparing this
	// to a clock. A calendar check here would be a second opinion that
	// can disagree with the instance — over a timezone, a grace period,
	// or a token revoked before its date. What it does answer is the
	// question using the token cannot: how long it will GO ON working,
	// which is what [Result.CredentialExpires] warns about.
	ExpiresAt Date `json:"expires_at"`
	// Scopes are what the token may do, as GitLab names them (`api`,
	// `read_api`). Read where a token is CHECKED before it is stored: a
	// token without the scope its field needs authenticates and then is
	// refused on the first call that matters.
	Scopes []string `json:"scopes"`
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
		//nolint:nilerr // Deliberate: see the paragraph above.
		return nil
	}
	d.Time = parsed
	return nil
}

// SelfToken is the access token this client authenticates with, as GitLab
// describes it — never its value.
//
// `/personal_access_tokens/self` answers for a personal, group or project
// access token on GitLab 15.5 and later, and refuses anything else (an OAuth
// token, a job token, an older instance) with a 4xx the caller reads as "no
// date to warn about" rather than as a fault.
func (c *Client) SelfToken(ctx context.Context) (Token, error) {
	var out Token
	if err := c.get(ctx, "/personal_access_tokens/self", nil, &out); err != nil {
		return Token{}, err
	}
	return out, nil
}

// tokenPath is where an account's tokens live, and it follows who owns the
// account because the PERMISSION does.
//
// THE INSTANCE ROUTES ARE ADMIN ONLY: listing another account's tokens,
// minting one, and revoking one. Nobody is an instance admin on gitlab.com, so
// a group Owner is refused at three points: the mint 403s, the list 401s on the
// next pass, and the revoke fails inside a rollback, leaving a live credential.
//
// The group routes are what a group Owner may call. So groupID decides:
// non-zero addresses the group that owns the account, zero the instance (the
// self-managed path, where the credential IS an admin token).
func tokenPath(groupID, userID int) string {
	if groupID != 0 {
		return "/groups/" + strconv.Itoa(groupID) + "/service_accounts/" +
			strconv.Itoa(userID) + "/personal_access_tokens"
	}
	return "/users/" + strconv.Itoa(userID) + "/personal_access_tokens"
}

// Tokens lists an account's personal access tokens.
//
// THROUGH THE GROUP where one owns the account: `/personal_access_tokens` is
// an admin listing and answers a group Owner 401. See [tokenPath].
//
// PAGED TO EXHAUSTION, like [Client.members]: one default page is TWENTY rows.
// The vendor's retire and revoke steps decide from this listing, so a
// truncated read leaves a credential live while the run says it cleaned up.
//
// The ceiling is a runaway guard, not a policy limit, so it is generous and an
// error rather than a truncation: a managed account holds one token, a badly
// broken one hundreds.
func (c *Client) Tokens(ctx context.Context, groupID, userID int) ([]Token, error) {
	path, params := "/personal_access_tokens", url.Values{"user_id": {strconv.Itoa(userID)}}
	if groupID != 0 {
		path, params = tokenPath(groupID, userID), url.Values{}
	}
	var out []Token
	for page := 1; ; page++ {
		params.Set("per_page", strconv.Itoa(userPageSize))
		params.Set("page", strconv.Itoa(page))
		var batch []Token
		if err := c.get(ctx, path, params, &batch); err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < userPageSize {
			return out, nil
		}
		if len(out) >= tokenWalkCeiling {
			return nil, fmt.Errorf(
				"gitlab: user %d holds more than %d access tokens, which is not "+
					"an account Crewlet can reason about — revoke them at GitLab",
				userID, tokenWalkCeiling)
		}
	}
}

// tokenWalkCeiling bounds a token walk at twenty pages.
//
// NOT A LIMIT ANYONE SHOULD REACH: an account this tool manages holds one
// token, and the worst real account seen held 164. It is here so that a server
// answering a full page for ever — a paging parameter it ignores, a proxy
// replaying one response — ends the walk instead of the process.
const tokenWalkCeiling = 2000

// CreateToken mints a personal access token for a service account.
//
// The route follows who owns the account, as in [tokenPath]: the instance route
// is admin only, so a group Owner minting through it gets a 403 on every seat.
// groupID non-zero mints through the owning group, zero through the instance.
//
// GitLab returns the value ONCE, which is why the sink is written through
// rather than batched: between minting and recording, the only copy of a live
// credential is in this process's memory.
func (c *Client) CreateToken(
	ctx context.Context, groupID, userID int, name string, scopes []string, expiry time.Time,
) (Token, error) {
	body := map[string]any{"name": name, "scopes": scopes}
	if !expiry.IsZero() {
		body["expires_at"] = expiry.UTC().Format(time.DateOnly)
	}
	var out Token
	err := c.send(ctx, http.MethodPost, tokenPath(groupID, userID), body, &out)
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
func (c *Client) RevokeToken(ctx context.Context, groupID, userID, tokenID int) error {
	path := "/personal_access_tokens/" + strconv.Itoa(tokenID)
	if groupID != 0 {
		path = tokenPath(groupID, userID) + "/" + strconv.Itoa(tokenID)
	}
	err := c.send(ctx, http.MethodDelete, path, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// Member is an account's membership of a group or a project.
//
// # The access level is the field, and it is why this is not a [User]
//
// A membership is not an account: the same account holds different levels in
// different places, and "at what level" is the only question a reconcile can
// ask that tells a converged membership from one that has drifted. Decoding
// only the account left this package able to see THAT a seat was a member and
// never AT WHAT, so the pass had nothing to compare against and re-added
// every seat on every run for ever.
type Member struct {
	User
	// AccessLevel is GitLab's numeric role (see [Roles]). Zero means the
	// listing did not say, which no GitLab version does; it compares unequal
	// to every configured level, so the safe direction is a write.
	AccessLevel int `json:"access_level"`

	// MemberRole is the custom role the membership holds on Ultimate, or
	// nil for a default role. GitLab serves it as an object on the listing;
	// only its id is compared.
	MemberRole *MemberRoleRef `json:"member_role"`
}

// MemberRoleRef is the custom role a membership names.
type MemberRoleRef struct {
	ID int `json:"id"`
}

// Access is what a membership holds: GitLab's default role by level, or a
// custom role (Ultimate) with the level it is based on.
func (m Member) Access() Access {
	access := Access{Level: m.AccessLevel}
	if m.MemberRole != nil {
		access.MemberRoleID = m.MemberRole.ID
	}
	return access
}

// GroupMembers lists a group's members.
//
// Per-seat access compares against it once per pass before changing anybody.
//
// DIRECT MEMBERS, not `/members/all`: the pass writes and can edit only direct
// memberships (GitLab refuses a PUT for one inherited from a parent group), so
// the inherited listing would report a seat converged at a level this pass
// cannot change.
//
// PAGED TO EXHAUSTION: a truncated listing hides every account past the first
// page, and a membership the pass should remove stays live with the run
// reporting success.
func (c *Client) GroupMembers(ctx context.Context, groupID int) ([]Member, error) {
	return c.members(ctx, "/groups/"+strconv.Itoa(groupID)+"/members",
		fmt.Sprintf("group %d", groupID))
}

// ProjectMembers lists a project's members.
//
// The counterpart of [Client.GroupMembers] and read for the same reason: it
// is what the seat loop compares against before it adds anybody to a
// `provisioning.projects` entry. Direct members only, for the reason stated
// there — and it matters more here, because every seat is a member of the
// group that contains these projects and the inherited listing would
// therefore report every one of them as already converged.
func (c *Client) ProjectMembers(ctx context.Context, project string) ([]Member, error) {
	return c.members(ctx, "/projects/"+url.PathEscape(project)+"/members", project)
}

// members walks one membership listing to exhaustion.
//
// ONE WALK for both surfaces rather than two copies of the paging: the
// truncation bug the group listing carried is exactly the one a second
// hand-written loop would reintroduce, and the two differ only in the path.
func (c *Client) members(ctx context.Context, path, subject string) ([]Member, error) {
	var out []Member
	for page := 1; ; page++ {
		var batch []Member
		err := c.get(ctx, path, url.Values{
			"per_page": {strconv.Itoa(userPageSize)},
			"page":     {strconv.Itoa(page)},
		}, &batch)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < userPageSize {
			return out, nil
		}
		if len(out) >= userWalkCeiling {
			return nil, fmt.Errorf(
				"gitlab: %s has more than %d members, which is not a "+
					"%s Crewlet provisions into", subject, userWalkCeiling,
				kindOfMembership(path))
		}
	}
}

// kindOfMembership names what a walk gave up on, for its own error.
func kindOfMembership(path string) string {
	if strings.HasPrefix(path, "/groups/") {
		return "group"
	}
	return "project"
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

// Hook is a registered webhook, as GitLab's listing serves it.
//
// DECODE-ONLY, and deliberately: [Events] is assembled from the flat event
// booleans GitLab sends rather than from a nested object, so this type does
// not round-trip through [encoding/json.Marshal] and nothing here marshals
// one. What goes the other way is [hookBody], which is the single statement
// of what a crewlet hook should be.
type Hook struct {
	ID  int    `json:"id"`
	URL string `json:"url"`

	// Name is what says a hook is THIS deployment's, and it is the only
	// field that survives a change of public base.
	//
	// GitLab has taken a name on a group or project hook since 17.1, and a
	// signing token needs 19.1 (see [Hook.SigningTokenPresent]), so every
	// version that can serve this integration stores it. An older instance
	// ignores the name and answers 200; [confirmHook] reads the hook back
	// and refuses it, removing a hook it has just created, because a
	// nameless hook is one no later pass can recognise as this
	// deployment's. `null` — a hook somebody registered without a name —
	// decodes to "", which is never this deployment's.
	Name string `json:"name"`

	// Description is where this engine records WHICH signing key the hook
	// holds, because GitLab will not say.
	//
	// It is a digest, never the key: [HookDigest] over the secret, one-way and
	// truncated. It gives the fact [SigningTokenPresent] cannot: whether the
	// hook's key is the one the fleet holds. Without it, a secret rotated
	// outside a reconcile run looks converged, GitLab keeps signing with the
	// old key, and the engine refuses every delivery.
	//
	// Empty where somebody cleared it at GitLab: it compares unequal and the
	// hook is re-keyed once, the right direction.
	Description string `json:"description"`

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

	// EnableSSLVerification is whether GitLab checks this deployment's
	// certificate before delivering. Read back because it is part of what
	// [hookBody] asserts, and a hook somebody turned it off on carries a
	// signing secret over a connection nothing authenticates.
	EnableSSLVerification bool `json:"enable_ssl_verification"`

	// Events is which subscriptions the hook actually holds, keyed by the
	// names in [hookEvents].
	//
	// A MAP RATHER THAN NINETEEN FIELDS, so the list of event names exists
	// once — beside the body that writes them — and a version that adds a
	// twentieth is one line in [hookEvents] rather than two places to keep
	// in step. A name this hook's GitLab did not send is absent and reads
	// as off, which is the safe direction on both sides: an event the
	// engine routes then compares unequal and the hook is re-written,
	// while one it does not route compares equal and nothing is.
	Events map[string]bool
}

// UnmarshalJSON reads a hook, folding GitLab's flat event booleans into
// [Hook.Events].
//
// Two passes over the same bytes rather than a struct with nineteen tagged
// fields: the second pass is keyed on [hookEvents], which is the same list
// [hookBody] writes from, so the two cannot drift into a reconcile that
// writes a subscription it then cannot see.
func (h *Hook) UnmarshalJSON(raw []byte) error {
	// An alias to borrow the field tags without recursing into this method.
	type hook Hook
	var row hook
	if err := json.Unmarshal(raw, &row); err != nil {
		return err
	}
	var flags map[string]json.RawMessage
	if err := json.Unmarshal(raw, &flags); err != nil {
		return err
	}
	*h = Hook(row)
	h.Events = nil
	for _, event := range hookEvents {
		encoded, present := flags[event]
		if !present {
			continue
		}
		var on bool
		if err := json.Unmarshal(encoded, &on); err != nil {
			// NOT A FAILURE OF THE WHOLE LISTING. A version that answers
			// something other than a bool here leaves the flag absent,
			// which reads as off — and off against a routed event is a
			// difference, so the hook is re-written rather than trusted.
			continue
		}
		if h.Events == nil {
			h.Events = make(map[string]bool, len(hookEvents))
		}
		h.Events[event] = on
	}
	return nil
}

// Converged reports a hook that already carries what [hookBody] would write,
// so writing it again would change nothing at the instance.
//
// THE NAME, ADDRESS AND KEY'S DIGEST ARE PART OF IT, because [hookBody] writes
// all three: a hook this pass must re-point would otherwise answer "converged".
//
// GitLab never returns a hook's `signing_token`, so the key is compared by
// digest: [hookBody] writes [HookDigest] of the secret into the description,
// the one field this engine controls and GitLab gives back. Checking only that
// the hook holds SOME token misses a rotation made by another process
// (`crewlet secrets set`, a peer's apply): the hook keeps the old key and every
// delivery is refused. A digest reads the same on every node.
func (h Hook) Converged(name, target, digest string) bool {
	if h.Name != name || h.URL != target || h.Description != digest {
		return false
	}
	if !h.SigningTokenPresent || !h.EnableSSLVerification {
		return false
	}
	for _, event := range hookEvents {
		if h.Events[event] != routedEvents[event] {
			return false
		}
	}
	return true
}

// GroupHooks lists a group's webhooks, PAGED TO EXHAUSTION.
//
// For the reason [Client.Tokens] is: a
// truncated listing is not a slow report, it is a wrong DECISION. Every hook
// choice this package makes is made out of this listing — [ours] selects this
// deployment's hooks from it, [ensureGroupHook] decides converged-or-create
// and deletes the extras from it, and [sweepGroupHooks] and the teardown's
// removeHooks delete from it. Unpaged it returned GitLab's default first page
// of twenty, so on a container already carrying twenty hooks this engine's own
// sat past the boundary, was invisible, and every pass registered another —
// which is the duplicate-hook failure the name matching exists to stop, with
// the disconnect reporting success and leaving the real hook live and signed.
func (c *Client) GroupHooks(ctx context.Context, groupID int) ([]Hook, error) {
	return hookPages(ctx, c, "/groups/"+strconv.Itoa(groupID)+"/hooks")
}

// GroupProjects is every project the group holds, subgroups included, as
// `<namespace>/<path>`, the spelling `provisioning.projects` uses.
//
// A teardown needs it, a reconcile does not. A disconnect must remove every
// hook this engine registered ANYWHERE, and the config is no record of where:
// a project dropped from `provisioning.projects` leaves hooks pointing at dead
// tunnels, and a `trycloudflare` hostname is re-issued to whoever asks next.
// A reconcile walking it would make a thousand-project group a thousand hook
// listings per tick. PAGED TO EXHAUSTION: a truncated listing means a project
// silently never swept.
func (c *Client) GroupProjects(ctx context.Context, groupID int) ([]string, error) {
	var out []string
	path := "/groups/" + strconv.Itoa(groupID) + "/projects"
	for page := 1; ; page++ {
		var batch []struct {
			PathWithNamespace string `json:"path_with_namespace"`
		}
		err := c.get(ctx, path, url.Values{
			"per_page":          {strconv.Itoa(userPageSize)},
			"page":              {strconv.Itoa(page)},
			"include_subgroups": {"true"},
			// ARCHIVED ONES TOO, and `true` is how GitLab spells that.
			//
			// NOT A FILTER, though it reads like one: GitLab's
			// ProjectsFinder#by_archived returns the collection UNFILTERED
			// for a truthy value; only the string `only` narrows to
			// archived. Verified against gitlab-org/gitlab.
			//
			// Sent explicitly although omitting it is documented to do the
			// same: an archived project keeps its webhooks, this pass must
			// see all of them, and a vendor default can change.
			"archived": {"true"},
			// Only the path is read, so the cheap listing form suffices.
			"simple": {"true"},
		}, &batch)
		if err != nil {
			return nil, err
		}
		for _, row := range batch {
			if path := strings.TrimSpace(row.PathWithNamespace); path != "" {
				out = append(out, path)
			}
		}
		if len(batch) < userPageSize {
			return out, nil
		}
		if len(out) >= projectWalkCeiling {
			return nil, fmt.Errorf(
				"gitlab: this group holds more than %d projects, which is not "+
					"a group Crewlet can sweep in one disconnect — remove the "+
					"hooks pointing at this deployment by hand",
				projectWalkCeiling)
		}
	}
}

// projectWalkCeiling bounds [Client.GroupProjects].
//
// Same shape and the same reason as [userWalkCeiling], and a smaller number
// because each project costs a HOOK LISTING afterwards rather than one row in
// a page: two thousand projects is two thousand round trips inside one
// disconnect, which is the point at which finishing by hand is faster than
// waiting. Above it the teardown says so rather than grinding.
const projectWalkCeiling = 2000

// CreateGroupHook registers a webhook on a group.
//
// EVERY EVENT THE PARSER UNDERSTANDS, and no more: a hook subscribed to
// something nothing routes is delivery this engine answers with a 200 and
// drops, which looks from the instance's side like a healthy integration.
func (c *Client) CreateGroupHook(ctx context.Context, groupID int, name, target, secret string) (Hook, error) {
	var out Hook
	err := c.send(ctx, http.MethodPost, "/groups/"+strconv.Itoa(groupID)+"/hooks",
		hookBody(name, target, secret), &out)
	return out, err
}

// UpdateGroupHook re-points an existing hook, which is what a rotation of the
// signing secret needs.
func (c *Client) UpdateGroupHook(ctx context.Context, groupID, hookID int, name, target, secret string) error {
	return c.send(ctx, http.MethodPut,
		"/groups/"+strconv.Itoa(groupID)+"/hooks/"+strconv.Itoa(hookID),
		hookBody(name, target, secret), nil)
}

// DeleteGroupHook removes a group hook, which is what a disconnect does with
// the one this engine registered.
func (c *Client) DeleteGroupHook(ctx context.Context, groupID, hookID int) error {
	return c.send(ctx, http.MethodDelete,
		"/groups/"+strconv.Itoa(groupID)+"/hooks/"+strconv.Itoa(hookID), nil, nil)
}

// ProjectHooks lists a project's webhooks, paged for the reason
// [Client.GroupHooks] gives — and this is the likelier of the two to overflow,
// because a project is where CI, chat and scanner integrations all register.
func (c *Client) ProjectHooks(ctx context.Context, project string) ([]Hook, error) {
	return hookPages(ctx, c, "/projects/"+url.PathEscape(project)+"/hooks")
}

// hookPages walks one container's hooks to exhaustion.
//
// NO CEILING, unlike the token and account walks beside it. Those bound a pile
// this engine's own bug created — 164 tokens on one account — where a number
// that large means something is wrong and refusing is safer than acting. A
// container's hooks are other people's integrations, a couple of dozen at
// worst, and refusing to read them would turn a busy project into a surface
// this engine cannot converge at all.
func hookPages(ctx context.Context, c *Client, path string) ([]Hook, error) {
	var out []Hook
	for page := 1; ; page++ {
		var batch []Hook
		err := c.get(ctx, path, url.Values{
			"per_page": {strconv.Itoa(userPageSize)},
			"page":     {strconv.Itoa(page)},
		}, &batch)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < userPageSize {
			return out, nil
		}
	}
}

// CreateProjectHook registers a webhook on one project.
//
// The path for an instance whose tier has no group hooks — Premium on
// gitlab.com, absent from Community Edition — where this is the only way a
// hook exists at all. See [config.ContainerWebhookMode].
func (c *Client) CreateProjectHook(ctx context.Context, project, name, target, secret string) (Hook, error) {
	var out Hook
	err := c.send(ctx, http.MethodPost, "/projects/"+url.PathEscape(project)+"/hooks",
		hookBody(name, target, secret), &out)
	return out, err
}

// DeleteProjectHook removes a project hook.
func (c *Client) DeleteProjectHook(ctx context.Context, project string, hookID int) error {
	return c.send(ctx, http.MethodDelete,
		"/projects/"+url.PathEscape(project)+"/hooks/"+strconv.Itoa(hookID), nil, nil)
}

// UpdateProjectHook re-points an existing project hook.
func (c *Client) UpdateProjectHook(
	ctx context.Context, project string, hookID int, name, target, secret string,
) error {
	return c.send(ctx, http.MethodPut,
		"/projects/"+url.PathEscape(project)+"/hooks/"+strconv.Itoa(hookID),
		hookBody(name, target, secret), nil)
}

// hookBody is the subscription every crewlet hook carries.
// EVERY EVENT IS STATED, including the ones that are off.
//
// Omitting a field means "whatever this GitLab version defaults it to", and
// `push_events` defaults to TRUE: sending only the events the parser routes
// subscribed every hook to every push, which the engine answers 200 and drops.
// So the list is exhaustive over what GitLab's hook API accepts, and a version
// that flips a default cannot sign this deployment up for traffic nothing reads.
func hookBody(name, target, secret string) map[string]any {
	body := map[string]any{
		"url": target,
		// THE NAME IS THE IDENTITY, and it is what a moved public base
		// leaves intact. See [Hook.Name] and [ours]: matching on the URL
		// alone made every change of address create a hook and abandon the
		// one before it.
		"name": name,
		// AND WHICH KEY THIS HOOK HOLDS, which GitLab answers about in no
		// other way. See [Hook.Description] and [HookDigest].
		"description": HookDigest(secret),
		// THE SIGNING TOKEN, and the field name is the whole feature.
		//
		// GitLab takes two unrelated secrets on a hook. `signing_token` is a
		// whsec_<base64> HMAC key over 32 bytes, never returned: GitLab signs
		// each delivery (webhook-signature), supported from 19.1. `token` is
		// a bearer string echoed in plaintext as X-Gitlab-Token, which GitLab
		// calls "weaker". The key goes in `signing_token` and never in
		// `token`: there GitLab never signs and echoes the HMAC key in
		// cleartext on every delivery, which the engine then rejects.
		"signing_token": secret,
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
//
// errors.As rather than a type assertion: every caller in this package wraps
// the client's error with the operation it was doing, so a bare assertion
// found the APIError only on the one path that had not wrapped yet — and a
// 404 that stopped being recognised reads as a hard failure of a reconcile
// that should simply have created the thing.
func asAPIError(err error, target **APIError) bool {
	return errors.As(err, target)
}

// HookDigest is how a hook records which signing key it was written with.
//
// GitLab only says whether a hook has a signing token, so which key it holds is
// stored in the description, the one field GitLab returns. See [Hook.Converged].
//
// It is a HASH, never the key: SHA-256 over a domain string and the secret, cut
// to 48 bits. Enough to notice a change, useless against a 32-byte secret. THE
// DOMAIN STRING IS NOT DECORATION: it stops a match with a digest of the same
// bytes made for another purpose, as internal/runtoken does.
//
// An empty secret digests to "", not to the hash of the empty string: a hook
// whose key this run does not know must not be reported as carrying it.
func HookDigest(secret string) string {
	if strings.TrimSpace(secret) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("crewlet/gitlab/hook-signing-key\x00" + secret))
	return "crewlet:" + hex.EncodeToString(sum[:6])
}
