package expo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// maxResponse bounds what one answer may be: an actor with its tokens is a
// few kilobytes, and the cap stops a proxy's page being read whole.
const maxResponse = 4 << 20

// Role is an actor's role on an account, spelled as Expo's GraphQL Role enum.
type Role string

// Expo's account roles.
const (
	RoleViewOnly       Role = "VIEW_ONLY"
	RoleRunner         Role = "RUNNER"
	RoleDeveloper      Role = "DEVELOPER"
	RoleReleaseManager Role = "RELEASE_MANAGER"
	RoleAdmin          Role = "ADMIN"
	RoleOwner          Role = "OWNER"
	RoleCustom         Role = "CUSTOM"
)

// Administers reports a role that manages the account, which no agent holds.
func (r Role) Administers() bool { return r == RoleAdmin || r == RoleOwner }

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
// alike either way: a known code its status, a transient error 503, an error
// Expo attributes to the caller 400, and anything else 500.
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

// Actor is who an access token acts as: a robot, a person or an SSO user.
type Actor struct {
	ID string
	// Kind is Expo's type name for the actor: Robot, User, SSOUser.
	Kind string
	Name string

	// Tokens are a robot's own access tokens, never their values; empty for
	// any other actor.
	Tokens []Token
}

// Robot reports an actor that is a robot user.
func (a Actor) Robot() bool { return a.Kind == "Robot" }

// Token is one access token, by the prefix Expo shows for it.
type Token struct {
	ID      string
	Prefix  string
	Revoked bool
}

// Viewer is the actor the client's token acts as, with a robot's tokens. A
// token Expo does not recognise answers a 401.
func (c *Client) Viewer(ctx context.Context) (Actor, error) {
	var data struct {
		MeActor *struct {
			Typename     string `json:"__typename"`
			ID           string `json:"id"`
			DisplayName  string `json:"displayName"`
			AccessTokens []struct {
				ID        string     `json:"id"`
				Prefix    string     `json:"visibleTokenPrefix"`
				RevokedAt *time.Time `json:"revokedAt"`
			} `json:"accessTokens"`
		} `json:"meActor"`
	}
	const q = `query CrewletViewer { meActor { __typename id displayName ` +
		`... on Robot { accessTokens { id visibleTokenPrefix revokedAt } } } }`
	if err := c.do(ctx, "CrewletViewer", q, nil, &data); err != nil {
		return Actor{}, err
	}
	if data.MeActor == nil || data.MeActor.ID == "" {
		return Actor{}, &APIError{Op: "CrewletViewer", Status: http.StatusUnauthorized,
			Detail: "Expo did not recognise the access token"}
	}
	me := data.MeActor
	out := Actor{ID: me.ID, Kind: me.Typename, Name: me.DisplayName}
	for _, t := range me.AccessTokens {
		out.Tokens = append(out.Tokens, Token{ID: t.ID, Prefix: t.Prefix, Revoked: t.RevokedAt != nil})
	}
	return out, nil
}

// RoleOn is the role the token's actor holds on the account named name. An
// account the actor holds no role on answers a 403 or a 404.
//
// Expo carries the Role only on the deprecated viewerUserPermission; the
// current viewerUserPermission2 answers permissions alone.
func (c *Client) RoleOn(ctx context.Context, name string) (Role, error) {
	var data struct {
		Account struct {
			ByName struct {
				Viewer *struct {
					Role Role `json:"role"`
				} `json:"viewerUserPermission"`
			} `json:"byName"`
		} `json:"account"`
	}
	const q = `query CrewletRole($name: String!) { account { byName(accountName: $name) { ` +
		`viewerUserPermission { role } } } }`
	if err := c.do(ctx, "CrewletRole", q, map[string]any{"name": name}, &data); err != nil {
		return "", err
	}
	v := data.Account.ByName.Viewer
	if v == nil || v.Role == "" {
		return "", &APIError{Op: "CrewletRole", Status: http.StatusForbidden,
			Detail: "the token's actor holds no role on " + strconv.Quote(name)}
	}
	return v.Role, nil
}
