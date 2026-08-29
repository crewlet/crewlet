package webhooks_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/crewlet/crewlet/internal/api/webhooks"
)

// The Forge tests mint a REAL RSA key and sign REAL tokens with it. Verifying
// against a fake that returns "valid" would test the plumbing and none of the
// checks — and the checks are the whole route: there is no shared secret here,
// so a token that verifies against the wrong key, algorithm, audience, issuer
// or clock is the entire attack surface.

// forgeKey is the signing key for the suite, generated once. 2048 bits because
// key generation is the slowest thing in this package and the size proves
// nothing about the code under test.
var forgeKey = func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}()

const forgeKID = "test-key-1"

// staticKeys is a KeySource holding one key.
type staticKeys struct {
	kid  string
	key  any
	asks atomic.Int64
}

func (s *staticKeys) Key(_ context.Context, kid string) (any, error) {
	s.asks.Add(1)
	if kid != s.kid {
		return nil, errNoSuchKey
	}
	return s.key, nil
}

var errNoSuchKey = &keyError{}

type keyError struct{}

func (*keyError) Error() string { return "unknown key" }

func testKeys(t *testing.T) webhooks.KeySource {
	t.Helper()
	return &staticKeys{kid: forgeKID, key: &forgeKey.PublicKey}
}

// forgeToken mints an invocation token. Every claim is a parameter, because
// every one of them is a check worth failing on its own.
func forgeToken(t *testing.T, claims jwt.MapClaims, method jwt.SigningMethod, kid string, key any) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	if kid != "" {
		token.Header["kid"] = kid
	}
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func validForgeClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss": webhooks.ForgeIssuer,
		"aud": "app-123",
		"exp": pinned.Add(time.Minute).Unix(),
		"iat": pinned.Add(-time.Minute).Unix(),
	}
}

var forgePageEvent = []byte(`{
  "eventType": "avi:confluence:updated:page",
  "atlassianId": "acct-9",
  "eventCreatedDate": "2026-08-23T11:59:00Z",
  "content": {"type": "page", "title": "Runbook", "space": {"key": "OPS"}}
}`)

func forgeHeaders(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func TestAValidForgeTokenIsAccepted(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	token := forgeToken(t, validForgeClaims(), jwt.SigningMethodRS256, forgeKID, forgeKey)

	res := e.post(t, "/webhooks/forge", forgePageEvent, forgeHeaders(token))
	if res.Code != http.StatusOK {
		t.Fatalf("got %d: %s", res.Code, res.Body)
	}
	ev := e.published.last()
	if ev == nil {
		t.Fatal("nothing was published")
	}
	// The SOURCE comes from the event name, not the route: one endpoint
	// carries both Jira and Confluence Cloud.
	if ev.Source != "confluence" {
		t.Errorf("source = %q, want confluence — the route is /webhooks/forge either way", ev.Source)
	}
	rows := e.rows(t)
	if len(rows) != 1 || rows[0].Type != "forge:avi:confluence:updated:page" {
		t.Fatalf("row = %v, want the forge: prefix so the feed says how it arrived", rows)
	}
}

func TestAForgeTokenIsRefusedForEveryClaimThatMatters(t *testing.T) {
	t.Parallel()
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		token func(t *testing.T) string
	}{
		{"signed by someone else", func(t *testing.T) string {
			return forgeToken(t, validForgeClaims(), jwt.SigningMethodRS256, forgeKID, other)
		}},
		{"addressed to another app", func(t *testing.T) string {
			c := validForgeClaims()
			c["aud"] = "someone-elses-app"
			return forgeToken(t, c, jwt.SigningMethodRS256, forgeKID, forgeKey)
		}},
		{"issued by something else", func(t *testing.T) string {
			c := validForgeClaims()
			c["iss"] = "not-forge"
			return forgeToken(t, c, jwt.SigningMethodRS256, forgeKID, forgeKey)
		}},
		{"expired", func(t *testing.T) string {
			c := validForgeClaims()
			c["exp"] = pinned.Add(-time.Minute).Unix()
			return forgeToken(t, c, jwt.SigningMethodRS256, forgeKID, forgeKey)
		}},
		{"never expires", func(t *testing.T) string {
			c := validForgeClaims()
			delete(c, "exp")
			return forgeToken(t, c, jwt.SigningMethodRS256, forgeKID, forgeKey)
		}},
		{"names no key", func(t *testing.T) string {
			return forgeToken(t, validForgeClaims(), jwt.SigningMethodRS256, "", forgeKey)
		}},
		{"names an unknown key", func(t *testing.T) string {
			return forgeToken(t, validForgeClaims(), jwt.SigningMethodRS256, "rotated-away", forgeKey)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEdge(t)
			res := e.post(t, "/webhooks/forge", forgePageEvent, forgeHeaders(tc.token(t)))
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("got %d, want 401", res.Code)
			}
			if e.published.count() != 0 || len(e.rows(t)) != 0 {
				t.Error("a refused token still reached the queue or the store")
			}
		})
	}
}

