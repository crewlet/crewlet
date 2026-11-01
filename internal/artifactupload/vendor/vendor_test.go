package vendor_test

import (
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifactupload/vendor"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// cdn is a fake CDN: every path answers its own body as JavaScript, but for
// the ones a test sets.
type cdn struct {
	hits  atomic.Int64
	files map[string]func(w http.ResponseWriter)
}

func (c *cdn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.hits.Add(1)
	if serve, ok := c.files[r.URL.Path]; ok {
		serve(w)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	_, _ = w.Write([]byte("/* " + r.URL.Path + " */"))
}

func newCache(t *testing.T, c *cdn, now func() time.Time, budget int64) *vendor.Cache {
	t.Helper()
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	cache, err := vendor.New(vendor.Options{
		Hosts:  []string{"cdn.jsdelivr.net", "fonts.googleapis.com"},
		Client: &http.Client{Transport: httpxtest.Rewrite(t, srv)}, Now: now, MaxBytes: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func req(t *testing.T, raw string, kind vendor.Kind) vendor.Request {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return vendor.Request{URL: u, Kind: kind, Immutable: true}
}

// A COPY IS FETCHED ONCE PER NODE: a second page asking for the same file is
// answered from the cache, its bytes and digest the same.
func TestAFileIsFetchedOnceAndAnsweredFromTheCache(t *testing.T) {
	t.Parallel()
	c := &cdn{}
	cache := newCache(t, c, nil, 0)
	r := req(t, "https://cdn.jsdelivr.net/npm/a@1.0.0/a.js", vendor.KindScript)
	first, err := cache.Fetch(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Fetch(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if c.hits.Load() != 1 {
		t.Fatalf("the CDN was asked %d times, want once", c.hits.Load())
	}
	if string(first.Body) != "/* /npm/a@1.0.0/a.js */" || first.SHA256 != second.SHA256 || len(first.SHA256) != 64 {
		t.Fatalf("the copies differ: %+v %+v", first, second)
	}
}

// THE PAGE'S INTEGRITY IS THE COPY'S: bytes that do not match the digest the
// page states are refused, and a value naming no known algorithm states a
// check nothing could make.
func TestACopyMustMatchThePagesIntegrity(t *testing.T) {
	t.Parallel()
	cache := newCache(t, &cdn{}, nil, 0)
	r := req(t, "https://cdn.jsdelivr.net/npm/a@1.0.0/a.js", vendor.KindScript)
	sum := sha512.Sum384([]byte("/* /npm/a@1.0.0/a.js */"))
	r.Integrity = "sha256-AAAA sha384-" + base64.StdEncoding.EncodeToString(sum[:])
	if _, err := cache.Fetch(t.Context(), r); err != nil {
		t.Fatalf("a matching integrity was refused: %v", err)
	}
	r.Integrity = "sha384-" + base64.StdEncoding.EncodeToString(make([]byte, 48))
	if _, err := cache.Fetch(t.Context(), r); !errors.Is(err, vendor.ErrRefused) {
		t.Fatalf("a mismatching integrity answered %v", err)
	}
	r.Integrity = "md5-abc"
	if _, err := cache.Fetch(t.Context(), r); !errors.Is(err, vendor.ErrRefused) {
		t.Fatalf("an integrity naming no known algorithm answered %v", err)
	}
}

// A FILE THE CDN ANSWERS AS SOMETHING ELSE, A FAILURE, OR ONE TOO LARGE IS
// REFUSED, and a host off the list is never asked.
func TestWhatTheCDNAnswersIsHeldToTheReference(t *testing.T) {
	t.Parallel()
	c := &cdn{files: map[string]func(w http.ResponseWriter){
		"/npm/a@1.0.0/page.js": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>"))
		},
		"/npm/a@1.0.0/gone.js": func(w http.ResponseWriter) { http.Error(w, "no", http.StatusNotFound) },
		"/npm/a@1.0.0/big.js": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write(make([]byte, vendor.MaxFileBytes+1))
		},
	}}
	cache := newCache(t, c, nil, 0)
	for _, p := range []string{"page.js", "gone.js", "big.js"} {
		_, err := cache.Fetch(t.Context(), req(t, "https://cdn.jsdelivr.net/npm/a@1.0.0/"+p, vendor.KindScript))
		if !errors.Is(err, vendor.ErrRefused) {
			t.Errorf("%s answered %v, want a refusal", p, err)
		}
	}
	before := c.hits.Load()
	for _, raw := range []string{"https://evil.example/a.js", "http://cdn.jsdelivr.net/npm/a@1.0.0/a.js"} {
		if _, err := cache.Fetch(t.Context(), req(t, raw, vendor.KindScript)); !errors.Is(err, vendor.ErrRefused) {
			t.Errorf("%s answered %v, want a refusal", raw, err)
		}
	}
	if c.hits.Load() != before {
		t.Fatal("a URL off the allowlist reached the network")
	}
}

// A REDIRECT IS FOLLOWED ON ITS OWN HOST ONLY.
func TestARedirectStaysOnItsHost(t *testing.T) {
	t.Parallel()
	c := &cdn{files: map[string]func(w http.ResponseWriter){
		"/npm/a@1.0.0": func(w http.ResponseWriter) {
			w.Header().Set("Location", "https://cdn.jsdelivr.net/npm/a@1.0.0/index.js")
			w.WriteHeader(http.StatusFound)
		},
		"/npm/b@1.0.0": func(w http.ResponseWriter) {
			w.Header().Set("Location", "https://evil.example/b.js")
			w.WriteHeader(http.StatusFound)
		},
	}}
	cache := newCache(t, c, nil, 0)
	f, err := cache.Fetch(t.Context(), req(t, "https://cdn.jsdelivr.net/npm/a@1.0.0", vendor.KindScript))
	if err != nil || !strings.Contains(string(f.Body), "index.js") {
		t.Fatalf("a same-host redirect answered %q, %v", f.Body, err)
	}
	if _, err := cache.Fetch(t.Context(), req(t, "https://cdn.jsdelivr.net/npm/b@1.0.0", vendor.KindScript)); err == nil {
		t.Fatal("a redirect off the host was followed")
	}
}

// A COPY OF A FILE WHOSE URL NAMES NO VERSION IS FETCHED AGAIN PAST ITS TERM;
// one naming a release is not.
func TestAMutableCopyIsFetchedAgainPastItsTerm(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	c := &cdn{files: map[string]func(w http.ResponseWriter){
		"/css2": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/css")
			_, _ = w.Write([]byte("@font-face{}"))
		},
	}}
	cache := newCache(t, c, clock, 0)
	mutable := req(t, "https://fonts.googleapis.com/css2?family=Inter", vendor.KindStylesheet)
	mutable.Immutable = false
	pinned := req(t, "https://cdn.jsdelivr.net/npm/a@1.0.0/a.js", vendor.KindScript)
	for _, r := range []vendor.Request{mutable, pinned} {
		if _, err := cache.Fetch(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(vendor.MutableTerm + time.Minute)
	for _, r := range []vendor.Request{mutable, pinned} {
		if _, err := cache.Fetch(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	if c.hits.Load() != 3 {
		t.Fatalf("the CDN was asked %d times, want the mutable file twice and the pinned once", c.hits.Load())
	}
}

// THE CACHE HOLDS ITS BUDGET, the least recently used copy first out.
func TestTheCacheHoldsItsBudget(t *testing.T) {
	t.Parallel()
	c := &cdn{}
	cache := newCache(t, c, nil, 30) // one copy of the 23 bytes each answers
	a := req(t, "https://cdn.jsdelivr.net/npm/a@1.0.0/a.js", vendor.KindScript)
	b := req(t, "https://cdn.jsdelivr.net/npm/b@1.0.0/b.js", vendor.KindScript)
	for _, r := range []vendor.Request{a, b, a} {
		if _, err := cache.Fetch(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	if c.hits.Load() != 3 {
		t.Fatalf("the CDN was asked %d times; a copy past the budget was kept", c.hits.Load())
	}
}
