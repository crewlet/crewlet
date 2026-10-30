package auth_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
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
	r := httptest.NewRequest(http.MethodGet, "/worklet", nil)
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
	// WHAT A CROSS-ORIGIN DASHBOARD READS, exposed: a browser hides every
	// header outside a short safelist, and without the engine's mark every
	// answer reads as something in front of the engine.
	exposed := rec.Header().Get("Access-Control-Expose-Headers")
	for _, want := range []string{httpjson.EngineHeader, "ETag", "Retry-After", "X-Request-Id"} {
		if !strings.Contains(exposed, want) {
			t.Errorf("Access-Control-Expose-Headers = %q, missing %s", exposed, want)
		}
	}
}

// AN ENTRY IS COMPARED AS A BROWSER SPELLS IT: written with its host in upper
// case and its scheme's default port, it permits the origin a browser sends
// for it, and the answer names the origin as the browser sent it, which is
// the one value the browser matches it against.
//
// Mutation: compare the request's Origin with the entries as written again
// and the first case fails; echo the Origin in this engine's spelling rather
// than the browser's and the second does, where a browser writes an
// IPv4-mapped address in hex and the canonical form is dotted.
func TestAnEntryIsComparedAsABrowserSpellsIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ entry, origin string }{
		{"https://Ops.Example.com:443", "https://ops.example.com"},
		{"http://[::ffff:127.0.0.1]:8080", "http://[::ffff:7f00:1]:8080"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/worklet", nil)
		r.Header.Set("Origin", tc.origin)
		rec, _ := serveCORS(t, corsFor(tc.entry), r)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != tc.origin {
			t.Errorf("entry %s: Access-Control-Allow-Origin = %q, want %q, the origin the browser sent",
				tc.entry, got, tc.origin)
		}
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
	// NOR IS ANYTHING EXPOSED TO IT: the headers a permitted origin reads
	// stay hidden from a page the allow-list does not name.
	if got := rec.Header().Get("Access-Control-Expose-Headers"); got != "" {
		t.Errorf("an unlisted origin was exposed %q", got)
	}
	if !reached {
		t.Error("a same-origin write was refused before it reached the API — " +
			"a same-origin POST carries an Origin, so this is the dashboard's " +
			"own path")
	}
	// AND THE DEFAULT IS SAME-ORIGIN ONLY.
	r2 := httptest.NewRequest(http.MethodGet, "/worklet", nil)
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
	// THE WRITE PATH WHOLE: each header a write the API takes reads, or a
	// cross-origin write that sends it is refused by the browser before it
	// is sent.
	allowed := strings.Split(rec.Header().Get("Access-Control-Allow-Headers"), ", ")
	for _, want := range []string{
		"Authorization", "Content-Type", "If-Match", "If-None-Match",
		"Idempotency-Key", "X-Summary", "X-Request-Id",
	} {
		if !slices.Contains(allowed, want) {
			t.Errorf("Access-Control-Allow-Headers = %q, missing %s", allowed, want)
		}
	}
}

// A PERMITTED ORIGIN MAY SEND WHAT THE ROUTES READ, AND NO HEADER BEYOND IT.
//
// A permitted origin may write, and the act route refuses a request with no
// `Idempotency-Key`, a `/config` write needs its `X-Summary` and its
// precondition, a write that names its own request id is logged under it, and
// a link's view takes its secret in a header: a preflight that did not allow
// one made the browser refuse the whole request, so such an origin could never
// make it at all. The reader header is the same-origin dashboard's alone and
// is not offered. Which headers the routes read is held by
// TestTheCORSListsAreTheHeadersTheRoutesReadAndSet; this holds the preflight
// to answering that list rather than the browser's question.
//
// Mutation: drop opkey.Header or X-Summary from the set and the first row
// fails; echo the browser's request back and the second does.
func TestAPermittedOriginMaySendWhatTheRoutesReadAndNothingBeyondIt(t *testing.T) {
	t.Parallel()
	allowed := func(asked string) []string {
		t.Helper()
		r := httptest.NewRequest(http.MethodOptions, "/operator/act/create_work_item", nil)
		r.Header.Set("Origin", "https://ops.example.com")
		r.Header.Set("Access-Control-Request-Method", http.MethodPost)
		r.Header.Set("Access-Control-Request-Headers", asked)
		rec, _ := serveCORS(t, corsFor("https://ops.example.com"), r)
		return strings.Split(rec.Header().Get("Access-Control-Allow-Headers"), ", ")
	}
	want := []string{"Authorization", "Content-Type", opkey.Header,
		"If-Match", "If-None-Match", "X-Summary", httpjson.RequestIDHeader,
		"X-Crewlet-Invite-Secret", "X-Crewlet-Reset-Secret"}
	if got := allowed("authorization, content-type, idempotency-key"); !slices.Equal(got, want) {
		t.Errorf("a permitted origin may send %v, want %v", got, want)
	}
	if got := allowed("x-crewlet-reader, x-invented"); slices.ContainsFunc(got, func(h string) bool {
		return strings.EqualFold(h, auth.ReaderHeader) || strings.EqualFold(h, "x-invented")
	}) {
		t.Errorf("a permitted origin may send %v: the set is closed, and the reader is the "+
			"same-origin dashboard's", got)
	}
}

// A PERMITTED ORIGIN'S SCRIPT MAY READ WHAT A CLIENT ACTS ON.
//
// A browser hides from a cross-origin page every response header that is not
// CORS-safelisted unless the answer exposes it, so a page on an allowed origin
// could read a `/config` document and never its `ETag` — the tag its next
// write's `If-Match` must carry — nor the `Retry-After` of a `503`, nor the
// engine's mark that says the answer is the engine's at all. The exposure
// rides the ANSWER, which is where the browser reads it; a preflight and an
// origin the allow-list does not name carry none.
//
// Mutation: drop the exposure from the answer and the first check fails; set
// it on the preflight and the second does.
func TestAPermittedOriginsScriptMayReadWhatAClientActsOn(t *testing.T) {
	t.Parallel()
	exposed := func(method, origin string, preflight bool, allowed ...string) []string {
		t.Helper()
		r := httptest.NewRequest(method, "/config", nil)
		r.Header.Set("Origin", origin)
		if preflight {
			r.Header.Set("Access-Control-Request-Method", http.MethodPut)
		}
		rec, _ := serveCORS(t, corsFor(allowed...), r)
		got := rec.Header().Get("Access-Control-Expose-Headers")
		if got == "" {
			return nil
		}
		return strings.Split(got, ", ")
	}
	want := []string{httpjson.EngineHeader, "ETag", "Retry-After", "Accept-Patch", "Allow",
		"Content-Disposition", httpjson.RequestIDHeader}
	if got := exposed(http.MethodGet, "https://ops.example.com", false,
		"https://ops.example.com"); !slices.Equal(got, want) {
		t.Errorf("a permitted origin's answer exposes %v, want %v", got, want)
	}
	if got := exposed(http.MethodOptions, "https://ops.example.com", true,
		"https://ops.example.com"); got != nil {
		t.Errorf("a preflight exposes %v: the browser reads the exposure off the answer", got)
	}
	if got := exposed(http.MethodGet, "https://evil.example.com", false,
		"https://ops.example.com"); got != nil {
		t.Errorf("an origin the allow-list does not name was exposed %v", got)
	}
}

// A REQUEST WITH NO ORIGIN IS LEFT ALONE. Every non-browser caller sends none,
// and they are the overwhelming majority of this API's traffic.
func TestARequestWithNoOriginIsUntouched(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/worklet", nil)
	rec, reached := serveCORS(t, corsFor("https://ops.example.com"), r)
	if !reached {
		t.Fatal("a request with no Origin was not served")
	}
	if len(rec.Header()) != 0 {
		t.Errorf("a request with no Origin collected %v", rec.Header())
	}
}
