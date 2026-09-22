package oidc_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/secrets"

	"github.com/golang-jwt/jwt/v5"
)

// --- an identity provider, in a test server ---------------------------------- //

// issued is what a provider remembers between the two halves of a round trip.
type issued struct {
	challenge string
	nonce     string
}

// issuer is an OpenID provider: discovery, a key set and a token endpoint. It
// is a real HTTP server because everything this package does is a round trip,
// and a fake that returned structs would exercise none of the wire.
type issuer struct {
	*httptest.Server

	// codes maps an authorization code to the challenge and nonce the
	// authorization request carried, which is what makes the PKCE check
	// and the nonce echo REAL rather than asserted.
	codes    map[string]issued
	nonce    string
	exchange atomic.Int64
	refusal  string
	rotate   string
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	i := &issuer{codes: map[string]issued{}, nonce: testNonce}
	mux := http.NewServeMux()
	mux.HandleFunc(oidc.MetadataPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                i.Server.URL,
			"authorization_endpoint":                i.Server.URL + "/authorize",
			"token_endpoint":                        i.Server.URL + "/token",
			"jwks_uri":                              i.Server.URL + "/jwks",
			"scopes_supported":                      []string{"openid", "profile", "email", "offline_access"},
			"code_challenge_methods_supported":      []string{"S256"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": testKID, "kty": "RSA", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(signingKey.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(
				big.NewInt(int64(signingKey.PublicKey.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		i.exchange.Add(1)
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if i.refusal != "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": i.refusal, "error_description": "for the test",
			})
			return
		}
		nonce := i.nonce
		if r.Form.Get("grant_type") == "authorization_code" {
			want, ok := i.codes[r.Form.Get("code")]
			// THE PKCE CHECK, PERFORMED BY THE PROVIDER as a real one
			// does: the challenge was recorded at the authorization
			// request and is compared with the hash of the verifier
			// presented here.
			if !ok || challengeOf(r.Form.Get("code_verifier")) != want.challenge {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error": "invalid_grant", "error_description": "pkce",
				})
				return
			}
			// AND THE NONCE IS ECHOED, as a provider does: it is the
			// value that binds the token to THIS login attempt, so a
			// suite whose issuer minted its own would be verifying
			// against a constant.
			nonce = want.nonce
		}
		c := claims()
		c["iss"] = i.Server.URL
		c["nonce"] = nonce
		out := map[string]string{
			"id_token": sign(t, c, jwt.SigningMethodRS256, testKID, signingKey),
		}
		if i.rotate != "" {
			out["refresh_token"] = i.rotate
		} else {
			out["refresh_token"] = "refresh-" + r.Form.Get("code")
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	// TLS, BECAUSE THE PACKAGE REFUSES A PLAIN-HTTP ISSUER. The rule is
	// right — the issuer is where every sign-in's verifying keys come
	// from — so the suite runs against a real https provider and hands
	// the server's own client to the code under test, rather than
	// relaxing the rule for the benefit of a test.
	i.Server = httptest.NewTLSServer(mux)
	t.Cleanup(i.Server.Close)
	return i
}

// authorize is what the provider does when the browser arrives: record the
// challenge and hand back a code.
func (i *issuer) authorize(t *testing.T, redirect string) string {
	t.Helper()
	parsed, err := url.Parse(redirect)
	if err != nil {
		t.Fatalf("the redirect is not a url: %v", err)
	}
	code := "code-" + parsed.Query().Get("state")[:8]
	i.codes[code] = issued{
		challenge: parsed.Query().Get("code_challenge"),
		nonce:     parsed.Query().Get("nonce"),
	}
	return code
}

// challengeOf is RFC 7636's S256 transformation, WRITTEN OUT HERE rather than
// taken from the package under test: a provider computes it independently, and
// a test that borrowed the implementation would pass whatever that
// implementation did.
func challengeOf(verifier string) string {
	if verifier == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (i *issuer) config() oidc.Config {
	c := testConfig()
	c.Issuer = i.Server.URL
	return c
}

// testCipher is the fleet keyring every node holds, which is what lets a login
// begun on one finish on another.
func testCipher(t *testing.T) secrets.Cipher {
	t.Helper()
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1", Keys: map[string][]byte{"k1": key},
	})
	if err != nil {
		t.Fatalf("build a cipher: %v", err)
	}
	return cipher
}

