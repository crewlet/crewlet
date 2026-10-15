// Package jwks is the ONE cached reader of a published JSON Web Key Set, and
// the one place that decides what this engine trusts one to say.
//
// # Why it is a package and not a type in its caller
//
// The Forge relay a webhook arrives over verifies Atlassian's signed token
// against the keys Atlassian publishes, and what it needs from a key set is
// six decisions none of which is about Forge and none of which is locally
// obvious — how long a key set is trusted without re-reading, how often an
// unknown key id may trigger an outbound fetch, how big a document may be,
// whether one unusable entry discards the rest, whether a stale key still
// verifies when the source is down, and whether the fetch happens under the
// lock. Kept apart from the relay, each is stated and tested once, against a
// key set rather than through a webhook; and the relay's own copy had already
// got the last one wrong before it was extracted: it held its mutex ACROSS the
// fetch.
//
// # THE FETCH DOES NOT HAPPEN UNDER THE LOCK
//
// It is the decision this package exists to get right. A key set is read over
// the network from somebody else's CDN, with a ten-second timeout, and the
// obvious implementation takes the mutex, looks in the map, and fetches while
// still holding it. That is correct and it serialises EVERY verification in
// the process behind one hung request: a provider having a bad minute becomes
// this engine having one, on a path that is otherwise pure arithmetic over a
// cached key.
//
// So the lock is taken to READ the cache, released, and taken again to write
// what was fetched. What that opens — several callers deciding to fetch at
// once — is closed by a SINGLEFLIGHT rather than by holding the lock: the
// first caller starts a fetch and every caller waits on its result, so one key
// rotation produces one request however many tokens arrive during it. The
// fetch belongs to none of them — see [Set.fetch].
//
// # An unknown key id is rate-limited, not free
//
// A `kid` nobody has ever published is what a forged token carries, and
// treating it as "the cache must be stale" makes one outbound request per
// forgery attempt — an amplifier an unauthenticated caller aims wherever the
// key set is hosted. So an unknown id refetches at most once per
// [RefreshFloor], and inside that window it is simply refused.
//
// THE FLOOR IS MEASURED FROM THE LAST ATTEMPT, WHETHER OR NOT IT SUCCEEDED.
// It was measured from the last success, so a minute into an outage at the
// source every token naming an unknown kid — on the Forge webhook, a kid the
// sender chooses — was a fetch again, one after another, each against a host
// that had just failed. And the same floor holds for a key the set DOES name
// once the set is past its TTL: inside a floor of a failed attempt the stale
// key is served without asking again, where every such token used to wait out
// a fresh attempt at a failing host before being handed that same stale key.
// A COLD cache is the exception, and deliberately: it has nothing to answer
// from, so it asks again — one request in flight at a time, however many
// callers — because refusing from a recorded failure would turn a blip at
// boot into a minute of refusals.
package jwks

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// THE THREE NUMBERS, and every one of them exists because this fetch sits on
// a request path an unauthenticated caller can reach.
const (
	// TTL bounds how long a key set is trusted without re-reading it.
	//
	// AN HOUR. What it actually decides is how long a key an issuer has
	// RETIRED goes on verifying here — a provider rotates by publishing
	// the new key beside the old and dropping the old later, so this is
	// the window in which a token signed by a key they have stopped using
	// is still accepted. An hour is far inside every provider's own
	// overlap and far above the cost of re-reading a few kilobytes.
	TTL = time.Hour

	// RefreshFloor rate-limits the refetch an UNKNOWN key id triggers.
	//
	// A MINUTE, and the asymmetry with the TTL is the point: the TTL
	// bounds staleness for keys that exist, and this bounds the OUTBOUND
	// REQUESTS a stranger can cause by sending tokens with key ids nobody
	// published. Without it, verification failure is an amplifier aimed
	// at whoever hosts the key set.
	RefreshFloor = time.Minute

	// FetchTimeout keeps a hung endpoint from holding a request open.
	//
	// TEN SECONDS. It is generous because the fetch is rare and a cold
	// cache on a slow link is a real thing; it is FINITE because the
	// alternative is a request that never answers, and with the fetch off
	// the lock a slow one now costs the caller that triggered it rather
	// than every caller in the process.
	FetchTimeout = 10 * time.Second

	// MaxBytes bounds what a compromised or misbehaving host can make
	// this process buffer. A key set is a few kilobytes; 1 MiB is orders
	// of magnitude of headroom and still finite.
	MaxBytes = 1 << 20
)

