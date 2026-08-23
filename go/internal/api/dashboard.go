package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
)

// contentTypes are what the dashboard's assets are served as.
//
// Named here rather than left to extension sniffing, for one reason that
// matters and several that follow it: an ES module served as anything other
// than a JavaScript type is REFUSED by the browser's module loader, and the
// page then fails with a MIME error rather than a missing file — which sends a
// reader looking for the wrong problem.
var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json",
	".map":   "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
}

// assets serves the embedded dashboard.
//
// ETag plus no-cache, so a browser always revalidates: an unchanged file costs
// a 304 and a redeploy is picked up on the very next request. The alternative —
// a long max-age — leaves a stale-module window in which a page runs half the
// old app and half the new one, which is the shape of bug nobody reproduces.
type assets struct {
	tree fs.FS

	// etags are computed once per file. The tree is embedded and therefore
	// immutable for the life of the process, so hashing a module on every
	// request would be work whose answer cannot change.
	mu    sync.Mutex
	etags map[string]string
}

func newAssets(tree fs.FS) *assets {
	return &assets{tree: tree, etags: map[string]string{}}
}

// serveIndex answers the dashboard shell.
//
// The shell is exempt from the auth guard, and it ships NO DATA: every byte it
// renders comes from an authenticated fetch. That is what lets the page that
// prompts for a token load without one.
func (a *assets) serveIndex(w http.ResponseWriter, r *http.Request) {
	a.serve(w, r, "dashboard/index.html")
}

// serveStatic answers one asset under /static/.
//
// The requested path is passed to the tree UNSANITISED, and that is safe by
// contract rather than by luck: fs.FS requires Open to reject any name that is
// not fs.ValidPath — no "..", no leading slash, no "." element — so a traversal
// cannot resolve to a file outside the tree whatever the request spelled.
//
// Measured on both sources this serves from, embed.FS and the MapFS the tests
// use: "../../etc/passwd", "dashboard/../../etc/passwd", "/etc/passwd", "" and
// "./x" all fail to open. A cleaning step here was written first and removed
// after that measurement — it made the refusal look like this code's doing,
// which is worse than no code at all, because the next reader would trust it
// instead of the contract that is actually holding.
func (a *assets) serveStatic(w http.ResponseWriter, r *http.Request) {
	a.serve(w, r, strings.TrimPrefix(r.URL.Path, "/static/"))
}

// serveFavicon answers the tab icon from wherever the dashboard keeps it.
func (a *assets) serveFavicon(w http.ResponseWriter, r *http.Request) {
	a.serve(w, r, "dashboard/favicon.ico")
}

func (a *assets) serve(w http.ResponseWriter, r *http.Request, name string) {
	data, err := fs.ReadFile(a.tree, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	etag := a.etagFor(name, data)

	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	kind, known := contentTypes[strings.ToLower(path.Ext(name))]
	if !known {
		kind = "application/octet-stream"
	}
	w.Header().Set("Content-Type", kind)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (a *assets) etagFor(name string, data []byte) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cached, ok := a.etags[name]; ok {
		return cached
	}
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:10]) + `"`
	a.etags[name] = etag
	return etag
}
