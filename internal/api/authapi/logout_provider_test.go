package authapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

// providerSignOut signs a person out through the provider route, presenting a
// live session cookie, and answers the response with what the writer closed.
func providerSignOut(t *testing.T, idp *provider) (*httptest.ResponseRecorder,
	*closingWriter, string) {

	t.Helper()
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendOIDC
	b.API.Auth.OIDC = &config.APIOIDC{Issuer: idp.URL, ClientID: idpClientID}
	lineage, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	millis := clock.UnixMilli()
	for i := range 6 {
		lineage[i] = byte(millis >> (8 * (5 - i)))
	}
	bearer, err := fixtureSigner(t).Mint(session.Mint{
		Lineage: lineage, Person: linkedPerson.ID, StartPosition: 1,
		AbsoluteExpiresAt: clock.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	writer := &closingWriter{}
	mux := http.NewServeMux()
	buildWith(t, b, oidc.NewProvider(oidc.Config{
		Issuer: idp.URL, ClientID: idpClientID, ClientSecret: "not-a-real-secret",
		RedirectURI: b.API.ExternalBase() + auth.PathAuthOIDCCallback,
	}, idp.Client(), func() time.Time { return clock }), func(o *authapi.Options) {
		o.Writer = writer
	}).Routes(mux)

	req := httptest.NewRequest(http.MethodPost, auth.PathAuthLogoutProvider, nil)
	req.AddCookie(&http.Cookie{Name: session.HostCookieName, Value: bearer})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec, writer, lineage.String()
}

// SIGNING OUT THROUGH THE PROVIDER ENDS THE SESSION HERE, THEN SENDS THE
// BROWSER TO END THE ONE THERE.
//
// A plain sign-out leaves the provider's session, so the next "sign in with
// the provider" on a shared machine is answered without anybody typing
// anything. This route ends the session here exactly as the plain one does —
// closed, and the cookie cleared under every name — and answers 303 to the
// provider's end_session_endpoint, keeping the query the provider published
// and naming this client and the dashboard to come back to.
//
// Mutation: skip the local sign-out and nothing is closed; send no
// post_logout_redirect_uri and the provider strands the person on its page.
func TestSigningOutThroughTheProviderEndsBothSessions(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	rec, writer, lineage := providerSignOut(t, idp)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("answered %d (%s), want 303 to the provider", rec.Code, rec.Body)
	}
	target, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location %q: %v", rec.Header().Get("Location"), err)
	}
	if got := target.Scheme + "://" + target.Host + target.Path; got != idp.URL+"/logout" {
		t.Errorf("redirected to %s, want the provider's end_session_endpoint", got)
	}
	query := target.Query()
	for key, want := range map[string]string{
		"tenant":                   "acme",
		"client_id":                idpClientID,
		"post_logout_redirect_uri": "https://crewlet.example.com/dashboard",
	} {
		if got := query.Get(key); got != want {
			t.Errorf("the redirect's %s is %q, want %q", key, got, want)
		}
	}
	if len(writer.closed) != 1 || writer.closed[0][0] != lineage {
		t.Errorf("closed %v, want the presented session %s", writer.closed, lineage)
	}
	cleared := map[string]bool{}
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			cleared[c.Name] = true
		}
	}
	for _, name := range session.CookieNames {
		if !cleared[name] {
			t.Errorf("the sign-out did not clear %q", name)
		}
	}
}

// A PROVIDER WITH NO END-SESSION ENDPOINT IS A PLAIN SIGN-OUT THAT SAYS SO.
//
// The session here is ended all the same, and the person is told their
// session at the provider was not — which somebody walking away from a shared
// machine has to know — rather than being sent nowhere.
func TestAProviderWithNoEndSessionIsAPlainSignOutThatSaysSo(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	idp.noEndSession = true
	rec, writer, lineage := providerSignOut(t, idp)

	if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
		t.Fatalf("answered %d to %q, want 200 and no redirect", rec.Code,
			rec.Header().Get("Location"))
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	if body["status"] != "signed out" || body["provider_session"] != "not_ended" ||
		body["detail"] == "" {
		t.Errorf("answered %v, want signed out with the provider session named "+
			"not ended", body)
	}
	if len(writer.closed) != 1 || writer.closed[0][0] != lineage {
		t.Errorf("closed %v, want the presented session %s", writer.closed, lineage)
	}
}

// awaitNothing is an identity applier that has always already applied.
type awaitNothing struct{}

