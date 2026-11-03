package expo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// GraphQLURL is Expo's GraphQL API. Fixed rather than configurable, so a
// provisioner token is only ever sent to Expo.
const GraphQLURL = "https://api.expo.dev/graphql"

// ClientTimeout bounds one call. Every call runs inside a pass with its own
// deadline, so this is slack for a slow answer rather than a budget to spend.
const ClientTimeout = 15 * time.Second

// Rate limiting: Expo answers 429 with `Retry-After`. A wait up to
// rateLimitWaitCap is taken in the call, up to rateLimitRetries times; a
// longer one is returned to the pass, whose loop backs off on its own.
const (
	rateLimitRetries  = 3
	rateLimitWaitCap  = 30 * time.Second
	defaultRetryAfter = 5 * time.Second
)

// maxResponse bounds what one answer may be: a page of robots with their
// tokens is tens of kilobytes, and the cap stops a proxy's page being read
// whole.
const maxResponse = 4 << 20

// Paging: the robot listing asks for robotPageSize members a page and stops
// after robotPageCeiling pages with an error, since a short read would read as
// robots that do not exist.
const (
	robotPageSize    = 50
	robotPageCeiling = 100
)

// Permission is one of Expo's account permissions, spelled as its GraphQL
// `Permission` enum spells it.
type Permission string

// Expo's account permissions.
const (
	PermissionView             Permission = "VIEW"
	PermissionRun              Permission = "RUN"
	PermissionPublish          Permission = "PUBLISH"
	PermissionPublishProtected Permission = "PUBLISH_PROTECTED"
	PermissionAdmin            Permission = "ADMIN"
	PermissionOwn              Permission = "OWN"
)

// Valid reports whether p is a permission Expo's schema defines.
func (p Permission) Valid() bool {
	switch p {
	case PermissionView, PermissionRun, PermissionPublish, PermissionPublishProtected,
		PermissionAdmin, PermissionOwn:
		return true
	default:
		return false
	}
}

// administers reports a permission set that manages the account's robots and
// their tokens: Admin, or ownership, which includes it.
func administers(perms []Permission) bool {
	return slices.Contains(perms, PermissionAdmin) || slices.Contains(perms, PermissionOwn)
}

// APIError is a call Expo refused: an HTTP status of 300 or more, or a 2xx
// whose body carries GraphQL `errors`. It never carries a credential.
type APIError struct {
	// Op is the operation, such as CreateRobot.
	Op string

	// Status is the HTTP status, or for a GraphQL error the status its code
	// stands for ([statusOf]), so [integration.Reject] reads both alike.
	Status int

	// Code is Expo's `extensions.errorCode`, empty where it gave none.
	Code string

	// Detail is Expo's messages as one bounded sentence.
	Detail string

	// RetryAfter is Expo's own wait on a 429, zero otherwise.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	code := ""
	if e.Code != "" {
		code = " " + e.Code
	}
	return fmt.Sprintf("expo: %s: %d%s: %s", e.Op, e.Status, code, e.Detail)
}

// Status reports the status a call was refused with, or 0 when the failure was
// not an API error, so [integration.Reject] can classify it.
func Status(err error) int {
	if api, ok := errors.AsType[*APIError](err); ok {
		return api.Status
	}
	return 0
}

// noDetail is what a refusal that said nothing reads as.
const noDetail = "no detail"

// Client talks to Expo's GraphQL API with one access token.
type Client struct {
	token string
	http  *http.Client
	wait  func(ctx context.Context, d time.Duration) error
}

// ClientOptions builds a client.
type ClientOptions struct {
	// Token is the access token every call carries.
	Token string
	// HTTP overrides the transport, for tests.
	HTTP *http.Client
}

