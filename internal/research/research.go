// Package research reads web pages for Ask Crewlet from the hosts the company
// lists (ADR-0151): https and GET only, no cookies or credentials, redirects only
// to listed hosts, bounded in size and time, and cached on this node.
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
	// UserAgent is the one header a fetch sends that says who asks.
	UserAgent = "Crewlet-Research/1.0"
)

// The refusals, each naming what to change.
var (
	ErrNotListed = errors.New("research: the page is not on a host the company lists")
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
}

type cached struct {
	title, text string
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
// refusing an address that is not https on one of hosts.
func (r *Reader) Read(ctx context.Context, hosts []string, raw string, from int) (Page, error) {
	u, err := Allowed(hosts, raw)
	if err != nil {
		return Page{}, err
	}
	key := u.String()
	title, text, at, err := r.page(ctx, hosts, key)
	if err != nil {
		return Page{}, err
	}
	from = min(max(from, 0), len(text))
	for from > 0 && from < len(text) && !utf8.RuneStart(text[from]) {
		from--
	}
	end := cut(text, from, PartBytes)
	p := Page{URL: key, Host: u.Hostname(), Title: title, Text: text[from:end], From: from,
		Length: len(text), ReadAt: at}
	if end < len(text) {
		p.Next = end
	}
	return p, nil
}

// Allowed is raw parsed and held to hosts: https, no credentials, a listed host.
func Allowed(hosts []string, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("%w: %q is not an https address", ErrNotListed, raw)
	}
	u.Fragment = ""
	if !listed(hosts, u.Hostname()) || u.Port() != "" && u.Port() != "443" {
		return nil, fmt.Errorf("%w: %s is not one of %s", ErrNotListed, u.Hostname(),
			strings.Join(hosts, ", "))
	}
	return u, nil
}

func listed(hosts []string, host string) bool {
	return slices.ContainsFunc(hosts, func(h string) bool { return strings.EqualFold(h, host) })
}

// page is the page's title and text from the cache, or read now.
func (r *Reader) page(ctx context.Context, hosts []string, key string) (string, string,
	time.Time, error) {

	r.mu.Lock()
	c, hit := r.cache[key]
	r.mu.Unlock()
	if hit && r.now().Sub(c.at) < CacheTTL {
		return c.title, c.text, c.at, nil
	}
	title, text, err := r.fetch(ctx, hosts, key)
	if err != nil {
		return "", "", time.Time{}, err
	}
	at := r.now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, held := r.cache[key]; !held {
		r.order = append(r.order, key)
	}
	r.cache[key] = cached{title: title, text: text, at: at}
	for len(r.order) > MaxCached {
		delete(r.cache, r.order[0])
		r.order = r.order[1:]
	}
	return title, text, at, nil
}

func (r *Reader) fetch(ctx context.Context, hosts []string, key string) (string, string, error) {
	client := *r.client
	client.Jar = nil
	client.Timeout = Timeout
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("research: too many redirects")
		}
		if req.URL.Scheme != "https" || !listed(hosts, req.URL.Hostname()) {
			return fmt.Errorf("%w: it redirects to %s", ErrNotListed, req.URL.Host)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, key, nil)
	if err != nil {
		return "", "", fmt.Errorf("research: %w", err)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "text/html, text/plain, text/markdown")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("research: read %s: %w", key, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("research: %s answered %s", key, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return "", "", fmt.Errorf("research: read %s: %w", key, err)
	}
	if len(body) > MaxBodyBytes {
		return "", "", fmt.Errorf("%w: %s is larger than %d bytes", ErrNotPage, key, MaxBodyBytes)
	}
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch media {
	case "text/html", "application/xhtml+xml":
		title, text := Text(string(body))
		return title, text, nil
	case "text/plain", "text/markdown":
		return "", strings.ToValidUTF8(string(body), ""), nil
	}
	return "", "", fmt.Errorf("%w: %s is %s", ErrNotPage, key, media)
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