// --- the cases --------------------------------------------------------------- //

// A LOGIN BEGUN ON ONE NODE FINISHES ON ANOTHER.
//
// THE CASE THE WHOLE DESIGN IS SHAPED BY. The two halves of a round trip land
// on whichever nodes a load balancer picks, so a flight kept in a map on the
// starting node fails in proportion to how many nodes are running — and never
// on the single-node deployment anybody tests on. Here the two halves use
// different Providers over one keyring, which is what a fleet is.
func TestALoginBegunOnOneNodeFinishesOnAnother(t *testing.T) {
	t.Parallel()
	idp := newIssuer(t)
	cipher := testCipher(t)
	config := idp.config()

	// Node A starts it and holds nothing afterwards.
	nodeA := oidc.NewProvider(config, idp.Client(), func() time.Time { return at })
	metadata, err := nodeA.Metadata(t.Context())
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	redirect, sealed, err := config.Start(cipher, metadata.AuthorizationEndpoint, "/work", at)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	code := idp.authorize(t, redirect)

	// Node B finishes it, having never seen the first request.
	nodeB := oidc.NewProvider(config, idp.Client(), func() time.Time { return at })
	flight, err := oidc.Open(cipher, sealed, at)
	if err != nil {
		t.Fatalf("a flight sealed on another node did not open: %v", err)
	}
	if flight.Return != "/work" {
		t.Errorf("the return address reads %q", flight.Return)
	}
	tokens, err := config.Exchange(t.Context(), idp.Client(), metadata.TokenEndpoint,
		code, flight.Verifier)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	keys, err := nodeB.Keys(t.Context())
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	got, err := config.Verify(t.Context(), keys, tokens.IDToken, flight.Nonce, at)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Subject != testSubject {
		t.Errorf("the subject reads %q", got.Subject)
	}
	if tokens.Refresh == "" {
		t.Error("no refresh token, so the deactivation probe has nothing to ask with")
	}
}

// THE AUTHORIZATION REQUEST CARRIES S256 AND NEVER THE VERIFIER ITSELF.
//
// RFC 7636 permits sending the verifier as the challenge (`plain`), which
// protects against nothing: the whole point is that an attacker who intercepts
// the authorization request cannot derive the verifier from what they saw.
func TestTheAuthorizationRequestUsesS256AndNeverPlain(t *testing.T) {
	t.Parallel()
	idp := newIssuer(t)
	config := idp.config()
	redirect, sealed, err := config.Start(testCipher(t), idp.Server.URL+"/authorize", "", at)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	query, err := url.Parse(redirect)
	if err != nil {
		t.Fatalf("redirect: %v", err)
	}
	q := query.Query()
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("the challenge method is %q", q.Get("code_challenge_method"))
	}
	if q.Get("code_challenge") == "" {
		t.Fatal("no code challenge at all")
	}
	// The sealed cookie must not be readable, and the challenge must not
	// be the verifier.
	flight, err := oidc.Open(testCipher(t), sealed, at)
	if err == nil {
		t.Fatal("a flight sealed under one keyring opened under another")
	}
	_ = flight
	if strings.Contains(sealed, q.Get("code_challenge")) {
		t.Error("the challenge appears verbatim in the sealed cookie")
	}
	for _, value := range []string{q.Get("state"), q.Get("nonce")} {
		if value == "" {
			t.Error("the authorization request omits state or nonce")
		}
		if strings.Contains(sealed, value) {
			t.Error("a flight value appears verbatim in the cookie, so it is " +
				"signed rather than sealed — and the PKCE verifier beside it " +
				"is exactly what must not be readable")
		}
	}
}

