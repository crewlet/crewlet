package authapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
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

	// other, once it holds a sighting, is who the subject resolves to in
	// place of the estate's own person: somebody other than whoever holds
	// the session cookie.
	other *atomic.Pointer[iamdomain.Sighting]
}

func (e providerEstate) PersonBySubjectBlind(_ context.Context, blind string,
	_ time.Time) (iamdomain.Sighting, error) {

	if blind != e.blind {
		return iamdomain.Sighting{}, nil
	}
	if other := e.other.Load(); other != nil {
		return *other, nil
	}
	return e.person, nil
}

// providerStepUp is one signed-in person, holding a live session and linked to
// the provider's subject, with a surface to confirm them through.
type providerStepUp struct {
	idp      *provider
	mux      *http.ServeMux
	estate   *estate
	other    *atomic.Pointer[iamdomain.Sighting]
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
	other := &atomic.Pointer[iamdomain.Sighting]{}
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
		o.Directory = providerEstate{estate: e, blind: blind, other: other}
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
	return &providerStepUp{idp: idp, mux: mux, estate: e, other: other,
		audit: audit, lineage: lineage, cookie: cookie, absolute: absolute}
}

// signedIn is the start resolved as the guard resolves the person holding
// the cookie: an unguarded route is handed what the guard found.
func (r *providerStepUp) signedIn(ctx context.Context) context.Context {
	return iam.WithPrincipal(ctx, iam.Principal{
		ID:    uuid.MustParse(r.estate.person.ID),
		Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive,
	})
}

// confirm runs a step-up start carrying query, resolved by as, and — when it
// redirects — the provider and the callback, presenting the person's cookie
// on both legs.
func (r *providerStepUp) confirm(t *testing.T, query string,
	as func(context.Context) context.Context) (started, finished *httptest.ResponseRecorder) {

	t.Helper()
	start := httptest.NewRequest(http.MethodGet,
		auth.PathAuthOIDCStart+"?"+query+"&return_to=/settings", nil)
	start.AddCookie(&http.Cookie{Name: session.CookieBaseName, Value: r.cookie})
	start = start.WithContext(as(start.Context()))
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

// changes is what the estate was asked to end and open, read under its lock.
func (r *providerStepUp) changes() ([]closedSession, []iamdomain.SessionStart) {
	r.estate.mu.Lock()
	defer r.estate.mu.Unlock()
	return slices.Clone(r.estate.closes), slices.Clone(r.estate.starts)
}

// A PROVIDER STEP-UP CONFIRMS AT THE PROVIDER AND REPLACES THE SESSION.
//
// Somebody who signs in only through their provider holds no password here,
// so the password step-up has nothing to verify — and with the surfaces that
// change a company asking for a recent proof, such a person could otherwise
// never make one of those changes. The confirmation is the provider's own:
// asked with `prompt=login` and `max_age` — the WINDOW the refusal named, in
// seconds — accepted on an `auth_time` inside it, and recorded the way the
// password step-up records one: the session it was made from ENDED first, the
// replacement keeping its absolute deadline and proved at the provider's
// instant.
func TestAProviderStepUpConfirmsAtTheProviderAndReplacesTheSession(t *testing.T) {
	t.Parallel()
	for window, maxAge := range map[iam.Recency]time.Duration{
		iam.RecencyStepUp:    config.DefaultSessionStepUp,
		iam.RecencySensitive: config.DefaultSessionStepUpSensitive,
	} {
		t.Run(string(window), func(t *testing.T) {
			t.Parallel()
			r := newProviderStepUp(t)
			r.idp.authTime = clock.Add(-30 * time.Second)
			started, finished := r.confirm(t, "step_up="+string(window), r.signedIn)
			if started.Code != http.StatusFound {
				t.Fatalf("the start answered %d (%s)", started.Code, started.Body)
			}
			asked, err := url.Parse(started.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			want := strconv.Itoa(int(maxAge / time.Second))
			if asked.Query().Get("prompt") != "login" || asked.Query().Get("max_age") != want {
				t.Errorf("the provider was asked prompt=%q max_age=%q, want login "+
					"and %s — the window the refusal named",
					asked.Query().Get("prompt"), asked.Query().Get("max_age"), want)
			}
			if finished == nil || finished.Code != http.StatusFound ||
				finished.Header().Get("Location") != "/settings" {
				t.Fatalf("the callback answered %+v, want a redirect to /settings", finished)
			}
			closes, starts := r.changes()
			if len(closes) != 1 || closes[0].lineage != r.lineage.String() {
				t.Errorf("ended %+v, want exactly the session the confirmation "+
					"was made from", closes)
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
				t.Errorf("announced %#v, want a step-up naming the session it "+
					"replaced", emitted)
			}
		})
	}
}

// A PROVIDER STEP-UP THE PROVIDER DID NOT CONFIRM CHANGES NOTHING.
//
// `max_age` makes `auth_time` required in the answer, so a token without one,
// or one saying the person authenticated outside the window ASKED FOR, is a
// provider that answered from a session it already had — not a confirmation.
// The sensitive window's cases are the ones the review measured: a provider
// that ignores `prompt=login` and honours `max_age` answered a confirmation
// asked with the ordinary hour from a session half an hour old, which was
// accepted here and then refused by the sensitive gesture it was for.
//
// Mutation: seal the ordinary window whatever the start named, and the
// half-hour-old authentication is accepted.
func TestAProviderStepUpTheProviderDidNotConfirmChangesNothing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		window   iam.Recency
		authTime time.Time
	}{
		"no auth_time":            {iam.RecencyStepUp, time.Time{}},
		"outside the window":      {iam.RecencyStepUp, clock.Add(-2 * time.Hour)},
		"just outside the window": {iam.RecencyStepUp, clock.Add(-config.DefaultSessionStepUp - time.Second)},
		"half an hour old, asked for the sensitive window": {iam.RecencySensitive,
			clock.Add(-30 * time.Minute)},
		"just outside the sensitive window": {iam.RecencySensitive,
			clock.Add(-config.DefaultSessionStepUpSensitive - time.Second)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newProviderStepUp(t)
			r.idp.authTime = tc.authTime
			_, finished := r.confirm(t, "step_up="+string(tc.window), r.signedIn)
			if finished == nil || finished.Code != http.StatusUnauthorized {
				t.Fatalf("the callback answered %+v, want 401", finished)
			}
			if closes, starts := r.changes(); len(closes) != 0 || len(starts) != 0 {
				t.Errorf("an unconfirmed step-up ended %+v and opened %+v",
					closes, starts)
			}
		})
	}
}