func TestOnlyRS256IsAccepted(t *testing.T) {
	t.Parallel()
	// The pin is asserted through the one case that OBSERVES it: a token
	// signed with the genuine key, with genuine claims, naming RS512.
	// Every check but the algorithm passes, so an unpinned parser accepts
	// it — which is what makes this the mutation the pin has to survive.
	//
	// The two classic breaks below are refused by the key TYPE rather than
	// by the pin, and this says so rather than taking credit for it.
	e := newEdge(t)

	wrongFamily := forgeToken(t, validForgeClaims(), jwt.SigningMethodRS512, forgeKID, forgeKey)
	if got := e.post(t, "/webhooks/forge", forgePageEvent, forgeHeaders(wrongFamily)).Code; got != http.StatusUnauthorized {
		t.Errorf("an RS512 token got %d, want 401 — the algorithm is not pinned", got)
	}

	// alg=none. Refused because the keyfunc returns an RSA key rather
	// than the library's none sentinel.
	unsigned := forgeToken(t, validForgeClaims(), jwt.SigningMethodNone, forgeKID,
		jwt.UnsafeAllowNoneSignatureType)
	if got := e.post(t, "/webhooks/forge", forgePageEvent, forgeHeaders(unsigned)).Code; got != http.StatusUnauthorized {
		t.Errorf("an alg=none token got %d, want 401", got)
	}

	// HS256 signed with the PUBLIC key as an HMAC secret — the attack that
	// works against libraries whose keys are all bytes. Go's HMAC verifier
	// takes []byte and is handed an *rsa.PublicKey, so it never gets as
	// far as comparing anything.
	pub, err := jwt.NewWithClaims(jwt.SigningMethodHS256, validForgeClaims()).
		SignedString(forgeKey.PublicKey.N.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got := e.post(t, "/webhooks/forge", forgePageEvent, forgeHeaders(pub)).Code; got != http.StatusUnauthorized {
		t.Errorf("an HS256 token got %d, want 401", got)
	}
}

func TestAKeylessTokenNeverReachesTheKeySource(t *testing.T) {
	t.Parallel()
	// A token with no kid is refused before the key source is consulted.
	// Not merely tidier: an unknown id makes a cold cache FETCH, so
	// without the short circuit a kid-less token is an unauthenticated
	// caller's way of making this process reach the network.
	keys := &staticKeys{kid: forgeKID, key: &forgeKey.PublicKey}
	e := newEdge(t, func(o *webhooks.Options) { o.Keys = keys })

	token := forgeToken(t, validForgeClaims(), jwt.SigningMethodRS256, "", forgeKey)
	if got := e.post(t, "/webhooks/forge", forgePageEvent, forgeHeaders(token)).Code; got != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", got)
	}
	if asked := keys.asks.Load(); asked != 0 {
		t.Errorf("the key source was asked %d times for a token naming no key", asked)
	}
}

