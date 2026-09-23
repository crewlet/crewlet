package authapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/secrets"
)

// A PROVIDER SIGN-IN IS PROVED WHEN THE PROVIDER SAYS IT WAS.
//
// A provider answers from its own session whenever it can, so a token that
// arrives now may carry an authentication from last week. The session used to
// be stamped "proved now" whatever the token said, which handed every step-up
// window to a week-old authentication at the provider. The proof is the ID
// token's `auth_time`; a token asserting none proved nothing this engine can
// date, which every step-up window reads as stale.
func TestAProviderSignInIsProvedWhenTheProviderSaysItWas(t *testing.T) {
	t.Parallel()
	for name, authTime := range map[string]time.Time{
		"a week-old provider session": clock.Add(-7 * 24 * time.Hour),
		"no auth_time at all":         {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			idp := newProvider(t)
			idp.authTime = authTime
			b := bootstrapFor(t)
			b.API.Auth.Backend = config.AuthBackendOIDC
			b.API.Auth.OIDC = &config.APIOIDC{Issuer: idp.URL, ClientID: idpClientID}
			writer := &sessionRecorder{}
			finished := signInThroughProvider(t, idp, b, func(o *authapi.Options) {
				o.Writer = writer
			})
			if finished.Code != http.StatusFound {
				t.Fatalf("the callback answered %d (%s)", finished.Code, finished.Body)
			}
			starts := writer.opened()
			if len(starts) != 1 {
				t.Fatalf("opened %d sessions", len(starts))
			}
			if got := starts[0].ProvedAt; !got.Equal(authTime) {
				t.Errorf("the session was proved at %s, want the provider's "+
					"own instant %s — not the instant the token arrived", got,
					authTime)
			}
		})
	}
}

// providerEstate is [estate] reached through a provider subject as well.
type providerEstate struct {
	*estate
	blind string
}

func (e providerEstate) PersonBySubjectBlind(_ context.Context, blind string,
	_ time.Time) (iamdomain.Sighting, error) {

	if blind != e.blind {
		return iamdomain.Sighting{}, nil
	}
	return e.person, nil
}

// providerStepUp is one signed-in person, holding a live session and linked to
// the provider's subject, with a surface to confirm them through.
type providerStepUp struct {
	idp      *provider
	mux      *http.ServeMux
	estate   *estate
	audit    *recordingAudit
	lineage  uuid.UUID
	cookie   string
	absolute time.Time
}

