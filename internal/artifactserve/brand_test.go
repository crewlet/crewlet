package artifactserve_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/objstore"
)

// adopt stores css as an adopted brand's stylesheet and allowlists it,
// answering its hash.
func (r *rig) adopt(css []byte) objstore.Hash {
	r.t.Helper()
	obj := r.put(css)
	r.reads.set(func(rd *reads) {
		if rd.brand == nil {
			rd.brand = map[objstore.Hash]artifactsdomain.BrandFile{}
		}
		rd.brand[obj.Hash] = artifactsdomain.BrandFile{Slot: "css",
			ContentType: "text/css; charset=utf-8", Object: obj}
	})
	return obj.Hash
}

// THE BRAND ROUTE SERVES ONLY ADOPTED BYTES, AS THEIR OWN TYPE: a hash an
// adoption names is answered public, immutable and inert; the same hash at
// another extension, a hash of an object the store holds that no adoption
// names, and a name that is no hash are each an empty 404.
//
// Mutation: look a hash up in the object store rather than the allowlist, and
// the page's script is served by its digest.
func TestTheBrandRouteServesOnlyAdoptedBytes(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	f := r.frame()
	css := []byte(":root {\n  --brand-primary: #2450e6;\n}\n")
	hash := r.adopt(css)
	res := r.do(request{url: f.Origin + "/_crewlet/brand/" + string(hash) + ".css"})
	if res.StatusCode != http.StatusOK || string(body(t, res)) != string(css) {
		t.Fatalf("the adopted stylesheet answered %d %q", res.StatusCode, body(t, res))
	}
	for header, want := range map[string]string{
		"Cache-Control":               "public, max-age=31536000, immutable",
		"Content-Type":                "text/css; charset=utf-8",
		"ETag":                        `"` + string(hash) + `"`,
		"Access-Control-Allow-Origin": "*",
		"X-Content-Type-Options":      "nosniff",
	} {
		if got := res.Header.Get(header); got != want {
			t.Errorf("%s is %q, want %q", header, got, want)
		}
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") ||
		!strings.Contains(csp, "default-src 'none'") {
		t.Errorf("the brand file's policy %q lets something run", csp)
	}
	for _, path := range []string{
		"/_crewlet/brand/" + string(hash) + ".svg",
		"/_crewlet/brand/" + string(r.objects["app.js"].Hash) + ".js",
		"/_crewlet/brand/" + string(r.objects["index.html"].Hash) + ".css",
		"/_crewlet/brand/" + string(hash),
		"/_crewlet/brand/" + strings.ToUpper(string(hash)) + ".css",
		"/_crewlet/brand/../kit/0.18.0/kit.css",
	} {
		empty(t, r.do(request{url: f.Origin + path}), http.StatusNotFound)
	}
}

// A PINNED PAGE LINKS ITS STYLESHEET AND A PAGE THAT OPTS OUT DOES NOT: the
// origin places the link to the stylesheet the version pinned at the page's
// opening, after the runtime, leaving the author's bytes as they are, and
// places nothing in a page whose <html> element keeps its own design.
//
// Mutation: drop the opt-out check from withBrand and the page that keeps its
// own design is branded.
func TestAPinnedPageLinksItsStylesheetAndAnOptedOutOneDoesNot(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	hash := r.adopt([]byte(":root{--brand-primary:#2450e6}"))
	r.reads.set(func(rd *reads) {
		v := rd.versions[2]
		v.Brand = string(hash)
		rd.versions[2] = v
	})
	got := string(body(t, r.document(r.frame())))
	link := `<link rel="stylesheet" href="/_crewlet/brand/` + string(hash) + `.css">`
	at := strings.Index(got, link)
	switch {
	case at < 0:
		t.Fatalf("the pinned page carries no link to its stylesheet: %s", got)
	case at > strings.Index(got, "<html>"):
		t.Errorf("the link is placed after the page's <html> element: %s", got)
	case at < strings.Index(got, "/_crewlet/runtime/"):
		t.Errorf("the link is placed before the runtime, which runs first: %s", got)
	}
	// THE AUTHOR'S BYTES FOLLOW UNCHANGED, the response's nonce aside.
	_, rest, _ := strings.Cut(got, `nonce="`)
	nonce, _, _ := strings.Cut(rest, `"`)
	if !strings.HasSuffix(strings.ReplaceAll(got, nonce, artifacts.NoncePlaceholder),
		link+page[len("<!doctype html>"):]) {
		t.Errorf("the author's bytes were changed: %s", got)
	}

	own := strings.Replace(page, "<html>", `<html data-crewlet-brand="off">`, 1)
	obj := r.put([]byte(own))
	r.reads.set(func(rd *reads) {
		v := rd.versions[2]
		files := append([]artifacts.Entry(nil), v.Manifest.Files...)
		for i := range files {
			if files[i].Path == "index.html" {
				files[i].Object = obj
			}
		}
		v.Manifest.Files = files
		rd.versions[2] = v
	})
	r.build()
	if got := string(body(t, r.document(r.frame()))); strings.Contains(got, "/_crewlet/brand/") {
		t.Errorf("a page that keeps its own design was branded: %s", got)
	}
	if typ, _ := mediatype.For("brand.css"); typ.Served == "" {
		t.Fatal("the media table serves no stylesheet")
	}
}