func TestAForgeRequestWithoutABearerIsRefused(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	for _, header := range []string{"", "Basic abc", "Bearer ", "bearer lowercase"} {
		res := e.post(t, "/webhooks/forge", forgePageEvent,
			map[string]string{"Authorization": header})
		if res.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q got %d, want 401", header, res.Code)
		}
	}
}

func TestForgeIgnoresItsOwnEcho(t *testing.T) {
	t.Parallel()
	// The app's own writes come back to it. Acting on them is how an agent
	// answers its own comment, forever.
	e := newEdge(t)
	body := []byte(`{"eventType":"avi:confluence:updated:page","selfGenerated":true,` +
		`"content":{"type":"page","title":"x"}}`)
	res := e.post(t, "/webhooks/forge", body,
		forgeHeaders(forgeToken(t, validForgeClaims(), jwt.SigningMethodRS256, forgeKID, forgeKey)))
	if res.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 — a 4xx would make Atlassian retry it", res.Code)
	}
	if e.published.count() != 0 {
		t.Error("the app's own event woke an agent")
	}
}

func TestAnUnmappedForgeEventIsAcceptedAndDropped(t *testing.T) {
	t.Parallel()
	// An app's subscriptions outlive this build's knowledge of them. A 4xx
	// would make Atlassian retry an event nothing here will ever handle.
	e := newEdge(t)
	body := []byte(`{"eventType":"avi:jira:created:sprint"}`)
	res := e.post(t, "/webhooks/forge", body,
		forgeHeaders(forgeToken(t, validForgeClaims(), jwt.SigningMethodRS256, forgeKID, forgeKey)))
	if res.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", res.Code)
	}
	if e.published.count() != 0 {
		t.Error("an unmapped event was published")
	}
}

func TestAForgePayloadIsRewrittenIntoWhatTransportsRead(t *testing.T) {
	t.Parallel()
	// Forge renames everything: a Cloud page update arrives as
	// avi:confluence:updated:page where the Data Center webhook for the
	// same thing says page_updated. Translating here is what lets ONE
	// transport per integration handle both deployments.
	e := newEdge(t)
	if got := e.post(t, "/webhooks/forge", forgePageEvent,
		forgeHeaders(forgeToken(t, validForgeClaims(), jwt.SigningMethodRS256, forgeKID, forgeKey))).Code; got != http.StatusOK {
		t.Fatalf("got %d", got)
	}
	body := publishedBody(t, e)
	if body["event"] != "page_updated" {
		t.Errorf("event = %v, want the name a native webhook uses", body["event"])
	}
	page, _ := body["page"].(map[string]any)
	if page["title"] != "Runbook" {
		t.Errorf("the page did not survive the rewrite: %v", body)
	}
	// Forge states the actor ONCE, at the top level, and strips it from
	// the payload — so a transport reading only the body would attribute
	// every Cloud event to nobody.
	if body["userAccountId"] != "acct-9" {
		t.Errorf("userAccountId = %v, want the relay's atlassianId", body["userAccountId"])
	}
	// The event's own time in epoch milliseconds, which is what the native
	// webhooks carry.
	want := float64(time.Date(2026, 8, 23, 11, 59, 0, 0, time.UTC).UnixMilli())
	if body["timestamp"] != want {
		t.Errorf("timestamp = %v, want %v", body["timestamp"], want)
	}
}