func newProviderStepUp(t *testing.T) *providerStepUp {
	t.Helper()
	idp := newProvider(t)
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendOIDC
	b.API.Auth.OIDC = &config.APIOIDC{Issuer: idp.URL, ClientID: idpClientID}
	blind, err := fixtureBlinder(t).Subject(idp.URL, "subject-42")
	if err != nil {
		t.Fatal(err)
	}
	e := &estate{person: iamdomain.Sighting{
		ID: "0192f00d-0000-7000-8000-00000000000a", Kind: iam.KindPerson,
		Stage: iam.StageActive, Login: "jane.doe",
	}}
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
	audit := &recordingAudit{}
	identity := session.Identity{
		Applied: ^uint64(0) >> 1, Generation: 2,
		Session: session.SessionRow{Found: true, Epoch: 3,
			ProvedAt: clock.Add(-2 * time.Hour)},
		Person: session.PersonRow{Found: true, Epoch: 3,
			Stage: iam.StageActive, Login: "jane.doe"},
	}
	svc := buildWith(t, b, oidc.NewProvider(oidc.Config{
		Issuer: idp.URL, ClientID: idpClientID, ClientSecret: "not-a-real-secret",
		RedirectURI: b.API.ExternalBase() + auth.PathAuthOIDCCallback,
	}, idp.Client(), func() time.Time { return clock }), func(o *authapi.Options) {
		o.Directory = providerEstate{estate: e, blind: blind}
		o.Writer = e
		o.Sessions = rows{identity}
		o.Cipher = cipher
		o.Audit = audit
	})
	mux := http.NewServeMux()
	svc.Routes(mux)

	absolute := clock.Add(72 * time.Hour)
	lineage := uuid.Must(uuid.NewV7())
	millis := clock.Add(-2 * time.Hour).UnixMilli()
	for i := range 6 {
		lineage[i] = byte(millis >> (8 * (5 - i)))
	}
	cookie, err := fixtureSigner(t).Mint(session.Mint{
		Lineage: lineage, Person: e.person.ID, StartPosition: 1,
		Epoch: 3, Generation: 2, AbsoluteExpiresAt: absolute,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return &providerStepUp{idp: idp, mux: mux, estate: e, audit: audit,
		lineage: lineage, cookie: cookie, absolute: absolute}
}

// confirm runs a step-up start and, when it redirects, the provider and the
// callback — as the signed-in person when signedIn, presenting their cookie.
func (r *providerStepUp) confirm(t *testing.T, signedIn bool) (
	started, finished *httptest.ResponseRecorder) {

	t.Helper()
	start := httptest.NewRequest(http.MethodGet,
		auth.PathAuthOIDCStart+"?step_up=true&return_to=/settings", nil)
	start.AddCookie(&http.Cookie{Name: session.CookieBaseName, Value: r.cookie})
	if signedIn {
		start = start.WithContext(iam.WithPrincipal(start.Context(), iam.Principal{
			ID:    uuid.MustParse(r.estate.person.ID),
			Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive,
		}))
	}
	started = httptest.NewRecorder()
	r.mux.ServeHTTP(started, start)
	if started.Code != http.StatusFound {
		return started, nil
	}
	code, state := r.idp.authorize(t, started.Header().Get("Location"))
	callback := httptest.NewRequest(http.MethodGet, auth.PathAuthOIDCCallback+
		"?state="+url.QueryEscape(state)+"&code="+url.QueryEscape(code), nil)
	callback.RemoteAddr = "198.51.100.7:5100"
	callback.AddCookie(&http.Cookie{Name: session.CookieBaseName, Value: r.cookie})
	for _, c := range started.Result().Cookies() {
		callback.AddCookie(c)
	}
	finished = httptest.NewRecorder()
	r.mux.ServeHTTP(finished, callback)
	return started, finished
}

// A PROVIDER STEP-UP CONFIRMS AT THE PROVIDER AND REPLACES THE SESSION.
//
// Somebody who signs in only through their provider holds no password here,
// so the password step-up has nothing to verify — and with the surfaces that
// change a company asking for a recent proof, such a person could otherwise
// never make one of those changes. The confirmation is the provider's own:
// asked with `prompt=login` and `max_age`, accepted on an `auth_time` inside
// the window, and recorded the way the password step-up records one — the
// session it was made from ENDED first, the replacement keeping its absolute
// deadline and proved at the provider's instant.
func TestAProviderStepUpConfirmsAtTheProviderAndReplacesTheSession(t *testing.T) {
	t.Parallel()
	r := newProviderStepUp(t)
	r.idp.authTime = clock.Add(-30 * time.Second)
	started, finished := r.confirm(t, true)
	if started.Code != http.StatusFound {
		t.Fatalf("the start answered %d (%s)", started.Code, started.Body)
	}
	asked, err := url.Parse(started.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	window := strconv.Itoa(int(config.DefaultSessionStepUp / time.Second))
	if asked.Query().Get("prompt") != "login" || asked.Query().Get("max_age") != window {
		t.Errorf("the provider was asked prompt=%q max_age=%q, want login and %s",
			asked.Query().Get("prompt"), asked.Query().Get("max_age"), window)
	}
	if finished == nil || finished.Code != http.StatusFound ||
		finished.Header().Get("Location") != "/settings" {
		t.Fatalf("the callback answered %+v, want a redirect to /settings", finished)
	}
	r.estate.mu.Lock()
	closes, starts := slices.Clone(r.estate.closes), slices.Clone(r.estate.starts)
	r.estate.mu.Unlock()
	if len(closes) != 1 || closes[0].lineage != r.lineage.String() {
		t.Errorf("ended %+v, want exactly the session the confirmation was "+
			"made from", closes)
	}
	if len(starts) != 1 {
		t.Fatalf("opened %d sessions, want the replacement", len(starts))
	}
	if !starts[0].ProvedAt.Equal(r.idp.authTime) {
		t.Errorf("the replacement was proved at %s, want the provider's %s",
			starts[0].ProvedAt, r.idp.authTime)
	}
	if !starts[0].AbsoluteExpiresAt.Equal(r.absolute) {
		t.Errorf("the replacement ends at %s, want the replaced session's %s",
			starts[0].AbsoluteExpiresAt, r.absolute)
	}
	emitted, _ := r.audit.snapshot()
	confirmed := false
	for _, e := range emitted {
		if up, ok := e.(types.IAMStepUpCompleted); ok &&
			up.Replaces == r.lineage.String() {
			confirmed = true
		}
	}
	if !confirmed {
		t.Errorf("announced %#v, want a step-up naming the session it replaced",
			emitted)
	}
}

// A PROVIDER STEP-UP THE PROVIDER DID NOT CONFIRM CHANGES NOTHING.
//
// `max_age` makes `auth_time` required in the answer, so a token without one,
// or one saying the person authenticated outside the window, is a provider
// that answered from a session it already had — not a confirmation. And the
// start is a signed-in person's gesture: nobody signed in has nothing to
// confirm.
func TestAProviderStepUpTheProviderDidNotConfirmChangesNothing(t *testing.T) {
	t.Parallel()
	for name, authTime := range map[string]time.Time{
		"no auth_time":            {},
		"outside the window":      clock.Add(-2 * time.Hour),
		"just outside the window": clock.Add(-config.DefaultSessionStepUp - time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newProviderStepUp(t)
			r.idp.authTime = authTime
			_, finished := r.confirm(t, true)
			if finished == nil || finished.Code != http.StatusUnauthorized {
				t.Fatalf("the callback answered %+v, want 401", finished)
			}
			if len(r.estate.closes) != 0 || len(r.estate.starts) != 0 {
				t.Errorf("an unconfirmed step-up ended %+v and opened %+v",
					r.estate.closes, r.estate.starts)
			}
		})
	}
	t.Run("nobody signed in", func(t *testing.T) {
		t.Parallel()
		r := newProviderStepUp(t)
		started, _ := r.confirm(t, false)
		if started.Code != http.StatusUnauthorized {
			t.Errorf("a step-up start with nobody signed in answered %d, want 401",
				started.Code)
		}
	})
}
