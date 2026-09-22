package jwks_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"

	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/jwks"
)

// testKey is the RSA key every case in this suite publishes and verifies
// against. Generated once, because 2048-bit key generation is the slowest
// thing here by an order of magnitude and nothing in these cases depends on
// having a fresh one.
var testKey = func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("jwks: generate a test key: " + err.Error())
	}
	return key
}()

// pinned is the instant every clock-dependent case starts from, truncated to
// an hour so a cache window is a round number a failure message can be read
// against.
var pinned = time.Now().UTC().Truncate(time.Hour).Add(-time.Hour)

func modulusOf(t *testing.T) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString(testKey.PublicKey.N.Bytes())
}

// --- the JWKS cache --------------------------------------------------------

// jwksKey is one entry of a served key set. The id is explicit because the
// tests that matter here are about a document whose CONTENTS changed — a key
// added, a key withdrawn — and an id derived from position cannot express that.
type jwksKey struct {
	kid string
	pub *rsa.PublicKey
}

// jwksServer serves a key set and counts how often it was asked.
func jwksServer(t *testing.T, keys ...jwksKey) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	var doc struct {
		Keys []map[string]string `json:"keys"`
	}
	for _, key := range keys {
		doc.Keys = append(doc.Keys, map[string]string{
			"kid": key.kid, "kty": "RSA",
			"n": base64.RawURLEncoding.EncodeToString(key.pub.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.pub.E)).Bytes()),
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(server.Close)
	return server, &hits
}

