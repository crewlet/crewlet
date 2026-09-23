package api_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/static"
)

// tree is a stand-in dashboard, so these assert about SERVING rather than
// about the real dashboard's current contents.
//
// The files a gzip test reads are LONG AND REPETITIVE on purpose, because the
// server sends gzip only when it is smaller, and a one-line fixture is not:
// gzip's own header and trailer are eighteen bytes. Those that are meant to
// stay identity say why beside them.
func tree() fstest.MapFS {
	return fstest.MapFS{
		"dashboard/index.html": {Data: shell},
		// Shaped like the build's output: the content-hashed directory,
		// where a name is a version.
		"dashboard/assets/index-Cndv8LWe.js":  {Data: entryModule},
		"dashboard/assets/index-BS_xpF9S.css": {Data: repeat(".crewlet-btn{border-radius:6px}\n")},
		// A name shaped like a hashed one in a directory that only STARTS
		// with the hashed directory's name, which is what pins the slash
		// ending that prefix.
		"dashboard/assets-previous/index-Cndv8LWe.js": {Data: entryModule},
		// The unhashed files the build keeps at stable paths.
		"dashboard/protocol.js": {Data: repeat("export function decode(frame) { return frame }\n")},
		"crewlet-icon.svg":      {Data: repeat(`<path d="M0 0h24v24H0z"/>`)},
		// Already-compressed kinds, with bytes that are NOT (see font).
		"dashboard/fonts/face.woff2": {Data: font},
		"dashboard/images/brand.png": {Data: font},
		// Short on purpose: gzip would grow each of these, so every one is
		// served as it is.
		"dashboard/NOTICES.txt":    {Data: []byte("notices")},
		"dashboard/favicon.ico":    {Data: []byte("icon-bytes")},
		"dashboard/js/app.js":      {Data: []byte("export const app = 1")},
		"dashboard/styles/app.css": {Data: []byte(":root{}")},
		"dashboard/data.bin":       {Data: []byte{0x00, 0x01}},
	}
}

var (
	// shell is a stand-in index.html, long enough to compress.
	shell = append([]byte("<!doctype html><title>shell</title>"),
		repeat(`<link rel="modulepreload" href="/static/dashboard/assets/index-Cndv8LWe.js">`)...)
	// entryModule is a stand-in hashed entry chunk.
	entryModule = repeat("export const app = () => document.querySelector('#root');\n")
	// font is a stand-in face, and COMPRESSIBLE, which a real woff2 is not:
	// what keeps it identity has to be the rule about its kind, never the
	// measurement that a real face would happen to fail.
	font = bytes.Repeat([]byte{0x00}, 4096)
)

// repeat is one line of text, sixty-four times.
func repeat(line string) []byte { return []byte(strings.Repeat(line, 64)) }

// The two cache classes, spelled out rather than imported: the values are the
// contract a browser reads, so the test states them.
const (
	forever    = "public, max-age=31536000, immutable"
	revalidate = "no-cache"
	hashedJS   = "/static/dashboard/assets/index-Cndv8LWe.js"
)

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
		// A notice is read, so it is text a browser shows rather than a
		// download.
		"/static/dashboard/NOTICES.txt": "text/plain; charset=utf-8",
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
	// An UNHASHED name gets ETag plus no-cache, so a browser always
	// revalidates: an unchanged file costs a 304 and a redeploy is picked up
	// on the very next request. A long max-age on a name that does not change
	// with its bytes would leave a stale-module window in which a page runs
	// half the old app and half the new one.
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