// ErrUnknownKey reports a key id the published set does not name.
//
// ITS OWN SENTINEL because the caller's two answers differ: a token naming a
// key nobody published is refused, while a fetch that FAILED may be worth
// serving a stale key for. Collapsing them would make a provider's outage
// indistinguishable from a forgery.
var ErrUnknownKey = errors.New("jwks: unknown signing key")

// Set is a cached view of one published key set.
//
// SAFE FOR CONCURRENT USE.
type Set struct {
	url    string
	client *http.Client
	now    func() time.Time
	logger *slog.Logger

	// mu guards the cached map and the flight, and is NEVER held across
	// a fetch. See the package doc.
	mu        sync.Mutex
	keys      map[string]any
	fetchedAt time.Time
	flight    *flight

	// attemptedAt is when the last fetch ENDED, whether or not it
	// succeeded, and failed says whether it did not — what the refresh
	// floor is measured from. See the package doc.
	attemptedAt time.Time
	failed      bool
}

// flight is one in-progress fetch, waited on by everybody who asked while it
// was running.
//
// A CHANNEL RATHER THAN A CONDITION VARIABLE, so a waiter can also give up
// when ITS OWN context is cancelled: the caller that started the fetch owns
// its deadline, and a second caller with a shorter one must not be held to the
// first's.
type flight struct {
	done chan struct{}
	keys map[string]any
	err  error
}

// Options configure [New].
type Options struct {
	// URL is where the key set is published.
	URL string

	// Client is the HTTP client. Nil takes one from [httpx] with
	// [FetchTimeout] applied — never a bare &http.Client{}, which shares
	// the two-connections-per-host default transport with every other
	// caller in the process.
	Client *http.Client

	// Now is the clock. Nil takes wall-clock time.
	//
	// A PARAMETER because the two windows above are the CALLER's
	// deadlines rather than this type's: a package with two clocks has
	// two answers to "how old is this", and only one of them is the one a
	// test can pin.
	Now func() time.Time

	// Logger is where a degraded fetch is reported. Nil takes the
	// default.
	Logger *slog.Logger
}