func TestAForgeCommentKeepsTheSpaceRoutingReads(t *testing.T) {
	t.Parallel()
	// The space can be stated on either the comment or its container, and
	// routing reads it off the page. A comment whose space stayed on the
	// comment would route nowhere.
	e := newEdge(t)
	body := []byte(`{
	  "eventType": "avi:confluence:created:comment",
	  "atlassianId": "acct-1",
	  "content": {"type":"comment","space":{"key":"OPS"},
	              "container":{"title":"Runbook"}}
	}`)
	if got := e.post(t, "/webhooks/forge", body,
		forgeHeaders(forgeToken(t, validForgeClaims(), jwt.SigningMethodRS256, forgeKID, forgeKey))).Code; got != http.StatusOK {
		t.Fatalf("got %d", got)
	}
	page, _ := publishedBody(t, e)["page"].(map[string]any)
	space, _ := page["space"].(map[string]any)
	if space["key"] != "OPS" {
		t.Errorf("the page carries no space, so the comment routes nowhere: %v", page)
	}
}

func TestAForgeJiraEventLosesTheRelaysOwnFields(t *testing.T) {
	t.Parallel()
	// A transport that had to know it was reading a relay would need the
	// whole event mapping as well.
	e := newEdge(t)
	body := []byte(`{
	  "eventType": "avi:jira:created:issue",
	  "atlassianId": "acct-2",
	  "selfGenerated": false,
	  "encryptedData": "should-not-travel",
	  "issue": {"key":"OPS-1","fields":{"summary":"Fix it"}}
	}`)
	if got := e.post(t, "/webhooks/forge", body,
		forgeHeaders(forgeToken(t, validForgeClaims(), jwt.SigningMethodRS256, forgeKID, forgeKey))).Code; got != http.StatusOK {
		t.Fatalf("got %d", got)
	}
	published := publishedBody(t, e)
	if published["webhookEvent"] != "jira:issue_created" {
		t.Errorf("webhookEvent = %v", published["webhookEvent"])
	}
	for _, gone := range []string{"eventType", "atlassianId", "encryptedData", "selfGenerated"} {
		if _, present := published[gone]; present {
			t.Errorf("the relay's own %q field travelled into the payload", gone)
		}
	}
	user, _ := published["user"].(map[string]any)
	if user["accountId"] != "acct-2" {
		t.Errorf("user = %v, want the relay's atlassianId filled in", published["user"])
	}
	// The RAW bytes are untouched, though: they are what the store keeps
	// and what a re-verification would need.
	rows := e.rows(t)
	rec, err := e.events.ByID(t.Context(), rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rec.Payload), "encryptedData") {
		t.Error("the stored payload was rewritten; it should be what the provider sent")
	}
}