// A HASHED NAME IS A VERSION, so a browser that holds it has nothing to ask.
//
// Everything under assets/ is named `<name>-<hash>.<ext>` by the build, so a
// changed file is a new URL and the shell — which revalidates — stops naming
// the old one. `immutable` is what stops a reload revalidating it anyway,
// which every browser otherwise does for each subresource however long the
// max-age: the reload was costing one round trip per module to hear 304.
//
// On the 304 too, because RFC 9110 has a 304 carry the Cache-Control its 200
// would, and a cache that stored the answer before this change refreshes its
// headers from the revalidation.
func TestAHashedAssetIsCachedForGood(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Assets: tree()})
	gz := map[string]string{"Accept-Encoding": "gzip"}

	first := fetch(t, a, hashedJS, nil)
	etag := first.Header.Get("ETag")
	zipped := fetch(t, a, hashedJS, gz)
	revalidated := fetch(t, a, hashedJS, map[string]string{"If-None-Match": etag})
	if revalidated.StatusCode != http.StatusNotModified {
		t.Fatalf("the revalidation answered %d, so the 304 was not exercised", revalidated.StatusCode)
	}

	for name, res := range map[string]*http.Response{
		"the entry module":           first,
		"its gzip representation":    zipped,
		"its revalidation (304)":     revalidated,
		"the hashed stylesheet":      fetch(t, a, "/static/dashboard/assets/index-BS_xpF9S.css", nil),
		"the stylesheet, gzip":       fetch(t, a, "/static/dashboard/assets/index-BS_xpF9S.css", gz),
		"the entry module over HEAD": head(t, a, hashedJS),
	} {
		if got := res.Header.Get("Cache-Control"); got != forever {
			t.Errorf("%s (%d): cache control = %q, want %q", name, res.StatusCode, got, forever)
		}
	}
}

// THE SHELL AND EVERY UNHASHED FILE REVALIDATE, because their names do not
// change when their bytes do.
//
// The shell above all: it is what names this build's hashed files, so a
// shell cached for a year would go on asking for last year's modules. The
// fonts, the notices, the icons and protocol.js keep stable paths on purpose
// (dashboard/vite.config.ts), which is exactly what makes them unsafe to keep
// without asking. And a hashed-LOOKING name outside assets/ is no exception:
// the class is the directory the build writes hashes into, not a guess from
// a name's shape.
func TestTheShellAndEveryUnhashedFileRevalidate(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Assets: tree()})
	for _, path := range []string{
		"/dashboard",
		"/favicon.ico",
		"/static/crewlet-icon.svg",
		"/static/dashboard/index.html",
		"/static/dashboard/protocol.js",
		"/static/dashboard/fonts/face.woff2",
		"/static/dashboard/NOTICES.txt",
		"/static/dashboard/assets-previous/index-Cndv8LWe.js",
	} {
		for _, headers := range []map[string]string{nil, {"Accept-Encoding": "gzip"}} {
			res := fetch(t, a, path, headers)
			if res.StatusCode != http.StatusOK {
				t.Errorf("%s: status = %d", path, res.StatusCode)
				continue
			}
			if got := res.Header.Get("Cache-Control"); got != revalidate {
				t.Errorf("%s %v: cache control = %q, want %q", path, headers, got, revalidate)
			}
			if res.Header.Get("ETag") == "" {
				t.Errorf("%s %v: no ETag, so revalidating it costs the whole body", path, headers)
			}
		}
	}
}