// A PROVIDER STEP-UP IS ASKED BY A SIGNED-IN PERSON, AND A NODE THAT CANNOT
// TELL WHO THAT IS SAYS SO.
//
// The start is unguarded, so the guard hands it whatever it resolved rather
// than refusing: nobody, a machine, a token acting as a person, or — on a node
// whose identity estate it cannot read — nothing it could establish. The last
// used to answer 401, which a browser reads as "sign in again", discarding a
// cookie that was fine on a node that merely could not vouch for it; it is
// 503 with a Retry-After, as it is on every other route. A machine and a
// token are refused naming the window, as every `step_up_required` is.
//
// Mutation: answer the unknown arm with the anonymous one's 401.
func TestAProviderStepUpStartAsksWhoIsSignedIn(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		as     func(r *providerStepUp) func(context.Context) context.Context
		status int
		code   string
	}{
		"nobody signed in": {func(*providerStepUp) func(context.Context) context.Context {
			return iam.WithAnonymous
		}, http.StatusUnauthorized, "invalid_token"},
		"a node that cannot tell": {func(*providerStepUp) func(context.Context) context.Context {
			return func(ctx context.Context) context.Context {
				return iam.WithUnresolved(ctx, errors.New("the identity estate is behind"))
			}
		}, http.StatusServiceUnavailable, "identity_unavailable"},
		"a machine": {func(*providerStepUp) func(context.Context) context.Context {
			return func(ctx context.Context) context.Context {
				return iam.WithPrincipal(ctx, iam.Principal{
					ID: uuid.New(), Login: "token:ops", Kind: iam.KindMachine,
					Stage: iam.StageActive,
				})
			}
		}, http.StatusForbidden, "step_up_required"},
		"a machine token acting as the person": {func(r *providerStepUp) func(context.Context) context.Context {
			return func(ctx context.Context) context.Context {
				return iam.WithPrincipal(ctx, iam.Principal{
					ID:    uuid.MustParse(r.estate.person.ID),
					Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive,
					Via: iam.MachineTokenName("0192f00d-0000-7000-8000-0000000000aa"),
				})
			}
		}, http.StatusForbidden, "step_up_required"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newProviderStepUp(t)
			started, _ := r.confirm(t, "step_up="+string(iam.RecencySensitive), tc.as(r))
			if started.Code != tc.status {
				t.Fatalf("the start answered %d (%s), want %d", started.Code,
					started.Body, tc.status)
			}
			var body map[string]any
			if err := json.Unmarshal(started.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["error"] != tc.code {
				t.Errorf("error %v, want %s", body["error"], tc.code)
			}
			switch tc.status {
			case http.StatusServiceUnavailable:
				if started.Header().Get("Retry-After") == "" {
					t.Error("a 503 carried no Retry-After")
				}
			case http.StatusForbidden:
				if body["window"] != string(iam.RecencySensitive) {
					t.Errorf("the refusal named window %v, want the one asked "+
						"for — a ceremony needs it to know which proof to ask",
						body["window"])
				}
			}
			if flight := flightSet(started); flight != "" {
				t.Errorf("a refused start set a flight: %q", flight)
			}
		})
	}
}

