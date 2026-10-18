package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/config"
)

// assetKind is what one extension is served as, and whether it is worth a
// gzip pass.
type assetKind struct {
	// contentType is named rather than left to extension sniffing, for one
	// reason that matters and several that follow it: an ES module served as
	// anything other than a JavaScript type is REFUSED by the browser's
	// module loader, and the page then fails with a MIME error rather than a
	// missing file — which sends a reader looking for the wrong problem.
	contentType string

	// compress is true for text. It is false for a format that is already
	// compressed — a woff2 face is brotli inside, a png is deflate — because
	// a second pass over entropy-coded bytes buys a handful of bytes at best
	// and a LARGER body at worst, spent on every file the size of a font.
	// The kind decides rather than a measurement per file, because the
	// measurement already happens: a compressible file whose gzip is not
	// smaller is served as it is (see gzipped).
	compress bool
}

// assetKinds are the dashboard's assets by extension. Anything else is served
// as bytes, uncompressed, rather than as a guess.
var assetKinds = map[string]assetKind{
	".html":  {"text/html; charset=utf-8", true},
	".js":    {"text/javascript; charset=utf-8", true},
	".mjs":   {"text/javascript; charset=utf-8", true},
	".css":   {"text/css; charset=utf-8", true},
	".json":  {"application/json", true},
	".map":   {"application/json", true},
	".svg":   {"image/svg+xml", true},
	".ico":   {"image/x-icon", true},
	".png":   {"image/png", false},
	".jpg":   {"image/jpeg", false},
	".jpeg":  {"image/jpeg", false},
	".woff":  {"font/woff", false},
	".woff2": {"font/woff2", false},
	// The third-party notices and the font license. Named so a browser shows
	// them as text rather than downloading an octet stream nobody opens.
	".txt": {"text/plain; charset=utf-8", true},
}

// unknownKind is what a file with an extension assetKinds does not name is
// served as.
var unknownKind = assetKind{contentType: "application/octet-stream"}

// The two caching classes a dashboard file is served under.
const (
	// hashedDir is where the dashboard's build writes CONTENT-HASHED names:
	// Vite emits every chunk, the entry and every non-font asset as
	// `assets/<name>-<hash>.<ext>` (dashboard/vite.config.ts), so a change to
	// the bytes is a change to the URL. The trailing slash is load-bearing —
	// without it `dashboard/assets-old/` would be cached for a year too.
	// TestEveryFileUnderAssetsIsContentHashed holds the built tree to it.
	hashedDir = "dashboard/assets/"

	// cacheForever is for a name that IS a version. The bytes behind it can
	// never change, so a browser that has them has no question to ask:
	// `immutable` stops it revalidating on a reload, which it otherwise does
	// for every subresource however long its max-age. One year is the
	// longest lifetime a cache is conventionally asked to honour (RFC 2616
	// capped Expires there, and the major caches still clamp to it), and any
	// finite value is correct here, since a redeploy that changes a file
	// changes its name and the shell simply stops asking for the old one.
	// `public` because these are served without a credential to anybody:
	// the shell ships no data, and neither does anything it loads.
	cacheForever = "public, max-age=31536000, immutable"

	// cacheRevalidate is for everything else — the shell above all, whose
	// name never changes and which names this build's hashed files, and the
	// fonts, notices, icons and protocol.js, which are unhashed on purpose.
	// A browser keeps the copy and asks every time: unchanged costs a 304,
	// and a redeploy is picked up on the very next load. A max-age on an
	// unhashed name would open a stale window in which a page runs half the
	// old build and half the new one, the shape of bug nobody reproduces.
	cacheRevalidate = "no-cache"
)

// assets serves the embedded dashboard.
//
// Each file is read, hashed and — when it is text — compressed ONCE for the
// life of the process. The tree is embedded, so none of those answers can
// change while this process runs, and computing them per request would be
// work whose answer is already known: the entry module is a megabyte of
// JavaScript, and compressing it at the best level costs tens of
// milliseconds, where serving the kept copy costs a write.
type assets struct {
	tree fs.FS

	// policy is the Content-Security-Policy every dashboard file is served
	// under: [pagepolicy.Dashboard], with a frame-src naming the artifact
	// origin on a deployment that serves one ([pagepolicy.DashboardFor]).
	policy string

	// files holds every file that has been served. Only files that exist
	// are kept: a probe for a path the tree does not have is read and
	// refused every time, so a crawler asking for a million names grows
	// nothing here.
	mu    sync.Mutex
	files map[string]*asset
}

// asset is one file and everything about how it is served.
type asset struct {
	body  []byte
	etag  string
	kind  assetKind
	cache string

	// gzipped is the body compressed at gzip's best level, or nil when the
	// kind is not compressible or the result was not smaller. Best rather
	// than the default because the cost is paid once per file per process
	// and the saving on every load: measured on the committed bundle, the
	// four files a first load fetches go from 1.47 MB to 401 KB at level 9,
	// about 18 KB less than level 6 gives, for about 60 ms more, once.
	gzipped func() []byte
}

