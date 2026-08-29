package webhooks

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

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

// The JWKS cache's three numbers. All of them exist because this fetch sits on
// the path of an UNAUTHENTICATED request — the token is checked with the key,
// so the key is fetched before the caller is known.
const (
	// jwksTTL bounds how long a key set is trusted without re-reading it.
	// Atlassian publishes no rotation cadence, so this is a staleness
	// budget rather than a schedule: an hour is 24 fetches a day and puts
	// a firm ceiling on how long a withdrawn key stays accepted.
	jwksTTL = time.Hour

	// jwksRefreshFloor rate-limits the refetch an UNKNOWN key id triggers.
	// Without it, a caller spraying tokens with random kids turns every
	// forgery into an outbound HTTPS request from this process — an
	// unauthenticated amplifier. One a minute is fast enough for a real
	// rotation (the token is retried by the provider) and slow enough that
	// the amplification is gone.
	jwksRefreshFloor = time.Minute

	// jwksFetchTimeout keeps a hung CDN from holding a webhook request
	// open. Deliveries arrive against a provider's own delivery deadline,
	// and a fetch that outlived it would burn the retry it was blocking.
	jwksFetchTimeout = 10 * time.Second
)

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
		keys = NewJWKS(ForgeJWKSURL, nil, now)
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

// --- the JWKS cache --------------------------------------------------------

// JWKS reads Atlassian's published keys, with a cache in front.
type JWKS struct {
	url    string
	client *http.Client

	mu        sync.Mutex
	keys      map[string]any
	fetchedAt time.Time
	now       func() time.Time
}

// NewJWKS builds a cached key source. A nil client uses one with the fetch
// timeout applied; a nil clock uses the wall clock.
//
// The clock is a parameter because the cache's two windows are the receiver's
// deadlines, not this type's: a package with two clocks has two answers to
// "how old is this", and only one of them is the one a test can pin.
func NewJWKS(url string, client *http.Client, now func() time.Time) *JWKS {
	if client == nil {
		client = &http.Client{Timeout: jwksFetchTimeout}
	}
	if now == nil {
		now = time.Now
	}
	return &JWKS{url: url, client: client, now: now}
}

// Key returns the key with this id, fetching the set when the cache is cold,
// stale, or does not name it.
func (j *JWKS) Key(ctx context.Context, keyID string) (any, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	age := j.now().Sub(j.fetchedAt)
	key, known := j.keys[keyID]
	switch {
	case known && age < jwksTTL:
		return key, nil
	case !known && j.keys != nil && age < jwksRefreshFloor:
		// A key id this set does not name, asked for again inside the
		// floor. Answering from the cache is what keeps a forged kid
		// from becoming an outbound request per attempt.
		return nil, fmt.Errorf("webhooks: unknown Forge signing key %q", keyID)
	}

	fetched, err := j.fetch(ctx)
	if err != nil {
		if known {
			// The cached key is stale, not wrong. Serving it beats
			// refusing every Cloud delivery because a CDN blinked —
			// the alternative is an outage caused by someone else's
			// availability, and the TTL bounds how long it lasts.
			log.Warn("forge_jwks_refresh_failed", "error", err,
				"detail", "verifying against the cached key set, which is past its TTL")
			return key, nil
		}
		return nil, err
	}
	j.keys, j.fetchedAt = fetched, j.now()
	if key, ok := fetched[keyID]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("webhooks: unknown Forge signing key %q", keyID)
}

// jwksDocument is the subset of a JWK set this needs. Only RSA keys, because
// the parser accepts only RS256 — a key of another type could never verify a
// token this route would accept.
type jwksDocument struct {
	Keys []struct {
		Kid string `json:"kid"`
		Kty string `json:"kty"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

// maxJWKSBytes bounds what a compromised or misbehaving CDN can make this
// process buffer. A key set is a few kilobytes; 1 MiB is orders of magnitude
// of headroom and still finite.
const maxJWKSBytes = 1 << 20

func (j *JWKS) fetch(ctx context.Context) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.url, nil)
	if err != nil {
		return nil, fmt.Errorf("webhooks: jwks request: %w", err)
	}
	res, err := j.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webhooks: fetch jwks: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("webhooks: fetch jwks: status %d", res.StatusCode)
	}
	var doc jwksDocument
	if err := json.NewDecoder(io.LimitReader(res.Body, maxJWKSBytes)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("webhooks: decode jwks: %w", err)
	}
	keys := make(map[string]any, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pub, err := rsaKey(k.N, k.E)
		if err != nil {
			// One unusable entry must not discard the rest: a key set
			// carries the outgoing key alongside the incoming one
			// through a rotation, and refusing the document over the
			// half being retired would break the half that works.
			log.Warn("forge_jwks_key_unusable", "kid", k.Kid, "error", err)
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		// An ERROR rather than an empty set, so the cache is not
		// updated. Storing the empty result would poison it: every
		// subsequent lookup finds an unknown id against a non-nil map,
		// which the refresh floor then holds for a minute — so a
		// momentarily broken document would keep refusing tokens well
		// after the CDN recovered.
		return nil, errors.New("webhooks: jwks document carried no usable RSA key")
	}
	return keys, nil
}

// rsaKey rebuilds a public key from a JWK's base64url modulus and exponent.
func rsaKey(modulus, exponent string) (*rsa.PublicKey, error) {
	n, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(modulus, "="))
	if err != nil {
		return nil, fmt.Errorf("modulus: %w", err)
	}
	e, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(exponent, "="))
	if err != nil {
		return nil, fmt.Errorf("exponent: %w", err)
	}
	if len(n) == 0 || len(e) == 0 {
		return nil, errors.New("empty modulus or exponent")
	}
	// The exponent is a big-endian integer of whatever length the issuer
	// chose. Reading a fixed width would work for the universal 65537 and
	// silently produce a wrong key for anything else — and reading an
	// arbitrary width into an int would WRAP, which produces a key that is
	// wrong without being detectably so.
	exp := new(big.Int).SetBytes(e)
	if !exp.IsInt64() || exp.Int64() < 3 || exp.Int64() > math.MaxInt32 {
		return nil, fmt.Errorf("exponent out of range: %s", exp)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exp.Int64())}, nil
}
