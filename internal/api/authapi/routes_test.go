package authapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/oidc"
)

// EVERY ROUTE THIS SURFACE REGISTERS IS DELIBERATELY GUARDED OR DELIBERATELY
// NOT, and the list of which is which is HERE, in a reviewed diff.
//
// # Why a gate rather than a convention
//
// The two halves live in different packages: internal/api/auth owns the
// exemption list and this package owns the registration. They agree today
// because the paths are shared constants — but a route ADDED here needs an
// entry there, and nothing about writing `mux.HandleFunc` says so. The failure
// is silent in the dangerous direction: a new route under /auth/ that should
// need a session gets one that does not only if somebody remembers, and a
// credential surface behind no credential looks exactly like a working one.
//
// So this walks what the surface registers and requires every pattern to be
// named below. A route nobody classified fails the build.
func TestEveryAuthRouteIsClassified(t *testing.T) {
	t.Parallel()

	// THE UNGUARDED FIVE, plus the invitation pair. Each is here because
	// requiring a credential to obtain one is a deployment nobody can
	// enter — or, for the invitation, because holding the link IS the
	// credential.
	unguarded := []string{
		auth.PathAuthConfig, auth.PathAuthLogin, auth.PathAuthBootstrap,
		auth.AuthInvitePrefix + "{id}",
	}
	// THE OIDC PAIR IS CONDITIONAL, and this case is over a surface with
	// no provider — which is an ordinary deployment rather than a gap:
	// a company signing in with passwords serves no provider routes, and
	// they are ABSENT rather than answering an error, because a 404 says
	// this company does not sign in that way where a 503 would say it
	// does and is broken. Their exemption is asserted below, on a surface
	// that has one.
	optional := []string{auth.PathAuthOIDCStart, auth.PathAuthOIDCCallback,
		auth.AuthInvitePrefix + "{id}/provider"}
	// EVERYTHING ELSE NEEDS A SESSION, and the list is spelled out rather
	// than derived as "the rest": a route that went missing from the
	// registration would otherwise pass silently, and one added would be
	// classified by omission.
	guarded := []string{
		"/auth/session", "/auth/token", "/auth/step-up",
		"/auth/totp", "/auth/totp/recovery",
		"/auth/logout", "/auth/logout/all", "/auth/logout/{lineage}",
	}

	mux := &recordingMux{}
	surface(t).Routes(mux)
	registered := mux.patterns
	if len(registered) < 12 {
		t.Fatalf("the surface registered %d routes (%v); this gate is not "+
			"reading the mux", len(registered), registered)
	}

	for _, pattern := range registered {
		path := pathOf(pattern)
		switch {
		case slices.Contains(unguarded, path), slices.Contains(optional, path):
			if !auth.Unguarded(strings.Replace(path, "{id}", "abc", 1)) {
				t.Errorf("%s is declared unguarded here and the guard guards "+
					"it: a sign-in route behind a credential is a deployment "+
					"nobody can enter", pattern)
			}
		case slices.Contains(guarded, path):
			if auth.Unguarded(strings.Replace(path, "{lineage}", "abc", 1)) {
				t.Errorf("%s is declared guarded here and the guard exempts "+
					"it: this surface ends sessions and enrols second "+
					"factors, and an exempt one is a credential surface "+
					"behind no credential", pattern)
			}
		default:
			t.Errorf("%s is registered and classified nowhere. Add it to the "+
				"guarded or the unguarded list in this case, and — if "+
				"unguarded — to internal/api/auth's exemption list, which is "+
				"a separate edit nothing else will remind you of", pattern)
		}
	}

	// AND THE OTHER DIRECTION. A classification for a route nobody
	// registers any more is one that would silently cover the next route
	// with that path.
	for _, declared := range slices.Concat(unguarded, guarded) {
		if !slices.ContainsFunc(registered, func(p string) bool {
			return pathOf(p) == declared
		}) {
			t.Errorf("%s is classified here and registered nowhere; delete it",
				declared)
		}
	}
}

