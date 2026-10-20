package artifactserve_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE TICKET'S OWN REFUSALS, each told apart only in the count.

// TestATicketIsRefusedWithoutSayingWhy: expired is the one refusal the host
// can act on (401, mint another); a forged ticket, another artifact's host and
// a top-level open are all an empty 404, counted under their reasons.
func TestATicketIsRefusedWithoutSayingWhy(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f := r.frame()
	u := strings.SplitN(f.URL, "#", 2)[0]

	empty(t, r.do(request{url: u, dest: "document"}), http.StatusNotFound)
	if res := r.do(request{url: u}); res.StatusCode != http.StatusOK {
		t.Errorf("a browser sending no Fetch Metadata was refused: %d", res.StatusCode)
	}

	forged := strings.Replace(u, "/t/v2.", "/t/v2.x", 1)
	empty(t, r.do(request{url: forged, dest: "iframe"}), http.StatusNotFound)

	other := artifacts.IDFor("another-op")
	elsewhere := strings.Replace(u, r.reads.head.ID.Label().String(), other.Label().String(), 1)
	empty(t, r.do(request{url: elsewhere, dest: "iframe"}), http.StatusNotFound)
	foreign := strings.Replace(u, "example-usercontent.net", "example-usercontent.net.evil.test", 1)
	empty(t, r.do(request{url: foreign, dest: "iframe"}), http.StatusNotFound)

	r.clock.advance(artifacts.TicketLifetime)
	empty(t, r.do(request{url: u, dest: "iframe"}), http.StatusUnauthorized)
	r.clock.advance(artifacts.TicketExpiredGrace)
	empty(t, r.do(request{url: u, dest: "iframe"}), http.StatusNotFound)

	for reason, want := range map[string]uint64{
		"navigation": 1, "invalid": 2, "host": 2, "expired": 1,
	} {
		if got := r.rec.count(metrics.ArtifactsTicketRefusals, metrics.Attrs{"reason": reason}); got != want {
			t.Errorf("%s refusals counted %d, want %d", reason, got, want)
		}
	}
}

// TestAFileIsServedAsItsManifestSaysAndNoLongerThanTheTicket: the served type is
// the manifest's, the validator is the digest's, the cache is private,
// immutable and never past the ticket, and a revalidation is a 304.
func TestAFileIsServedAsItsManifestSaysAndNoLongerThanTheTicket(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f := r.frame()
	r.clock.advance(artifacts.TicketLifetime - 600_000_000_000) // ten minutes left
	res := r.do(request{url: base(f) + "logo.png", dest: "image"})
	got := body(t, res)
	if res.StatusCode != http.StatusOK || !bytes.Equal(got, r.files["logo.png"]) {
		t.Fatalf("status %d with %d bytes", res.StatusCode, len(got))
	}
	etag := `"` + string(r.objects["logo.png"].Hash)[:40] + `"`
	for name, want := range map[string]string{
		"Content-Type":            "image/png",
		"ETag":                    etag,
		"Cache-Control":           "private, max-age=600, immutable",
		"Content-Length":          strconv.Itoa(len(r.files["logo.png"])),
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; sandbox",
	} {
		if res.Header.Get(name) != want {
			t.Errorf("%s = %q, want %q", name, res.Header.Get(name), want)
		}
	}
	if res.Header.Get("Content-Encoding") != "" || res.Header.Get("Vary") != "" {
		t.Errorf("a PNG was offered compressed: %v", res.Header)
	}
	empty(t, r.do(request{url: base(f) + "logo.png", dest: "image",
		headers: map[string]string{"If-None-Match": `W/` + etag}}), http.StatusNotModified)

	head := r.do(request{url: base(f) + "logo.png", method: http.MethodHead})
	if b := body(t, head); head.StatusCode != http.StatusOK || len(b) != 0 ||
		head.Header.Get("Content-Length") != strconv.Itoa(len(r.files["logo.png"])) {
		t.Errorf("HEAD answered %d with %d bytes and length %q", head.StatusCode, len(b),
			head.Header.Get("Content-Length"))
	}
}

