// Package oauth is the engine's OAuth client for a remote MCP server: the
// protocol of signing the company in at the server's vendor, and of renewing
// and revoking what that sign-in granted. It holds nothing and stores nothing:
// the grant, the session a sign-in is between and the access token a server is
// sent are the engine's (internal/engine's mcpoauth.go), and this package is
// the requests they are made with.
//
// # What a sign-in is
//
// The MCP authorization specification's: OAuth 2.1's authorization code grant
// with PKCE, the client registered one of three ways ([catalogue.OAuthClient]),
// at the authorization server the server's protected-resource metadata names
// (RFC 9728, read by internal/mcp's address check, [mcp.OAuthEndpoints]).
//
//   - A CLIENT METADATA DOCUMENT (cimd): the engine's client id is the https
//     address of a document it publishes ([ClientMetadata]), which the
//     authorization server reads. Nothing is registered and nothing is
//     stored; the server needs `api.external_url`, where the document is.
//   - DYNAMIC REGISTRATION (dcr, RFC 7591): the engine registers a client at
//     the server's registration endpoint as the sign-in starts
//     ([Register]), and keeps what it was issued with the grant.
//   - THE COMPANY'S OWN CLIENT (own): an administrator registered one at the
//     vendor beforehand and configured its id, and its secret where it has
//     one.
//
// # Held to the specification's floor, and refused below it
//
//   - PKCE S256 AND NOTHING ELSE. A server whose authorization server does
//     not offer S256 is refused before a person is sent anywhere
//     ([CheckSignIn]); `plain` is never sent.
//   - THE RESOURCE IS NAMED on the authorization request, the exchange and
//     every renewal (RFC 8707), so a token is bound to the server it was
//     asked for and is no use at another.
//   - THE ISSUER IS CHECKED on the way back where the authorization server
//     names itself in its responses (RFC 9207), so a code another
//     authorization server issued is never exchanged at this one's endpoint.
//   - A REFRESH TOKEN IS REQUIRED. The engine renews a server's access on
//     every node that runs it, long after the person who signed in has gone,
//     so a grant with nothing to renew it is refused at the exchange
//     ([ErrNoRefreshToken]) rather than kept until its first hour runs out.
//
// # Every address is one a remote party named
//
// The authorization, token, registration and revocation endpoints come out
// of a third party's metadata, so every request this package sends goes
// through [mcp.ServerNamedClient]: on the server's own origin as the
// server's address is dialled (the operator chose it), anywhere else over
// https alone and through the public-address guard (ADR-0072). None follows
// a redirect, each is bounded by [requestBudget], and each answer is read to
// [maxAnswer] and no further.
//
// # What a vendor says is quoted, bounded and redacted
//
// An authorization server's refusal ([Error]) carries its own code and its own
// description, because the code is what a person searches the vendor's
// documentation for. Both are cut to [maxVendorWords] and passed through
// internal/redact before anything holds them, and neither ever carries a
// token this package sent.
package oauth

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// The bounds of a request to an authorization server.
const (
	// requestBudget bounds one request to a token, registration or
	// revocation endpoint, its answer included. REASONED: an authorization
	// server answers in well under a second; fifteen seconds is a slow
	// vendor's edge and a cold start past that, and a renewal is waited on
	// by a tool call that has its own budget behind it.
	requestBudget = 15 * time.Second
	// maxAnswer bounds an authorization server's answer. REASONED: a token
	// answer is a few hundred bytes, a JWT access token a few kilobytes, a
	// registration answer echoing the metadata it was sent about two; 64 KiB
	// keeps any real one whole and refuses a server's choice of what this
	// process holds.
	maxAnswer = 64 << 10
	// maxVendorWords bounds an authorization server's error code and its
	// description as anything holds them. REASONED: RFC 6749's codes are
	// short words and a description is a sentence; 512 bytes is a long one.
	maxVendorWords = 512
	// maxScopes bounds the scopes a sign-in asks for and a grant records.
	// REASONED: the address check keeps a document's first fifty, and a
	// vendor's whole list is a few dozen.
	maxScopes = 50
)

// MethodS256 is the one PKCE method the engine sends, and the one an MCP
// client may.
const MethodS256 = "S256"

// Client is how the engine is registered at one authorization server.
type Client struct {
	// Kind is how it was registered.
	Kind catalogue.OAuthClient
	// ID is the client id: the document's address for a metadata document,
	// what the registration issued, or what an administrator configured.
	ID string
	// Secret is the client's secret, "" for a public client: the one a
	// registration issued, or the company's own client's.
	Secret string
}