// NewClient builds a client, refusing an empty token, which would fail every
// call with a misleading refusal.
func NewClient(opts ClientOptions) (*Client, error) {
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		return nil, errors.New("expo: no access token")
	}
	hc := opts.HTTP
	if hc == nil {
		hc = httpx.Client(ClientTimeout)
	}
	return &Client{token: token, http: hc, wait: sleep}, nil
}

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// gqlRequest is one GraphQL operation as Expo takes it.
type gqlRequest struct {
	OperationName string         `json:"operationName"`
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables,omitempty"`
}

// gqlError is one entry of a GraphQL answer's `errors`, as far as the engine
// reads it.
type gqlError struct {
	Message    string `json:"message"`
	Extensions struct {
		ErrorCode   string `json:"errorCode"`
		Code        string `json:"code"`
		ErrorType   string `json:"errorType"`
		IsTransient bool   `json:"isTransient"`
	} `json:"extensions"`
}

// code is the error's own code: Expo's errorCode, else the server's code.
func (e gqlError) code() string {
	if e.Extensions.ErrorCode != "" {
		return e.Extensions.ErrorCode
	}
	return e.Extensions.Code
}

// gqlResponse is a GraphQL answer.
type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors"`
}

// statusOf is the HTTP status a GraphQL error stands for, so a refusal reads
// the same whether Expo sent it as a status or as an error code. A transient
// error is a 503 the sync retries; an error Expo attributes to the caller is a
// 400; anything else is a 500 the sync retries.
func statusOf(e gqlError) int {
	switch e.code() {
	case "UNAUTHENTICATED", "UNAUTHENTICATED_ERROR":
		return http.StatusUnauthorized
	case "UNAUTHORIZED_ERROR", "FORBIDDEN":
		return http.StatusForbidden
	case "ENTITY_NOT_FOUND", "NOT_FOUND_ERROR":
		return http.StatusNotFound
	case "TOO_MANY_REQUESTS", "RATE_LIMITED":
		return http.StatusTooManyRequests
	case "GRAPHQL_VALIDATION_FAILED", "GRAPHQL_PARSE_FAILED", "BAD_USER_INPUT", "VALIDATION_ERROR":
		return http.StatusBadRequest
	}
	switch {
	case e.Extensions.IsTransient:
		return http.StatusServiceUnavailable
	case e.Extensions.ErrorType == "USER":
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// do sends one operation and decodes its `data` into out, waiting out a short
// 429 as Expo asks. It is the only place a request is built.
func (c *Client) do(ctx context.Context, op, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(gqlRequest{OperationName: op, Query: query, Variables: vars})
	if err != nil {
		return fmt.Errorf("expo: encode %s: %w", op, err)
	}
	for attempt := 0; ; attempt++ {
		err := c.once(ctx, op, body, out)
		api, ok := errors.AsType[*APIError](err)
		if ok && api.Status == http.StatusTooManyRequests && attempt < rateLimitRetries &&
			api.RetryAfter <= rateLimitWaitCap && fits(ctx, api.RetryAfter) {
			if werr := c.wait(ctx, api.RetryAfter); werr != nil {
				return err
			}
			continue
		}
		return err
	}
}

// fits reports whether a wait ends before ctx's deadline.
func fits(ctx context.Context, d time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) > d
}

// once sends one request and reads its answer.
func (c *Client) once(ctx context.Context, op string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GraphQLURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("expo: build %s: %w", op, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("expo: %s: %w", op, err)
	}
	defer func() { _ = res.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("expo: read %s: %w", op, err)
	}
	var answer gqlResponse
	decodeErr := json.Unmarshal(payload, &answer)
	if res.StatusCode >= http.StatusMultipleChoices || (decodeErr == nil && len(answer.Errors) > 0) {
		api := &APIError{Op: op, Status: res.StatusCode}
		if decodeErr == nil && len(answer.Errors) > 0 {
			api.Code, api.Detail = refusalOf(answer.Errors)
			if res.StatusCode < http.StatusMultipleChoices {
				api.Status = statusOf(answer.Errors[0])
			}
		}
		if api.Detail == "" {
			api.Detail = httpx.Refusal(res.Header.Get("Content-Type"), payload)
		}
		if api.Detail == "" {
			api.Detail = noDetail
		}
		if api.Status == http.StatusTooManyRequests {
			api.RetryAfter = retryAfter(res.Header.Get("Retry-After"))
		}
		return api
	}
	if decodeErr != nil {
		return fmt.Errorf("expo: decode %s: %w", op, decodeErr)
	}
	if len(answer.Data) == 0 || string(answer.Data) == "null" {
		return fmt.Errorf("expo: %s answered no data", op)
	}
	if out != nil {
		if err := json.Unmarshal(answer.Data, out); err != nil {
			return fmt.Errorf("expo: decode %s: %w", op, err)
		}
	}
	return nil
}

// refusalOf is the first error's code and every error's message as one
// bounded sentence.
func refusalOf(errs []gqlError) (string, string) {
	texts := make([]string, 0, len(errs))
	for _, e := range errs {
		if text := strings.TrimSpace(e.Message); text != "" {
			texts = append(texts, text)
		}
	}
	if len(texts) == 0 {
		return errs[0].code(), ""
	}
	return errs[0].code(), httpx.Refusal("text/plain", []byte(strings.Join(texts, "; ")))
}

// retryAfter reads Expo's wait in seconds, or the default where it named none.
func retryAfter(raw string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	return defaultRetryAfter
}

// Actor is who an access token acts as: a user, a robot or an SSO user.
type Actor struct {
	ID string
	// Kind is Expo's type name for the actor: User, Robot, SSOUser.
	Kind string
	Name string
}

// Viewer is the actor the client's token acts as. A token Expo does not
// recognise answers a 401.
func (c *Client) Viewer(ctx context.Context) (Actor, error) {
	var data struct {
		MeActor *struct {
			Typename    string `json:"__typename"`
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
		} `json:"meActor"`
	}
	const q = `query CrewletViewer { meActor { __typename id displayName } }`
	if err := c.do(ctx, "CrewletViewer", q, nil, &data); err != nil {
		return Actor{}, err
	}
	if data.MeActor == nil || data.MeActor.ID == "" {
		return Actor{}, &APIError{Op: "CrewletViewer", Status: http.StatusUnauthorized,
			Detail: "Expo did not recognise the access token"}
	}
	return Actor{ID: data.MeActor.ID, Kind: data.MeActor.Typename, Name: data.MeActor.DisplayName}, nil
}

// Account is an Expo account as the engine reads it.
type Account struct {
	ID   string
	Name string
}

// Account reads the account named name. A name Expo does not know, or one
// the token's actor cannot see, is a refusal (404 or 403).
func (c *Client) Account(ctx context.Context, name string) (Account, error) {
	var data struct {
		Account struct {
			ByName struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"byName"`
		} `json:"account"`
	}
	const q = `query CrewletAccount($name: String!) { account { byName(accountName: $name) { id name } } }`
	if err := c.do(ctx, "CrewletAccount", q, map[string]any{"name": name}, &data); err != nil {
		return Account{}, err
	}
	got := data.Account.ByName
	if got.ID == "" {
		return Account{}, &APIError{Op: "CrewletAccount", Status: http.StatusNotFound,
			Detail: "Expo has no account named " + strconv.Quote(name)}
	}
	return Account{ID: got.ID, Name: got.Name}, nil
}

// ViewerPermissions is what the token's actor holds on the account named
// name, its own permissions and those its role implies, each once.
func (c *Client) ViewerPermissions(ctx context.Context, name string) ([]Permission, error) {
	var data struct {
		Account struct {
			ByName struct {
				Viewer struct {
					Permissions []Permission `json:"permissions"`
					Effective   []Permission `json:"effectivePermissions"`
				} `json:"viewerUserPermission2"`
			} `json:"byName"`
		} `json:"account"`
	}
	const q = `query CrewletViewerPermissions($name: String!) { account { byName(accountName: $name) { ` +
		`viewerUserPermission2 { permissions effectivePermissions } } } }`
	if err := c.do(ctx, "CrewletViewerPermissions", q, map[string]any{"name": name}, &data); err != nil {
		return nil, err
	}
	v := data.Account.ByName.Viewer
	return permissionSet(append(slices.Clone(v.Permissions), v.Effective...)), nil
}

// AppCount is how many projects the account holds.
func (c *Client) AppCount(ctx context.Context, name string) (int, error) {
	var data struct {
		Account struct {
			ByName struct {
				AppCount int `json:"appCount"`
			} `json:"byName"`
		} `json:"account"`
	}
	const q = `query CrewletAppCount($name: String!) { account { byName(accountName: $name) { appCount } } }`
	if err := c.do(ctx, "CrewletAppCount", q, map[string]any{"name": name}, &data); err != nil {
		return 0, err
	}
	return data.Account.ByName.AppCount, nil
}

// Robot is one robot user on an account: its permissions there and its
// access tokens.
type Robot struct {
	ID      string
	Name    string
	Created time.Time

	// Deleting is a robot whose deletion is scheduled: it is going, and no
	// longer the seat's.
	Deleting bool

	// Permissions are what it holds on the account the listing read.
	Permissions []Permission

	// Tokens are its access tokens, revoked ones included.
	Tokens []Token
}

// Token is one access token, never its value.
type Token struct {
	ID        string
	Note      string
	Revoked   bool
	CreatedAt time.Time
}

// robotNode is one robot member as the listing answers it.
type robotNode struct {
	Permissions []Permission `json:"permissions"`
	Actor       struct {
		Typename                string     `json:"__typename"`
		ID                      string     `json:"id"`
		DisplayName             string     `json:"displayName"`
		Created                 time.Time  `json:"created"`
		LastDeletionAttemptTime *time.Time `json:"lastDeletionAttemptTime"`
		AccessTokens            []struct {
			ID        string     `json:"id"`
			Note      *string    `json:"note"`
			RevokedAt *time.Time `json:"revokedAt"`
			CreatedAt time.Time  `json:"createdAt"`
		} `json:"accessTokens"`
	} `json:"actor"`
}

func (n robotNode) robot() Robot {
	r := Robot{
		ID: n.Actor.ID, Name: n.Actor.DisplayName, Created: n.Actor.Created,
		Deleting:    n.Actor.LastDeletionAttemptTime != nil,
		Permissions: permissionSet(n.Permissions),
	}
	for _, t := range n.Actor.AccessTokens {
		tok := Token{ID: t.ID, Revoked: t.RevokedAt != nil, CreatedAt: t.CreatedAt}
		if t.Note != nil {
			tok.Note = *t.Note
		}
		r.Tokens = append(r.Tokens, tok)
	}
	return r
}

// Robots is every robot user on the account named name, read page by page.
func (c *Client) Robots(ctx context.Context, name string) ([]Robot, error) {
	const q = `query CrewletRobots($name: String!, $first: Int!, $after: String) { account { ` +
		`byName(accountName: $name) { membersPaginated(memberType: ROBOT, first: $first, after: $after) { ` +
		`edges { node { permissions actor { __typename id displayName created lastDeletionAttemptTime ` +
		`accessTokens { id note revokedAt createdAt } } } } pageInfo { hasNextPage endCursor } } } } }`
	var out []Robot
	var after *string
	for page := 0; ; page++ {
		if page >= robotPageCeiling {
			return nil, fmt.Errorf("expo: the robot listing did not finish after %d pages of %d",
				robotPageCeiling, robotPageSize)
		}
		var data struct {
			Account struct {
				ByName struct {
					Members struct {
						Edges []struct {
							Node robotNode `json:"node"`
						} `json:"edges"`
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
					} `json:"membersPaginated"`
				} `json:"byName"`
			} `json:"account"`
		}
		vars := map[string]any{"name": name, "first": robotPageSize, "after": after}
		if err := c.do(ctx, "CrewletRobots", q, vars, &data); err != nil {
			return nil, err
		}
		members := data.Account.ByName.Members
		for _, edge := range members.Edges {
			if edge.Node.Actor.Typename == "Robot" && edge.Node.Actor.ID != "" {
				out = append(out, edge.Node.robot())
			}
		}
		if !members.PageInfo.HasNextPage || members.PageInfo.EndCursor == "" || len(members.Edges) == 0 {
			return out, nil
		}
		cursor := members.PageInfo.EndCursor
		after = &cursor
	}
}

// CreateRobot creates a robot user on the account with accountID, named name
// and holding perms there.
func (c *Client) CreateRobot(ctx context.Context, accountID, name string, perms []Permission) (Robot, error) {
	var data struct {
		Robot struct {
			Create struct {
				ID          string    `json:"id"`
				DisplayName string    `json:"displayName"`
				Created     time.Time `json:"created"`
			} `json:"createRobotForAccount"`
		} `json:"robot"`
	}
	const q = `mutation CrewletCreateRobot($accountID: String!, $permissions: [Permission]!, ` +
		`$robotData: RobotDataInput) { robot { createRobotForAccount(accountID: $accountID, ` +
		`permissions: $permissions, robotData: $robotData) { id displayName created } } }`
	vars := map[string]any{
		"accountID": accountID, "permissions": perms, "robotData": map[string]any{"name": name},
	}
	if err := c.do(ctx, "CrewletCreateRobot", q, vars, &data); err != nil {
		return Robot{}, err
	}
	got := data.Robot.Create
	if got.ID == "" {
		return Robot{}, errors.New("expo: created a robot and Expo returned no id for it")
	}
	return Robot{ID: got.ID, Name: got.DisplayName, Created: got.Created,
		Permissions: permissionSet(perms)}, nil
}

// RenameRobot sets a robot's name.
func (c *Client) RenameRobot(ctx context.Context, id, name string) error {
	const q = `mutation CrewletRenameRobot($id: String!, $robotData: RobotDataInput!) { robot { ` +
		`updateRobot(id: $id, robotData: $robotData) { id } } }`
	return c.do(ctx, "CrewletRenameRobot", q, map[string]any{
		"id": id, "robotData": map[string]any{"name": name},
	}, nil)
}

// DeleteRobot schedules a robot's deletion, which removes it and every token
// it holds. A robot Expo no longer has is the state asked for.
func (c *Client) DeleteRobot(ctx context.Context, id string) error {
	var data struct {
		Robot struct {
			Receipt struct {
				State        string `json:"state"`
				WillRetry    bool   `json:"willRetry"`
				ErrorMessage string `json:"errorMessage"`
			} `json:"scheduleRobotDeletion"`
		} `json:"robot"`
	}
	const q = `mutation CrewletDeleteRobot($id: ID!) { robot { scheduleRobotDeletion(id: $id) { ` +
		`state willRetry errorMessage } } }`
	err := c.do(ctx, "CrewletDeleteRobot", q, map[string]any{"id": id}, &data)
	if Status(err) == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	if r := data.Robot.Receipt; r.State == "FAILURE" && !r.WillRetry {
		detail := strings.TrimSpace(r.ErrorMessage)
		if detail == "" {
			detail = noDetail
		}
		return &APIError{Op: "CrewletDeleteRobot", Status: http.StatusInternalServerError,
			Detail: "Expo could not delete the robot: " + httpx.Refusal("text/plain", []byte(detail))}
	}
	return nil
}

// CreateToken mints an access token for the actor with actorID, under note.
// The value is answered once and never again: the caller seals it at once.
func (c *Client) CreateToken(ctx context.Context, actorID, note string) (Token, string, error) {
	var data struct {
		AccessToken struct {
			Create struct {
				Token       string `json:"token"`
				AccessToken struct {
					ID        string    `json:"id"`
					Note      *string   `json:"note"`
					CreatedAt time.Time `json:"createdAt"`
				} `json:"accessToken"`
			} `json:"createAccessToken"`
		} `json:"accessToken"`
	}
	const q = `mutation CrewletCreateToken($data: CreateAccessTokenInput!) { accessToken { ` +
		`createAccessToken(createAccessTokenData: $data) { token accessToken { id note createdAt } } } }`
	vars := map[string]any{"data": map[string]any{"actorID": actorID, "note": note}}
	if err := c.do(ctx, "CrewletCreateToken", q, vars, &data); err != nil {
		return Token{}, "", err
	}
	got := data.AccessToken.Create
	tok := Token{ID: got.AccessToken.ID, CreatedAt: got.AccessToken.CreatedAt}
	if got.AccessToken.Note != nil {
		tok.Note = *got.AccessToken.Note
	}
	if tok.ID == "" {
		return Token{}, "", errors.New("expo: created an access token and Expo returned no id for it")
	}
	return tok, got.Token, nil
}

// RevokeToken revokes an access token, which stops it authenticating at once.
// A token Expo no longer has is the state asked for.
func (c *Client) RevokeToken(ctx context.Context, id string) error {
	const q = `mutation CrewletRevokeToken($id: ID!) { accessToken { ` +
		`setAccessTokenRevoked(id: $id, revoked: true) { id } } }`
	err := c.do(ctx, "CrewletRevokeToken", q, map[string]any{"id": id}, nil)
	if Status(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// DeleteToken deletes an access token. A token Expo no longer has is the
// state asked for.
func (c *Client) DeleteToken(ctx context.Context, id string) error {
	const q = `mutation CrewletDeleteToken($id: ID!) { accessToken { deleteAccessToken(id: $id) { id } } }`
	err := c.do(ctx, "CrewletDeleteToken", q, map[string]any{"id": id}, nil)
	if Status(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// RetireToken revokes then deletes a token, so one whose delete fails has
// still stopped working.
func (c *Client) RetireToken(ctx context.Context, id string) error {
	if err := c.RevokeToken(ctx, id); err != nil {
		return err
	}
	return c.DeleteToken(ctx, id)
}

// Grant adds perms to what the actor holds on the account. Nothing to add
// makes no call.
func (c *Client) Grant(ctx context.Context, accountID, actorID string, perms []Permission) error {
	if len(perms) == 0 {
		return nil
	}
	const q = `mutation CrewletGrant($accountID: ID!, $actorID: ID!, $permissions: [Permission]) { ` +
		`account { grantActorPermissions(accountID: $accountID, actorID: $actorID, ` +
		`permissions: $permissions) { id } } }`
	return c.do(ctx, "CrewletGrant", q, map[string]any{
		"accountID": accountID, "actorID": actorID, "permissions": perms,
	}, nil)
}

// Revoke takes perms away from what the actor holds on the account. Nothing
// to take makes no call.
func (c *Client) Revoke(ctx context.Context, accountID, actorID string, perms []Permission) error {
	if len(perms) == 0 {
		return nil
	}
	const q = `mutation CrewletRevoke($accountID: ID!, $actorID: ID!, $permissions: [Permission]) { ` +
		`account { revokeActorPermissions(accountID: $accountID, actorID: $actorID, ` +
		`permissions: $permissions) { id } } }`
	return c.do(ctx, "CrewletRevoke", q, map[string]any{
		"accountID": accountID, "actorID": actorID, "permissions": perms,
	}, nil)
}

// permissionSet is perms sorted, each once.
func permissionSet(perms []Permission) []Permission {
	out := slices.Clone(perms)
	slices.Sort(out)
	return slices.Compact(out)
}