// TEXT IS GZIPPED FOR A CLIENT THAT ASKS, in every spelling a real client
// asks in.
//
// The first load is four files and 1.47 MB of JavaScript and CSS as built,
// 401 KB gzipped; nothing in front of the engine is assumed to compress, and
// the air-gapped deployment the embedded fonts exist for is the one least
// likely to have a proxy that does.
func TestATextAssetIsGzippedWhenAsked(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Assets: tree()})
	files := tree()

	for _, accept := range []string{
		"gzip",
		"gzip, deflate, br, zstd", // Chrome
		"gzip, deflate, br",       // Safari
		"br;q=1.0, gzip;q=0.8, *;q=0.1",
		"*",
		"GZIP",
		"x-gzip", // RFC 9110 §12.5.3: the same coding
		"identity;q=0.5, *;q=1",
		"gzip;q=0.001",
		"gzip ; Q=1.000",
		"deflate,gzip;q=0.5",
	} {
		for path, file := range map[string]string{
			hashedJS: "dashboard/assets/index-Cndv8LWe.js",
			"/static/dashboard/assets/index-BS_xpF9S.css": "dashboard/assets/index-BS_xpF9S.css",
			"/dashboard":                    "dashboard/index.html",
			"/static/dashboard/protocol.js": "dashboard/protocol.js",
			"/static/crewlet-icon.svg":      "crewlet-icon.svg",
		} {
			res := fetch(t, a, path, map[string]string{"Accept-Encoding": accept})
			assertGzipped(t, path+" ("+accept+")", res, files[file].Data)
		}
	}

	// Two header lines are one list (RFC 9110 §5.3), so gzip on the second
	// is as much a request for it as gzip on the first.
	req := httptest.NewRequest(http.MethodGet, hashedJS, nil)
	req.Header.Add("Accept-Encoding", "br")
	req.Header.Add("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	assertGzipped(t, "two Accept-Encoding lines", rec.Result(), entryModule)
}

// assertGzipped checks one response is the gzip representation of want.
func assertGzipped(t *testing.T, name string, res *http.Response, want []byte) {
	t.Helper()
	if res.StatusCode != http.StatusOK {
		t.Errorf("%s: status = %d", name, res.StatusCode)
		return
	}
	if got := res.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("%s: content encoding = %q, want gzip", name, got)
		return
	}
	if !varies(res) {
		t.Errorf("%s: no `Vary: Accept-Encoding`, so a shared cache may hand these "+
			"gzip bytes to a client that cannot decode them", name)
	}
	body, _ := io.ReadAll(res.Body)
	if len(body) >= len(want) {
		t.Errorf("%s: %d bytes sent for a %d-byte file", name, len(body), len(want))
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Errorf("%s: the body is not gzip: %v", name, err)
		return
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Errorf("%s: the gzip body does not decode: %v", name, err)
		return
	}
	if !bytes.Equal(plain, want) {
		t.Errorf("%s: the gzip body decodes to %d bytes that are not the file", name, len(plain))
	}
}

// NOTHING IS ENCODED UNLESS THE REQUEST ASKED FOR IT — and a request that
// says nothing did not.
//
// RFC 9110 reads an absent Accept-Encoding as "anything", but no browser
// sends a request without one; the clients that do are curl, a script and a
// health probe, which would print the compressed bytes. A zero weight is a
// refusal, a member naming gzip outranks the wildcard whatever the order,
// and a weight that is not a qvalue is not a yes.
//
// The response still VARIES: the file has a gzip representation, so a shared
// cache has to key this identity answer on the header that chose it.
func TestNoEncodingIsSentUnasked(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Assets: tree()})
	for _, accept := range []string{
		"", // no header at all
		"identity",
		"br",
		"br, zstd, deflate",
		"gzip;q=0",
		"gzip;q=0.000",
		"x-gzip;q=0",
		"*;q=0",
		"*, gzip;q=0",
		"gzip;q=0, *",
		"gzip;q=2",
		"gzip;q=1.5",
		"gzip;q=0.0001",
		"gzip;q=",
		"gzip;q=high",
		"gzipped",
	} {
		headers := map[string]string{"Accept-Encoding": accept}
		if accept == "" {
			headers = nil
		}
		res := fetch(t, a, hashedJS, headers)
		if got := res.Header.Get("Content-Encoding"); got != "" {
			t.Errorf("%q: content encoding = %q, want none", accept, got)
		}
		body, _ := io.ReadAll(res.Body)
		if !bytes.Equal(body, entryModule) {
			t.Errorf("%q: %d bytes served, not the file's %d", accept, len(body), len(entryModule))
		}
		if !varies(res) {
			t.Errorf("%q: the identity answer does not vary on Accept-Encoding, so a "+
				"shared cache would hand it to every client after this one", accept)
		}
	}

	// The same holds for an empty header present on the request, which is
	// a client saying it wants no coding at all.
	req := httptest.NewRequest(http.MethodGet, hashedJS, nil)
	req.Header["Accept-Encoding"] = []string{""}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	if got := rec.Result().Header.Get("Content-Encoding"); got != "" {
		t.Errorf("an empty Accept-Encoding: content encoding = %q, want none", got)
	}
}

