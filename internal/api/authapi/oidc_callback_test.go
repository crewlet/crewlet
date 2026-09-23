package authapi_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/secrets"

	"github.com/golang-jwt/jwt/v5"
)

// linkedDirectory holds one person, reached by the blind of the provider
// subject they are linked to — the only way a callback resolves anybody.
type linkedDirectory struct {
	stubDirectory
	blind  string
	person iamdomain.Sighting
}

func (d linkedDirectory) PersonByEmailBlind(_ context.Context, blind string) (
	iamdomain.Sighting, error) {

	if blind != d.blind {
		return iamdomain.Sighting{}, nil
	}
	return d.person, nil
}

// provider is an OpenID provider in a TLS test server: discovery, one key and
// a token endpoint that echoes the nonce the authorization request carried.
type provider struct {
	*httptest.Server
	key *rsa.PrivateKey

	mu     sync.Mutex
	nonces map[string]string // code -> nonce
}

const idpClientID = "crewlet"

func newProvider(t *testing.T) *provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &provider{key: key, nonces: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc(oidc.MetadataPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.URL,
			"authorization_endpoint":                p.URL + "/authorize",
			"token_endpoint":                        p.URL + "/token",
			"jwks_uri":                              p.URL + "/jwks",
			"code_challenge_methods_supported":      []string{"S256"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "k1", "kty": "RSA", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(
				big.NewInt(int64(key.PublicKey.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		nonce := p.nonces[r.Form.Get("code")]
		p.mu.Unlock()
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": p.URL, "aud": idpClientID, "sub": "subject-42",
			"nonce": nonce,
			"exp":   clock.Add(time.Hour).Unix(),
			"iat":   clock.Add(-time.Minute).Unix(),
		})
		token.Header["kid"] = "k1"
		raw, err := token.SignedString(key)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id_token": raw, "refresh_token": "refresh-" + r.Form.Get("code"),
		})
	})
	p.Server = httptest.NewTLSServer(mux)
	t.Cleanup(p.Close)
	return p
}

// authorize is the provider meeting the browser: it remembers the nonce and
// hands back a code.
func (p *provider) authorize(t *testing.T, redirect string) (code, state string) {
	t.Helper()
	parsed, err := url.Parse(redirect)
	if err != nil {
		t.Fatalf("the start redirected to %q: %v", redirect, err)
	}
	state = parsed.Query().Get("state")
	code = "code-for-" + state
	p.mu.Lock()
	p.nonces[code] = parsed.Query().Get("nonce")
	p.mu.Unlock()
	return code, state
}

// A SIGN-IN THROUGH AN IDENTITY PROVIDER ENDS WHERE IT BEGAN, BY REDIRECT, AND
// SAYS HOW IT WAS PROVED.
//
// The callback is a browser following the provider's redirect. It used to
// answer the JSON body the password route answers — which a browser renders as
// text and goes nowhere from — followed by a dead `if flight.Return != ""`
// that was meant to be the redirect. The return path is the one checked at the
// start and sealed into the flight, so the case starts at `/work` and must end
// there, holding the session cookie.
//
// Mutation: answer the JSON body and the status is 200 with no Location.
func TestAnIdentityProviderSignInRedirectsBackAndIsAnnounced(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendOIDC
	b.API.Auth.OIDC = &config.APIOIDC{Issuer: idp.URL, ClientID: idpClientID}
	person := iamdomain.Sighting{
		ID: "0192f00d-0000-7000-8000-00000000004c", Kind: iam.KindPerson,
		Stage: iam.StageActive, Login: "sam.okoro",
	}
	audit := &recordingAudit{}
	// A REAL KEYRING, because the flight is a cookie: the fixture's
	// pass-through cipher leaves JSON in it, which net/http refuses to
	// carry, exactly as a browser would.
	material, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1", Keys: map[string][]byte{"k1": material},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := buildWith(t, b, oidc.NewProvider(oidc.Config{
		Issuer: idp.URL, ClientID: idpClientID, ClientSecret: "not-a-real-secret",
		RedirectURI: b.API.ExternalBase() + auth.PathAuthOIDCCallback,
	}, idp.Client(), func() time.Time { return clock }), func(o *authapi.Options) {
		o.Directory = linkedDirectory{
			// stubBlinder's own spelling of the subject's blind.
			blind:  "subject:" + idp.URL + "|subject-42",
			person: person,
		}
		o.Audit, o.Cipher = audit, cipher
	})
	mux := http.NewServeMux()
	svc.Routes(mux)

	start := httptest.NewRequest(http.MethodGet,
		auth.PathAuthOIDCStart+"?return_to=/work", nil)
	started := httptest.NewRecorder()
	mux.ServeHTTP(started, start)
	if started.Code != http.StatusFound {
		t.Fatalf("the start answered %d: %s", started.Code, started.Body)
	}
	code, state := idp.authorize(t, started.Header().Get("Location"))

	callback := httptest.NewRequest(http.MethodGet, auth.PathAuthOIDCCallback+
		"?state="+url.QueryEscape(state)+"&code="+url.QueryEscape(code), nil)
	callback.RemoteAddr = "198.51.100.7:5100"
	for _, c := range started.Result().Cookies() {
		callback.AddCookie(c)
	}
	finished := httptest.NewRecorder()
	mux.ServeHTTP(finished, callback)

	if finished.Code != http.StatusFound || finished.Header().Get("Location") != "/work" {
		t.Fatalf("the callback answered %d to %q (%s), want a redirect to /work",
			finished.Code, finished.Header().Get("Location"), finished.Body)
	}
	bearer := false
	for _, c := range finished.Result().Cookies() {
		if c.Name == session.CookieName(b.API.ExternalBase()) && c.Value != "" {
			bearer = true
		}
	}
	if !bearer {
		t.Error("the redirect carries no session cookie, so the browser arrives signed out")
	}
	emitted, failures := audit.snapshot()
	if len(failures) != 0 {
		t.Errorf("a sign-in that succeeded was counted as failing: %+v", failures)
	}
	if len(emitted) != 1 {
		t.Fatalf("announced %d events, want the one session", len(emitted))
	}
	started2, ok := emitted[0].(types.IAMSessionStarted)
	if !ok || started2.Method != types.SignInOIDC || started2.Person != person.ID ||
		started2.Remote != "198.51.100.7" {
		t.Errorf("announced %#v", emitted[0])
	}
}
