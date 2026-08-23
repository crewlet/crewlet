package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/static"
)

// tree is a stand-in dashboard, so these assert about SERVING rather than
// about the real dashboard's current contents.
func tree() fstest.MapFS {
	return fstest.MapFS{
		"dashboard/index.html":     {Data: []byte("<!doctype html><title>shell</title>")},
		"dashboard/favicon.ico":    {Data: []byte("icon-bytes")},
		"dashboard/js/app.js":      {Data: []byte("export const app = 1")},
		"dashboard/styles/app.css": {Data: []byte(":root{}")},
		"dashboard/data.bin":       {Data: []byte{0x00, 0x01}},
	}
}

// fetch runs one request and returns the whole response.
func fetch(t *testing.T, a *api.App, path string, headers map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return rec.Result()
}

func TestTheRootRedirectsToTheDashboard(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Assets: tree()})
	res := fetch(t, a, "/", nil)
	if res.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want a redirect", res.StatusCode)
	}
	if got := res.Header.Get("Location"); got != "/dashboard" {
		t.Errorf("location = %q", got)
	}
}

func TestTheShellServesWithoutAToken(t *testing.T) {
	t.Parallel()
	// The page that prompts for a token cannot itself require one, and it
	// ships no data — every byte it renders comes from an authenticated
	// fetch.
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, Assets: tree()})

	res := fetch(t, a, "/dashboard", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("content type = %q", got)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) == "" {
		t.Error("the shell served nothing")
	}
}

func TestAModuleIsServedAsJavaScript(t *testing.T) {
	t.Parallel()
	// An ES module served as anything else is REFUSED by the browser's
	// module loader, and the page then fails with a MIME error rather than
	// a missing file — which sends a reader looking for the wrong problem.
	a := newApp(t, api.Options{Assets: tree()})
	res := fetch(t, a, "/static/dashboard/js/app.js", nil)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Errorf("content type = %q, want a JavaScript type", got)
	}
}

func TestEachAssetKindGetsItsOwnType(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Assets: tree()})
	for path, want := range map[string]string{
		"/static/dashboard/styles/app.css": "text/css; charset=utf-8",
		"/static/dashboard/index.html":     "text/html; charset=utf-8",
		"/static/dashboard/favicon.ico":    "image/x-icon",
		// Anything unrecognised is bytes, not a guess.
		"/static/dashboard/data.bin": "application/octet-stream",
	} {
		res := fetch(t, a, path, nil)
		if got := res.Header.Get("Content-Type"); got != want {
			t.Errorf("%s: content type = %q, want %q", path, got, want)
		}
	}
}

func TestAnUnchangedAssetRevalidatesCheaply(t *testing.T) {
	t.Parallel()
	// ETag plus no-cache, so a browser always revalidates: an unchanged
	// file costs a 304 and a redeploy is picked up on the very next
	// request. A long max-age would leave a stale-module window in which a
	// page runs half the old app and half the new one.
	a := newApp(t, api.Options{Assets: tree()})
	first := fetch(t, a, "/static/dashboard/js/app.js", nil)

	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag, so every reload refetches every module")
	}
	if got := first.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("cache control = %q, want no-cache", got)
	}

	second := fetch(t, a, "/static/dashboard/js/app.js",
		map[string]string{"If-None-Match": etag})
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("status = %d on a matching ETag, want 304", second.StatusCode)
	}
	body, _ := io.ReadAll(second.Body)
	if len(body) != 0 {
		t.Errorf("a 304 carried %d bytes", len(body))
	}
}

func TestADifferentAssetGetsADifferentETag(t *testing.T) {
	t.Parallel()
	// The counterfactual: an ETag shared between files would serve one
	// module's bytes for another's request after a redeploy.
	a := newApp(t, api.Options{Assets: tree()})
	js := fetch(t, a, "/static/dashboard/js/app.js", nil).Header.Get("ETag")
	css := fetch(t, a, "/static/dashboard/styles/app.css", nil).Header.Get("ETag")
	if js == css {
		t.Errorf("two different assets share the ETag %s", js)
	}
	// And a stale ETag still serves the body.
	res := fetch(t, a, "/static/dashboard/js/app.js",
		map[string]string{"If-None-Match": css})
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d on a mismatched ETag, want the body", res.StatusCode)
	}
}

func TestATraversalNeverReachesTheAssetHandler(t *testing.T) {
	t.Parallel()
	// The FIRST of two layers, and the only one an end-to-end request can
	// see: http.ServeMux cleans a request path before routing, so a ".."
	// spelled in a URL is redirected rather than routed.
	//
	// Which is exactly why this is not the test that proves the handler
	// safe — it passed with the handler serving whatever it was asked for.
	// The second layer is asserted in the internal test, without a mux.
	a := newApp(t, api.Options{Assets: tree()})
	for _, path := range []string{
		"/static/../../etc/passwd",
		"/static/dashboard/../../../etc/passwd",
		"/static/",
	} {
		res := fetch(t, a, path, nil)
		if res.StatusCode == http.StatusOK {
			body, _ := io.ReadAll(res.Body)
			t.Errorf("%s served %d bytes", path, len(body))
		}
	}
}

func TestAMissingAssetIsNotFound(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Assets: tree()})
	if res := fetch(t, a, "/static/dashboard/js/nope.js", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

func TestTheRealDashboardIsInTheBinary(t *testing.T) {
	t.Parallel()
	// The product is one binary. A dashboard read from disk beside the
	// executable would make deployment a copy of two things, and a version
	// skew between them a supported state.
	//
	// This is the one case that asserts about the REAL tree, because what
	// it checks is that there is one.
	a := newApp(t, api.Options{})
	res := fetch(t, a, "/dashboard", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the embedded dashboard did not serve: %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if len(body) == 0 {
		t.Error("the embedded shell is empty")
	}

	// And its entry module, which is what the shell imports.
	if res := fetch(t, a, "/static/dashboard/js/app.js", nil); res.StatusCode != http.StatusOK {
		t.Errorf("the dashboard's entry module is not in the binary: %d", res.StatusCode)
	}
	if _, err := static.FS().Open("dashboard/index.html"); err != nil {
		t.Errorf("the embedded tree is not rooted where the URLs expect: %v", err)
	}
}
