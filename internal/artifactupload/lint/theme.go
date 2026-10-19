package lint

import (
	"bytes"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/html"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// Theme is what one page or stylesheet does about its reader's appearance:
// the facts the publish pipeline folds over a whole version before it warns
// ([CodeNoDarkMode]), since the rules that theme a page and the page itself are
// often different files.
type Theme struct {
	// Rules reports a prefers-color-scheme rule in the file's own text.
	Rules bool

	// Kit is every link the file makes to a theming file of the embedded
	// artifact kit ([artifacts.KitStylesheet] and [artifacts.KitTheme] under
	// [artifacts.KitPrefix]), in the order written. Whether the version it
	// names is one the build serves is the pipeline's question, not the
	// linter's.
	Kit []KitLink
}

// KitLink is one link to a theming file of the artifact kit.
type KitLink struct {
	// Version is the kit release the link names.
	Version string

	// File is [artifacts.KitStylesheet] or [artifacts.KitTheme].
	File string

	// Line is where the link is written.
	Line int
}

// ThemeOf reads what a page (isHTML) or a stylesheet does about its reader's
// appearance.
//
// A KIT FILE COUNTS ONLY WHERE A BROWSER LOADS IT AS WHAT IT IS: the
// stylesheet from a `<link rel="stylesheet">` or an `@import` (in a page's
// `<style>` or in a stylesheet), the theme bootstrap from a `<script src>`.
// A stylesheet named by a script, or the bootstrap by a link, loads nothing
// that themes the page, and is no reason to stay quiet.
func ThemeOf(text []byte, isHTML bool) Theme {
	t := Theme{Rules: bytes.Contains(bytes.ToLower(text), []byte("prefers-color-scheme"))}
	if !isHTML {
		t.Kit = cssKitLinks(text, 0)
		return t
	}
	z := html.NewTokenizer(bytes.NewReader(text))
	line, inStyle := 1, false
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return t
		}
		at := line
		raw := z.Raw()
		line += bytes.Count(raw, []byte("\n"))
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			name := strings.ToLower(tok.Data)
			inStyle = name == "style" && tt == html.StartTagToken
			if link, ok := tagKitLink(name, tok.Attr, at); ok {
				t.Kit = append(t.Kit, link)
			}
		case html.TextToken:
			if inStyle {
				t.Kit = append(t.Kit, cssKitLinks(raw, at-1)...)
			}
		case html.EndTagToken:
			inStyle = false
		}
	}
}

// tagKitLink is the kit link one start tag makes, if it makes one.
func tagKitLink(name string, attrs []html.Attribute, line int) (KitLink, bool) {
	get := func(key string) string {
		for _, a := range attrs {
			if a.Namespace == "" && a.Key == key {
				return a.Val
			}
		}
		return ""
	}
	switch name {
	case "link":
		if !slices.Contains(strings.Fields(strings.ToLower(get("rel"))), "stylesheet") {
			return KitLink{}, false
		}
		return kitLink(get("href"), artifacts.KitStylesheet, line)
	case "script":
		return kitLink(get("src"), artifacts.KitTheme, line)
	}
	return KitLink{}, false
}

// cssKitLinks is every @import of the kit's stylesheet in a stylesheet, its
// lines offset by base (the line a page's <style> text starts on, less one).
func cssKitLinks(text []byte, base int) []KitLink {
	var out []KitLink
	for _, r := range scanCSS(text) {
		if r.kind != RefStylesheet {
			continue
		}
		if link, ok := kitLink(r.url, artifacts.KitStylesheet, base+r.line); ok {
			out = append(out, link)
		}
	}
	return out
}

// kitLink reads ref as a link to the kit file want: a path on the artifact's
// own origin, `<prefix><version>/<want>`, with any query or fragment ignored
// (the origin serves the file whatever follows it). An absolute URL is not
// one: the kit is served on every artifact host and a page names it by path,
// while a URL naming a host is held to the CDN allowlist like any other.
func kitLink(ref, want string, line int) (KitLink, bool) {
	n := normalizeRef(ref)
	n, _, _ = strings.Cut(n, "#")
	n, _, _ = strings.Cut(n, "?")
	if unescaped, err := url.PathUnescape(n); err == nil {
		n = unescaped
	}
	rest, ok := strings.CutPrefix(n, artifacts.KitPrefix)
	if !ok {
		return KitLink{}, false
	}
	version, file, ok := strings.Cut(rest, "/")
	if !ok || version == "" || file != want {
		return KitLink{}, false
	}
	return KitLink{Version: version, File: file, Line: line}, true
}
