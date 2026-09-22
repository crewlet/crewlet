package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// THE SECOND HALF OF THE ROUND TRIP: the provider sends the browser back with
// a code, and the code is exchanged for tokens at the provider's own endpoint.
//
// # The exchange is written out rather than taken from a module
//
// It is one form POST and one JSON document. What a module would add here is
// its own client-authentication auto-detection, its own token cache and its own
// idea of what an error response looks like — three behaviours on the one path
// where this engine decides whether somebody is who they say they are, none of
// which would be visible in this file. The dependency rule asks a module to
// earn its place against what the standard library already does, and for a
// form POST it does all of it.
//
// CLIENT AUTHENTICATION IS `client_secret_post` AND NOT BASIC, deliberately:
// RFC 6749 requires the client id and secret in a Basic header to be
// form-urlencoded FIRST, which half of the ecosystem gets wrong in one
// direction and half in the other, so a secret containing a `+` or a `:`
// works at some providers and not others. In the body there is one encoding
// and every provider agrees about it.

// ExchangeTimeout bounds the token request.
//
// TEN SECONDS, which is a person waiting at a redirect rather than a machine
// retrying: longer and the browser has given up anyway, shorter and a provider
// on a slow path fails a login that would have worked.
const ExchangeTimeout = 10 * time.Second

// maxTokenBytes bounds what a misbehaving provider can make this buffer. A
// token response is a few kilobytes even with a large ID token in it.
const maxTokenBytes = 1 << 20

// Tokens is what a code exchange yields.
type Tokens struct {
	// IDToken is the assertion this engine verifies. Everything else here
	// is incidental to it.
	IDToken string

	// Refresh is what the deactivation probe has to ask the provider
	// with. EMPTY IS ORDINARY: a provider that does not grant
	// `offline_access` simply yields none, and the login still succeeds —
	// what is lost is the probe, and validation says so rather than
	// leaving an operator believing a central deactivation is felt before
	// the absolute session lifetime.
	Refresh string
}

// Exchange redeems an authorization code.
//
// THE VERIFIER GOES WITH IT, which is the whole of PKCE: the provider hashed
// the challenge at the authorization request and compares it to this, so a code
// intercepted between the provider and this engine cannot be redeemed by
// whoever intercepted it.
func (c Config) Exchange(ctx context.Context, client *http.Client,
	tokenEndpoint, code, verifier string) (Tokens, error) {

	if client == nil {
		client = httpx.Client(ExchangeTimeout)
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {c.RedirectURI},
		"code_verifier": {verifier},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
	}
	body, err := c.post(ctx, client, tokenEndpoint, form)
	if err != nil {
		return Tokens{}, err
	}
	if body.IDToken == "" {
		return Tokens{}, fmt.Errorf("%w: the provider's token response "+
			"carried no id_token, so there is nothing that asserts who "+
			"signed in", ErrRefused)
	}
	return Tokens{IDToken: body.IDToken, Refresh: body.RefreshToken}, nil
}

// Refresh exchanges a refresh token, which is what the deactivation probe
// does: an account the provider has deactivated answers `invalid_grant`.
func (c Config) Refresh(ctx context.Context, client *http.Client,
	tokenEndpoint, refresh string) (Tokens, error) {

	if client == nil {
		client = httpx.Client(ExchangeTimeout)
	}
	body, err := c.post(ctx, client, tokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
	})
	if err != nil {
		return Tokens{}, err
	}
	// A REFRESH MAY OR MAY NOT ROTATE THE TOKEN. A provider that returns
	// a new one has invalidated the old, so keeping the old would make
	// the NEXT probe report a deactivation that has not happened; one
	// that returns none has left the old valid. Carrying whichever was
	// returned and letting the caller keep its own when empty is the only
	// reading that is correct for both.
	return Tokens{IDToken: body.IDToken, Refresh: body.RefreshToken}, nil
}

// ErrDeactivated reports a provider that no longer recognises a grant.
//
// ITS OWN SENTINEL because it is the probe's whole answer: `invalid_grant` is
// how a provider says the account behind this token is gone, suspended or has
// had its consent withdrawn, and the caller ends the session as
// `idp_revoked` rather than retrying.
var ErrDeactivated = errors.New("oidc: the provider no longer recognises this grant")

// tokenResponse is the subset of a token endpoint's document this reads.
type tokenResponse struct {
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`

	// Error and ErrorDescription are the failure shape RFC 6749 defines,
	// carried on a 400 — so a failed exchange has a reason an operator
	// can read rather than a status code.
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (c Config) post(ctx context.Context, client *http.Client,
	endpoint string, form url.Values) (tokenResponse, error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("%w: token request: %w", ErrRefused, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("%w: reach the provider's token "+
			"endpoint: %w", ErrRefused, err)
	}
	defer func() { _ = res.Body.Close() }()

	var body tokenResponse
	// THE BODY IS DECODED WHATEVER THE STATUS, because the reason lives
	// in it: RFC 6749 puts `error` and `error_description` in a 400's
	// document, and a reader that branched on the status first would
	// report "status 400" for every failure a provider bothered to
	// explain.
	decodeErr := json.NewDecoder(io.LimitReader(res.Body, maxTokenBytes)).Decode(&body)
	switch {
	case body.Error == "invalid_grant":
		return tokenResponse{}, fmt.Errorf("%w: %s", ErrDeactivated,
			firstNonEmpty(body.ErrorDescription, body.Error))
	case body.Error != "":
		return tokenResponse{}, fmt.Errorf("%w: the provider refused the "+
			"exchange: %s (%s)", ErrRefused, body.Error, body.ErrorDescription)
	case res.StatusCode != http.StatusOK:
		return tokenResponse{}, fmt.Errorf("%w: the provider's token endpoint "+
			"answered %d", ErrRefused, res.StatusCode)
	case decodeErr != nil:
		return tokenResponse{}, fmt.Errorf("%w: decode the token response: %w",
			ErrRefused, decodeErr)
	}
	return body, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
