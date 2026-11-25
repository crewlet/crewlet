package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcp/oauth"
	"github.com/crewlet/crewlet/internal/secrets"
)

// The test-only consumer of the consent grant: a product registered under a
// kind no real product uses, signing in at a fake Google.

const (
	consentScope       = "https://www.googleapis.com/auth/consent-test.readonly"
	consentSecretVar   = "GOOGLE_OAUTH_CLIENT_SECRET"
	consentSecretValue = "placeholder-client-secret"
)

// consentScope2 is the test product's second scope: a sign-in asks for a set.
const consentScope2 = "https://www.googleapis.com/auth/consent-test.manage"

// consentScopes is the test product's scopes, whatever the company.
func consentScopes(*config.Company) []string { return []string{consentScope, consentScope2} }

// consentRegistered keeps each test kind registered once per test binary.
var consentRegistered sync.Map

// consentProduct registers a consent product of kind with client id, turned on
// while on holds, and answers kind.
func consentProduct(t *testing.T, kind, id string, on *atomic.Bool) integration.Kind {
	t.Helper()
	if _, again := consentRegistered.LoadOrStore(kind, true); again {
		t.Fatalf("consent kind %s registered twice", kind)
	}
	googleauth.RegisterConsent(googleauth.Consent{Kind: integration.Kind(kind), Name: "Consent " + kind,
		Scopes: consentScopes, Client: func(*config.Company) (googleauth.ConsentClient, bool) {
			return googleauth.ConsentClient{ID: id, Secret: "${" + consentSecretVar + "}"}, on == nil || on.Load()
		}})
	return integration.Kind(kind)
}