func TestTheKeySetIsFetchedOnceAndReused(t *testing.T) {
	t.Parallel()
	// This fetch sits on the path of an UNAUTHENTICATED request — the token
	// is checked with the key, so the key is fetched before the caller is
	// known. Without the cache every delivery is an outbound round trip.
	server, hits := jwksServer(t, jwksKey{"k0", &testKey.PublicKey})
	source := jwks.New(jwks.Options{URL: server.URL})

	for range 5 {
		if _, err := source.Key(t.Context(), "k0"); err != nil {
			t.Fatalf("Key: %v", err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("the key set was fetched %d times for 5 lookups, want 1", got)
	}
}

func TestAnUnknownKeyIDDoesNotBecomeAFetchPerAttempt(t *testing.T) {
	t.Parallel()
	// A caller spraying tokens with random kids would otherwise turn every
	// forgery into an outbound HTTPS request from this process — an
	// unauthenticated amplifier.
	server, hits := jwksServer(t, jwksKey{"k0", &testKey.PublicKey})
	source := jwks.New(jwks.Options{URL: server.URL})

	for range 10 {
		if _, err := source.Key(t.Context(), "made-up"); err == nil {
			t.Fatal("an unknown key id resolved")
		}
	}
	if got := hits.Load(); got > 2 {
		t.Errorf("%d fetches for 10 forged key ids: the refresh floor is not holding", got)
	}
}

func TestAnUnreachableKeySetDoesNotStopVerification(t *testing.T) {
	t.Parallel()
	// The cached key is stale, not wrong. Refusing every Cloud delivery
	// because a CDN blinked is an outage caused by someone else's
	// availability, and the TTL bounds how long it lasts.
	//
	// The clock has to MOVE for this to mean anything: inside the TTL the
	// cache answers without reaching the network at all, so a test that
	// only closed the server would pass against a receiver that refuses
	// every stale key. Mutation testing found exactly that.
	server, _ := jwksServer(t, jwksKey{"k0", &testKey.PublicKey})
	clock := pinned
	source := jwks.New(jwks.Options{URL: server.URL, Now: func() time.Time { return clock }})
	if _, err := source.Key(t.Context(), "k0"); err != nil {
		t.Fatalf("warm: %v", err)
	}
	server.Close()

	// Still inside the TTL: answered from the cache, no network.
	if _, err := source.Key(t.Context(), "k0"); err != nil {
		t.Fatalf("a cached key stopped working when the CDN went away: %v", err)
	}

	// Past it: the refetch fails, and the stale key is served anyway.
	clock = clock.Add(2 * time.Hour)
	key, err := source.Key(t.Context(), "k0")
	if err != nil {
		t.Fatalf("an expired cache refused to serve a stale key with the CDN down: %v", err)
	}
	if key == nil {
		t.Fatal("the stale path answered with no key")
	}
}

func TestTheCacheExpires(t *testing.T) {
	// The TTL is a staleness BUDGET: Atlassian publishes no rotation
	// cadence, so the only thing bounding how long a withdrawn key stays
	// accepted is that the set is re-read. A cache that never expires
	// passes every other test here — it answers each warm lookup and never
	// reaches the network — which is why this asks for a key that did not
	// exist when the set was fetched.
	t.Parallel()
	rotated, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var second atomic.Bool
	one, _ := jwksServer(t, jwksKey{"k0", &testKey.PublicKey})
	two, _ := jwksServer(t, jwksKey{"k0", &testKey.PublicKey}, jwksKey{"k1", &rotated.PublicKey})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := one.URL
		if second.Load() {
			target = two.URL
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)

	clock := pinned
	source := jwks.New(jwks.Options{URL: server.URL, Now: func() time.Time { return clock }})
	if _, err := source.Key(t.Context(), "k0"); err != nil {
		t.Fatalf("warm: %v", err)
	}

	second.Store(true)
	clock = clock.Add(2 * time.Hour)
	if _, err := source.Key(t.Context(), "k1"); err != nil {
		t.Fatalf("a key added after the TTL elapsed was never seen: %v", err)
	}
}

func TestAWithdrawnKeyStopsBeingAccepted(t *testing.T) {
	// This is what the TTL is FOR, and it is the only thing that forces a
	// key already held to be re-read. Every other test here can be
	// satisfied by a cache that answers warm lookups forever — asking for
	// a key that has been taken out of the document is what tells the two
	// apart.
	t.Parallel()
	rotated, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var after atomic.Bool
	before, _ := jwksServer(t, jwksKey{"k0", &testKey.PublicKey})
	// k0 is GONE from the second document — the rotation completed.
	afterServer, _ := jwksServer(t, jwksKey{"k1", &rotated.PublicKey})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := before.URL
		if after.Load() {
			target = afterServer.URL
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)

	clock := pinned
	source := jwks.New(jwks.Options{URL: server.URL, Now: func() time.Time { return clock }})
	if _, err := source.Key(t.Context(), "k0"); err != nil {
		t.Fatalf("warm: %v", err)
	}

	after.Store(true)
	clock = clock.Add(2 * time.Hour)
	if _, err := source.Key(t.Context(), "k0"); err == nil {
		t.Fatal("a key withdrawn from the document is still accepted, so the " +
			"cache never expires and a revoked key is trusted for ever")
	}
}

func TestABrokenKeySetDoesNotPoisonTheCache(t *testing.T) {
	t.Parallel()
	// A document that carries no usable key is an ERROR rather than an
	// empty set, so the cache is not updated. Storing the empty result
	// would make every subsequent lookup an unknown id against a non-nil
	// map — which the refresh floor then holds for a minute, so a
	// momentarily broken document keeps refusing tokens long after the
	// CDN recovered.
	var broken atomic.Bool
	broken.Store(true)
	good, _ := jwksServer(t, jwksKey{"k0", &testKey.PublicKey})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() {
			_, _ = w.Write([]byte(`{"keys":[]}`))
			return
		}
		http.Redirect(w, r, good.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)

	clock := pinned
	source := jwks.New(jwks.Options{URL: server.URL, Now: func() time.Time { return clock }})
	if _, err := source.Key(t.Context(), "k0"); err == nil {
		t.Fatal("an empty key set was accepted")
	}

	// The CDN recovers a second later — well inside the refresh floor.
	broken.Store(false)
	clock = clock.Add(time.Second)
	if _, err := source.Key(t.Context(), "k0"); err != nil {
		t.Fatalf("the recovered key set was not picked up: %v", err)
	}
}

func TestAKeySetWithNoUsableKeyIsRefused(t *testing.T) {
	t.Parallel()
	// Answering with no keys would make every token fail with "unknown
	// key", which reads as a rotation rather than as a broken document.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[{"kid":"k0","kty":"EC","n":"","e":""}]}`))
	}))
	t.Cleanup(server.Close)
	if _, err := jwks.New(jwks.Options{URL: server.URL}).Key(t.Context(), "k0"); err == nil {
		t.Fatal("a key set with no RSA key was accepted")
	}
}

