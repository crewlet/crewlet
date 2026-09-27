package authapi_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
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

	// exchanges counts every request the token endpoint was sent.
	exchanges atomic.Int64

	// noEndSession makes the discovery document publish no
	// end_session_endpoint, as some providers' do not.
	noEndSession bool

	// endSession, when set, is the end_session_endpoint the document
	// publishes in place of the provider's own.
	endSession string

	// discoveryDown makes the discovery document answer 502, as a
	// metadata host that is down does.
	discoveryDown bool

	// groups is the groups claim every ID token carries, or none.
	groups []string

	// authTime is the auth_time every ID token asserts — when the person
	// last authenticated AT THE PROVIDER — or none when zero.
	authTime time.Time
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
		if p.discoveryDown {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		doc := map[string]any{
			"issuer":                                p.URL,
			"authorization_endpoint":                p.URL + "/authorize",
			"token_endpoint":                        p.URL + "/token",
			"jwks_uri":                              p.URL + "/jwks",
			"end_session_endpoint":                  p.URL + "/logout?tenant=acme",
			"code_challenge_methods_supported":      []string{"S256"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		if p.noEndSession {
			delete(doc, "end_session_endpoint")
		}
		if p.endSession != "" {
			doc["end_session_endpoint"] = p.endSession
		}
		_ = json.NewEncoder(w).Encode(doc)
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
		p.exchanges.Add(1)
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
		if !p.authTime.IsZero() {
			claims["auth_time"] = p.authTime.Unix()
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
	rig := newProviderRig(t, idp, b, options)
	started := rig.start(t, "/work")
	code, state := idp.authorize(t, started.Header().Get("Location"))
	return rig.callback(t, started.Result().Cookies(), code, state)
}

// providerRig is one sign-in surface over one provider, and the keyring its
// flights are sealed under — so a case can seal a flight of its own.
type providerRig struct {
	mux    *http.ServeMux
	cipher secrets.Cipher
	config oidc.Config

	// provider is the surface's own, so a case can hold its slots.
	provider *oidc.Provider
}

// newProviderRig builds the surface a provider sign-in runs through, linked
// to [linkedPerson] by the subject the provider asserts.
func newProviderRig(t *testing.T, idp *provider, b config.Bootstrap,
	options func(*authapi.Options)) providerRig {

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
	cfg := oidc.Config{
		Issuer: idp.URL, ClientID: idpClientID, ClientSecret: "not-a-real-secret",
		RedirectURI: b.API.ExternalBase() + auth.PathAuthOIDCCallback,
		// THE CLAIM THE DEPLOYMENT NAMES, as the engine's own wiring
		// hands it over.
		GroupsClaim: b.API.Auth.OIDC.GroupsClaim,
	}
	provider := oidc.NewProvider(cfg, idp.Client(), func() time.Time { return clock })
	svc := buildWith(t, b, provider, func(o *authapi.Options) {
		o.Directory = linkedDirectory{blind: subjectBlind, person: linkedPerson}
		o.Cipher = cipher
		options(o)
	})
	mux := http.NewServeMux()
	svc.Routes(mux)
	return providerRig{mux: mux, cipher: cipher, config: cfg, provider: provider}
}

// start begins a sign-in that returns to the given path.
func (rig providerRig) start(t *testing.T, returnTo string) *httptest.ResponseRecorder {
	t.Helper()
	started := httptest.NewRecorder()
	rig.mux.ServeHTTP(started, httptest.NewRequest(http.MethodGet,
		auth.PathAuthOIDCStart+"?return_to="+url.QueryEscape(returnTo), nil))
	if started.Code != http.StatusFound {
		t.Fatalf("the start answered %d: %s", started.Code, started.Body)
	}
	return started
}

// callback is the browser coming back from the provider carrying cookies.
func (rig providerRig) callback(t *testing.T, cookies []*http.Cookie, code,
	state string) *httptest.ResponseRecorder {

	t.Helper()
	return rig.callbackWithin(t.Context(), t, cookies, code, state)
}

// callbackWithin is [providerRig.callback] on a request whose context is ctx,
// for a browser that leaves before it is answered.
func (rig providerRig) callbackWithin(ctx context.Context, t *testing.T,
	cookies []*http.Cookie, code, state string) *httptest.ResponseRecorder {

	t.Helper()
	callback := httptest.NewRequestWithContext(ctx, http.MethodGet,
		auth.PathAuthOIDCCallback+"?state="+url.QueryEscape(state)+
			"&code="+url.QueryEscape(code), nil)
	callback.RemoteAddr = "198.51.100.7:5100"
	for _, c := range cookies {
		callback.AddCookie(c)
	}
	finished := httptest.NewRecorder()
	rig.mux.ServeHTTP(finished, callback)
	return finished
}

// A SIGN-IN LANDS ON THIS DEPLOYMENT WHATEVER THE FLIGHT CARRIES.
//
// The return path is judged where it arrives, and a flight this surface
// started can carry nothing else — but a flight is opened by whichever node
// the callback reaches, and during a rolling upgrade the node that SEALED it
// may be a build that judged by a looser rule. So the callback judges the path
// again where it leaves, as the redirect: a flight sealed carrying
// `/\evil.example.com` — another host, to a browser — lands on the dashboard.
//
// Mutation: redirect to the flight's path as it came and this lands off-site.
func TestASignInLandsOnThisDeploymentWhateverTheFlightCarries(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendOIDC
	b.API.Auth.OIDC = &config.APIOIDC{Issuer: idp.URL, ClientID: idpClientID}
	rig := newProviderRig(t, idp, b, func(*authapi.Options) {})

	// THE COOKIE'S NAME is this surface's, so a real start supplies it.
	started := rig.start(t, "/work")
	cookies := started.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("the start set %d cookies, want the flight alone", len(cookies))
	}
	// A FLIGHT ANOTHER BUILD SEALED, under the fleet's own keyring.
	redirect, sealed, err := rig.config.Start(rig.cipher, idp.URL+"/authorize",
		oidc.Flight{Return: `/\evil.example.com`}, clock)
	if err != nil {
		t.Fatalf("seal a flight: %v", err)
	}
	cookies[0].Value = sealed
	code, state := idp.authorize(t, redirect)

	finished := rig.callback(t, cookies, code, state)
	if finished.Code != http.StatusFound {
		t.Fatalf("the callback answered %d (%s), want a redirect", finished.Code,
			finished.Body)
	}
	if got := finished.Header().Get("Location"); got != auth.PathDashboard {
		t.Errorf("the sign-in redirected to %q, which a browser follows off "+
			"this deployment; want %s", got, auth.PathDashboard)
	}
}

// A FLIGHT IS EXCHANGED AT THE PROVIDER ONCE, HOWEVER OFTEN IT IS PRESENTED.
//
// Whoever started a flight holds its cookie and its state, and a made-up code
// is free — so without a record of which flights a node has finished, each
// presentation of one cookie was an exchange at somebody else's token
// endpoint, for the flight's whole ten minutes. The second presentation here
// is refused as a failed sign-in, and the provider never hears of it.
//
// Mutation: drop the redemption check and the token endpoint counts two.
func TestARedeemedFlightIsRefusedOnReplay(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendOIDC
	b.API.Auth.OIDC = &config.APIOIDC{Issuer: idp.URL, ClientID: idpClientID}
	audit := &recordingAudit{}
	rig := newProviderRig(t, idp, b, func(o *authapi.Options) { o.Audit = audit })

	started := rig.start(t, "/work")
	code, state := idp.authorize(t, started.Header().Get("Location"))
	cookies := started.Result().Cookies()
	if first := rig.callback(t, cookies, code, state); first.Code != http.StatusFound {
		t.Fatalf("the first callback answered %d (%s), want the sign-in",
			first.Code, first.Body)
	}
	replayed := rig.callback(t, cookies, code, state)
	if replayed.Code != http.StatusUnauthorized {
		t.Errorf("the replayed flight answered %d (%s), want the one sign-in "+
			"refusal", replayed.Code, replayed.Body)
	}
	if n := idp.exchanges.Load(); n != 1 {
		t.Errorf("the provider's token endpoint was asked %d times for one "+
			"flight, want once", n)
	}
	if _, failures := audit.snapshot(); len(failures) != 1 {
		t.Errorf("the trail holds %d failed attempts, want the replay", len(failures))
	}
}

// A BROWSER THAT LEAVES WHILE IT WAITS ITS TURN HAS SPENT NOTHING.
//
// A callback beyond the eight a provider admits at once waits for a slot on
// its own request ([oidc.ExchangeSlots]), which is exactly the morning's wave
// a person reloads a spinning page in. The browser that left is answered 503
// with a Retry-After and counted as no attempt — and its flight was never
// spent, since the provider was never asked: the flight used to be spent
// BEFORE the wait, so the reload presenting the same cookie was refused as a
// replay and counted as a failed attempt, and every reload in the wave burned
// a login. Here eight slots are held, the first callback gives up waiting, and
// the reload of the same cookie, once a slot is free, signs the person in.
//
// Mutation: spend the flight before the slot is taken and the reload is 401
// with a failed attempt; drop the admission's own error arm and the abandoned
// callback is a counted 401 too.
func TestACallbackThatGivesUpWaitingForATurnSpendsNoFlight(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendOIDC
	b.API.Auth.OIDC = &config.APIOIDC{Issuer: idp.URL, ClientID: idpClientID}
	audit := &recordingAudit{}
	rig := newProviderRig(t, idp, b, func(o *authapi.Options) { o.Audit = audit })

	started := rig.start(t, "/work")
	code, state := idp.authorize(t, started.Header().Get("Location"))
	cookies := started.Result().Cookies()

	// THE WAVE: every slot held by somebody else's exchange.
	held := holdEverySlot(t, rig.provider)
	gone, leave := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer leave()
	abandoned := rig.callbackWithin(gone, t, cookies, code, state)
	for _, admission := range held {
		admission.Release()
	}
	if abandoned.Code != http.StatusServiceUnavailable ||
		abandoned.Header().Get("Retry-After") == "" {
		t.Fatalf("a callback that gave up waiting answered %d (Retry-After %q): "+
			"%s, want 503 with a Retry-After", abandoned.Code,
			abandoned.Header().Get("Retry-After"), abandoned.Body)
	}
	if n := idp.exchanges.Load(); n != 0 {
		t.Fatalf("the token endpoint was asked %d times while every slot was "+
			"held, want none", n)
	}

	reloaded := rig.callback(t, cookies, code, state)
	if reloaded.Code != http.StatusFound || reloaded.Header().Get("Location") != "/work" {
		t.Fatalf("the reload of a flight that never reached the provider "+
			"answered %d (%s), want the sign-in", reloaded.Code, reloaded.Body)
	}
	if n := idp.exchanges.Load(); n != 1 {
		t.Errorf("the token endpoint was asked %d times, want the reload's one", n)
	}
	if _, failures := audit.snapshot(); len(failures) != 0 {
		t.Errorf("the trail holds %d failed attempts, want none: nobody "+
			"presented anything wrong", len(failures))
	}
}

// A SPENT FLIGHT IS REFUSED WITHOUT WAITING FOR A TURN.
//
// Whoever called back once holds a spent cookie for its ten minutes, and a
// presentation of it asks the provider nothing — so it has nothing to wait for
// at the token endpoint. Queued for a turn, it took one of the eight to learn
// it would be refused: eight replays at a time kept every slot, and every
// legitimate callback and deactivation probe on the node waited behind them
// for as long as the replays were sent. Here every slot is held by somebody
// else's exchange, and the replay is still refused at once, as the one failed
// sign-in, having asked the provider nothing.
//
// Mutation: drop the check before the turn and the replay waits for a slot it
// never gets, answering 503 when its request ends.
func TestASpentFlightIsRefusedWithoutWaitingForATurn(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendOIDC
	b.API.Auth.OIDC = &config.APIOIDC{Issuer: idp.URL, ClientID: idpClientID}
	audit := &recordingAudit{}
	rig := newProviderRig(t, idp, b, func(o *authapi.Options) { o.Audit = audit })

	started := rig.start(t, "/work")
	code, state := idp.authorize(t, started.Header().Get("Location"))
	cookies := started.Result().Cookies()
	if first := rig.callback(t, cookies, code, state); first.Code != http.StatusFound {
		t.Fatalf("the first callback answered %d (%s), want the sign-in",
			first.Code, first.Body)
	}

	held := holdEverySlot(t, rig.provider)
	defer func() {
		for _, admission := range held {
			admission.Release()
		}
	}()
	// BOUNDED, so the mutation answers rather than hanging the case: a
	// replay refused before the turn never waits at all.
	within, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	replayed := rig.callbackWithin(within, t, cookies, code, state)
	if replayed.Code != http.StatusUnauthorized {
		t.Errorf("a spent flight presented while every turn was held answered "+
			"%d (%s), want the one sign-in refusal at once", replayed.Code,
			replayed.Body)
	}
	if n := idp.exchanges.Load(); n != 1 {
		t.Errorf("the provider's token endpoint was asked %d times for one "+
			"flight, want once", n)
	}
	if _, failures := audit.snapshot(); len(failures) != 1 {
		t.Errorf("the trail holds %d failed attempts, want the replay", len(failures))
	}
}

// A REFUSAL GIVES ITS TURN BACK BEFORE IT WAITS OUT ITS PAD.
//
// Two presentations of one flight that arrive before either is exchanged both
// pass the check before the turn, and the second to hold a turn finds the
// flight spent. It is refused — and every refusal waits out the sign-in pad
// first, four times an exchange, so one refused from inside its turn held one
// of the provider's eight slots for the whole pad having asked the provider
// nothing, which anybody can do as often as they start a flight and call back
// twice at once. Here both presentations wait while every slot is held; one is
// exchanged and signs the person in, the other is parked in its refusal's pad,
// and while it is parked another caller is admitted at once.
//
// NOT PARALLEL, because it knows both presentations are past the check and
// waiting by counting the goroutines parked for a turn in this process, and a
// parallel case's callback waiting beside them would be counted too.
//
// Mutation: give the turn back only once the refusal has answered (the
// deferred release this replaced) and the caller beside the pad is never
// admitted.
func TestARefusalGivesItsTurnBackBeforeItsPad(t *testing.T) {
	idp := newProvider(t)
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendOIDC
	b.API.Auth.OIDC = &config.APIOIDC{Issuer: idp.URL, ClientID: idpClientID}
	padding := make(chan struct{}, 2)
	resume := make(chan struct{})
	throttle, err := credential.NewThrottle(credential.ThrottleDeps{
		Now: func() time.Time { return clock },
		// THE PAD, parked until the case has looked beside it.
		Sleep: func(ctx context.Context, _ time.Duration) {
			padding <- struct{}{}
			select {
			case <-resume:
			case <-ctx.Done():
			}
		},
	})
	if err != nil {
		t.Fatalf("credential.NewThrottle: %v", err)
	}
	audit := &recordingAudit{}
	rig := newProviderRig(t, idp, b, func(o *authapi.Options) {
		o.Throttle = throttle
		o.Audit = audit
	})
	started := rig.start(t, "/work")
	code, state := idp.authorize(t, started.Header().Get("Location"))
	cookies := started.Result().Cookies()

	held := holdEverySlot(t, rig.provider)
	answers := make([]*httptest.ResponseRecorder, 2)
	var presented sync.WaitGroup
	for i := range answers {
		presented.Go(func() { answers[i] = rig.callback(t, cookies, code, state) })
	}
	// BOTH ARE PAST THE CHECK BEFORE THE TURN, which nothing can spend the
	// flight ahead of while every turn is held.
	waitUntil(t, func() bool { return waitingForATurn() == len(answers) })
	held[0].Release()
	select {
	case <-padding:
	case <-time.After(5 * time.Second):
		t.Fatal("neither presentation of the flight was refused")
	}
	beside, cancel := context.WithTimeout(t.Context(), time.Second)
	admission, err := rig.provider.Admit(beside)
	cancel()
	if err != nil {
		t.Error("a caller beside a refusal's pad waited for the turn the " +
			"refusal still held, having asked the provider nothing")
	} else {
		admission.Release()
	}
	close(resume)
	presented.Wait()
	for _, admission := range held[1:] {
		admission.Release()
	}

	codes := []int{answers[0].Code, answers[1].Code}
	slices.Sort(codes)
	if !slices.Equal(codes, []int{http.StatusFound, http.StatusUnauthorized}) {
		t.Errorf("two presentations of one flight answered %v, want one "+
			"sign-in and one refusal", codes)
	}
	if n := idp.exchanges.Load(); n != 1 {
		t.Errorf("the provider's token endpoint was asked %d times for one "+
			"flight, want once", n)
	}
	if _, failures := audit.snapshot(); len(failures) != 1 {
		t.Errorf("the trail holds %d failed attempts, want the refused "+
			"presentation", len(failures))
	}
}

// holdEverySlot takes every one of a provider's turns at its token endpoint,
// as a wave of somebody else's exchanges would. The caller releases them.
func holdEverySlot(t *testing.T, provider *oidc.Provider) []*oidc.Admission {
	t.Helper()
	held := make([]*oidc.Admission, 0, oidc.ExchangeSlots)
	for range oidc.ExchangeSlots {
		admission, err := provider.Admit(t.Context())
		if err != nil {
			t.Fatalf("hold a slot: %v", err)
		}
		held = append(held, admission)
	}
	return held
}

// waitingForATurn counts the goroutines in this process parked waiting for a
// turn at a provider's token endpoint. A request gives no other sign that it
// has reached the wait, and the wait is the one place a case about the order
// around it has to know a request is.
func waitingForATurn() int {
	stacks := make([]byte, 1<<20)
	for {
		n := runtime.Stack(stacks, true)
		if n < len(stacks) {
			stacks = stacks[:n]
			break
		}
		stacks = make([]byte, 2*len(stacks))
	}
	return strings.Count(string(stacks), "/internal/iam/oidc.(*Provider).Admit(")
}

// waitUntil polls a condition for up to five seconds.
func waitUntil(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatal("the condition was never reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A PROVIDER SUBJECT TWO PEOPLE HOLD IS A CONFLICT, NOT AN OUTAGE.
//
// A restore can leave one provider account linked to two people, and the
// sign-in then resolves to neither — the subject is the whole of what it
// proves. It was answered as `503 identity_unavailable` with a Retry-After,
// which told the browser to wait out a state that waiting never ends: only an
// administrator removing one of the links does. It is a definite refusal —
// 409 `subject_conflict`, no Retry-After, no session — that names neither
// holder to the caller, and it is one failed attempt on the trail.
//
// The control is a directory that genuinely cannot be read, which IS an
// outage and keeps its 503 and its Retry-After.
func TestAProviderSubjectTwoPeopleHoldIsAConflictNotAnOutage(t *testing.T) {
	t.Parallel()
	const (
		first  = "0192f00d-0000-7000-8000-0000000000a1"
		second = "0192f00d-0000-7000-8000-0000000000b2"
	)
	round := func(err error) (*httptest.ResponseRecorder, *recordingAudit,
		config.Bootstrap) {

		t.Helper()
		idp := newProvider(t)
		b := bootstrapFor(t)
		b.API.Auth.Backend = config.AuthBackendOIDC
		b.API.Auth.OIDC = &config.APIOIDC{Issuer: idp.URL, ClientID: idpClientID}
		audit := &recordingAudit{}
		finished := signInThroughProvider(t, idp, b, func(o *authapi.Options) {
			o.Directory = failingSubjects{err: err}
			o.Audit = audit
		})
		return finished, audit, b
	}
	finished, audit, b := round(fmt.Errorf("%w: %s, %s",
		iamdomain.ErrSubjectAmbiguous, first, second))
	if finished.Code != http.StatusConflict {
		t.Fatalf("an ambiguous subject answered %d (%s), want 409", finished.Code,
			finished.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(finished.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal is not JSON: %v (%s)", err, finished.Body)
	}
	if body["error"] != string(httpjson.CodeSubjectConflict) {
		t.Errorf("the refusal's code is %v, want %s", body["error"],
			httpjson.CodeSubjectConflict)
	}
	if after := finished.Header().Get("Retry-After"); after != "" {
		t.Errorf("the refusal carries Retry-After %q, so a browser retries a "+
			"state only an administrator can end", after)
	}
	if raw := finished.Body.String(); strings.Contains(raw, first) ||
		strings.Contains(raw, second) {
		t.Errorf("the refusal names a holder to the caller: %s", raw)
	}
	for _, c := range finished.Result().Cookies() {
		if c.Name == session.CookieName(b.API.ExternalBase()) && c.Value != "" {
			t.Error("an ambiguous subject was handed a session")
		}
	}
	if _, failures := audit.snapshot(); len(failures) != 1 ||
		failures[0].Subject == "" {
		t.Errorf("the trail holds %+v, want the one refused attempt naming the "+
			"provider subject", failures)
	}

	// THE CONTROL: an unreadable directory is an outage.
	unread, _, _ := round(errors.New("the replicated estate is not open"))
	if unread.Code != http.StatusServiceUnavailable ||
		unread.Header().Get("Retry-After") == "" {
		t.Errorf("an unreadable directory answered %d with Retry-After %q, want "+
			"503 and a time to come back", unread.Code,
			unread.Header().Get("Retry-After"))
	}
}

// failingSubjects answers every provider subject with one error.
type failingSubjects struct {
	stubDirectory
	err error
}

func (d failingSubjects) PersonBySubjectBlind(context.Context, string, time.Time) (
	iamdomain.Sighting, error) {

	return iamdomain.Sighting{}, d.err
}

// THE FLIGHT COOKIE IS `__Host-` ON HTTPS, AND ONLY THAT NAME IS READ THERE.
//
// A browser sets a `__Host-` cookie only from this exact host, Secure, at
// `Path=/` and with no Domain, so no sibling host can write one; the bare name
// any sibling can write with a Domain covering this host. The flight is what
// the callback finishes — a flight a sibling planted would be one its author
// began, finished with the provider account the victim's browser holds — so on
// https it is set under the prefix and a flight under the bare name is not
// read at all. Plain http can hold no prefixed cookie, so it takes the bare
// name, as the session cookie does.
//
// Mutation: drop the prefix and the https case fails on the name; read the
// bare name as well and the planted flight is finished.
func TestTheFlightCookieIsHostPrefixedOnHTTPS(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendOIDC
	b.API.Auth.OIDC = &config.APIOIDC{Issuer: idp.URL, ClientID: idpClientID}
	rig := newProviderRig(t, idp, b, func(*authapi.Options) {})

	started := rig.start(t, "/work")
	cookies := started.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("the start set %d cookies, want the flight", len(cookies))
	}
	flight := cookies[0]
	if flight.Name != "__Host-crewlet_oidc_flight" || flight.Path != "/" ||
		!flight.Secure || flight.Domain != "" {
		t.Errorf("an https deployment set the flight as %q, Path=%q, Secure=%v, "+
			"Domain=%q — want __Host-crewlet_oidc_flight at / , Secure, no Domain",
			flight.Name, flight.Path, flight.Secure, flight.Domain)
	}

	// THE SAME FLIGHT UNDER THE BARE NAME, as a sibling host would plant it.
	code, state := idp.authorize(t, started.Header().Get("Location"))
	planted := *flight
	planted.Name = "crewlet_oidc_flight"
	refused := rig.callback(t, []*http.Cookie{&planted}, code, state)
	if refused.Code != http.StatusUnauthorized {
		t.Errorf("a flight under the bare name answered %d on https, want the "+
			"sign-in refusal", refused.Code)
	}
	if n := idp.exchanges.Load(); n != 0 {
		t.Errorf("a flight under the bare name reached the provider %d times", n)
	}

	// AND PLAIN HTTP TAKES THE BARE NAME, which is all it can hold.
	plain := bootstrapFor(t)
	plain.API.ExternalURL = "http://127.0.0.1:8080"
	plain.API.Auth.Backend = config.AuthBackendOIDC
	plain.API.Auth.OIDC = b.API.Auth.OIDC
	onHTTP := newProviderRig(t, idp, plain, func(*authapi.Options) {}).start(t, "/work")
	if c := onHTTP.Result().Cookies(); len(c) != 1 || c[0].Name != "crewlet_oidc_flight" ||
		c[0].Secure || c[0].Path != "/" {
		t.Errorf("a plain http deployment set %+v, want the bare name at /, not Secure", c)
	}
}
