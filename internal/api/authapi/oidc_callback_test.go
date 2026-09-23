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
	"slices"
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

// PersonBySubjectBlind answers the LINK: a sign-in resolves the provider's
// subject through the link an invitation or an administrator made, never an
// address the provider asserted.
func (d linkedDirectory) PersonBySubjectBlind(_ context.Context, blind string,
	_ time.Time) (iamdomain.Sighting, error) {

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

	// groups is the groups claim every ID token carries, or none.
	groups []string
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
		claims := jwt.MapClaims{
			"iss": p.URL, "aud": idpClientID, "sub": "subject-42",
			"nonce": nonce,
			"exp":   clock.Add(time.Hour).Unix(),
			"iat":   clock.Add(-time.Minute).Unix(),
		}
		if len(p.groups) > 0 {
			claims["groups"] = p.groups
		}
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
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
	audit := &recordingAudit{}
	finished := signInThroughProvider(t, idp, b, func(o *authapi.Options) {
		o.Audit = audit
	})

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
	if !ok || started2.Method != types.SignInOIDC || started2.Person != linkedPerson.ID ||
		started2.Remote != "198.51.100.7" {
		t.Errorf("announced %#v", emitted[0])
	}
}

// A PROVIDER'S GROUPS RIDE THE SESSION THEY OPENED, AND ONLY THAT SESSION.
//
// The group mapping turns the ID token's groups claim into grants. The
// callback used to merge them into the sighting it signed in and then drop
// them — nothing downstream reads a sighting's grants — so no mapping ever
// conferred anything. They are recorded on the session record, where the
// guard unions them with the person's declared set at decision time, and
// never on the person, so they lapse with the session that presented them.
func TestAProvidersGroupsRideTheSessionTheyOpened(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	idp.groups = []string{"engineering", "oncall", "a-team-nobody-mapped"}
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendOIDC
	b.API.Auth.OIDC = &config.APIOIDC{
		Issuer: idp.URL, ClientID: idpClientID, GroupsClaim: "groups",
		GroupGrants: map[string][]iam.Grant{
			"engineering": {iam.GrantWorkWrite},
			"oncall":      {iam.GrantWorkWrite, iam.GrantStateRead},
		},
	}
	writer := &sessionRecorder{}
	finished := signInThroughProvider(t, idp, b, func(o *authapi.Options) {
		o.Writer = writer
	})
	if finished.Code != http.StatusFound {
		t.Fatalf("the callback answered %d (%s)", finished.Code, finished.Body)
	}
	starts := writer.opened()
	if len(starts) != 1 {
		t.Fatalf("opened %d sessions, want one", len(starts))
	}
	want := []iam.Grant{iam.GrantWorkWrite, iam.GrantStateRead}
	if got := starts[0].GroupGrants; !slices.Equal(got, want) {
		t.Errorf("the session carries %v, want %v: the mapped union of the "+
			"groups the provider asserted", got, want)
	}
}

// sessionRecorder is a writer that remembers every session it opened.
type sessionRecorder struct {
	stubWriter
	mu     sync.Mutex
	starts []iamdomain.SessionStart
}

func (w *sessionRecorder) OpenSession(ctx context.Context,
	in iamdomain.SessionStart) (iamdomain.SessionOpened, error) {

	w.mu.Lock()
	w.starts = append(w.starts, in)
	w.mu.Unlock()
	return w.stubWriter.OpenSession(ctx, in)
}

func (w *sessionRecorder) opened() []iamdomain.SessionStart {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.starts)
}

// linkedPerson is who the provider's subject is linked to.
var linkedPerson = iamdomain.Sighting{
	ID: "0192f00d-0000-7000-8000-00000000004c", Kind: iam.KindPerson,
	Stage: iam.StageActive, Login: "sam.okoro",
}

// signInThroughProvider runs one whole round trip — the start, the provider,
// the callback — from /work, and answers the callback's response.
func signInThroughProvider(t *testing.T, idp *provider, b config.Bootstrap,
	options func(*authapi.Options)) *httptest.ResponseRecorder {

	t.Helper()
	// THE LINK IS KEYED ON THE SUBJECT'S BLIND under the surface's own
	// blinder, so the directory answers only for the subject this provider
	// asserts.
	subjectBlind, err := fixtureBlinder(t).Subject(idp.URL, "subject-42")
	if err != nil {
		t.Fatal(err)
	}
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
		// THE CLAIM THE DEPLOYMENT NAMES, as the engine's own wiring
		// hands it over.
		GroupsClaim: b.API.Auth.OIDC.GroupsClaim,
	}, idp.Client(), func() time.Time { return clock }), func(o *authapi.Options) {
		o.Directory = linkedDirectory{blind: subjectBlind, person: linkedPerson}
		o.Cipher = cipher
		options(o)
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
	return finished
}