// TestACompressibleFileIsGzippedForAClientThatAsks: kept compressed when small,
// compressed as it streams when large, with its own validator, and identity
// for a client that does not ask.
func TestACompressibleFileIsGzippedForAClientThatAsks(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f := r.frame()
	gz := map[string]string{"Accept-Encoding": "gzip"}
	for _, path := range []string{"app.js", "big.css"} {
		res := r.do(request{url: base(f) + path, dest: "script", headers: gz})
		raw := body(t, res)
		if res.Header.Get("Content-Encoding") != "gzip" || res.Header.Get("Vary") != "Accept-Encoding" ||
			!strings.HasSuffix(res.Header.Get("ETag"), `-gz"`) {
			t.Fatalf("%s: headers %v", path, res.Header)
		}
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s: not gzip: %v", path, err)
		}
		plain, err := io.ReadAll(zr)
		if err != nil || !bytes.Equal(plain, r.files[path]) {
			t.Fatalf("%s: the gzip does not decode to the file (%v)", path, err)
		}
		identity := r.do(request{url: base(f) + path, dest: "script"})
		if b := body(t, identity); identity.Header.Get("Content-Encoding") != "" ||
			!bytes.Equal(b, r.files[path]) || identity.Header.Get("Vary") != "Accept-Encoding" {
			t.Fatalf("%s: the identity answer: %v", path, identity.Header)
		}
	}
}

// TestARangeIsServedExactly: a single range, a suffix and an open end are 206
// with the bytes asked for; one past the end is 416; a list, or an If-Range
// naming another version, is the whole file.
func TestARangeIsServedExactly(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f := r.frame()
	clip := r.files["clip.mp4"]
	size := len(clip)
	u := base(f) + "clip.mp4"
	for _, tc := range []struct {
		header     string
		start, end int
	}{
		{"bytes=10-19", 10, 19},
		{"bytes=-5", size - 5, size - 1},
		{"bytes=200000-", 200000, size - 1},
		{"bytes=0-", 0, size - 1},
		{"bytes=300000-999999999", 300000, size - 1},
	} {
		res := r.do(request{url: u, dest: "video", headers: map[string]string{"Range": tc.header}})
		got := body(t, res)
		want := "bytes " + strconv.Itoa(tc.start) + "-" + strconv.Itoa(tc.end) + "/" + strconv.Itoa(size)
		if res.StatusCode != http.StatusPartialContent || res.Header.Get("Content-Range") != want ||
			!bytes.Equal(got, clip[tc.start:tc.end+1]) {
			t.Errorf("%s: status %d range %q and %d bytes, want 206 %q", tc.header, res.StatusCode,
				res.Header.Get("Content-Range"), len(got), want)
		}
	}
	unsatisfiable := r.do(request{url: u, headers: map[string]string{"Range": "bytes=99999999-"}})
	if unsatisfiable.Header.Get("Content-Range") != "bytes */"+strconv.Itoa(size) {
		t.Errorf("an unsatisfiable range answered Content-Range %q", unsatisfiable.Header.Get("Content-Range"))
	}
	empty(t, unsatisfiable, http.StatusRequestedRangeNotSatisfiable)
	for _, headers := range []map[string]string{
		{"Range": "bytes=0-1,5-6"},
		{"Range": "bytes=0-1", "If-Range": `"another-version"`},
		{"Range": "items=0-1"},
	} {
		res := r.do(request{url: u, headers: headers})
		if got := body(t, res); res.StatusCode != http.StatusOK || len(got) != size {
			t.Errorf("%v: status %d with %d bytes, want the whole file", headers, res.StatusCode, len(got))
		}
	}
}