// A CODE REDEEMED WITHOUT THE VERIFIER IS REFUSED BY THE PROVIDER.
//
// PKCE, end to end: the provider hashed the challenge at the authorization
// request, so a code intercepted on the way back cannot be redeemed by
// whoever intercepted it.
func TestACodeCannotBeRedeemedWithoutTheVerifier(t *testing.T) {
	t.Parallel()
	idp := newIssuer(t)
	config := idp.config()
	redirect, sealed, err := config.Start(testCipher(t), idp.Server.URL+"/authorize", "", at)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	code := idp.authorize(t, redirect)
	_ = sealed

	for name, verifier := range map[string]string{
		"no verifier at all": "",
		"somebody else's":    "a-verifier-the-challenge-was-not-made-from",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Exchange(t.Context(), idp.Client(),
				idp.Server.URL+"/token", code, verifier); err == nil {
				t.Error("the provider redeemed a code against the wrong " +
					"verifier, so this case is asserting about a provider " +
					"that is not checking")
			}
		})
	}
}

// A FLIGHT EXPIRES, AND AN EXPIRED ONE IS NOT REDEEMABLE.
func TestAFlightIsNotRedeemableAfterItsWindow(t *testing.T) {
	t.Parallel()
	idp := newIssuer(t)
	cipher := testCipher(t)
	_, sealed, err := idp.config().Start(cipher, idp.Server.URL+"/authorize", "", at)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := oidc.Open(cipher, sealed, at.Add(oidc.FlightTTL-time.Second)); err != nil {
		t.Errorf("a flight inside its window did not open: %v", err)
	}
	if _, err := oidc.Open(cipher, sealed, at.Add(oidc.FlightTTL)); err == nil {
		t.Error("a flight opened at exactly its deadline — the cookie carries " +
			"the PKCE verifier, so its window is how long that sits in a browser")
	}
}

// A SEALED FLIGHT IS BOUND TO ITS PURPOSE.
//
// Every other sealed value in this engine is encrypted under the same keyring.
// Without associated data binding this envelope to the login flow, one of them
// pasted into the login cookie would unseal here.
func TestASealedFlightDoesNotOpenAsAnythingElse(t *testing.T) {
	t.Parallel()
	cipher := testCipher(t)
	elsewhere, err := cipher.Encrypt(`{"state":"s","nonce":"n","verifier":"v"}`,
		secrets.AADForVar("SOMETHING_ELSE"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := oidc.Open(cipher, elsewhere, at); err == nil {
		t.Error("a value sealed for another purpose opened as a login flight")
	}
}

// DISCOVERY REFUSES A DOCUMENT NAMING ANOTHER ISSUER.
//
// Every token comparison afterwards is against the configured issuer, so a
// document naming a different one is either a misconfiguration or somebody's
// redirect — and accepting it would let the metadata decide what the tokens
// are checked against.
func TestDiscoveryRefusesMetadataForAnotherIssuer(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 "https://idp.attacker.example",
			"authorization_endpoint": "https://idp.attacker.example/authorize",
			"token_endpoint":         "https://idp.attacker.example/token",
			"jwks_uri":               "https://idp.attacker.example/jwks",
		})
	}))
	t.Cleanup(server.Close)

	config := testConfig()
	config.Issuer = server.URL
	provider := oidc.NewProvider(config, server.Client(), func() time.Time { return at })
	if _, err := provider.Metadata(t.Context()); err == nil {
		t.Error("metadata naming another issuer was accepted")
	}
}

// A DISCOVERY DOCUMENT MISSING AN ENDPOINT IS REFUSED, NOT HALF-USED.
func TestDiscoveryRefusesADocumentMissingAnEndpoint(t *testing.T) {
	t.Parallel()
	for _, drop := range []string{"authorization_endpoint", "token_endpoint", "jwks_uri"} {
		t.Run(drop, func(t *testing.T) {
			t.Parallel()
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				doc := map[string]any{
					"issuer":                 server.URL,
					"authorization_endpoint": server.URL + "/authorize",
					"token_endpoint":         server.URL + "/token",
					"jwks_uri":               server.URL + "/jwks",
				}
				delete(doc, drop)
				_ = json.NewEncoder(w).Encode(doc)
			}))
			t.Cleanup(server.Close)
			config := testConfig()
			config.Issuer = server.URL
			provider := oidc.NewProvider(config, server.Client(), func() time.Time { return at })
			if _, err := provider.Metadata(t.Context()); err == nil {
				t.Errorf("a document with no %s was accepted", drop)
			}
		})
	}
}