// A PROVIDER STEP-UP NAMES THE WINDOW IT CONFIRMS INSIDE.
//
// The window is what `max_age` is sealed as and what the callback holds the
// provider's `auth_time` to, so a value that names none — the `true` this
// route used to take, the `any` no gesture asks for, nothing at all — is a
// request the start cannot make, refused before the provider is asked.
func TestAProviderStepUpNamesTheWindowItConfirmsInside(t *testing.T) {
	t.Parallel()
	for _, query := range []string{"step_up=true", "step_up=any", "step_up="} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			r := newProviderStepUp(t)
			started, _ := r.confirm(t, query, r.signedIn)
			if started.Code != http.StatusBadRequest ||
				!strings.Contains(started.Body.String(), string(iam.RecencySensitive)) {
				t.Fatalf("the start answered %d (%s), want 400 naming the windows",
					started.Code, started.Body)
			}
			if flight := flightSet(started); flight != "" {
				t.Errorf("a refused start set a flight: %q", flight)
			}
		})
	}
}

// A PROVIDER STEP-UP FOR SOMEBODY ELSE REPLACES NOTHING.
//
// The confirmation replaces the session the browser PRESENTED, so the person
// the provider comes back as must be the one that cookie is for. A provider
// account linked to somebody else — a colleague's, signed in on a shared
// machine — would otherwise end this person's session and open one for the
// colleague under this browser's step-up. It is refused as the guard would
// refuse a cookie that no longer names its holder, and nothing is ended or
// opened.
//
// Mutation: drop the person comparison when the step-up reads the session it
// replaces, and the colleague's session is opened.
func TestAProviderStepUpForSomebodyElseReplacesNothing(t *testing.T) {
	t.Parallel()
	r := newProviderStepUp(t)
	r.idp.authTime = clock.Add(-30 * time.Second)
	r.other.Store(&iamdomain.Sighting{
		ID: "0192f00d-0000-7000-8000-00000000000b", Kind: iam.KindPerson,
		Stage: iam.StageActive, Login: "somebody.else",
	})
	_, finished := r.confirm(t, "step_up="+string(iam.RecencyStepUp), r.signedIn)
	if finished == nil || finished.Code != http.StatusUnauthorized {
		t.Fatalf("the callback answered %+v, want 401", finished)
	}
	if closes, starts := r.changes(); len(closes) != 0 || len(starts) != 0 {
		t.Errorf("a step-up that came back as somebody else ended %+v and "+
			"opened %+v", closes, starts)
	}
}