// New builds a cached key source.
func New(opts Options) *Set {
	s := &Set{url: opts.URL, client: opts.Client, now: opts.Now, logger: opts.Logger}
	if s.client == nil {
		s.client = httpx.Client(FetchTimeout)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	return s
}

// Key returns the key with this id, fetching the set when the cache is cold,
// stale, or does not name it — at most once per [RefreshFloor] unless the
// cache is cold.
func (s *Set) Key(ctx context.Context, keyID string) (any, error) {
	cached, known, refuse := s.cached(keyID)
	switch {
	case known:
		return cached, nil
	case refuse:
		return nil, fmt.Errorf("%w %q", ErrUnknownKey, keyID)
	}

	fetched, err := s.fetch(ctx)
	if err != nil {
		// A STALE KEY BEATS REFUSING EVERY DELIVERY, when there is one.
		// The cached key is old, not wrong: serving it is an outage
		// caused by somebody else's availability avoided, and the TTL
		// already bounds how long it can last. A cache with nothing in
		// it has nothing to fall back to and the error stands.
		if stale, ok := s.stale(keyID); ok {
			s.logger.WarnContext(ctx, "jwks_refresh_failed",
				"url", s.url, "error", err.Error(),
				"detail", "verifying against the cached key set, which is past its TTL")
			return stale, nil
		}
		return nil, err
	}
	if key, ok := fetched[keyID]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("%w %q", ErrUnknownKey, keyID)
}

// cached answers from the map alone when it can: the key when it is fresh, or
// stale inside the refresh floor of a failed attempt, and whether an unknown
// id is inside the floor and must simply be refused.
func (s *Set) cached(keyID string) (key any, known, refuse bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	key, held := s.keys[keyID]
	floored := s.keys != nil && now.Sub(s.attemptedAt) < RefreshFloor
	switch {
	case held && now.Sub(s.fetchedAt) < TTL:
		return key, true, false
	case held && floored && s.failed:
		// STALE, AND THE SOURCE FAILED MOMENTS AGO: served as the fetch
		// that failed served it, without asking a failing host again.
		// The failure was reported when it happened, once.
		return key, true, false
	case !held && floored:
		// A key id this set does not name, asked for again inside the
		// floor of the last attempt — a success or a failure. Answering
		// from the cache is what keeps a forged kid from becoming an
		// outbound request per attempt, an outage at the source
		// included.
		return nil, false, true
	}
	return nil, false, false
}

// stale is the cached key for an id, however old, for the arm that serves one
// when the source cannot be reached.
func (s *Set) stale(keyID string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, held := s.keys[keyID]
	return key, held
}

// fetch reads the key set, at most once at a time.
//
// THE LOCK IS HELD ONLY TO JOIN OR START A FLIGHT, never across the request
// itself — which is the whole of what this package exists to get right.
//
// # The flight belongs to nobody's request
//
// Whoever finds no flight starts one, and every caller — that one included —
// then waits on ITS OWN context: a caller with a shorter deadline gives up
// rather than being held to somebody else's. The request itself runs on a
// context no caller can cancel, bounded by [FetchTimeout] instead. It used to
// run on the first caller's, so the first caller hanging up — a webhook sender
// giving up — failed every verification waiting on the same flight, each
// refused for somebody else's disconnect.
func (s *Set) fetch(ctx context.Context) (map[string]any, error) {
	s.mu.Lock()
	inflight := s.flight
	if inflight == nil {
		inflight = &flight{done: make(chan struct{})}
		s.flight = inflight
		go s.fly(context.WithoutCancel(ctx), inflight)
	}
	s.mu.Unlock()

	select {
	case <-inflight.done:
		return inflight.keys, inflight.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// fly performs one flight's request and publishes what it found to everybody
// waiting on it.
//
// ITS LIFETIME IS THE REQUEST'S, bounded by [FetchTimeout] whatever client the
// set was given, so a flight nobody waits on any more still ends.
func (s *Set) fly(ctx context.Context, inflight *flight) {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	keys, err := s.read(ctx)
	s.mu.Lock()
	now := s.now()
	if err == nil {
		s.keys, s.fetchedAt = keys, now
	}
	// THE ATTEMPT IS RECORDED WHETHER OR NOT IT SUCCEEDED, and the flight
	// is cleared either way, so a failure is never latched: once the
	// refresh floor has passed the next caller asks again. What keeps an
	// outage from being a request per attempt is that floor, measured
	// from THIS instant — see [Set.cached] — which holds wherever there is
	// a cache to answer from. A cold cache retries at once, one flight at
	// a time: it has nothing else to say.
	s.attemptedAt, s.failed = now, err != nil
	inflight.keys, inflight.err = keys, err
	s.flight = nil
	s.mu.Unlock()
	close(inflight.done)
}

// document is the subset of a JWK set this reads: RSA keys.
//
// WHAT THE CALLER NEEDS, and nothing it does not. The Forge relay pins RS256,
// so a key of any other type could verify nothing it accepts, and a key
// nothing should verify with is a key nothing should hold. Every other type
// (`EC`, `OKP`, `oct`) is skipped — an `oct` key above all, since a symmetric
// key read out of a published set is a signing secret anybody can download.
type document struct {
	Keys []struct {
		Kid string `json:"kid"`
		Kty string `json:"kty"`
		Use string `json:"use"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

// read performs the request and decodes the document.
func (s *Set) read(ctx context.Context) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, fmt.Errorf("jwks: request %s: %w", s.url, err)
	}
	res, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jwks: fetch %s: %w", s.url, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks: fetch %s: status %d", s.url, res.StatusCode)
	}
	var doc document
	if err := json.NewDecoder(io.LimitReader(res.Body, MaxBytes)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("jwks: decode %s: %w", s.url, err)
	}
	keys := make(map[string]any, len(doc.Keys))
	for _, k := range doc.Keys {
		// `use` IS ADVISORY AND ONLY ONE VALUE IS REFUSED. The field is
		// optional, so requiring "sig" would discard every set that
		// omits it; but a key published for ENCRYPTION is one the
		// issuer has said is not for signatures, and accepting it
		// would verify a token against a key its own publisher says
		// cannot have signed one.
		if k.Kid == "" || k.Use == "enc" {
			continue
		}
		if k.Kty != "RSA" {
			continue
		}
		pub, err := rsaKey(k.N, k.E)
		if err != nil {
			// ONE UNUSABLE ENTRY MUST NOT DISCARD THE REST: a key
			// set carries the outgoing key alongside the incoming
			// one through a rotation, and refusing the document
			// over the half being retired would break the half
			// that works.
			s.logger.WarnContext(ctx, "jwks_key_unusable",
				"url", s.url, "kid", k.Kid, "error", err.Error())
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		// AN ERROR RATHER THAN AN EMPTY SET, so the cache is not
		// updated. Storing the empty result would poison it: every key
		// the set held before would be gone, and on a cold cache every
		// lookup would find an unknown id against a non-nil map, which
		// the refresh floor then holds for a minute — so a momentarily
		// broken document would keep refusing tokens well after the
		// source recovered.
		return nil, fmt.Errorf("jwks: %s carried no usable signing key", s.url)
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