func newAssets(tree fs.FS, policy string) *assets {
	return &assets{tree: tree, policy: policy, files: map[string]*asset{}}
}

// dashboardPolicy is the Content-Security-Policy the dashboard is served under
// on this deployment: [pagepolicy.Dashboard], plus a frame-src naming the
// artifact origin where `api.artifacts.external_url` sets one, so the viewer
// may embed an artifact's sandboxed frame and nothing else.
//
// EVERY NODE SERVING THE DASHBOARD NAMES THE ORIGIN, whether or not it binds
// the artifact listener itself: the frame's bytes come from whichever ingress
// node the load balancer picks, and the shell a browser loaded from this node
// must let it load. No Tier A at all, a suite's, frames nothing.
func dashboardPolicy(b *config.Bootstrap) (string, error) {
	if b == nil {
		return pagepolicy.Dashboard, nil
	}
	source, err := artifactserve.FrameSource(b.API.Artifacts.ExternalBase())
	if err != nil {
		return "", fmt.Errorf("api: %w", err)
	}
	if source == "" {
		return pagepolicy.Dashboard, nil
	}
	return pagepolicy.DashboardFor([]string{source}), nil
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
	file, ok := a.lookup(name)
	if !ok {
		// JSON like every other refusal this surface writes, so a client
		// never has to tell the engine's own 404 from a proxy's.
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNoRoute)
		return
	}

	// EVERY HEADER BEFORE THE STATUS, which is the reason for where this
	// sits. A header set after WriteHeader is dropped without a word, and the
	// revalidation answer is the one a browser mostly gets: an asset served
	// 200 once and 304 ever after would carry its policy exactly once.
	h := w.Header()
	pagepolicy.Set(h, a.policy)
	h.Set("Cache-Control", file.cache)
	h.Set("Content-Type", file.kind.contentType)

	body, etag := file.body, file.etag
	if gz := file.gzipped(); gz != nil {
		// VARY WHENEVER THERE IS A CHOICE, on the identity answer and the
		// 304 as much as on the gzip one. A shared cache that stored this
		// response under a key without the request's Accept-Encoding would
		// hand gzip bytes to a client that cannot decode them, or identity
		// bytes to every client after the first. Added, never set: the
		// CORS layer has already written `Vary: Origin` when one was sent.
		h.Add("Vary", "Accept-Encoding")
		if pagepolicy.AcceptsGzip(r.Header) {
			body, etag = gz, pagepolicy.GzipTag(file.etag)
			h.Set("Content-Encoding", "gzip")
		}
	}
	h.Set("ETag", etag)

	// http.ServeContent rather than a hand-written branch, for the
	// conditional request above all: If-None-Match is a LIST under WEAK
	// comparison (RFC 9110 §13.1.2), so `"a", "b"`, `W/"a"` and `*` all
	// match — and a cache holding both representations of one URL is exactly
	// the client that sends a list. The branch this replaced compared the
	// header to one tag as a string, so each of those answered 200 with the
	// whole body. It also answers HEAD and Range, and a 304 from it drops
	// Content-Type and Content-Encoding but keeps ETag, Vary and
	// Cache-Control, which are the fields RFC 9110 §15.4.5 requires there.
	// It leaves Content-Length off an encoded body — its rule for handlers
	// that compress in a wrapper, where the length it knows is the wrong one
	// — so the gzip answer goes out chunked over HTTP/1.1, which costs a few
	// bytes of framing and nothing a browser notices. A zero modification
	// time turns If-Modified-Since off: the tag is the only validator, since
	// an embedded file's time is the zero time anyway.
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
}

// lookup returns one file, reading, hashing and classifying it on first use.
//
// The read and the hash happen outside the lock, so the first request for
// the entry module does not hold up every other file's. Two first requests
// racing for one file both do that work and the second keeps the first's
// entry, which is what makes its gzip happen once.
func (a *assets) lookup(name string) (*asset, bool) {
	a.mu.Lock()
	file, ok := a.files[name]
	a.mu.Unlock()
	if ok {
		return file, true
	}

	data, err := fs.ReadFile(a.tree, name)
	if err != nil {
		return nil, false
	}
	built := newAsset(name, data)

	a.mu.Lock()
	defer a.mu.Unlock()
	if file, ok := a.files[name]; ok {
		return file, true
	}
	a.files[name] = built
	return built, true
}

func newAsset(name string, data []byte) *asset {
	kind, known := assetKinds[strings.ToLower(path.Ext(name))]
	if !known {
		kind = unknownKind
	}
	cache := cacheRevalidate
	if strings.HasPrefix(name, hashedDir) {
		cache = cacheForever
	}
	sum := sha256.Sum256(data)
	return &asset{
		body:  data,
		etag:  `"` + hex.EncodeToString(sum[:10]) + `"`,
		kind:  kind,
		cache: cache,
		gzipped: sync.OnceValue(func() []byte {
			if !kind.compress {
				return nil
			}
			return pagepolicy.GzipSmaller(data, gzip.BestCompression)
		}),
	}
}