// TestEveryByteIsCheckedAgainstTheRow: a file the store holds other bytes for
// is never served as though whole. A document or a small file fails before
// its status (500, the engine's own fault, which no retry clears); a large one
// is cut, so the client reads short of its length; a file the store no longer
// holds is a 500 too. Mutation: serve from the backend directly rather than
// through the checking reader and every case serves the wrong bytes.
func TestEveryByteIsCheckedAgainstTheRow(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f := r.frame()
	r.backend.Corrupt(r.objects["index.html"].Key.Name(), []byte(strings.Replace(page, "Review", "Revuew", 1)))
	empty(t, r.document(f), http.StatusInternalServerError)
	r.backend.Corrupt(r.objects["logo.png"].Key.Name(), append([]byte{0}, r.files["logo.png"][1:]...))
	empty(t, r.do(request{url: base(f) + "logo.png"}), http.StatusInternalServerError)
	if err := r.backend.Delete(t.Context(), r.objects["app.js"].Key.Name()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	empty(t, r.do(request{url: base(f) + "app.js"}), http.StatusInternalServerError)

	damaged := append([]byte{}, r.files["clip.mp4"]...)
	damaged[len(damaged)-1] ^= 0xff
	r.backend.Corrupt(r.objects["clip.mp4"].Key.Name(), damaged)
	server := httptest.NewServer(r.handler)
	defer server.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		server.URL+strings.TrimPrefix(base(f), r.origins.Origin(r.reads.head.ID))+"clip.mp4", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Host = r.origins.Host(r.reads.head.ID)
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer res.Body.Close()
	got, readErr := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || readErr == nil || len(got) >= len(damaged) {
		t.Fatalf("a damaged file read %d of %d bytes (%v), want a cut response", len(got),
			len(damaged), readErr)
	}
}

// TestTheAssetStoreIsServedOnlyWhereTheTicketGrantsIt.
func TestTheAssetStoreIsServedOnlyWhereTheTicketGrantsIt(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	asset := r.reads.assets[0].ID.String()
	granted := r.frame()
	res := r.do(request{url: base(granted) + "_assets/" + asset, dest: "empty"})
	if got := body(t, res); res.StatusCode != http.StatusOK || string(got) != "asset bytes" ||
		res.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("a granted asset answered %d %q as %q", res.StatusCode, got, res.Header.Get("Content-Type"))
	}
	bare := r.frameFor(r.person, nil)
	empty(t, r.do(request{url: base(bare) + "_assets/" + asset}), http.StatusNotFound)
	empty(t, r.do(request{url: base(granted) + "_assets/" + uuid.NewString()}), http.StatusNotFound)
	empty(t, r.do(request{url: base(granted) + "_assets/" + strings.ToUpper(asset)}), http.StatusNotFound)
}

// TestOnlyAFileOfTheVersionIsServed: an unknown path, a traversal, a reserved
// prefix and a path in another spelling are all an empty 404.
func TestOnlyAFileOfTheVersionIsServed(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f := r.frame()
	for _, path := range []string{"missing.js", "../index.html", "a/../index.html", "/index.html",
		"_crewlet/runtime/1.0.0/runtime.js", "index.html/", "./index.html"} {
		empty(t, r.do(request{url: base(f) + path}), http.StatusNotFound)
	}
	if res := r.do(request{url: base(f), dest: "iframe"}); res.StatusCode != http.StatusOK ||
		res.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("the bare ticket path did not serve the entry: %d", res.StatusCode)
	}
}

