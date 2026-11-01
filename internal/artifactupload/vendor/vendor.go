package vendor

import (
	"container/list"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// Kind is what a copied file is, which decides the types the CDN may answer
// it as.
type Kind string

// The kinds a vendored file can be fetched as.
const (
	KindScript     Kind = "script"
	KindModule     Kind = "module"
	KindStylesheet Kind = "stylesheet"
	KindFont       Kind = "font"
	KindAsset      Kind = "asset"
)

// The bounds on one copy and on the cache.
const (
	// MaxFileBytes is the largest file copied in: the biggest browser
	// bundles a page loads from a CDN (a charting or diagramming library)
	// run to three or four mebibytes minified, so eight is their size
	// twice over, and half the text file bound a version holds.
	MaxFileBytes = 8 << 20

	// FetchTimeout bounds one request to a CDN, the body included: a CDN
	// edge answers a file of MaxFileBytes in a second or two, so fifteen
	// seconds covers half a mebibyte a second, and a publish waiting on a
	// CDN that stopped answering is told so in seconds, not minutes.
	FetchTimeout = 15 * time.Second

	// MutableTerm is how long a copy of a file whose URL names no version
	// (a web font's stylesheet) is reused before it is fetched again: a
	// day, the cadence at which such a stylesheet changes at all.
	MutableTerm = 24 * time.Hour

	// DefaultCacheBytes is the node's cache budget: sixteen of the largest
	// files, or a few hundred ordinary ones, which is the working set of a
	// company's pages without holding a meaningful share of a node's memory.
	DefaultCacheBytes = 128 << 20

	// maxRedirects is how many same-host redirects a fetch follows: a
	// package CDN redirects a package path to its file once.
	maxRedirects = 3
)

// ErrRefused reports a file that cannot be copied in: the host is not
// allowlisted, the CDN answered something other than the file, the file is too
// large or of the wrong type, or its bytes fail the integrity the page states.
var ErrRefused = errors.New("vendor: the file cannot be copied in")

// Request is one file to copy.
type Request struct {
	// URL is the file, https, on an allowlisted host.
	URL *url.URL

	// Kind is what the page loads it as.
	Kind Kind

	// Integrity is the page's subresource integrity value for it, empty for
	// none.
	Integrity string

	// Immutable reports a URL naming one release, whose bytes never change:
	// its copy is reused until the cache evicts it. A mutable one is
	// fetched again past [MutableTerm].
	Immutable bool
}

// File is one copied file.
type File struct {
	// URL is the file's URL, as requested.
	URL string

	// Body is its bytes; SHA256 their digest in lowercase hex.
	Body   []byte
	SHA256 string

	// ContentType is the type the CDN answered it as.
	ContentType string
}

// Options configure a cache.
type Options struct {
	// Hosts are the hosts a fetch may reach, the linter's allowlist: a
	// request for any other is refused before it is made.
	Hosts []string

	// Client makes the requests. Nil is [httpx.Guarded] bounded by
	// [FetchTimeout], which dials no address that is not public.
	Client *http.Client

	// Now is the clock a mutable copy's age is judged by; nil is the wall
	// clock.
	Now func() time.Time

	// MaxBytes is the cache's budget; zero is [DefaultCacheBytes].
	MaxBytes int64
}

// Cache is this node's copies of CDN files, keyed by URL, least recently used
// first out.
type Cache struct {
	hosts  []string
	client *http.Client
	now    func() time.Time
	budget int64

	mu       sync.Mutex
	entries  map[string]*list.Element
	order    *list.List
	held     int64
	inFlight map[string]*call
}

// entry is one cached copy.
type entry struct {
	file      File
	fetched   time.Time
	immutable bool
}

// call is one fetch in flight, which a second request for the same URL waits
// on rather than fetching again.
type call struct {
	done chan struct{}
	file File
	err  error
}

// New builds a cache.
func New(opts Options) (*Cache, error) {
	if len(opts.Hosts) == 0 {
		return nil, errors.New("vendor: a cache needs the hosts it may fetch from")
	}
	c := &Cache{
		hosts: slices.Clone(opts.Hosts), client: opts.Client, now: opts.Now,
		budget: opts.MaxBytes, entries: map[string]*list.Element{}, order: list.New(),
		inFlight: map[string]*call{},
	}
	for i, h := range c.hosts {
		c.hosts[i] = strings.ToLower(h)
	}
	if c.client == nil {
		c.client = httpx.Guarded(FetchTimeout)
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.budget <= 0 {
		c.budget = DefaultCacheBytes
	}
	return c, nil
}

// Fetch answers the file req names: this node's copy where it holds a current
// one, the CDN's otherwise. Either way its bytes are checked against the
// integrity the page states before they are answered.
func (c *Cache) Fetch(ctx context.Context, req Request) (File, error) {
	if req.URL == nil {
		return File{}, fmt.Errorf("%w: the request names no URL", ErrRefused)
	}
	if err := c.allowed(req.URL); err != nil {
		return File{}, err
	}
	key := req.URL.String()
	c.mu.Lock()
	if f, ok := c.cached(key); ok {
		c.mu.Unlock()
		return f, checkFile(f, req)
	}
	if inflight, ok := c.inFlight[key]; ok {
		c.mu.Unlock()
		select {
		case <-inflight.done:
		case <-ctx.Done():
			return File{}, ctx.Err()
		}
		if inflight.err != nil {
			return File{}, inflight.err
		}
		return inflight.file, checkFile(inflight.file, req)
	}
	run := &call{done: make(chan struct{})}
	c.inFlight[key] = run
	c.mu.Unlock()

	run.file, run.err = c.fetch(ctx, req)
	c.mu.Lock()
	delete(c.inFlight, key)
	if run.err == nil {
		c.store(key, run.file, req.Immutable)
	}
	c.mu.Unlock()
	close(run.done)
	if run.err != nil {
		return File{}, run.err
	}
	return run.file, checkFile(run.file, req)
}

// allowed refuses a URL off the allowlist, or not https.
func (c *Cache) allowed(u *url.URL) error {
	if u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return fmt.Errorf("%w: %s is not a plain https URL", ErrRefused, u.Redacted())
	}
	if !slices.Contains(c.hosts, strings.ToLower(u.Hostname())) {
		return fmt.Errorf("%w: %s is not an allowlisted CDN", ErrRefused, u.Hostname())
	}
	return nil
}

// cached is the current copy of key, under c.mu, moved to the front.
func (c *Cache) cached(key string) (File, bool) {
	el, ok := c.entries[key]
	if !ok {
		return File{}, false
	}
	e := el.Value.(*entry)
	if !e.immutable && c.now().Sub(e.fetched) > MutableTerm {
		c.evict(el)
		return File{}, false
	}
	c.order.MoveToFront(el)
	return e.file, true
}

// store keeps a copy under c.mu, evicting the least recently used past the
// budget.
func (c *Cache) store(key string, f File, immutable bool) {
	if el, ok := c.entries[key]; ok {
		c.evict(el)
	}
	size := int64(len(f.Body))
	if size > c.budget {
		return
	}
	c.entries[key] = c.order.PushFront(&entry{file: f, fetched: c.now(), immutable: immutable})
	c.held += size
	for c.held > c.budget {
		c.evict(c.order.Back())
	}
}

// evict drops one copy under c.mu.
func (c *Cache) evict(el *list.Element) {
	e := el.Value.(*entry)
	c.order.Remove(el)
	delete(c.entries, e.file.URL)
	c.held -= int64(len(e.file.Body))
}

// fetch asks the CDN for the file.
func (c *Cache) fetch(ctx context.Context, req Request) (File, error) {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	client := *c.client
	host := strings.ToLower(req.URL.Hostname())
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		switch {
		case len(via) > maxRedirects:
			return fmt.Errorf("%w: %s redirected more than %d times", ErrRefused, req.URL.Redacted(), maxRedirects)
		case next.URL.Scheme != "https" || strings.ToLower(next.URL.Hostname()) != host:
			return fmt.Errorf("%w: %s redirected to %s, off its host", ErrRefused,
				req.URL.Redacted(), next.URL.Redacted())
		}
		return nil
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL.String(), nil)
	if err != nil {
		return File{}, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	// A BROWSER'S AGENT, because a web font's stylesheet is written for the
	// agent asking: the one a current browser is answered with names WOFF2
	// files, which is what every viewer's browser will load.
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "+
		"(KHTML, like Gecko) Chrome/126.0 Safari/537.36 crewlet-vendor")
	httpReq.Header.Set("Accept", accepts(req.Kind))
	resp, err := client.Do(httpReq)
	if err != nil {
		return File{}, fmt.Errorf("vendor: fetch %s: %w", req.URL.Redacted(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return File{}, fmt.Errorf("%w: %s answered %s", ErrRefused, req.URL.Redacted(), resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFileBytes+1))
	if err != nil {
		return File{}, fmt.Errorf("vendor: read %s: %w", req.URL.Redacted(), err)
	}
	if len(body) > MaxFileBytes {
		return File{}, fmt.Errorf("%w: %s is larger than the %d bytes a copied file may be",
			ErrRefused, req.URL.Redacted(), MaxFileBytes)
	}
	sum := sha256.Sum256(body)
	return File{
		URL: req.URL.String(), Body: body, SHA256: hex.EncodeToString(sum[:]),
		ContentType: resp.Header.Get("Content-Type"),
	}, nil
}

// accepts is the Accept header a request of kind k sends.
func accepts(k Kind) string {
	switch k {
	case KindStylesheet:
		return "text/css,*/*;q=0.1"
	case KindScript, KindModule:
		return "*/*"
	}
	return "*/*"
}

// checkFile refuses a copy whose type the page cannot load as req's kind, or
// whose bytes fail the page's integrity.
func checkFile(f File, req Request) error {
	if !typeFits(req.Kind, f.ContentType) {
		return fmt.Errorf("%w: %s was answered as %q, which a page does not load as a %s",
			ErrRefused, f.URL, f.ContentType, req.Kind)
	}
	return checkIntegrity(f, req.Integrity)
}

// typeFits reports whether a CDN's answer type is one a page loads as kind.
func typeFits(k Kind, contentType string) bool {
	essence, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		essence = strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	}
	switch k {
	case KindScript, KindModule:
		return slices.Contains([]string{"text/javascript", "application/javascript",
			"application/x-javascript", "text/ecmascript", "application/ecmascript"}, essence)
	case KindStylesheet:
		return essence == "text/css"
	case KindFont:
		return strings.HasPrefix(essence, "font/") || slices.Contains([]string{
			"application/font-woff", "application/font-woff2", "application/x-font-woff",
			"application/x-font-ttf", "application/x-font-otf", "application/vnd.ms-fontobject",
			"application/octet-stream"}, essence)
	case KindAsset:
		return strings.HasPrefix(essence, "image/") || strings.HasPrefix(essence, "font/") ||
			essence == "application/octet-stream"
	}
	return false
}

// checkIntegrity holds a copy to the page's subresource integrity value: the
// strongest algorithm it names must match one of its digests, as a browser
// judges it. A value naming no algorithm this build knows is refused, since
// the page states a check nothing could make.
func checkIntegrity(f File, integrity string) error {
	if strings.TrimSpace(integrity) == "" {
		return nil
	}
	strength := map[string]int{"sha256": 1, "sha384": 2, "sha512": 3}
	byAlg := map[string][]string{}
	best := ""
	for _, token := range strings.Fields(integrity) {
		token, _, _ = strings.Cut(token, "?")
		alg, digest, ok := strings.Cut(token, "-")
		if !ok || strength[alg] == 0 {
			continue
		}
		byAlg[alg] = append(byAlg[alg], digest)
		if strength[alg] > strength[best] {
			best = alg
		}
	}
	if best == "" {
		return fmt.Errorf("%w: the integrity %q names no sha256, sha384 or sha512 digest",
			ErrRefused, integrity)
	}
	var h hash.Hash
	switch best {
	case "sha256":
		h = sha256.New()
	case "sha384":
		h = sha512.New384()
	default:
		h = sha512.New()
	}
	h.Write(f.Body)
	got := base64.StdEncoding.EncodeToString(h.Sum(nil))
	if slices.Contains(byAlg[best], got) {
		return nil
	}
	return fmt.Errorf("%w: %s does not match the page's integrity value (%s-%s)", ErrRefused,
		f.URL, best, got)
}
