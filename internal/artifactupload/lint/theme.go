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

	// Kit is every link the file makes to a file of the embedded artifact
	// kit (under [artifacts.KitPrefix]) that a browser loads, in the order
	// written. Whether the release it names is one the build serves, and
	// whether that release ships the file, is the pipeline's question, not
	// the linter's.
	Kit []KitLink
}

// KitLink is one link to a file of the artifact kit.
type KitLink struct {
	// Version is the kit release the link names.
	Version string

	// File is the path inside the release the link names: `kit.css`,
	// `content.css`, `theme.js`, `kit.js`.
	File string

	// As is how the browser loads it.
	As KitLoad

	// Line is where the link is written.
	Line int
}

// KitLoad is how a page loads a kit file.
type KitLoad string

const (
	// KitAsStylesheet is a `<link rel="stylesheet">` or an `@import`.
	KitAsStylesheet KitLoad = "stylesheet"

	// KitAsScript is a `<script src>`.
	KitAsScript KitLoad = "script"
)

// ThemesStylesheet reports whether the link loads the kit's palettes: its
// stylesheet, loaded as one.
func (k KitLink) ThemesStylesheet() bool {
	return k.File == artifacts.KitStylesheet && k.As == KitAsStylesheet
}

// ThemesBootstrap reports whether the link loads the kit's theme bootstrap:
// its script, loaded as one.
func (k KitLink) ThemesBootstrap() bool {
	return k.File == artifacts.KitTheme && k.As == KitAsScript
}

// ThemeOf reads what a page (isHTML) or a stylesheet does about its reader's
// appearance.
//
// A KIT FILE COUNTS WHERE A BROWSER LOADS IT: from a `<link rel="stylesheet">`
// or an `@import` (in a page's `<style>` or in a stylesheet), or from a
// `<script src>`, each recorded with how it is loaded. Only the stylesheet
// loaded as one and the bootstrap loaded as a script theme the page
// ([KitLink.ThemesStylesheet], [KitLink.ThemesBootstrap]); every link is
// still one the release must ship.
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
		return kitLink(get("href"), KitAsStylesheet, line)
	case "script":
		return kitLink(get("src"), KitAsScript, line)
	}
	return KitLink{}, false
}

// cssKitLinks is every @import of a kit file in a stylesheet, its lines
// offset by base (the line a page's <style> text starts on, less one).
func cssKitLinks(text []byte, base int) []KitLink {
	var out []KitLink
	for _, r := range scanCSS(text) {
		if r.kind != RefStylesheet {
			continue
		}
		if link, ok := kitLink(r.url, KitAsStylesheet, base+r.line); ok {
			out = append(out, link)
		}
	}
	return out
}

// kitLink reads ref as a link to a kit file, loaded as as: a path on the
// artifact's own origin, `<prefix><version>/<file>`, with any query or
// fragment ignored (the origin serves the file whatever follows it). An
// absolute URL is not one: the kit is served on every artifact host and a page
// names it by path, while a URL naming a host is held to the CDN allowlist
// like any other.
func kitLink(ref string, as KitLoad, line int) (KitLink, bool) {
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
	if !ok || version == "" || file == "" || strings.HasSuffix(file, "/") {
		return KitLink{}, false
	}
	return KitLink{Version: version, File: file, As: as, Line: line}, true
}
