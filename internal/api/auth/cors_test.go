package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
)

// AN ALLOW-LIST NOTHING ENFORCES IS INVISIBLE.
//
// `api.auth.allowed_origins` had no reader and there was no CORS
// implementation at all, so an operator who named a site got an allow-list
// that looked configured, a browser that blocked every fetch, and nothing
// anywhere saying why — the engine never sees a CORS failure, because the
// browser makes it after the response arrives.
func corsFor(origins ...string) *auth.CORS {
	return auth.NewCORS(&config.Bootstrap{
		API: config.API{Auth: config.APIAuth{AllowedOrigins: origins}},
	})
}

func serveCORS(t *testing.T, c *auth.CORS, r *http.Request) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	reached := false
	rec := httptest.NewRecorder()
	c.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	})).ServeHTTP(rec, r)
	return rec, reached
}

// A PERMITTED ORIGIN IS NAMED IN THE ANSWER.
func TestAPermittedOriginIsAllowed(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/work", nil)
	r.Header.Set("Origin", "https://ops.example.com")
	rec, reached := serveCORS(t, corsFor("https://ops.example.com"), r)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://ops.example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q — the browser discards the "+
			"answer, and the operator who configured this origin sees only a "+
			"fetch that failed", got)
	}
	if got := rec.Header().Values("Vary"); len(got) == 0 {
		t.Error("no Vary: Origin — a shared cache keyed without it serves one " +
			"site's permission to every other")
	}
	if !reached {
		t.Error("the request never reached the API")
	}
}

// AND ONE THAT IS NOT IS NOT REFUSED — it is answered with no permission, and
// the browser blocks it. Refusing with a status would break the default: a
// same-origin POST carries an Origin too, and a company with an empty
// allow-list would be refusing its own dashboard.
func TestAnUnknownOriginIsUnpermittedRatherThanRefused(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodPost, "/backup", nil)
	r.Header.Set("Origin", "https://evil.example.com")
	rec, reached := serveCORS(t, corsFor("https://ops.example.com"), r)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("an unlisted origin was permitted as %q", got)
	}
	if !reached {
		t.Error("a same-origin write was refused before it reached the API — " +
			"a same-origin POST carries an Origin, so this is the dashboard's " +
			"own path")
	}
	// AND THE DEFAULT IS SAME-ORIGIN ONLY.
	r2 := httptest.NewRequest(http.MethodGet, "/work", nil)
	r2.Header.Set("Origin", "https://ops.example.com")
	if rec2, _ := serveCORS(t, corsFor(), r2); rec2.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("a company that configured no origin permitted one")
	}
}

// A PREFLIGHT IS ANSWERED HERE AND NEVER REACHES THE GUARD.
//
// The browser sends it itself and attaches no Authorization header — it will
// not until it has been told the origin is permitted. Answered inside the
// guard, every preflight to a guarded route is a 401, the browser reports a
// CORS failure, and the real request is never sent at all.
func TestAPreflightIsAnsweredWithoutACredential(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodOptions, "/config", nil)
	r.Header.Set("Origin", "https://ops.example.com")
	r.Header.Set("Access-Control-Request-Method", "PATCH")
	rec, reached := serveCORS(t, corsFor("https://ops.example.com"), r)

	if reached {
		t.Error("the preflight reached the mux, where the guard answers 401 " +
			"to a request the browser cannot put a token on")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("the preflight answered %d, want %d", rec.Code, http.StatusNoContent)
	}
	for header, want := range map[string]string{
		"Access-Control-Allow-Origin":  "https://ops.example.com",
		"Access-Control-Allow-Methods": "PATCH",
		"Access-Control-Allow-Headers": "Authorization",
		"Access-Control-Max-Age":       "600",
	} {
		if got := rec.Header().Get(header); got == "" || !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to carry %q", header, got, want)
		}
	}
}

// A REQUEST WITH NO ORIGIN IS LEFT ALONE. Every non-browser caller sends none,
// and they are the overwhelming majority of this API's traffic.
func TestARequestWithNoOriginIsUntouched(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/work", nil)
	rec, reached := serveCORS(t, corsFor("https://ops.example.com"), r)
	if !reached {
		t.Fatal("a request with no Origin was not served")
	}
	if len(rec.Header()) != 0 {
		t.Errorf("a request with no Origin collected %v", rec.Header())
	}
}