func TestAMalformedJWKEntryIsSkippedNotTrusted(t *testing.T) {
	t.Parallel()
	// One unusable entry must not discard the rest — a key set carries the
	// outgoing key alongside the incoming one through a rotation — and an
	// entry whose exponent will not fit an int must be skipped rather than
	// WRAPPED into a different key, which produces a public key that is
	// wrong without being detectably so.
	cases := []struct {
		name string
		doc  string
		want bool
	}{
		{"a good entry beside a broken one",
			`{"keys":[{"kid":"bad","kty":"RSA","n":"!!!","e":"AQAB"},` +
				`{"kid":"k0","kty":"RSA","n":"` + modulusOf(t) + `","e":"AQAB"}]}`, true},
		{"an exponent too large for an int",
			`{"keys":[{"kid":"k0","kty":"RSA","n":"` + modulusOf(t) +
				`","e":"AQABAQABAQABAQAB"}]}`, false},
		{"an exponent of zero",
			`{"keys":[{"kid":"k0","kty":"RSA","n":"` + modulusOf(t) + `","e":"AA"}]}`, false},
		{"an empty modulus",
			`{"keys":[{"kid":"k0","kty":"RSA","n":"","e":"AQAB"}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.doc))
			}))
			t.Cleanup(server.Close)
			_, err := jwks.New(jwks.Options{URL: server.URL}).Key(t.Context(), "k0")
			if tc.want && err != nil {
				t.Errorf("the usable key was discarded with the broken one: %v", err)
			}
			if !tc.want && err == nil {
				t.Error("a key that cannot be rebuilt correctly was accepted")
			}
		})
	}
}

// THE LOCK IS NOT HELD ACROSS THE FETCH.
//
// THE PROPERTY THIS PACKAGE WAS EXTRACTED TO GET RIGHT, and the one the copy
// it replaced had wrong. A key set is read over somebody else's network with a
// ten-second timeout; take the mutex, look in the map and fetch while still
// holding it, and one hung request serialises every verification in the
// process behind it — a provider having a bad minute becomes this engine
// having one, on a path that is otherwise arithmetic over a cached key.
//
// The case measures it the only way it is observable: hold the server open,
// and check that a SECOND caller can reach the cache and be answered while the
// first is still waiting on the wire.
func TestTheLockIsNotHeldAcrossTheFetch(t *testing.T) {
	t.Parallel()
	reached := make(chan struct{})
	release := make(chan struct{})
	var served atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if served.Add(1) == 1 {
			close(reached)
			<-release
		}
		_, _ = w.Write([]byte(`{"keys":[{"kid":"k0","kty":"RSA","n":"` +
			base64.RawURLEncoding.EncodeToString(testKey.PublicKey.N.Bytes()) +
			`","e":"AQAB"}]}`))
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	set := jwks.New(jwks.Options{URL: server.URL})
	go func() { _, _ = set.Key(t.Context(), "k0") }()
	<-reached

	// The first fetch is parked on the wire. A second caller must still be
	// able to take the mutex — it will end up waiting on the flight, not
	// on the lock, and the difference is invisible from outside EXCEPT
	// that a caller with its own deadline can give up.
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := set.Key(ctx, "k0"); done <- err }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the second caller was answered while the first fetch " +
				"was still parked, which this case cannot distinguish from " +
				"a broken server")
		}
		// It gave up on ITS OWN deadline, which it could only do by
		// having reached the wait rather than being stuck on the mutex.
	case <-time.After(3 * time.Second):
		t.Fatal("a second caller was still blocked three seconds after the " +
			"first fetch parked — the mutex is held across the fetch, so one " +
			"hung request serialises every verification in the process")
	}
	close(release)
}