// NOTHING THIS SURFACE ANSWERS MAY BE STORED.
//
// Its answers are a second-factor seed and the URI that carries it, recovery
// codes shown exactly once, who somebody is, the address an invitation was sent
// to and every sign-in's Set-Cookie — and none carried a Cache-Control, so a
// browser's disk cache or a shared proxy was free to keep them long after the
// tab, the session and the step-up that was needed to read them. Then only
// what the routes wrote carried one: a wrapper around them could not reach the
// answers written before them — the guard's refusals, the origin check's, the
// mux's own 404 and 405. So the guard marks every answer under /auth, and this
// walks EVERY route the surface registers — on a deployment with a provider,
// so the conditional three are mounted too — through the REAL guard and origin
// check a node runs them behind, and holds each answer, whatever its status
// and whoever wrote it, to `no-store`: with no credential (the guard's 401 on
// every guarded route), from another site, an enrolment-only session on a
// route it may not reach, and a path and a method nothing serves.
//
// Mutation: drop the guard's marking and every row fails.
func TestNothingThisSurfaceAnswersMayBeStored(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendOIDC
	b.API.Auth.OIDC = &config.APIOIDC{Issuer: idp.URL, ClientID: idpClientID}
	svc := build(t, b, oidc.NewProvider(oidc.Config{
		Issuer: idp.URL, ClientID: idpClientID,
		RedirectURI: b.API.ExternalBase() + auth.PathAuthOIDCCallback,
	}, idp.Client(), func() time.Time { return clock }))

	serve := http.NewServeMux()
	recorded := &recordingMux{}
	svc.Routes(teeMux{recorded, serve})
	if len(recorded.patterns) < 15 {
		t.Fatalf("the surface registered %d routes (%v); this walk is not "+
			"reading the mux", len(recorded.patterns), recorded.patterns)
	}
	// AS A NODE RUNS IT: the guard, and the origin check beneath it.
	h := auth.New(&b).Middleware(auth.NewCSRF(&b).Middleware(serve))
	hold := func(what string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s answered %d with Cache-Control %q, want no-store",
				what, rec.Code, got)
		}
	}
	for _, pattern := range recorded.patterns {
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.NewReplacer("{id}", invitationID,
			"{lineage}", "0192f00d-0000-7000-8000-0000000000aa").Replace(path)
		for _, origin := range []string{b.API.ExternalBase(), "https://elsewhere.example"} {
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("Origin", origin)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			hold(pattern+" from "+origin, rec)
		}
	}
	for _, stray := range [][2]string{
		{http.MethodGet, auth.PathAuthLogin},   // a method nothing serves
		{http.MethodGet, "/auth/nothing-here"}, // a path nothing serves
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(stray[0], stray[1], nil))
		hold(stray[0]+" "+stray[1], rec)
	}

	// AND THE GUARD'S REFUSAL OF A SESSION IT RESOLVED: an enrolment-only
	// session asking for the recovery codes it may not have yet.
	r := newSignInRigWith(t, func(o *authapi.Options) {
		requiring(o, iam.SecondFactorRequired)
		o.Sessions = o.Writer.(*estate)
	})
	passwordOnly(r.estate)
	g := guarded(t, r)
	login, _ := json.Marshal(map[string]string{"login": "jane.doe", "password": password})
	_, restricted := send(t, g, http.MethodPost, auth.PathAuthLogin, string(login), "")
	if restricted == "" {
		t.Fatal("the password sign-in set no cookie; this case tests nothing")
	}
	refused, _ := send(t, g, http.MethodPost, "/auth/totp/recovery", "{}", restricted)
	if refused.Code != http.StatusForbidden {
		t.Fatalf("the enrolment-only session was answered %d at the recovery "+
			"codes, want the guard's 403", refused.Code)
	}
	hold("the guard's enrolment refusal", refused)
}

// teeMux registers every route on each of its muxes: one to read the patterns
// back, one to serve them.
type teeMux []auth.Mux

func (m teeMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	for _, each := range m {
		each.HandleFunc(pattern, handler)
	}
}

// recordingMux is a mux that mounts nothing and remembers everything.
//
// [http.ServeMux] does not report what was registered on it, which is why
// [authapi.Service.Routes] takes a mux it can name: a gate that could not read
// the registration would be holding the exemption list against nothing.
type recordingMux struct{ patterns []string }

func (m *recordingMux) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	m.patterns = append(m.patterns, pattern)
}

// pathOf is a mux pattern's path, without its method.
func pathOf(pattern string) string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		return pattern[i+1:]
	}
	return pattern
}

// AND THE PROVIDER ROUTES ARE MOUNTED AND EXEMPT WHERE THERE IS ONE.
//
// They are absent from the case above because that surface has no provider,
// which is what a company signing in with passwords looks like. Asserted
// separately rather than folded in, because "not registered" and "registered
// and guarded" are different failures with different remedies, and a single
// case over an optional route can only report one of them.
func TestTheProviderRoutesAreMountedAndExemptWhereThereIsOne(t *testing.T) {
	t.Parallel()
	mux := &recordingMux{}
	withProvider(t).Routes(mux)

	for _, want := range []string{auth.PathAuthOIDCStart, auth.PathAuthOIDCCallback,
		auth.AuthInvitePrefix + "{id}/provider"} {
		if !slices.ContainsFunc(mux.patterns, func(p string) bool {
			return pathOf(p) == want
		}) {
			t.Errorf("%s is not mounted on a deployment that has a provider, "+
				"so its only way in does not exist", want)
			continue
		}
		if !auth.Unguarded(strings.Replace(want, "{id}", "abc", 1)) {
			t.Errorf("%s is guarded: a provider round trip is a BROWSER "+
				"following a redirect, which carries nothing this engine "+
				"issued — so requiring a credential makes it unreachable", want)
		}
	}
}

// A BODY OVER THE CAP IS ANSWERED 413, not abandoned.
//
// The sign-in and bootstrap handlers returned without writing a status when
// the body reader refused a body, so the caller was answered an empty 200 —
// which a client reads as signed in with no cookie. A 413 discloses nothing a
// roster could be built from: it is about the size of the request, answered
// the same whoever is named in it.
func TestAnOversizedSignInIsAnsweredRatherThanDropped(t *testing.T) {
	t.Parallel()
	// A PASSWORD DEPLOYMENT, or the sign-in route answers that it serves
	// none before it reads a byte.
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendLocal
	mux := http.NewServeMux()
	build(t, b, nil).Routes(mux)
	for _, path := range []string{auth.PathAuthLogin, auth.PathAuthBootstrap} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path,
			strings.NewReader(`{"login":"`+strings.Repeat("x", 8<<10)+`"}`)))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: an oversized body answered %d, want 413", path, rec.Code)
		}
	}
}
