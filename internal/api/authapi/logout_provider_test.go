package authapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iam/session"
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