// fakeGoogle is Google's consent endpoints and one API behind them: a code is
// exchanged once with its verifier, as the client it was issued to, and a
// refresh token renews until it is revoked or refused.
type fakeGoogle struct {
	srv *httptest.Server

	mu         sync.Mutex
	challenges map[string]string // code -> challenge
	refresh    map[string]bool   // refresh tokens that renew
	serial     int
	exchanges  int
	renewals   int
	revoked    []string
	// The vendor's moods: no refresh token on the exchange, another scope
	// granted, every renewal refused, a new refresh token on each renewal.
	noRefresh, otherScope, refuse, rotate bool
	// api is every bearer the API was sent; current is the access token it
	// takes, "" to refuse every one.
	api     []string
	current string
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	g := &fakeGoogle{challenges: map[string]string{}, refresh: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", g.token)
	mux.HandleFunc("/revoke", func(_ http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		g.mu.Lock()
		defer g.mu.Unlock()
		g.revoked = append(g.revoked, r.PostForm.Get("token"))
		delete(g.refresh, r.PostForm.Get("token"))
	})
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		g.api = append(g.api, bearer)
		if g.current == "" || bearer != g.current {
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGoogle) endpoints() *mcp.OAuthEndpoints {
	return &mcp.OAuthEndpoints{Server: g.srv.URL, Issuer: g.srv.URL, Authorization: g.srv.URL + "/auth",
		Token: g.srv.URL + "/token", Revocation: g.srv.URL + "/revoke", PKCEMethods: []string{"S256"},
		TokenAuthMethods: []string{"client_secret_basic", "client_secret_post"}}
}

// authorize is the person consenting: it holds the authorization address to
// Google's terms and answers the code and state the browser carries back.
func (g *fakeGoogle) authorize(t *testing.T, address, clientID string) (code, state string) {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for name, want := range map[string]string{"access_type": "offline", "prompt": "consent",
		"include_granted_scopes": "false", "scope": consentScope + " " + consentScope2, "code_challenge_method": "S256",
		"client_id": clientID, "response_type": "code",
		"redirect_uri": "https://crewlet.example.com/webhooks/mcp-oauth"} {
		if got := q[name]; len(got) != 1 || got[0] != want {
			t.Fatalf("the authorization address's %s = %q, want only %q", name, got, want)
		}
	}
	if q.Has("resource") {
		t.Fatalf("Google was sent resource=%q", q.Get("resource"))
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.serial++
	code = fmt.Sprintf("code-%d", g.serial)
	g.challenges[code] = q.Get("code_challenge")
	return code, q.Get("state")
}

func (g *fakeGoogle) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	g.mu.Lock()
	defer g.mu.Unlock()
	refuse := func(code, description string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": description})
	}
	if id, secret, ok := r.BasicAuth(); !ok || secret != consentSecretValue || !strings.HasSuffix(id, ".apps.googleusercontent.com") {
		refuse("invalid_client", "Unauthorized")
		return
	}
	if r.PostForm.Has("resource") {
		refuse("invalid_request", "Parameter not allowed for this message type: resource")
		return
	}
	// GOOGLE NAMES WHAT IT GRANTED IN ITS OWN ORDER, which a set compares alike.
	scope := consentScope2 + " " + consentScope
	if g.otherScope {
		scope += " https://www.googleapis.com/auth/other.readonly"
	}
	answer := map[string]any{"token_type": "Bearer", "expires_in": 3599, "scope": scope}
	g.serial++
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		challenge, ok := g.challenges[r.PostForm.Get("code")]
		delete(g.challenges, r.PostForm.Get("code"))
		if !ok || oauth.ChallengeOf(r.PostForm.Get("code_verifier")) != challenge {
			refuse("invalid_grant", "Malformed auth code.")
			return
		}
		g.exchanges++
		if !g.noRefresh {
			rt := fmt.Sprintf("1//0refresh-%d-%s", g.serial, strings.Repeat("x", 64))
			g.refresh[rt] = true
			answer["refresh_token"] = rt
		}
	case "refresh_token":
		g.renewals++
		presented := r.PostForm.Get("refresh_token")
		if g.refuse || !g.refresh[presented] {
			// GOOGLE'S WORDS, with a token-shaped run the record must not keep.
			refuse("invalid_grant", "Token has been expired or revoked. 1//0g"+strings.Repeat("Ab3dE6gH9jK2", 6))
			return
		}
		if g.rotate {
			delete(g.refresh, presented)
			rt := fmt.Sprintf("1//0rotated-%d-%s", g.serial, strings.Repeat("y", 64))
			g.refresh[rt] = true
			answer["refresh_token"] = rt
		}
	default:
		refuse("unsupported_grant_type", "")
		return
	}
	access := fmt.Sprintf("ya29.access-%d", g.serial)
	g.current = access
	answer["access_token"] = access
	writeJSONBody(w, answer)
}

func (g *fakeGoogle) count() (exchanges, renewals int, revoked []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.exchanges, g.renewals, slices.Clone(g.revoked)
}

// consentClock is a fleet's one adjustable clock.
type consentClock struct{ at atomic.Pointer[time.Time] }

func (c *consentClock) now() time.Time { return *c.at.Load() }
func (c *consentClock) advance(d time.Duration) {
	next := c.now().Add(d)
	c.at.Store(&next)
}

// consentNodes are two nodes of one fleet sharing its stores, keyring and
// clock, both signing in at g.
func consentNodes(t *testing.T, g *fakeGoogle) (*Engine, *Engine, *consentClock) {
	t.Helper()
	company := companyFor(t, "name: Acme\n")
	fleet, backend := coordmem.NewFleet(), coordmem.New()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": []byte("crewlet-test-sealing-key-32bytes")}})
	if err != nil {
		t.Fatal(err)
	}
	boot := &config.Bootstrap{Secrets: config.Secrets{ActiveKeyID: "k1", Keys: []config.SecretKey{
		{ID: "k1", Material: base64.StdEncoding.EncodeToString([]byte("crewlet-test-signing-key-32bytes"))}}}}
	clock := &consentClock{}
	start := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	clock.at.Store(&start)
	node := func(id string) *Engine {
		e := &Engine{backends: &Backends{Fleet: fleet, Coord: backend}, cipher: cipher, boot: boot,
			testNode: id, webhookBase: "https://crewlet.example.com", externalBase: "https://crewlet.example.com",
			environ: config.MapSource{consentSecretVar: consentSecretValue}, clock: clock.now}
		e.oauth.consentAt = g.endpoints()
		e.epoch.current.Store(company)
		return e
	}
	return node("node-a"), node("node-b"), clock
}

// consentSignIn signs kind in through a, finishing through b.
func consentSignIn(t *testing.T, g *fakeGoogle, a, b *Engine, kind integration.Kind, clientID string) {
	t.Helper()
	started, err := a.StartIntegrationSignIn(t.Context(), kind, signingJane)
	if err != nil {
		t.Fatalf("StartIntegrationSignIn: %v", err)
	}
	code, state := g.authorize(t, started.AuthorizeURL, clientID)
	landed, err := b.CompleteSignIn(t.Context(), SignInLanding{State: state, Code: code, Issuer: g.srv.URL})
	if err != nil || landed.Name != string(kind) || !landed.Integration {
		t.Fatalf("CompleteSignIn = %+v, %v", landed, err)
	}
}

// sealedRefresh is the refresh token kind's estate row holds, "" for none.
func sealedRefresh(t *testing.T, e *Engine, kind integration.Kind) string {
	t.Helper()
	held, found, err := fleetsecrets.New(e.backends.Fleet, e.cipher).Estate().Held(t.Context(),
		"integration/"+string(kind)+"/refresh")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return ""
	}
	var grant sealedGrant
	if err := json.Unmarshal([]byte(held.Value), &grant); err != nil {
		t.Fatal(err)
	}
	return grant.Refresh
}