// A FONT IS NEVER RECOMPRESSED, and neither is any other format that already
// is: woff2 is brotli inside, a png is deflate. A second pass buys a handful
// of bytes at best and a larger body at worst.
//
// The stand-in face is compressible zero bytes, which a real one is not, so
// what this pins is the rule about the KIND: with the measurement alone
// deciding, it would be gzipped. And with nothing to choose between, the
// response does not vary — a Vary that never changes anything costs every
// shared cache a key it does not need.
func TestAFontIsNeverRecompressed(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Assets: tree()})
	for _, path := range []string{
		"/static/dashboard/fonts/face.woff2",
		"/static/dashboard/images/brand.png",
	} {
		res := fetch(t, a, path, map[string]string{"Accept-Encoding": "gzip"})
		if got := res.Header.Get("Content-Encoding"); got != "" {
			t.Errorf("%s: content encoding = %q, want none", path, got)
		}
		body, _ := io.ReadAll(res.Body)
		if !bytes.Equal(body, font) {
			t.Errorf("%s: %d bytes served, not the file's %d", path, len(body), len(font))
		}
		if varies(res) {
			t.Errorf("%s: varies on Accept-Encoding with nothing to choose between", path)
		}
	}
}

// A FILE GZIP WOULD GROW IS SERVED AS IT IS. gzip's own header and trailer
// are eighteen bytes, so a small file compresses to more than it was; a
// body that grew is one nobody should be sent, and one that never changes
// representation does not vary.
func TestAFileGzipWouldGrowIsServedAsItIs(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Assets: tree()})
	res := fetch(t, a, "/static/dashboard/js/app.js", map[string]string{"Accept-Encoding": "gzip"})
	if got := res.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("content encoding = %q for a %d-byte file", got, len("export const app = 1"))
	}
	if body, _ := io.ReadAll(res.Body); string(body) != "export const app = 1" {
		t.Errorf("body = %q", body)
	}
	if varies(res) {
		t.Error("varies on Accept-Encoding with only one representation to send")
	}
}

// EACH REPRESENTATION HAS ITS OWN ENTITY TAG.
//
// A strong tag names bytes. A cache that keeps both answers for one URL —
// which is what Vary asks of it — revalidates each with the tag it came
// with, so a shared tag would have the server confirm gzip bytes as the
// current identity body, and the reverse.
func TestTheGzippedRepresentationHasItsOwnETag(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Assets: tree()})
	gz := map[string]string{"Accept-Encoding": "gzip"}

	plain := fetch(t, a, hashedJS, nil).Header.Get("ETag")
	zipped := fetch(t, a, hashedJS, gz).Header.Get("ETag")
	if plain == "" || zipped == "" {
		t.Fatalf("identity tag %q, gzip tag %q", plain, zipped)
	}
	if plain == zipped {
		t.Fatalf("both representations are tagged %s", plain)
	}
	for _, tag := range []string{plain, zipped} {
		if strings.HasPrefix(tag, "W/") {
			t.Errorf("%s is weak; both bodies are byte-for-byte reproducible", tag)
		}
	}

	for _, c := range []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"the gzip tag, asking for gzip", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": zipped}, http.StatusNotModified},
		{"the identity tag, asking for identity", map[string]string{"If-None-Match": plain}, http.StatusNotModified},
		{"the identity tag, asking for gzip", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": plain}, http.StatusOK},
		{"the gzip tag, asking for identity", map[string]string{"If-None-Match": zipped}, http.StatusOK},
	} {
		res := fetch(t, a, hashedJS, c.headers)
		if res.StatusCode != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, res.StatusCode, c.want)
		}
		if !varies(res) {
			t.Errorf("%s (%d): no `Vary: Accept-Encoding`; RFC 9110 has a 304 carry "+
				"the Vary its 200 would", c.name, res.StatusCode)
		}
		want := plain
		if c.headers["Accept-Encoding"] == "gzip" {
			want = zipped
		}
		if got := res.Header.Get("ETag"); got != want {
			t.Errorf("%s (%d): ETag = %s, want %s", c.name, res.StatusCode, got, want)
		}
	}
}