// TestADecisionFollowsTheRowsAndTheChart: a version past retention, an
// artifact of a kind the dashboard renders, a credential now acting as another
// record and a unit share are each decided from what the rows and the chart
// say now; an administrator's admin path admits with no relation at all.
func TestADecisionFollowsTheRowsAndTheChart(t *testing.T) {
	t.Parallel()
	pruned := newRig(t)
	pf := pruned.frame()
	pruned.reads.set(func(r *reads) { delete(r.versions, 2) })
	empty(t, pruned.document(pf), http.StatusNotFound)

	sheet := newRig(t)
	sf := sheet.frame()
	sheet.reads.set(func(r *reads) { r.head.Kind = artifacts.KindSheet })
	empty(t, sheet.document(sf), http.StatusNotFound)

	// A CREDENTIAL NOW ACTING AS ANOTHER RECORD is refused even where that
	// record could read the artifact too (a company share): the ticket was
	// minted for the record it no longer is, and the host mints another.
	moved := newRig(t)
	moved.reads.set(func(r *reads) {
		r.head.Shares = append(r.head.Shares, artifacts.Share{
			Audience: artifacts.Audience{Kind: artifacts.AudienceCompany}, Role: artifacts.RoleViewer})
	})
	mf := moved.frame()
	elsewhere := moved.person
	elsewhere.Seat = "another-seat"
	moved.creds.set(moved.lineage, elsewhere, iam.Resolved)
	empty(t, moved.document(mf), http.StatusNotFound)

	unit := newRig(t)
	unit.reads.set(func(r *reads) {
		r.head.Shares = []artifacts.Share{{Audience: artifacts.Audience{Kind: artifacts.AudienceUnit,
			ID: "platform"}, Role: artifacts.RoleViewer}}
	})
	uf := unit.frame()
	empty(t, unit.document(uf), http.StatusNotFound)
	unit.chart.members[viewer+"|platform"] = true
	if res := unit.document(uf); res.StatusCode != http.StatusOK {
		t.Errorf("a member of the unit the artifact is shared with was refused: %d", res.StatusCode)
	}

	admin := newRig(t)
	admin.reads.set(func(r *reads) { r.head.Shares = nil })
	boss := admin.person
	boss.Grants = []iam.Grant{iam.GrantAdmin}
	admin.creds.set(admin.lineage, boss, iam.Resolved)
	af := admin.frameFor(boss, nil)
	if res := admin.document(af); res.StatusCode != http.StatusOK {
		t.Errorf("an administrator on the admin path was refused: %d", res.StatusCode)
	}
}

// TestASubresourceRidesItsDocumentsVerdictAndADocumentNever: within the age,
// files load without a decision each; a document always decides; a reset
// forgets everything.
func TestASubresourceRidesItsDocumentsVerdictAndADocumentNever(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f := r.frame()
	for range 3 {
		if res := r.document(f); res.StatusCode != http.StatusOK {
			t.Fatalf("document: %d", res.StatusCode)
		}
	}
	if got := r.reads.reads(); got != 3 {
		t.Fatalf("three documents read the head %d times, want 3", got)
	}
	for range 5 {
		if res := r.do(request{url: base(f) + "logo.png", dest: "image"}); res.StatusCode != http.StatusOK {
			t.Fatalf("file: %d", res.StatusCode)
		}
	}
	if got := r.reads.reads(); got != 3 {
		t.Fatalf("five files after a document read the head %d more times, want none", got-3)
	}
	// AN HTML FILE FETCHED AS DATA is decided again too.
	if res := r.do(request{url: base(f) + "index.html", dest: "empty"}); res.StatusCode != http.StatusOK ||
		r.reads.reads() != 4 {
		t.Fatalf("an HTML file fetched as data: %d, head read %d times", res.StatusCode, r.reads.reads())
	}
	r.handler.ArtifactsMoved([]artifactsdomain.Moved{artifactsdomain.Reset()})
	if res := r.do(request{url: base(f) + "logo.png", dest: "image"}); res.StatusCode != http.StatusOK ||
		r.reads.reads() != 5 {
		t.Fatalf("after a reset a file: %d, head read %d times, want a fresh decision", res.StatusCode,
			r.reads.reads())
	}
	// A MOVEMENT OF ANOTHER ARTIFACT forgets nothing of this one.
	r.handler.ArtifactsMoved([]artifactsdomain.Moved{{Artifact: artifacts.IDFor("other"),
		What: artifactsdomain.MoveShare}})
	if res := r.do(request{url: base(f) + "logo.png", dest: "image"}); res.StatusCode != http.StatusOK ||
		r.reads.reads() != 5 {
		t.Fatalf("another artifact's movement cost a decision here: head read %d times", r.reads.reads())
	}
}

