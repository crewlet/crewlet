package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
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
	})

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
	})
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