// A REVALIDATION MATCHES ANY TAG IT NAMES.
//
// If-None-Match is a list compared weakly (RFC 9110 §13.1.2), and a cache
// holding both representations of a URL is exactly the client that sends
// one. This answered 200 with the whole body to each of these: the handler
// compared the header to a single tag as a string.
func TestARevalidationMatchesAnyTagItNames(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Assets: tree()})
	etag := fetch(t, a, hashedJS, nil).Header.Get("ETag")

	for _, match := range []string{
		`"stale", ` + etag,
		etag + `,"stale"`,
		"W/" + etag,
		"*",
	} {
		res := fetch(t, a, hashedJS, map[string]string{"If-None-Match": match})
		if res.StatusCode != http.StatusNotModified {
			t.Errorf("If-None-Match: %s answered %d, want 304", match, res.StatusCode)
		}
	}
	if res := fetch(t, a, hashedJS, map[string]string{"If-None-Match": `"stale", W/"other"`}); res.StatusCode != http.StatusOK {
		t.Errorf("a list naming no current tag answered %d, want the body", res.StatusCode)
	}
}

// THE ENCODING'S VARY IS ADDED TO THE ORIGIN'S, never written over it.
//
// The CORS layer runs first and names Origin whenever a request carried one,
// because a shared cache keyed without it would serve one site's permission
// to every other. A Set here would erase that and leave the cache keyed on
// the encoding alone.
func TestTheEncodingVaryKeepsTheOriginVary(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Assets: tree()})
	res := fetch(t, a, hashedJS, map[string]string{
		"Accept-Encoding": "gzip",
		"Origin":          "https://dashboard.example.com",
	})
	if !varies(res) {
		t.Errorf("Vary = %q, want Accept-Encoding among it", res.Header.Values("Vary"))
	}
	origin := false
	for _, field := range res.Header.Values("Vary") {
		for _, name := range strings.Split(field, ",") {
			origin = origin || strings.EqualFold(strings.TrimSpace(name), "Origin")
		}
	}
	if !origin {
		t.Errorf("Vary = %q, want Origin kept beside Accept-Encoding", res.Header.Values("Vary"))
	}
}

// varies reports whether a response names Accept-Encoding in its Vary.
func varies(res *http.Response) bool {
	for _, field := range res.Header.Values("Vary") {
		for _, name := range strings.Split(field, ",") {
			if strings.EqualFold(strings.TrimSpace(name), "Accept-Encoding") {
				return true
			}
		}
	}
	return false
}

// head runs one HEAD request.
func head(t *testing.T, a *api.App, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, path, nil))
	return rec.Result()
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