func (awaitNothing) AwaitApplied(context.Context, uint64) error { return nil }

// A SIGN-OUT CLEARS THE COOKIE ON A NODE THAT CANNOT READ ITS ROWS.
//
// Both sign-outs promise the cookie goes whatever the write did — a sign-out
// that answered 503 would leave somebody looking at a signed-in page on a
// shared machine, and on the provider route it would never reach the
// provider's end_session_endpoint either. Their handlers keep that promise,
// verifying every bearer the browser holds themselves; but they were GUARDED,
// and the guard answers `503 identity_unavailable` before any handler runs on
// a node whose identity estate it cannot read. So on exactly the node that
// could not vouch for anybody, nobody could sign out. Here the node's rows are
// past the stall grace, as the real guard and origin check a node runs them
// behind see them: the plain sign-out answers 200 and the provider one 303,
// both clearing the cookie and recording the close the person asked for — and
// the control, a guarded route beside them, is still the guard's 503.
//
// Mutation: put the two sign-outs back behind the guard and both answer 503
// with the cookie still set.
func TestASignOutClearsTheCookieOnANodeThatCannotReadItsRows(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendOIDC
	b.API.Auth.OIDC = &config.APIOIDC{Issuer: idp.URL, ClientID: idpClientID}
	stalled := rows{session.Identity{
		Applied: ^uint64(0) >> 1, Generation: 2,
		Lag: statelog.StallGrace + time.Second,
		Session: session.LineageRow{Found: true, Epoch: 3,
			ProvedAt: clock.Add(-time.Hour)},
		Person: session.PersonRow{Found: true, Epoch: 3,
			Stage: iam.StageActive, Login: linkedPerson.Login},
	}}
	writer := &closingWriter{}
	mux := http.NewServeMux()
	buildWith(t, b, oidc.NewProvider(oidc.Config{
		Issuer: idp.URL, ClientID: idpClientID, ClientSecret: "not-a-real-secret",
		RedirectURI: b.API.ExternalBase() + auth.PathAuthOIDCCallback,
	}, idp.Client(), func() time.Time { return clock }), func(o *authapi.Options) {
		o.Writer = writer
		o.Sessions = stalled
	}).Routes(mux)
	arm, err := auth.NewSessions(auth.SessionsDeps{
		Signer: fixtureSigner(t), Directory: stalled, Applier: awaitNothing{},
		Chart: seatless{}, External: b.API.ExternalBase(), Audit: &recordingAudit{},
		Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("build the session arm: %v", err)
	}
	// AS A NODE RUNS THEM: the guard, and the origin check beneath it.
	h := auth.New(&b).WithSessions(arm).Middleware(auth.NewCSRF(&b).Middleware(mux))

	for _, tc := range []struct {
		path   string
		status int
		clears bool
	}{
		{auth.PathAuthLogout, http.StatusOK, true},
		{auth.PathAuthLogoutProvider, http.StatusSeeOther, true},
		// THE CONTROL: a guarded route on the same node, which the guard
		// still answers 503 — so the rows really are unreadable here.
		{"/auth/logout/all", http.StatusServiceUnavailable, false},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			lineage := uuid.Must(uuid.NewV7())
			millis := clock.Add(-time.Hour).UnixMilli()
			for i := range 6 {
				lineage[i] = byte(millis >> (8 * (5 - i)))
			}
			bearer, err := fixtureSigner(t).Mint(session.Mint{
				Lineage: lineage, Person: linkedPerson.ID, StartPosition: 1,
				Epoch: 3, Generation: 2, AbsoluteExpiresAt: clock.Add(time.Hour),
			})
			if err != nil {
				t.Fatalf("mint: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.Header.Set("Origin", b.API.ExternalBase())
			req.AddCookie(&http.Cookie{Name: session.HostCookieName, Value: bearer})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("answered %d (%s), want %d", rec.Code, rec.Body, tc.status)
			}
			cleared := false
			for _, c := range rec.Result().Cookies() {
				if c.Name == session.HostCookieName && c.MaxAge < 0 {
					cleared = true
				}
			}
			if cleared != tc.clears {
				t.Errorf("cleared the cookie = %v, want %v", cleared, tc.clears)
			}
			if !tc.clears {
				return
			}
			writer.mu.Lock()
			closed := slices.ContainsFunc(writer.closed, func(c [2]string) bool {
				return c[0] == lineage.String()
			})
			writer.mu.Unlock()
			if !closed {
				t.Errorf("the close the person asked for was not recorded")
			}
		})
	}
}
