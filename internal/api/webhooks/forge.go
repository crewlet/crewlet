package webhooks

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/jwks"

	"github.com/golang-jwt/jwt/v5"
)

// Forge Remote is how Atlassian CLOUD reaches this engine. There is no shared
// secret: the Forge app relays each event with an invocation token — a JWT
// signed by Atlassian and verified against their published keys — so the
// credential is an audience, not a secret, and the check is a signature over
// the token rather than over the body.
//
// One route carries Jira AND Confluence Cloud, which is why a Forge delivery's
// source is decided by its event name rather than by the endpoint it arrived
// at.

const (
	// ForgeJWKSURL is where Atlassian publishes the keys Forge signs with.
	ForgeJWKSURL = "https://forge.cdn.prod.atlassian-dev.net/.well-known/jwks.json"

	// ForgeIssuer is the iss claim every invocation token carries.
	ForgeIssuer = "forge/invocation-token"
)

// THE JWKS CACHE IS internal/jwks, AND ITS NUMBERS ARE ARGUED THERE.
//
// This edge used to carry its own, and the copy had already drifted from the
// property that matters most: it held its mutex ACROSS the network fetch, so a
// CDN having a bad minute serialised every Forge delivery in the process
// behind one hung request. The shared reader releases the lock and closes the
// duplicate-fetch window with a singleflight instead.

// KeySource supplies the public key a Forge invocation token names.
//
// An interface so a test can verify a real signature against a key it minted,
// rather than reaching Atlassian's CDN — which would make the suite depend on
// the network and on a third party's key rotation.
type KeySource interface {
	// Key returns the public key for a JWKS key id. An unknown id is an
	// error, never a nil key: a nil key reaches the JWT library as "verify
	// against nothing", and the shape of that failure depends on the
	// library rather than on this package.
	Key(ctx context.Context, keyID string) (any, error)
}

// forgeVerifier checks invocation tokens.
type forgeVerifier struct {
	keys KeySource
	now  func() time.Time
}

func newForgeVerifier(keys KeySource, now func() time.Time) *forgeVerifier {
	if keys == nil {
		keys = jwks.New(jwks.Options{URL: ForgeJWKSURL, Now: now})
	}
	return &forgeVerifier{keys: keys, now: now}
}

// verify checks one invocation token against the app id it must be addressed
// to, returning the reason it failed rather than a bare false: the difference
// between an expired token and one for somebody else's app is what an operator
// needs, and it is invisible from the outside.
//
// The options are assembled per call, not captured once, because two of them
// move: the app id comes from config and changes with a reload, and the clock
// is the receiver's own so a test can pin it.
func (f *forgeVerifier) verify(ctx context.Context, raw, appID string) error {
	keyfunc := func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			// Short-circuited before the key source is consulted, not
			// merely for a better message: an unknown id makes a cold
			// JWKS cache fetch, so a token with no kid at all would
			// otherwise be an unauthenticated caller's way of making
			// this process reach the network.
			return nil, errors.New("no kid in the token header")
		}
		return f.keys.Key(ctx, kid)
	}
	_, err := jwt.Parse(raw, keyfunc,
		// The algorithm is PINNED: Atlassian signs RS256, so a token
		// naming anything else did not come from the path this trusts.
		//
		// It is NOT what stops the classic confusion attack here — that
		// is the key TYPE. A keyfunc returning *rsa.PublicKey makes an
		// HS256 token fail on the key alone, because Go's HMAC verifier
		// takes []byte and refuses anything else; the attack works
		// against libraries whose keys are all bytes. The pin is what
		// stops a silent downgrade WITHIN the RSA family, and it is what
		// keeps this correct if the keyfunc ever returns another type.
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(ForgeIssuer),
		jwt.WithAudience(appID),
		// Required, not merely honoured when present. A token with no
		// exp is valid forever, so one captured off the wire replays
		// until Atlassian rotates the signing key.
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(f.now),
	)
	return err
}