// TestEveryRequestIsCounted by destination and status class.
func TestEveryRequestIsCounted(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f := r.frame()
	r.document(f)
	r.do(request{url: base(f) + "logo.png", dest: "image"})
	r.do(request{url: f.Origin + "/_crewlet/runtime/1.0.0/runtime.js"})
	r.do(request{url: f.Origin + "/_crewlet/kit/9.9.9/kit.css"})
	for _, tc := range []struct{ dest, status string }{
		{"document", "2xx"}, {"subresource", "2xx"}, {"runtime", "2xx"}, {"kit", "4xx"},
	} {
		if r.rec.count(metrics.ArtifactsServeRequests, metrics.Attrs{"dest": tc.dest, "status": tc.status}) != 1 {
			t.Errorf("no %s %s request was counted", tc.dest, tc.status)
		}
	}
}

// TestTheEmbeddedRuntimeAndKitArePublicAndForever: the runtime at its bytes'
// name and the kit at its release, served on any host with the public policy,
// kept a year, compressed for a client that asks; nothing else in the tree is
// reachable.
func TestTheEmbeddedRuntimeAndKitArePublicAndForever(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	runtime := r.embedded["runtime/1.0.0/runtime.js"].Data
	res := r.do(request{url: "https://anything.example.org" + artifactserve.RuntimeAddress("1.0.0", runtime),
		headers: map[string]string{"Accept-Encoding": "gzip"}})
	raw := body(t, res)
	for name, want := range map[string]string{
		"Cache-Control":                "public, max-age=31536000, immutable",
		"Content-Type":                 "text/javascript; charset=utf-8",
		"Cross-Origin-Resource-Policy": "cross-origin",
		"Access-Control-Allow-Origin":  "*",
		"Content-Encoding":             "gzip",
	} {
		if res.Header.Get(name) != want {
			t.Errorf("%s = %q, want %q", name, res.Header.Get(name), want)
		}
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	if plain, _ := io.ReadAll(zr); !bytes.Equal(plain, runtime) {
		t.Fatal("the runtime does not decode to its file")
	}
	if kit := r.do(request{url: "https://anything.example.org/_crewlet/kit/0.18.0/kit.css"}); kit.StatusCode != http.StatusOK ||
		kit.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("the kit answered %d, Cache-Control %q", kit.StatusCode, kit.Header.Get("Cache-Control"))
	}
	for _, path := range []string{
		"/_crewlet/runtime/9.0.0/runtime.js", "/_crewlet/runtime/notes/runtime.js",
		"/_crewlet/runtime/1.0/runtime.js", "/_crewlet/runtime/1.0.0/../../kit/0.18.0/kit.css",
		"/_crewlet/runtime/1.0.0/runtime.exe", "/_crewlet/kit/0.18.0/", "/_crewlet/runtime/01.0.0/runtime.js",
		// Another build's bytes, another contract's, and a digest of another length.
		artifactserve.RuntimeAddress("1.0.0", []byte("another build")),
		artifactserve.RuntimeAddress("1.0.0", r.embedded["runtime/1.2.0/runtime.js"].Data),
		"/_crewlet/runtime/1.0.0/runtime." + strings.Repeat("a", 15) + ".js",
	} {
		empty(t, r.do(request{url: "https://anything.example.org" + path}), http.StatusNotFound)
	}
	if res := r.do(request{url: "https://anything.example.org" + artifactserve.RuntimeAddress("1.10.0",
		r.embedded["runtime/1.10.0/runtime.js"].Data)}); res.StatusCode != http.StatusOK {
		t.Errorf("a two-digit minor's runtime answered %d", res.StatusCode)
	}
}

