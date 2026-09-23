// Package oidc is signing in through somebody else's identity provider.
//
// # The round trip is STATELESS, and that is the whole shape of it
//
// A login is two requests: the browser is sent to the provider, and the
// provider sends it back with a code. Between them the engine has to remember
// three values — the state it will compare, the nonce it will find in the ID
// token, and the PKCE verifier it will present at the exchange — and the
// obvious place to keep them is a map on the node that started it.
//
// THAT MAP IS WHY A FLEET CANNOT SERVE LOGINS. A load balancer puts the two
// requests on whichever nodes it likes, so a login begun on one and finished
// on another finds nothing and fails — intermittently, in proportion to how
// many nodes are running, and never on the single-node deployment anybody
// tests on. So the three values are SEALED INTO A COOKIE the browser carries,
// under the fleet keyring every node holds, and no node remembers anything.
//
// SEALED AND NOT MERELY SIGNED. A signed cookie is readable, and one of the
// three values is a secret: the PKCE verifier is what proves the party
// redeeming the code is the party that asked for it, so an attacker who can
// read it has defeated exactly the protection PKCE is. The other two would
// survive being read; the verifier decides the whole envelope.
//
// # Five validations, and each one has a test that fails when it is removed
//
// An ID token is a bearer assertion from a third party, and every one of the
// checks in idtoken.go is load-bearing in a way that is invisible when it
// works: drop the issuer check and any provider's token is accepted; drop the
// audience check and a token minted for a DIFFERENT application at the same
// provider signs somebody in here; drop the algorithm pin and `alg: none`
// arrives; drop the nonce and a token captured from another login replays;
// drop the expiry and one captured ever replays. None of them changes a single
// successful sign-in, which is why the suite mutates each in turn.
//
// # Linking is EXPLICIT, and an email match is never a link
//
// The engine does not create a person because a provider asserted an address,
// and it does not attach a provider subject to an existing person because the
// addresses agree. Both are the same hazard: at most providers a user can set
// their own address, so "the addresses match" is a claim the attacker controls
// — and the person it would link them to is whoever is most worth becoming.
// A subject is bound to a person by an invitation somebody issued or by an
// administrator, once, and the binding is what authenticates afterwards.
//
// There is no `auto_provision`. It is the same decision written as a config
// field, and a field is how it ends up on by accident.
package oidc

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// FlightTTL is how long a login may take between the two requests.
//
// TEN MINUTES, and both directions cost something real. Shorter and a person
// who has to fetch their phone for a second factor at the provider comes back
// to a login that has expired, with nothing to tell them why. Longer and the
// sealed cookie — which carries the PKCE verifier — is a credential sitting in
// a browser for the length of a meeting. Ten minutes is the interval an
// interactive sign-in actually takes, including a second factor.
const FlightTTL = 10 * time.Minute

// DefaultDeactivationProbe is how often a live session's refresh token is
// exchanged at the provider to see whether the account still exists.
//
// AN HOUR. What it decides is how long somebody deactivated centrally keeps a
// live session here: the provider tells nobody, so the only way to find out is
// to ask, and the only thing to ask with is the refresh token the login
// obtained. Shorter is a request per live session per interval against
// somebody else's rate limit; longer and an off-boarding at the identity
// provider takes most of a working day to reach this engine.
//
// WITHOUT `offline_access` THERE IS NO REFRESH TOKEN and therefore no probe,
// and validation says so rather than leaving an operator believing a central
// deactivation is felt before the absolute session lifetime.
const DefaultDeactivationProbe = time.Hour

// Scopes are what this engine asks the provider for.
//
// THE MINIMUM THAT WORKS, and nothing else. `openid` is the protocol,
// `profile` is a display name and `email` is the address an invitation is
// matched against at REDEMPTION — never at sign-in, which is the linking rule
// in the package doc. `offline_access` is asked for because the deactivation
// probe has nothing to ask with otherwise, and a provider that refuses it
// simply yields no refresh token rather than failing the login.
var Scopes = []string{"openid", "profile", "email", "offline_access"}

// ErrRefused reports a round trip this engine will not complete.
//
// ONE SENTINEL FOR EVERY ARM, for the reason internal/iam/credential gives
// about a sign-in: the detail is logged and the caller is told a login failed.
// The exception is the two states an operator has to be able to act on — an
// unconfigured provider and a subject already bound to somebody else — which
// have sentinels of their own below.
var ErrRefused = errors.New("oidc: this sign-in could not be completed")

// ErrNotConfigured reports a deployment with no identity provider.
var ErrNotConfigured = errors.New("oidc: no identity provider is configured")

