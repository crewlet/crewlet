// Package research reads web pages for Ask Crewlet: any public https page,
// GET only, no cookies or credentials, redirects only to public https
// addresses, bounded in size and time, and cached on this node. The client
// refuses a private, loopback or link-local address at connect time.
package research

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// The bounds of a read (ADR-0151).
const (
	// MaxBodyBytes is the most of a page read: a documentation page is
	// typically under 500 KiB of HTML.
	MaxBodyBytes = 2 << 20
	// Timeout bounds one fetch, as the artifact CDN fetch is bounded.
	Timeout = 15 * time.Second
	// CacheTTL is how long a page is answered from this node's cache.
	CacheTTL = 10 * time.Minute
	// PartBytes is the most text one read answers; `from` reads the next part.
	// Twelve KiB leaves the model's 16 KiB room for the fence and the facts.
	PartBytes = 12 << 10
	// MaxCached bounds the cache's entries, the oldest dropped first.
	MaxCached = 256
	// MaxRedirects is how many redirects a read follows.
	MaxRedirects = 5
	// MaxLinks is how many of a page's links a read answers.
	MaxLinks = 40
	// UserAgent is the one header a fetch sends that says who asks.
	UserAgent = "Crewlet-Research/1.0"
)

// The refusals, each naming what to change.
var (
	ErrNotPublic = errors.New("research: the address is not a public https page")
	ErrNotPage   = errors.New("research: the address is not a page of text")
)

// Page is one part of a page as read.
type Page struct {
	URL    string    `json:"url"`
	Host   string    `json:"host"`
	Title  string    `json:"title" untrusted:"name"`
	Text   string    `json:"text" untrusted:"fence"`
	From   int       `json:"from"`
	Next   int       `json:"next,omitempty"`
	Length int       `json:"length"`
	ReadAt time.Time `json:"read_at"`
	// Links are the page's own links, https and absolute, at most [MaxLinks]:
	// addresses a later read may follow, since the page wrote them.
	Links []string `json:"links"`
}

type cached struct {
	title, text string
	links       []string
	at          time.Time
}

// Reader reads pages through client, caching them by URL.
type Reader struct {
	client *http.Client
	now    func() time.Time
	mu     sync.Mutex
	cache  map[string]cached
	order  []string
}

// New is a reader over client, which must refuse private addresses
// (httpx.Guarded) everywhere but a test.
func New(client *http.Client, now func() time.Time) *Reader {
	return &Reader{client: client, now: now, cache: map[string]cached{}}
}

// Read answers the part of the page at raw starting at byte from of its text,
// refusing an address that is not a public https page.
func (r *Reader) Read(ctx context.Context, raw string, from int) (Page, error) {
	u, err := Public(raw)
	if err != nil {
		return Page{}, err
	}
	key := u.String()
	c, err := r.page(ctx, key)
	if err != nil {
		return Page{}, err
	}
	title, text, at := c.title, c.text, c.at
	from = min(max(from, 0), len(text))
	for from > 0 && from < len(text) && !utf8.RuneStart(text[from]) {
		from--
	}
	end := cut(text, from, PartBytes)
	p := Page{URL: key, Host: u.Hostname(), Title: title, Text: text[from:end], From: from,
		Length: len(text), ReadAt: at, Links: slices.Clone(c.links)}
	if p.Links == nil {
		p.Links = []string{}
	}
	if end < len(text) {
		p.Next = end
	}
	return p, nil
}

// Public is raw parsed and held to a public https page: https on port 443, a
// host, no credentials in it, its fragment dropped. Whether the host's address
// is public is the client's to decide at connect time ([New]).
func Public(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Port() != "" && u.Port() != "443") {
		return nil, fmt.Errorf("%w: %q", ErrNotPublic, raw)
	}
	u.Fragment = ""
	return u, nil
}

// page is the page from the cache, or read now.
func (r *Reader) page(ctx context.Context, key string) (cached, error) {
	r.mu.Lock()
	c, hit := r.cache[key]
	r.mu.Unlock()
	if hit && r.now().Sub(c.at) < CacheTTL {
		return c, nil
	}
	c, err := r.fetch(ctx, key)
	if err != nil {
		return cached{}, err
	}
	c.at = r.now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, held := r.cache[key]; !held {
		r.order = append(r.order, key)
	}
	r.cache[key] = c
	for len(r.order) > MaxCached {
		delete(r.cache, r.order[0])
		r.order = r.order[1:]
	}
	return c, nil
}