// Tokens are what a token endpoint answered: an access token, the refresh
// token that renews it, and when each runs out where the server said.
type Tokens struct {
	// Access is the access token. Held in a process's memory alone.
	Access string
	// Refresh is the refresh token, "" where a renewal issued no new one
	// (the one it was made with stays good).
	Refresh string
	// Expiry is when Access runs out, zero where the server did not say.
	Expiry time.Time
	// RefreshExpiry is when Refresh runs out, zero where the server did not
	// say (the `refresh_token_expires_in` several vendors send).
	RefreshExpiry time.Time
	// Scopes are the scopes granted, nil where the server did not say
	// (which RFC 6749 reads as the ones asked for).
	Scopes []string
}

// ErrNoRefreshToken is a sign-in whose exchange issued no refresh token: a
// grant nothing can renew past its first access token's life.
var ErrNoRefreshToken = errors.New("the authorization server issued no refresh token, so the " +
	"engine could not keep the sign-in past the access token's own life; ask its vendor to " +
	"issue refresh tokens to this client")

// Error is an authorization server's refusal, in its own words: the RFC 6749
// error code and its description, bounded and redacted, and the HTTP status
// it came with.
type Error struct {
	Status      int
	Code        string
	Description string
}

func (e *Error) Error() string {
	said := cmp.Or(e.Code, fmt.Sprintf("HTTP %d", e.Status))
	if e.Description != "" {
		said += ": " + e.Description
	}
	return "the authorization server refused: " + said
}

// Definitive reports whether the refusal is one waiting does not end: the
// grant or the client is no longer good, and a person has to sign in again.
// Anything else (a 5xx, a rate limit, an answer that is not one) is a renewal
// that may pass at the next try.
func (e *Error) Definitive() bool {
	switch e.Code {
	case "invalid_grant", "invalid_client", "unauthorized_client", "unsupported_grant_type",
		"invalid_scope", "access_denied", "invalid_target":
		return true
	}
	return false
}