// TestTheRuntimeAtItsContractsNameIsRevalidated: a page or a cache that still
// names a contract's path is answered, but never kept unasked, because a fix
// to the runtime ships under the same contract; an unchanged runtime answers
// its ETag with a 304, so revalidating costs a round trip and no bytes, and a
// changed one answers the new bytes. Mutation: serve the contract's path
// immutable, and a browser keeps a runtime from before a fix for a year.
func TestTheRuntimeAtItsContractsNameIsRevalidated(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	url := "https://anything.example.org" + artifactserve.RuntimePath("1.0.0")
	if url != "https://anything.example.org/_crewlet/runtime/1.0.0/runtime.js" {
		t.Fatalf("RuntimePath = %q", url)
	}
	first := r.do(request{url: url})
	if first.StatusCode != http.StatusOK || first.Header.Get("Cache-Control") != "public, no-cache, must-revalidate" {
		t.Fatalf("the contract's path answered %d, Cache-Control %q", first.StatusCode,
			first.Header.Get("Cache-Control"))
	}
	etag := first.Header.Get("ETag")
	if etag == "" || !bytes.Equal(body(t, first), r.embedded["runtime/1.0.0/runtime.js"].Data) {
		t.Fatalf("the contract's path served no ETag (%q) or other bytes", etag)
	}
	again := r.do(request{url: url, headers: map[string]string{"If-None-Match": etag}})
	if again.StatusCode != http.StatusNotModified {
		t.Errorf("an unchanged runtime revalidated with %d, want 304", again.StatusCode)
	}

	// Another build of the same contract: the old ETag is answered the new bytes.
	fixed := newRig(t)
	fixed.embedded["runtime/1.0.0/runtime.js"] = &fstest.MapFile{Data: []byte("window.crewlet = {fixed: true};")}
	res := fixed.do(request{url: url, headers: map[string]string{"If-None-Match": etag}})
	if res.StatusCode != http.StatusOK || string(body(t, res)) != "window.crewlet = {fixed: true};" {
		t.Errorf("a fixed runtime answered a stale ETag %d with %q", res.StatusCode, body(t, res))
	}
}

// TestThePageNamesItsRuntimeByItsBytes: the element the origin places in a page
// names the runtime's content-addressed path, which is served, kept a year,
// and admitted by the page's own policy through the response's nonce; another
// build of the same contract is another name, and the first build's name is
// absent there, so no cache can hand a page a runtime of another build.
// Mutation: place the contract's path, and the element names bytes no name
// pins.
func TestThePageNamesItsRuntimeByItsBytes(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	runtime := r.embedded["runtime/1.0.0/runtime.js"].Data
	src, nonce := runtimeElement(t, r)
	if want := artifactserve.RuntimeAddress("1.0.0", runtime); src != want {
		t.Fatalf("the page names its runtime %q, want %q", src, want)
	}
	if !strings.HasPrefix(src, "/_crewlet/runtime/1.0.0/runtime.") || !strings.HasSuffix(src, ".js") {
		t.Errorf("the runtime's address %q is not under its contract", src)
	}
	res := r.do(request{url: "https://anything.example.org" + src})
	if res.StatusCode != http.StatusOK || !bytes.Equal(body(t, res), runtime) ||
		res.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("the page's runtime answered %d, Cache-Control %q", res.StatusCode,
			res.Header.Get("Cache-Control"))
	}
	if nonce == "" {
		t.Error("the runtime element carries no nonce the page's policy admits")
	}

	fixed := newRig(t)
	fixed.embedded["runtime/1.0.0/runtime.js"] = &fstest.MapFile{Data: []byte("window.crewlet = {fixed: true};")}
	moved, _ := runtimeElement(t, fixed)
	if moved == src {
		t.Fatalf("another build of contract 1.0.0 kept the name %q", src)
	}
	empty(t, fixed.do(request{url: "https://anything.example.org" + src}), http.StatusNotFound)
	if res := fixed.do(request{url: "https://anything.example.org" + moved}); res.StatusCode != http.StatusOK {
		t.Errorf("the new build's runtime answered %d", res.StatusCode)
	}

	gone := newRig(t)
	delete(gone.embedded, "runtime/1.0.0/runtime.js")
	if got := string(body(t, gone.document(gone.frame()))); strings.Contains(got, "/_crewlet/runtime/") {
		t.Errorf("a contract this binary holds no runtime for was placed: %s", got)
	}
}