// THE SHELL AND EVERY ASSET CARRY THE DASHBOARD'S POLICY, the revalidation
// answer included.
//
// The 304 is the case that was missing: the asset handler wrote it before any
// header after it, and a header set after the status is dropped without a
// word. A browser mostly receives the 304, so a policy on the 200 alone is a
// policy on the first visit.
func TestTheDashboardCarriesItsSecurityHeaders(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Assets: tree()})
	etag := fetch(t, a, "/static/dashboard/js/app.js", nil).Header.Get("ETag")
	gz := map[string]string{"Accept-Encoding": "gzip"}
	gzEtag := fetch(t, a, hashedJS, gz).Header.Get("ETag")
	gzRevalidate := map[string]string{"Accept-Encoding": "gzip", "If-None-Match": gzEtag}

	for name, res := range map[string]*http.Response{
		"the shell":             fetch(t, a, "/dashboard", nil),
		"the shell, gzipped":    fetch(t, a, "/dashboard", gz),
		"the favicon":           fetch(t, a, "/favicon.ico", nil),
		"an asset (200)":        fetch(t, a, "/static/dashboard/js/app.js", nil),
		"an asset (304)":        fetch(t, a, "/static/dashboard/js/app.js", map[string]string{"If-None-Match": etag}),
		"a gzipped asset (200)": fetch(t, a, hashedJS, gz),
		"a gzipped asset (304)": fetch(t, a, hashedJS, gzRevalidate),
	} {
		assertSecurityHeaders(t, name, res, pagepolicy.Dashboard)
	}
	if res := fetch(t, a, "/static/dashboard/js/app.js", map[string]string{"If-None-Match": etag}); res.StatusCode != http.StatusNotModified {
		t.Fatalf("the revalidation case answered %d, so the 304 was not exercised", res.StatusCode)
	}
	if res := fetch(t, a, hashedJS, gzRevalidate); res.StatusCode != http.StatusNotModified {
		t.Fatalf("the gzip revalidation answered %d, so its 304 was not exercised", res.StatusCode)
	}
	if res := fetch(t, a, "/dashboard", gz); res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("the shell was not gzipped, so the gzip case was not exercised")
	}
}

// EVERY OTHER RESPONSE CARRIES THE API POLICY, including the ones no handler
// writes on purpose: the redirect from `/` (an HTML body), the mux's own 404,
// a JSON read and a refusal from the guard.
func TestEveryOtherResponseCarriesTheAPIPolicy(t *testing.T) {
	t.Parallel()
	open := newApp(t, api.Options{Assets: tree()})
	closed := closedPosture()
	guarded := newApp(t, api.Options{Bootstrap: &closed, Assets: tree()})

	for name, res := range map[string]*http.Response{
		"the redirect from /":  fetch(t, open, "/", nil),
		"an unrouted path":     fetch(t, open, "/no-such-route", nil),
		"a JSON read":          fetch(t, open, "/health", nil),
		"a refusal (401)":      fetch(t, guarded, "/org", nil),
		"a missing asset":      fetch(t, open, "/static/dashboard/js/nope.js", nil),
		"a traversal redirect": fetch(t, open, "/static/../../etc/passwd", nil),
	} {
		assertSecurityHeaders(t, name, res, pagepolicy.API)
	}
}

func assertSecurityHeaders(t *testing.T, name string, res *http.Response, policy string) {
	t.Helper()
	for header, want := range map[string]string{
		"Content-Security-Policy": policy,
		"X-Frame-Options":         "DENY",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
	} {
		if got := res.Header.Get(header); got != want {
			t.Errorf("%s (%d): %s = %q, want %q", name, res.StatusCode, header, got, want)
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

	// And its entry module, whose name the SHELL carries: the bundle is
	// content-hashed, so a literal path here would be a path that goes stale
	// on the next build and a test that fails for the wrong reason.
	entry := regexp.MustCompile(`src="(/static/dashboard/assets/[^"]+\.js)"`).FindSubmatch(body)
	if entry == nil {
		t.Fatalf("the shell names no entry module:\n%s", body)
	}
	if res := fetch(t, a, string(entry[1]), nil); res.StatusCode != http.StatusOK {
		t.Errorf("the dashboard's entry module is not in the binary: %d", res.StatusCode)
	}
	if _, err := static.FS().Open("dashboard/index.html"); err != nil {
		t.Errorf("the embedded tree is not rooted where the URLs expect: %v", err)
	}
}