// callAPI sends one request to g's API with kind's grant through e.
func callAPI(t *testing.T, e *Engine, g *fakeGoogle, kind integration.Kind) (int, error) {
	t.Helper()
	cred, err := e.IntegrationCredential(t.Context(), kind)
	if err != nil {
		return 0, err
	}
	resp, err := cred.Do(t.Context(), func(ctx context.Context, token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.srv.URL+"/api", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return httpxtest.Pool(t).Do(req)
	})
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

const clientOne = "123456789012-consentone.apps.googleusercontent.com"

// A CONSENT SIGN-IN ASKS GOOGLE FOR ONE SCOPE, OFFLINE, AND KEEPS THE GRANT
// WHERE NO OPERATOR REACHES IT: the authorization address carries Google's own
// parameters and no resource; the callback on another node exchanges the code
// as the company's client, seals the refresh token at
// `integration/<kind>/refresh` under the integration owner, records who signed
// in on Crewlet and the scopes granted, and its token reaches the API. The MCP
// family's records are untouched.
//
// Mutation: drop ConsentParams from the start, or record the grant under the
// MCP family, and the case goes red.
func TestAConsentSignInAsksForItsScopesOfflineAndSealsTheGrant(t *testing.T) {
	t.Parallel()
	g := newFakeGoogle(t)
	a, b, _ := consentNodes(t, g)
	kind := consentProduct(t, "consent_sign_in", clientOne, nil)
	consentSignIn(t, g, a, b, kind, clientOne)

	if rt := sealedRefresh(t, a, kind); !strings.HasPrefix(rt, "1//0refresh-") {
		t.Fatalf("the sealed row holds %q, want the refresh token Google issued", rt)
	}
	record, found, err := a.IntegrationGrant(t.Context(), kind)
	if err != nil || !found || record.ClientID != clientOne || record.By != signingJane.Name ||
		!slices.Equal(record.Scopes, []string{consentScope2, consentScope}) || record.Resource != "" {
		t.Fatalf("IntegrationGrant = %+v, %t, %v", record, found, err)
	}
	signIns, err := a.backends.Fleet.ListOAuthSignIns(t.Context(), coord.IntegrationGrantFamily)
	if err != nil || len(signIns) != 1 || signIns[0].Status != coord.OAuthSignInApproved || signIns[0].Sealed != "" {
		t.Fatalf("the sign-in reads %+v (%v), want approved with nothing sealed left", signIns, err)
	}
	if mcpGrants, err := a.MCPGrants(t.Context()); err != nil || len(mcpGrants) != 0 {
		t.Fatalf("the MCP family holds %+v (%v)", mcpGrants, err)
	}
	if finding, err := b.IntegrationGrantFinding(t.Context(), kind); err != nil || finding != nil {
		t.Fatalf("a renewing grant reports %+v (%v)", finding, err)
	}
	if status, err := callAPI(t, b, g, kind); err != nil || status != http.StatusOK {
		t.Fatalf("the API answered %d (%v)", status, err)
	}
	if err := a.RecordIntegrationAccounts(t.Context(), kind, []string{"pub-2", "pub-1"}); err != nil {
		t.Fatal(err)
	}
	if record, _, _ := b.IntegrationGrant(t.Context(), kind); !slices.Equal(record.Accounts, []string{"pub-1", "pub-2"}) {
		t.Fatalf("the accounts reached read %q", record.Accounts)
	}
}

// A CONSENT WITHOUT A REFRESH TOKEN, OR WITH ANOTHER SCOPE, IS REFUSED AND
// KEPT NOWHERE: nothing could renew the first, and the second is not the
// grant asked for. The second is revoked at Google only where that ends no
// live grant: Google's revocation ends every grant the person made to the
// client's project, so beside a grant of another product in that project, or
// over this product's own grant, it is left unrevoked and the person is told
// how to end it, and the live grant keeps working.
//
// Mutation: drop the scope check from the consent kind's accept, and the
// second sign-in seals a grant; let keepFresh answer "" always, and the
// shared cases revoke.
func TestAConsentWithoutARefreshTokenOrWithAnotherScopeIsRefused(t *testing.T) {
	t.Parallel()
	const clientShared = "123456789012-refusedshared.apps.googleusercontent.com"
	for _, c := range []struct {
		mood, live string
		revoked    int
	}{
		{mood: "no refresh token"},
		{mood: "another scope", revoked: 1},
		{mood: "another scope beside a grant in its project", live: "other"},
		{mood: "another scope over its own grant", live: "own"},
	} {
		t.Run(c.mood, func(t *testing.T) {
			t.Parallel()
			g := newFakeGoogle(t)
			a, b, _ := consentNodes(t, g)
			slug := strings.ReplaceAll(c.mood, " ", "_")
			kind := consentProduct(t, "consent_refused_"+slug, clientOne, nil)
			live := kind
			switch c.live {
			case "other":
				live = consentProduct(t, "consent_refused_live_"+slug, clientShared, nil)
				consentSignIn(t, g, a, b, live, clientShared)
			case "own":
				consentSignIn(t, g, a, b, kind, clientOne)
			}
			g.mu.Lock()
			g.noRefresh, g.otherScope = c.mood == "no refresh token", c.mood != "no refresh token"
			g.mu.Unlock()
			started, err := a.StartIntegrationSignIn(t.Context(), kind, signingJane)
			if err != nil {
				t.Fatal(err)
			}
			code, state := g.authorize(t, started.AuthorizeURL, clientOne)
			_, err = b.CompleteSignIn(t.Context(), SignInLanding{State: state, Code: code})
			var refused *SignInRefusal
			if !errors.As(err, &refused) {
				t.Fatalf("CompleteSignIn = %v, want a refusal", err)
			}
			if s, _ := a.IntegrationSignIn(t.Context(), started.SignIn.ID); s.Status != coord.OAuthSignInFailed {
				t.Fatalf("the sign-in reads %s, want failed", s.Status)
			}
			if _, _, revoked := g.count(); len(revoked) != c.revoked {
				t.Fatalf("the refused grant was revoked %d times, want %d", len(revoked), c.revoked)
			}
			if c.live == "" {
				if rt := sealedRefresh(t, a, kind); rt != "" {
					t.Fatalf("a refused consent sealed %q", rt)
				}
				if _, found, _ := a.IntegrationGrant(t.Context(), kind); found {
					t.Fatal("a refused consent was recorded")
				}
				return
			}
			if !strings.Contains(refused.Reason, "myaccount.google.com") {
				t.Fatalf("an unrevoked refusal says %q, want how to end the access at Google", refused.Reason)
			}
			g.mu.Lock()
			g.otherScope = false
			g.mu.Unlock()
			if status, err := callAPI(t, b, g, live); err != nil || status != http.StatusOK {
				t.Fatalf("the live grant answered %d (%v) after the refusal", status, err)
			}
		})
	}
}

// A 401 IS ONE RENEWAL AND ONE RETRY, and a refresh token Google hands back
// replaces the sealed one at the version read: a token the API refuses is
// renewed once and the request sent once more, never looped, on any node.
//
// Mutation: retry Do without asking Rejected, or drop the swap of a returned
// refresh token, and the case goes red.
func TestAConsentRenewsOnceOnA401AndKeepsAReturnedRefreshToken(t *testing.T) {
	t.Parallel()
	g := newFakeGoogle(t)
	g.rotate = true
	a, b, clock := consentNodes(t, g)
	kind := consentProduct(t, "consent_renewal", clientOne, nil)
	consentSignIn(t, g, a, b, kind, clientOne)
	first := sealedRefresh(t, a, kind)
	if status, err := callAPI(t, a, g, kind); err != nil || status != http.StatusOK {
		t.Fatalf("the first call answered %d (%v)", status, err)
	}
	_, before, _ := g.count()
	// THE TOKEN GOES BAD AT GOOGLE past the fresh window: one renewal, one retry.
	clock.advance(time.Minute)
	g.mu.Lock()
	g.current = "ya29.not-the-one-sent"
	g.mu.Unlock()
	status, err := callAPI(t, a, g, kind)
	_, after, _ := g.count()
	if err != nil || status != http.StatusOK || after != before+1 {
		t.Fatalf("a refused token answered %d (%v) after %d renewals, want 200 after one", status, err, after-before)
	}
	if rotated := sealedRefresh(t, b, kind); rotated == first || !strings.HasPrefix(rotated, "1//0rotated-") {
		t.Fatalf("the sealed row holds %q after Google returned a new refresh token", rotated)
	}
	// AN API THAT REFUSES EVERY TOKEN is sent twice and renewed once.
	clock.advance(time.Minute)
	g.mu.Lock()
	sentBefore := len(g.api)
	g.mu.Unlock()
	cred, err := a.IntegrationCredential(t.Context(), kind)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := cred.Do(t.Context(), func(ctx context.Context, token string) (*http.Response, error) {
		g.mu.Lock()
		g.current = ""
		g.mu.Unlock()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.srv.URL+"/api", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		return httpxtest.Pool(t).Do(req)
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	g.mu.Lock()
	sent := len(g.api) - sentBefore
	g.mu.Unlock()
	if _, renewals, _ := g.count(); resp.StatusCode != http.StatusUnauthorized || sent != 2 || renewals != after+1 {
		t.Fatalf("an API refusing every token answered %d after %d sends and %d renewals, want 401, 2 and 1",
			resp.StatusCode, sent, renewals-after)
	}
}

// AN INVALID_GRANT ASKS GOOGLE NO MORE UNTIL A SIGN-IN: the refusal is recorded
// on the grant in Google's words, redacted, with the Testing reading about a
// week after the sign-in; it reports credential_rejected; no node renews
// again; and a new sign-in clears it.
//
// Mutation: skip the Failed check before a renewal, or drop the Testing
// reading, and the case goes red.
func TestAnInvalidGrantAsksGoogleNoMoreUntilASignIn(t *testing.T) {
	t.Parallel()
	g := newFakeGoogle(t)
	a, b, clock := consentNodes(t, g)
	kind := consentProduct(t, "consent_invalid_grant", clientOne, nil)
	consentSignIn(t, g, a, b, kind, clientOne)
	// B HOLDS ITS CREDENTIAL ACROSS THE REFUSAL, as a product's client does.
	held, err := b.IntegrationCredential(t.Context(), kind)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := held.source.Token(t.Context()); err != nil {
		t.Fatalf("b's first token: %v", err)
	}
	clock.advance(7 * 24 * time.Hour)
	g.mu.Lock()
	g.refuse = true
	g.mu.Unlock()
	if _, err := callAPI(t, a, g, kind); !errors.Is(err, ErrIntegrationSignInNeeded) {
		t.Fatalf("a refused renewal answered %v, want a sign-in needed", err)
	}
	record, _, _ := a.IntegrationGrant(t.Context(), kind)
	if !record.Failed() || !strings.Contains(record.Failure, "invalid_grant") ||
		!strings.Contains(record.Failure, googleauth.TestingModeHint) || strings.Contains(record.Failure, "1//0g") {
		t.Fatalf("the grant's failure reads %q", record.Failure)
	}
	finding, err := b.IntegrationGrantFinding(t.Context(), kind)
	if err != nil || finding == nil || finding.Kind != integration.FindingCredentialRejected {
		t.Fatalf("the finding is %+v (%v), want credential_rejected", finding, err)
	}
	_, renewals, _ := g.count()
	for _, e := range []*Engine{a, b} {
		if _, err := callAPI(t, e, g, kind); !errors.Is(err, ErrIntegrationSignInNeeded) {
			t.Fatalf("a refused grant answered %v", err)
		}
	}
	// b's held token ran out long since: its renewal reads the refusal and
	// asks Google nothing.
	if _, err := held.source.Token(t.Context()); !errors.Is(err, ErrIntegrationSignInNeeded) {
		t.Fatalf("a held credential renewed past the refusal: %v", err)
	}
	if _, again, _ := g.count(); again != renewals {
		t.Fatalf("Google was asked %d more times after it refused the grant", again-renewals)
	}
	g.mu.Lock()
	g.refuse = false
	g.mu.Unlock()
	consentSignIn(t, g, b, a, kind, clientOne)
	if status, err := callAPI(t, a, g, kind); err != nil || status != http.StatusOK {
		t.Fatalf("after a new sign-in the API answered %d (%v)", status, err)
	}
}

// SIGNING IN AGAIN REVOKES NOTHING AND KEEPS NO OLD TOKEN: Google's revocation
// ends every grant to the client's project, the new one too, so the new
// refresh token replaces the old in the estate and the old is held nowhere.
//
// Mutation: let the consent kind revoke a replaced grant, and Google is asked
// to revoke.
func TestSigningInAgainRevokesNothingAndKeepsNoOldToken(t *testing.T) {
	t.Parallel()
	g := newFakeGoogle(t)
	a, b, _ := consentNodes(t, g)
	kind := consentProduct(t, "consent_again", clientOne, nil)
	consentSignIn(t, g, a, b, kind, clientOne)
	first := sealedRefresh(t, a, kind)
	before, _, _ := a.IntegrationGrant(t.Context(), kind)
	consentSignIn(t, g, b, a, kind, clientOne)
	second := sealedRefresh(t, a, kind)
	after, _, _ := a.IntegrationGrant(t.Context(), kind)
	if _, _, revoked := g.count(); len(revoked) != 0 {
		t.Fatalf("signing in again revoked %d tokens at Google", len(revoked))
	}
	if second == first || second == "" || after.Grant == before.Grant {
		t.Fatalf("the estate holds %q (was %q), grant %s (was %s)", second, first, after.Grant, before.Grant)
	}
}

// A DISCONNECT REVOKES AT GOOGLE ONLY WHEN NO OTHER LIVE GRANT SHARES THE
// CLIENT'S PROJECT: either way the sealed row and the record go, but two
// products signed in through clients of one project are one revocation, so
// the first to go leaves Google alone and names the other, and the last
// revokes.
//
// Mutation: revoke regardless of the project, or compare client IDs instead
// of project numbers, and the case goes red.
func TestADisconnectRevokesOnlyWhenNoOtherGrantSharesTheProject(t *testing.T) {
	t.Parallel()
	g := newFakeGoogle(t)
	a, b, _ := consentNodes(t, g)
	const (
		clientShared = "123456789012-consenttwo.apps.googleusercontent.com"
		clientApart  = "987654321098-consentthree.apps.googleusercontent.com"
	)
	one := consentProduct(t, "consent_disconnect_one", clientOne, nil)
	two := consentProduct(t, "consent_disconnect_two", clientShared, nil)
	apart := consentProduct(t, "consent_disconnect_apart", clientApart, nil)
	consentSignIn(t, g, a, b, one, clientOne)
	consentSignIn(t, g, a, b, two, clientShared)
	consentSignIn(t, g, a, b, apart, clientApart)

	out, err := a.SignOutIntegration(t.Context(), one, signingJane)
	if err != nil || out.Revocation != RevokeShared || !slices.Equal(out.Shared, []string{string(two)}) {
		t.Fatalf("the first of a project's grants signed out with %+v (%v)", out, err)
	}
	if _, _, revoked := g.count(); len(revoked) != 0 {
		t.Fatalf("a shared project's grant was revoked at Google: %d", len(revoked))
	}
	if sealedRefresh(t, a, one) != "" {
		t.Fatal("the signed-out grant's row is still sealed")
	}
	if _, found, _ := a.IntegrationGrant(t.Context(), one); found {
		t.Fatal("the signed-out grant is still recorded")
	}
	for _, kind := range []integration.Kind{apart, two} {
		out, err := b.SignOutIntegration(t.Context(), kind, signingJane)
		if err != nil || out.Revocation != RevokedAtVendor {
			t.Fatalf("%s, alone in its project, signed out with %+v (%v)", kind, out, err)
		}
	}
	if _, _, revoked := g.count(); len(revoked) != 2 {
		t.Fatalf("Google revoked %d grants, want the two alone in their projects", len(revoked))
	}
	if _, err := a.SignOutIntegration(t.Context(), one, signingJane); !errors.Is(err, ErrIntegrationNotSignedIn) {
		t.Fatalf("a second sign-out answered %v", err)
	}
}

// A CONSENT NEEDS A PUBLIC ADDRESS AND ITS INTEGRATION ON: without
// api.external_url there is no callback and Google offers no device sign-in,
// so the start says why; an integration turned off starts no sign-in and its
// grant is not sent.
//
// Mutation: drop the callback check from the start, and a sign-in is begun
// that can never come back.
func TestAConsentNeedsAPublicAddressAndItsIntegrationOn(t *testing.T) {
	t.Parallel()
	g := newFakeGoogle(t)
	a, b, _ := consentNodes(t, g)
	on := &atomic.Bool{}
	on.Store(true)
	kind := consentProduct(t, "consent_needs", clientOne, on)
	consentSignIn(t, g, a, b, kind, clientOne)

	bare := &Engine{backends: a.backends, cipher: a.cipher, boot: a.boot, testNode: "node-c",
		environ: a.environ, clock: a.clock}
	bare.oauth.consentAt = g.endpoints()
	bare.epoch.current.Store(a.Company())
	if _, err := bare.StartIntegrationSignIn(t.Context(), kind, signingJane); !errors.Is(err, ErrNoConsentCallback) ||
		!strings.Contains(err.Error(), "api.external_url") {
		t.Fatalf("a start without a public address answered %v", err)
	}
	on.Store(false)
	var refused *SignInRefusal
	if _, err := a.StartIntegrationSignIn(t.Context(), kind, signingJane); !errors.As(err, &refused) {
		t.Fatalf("a start of an integration turned off answered %v", err)
	}
	if _, err := a.IntegrationCredential(t.Context(), kind); !errors.Is(err, ErrIntegrationSignInNeeded) {
		t.Fatalf("an integration turned off handed out its grant (%v)", err)
	}
	if _, err := a.StartIntegrationSignIn(t.Context(), "consent_never_registered", signingJane); !errors.Is(err, ErrNoConsentProduct) {
		t.Fatalf("a kind no consent product is answered %v", err)
	}
}

// A GRANT GOES ONLY WITH THE CLIENT IT WAS MADE AS: Google renews a refresh
// token for the client it was issued to alone, so once the configuration
// names another client the grant is held back for a new sign-in rather than
// sent with the wrong secret.
//
// Mutation: drop the client ID comparison from the consent kind's usable, and
// the grant is handed out under the new client.
func TestAConsentGrantGoesOnlyWithTheClientItWasMadeAs(t *testing.T) {
	t.Parallel()
	g := newFakeGoogle(t)
	a, b, _ := consentNodes(t, g)
	const kind = "consent_client_moved"
	var id atomic.Pointer[string]
	first := clientOne
	id.Store(&first)
	if _, again := consentRegistered.LoadOrStore(kind, true); again {
		t.Fatal("registered twice")
	}
	googleauth.RegisterConsent(googleauth.Consent{Kind: kind, Name: "Moved", Scopes: consentScopes,
		Client: func(*config.Company) (googleauth.ConsentClient, bool) {
			return googleauth.ConsentClient{ID: *id.Load(), Secret: "${" + consentSecretVar + "}"}, true
		}})
	consentSignIn(t, g, a, b, kind, clientOne)
	if _, err := a.IntegrationCredential(t.Context(), kind); err != nil {
		t.Fatalf("the grant under its own client: %v", err)
	}
	moved := "555555555555-another.apps.googleusercontent.com"
	id.Store(&moved)
	if _, err := b.IntegrationCredential(t.Context(), kind); !errors.Is(err, ErrIntegrationSignInNeeded) ||
		!strings.Contains(err.Error(), "client changed") {
		t.Fatalf("the grant under another client answered %v", err)
	}
}

// A CONSENT PRODUCT WITH NO SIGN-IN CLIENT SAYS SO, not that nobody signed in:
// the finding names the shared client, whose subject the Google page answers
// with its client form; with a client and no grant, it waits for a sign-in.
//
// Mutation: drop the client check from IntegrationGrantFinding, and the first
// case reads as a sign-in nobody made.
func TestAConsentProductWithNoClientAsksForTheClientFirst(t *testing.T) {
	t.Parallel()
	g := newFakeGoogle(t)
	a, _, _ := consentNodes(t, g)
	for _, c := range []struct {
		kind, client, subject string
	}{
		{"consent_no_client", "", SignInClientSubject},
		{"consent_with_client", clientOne, ""},
	} {
		kind := consentProduct(t, c.kind, c.client, nil)
		f, err := a.IntegrationGrantFinding(t.Context(), kind)
		if err != nil || f == nil || f.Kind != integration.FindingCredentialMissing || f.Subject != c.subject {
			t.Errorf("%s: finding = %+v, %v; want credential_missing on %q", c.kind, f, err, c.subject)
		}
	}
}

// grantFleet is the fleet store unreadGrants wraps.
type grantFleet interface{ coord.Fleet }

// unreadGrants is a fleet whose integration grants cannot be read.
type unreadGrants struct{ grantFleet }

func (unreadGrants) OAuthGrant(context.Context, coord.GrantFamily, string) (coord.OAuthGrant, bool, error) {
	return coord.OAuthGrant{}, false, errors.New("the store did not answer")
}

// A GRANT THE STORE COULD NOT READ IS NOT A GRANT NOBODY HOLDS: recording the
// accounts a grant reached raises the store's failure, and says nobody
// signed in only when the store answered so.
//
// Mutation: join the read failure with ErrIntegrationNotSignedIn, and the
// first case reads as nobody signed in.
func TestAnUnreadGrantIsNotOneNobodyHolds(t *testing.T) {
	t.Parallel()
	g := newFakeGoogle(t)
	a, _, _ := consentNodes(t, g)
	kind := consentProduct(t, "consent_unread", clientOne, nil)
	unread := &Engine{backends: &Backends{Fleet: unreadGrants{a.backends.Fleet}, Coord: a.backends.Coord}}
	if err := unread.RecordIntegrationAccounts(t.Context(), kind, []string{"pub-1"}); err == nil ||
		errors.Is(err, ErrIntegrationNotSignedIn) {
		t.Fatalf("an unread grant answered %v, want the store's failure alone", err)
	}
	if err := a.RecordIntegrationAccounts(t.Context(), kind, []string{"pub-1"}); !errors.Is(err, ErrIntegrationNotSignedIn) {
		t.Fatalf("a grant nobody holds answered %v", err)
	}
}