// ErrSubjectClaimed reports a provider subject already bound to a DIFFERENT
// person.
//
// ITS OWN SENTINEL and audited, because it is the one refusal here that is
// evidence rather than noise: somebody's provider account is being offered as
// proof of an identity it is not bound to, and the caller answers 409 and
// leaves whatever invitation was in play unspent.
var ErrSubjectClaimed = errors.New("oidc: that provider account is already bound to somebody else")

// Config is the identity provider a company signs in through.
//
// ONE PROVIDER PER COMPANY. Two would make "which provider is this person's"
// a question every sign-in has to answer from an address the user controls,
// which is the linking hazard the package doc refuses.
type Config struct {
	// Issuer is the provider's own identifier, and the value an ID token's
	// `iss` must equal EXACTLY. Not a prefix, not a host match: providers
	// that serve many tenants distinguish them by path, so a prefix
	// comparison accepts another tenant's tokens.
	Issuer string

	// ClientID is this engine's registration at the provider, and the
	// value an ID token's `aud` must contain.
	ClientID string

	// ClientSecret authenticates the token exchange. It is a `${VAR}`
	// pointer in the company document and is resolved where the exchange
	// is built, never stored here in the clear.
	ClientSecret string

	// RedirectURI is where the provider sends the browser back, derived
	// from `api.external_url` rather than configured: two values for one
	// address is how a deployment comes to have a redirect the provider
	// rejects and a config that looks right.
	RedirectURI string

	// GroupsClaim is the ID token claim that carries the person's groups,
	// or empty for a deployment that maps none — in which case no claim is
	// read at all. See rawClaims.groups for why it is one name and never a
	// list of aliases.
	GroupsClaim string

	// RequireACR is the authentication context this company insists on —
	// a provider's name for "this person used a second factor". Empty
	// accepts whatever the provider asserts.
	//
	// IT IS CHECKED ONLY WHEN SET, and that is not laxity: the values are
	// the provider's own strings, so a default would be one vendor's
	// vocabulary imposed on every other, and a login refused for an `acr`
	// nobody configured is a lockout with no field to change.
	RequireACR string

	// DeactivationProbe is how often a live session is checked against
	// the provider. Zero takes [DefaultDeactivationProbe].
	DeactivationProbe time.Duration

	// Scopes is what the authorization request asks for. Empty takes
	// [Scopes], the engine's own set; `openid` is added when a list omits
	// it, because a request without it is not an OpenID Connect request.
	//
	// IT USED TO BE IGNORED: the request always sent [Scopes] whatever
	// `api.auth.oidc.scopes` said, so a deployment that narrowed the list
	// asked for what it had removed, and validation warned about a missing
	// `offline_access` the request was in fact sending.
	Scopes []string
}

// requested is the scope list one authorization request carries.
func (c Config) requested() []string {
	if len(c.Scopes) == 0 {
		return Scopes
	}
	if slices.Contains(c.Scopes, "openid") {
		return c.Scopes
	}
	return append([]string{"openid"}, c.Scopes...)
}

// Validate reports what is wrong with a provider configuration, naming the
// field.
func (c Config) Validate() error {
	var problems []error
	switch {
	case strings.TrimSpace(c.Issuer) == "":
		problems = append(problems, errors.New("`oidc.issuer` is empty"))
	default:
		issuer, err := url.Parse(c.Issuer)
		switch {
		case err != nil:
			problems = append(problems, fmt.Errorf("`oidc.issuer` is not a url: %w", err))
		case issuer.Scheme != "https":
			// HTTPS ONLY. The issuer is where this engine fetches the
			// keys it verifies every sign-in against, so a plain-http
			// one is a sign-in anybody on the path can forge.
			problems = append(problems, fmt.Errorf(
				"`oidc.issuer` is %q — it must be https, because it is where "+
					"this engine fetches the keys it verifies every sign-in "+
					"against", c.Issuer))
		}
	}
	if strings.TrimSpace(c.ClientID) == "" {
		problems = append(problems, errors.New("`oidc.client_id` is empty"))
	}
	if strings.TrimSpace(c.ClientSecret) == "" {
		problems = append(problems, errors.New(
			"`oidc.client_secret` is empty — the token exchange is "+
				"authenticated, and a public client here would let anybody "+
				"who intercepts a code redeem it"))
	}
	return errors.Join(problems...)
}

// Probe is the configured probe interval, or the default.
func (c Config) Probe() time.Duration {
	if c.DeactivationProbe > 0 {
		return c.DeactivationProbe
	}
	return DefaultDeactivationProbe
}