func (r *Reader) fetch(ctx context.Context, key string) (cached, error) {
	client := *r.client
	client.Jar = nil
	client.Timeout = Timeout
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= MaxRedirects {
			return errors.New("research: too many redirects")
		}
		if _, err := Public(req.URL.String()); err != nil {
			return fmt.Errorf("%w: it redirects to %s", ErrNotPublic, req.URL.Redacted())
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, key, nil)
	if err != nil {
		return cached{}, fmt.Errorf("research: %w", err)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html, text/plain, text/markdown")
	resp, err := client.Do(req)
	if err != nil {
		return cached{}, fmt.Errorf("research: read %s: %w", key, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return cached{}, fmt.Errorf("research: %s answered %s", key, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return cached{}, fmt.Errorf("research: read %s: %w", key, err)
	}
	if len(body) > MaxBodyBytes {
		return cached{}, fmt.Errorf("%w: %s is larger than %d bytes", ErrNotPage, key, MaxBodyBytes)
	}
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch media {
	case "text/html", "application/xhtml+xml":
		title, text := Text(string(body))
		return cached{title: title, text: text, links: Links(string(body), resp.Request.URL)}, nil
	case "text/plain", "text/markdown":
		return cached{text: strings.ToValidUTF8(string(body), "")}, nil
	}
	return cached{}, fmt.Errorf("%w: %s is %s", ErrNotPage, key, media)
}

// Links are a page's links as absolute public https addresses, each once, at
// most [MaxLinks], resolved against base, the address the page was read at.
func Links(page string, base *url.URL) []string {
	z := html.NewTokenizer(strings.NewReader(page))
	var out []string
	for len(out) < MaxLinks {
		switch z.Next() {
		case html.ErrorToken:
			return out
		case html.StartTagToken, html.SelfClosingTagToken:
			name, attrs := z.TagName()
			if string(name) != "a" || !attrs {
				continue
			}
			for {
				key, value, more := z.TagAttr()
				if string(key) == "href" {
					if link := absolute(base, string(value)); link != "" && !slices.Contains(out, link) {
						out = append(out, link)
					}
				}
				if !more {
					break
				}
			}
		}
	}
	return out
}

// absolute is href resolved against base as a public https address, or "".
func absolute(base *url.URL, href string) string {
	ref, err := url.Parse(strings.TrimSpace(href))
	if err != nil || base == nil {
		return ""
	}
	u, err := Public(base.ResolveReference(ref).String())
	if err != nil {
		return ""
	}
	return u.String()
}

// cut is the end of the part of text from from, at most n bytes, ended at the
// last line break inside it where there is one.
func cut(text string, from, n int) int {
	end := min(from+n, len(text))
	if end == len(text) {
		return end
	}
	if i := strings.LastIndexByte(text[from:end], '\n'); i > 0 {
		return from + i + 1
	}
	for end > from && !utf8.RuneStart(text[end]) {
		end--
	}
	return end
}

// skipped are the elements whose text is not the page's.
var skipped = map[string]bool{"script": true, "style": true, "noscript": true, "svg": true,
	"template": true, "nav": true, "header": true, "footer": true, "iframe": true}

// block are the elements that end a line of text.
var block = map[string]bool{"p": true, "div": true, "br": true, "li": true, "tr": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "pre": true,
	"section": true, "article": true, "table": true, "ul": true, "ol": true, "dt": true,
	"dd": true, "blockquote": true}

// Text is an HTML page's title and its readable text, a line per block.
func Text(page string) (string, string) {
	z := html.NewTokenizer(strings.NewReader(page))
	var title, b strings.Builder
	depth, pre, inTitle := 0, 0, false
	unbreak := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")
	for {
		switch z.Next() {
		case html.ErrorToken:
			return strings.Join(strings.Fields(title.String()), " "), tidy(b.String())
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			tag := string(name)
			switch {
			case skipped[tag]:
				depth++
			case tag == "title":
				inTitle = true
			case block[tag]:
				b.WriteByte('\n')
			}
			if tag == "pre" {
				pre++
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			switch {
			case skipped[tag] && depth > 0:
				depth--
			case tag == "title":
				inTitle = false
			case block[tag]:
				b.WriteByte('\n')
			}
			if tag == "pre" && pre > 0 {
				pre--
			}
		case html.TextToken:
			switch {
			case inTitle:
				title.Write(z.Text())
			case depth == 0 && pre > 0:
				b.Write(z.Text())
			case depth == 0:
				// Source line breaks inside a paragraph are spaces.
				b.WriteString(unbreak.Replace(string(z.Text())))
			}
		}
	}
}

// tidy is text with each line's spaces collapsed and blank lines dropped.
func tidy(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if f := strings.Join(strings.Fields(line), " "); f != "" {
			out = append(out, f)
		}
	}
	return strings.ToValidUTF8(strings.Join(out, "\n"), "")
}
