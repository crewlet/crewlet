package searchconsole

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/crewlet/crewlet/internal/growth"
)

// The Indexing API's two notifications.
const (
	notifyUpdated = "URL_UPDATED"
	notifyDeleted = "URL_DELETED"
)

// pageLimit is what Googlebot reads of an HTML page, its first 15 MB, so the
// page the engine judges is never shorter than the one Google judges.
const pageLimit = 15 << 20

// pageTimeout bounds reading one page, its body included: the engine's bound
// on any public fetch an agent's call waits on (artifactupload's CDN fetch).
const pageTimeout = 15 * time.Second

// pageAllows refuses an Indexing API notification of page that Google's terms
// do not allow, read now: an update needs a page carrying JobPosting, or
// BroadcastEvent in VideoObject, structured data; a removal one that is gone
// or carries noindex (developers.google.com/search/apis/indexing-api/v3). The
// page itself is judged, never where it redirects: Google is told of it.
func pageAllows(ctx context.Context, client *http.Client, page, notification string) error {
	ctx, cancel := context.WithTimeout(ctx, pageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
	if err != nil {
		return refusal(growth.RefusedInvalid, page+" is not a page's URL. No vendor was reached.")
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	once := *client
	once.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// THE CLIENT'S ERROR IS NOT SAID: it can name a redirect's target or the
	// private address the guard refused.
	unread := refusal(growth.RefusedUnavailable, "the engine could not read "+page+" to check that Google's "+
		"terms allow this Indexing API notification: it did not answer in time, or not from a public "+
		"address. Nothing was sent.")
	resp, err := once.Do(req)
	if err != nil {
		return unread
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, pageLimit))
	if err != nil {
		return unread
	}
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return refusal(growth.RefusedForbidden, fmt.Sprintf("%s answers %d, a redirect: Google takes a "+
			"notification for the page that answers itself, so notify where it leads instead. Nothing was sent.",
			page, resp.StatusCode))
	case notification == notifyDeleted && (resp.StatusCode == http.StatusNotFound ||
		resp.StatusCode == http.StatusGone || (ok && (readPage(body).noindex || noindexHeader(resp.Header)))):
		return nil
	case notification == notifyDeleted:
		return refusal(growth.RefusedForbidden, fmt.Sprintf("%s answers %d without noindex, and Google takes "+
			"URL_DELETED only for a page that answers 404 or 410 or carries noindex. Nothing was sent.", page,
			resp.StatusCode))
	case !ok:
		return refusal(growth.RefusedForbidden, fmt.Sprintf("%s answers %d, so Google could not crawl it. "+
			"Nothing was sent.", page, resp.StatusCode))
	case readPage(body).allowed:
		return nil
	}
	return refusal(growth.RefusedForbidden, page+" carries no JobPosting, nor BroadcastEvent in VideoObject, "+
		"structured data, and Google's terms allow the Indexing API for no other page: notifying it could "+
		"lose the company its access. Nothing was sent. Submit a sitemap (search_console_sitemap_submit) to "+
		"have Google crawl other pages.")
}

// pageRead is what a page's markup says of the Indexing API's terms.
type pageRead struct {
	// allowed is a schema.org JobPosting, or a BroadcastEvent nested in a
	// VideoObject, in JSON-LD, microdata or RDFa.
	allowed bool
	noindex bool
}

// schemaOrg is the vocabulary Google reads structured data in.
const schemaOrg = "schema.org/"

// readPage reads a page's structured data and robots meta tags, following
// each item's nesting as the page's tree holds it.
func readPage(body []byte) pageRead {
	var out pageRead
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return out
	}
	var walk func(n *html.Node, vocab string, inVideo bool)
	walk = func(n *html.Node, vocab string, inVideo bool) {
		if n.Type == html.ElementNode {
			attrs := map[string]string{}
			for _, a := range n.Attr {
				attrs[strings.ToLower(a.Key)] = a.Val
			}
			if v, ok := attrs["vocab"]; ok {
				vocab = v
			}
			var types []string
			if _, scoped := attrs["itemscope"]; scoped {
				for _, t := range strings.Fields(attrs["itemtype"]) {
					types = append(types, schemaType(t, ""))
				}
			}
			for _, t := range strings.Fields(attrs["typeof"]) {
				types = append(types, schemaType(t, vocab))
			}
			if slices.Contains(types, "JobPosting") || (inVideo && slices.Contains(types, "BroadcastEvent")) {
				out.allowed = true
			}
			inVideo = inVideo || slices.Contains(types, "VideoObject")
			robots := strings.ToLower(attrs["name"])
			if n.Data == "meta" && (robots == "robots" || robots == "googlebot") &&
				strings.Contains(strings.ToLower(attrs["content"]), "noindex") {
				out.noindex = true
			}
			if n.Data == "script" && strings.EqualFold(strings.TrimSpace(attrs["type"]), "application/ld+json") &&
				n.FirstChild != nil {
				var v any
				if json.Unmarshal([]byte(n.FirstChild.Data), &v) == nil && ldAllows(v, false) {
					out.allowed = true
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, vocab, inVideo)
		}
	}
	walk(doc, "", false)
	return out
}

// schemaType is a type's schema.org name, "" for one of another vocabulary:
// https://schema.org/JobPosting, schema:JobPosting, or a bare name where the
// vocabulary in force (RDFa's vocab) is schema.org's.
func schemaType(t, vocab string) string {
	t = strings.TrimSpace(t)
	for _, prefix := range []string{"https://" + schemaOrg, "http://" + schemaOrg, "schema:"} {
		if name, ok := strings.CutPrefix(t, prefix); ok && !strings.ContainsAny(name, "/:#") {
			return name
		}
	}
	if !strings.ContainsAny(t, "/:#") && strings.Contains(vocab, schemaOrg) {
		return t
	}
	return ""
}

// ldAllows reports whether a JSON-LD value holds a JobPosting, or a
// BroadcastEvent inside a VideoObject, at any depth (an @graph included). A
// bare type is read in schema.org's vocabulary, the one Google reads.
func ldAllows(v any, inVideo bool) bool {
	switch t := v.(type) {
	case []any:
		return slices.ContainsFunc(t, func(e any) bool { return ldAllows(e, inVideo) })
	case map[string]any:
		var types []string
		switch ty := t["@type"].(type) {
		case string:
			types = []string{schemaType(ty, schemaOrg)}
		case []any:
			for _, e := range ty {
				if s, ok := e.(string); ok {
					types = append(types, schemaType(s, schemaOrg))
				}
			}
		}
		if slices.Contains(types, "JobPosting") || (inVideo && slices.Contains(types, "BroadcastEvent")) {
			return true
		}
		video := inVideo || slices.Contains(types, "VideoObject")
		for k, inner := range t {
			if k != "@type" && ldAllows(inner, video) {
				return true
			}
		}
	}
	return false
}

// noindexHeader reports an X-Robots-Tag that keeps the page out of Google's
// index: one for every crawler, or one naming Googlebot.
func noindexHeader(h http.Header) bool {
	for _, v := range h.Values("X-Robots-Tag") {
		rules := strings.ToLower(v)
		if agent, rest, ok := strings.Cut(rules, ":"); ok && !strings.ContainsAny(agent, ", ") &&
			agent != "unavailable_after" {
			if agent != "googlebot" {
				continue
			}
			rules = rest
		}
		if strings.Contains(rules, "noindex") {
			return true
		}
	}
	return false
}
