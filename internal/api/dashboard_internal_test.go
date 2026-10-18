package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/config"
)

// Internal because the handler has to be reached WITHOUT the mux in front of
// it. http.ServeMux cleans a request path before routing, so a ".." spelled in
// a URL never arrives here — which means an external test of a traversal
// asserts about the mux and not about this code at all. It passed with the
// handler serving whatever it was asked for.

func TestTheAssetHandlerCannotBeMadeToLeaveItsTree(t *testing.T) {
	t.Parallel()
	// Safe by CONTRACT: fs.FS requires Open to reject any name that is not
	// fs.ValidPath — no "..", no leading slash, no "." element — so a
	// traversal cannot resolve to a file outside the tree whatever the
	// request spelled.
	files := newAssets(fstest.MapFS{
		"dashboard/index.html": {Data: []byte("shell")},
	}, pagepolicy.Dashboard)

	for _, raw := range []string{
		"/static/../../etc/passwd",
		"/static/dashboard/../../../etc/passwd",
		"/static//etc/passwd",
		"/static/./dashboard/index.html",
		"/static/",
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		// Set directly, so the path arrives exactly as written rather
		// than as the mux would have cleaned it.
		req.URL.Path = raw
		rec := httptest.NewRecorder()
		files.serveStatic(rec, req)

		res := rec.Result()
		if res.StatusCode == http.StatusOK {
			body, _ := io.ReadAll(res.Body)
			t.Errorf("%s served %d bytes", raw, len(body))
		}
	}
}

func TestTheAssetHandlerStillServesAValidPath(t *testing.T) {
	t.Parallel()
	// The counterfactual. Refusing everything would satisfy the case above
	// and serve no dashboard at all.
	files := newAssets(fstest.MapFS{
		"dashboard/js/app.js": {Data: []byte("export const app = 1")},
	}, pagepolicy.Dashboard)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.URL.Path = "/static/dashboard/js/app.js"
	rec := httptest.NewRecorder()
	files.serveStatic(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); body != "export const app = 1" {
		t.Errorf("body = %q", body)
	}
}

// THE DASHBOARD FRAMES THE ARTIFACT ORIGIN AND NOTHING ELSE: a deployment that
// names one adds exactly that frame-src to the dashboard's policy (a wildcard
// over the hosts a template names), on every node whether or not it binds the
// listener, and one that names none keeps the policy that frames nothing.
// Mutation: build the dashboard policy from the constant and every artifact
// frame is refused by the browser.
func TestTheDashboardFramesTheArtifactOrigin(t *testing.T) {
	t.Parallel()
	for external, want := range map[string]string{
		"":                                  pagepolicy.Dashboard,
		"https://artifacts.example.net":     pagepolicy.DashboardFor([]string{"https://artifacts.example.net"}),
		"https://{artifact}.example-uc.net": pagepolicy.DashboardFor([]string{"https://*.example-uc.net"}),
	} {
		b := config.DefaultBootstrap()
		b.API.Artifacts.ExternalURL = external
		got, err := dashboardPolicy(&b)
		if err != nil || got != want {
			t.Errorf("%q: the dashboard policy is %q (%v), want %q", external, got, err, want)
		}
	}
	if got, err := dashboardPolicy(nil); err != nil || got != pagepolicy.Dashboard {
		t.Errorf("no Tier A: %q, %v", got, err)
	}
	files := newAssets(fstest.MapFS{"dashboard/index.html": {Data: []byte("shell")}},
		pagepolicy.DashboardFor([]string{"https://*.example-uc.net"}))
	rec := httptest.NewRecorder()
	files.serveIndex(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if got := rec.Header().Get("Content-Security-Policy"); !strings.HasSuffix(got, "; frame-src https://*.example-uc.net") {
		t.Errorf("the shell is served under %q", got)
	}
}
