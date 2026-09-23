package oidc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/secrets"
)

// THE FIRST HALF OF THE ROUND TRIP: mint the three values, seal them, and send
// the browser to the provider.

// Flight is what a login in progress has to remember.
//
// IT LIVES IN THE BROWSER, sealed, and on no node — see the package doc for
// why a map here is why a fleet cannot serve logins.
type Flight struct {
	// State is compared with what the provider sends back. It is what
	// stops a third party's callback — a link somebody was sent —
	// completing a login in this browser, because they cannot know the
	// value sealed in the cookie beside it.
	State string `json:"state"`

	// Nonce is what must appear in the ID token. It binds the token to
	// THIS login attempt, so one captured from another cannot be
	// replayed here.
	Nonce string `json:"nonce"`

	// Verifier is the PKCE code verifier, and the reason this envelope is
	// SEALED rather than signed: it is what proves the party redeeming
	// the code is the party that asked for it, so a readable one is PKCE
	// defeated.
	Verifier string `json:"verifier"`

	// Return is where the browser goes once the login completes. It is
	// carried here rather than in a query parameter on the callback so it
	// cannot be rewritten between the two requests — an open redirect is
	// the ordinary way a sign-in flow leaks a token.
	Return string `json:"return,omitempty"`

	// ExpiresAt is when the flight stops being redeemable, stamped by the
	// node that minted it and enforced by whichever node finishes.
	ExpiresAt time.Time `json:"expires_at"`
}

// flightAAD binds a sealed flight to this purpose, so an envelope from
// anywhere else in the engine cannot be presented as one.
const flightAAD = "iam_oidc/flight"

// entropyBytes is how much randomness each of the three values carries.
//
// 32 BYTES. The state and the nonce need only be unguessable; the VERIFIER is
// held to RFC 7636, which requires 43 to 128 characters of the unreserved
// alphabet — 32 bytes of base64url is 43, the specification's own minimum and
// the value every provider is tested against.
const entropyBytes = 32

// Start mints a flight and returns the provider URL to send the browser to,
// with the sealed cookie value to set beside it.
//
// THE SEALED VALUE IS RETURNED RATHER THAN A COOKIE, because how a cookie is
// named and attributed is one decision this engine makes in one place, and it
// is not this package's: see internal/iam/session.
func (c Config) Start(cipher secrets.Cipher, authorizationEndpoint, returnTo string,
	now time.Time) (redirect, sealed string, err error) {

	if cipher == nil {
		return "", "", fmt.Errorf("%w: this node has no keyring, so a login "+
			"begun here could not be finished anywhere — including here",
			ErrNotConfigured)
	}
	if err := c.Validate(); err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrNotConfigured, err)
	}
	flight := Flight{Return: returnTo, ExpiresAt: now.Add(FlightTTL)}
	for _, into := range []*string{&flight.State, &flight.Nonce, &flight.Verifier} {
		value, err := randomValue()
		if err != nil {
			return "", "", err
		}
		*into = value
	}
	body, err := json.Marshal(flight)
	if err != nil {
		return "", "", fmt.Errorf("oidc: seal a flight: %w", err)
	}
	sealed, err = cipher.Encrypt(string(body), flightAAD)
	if err != nil {
		return "", "", fmt.Errorf("oidc: seal a flight: %w", err)
	}

	endpoint, err := url.Parse(authorizationEndpoint)
	if err != nil {
		return "", "", fmt.Errorf("%w: the provider's authorization endpoint "+
			"%q is not a url: %w", ErrNotConfigured, authorizationEndpoint, err)
	}
	query := endpoint.Query()
	for key, value := range map[string]string{
		"response_type": "code",
		"client_id":     c.ClientID,
		"redirect_uri":  c.RedirectURI,
		"scope":         strings.Join(c.requested(), " "),
		"state":         flight.State,
		"nonce":         flight.Nonce,
		// S256 AND NEVER `plain`. RFC 7636 permits sending the verifier
		// itself as the challenge, which protects against nothing: the
		// whole point is that an attacker who intercepts the
		// authorization request cannot derive the verifier from what
		// they saw.
		"code_challenge":        challengeFor(flight.Verifier),
		"code_challenge_method": "S256",
	} {
		query.Set(key, value)
	}
	if c.RequireACR != "" {
		// ASKED FOR, and separately CHECKED on the way back. A provider
		// is free to ignore this parameter, so requesting it is a
		// request and the validation is what enforces it.
		query.Set("acr_values", c.RequireACR)
	}
	endpoint.RawQuery = query.Encode()
	return endpoint.String(), sealed, nil
}

// Open unseals a flight and reports whether it is still redeemable.
//
// EVERY FAILURE IS ONE ERROR. A cookie that will not unseal, one from another
// purpose, one that has expired and one somebody edited are four facts, and
// the caller's move for all of them is to start the login again.
func Open(cipher secrets.Cipher, sealed string, now time.Time) (Flight, error) {
	if cipher == nil {
		return Flight{}, fmt.Errorf("%w: this node has no keyring", ErrNotConfigured)
	}
	body, err := cipher.Decrypt(sealed, flightAAD)
	if err != nil {
		return Flight{}, fmt.Errorf("%w: the login cookie did not unseal: %w",
			ErrRefused, err)
	}
	var flight Flight
	if err := json.Unmarshal([]byte(body), &flight); err != nil {
		return Flight{}, fmt.Errorf("%w: the login cookie did not decode: %w",
			ErrRefused, err)
	}
	switch {
	case flight.State == "" || flight.Nonce == "" || flight.Verifier == "":
		return Flight{}, fmt.Errorf("%w: the login cookie is missing one of "+
			"the three values a round trip carries", ErrRefused)
	case !now.Before(flight.ExpiresAt):
		return Flight{}, fmt.Errorf("%w: the login took longer than %s",
			ErrRefused, FlightTTL)
	}
	return flight, nil
}

// randomValue is one unguessable URL-safe value.
func randomValue() (string, error) {
	raw := make([]byte, entropyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("oidc: read randomness: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// challengeFor is RFC 7636's S256 transformation: the base64url of the
// SHA-256 of the verifier's ASCII.
func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