// ONE ROTATION PRODUCES ONE REQUEST, HOWEVER MANY TOKENS ARRIVE DURING IT.
//
// Releasing the lock across the fetch opens a window in which every caller
// decides to fetch at once — so a key rotation would become one outbound
// request per in-flight token, aimed at whoever hosts the key set, exactly
// when they are already rotating. The singleflight is what closes it, and this
// is the case that says so.
func TestASingleFlightCollapsesAThunderingHerd(t *testing.T) {
	t.Parallel()
	gate := make(chan struct{})
	var served atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		<-gate
		_, _ = w.Write([]byte(`{"keys":[{"kid":"k0","kty":"RSA","n":"` +
			base64.RawURLEncoding.EncodeToString(testKey.PublicKey.N.Bytes()) +
			`","e":"AQAB"}]}`))
	}))
	t.Cleanup(server.Close)

	set := jwks.New(jwks.Options{URL: server.URL})
	const callers = 24
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = set.Key(t.Context(), "k0")
		}()
	}
	// Let them all pile up before the server answers.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if n := served.Load(); n != 1 {
		t.Errorf("%d callers produced %d fetches, want 1 — a rotation would "+
			"be one outbound request per in-flight token, aimed at whoever "+
			"hosts the key set at exactly the moment they are rotating",
			callers, n)
	}
}

// A FAILED FETCH IS NOT LATCHED: THE NEXT CALLER RETRIES.
//
// The flight is cleared whether or not it succeeded. Latching would mean one
// bad minute at the provider left this process refusing every token until
// something restarted it.
func TestAFailedFetchIsRetriedByTheNextCaller(t *testing.T) {
	t.Parallel()
	var served atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if served.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"keys":[{"kid":"k0","kty":"RSA","n":"` +
			base64.RawURLEncoding.EncodeToString(testKey.PublicKey.N.Bytes()) +
			`","e":"AQAB"}]}`))
	}))
	t.Cleanup(server.Close)

	set := jwks.New(jwks.Options{URL: server.URL})
	if _, err := set.Key(t.Context(), "k0"); err == nil {
		t.Fatal("a 502 was accepted as a key set")
	}
	if _, err := set.Key(t.Context(), "k0"); err != nil {
		t.Errorf("the second caller inherited the first's failure: %v — one "+
			"bad minute at the provider would refuse every token until "+
			"something restarted this process", err)
	}
}

// A KEY THE ISSUER PUBLISHED FOR ENCRYPTION NEVER VERIFIES A SIGNATURE.
//
// `use` is optional, so requiring "sig" would discard every set that omits it.
// But a key marked "enc" is one the issuer has SAID is not for signatures, and
// accepting it would verify a token against a key its own publisher says
// cannot have signed one.
func TestAnEncryptionKeyIsNotUsedToVerifySignatures(t *testing.T) {
	t.Parallel()
	modulus := base64.RawURLEncoding.EncodeToString(testKey.PublicKey.N.Bytes())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"keys":[` +
			`{"kid":"enc","kty":"RSA","use":"enc","n":"` + modulus + `","e":"AQAB"},` +
			`{"kid":"sig","kty":"RSA","use":"sig","n":"` + modulus + `","e":"AQAB"},` +
			`{"kid":"bare","kty":"RSA","n":"` + modulus + `","e":"AQAB"}]}`))
	}))
	t.Cleanup(server.Close)

	set := jwks.New(jwks.Options{URL: server.URL})
	if _, err := set.Key(t.Context(), "enc"); err == nil {
		t.Error("a key published for encryption was handed back to verify a " +
			"signature with")
	}
	for _, kid := range []string{"sig", "bare"} {
		if _, err := set.Key(t.Context(), kid); err != nil {
			t.Errorf("the %q key was discarded: %v — `use` is optional, so "+
				"requiring it would refuse every set that omits it", kid, err)
		}
	}
}