// publishedBody pulls the transformed body back out of the published event.
func publishedBody(t *testing.T, e *edge) map[string]any {
	t.Helper()
	ev := e.published.last()
	if ev == nil {
		t.Fatal("nothing was published")
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var envelope struct {
		Body map[string]any `json:"body"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return envelope.Body
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
	server, hits := jwksServer(t, jwksKey{"k0", &forgeKey.PublicKey})
	source := webhooks.NewJWKS(server.URL, nil, nil)

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
	server, hits := jwksServer(t, jwksKey{"k0", &forgeKey.PublicKey})
	source := webhooks.NewJWKS(server.URL, nil, nil)

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
	server, _ := jwksServer(t, jwksKey{"k0", &forgeKey.PublicKey})
	clock := pinned
	source := webhooks.NewJWKS(server.URL, nil, func() time.Time { return clock })
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
	one, _ := jwksServer(t, jwksKey{"k0", &forgeKey.PublicKey})
	two, _ := jwksServer(t, jwksKey{"k0", &forgeKey.PublicKey}, jwksKey{"k1", &rotated.PublicKey})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := one.URL
		if second.Load() {
			target = two.URL
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)

	clock := pinned
	source := webhooks.NewJWKS(server.URL, nil, func() time.Time { return clock })
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
	before, _ := jwksServer(t, jwksKey{"k0", &forgeKey.PublicKey})
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
	source := webhooks.NewJWKS(server.URL, nil, func() time.Time { return clock })
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
	good, _ := jwksServer(t, jwksKey{"k0", &forgeKey.PublicKey})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() {
			_, _ = w.Write([]byte(`{"keys":[]}`))
			return
		}
		http.Redirect(w, r, good.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)

	clock := pinned
	source := webhooks.NewJWKS(server.URL, nil, func() time.Time { return clock })
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
	if _, err := webhooks.NewJWKS(server.URL, nil, nil).Key(t.Context(), "k0"); err == nil {
		t.Fatal("a key set with no RSA key was accepted")
	}
}

func TestBothSurfacesShowWhatTheProviderSentNotWhatWasRouted(t *testing.T) {
	t.Parallel()
	// Forge is the one source where the published body and the delivered
	// one differ, so it is the one place these can disagree. The dashboard
	// reads the LIVE envelope while a delivery is fresh and the STORED row
	// after a reload — and the same event id showing two different payloads
	// depending on when you looked is exactly the kind of divergence this
	// package is built to avoid.
	e := newEdge(t)
	body := []byte(`{"eventType":"avi:jira:created:issue","atlassianId":"acct-3",
	  "encryptedData":"relay-only","issue":{"key":"OPS-2"}}`)
	if got := e.post(t, "/webhooks/forge", body,
		forgeHeaders(forgeToken(t, validForgeClaims(), jwt.SigningMethodRS256, forgeKID, forgeKey))).Code; got != http.StatusOK {
		t.Fatalf("got %d", got)
	}

	e.stream.mu.Lock()
	live := e.stream.seen[0]
	e.stream.mu.Unlock()
	if _, present := live.Payload["encryptedData"]; !present {
		t.Errorf("the live envelope shows the ROUTED body, not what arrived: %v", live.Payload)
	}

	rows := e.rows(t)
	rec, err := e.events.ByID(t.Context(), rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rec.Payload), "encryptedData") {
		t.Error("the stored row shows the routed body, not what arrived")
	}

	// And the TRANSPORTS still get the rewritten one — the split exists
	// because both are needed, not because either is wrong.
	if _, present := publishedBody(t, e)["encryptedData"]; present {
		t.Error("the relay's own field reached the transport")
	}
}

// modulusOf is the suite key's modulus in the base64url form a JWK carries.
func modulusOf(t *testing.T) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString(forgeKey.PublicKey.N.Bytes())
}

// A RELAYED CLOUD EVENT CARRIES NO DELIVERY HEADER. forgeID is the Atlassian
// ACCOUNT behind the event — the actor, not the delivery — so it cannot
// identify one, and the relay's retries resend the same bytes.
func TestForgeRetriesAreDedupedOnThePayload(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	for range 2 {
		token := forgeToken(t, validForgeClaims(), jwt.SigningMethodRS256, forgeKID, forgeKey)
		if got := e.post(t, "/webhooks/forge", forgePageEvent, forgeHeaders(token)).Code; got != http.StatusOK {
			t.Fatalf("got %d", got)
		}
	}
	if n := e.published.count(); n != 1 {
		t.Errorf("%d events for one relayed delivery and its retry, want 1", n)
	}
}

// TWO RELAYED EVENTS ARE BOTH DELIVERED.
func TestTwoForgeEventsAreBothDelivered(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	for _, title := range []string{"Runbook", "Handbook"} {
		body := bytes.Replace(forgePageEvent, []byte(`"title"`), []byte(`"title"`), 1)
		body = append([]byte(nil), body...)
		body = bytes.Replace(body, []byte("}}"), []byte(`},"marker":"`+title+`"}`), 1)
		token := forgeToken(t, validForgeClaims(), jwt.SigningMethodRS256, forgeKID, forgeKey)
		if got := e.post(t, "/webhooks/forge", body, forgeHeaders(token)).Code; got != http.StatusOK {
			t.Fatalf("%s: %d", title, got)
		}
	}
	if n := e.published.count(); n != 2 {
		t.Errorf("%d events for two distinct relayed events, want 2", n)
	}
}