// PKCE is one sign-in's proof key: the verifier the engine keeps, and the S256
// challenge the person's browser carries.
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE mints a proof key: 32 random bytes as the verifier (43 characters
// of base64url, RFC 7636's floor, with 256 bits of entropy), and its S256
// challenge.
func NewPKCE() (PKCE, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return PKCE{}, fmt.Errorf("oauth: mint a proof key: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	return PKCE{Verifier: verifier, Challenge: ChallengeOf(verifier)}, nil
}

// ChallengeOf is a verifier's S256 challenge (RFC 7636, 4.2).
func ChallengeOf(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// CheckSignIn refuses a sign-in at ep before a person is sent anywhere: an
// authorization server that does not offer PKCE S256, has no authorization
// or token endpoint, or does not support the registration kind.
func CheckSignIn(ep *mcp.OAuthEndpoints, kind catalogue.OAuthClient) error {
	switch {
	case ep == nil:
		return errors.New("the server's sign-in metadata could not be read")
	case !slices.Contains(ep.PKCEMethods, MethodS256):
		return errors.New("the authorization server does not offer PKCE with S256, the only " +
			"proof key an MCP client may use, so the engine does not sign in there")
	case ep.Authorization == "" || ep.Token == "":
		return errors.New("the authorization server's metadata names no authorization or token " +
			"endpoint")
	case kind == catalogue.ClientCIMD && !ep.ClientIDMetadataDocument:
		return errors.New("the authorization server does not read a client metadata document " +
			"(cimd); set oauth.client to dcr or own")
	case kind == catalogue.ClientDCR && ep.Registration == "":
		return errors.New("the authorization server offers no registration endpoint (dcr); set " +
			"oauth.client to cimd or own")
	}
	return nil
}

// Scopes is what a sign-in at ep asks for: the configured scopes where there
// are any, else the ones the server's own metadata lists, and offline_access
// beside them where the authorization server lists it, which some issue a
// refresh token for alone.
func Scopes(configured []string, ep *mcp.OAuthEndpoints) []string {
	out := slices.Clone(configured)
	if len(out) == 0 && ep.ResourceScopes {
		out = slices.Clone(ep.Scopes)
	}
	if ep.OfflineAccess && !slices.Contains(out, "offline_access") {
		out = append(out, "offline_access")
	}
	return out[:min(len(out), maxScopes)]
}

// AuthorizeURL is where a person's browser is sent to consent: ep's
// authorization endpoint, with the client, the callback, the signed state, the
// PKCE challenge, the scopes and the resource.
//
// HTTPS, OR THE SERVER'S OWN ORIGIN: the browser of a signed-in person is sent
// to it, so an address in some other scheme (javascript:, data:, plain http
// to a third party) is refused rather than handed to a page.
func AuthorizeURL(ep *mcp.OAuthEndpoints, client Client, redirect, state, challenge string,
	scopes []string) (string, error) {

	u, err := url.Parse(ep.Authorization)
	if err != nil || u.Host == "" {
		return "", errors.New("the authorization endpoint is not an address")
	}
	if !strings.EqualFold(u.Scheme, "https") && !mcp.SameOrigin(ep.Authorization, ep.Server) {
		return "", fmt.Errorf("the authorization endpoint %s is not an https address",
			redact.URL(textcut.Bytes(ep.Authorization, maxVendorWords)))
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", client.ID)
	q.Set("redirect_uri", redirect)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", MethodS256)
	q.Set("resource", ep.Resource)
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Exchange trades an authorization code for the grant (RFC 6749, 4.1.3), with
// the verifier the code's challenge was minted from and the resource it is
// for. A grant that carries no refresh token is refused ([ErrNoRefreshToken]).
func Exchange(ctx context.Context, ep *mcp.OAuthEndpoints, client Client, code, verifier,
	redirect string, now time.Time) (Tokens, error) {

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
		"resource":      {ep.Resource},
	}
	tokens, err := tokenRequest(ctx, ep, client, form, now)
	if err != nil {
		return Tokens{}, err
	}
	if tokens.Refresh == "" {
		return Tokens{}, ErrNoRefreshToken
	}
	return tokens, nil
}

// Refresh renews a grant (RFC 6749, 6): a new access token, and the refresh
// token that replaces the one sent where the server rotates it.
func Refresh(ctx context.Context, ep *mcp.OAuthEndpoints, client Client, refresh string,
	now time.Time) (Tokens, error) {

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"resource":      {ep.Resource},
	}
	return tokenRequest(ctx, ep, client, form, now)
}

// Revoke asks the authorization server to revoke token (RFC 7009), hint naming
// what it is (`refresh_token`). False, and no error, where the server
// publishes no revocation endpoint: the grant is forgotten, not revoked, and
// the caller says so.
func Revoke(ctx context.Context, ep *mcp.OAuthEndpoints, client Client, token, hint string) (bool, error) {
	if ep.Revocation == "" {
		return false, nil
	}
	form := url.Values{"token": {token}, "token_type_hint": {hint}}
	resp, err := post(ctx, ep, ep.Revocation, client, form)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return false, fmt.Errorf("reading the revocation answer: %w", err)
	}
	// 200 IS REVOKED, AND SO IS A TOKEN THE SERVER NO LONGER KNOWS
	// (RFC 7009, 2.2): either way nothing it issued is good any more.
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	return false, refusal(resp.StatusCode, body, token, client.Secret)
}

// Metadata is a client's metadata (RFC 7591, 2): the document a client
// metadata document publishes, and the body a registration sends.
type Metadata struct {
	ClientID                string   `json:"client_id,omitempty"`
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// ClientName is the name an authorization server shows a person consenting.
const ClientName = "Crewlet"

// ClientMetadata is the engine's client metadata: a public client (no secret,
// PKCE its proof) whose one redirect is redirect. clientID is the document's
// own address for a client metadata document, and "" for a registration's
// body, where the server issues one.
func ClientMetadata(clientID, base, redirect string) Metadata {
	return Metadata{
		ClientID:                clientID,
		ClientName:              ClientName,
		ClientURI:               base,
		RedirectURIs:            []string{redirect},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
	}
}

// Register registers the engine as a client at ep's registration endpoint
// (RFC 7591), answering what it was issued: a client id, and a secret where
// the server issued one although none was asked for.
func Register(ctx context.Context, ep *mcp.OAuthEndpoints, doc Metadata) (Client, error) {
	if ep.Registration == "" {
		return Client{}, errors.New("the authorization server offers no registration endpoint")
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return Client{}, fmt.Errorf("oauth: encode the client metadata: %w", err)
	}
	client, u, err := mcp.ServerNamedClient(ep.Server, ep.Registration, requestBudget)
	if err != nil {
		return Client{}, fmt.Errorf("the registration endpoint: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(string(body)))
	if err != nil {
		return Client{}, errors.New("the registration endpoint is not an address a request can be sent to")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Client{}, fmt.Errorf("registering at %s: %w", redact.URL(u.String()), unwrap(err))
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return Client{}, fmt.Errorf("reading the registration answer: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return Client{}, refusal(resp.StatusCode, answer)
	}
	if len(answer) > maxAnswer {
		return Client{}, errors.New("the registration answer is larger than any registration")
	}
	var issued struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(answer, &issued); err != nil || issued.ClientID == "" {
		return Client{}, errors.New("the registration answer names no client id")
	}
	return Client{Kind: catalogue.ClientDCR, ID: issued.ClientID, Secret: issued.ClientSecret}, nil
}

// tokenRequest posts form to ep's token endpoint as client and reads the
// tokens it answers.
func tokenRequest(ctx context.Context, ep *mcp.OAuthEndpoints, client Client, form url.Values,
	now time.Time) (Tokens, error) {

	resp, err := post(ctx, ep, ep.Token, client, form)
	if err != nil {
		return Tokens{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return Tokens{}, fmt.Errorf("reading the token answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Tokens{}, refusal(resp.StatusCode, body, form.Get("code"), form.Get("code_verifier"),
			form.Get("refresh_token"), client.Secret)
	}
	if len(body) > maxAnswer {
		return Tokens{}, errors.New("the token answer is larger than any token answer")
	}
	var answer struct {
		AccessToken           string `json:"access_token"`
		TokenType             string `json:"token_type"`
		ExpiresIn             int64  `json:"expires_in"`
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
		Scope                 string `json:"scope"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return Tokens{}, errors.New("the token answer is not a JSON document")
	}
	if answer.AccessToken == "" {
		return Tokens{}, errors.New("the token answer carries no access token")
	}
	if !strings.EqualFold(answer.TokenType, "bearer") {
		return Tokens{}, fmt.Errorf("the token answer's type is %q, and an MCP server is sent a "+
			"Bearer token", textcut.Bytes(answer.TokenType, maxVendorWords))
	}
	out := Tokens{Access: answer.AccessToken, Refresh: answer.RefreshToken}
	if answer.ExpiresIn > 0 {
		out.Expiry = now.Add(time.Duration(answer.ExpiresIn) * time.Second).UTC()
	}
	if answer.RefreshTokenExpiresIn > 0 {
		out.RefreshExpiry = now.Add(time.Duration(answer.RefreshTokenExpiresIn) * time.Second).UTC()
	}
	if fields := strings.Fields(answer.Scope); len(fields) > 0 {
		out.Scopes = fields[:min(len(fields), maxScopes)]
	}
	return out, nil
}

// post sends form to address, one of ep's endpoints, authenticated as client:
// a public client names itself in the body; a confidential one sends its
// secret by HTTP basic authentication unless the server offers only the
// body (RFC 6749, 2.3.1).
func post(ctx context.Context, ep *mcp.OAuthEndpoints, address string, client Client,
	form url.Values) (*http.Response, error) {

	hc, u, err := mcp.ServerNamedClient(ep.Server, address, requestBudget)
	if err != nil {
		return nil, err
	}
	basic := client.Secret != "" && (len(ep.TokenAuthMethods) == 0 ||
		slices.Contains(ep.TokenAuthMethods, "client_secret_basic"))
	switch {
	case basic:
	case client.Secret != "":
		form.Set("client_id", client.ID)
		form.Set("client_secret", client.Secret)
	default:
		form.Set("client_id", client.ID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errors.New("the endpoint is not an address a request can be sent to")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		// FORM-ENCODED BEFORE THE BASIC ENCODING (RFC 6749, 2.3.1), which
		// a client id or secret holding a colon or a percent needs.
		req.SetBasicAuth(url.QueryEscape(client.ID), url.QueryEscape(client.Secret))
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending to %s: %w", redact.URL(u.String()), unwrap(err))
	}
	return resp, nil
}

// refusal is an authorization server's answer that was not a success, as an
// [*Error]: its code and description where it sent the RFC 6749 shape, its
// status alone otherwise. Bounded and redacted before anything holds it, and
// every value in sent (the code, the verifier, a refresh token, a client
// secret) withheld from it: a server that echoes what it was sent would
// otherwise put a credential in the words a log, a record and an alarm keep.
func refusal(status int, body []byte, sent ...string) error {
	var said struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(body[:min(len(body), maxAnswer)], &said)
	return &Error{
		Status:      status,
		Code:        vendorWords(withheld(said.Error, sent)),
		Description: vendorWords(withheld(said.ErrorDescription, sent)),
	}
}

// withheld is s with every non-empty value of sent replaced.
func withheld(s string, sent []string) string {
	for _, v := range sent {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[withheld]")
		}
	}
	return s
}

// vendorWords is a string an authorization server sent, as anything here
// holds it: one line, cut to [maxVendorWords], every credential-shaped run
// withheld.
func vendorWords(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return redact.Secrets(textcut.Bytes(s, maxVendorWords))
}

// unwrap is a client's error without the url.Error around it, whose sentence
// repeats the address unredacted; the caller names the address itself.
func unwrap(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err
	}
	return err
}
