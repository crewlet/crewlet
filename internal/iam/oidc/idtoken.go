package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// THE FIVE VALIDATIONS, and what each one costs when it is missing.
//
// An ID token is a bearer assertion by a third party that a particular person
// authenticated. Every check below is invisible while it works — removing any
// one of them changes no successful sign-in — and each one, removed, is a
// different way to sign in as somebody else:
//
//  1. THE SIGNATURE, under a key the ISSUER publishes, with the algorithm
//     PINNED. Without the pin a token arrives claiming `alg: none`, or HS256
//     signed with the public key an attacker downloaded from the issuer's own
//     key set.
//  2. THE ISSUER, compared EXACTLY. Without it, any provider's token is
//     accepted — including one from a free tenant the attacker registered
//     themselves.
//  3. THE AUDIENCE, which must contain this engine's client id. Without it, a
//     token the same provider minted for a DIFFERENT application signs
//     somebody in here: every application at that provider becomes a way in.
//  4. THE NONCE, compared against the one sealed in the flight cookie. Without
//     it, an ID token captured from any other login replays into this one.
//  5. THE EXPIRY, required rather than honoured-when-present. A token with no
//     `exp` is valid for ever, so one captured off the wire replays until the
//     provider rotates its signing key.
//
// The suite removes each in turn and fails; that is what the checks are, and a
// comment claiming them would be a claim.

// Claims are what this engine reads out of an ID token.
//
// A STRUCT AND NOT A MAP, so a claim this build does not know is simply not
// read: an identity provider is free to put anything in a token, and a map
// invites a later reader to act on a value nobody validated.
type Claims struct {
	// Subject is the provider's own identifier for this person, and the
	// ONLY value a binding may be keyed on. It is stable across a
	// person's name, address and group changes, which is exactly what an
	// address is not.
	Subject string

	// Issuer is the provider that minted it, kept so an audit row can say
	// which one.
	Issuer string

	// Email and Name are what the provider asserts about the person.
	//
	// NEITHER IS EVER A LINK. See the package doc: at most providers a
	// user can set their own address, so matching on one binds whoever
	// is most worth becoming. The address is used at REDEMPTION, against
	// an invitation somebody issued, and nowhere else.
	Email string
	Name  string

	// EmailVerified is what the provider says about the address. It is
	// recorded and is NOT a link either — a verified address is still an
	// address the provider decided to trust, at a provider this engine
	// does not administer.
	EmailVerified bool

	// Groups are the provider's group names, read from the claim
	// [Config.GroupsClaim] names and mapped to grants that ride into the
	// session. Empty when the deployment names no claim.
	Groups []string

	// ACR is the authentication context the provider asserts, and
	// AuthTime when the person actually authenticated — which is what a
	// step-up compares against, since a provider is free to answer a
	// `max_age` request from an existing session.
	ACR      string
	AuthTime time.Time
}

// Keys supplies the public key an ID token's `kid` names.
//
// A SEAM OF ONE METHOD so the suite can verify a REAL signature against a key
// it minted rather than reaching a provider's CDN — which would make every
// case depend on the network and on a third party's rotation. internal/jwks is
// what satisfies it in the engine.
type Keys interface {
	Key(ctx context.Context, keyID string) (any, error)
}

// Algorithms are the signing algorithms an ID token may use.
//
// RS256 IS THE ONE EVERY PROVIDER SIGNS WITH, and the asymmetric families
// beside it are here because some providers offer them and nothing is lost by
// reading one. What is NOT here is every HMAC algorithm: a symmetric token is
// verified with a shared secret, and the secret a provider shares is the
// client secret — so accepting HS256 turns the client secret into a token
// signing key, which is the confusion attack in its original form.
var Algorithms = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512"}