// runtimeElement is the runtime element of the rig's page: the path it names,
// and its nonce, which must be the one the response's policy admits.
func runtimeElement(t *testing.T, r *rig) (src, nonce string) {
	t.Helper()
	res := r.document(r.frame())
	policy := nonceOf(t, res.Header.Get("Content-Security-Policy"))
	m := regexp.MustCompile(`<script src="([^"]+)"[^>]* nonce="([^"]+)"></script>`).
		FindStringSubmatch(string(body(t, res)))
	if m == nil {
		t.Fatalf("no runtime element in %s", body(t, res))
	}
	if m[2] != policy {
		t.Errorf("the runtime element's nonce %q is not the policy's %q", m[2], policy)
		return m[1], ""
	}
	return m[1], m[2]
}

// TestAHandlerWithNoRecorderRecordsNothingAndServes: the engine's reading of a
// node built without a recorder, which must never be a handler that fails the
// first request it answers.
func TestAHandlerWithNoRecorderRecordsNothingAndServes(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	h, err := artifactserve.New(artifactserve.Deps{
		Tickets: r.tickets, Origins: r.origins, Artifacts: r.reads, Objects: r.store,
		Credentials: r.creds, Chart: r.chart, Local: r.local, Embedded: r.embedded,
		Now: r.clock.now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.handler = h
	if res := r.document(r.frame()); res.StatusCode != http.StatusOK {
		t.Fatalf("a handler with no recorder answered %d", res.StatusCode)
	}
	for name, deps := range map[string]artifactserve.Deps{
		"no tickets":     {Origins: r.origins, Artifacts: r.reads, Objects: r.store, Credentials: r.creds, Chart: r.chart, Local: r.local, Embedded: r.embedded},
		"no origin":      {Tickets: r.tickets, Artifacts: r.reads, Objects: r.store, Credentials: r.creds, Chart: r.chart, Local: r.local, Embedded: r.embedded},
		"no estate":      {Tickets: r.tickets, Origins: r.origins, Objects: r.store, Credentials: r.creds, Chart: r.chart, Local: r.local, Embedded: r.embedded},
		"no objects":     {Tickets: r.tickets, Origins: r.origins, Artifacts: r.reads, Credentials: r.creds, Chart: r.chart, Local: r.local, Embedded: r.embedded},
		"no credentials": {Tickets: r.tickets, Origins: r.origins, Artifacts: r.reads, Objects: r.store, Chart: r.chart, Local: r.local, Embedded: r.embedded},
		"no chart":       {Tickets: r.tickets, Origins: r.origins, Artifacts: r.reads, Objects: r.store, Credentials: r.creds, Local: r.local, Embedded: r.embedded},
		"no copy health": {Tickets: r.tickets, Origins: r.origins, Artifacts: r.reads, Objects: r.store, Credentials: r.creds, Chart: r.chart, Embedded: r.embedded},
		"no tree":        {Tickets: r.tickets, Origins: r.origins, Artifacts: r.reads, Objects: r.store, Credentials: r.creds, Chart: r.chart, Local: r.local},
	} {
		if _, err := artifactserve.New(deps); err == nil {
			t.Errorf("%s: a handler was built", name)
		}
	}
}

// TestTheRuntimeIsToldTheFramesCapabilities: the runtime element carries the
// capabilities the frame grants, in the canonical order, so a page asking
// `crewlet.capabilities.has` before the host's handshake is answered truly; a
// frame granting none carries no attribute, and the same document framed with
// another grant is served its own, never the first one kept. Mutation: drop
// the grant from the template's key and the second frame is told the first's.
func TestTheRuntimeIsToldTheFramesCapabilities(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	declared := func(f artifacts.Frame) string {
		t.Helper()
		got := string(body(t, r.document(f)))
		open := `<script src="` + artifactserve.RuntimeAddress("1.0.0",
			r.embedded["runtime/1.0.0/runtime.js"].Data) + `"`
		at := strings.Index(got, open)
		if at < 0 {
			t.Fatalf("no runtime element: %s", got)
		}
		element := got[at : at+strings.Index(got[at:], "></script>")]
		_, value, ok := strings.Cut(element, ` data-capabilities="`)
		if !ok {
			return ""
		}
		return value[:strings.IndexByte(value, '"')]
	}
	both := r.frameFor(r.person, []artifacts.Capability{artifacts.CapabilityDB, artifacts.CapabilityAssets})
	if got := declared(both); got != "assets db" {
		t.Errorf("a frame granting db and assets told the runtime %q, want %q", got, "assets db")
	}
	if got := declared(r.frameFor(r.person, []artifacts.Capability{artifacts.CapabilityAssets})); got != "assets" {
		t.Errorf("the same document under another grant told the runtime %q, want %q", got, "assets")
	}
	if got := declared(r.frameFor(r.person, nil)); got != "" {
		t.Errorf("a frame granting nothing told the runtime %q", got)
	}
}

// TestThePinnedRuntimeRunsBeforeEveryScriptOfThePage: an author never includes
// the runtime, so every HTML page of a version pinning a contract is served
// with that contract's runtime first (past a byte order mark, comments and the
// doctype, so the page keeps its mode), under the response's own nonce; a page
// of a version pinning none, a scriptable document that is not HTML and an
// asset get none. Mutation: place the runtime at byte zero and a page with a
// doctype falls into quirks mode; leave out the placeholder and the runtime is
// refused by the page's own policy.
func TestThePinnedRuntimeRunsBeforeEveryScriptOfThePage(t *testing.T) {
	t.Parallel()
	tag := func(r *rig, nonce string) string {
		return `<script src="` + artifactserve.RuntimeAddress("1.0.0",
			r.embedded["runtime/1.0.0/runtime.js"].Data) + `" data-capabilities="assets" nonce="` +
			nonce + `"></script>`
	}
	for name, tc := range map[string]struct{ page, before string }{
		"a page with a doctype":        {"<!doctype html><html><body>x</body></html>", "<!doctype html>"},
		"an upper-case doctype":        {"<!DOCTYPE HTML>\n<html></html>", "<!DOCTYPE HTML>"},
		"a comment before the doctype": {"<!-- built --> <!doctype html><p>x", "<!-- built --> <!doctype html>"},
		"a byte order mark":            {"\xef\xbb\xbf<!doctype html><p>x", "\xef\xbb\xbf<!doctype html>"},
		"no doctype at all":            {"<p>x</p>", ""},
		"leading whitespace":           {"\n  <html><p>x", "\n  "},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			obj := r.put([]byte(tc.page))
			r.reads.set(func(rd *reads) {
				v := rd.versions[2]
				files := append([]artifacts.Entry{}, v.Manifest.Files...)
				for i := range files {
					if files[i].Path == "index.html" {
						files[i].Object = obj
					}
				}
				v.Manifest.Files = files
				rd.versions[2] = v
			})
			res := r.document(r.frame())
			nonce := nonceOf(t, res.Header.Get("Content-Security-Policy"))
			got := string(body(t, res))
			if !strings.HasPrefix(got, tc.before+tag(r, nonce)) || !strings.HasSuffix(got, tc.page[len(tc.before):]) {
				t.Fatalf("served %q, want %q then the runtime then the rest", got, tc.before)
			}
		})
	}

	none := newRig(t)
	none.reads.set(func(rd *reads) {
		v := rd.versions[2]
		v.Contract = ""
		rd.versions[2] = v
	})
	if got := string(body(t, none.document(none.frame()))); strings.Contains(got, "/_crewlet/runtime/") {
		t.Errorf("a version pinning no contract was served a runtime: %s", got)
	}
	odd := newRig(t)
	odd.reads.set(func(rd *reads) {
		v := rd.versions[2]
		v.Contract = `1.0.0"><script>alert(1)</script>`
		rd.versions[2] = v
	})
	if got := string(body(t, odd.document(odd.frame()))); strings.Contains(got, "/_crewlet/runtime/") {
		t.Errorf("a contract that is no exact version was written into the page: %s", got)
	}
	r := newRig(t)
	f := r.frame()
	if got := string(body(t, r.do(request{url: base(f) + "diagram.svg", dest: "iframe"}))); strings.Contains(got, "/_crewlet/runtime/") {
		t.Errorf("an SVG document was served the runtime: %s", got)
	}
}