// Verify checks an ID token and returns what it asserts.
//
// THE NONCE IS AN ARGUMENT rather than a field on the config, because it is
// per-login: it comes out of the flight cookie the browser carried, and a
// verifier that held one would be a verifier per login attempt.
func (c Config) Verify(ctx context.Context, keys Keys, raw, nonce string,
	now time.Time) (Claims, error) {

	if keys == nil {
		return Claims{}, fmt.Errorf("%w: this node has no source for the "+
			"provider's signing keys", ErrNotConfigured)
	}
	keyfunc := func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			// SHORT-CIRCUITED BEFORE THE KEY SOURCE IS CONSULTED, and
			// not merely for a better message: an unknown id makes a
			// cold cache fetch, so a token with no kid at all would
			// otherwise be an unauthenticated caller's way of making
			// this process reach the network.
			return nil, errors.New("no kid in the token header")
		}
		return keys.Key(ctx, kid)
	}
	var claims rawClaims
	_, err := jwt.ParseWithClaims(raw, &claims, keyfunc,
		// (1) THE ALGORITHM PIN.
		jwt.WithValidMethods(Algorithms),
		// (2) THE ISSUER, compared exactly by the library.
		jwt.WithIssuer(c.Issuer),
		// (3) THE AUDIENCE.
		jwt.WithAudience(c.ClientID),
		// (5) THE EXPIRY, REQUIRED.
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: the id token did not verify: %w",
			ErrRefused, err)
	}

	// (4) THE NONCE, which the library knows nothing about.
	//
	// CONSTANT TIME, because it is a secret this engine minted and the
	// comparison is reachable by anybody who can reach the callback.
	if subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(nonce)) != 1 {
		return Claims{}, fmt.Errorf("%w: the id token's nonce is not this "+
			"login's — a token from another sign-in was presented here",
			ErrRefused)
	}
	if claims.Subject == "" {
		return Claims{}, fmt.Errorf("%w: the id token names no subject, so "+
			"there is nothing to bind a person to", ErrRefused)
	}
	// AND THE AUTHENTICATION CONTEXT, WHEN THIS COMPANY INSISTS ON ONE.
	// Requesting it is a request the provider may ignore, so this is what
	// enforces it.
	if c.RequireACR != "" && claims.ACR != c.RequireACR {
		return Claims{}, fmt.Errorf("%w: this company requires the %q "+
			"authentication context and the provider asserted %q",
			ErrRefused, c.RequireACR, claims.ACR)
	}
	// A MULTI-AUDIENCE TOKEN MUST NAME ITS AUTHORIZED PARTY. OpenID
	// Connect says `azp` is required when there is more than one audience,
	// and that it must be the client — without the check, a token minted
	// for two applications is accepted by both whichever it was meant for.
	if len(claims.Audience) > 1 && claims.AuthorizedParty != c.ClientID {
		return Claims{}, fmt.Errorf("%w: the id token names %d audiences and "+
			"its authorized party is %q rather than this client",
			ErrRefused, len(claims.Audience), claims.AuthorizedParty)
	}

	groups, err := claims.groups(c.GroupsClaim)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	out := Claims{
		Subject: claims.Subject, Issuer: claims.Issuer,
		Email: claims.Email, Name: claims.Name,
		EmailVerified: bool(claims.EmailVerified),
		Groups:        groups, ACR: claims.ACR,
	}
	if claims.AuthTime > 0 {
		out.AuthTime = time.Unix(claims.AuthTime, 0).UTC()
	}
	return out, nil
}

// rawClaims is the wire shape, with the registered claims the library
// validates embedded.
type rawClaims struct {
	jwt.RegisteredClaims

	Nonce           string `json:"nonce,omitempty"`
	AuthorizedParty string `json:"azp,omitempty"`
	Email           string `json:"email,omitempty"`
	Name            string `json:"name,omitempty"`

	// EmailVerified is a BOOL SOME PROVIDERS SEND AS A STRING, which is
	// not this engine being lenient for its own sake: a token that fails
	// to decode is a login nobody can complete, and the providers that do
	// it are large enough that refusing would be refusing them.
	EmailVerified flexibleBool `json:"email_verified,omitempty"`

	// ACR is the authentication context class the provider asserts. Its
	// values are the PROVIDER's own strings, which is why this engine
	// compares one an operator configured rather than carrying a
	// vocabulary of its own.
	ACR string `json:"acr,omitempty"`

	AuthTime int64 `json:"auth_time,omitempty"`

	// all is every claim the token carries, kept so the ONE claim this
	// deployment names as carrying groups can be read whatever it is
	// called. Nothing else reads it: the struct above is still what this
	// engine acts on, for [Claims]' reason.
	all map[string]json.RawMessage
}

// UnmarshalJSON decodes the named claims and keeps the whole set beside them.
func (c *rawClaims) UnmarshalJSON(data []byte) error {
	type named rawClaims // the same fields, without this method
	if err := json.Unmarshal(data, (*named)(c)); err != nil {
		return err
	}
	return json.Unmarshal(data, &c.all)
}

// groups reads the claim this deployment names as carrying the person's
// groups.
//
// # The claim is NAMED BY THE OPERATOR, and nothing is guessed
//
// Providers disagree about it — `groups` at most, `roles` for an application's
// own roles, a namespaced URL at the ones that insist custom claims carry one
// — so the name is `api.auth.oidc.group_claim`'s to state. A LIST OF ALIASES
// tried in turn would be a guess about which claim carries authority, and the
// first alias an attacker could get a provider to emit would win. EMPTY READS
// NO GROUPS AT ALL: a deployment that maps none does not have its logins'
// authority depend on a claim nobody chose to trust.
//
// ABSENT IS NO GROUPS and not an error, because a person in no group is a
// person; a single string is one group, because some providers flatten a
// one-element list. ANY OTHER SHAPE REFUSES THE SIGN-IN, naming the claim: the
// claim was named because it carries authority, and reading an object or a
// number as "no groups" would leave an operator's mapping silently conferring
// nothing, with a sign-in that looks like it worked.
func (c rawClaims) groups(claim string) ([]string, error) {
	if claim == "" {
		return nil, nil
	}
	raw, ok := c.all[claim]
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	return nil, fmt.Errorf("the id token's %q claim is neither a list of "+
		"group names nor one name, so this sign-in cannot say which groups "+
		"it confers; fix the claim at the provider or name another in "+
		"api.auth.oidc.groups_claim", claim)
}

// flexibleBool decodes a JSON bool or the strings "true" and "false".
type flexibleBool bool

func (f *flexibleBool) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case "true", `"true"`:
		*f = true
	case "false", `"false"`, "null":
		*f = false
	default:
		return fmt.Errorf("oidc: %s is not a boolean", data)
	}
	return nil
}

// KnownAlgorithm reports whether an algorithm is one this engine accepts, for
// a caller that has to say why a provider's metadata is unusable.
func KnownAlgorithm(alg string) bool { return slices.Contains(Algorithms, alg) }
