// Package pagepolicy is what a browser is told about every response this
// engine serves: which scripts, styles and connections a page may use, and
// that nobody may frame it, sniff it or learn where it was opened from.
//
// # Why it matters here more than on most servers
//
// The operator token that writes /config and /secrets lives in the dashboard
// origin's localStorage. Anything that runs script on that origin can read it,
// and the engine serves three kinds of HTML there: the dashboard shell, the
// GitHub App landing page and the Slack OAuth landing page. The two landing
// pages are unauthenticated and render values an arriving browser supplies.
// A Content-Security-Policy is per response, so a policy on the shell alone
// would leave exactly those two pages as the way in.
//
// # One helper, set before the status
//
// [Set] writes all four headers into a header map, and every caller runs it
// before anything writes a status line. That ordering is the whole rule: a
// header set after WriteHeader is silently dropped, which is how a 304 from
// the asset handler used to go out with none of the headers its 200 carried.
//
// The API applies [API] to every response before routing, so a response no
// handler thought about (the mux's own 404 and 405, the redirect from `/`) is
// covered by construction. A handler that serves a page replaces the policy
// with the one that page needs.
package pagepolicy

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html/template"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// Dashboard is the policy for the dashboard shell and every static asset.
//
// Everything the built dashboard loads is its own: the entry module, the
// vendor chunk, the stylesheet and the embedded fonts are all served from
// /static on this origin, and the index.html Vite emits carries no inline
// script or style. So each fetch directive is 'self' and nothing else:
//
//   - connect-src 'self' covers the REST calls and the /ws/stream socket. CSP
//     Level 3 matches ws: and wss: against 'self' on the same host, which every
//     browser the dashboard supports implements.
//   - style-src 'self' still permits React's `style` prop, because React writes
//     it through the CSSOM rather than as a markup attribute, and the CSSOM is
//     not subject to style-src.
//   - img-src adds data: because Vite inlines an imported image below its
//     assetsInlineLimit as a data: URI, so a small icon added to the source
//     arrives in the bundle in that form rather than as a file.
//   - object-src and base-uri are 'none': the dashboard embeds no plugin and
//     sets no base, and a base tag injected into the shell would otherwise
//     re-point every relative URL.
//
// form-action is 'self' AND https:, and the https: is load bearing. Creating a
// seat's GitHub App posts a manifest form from the dashboard to the code host
// (dashboard/src/routes/Integrations.tsx, postManifest), because the host must
// see the operator's own session and render its own confirmation page. That
// host is github.com or a GitHub Enterprise Server base the company configures,
// so no static host list can name it. Browsers enforce form-action on a form
// submitted into a new tab too, so 'self' alone makes the button do nothing
// but log a violation.
const Dashboard = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; " +
	"base-uri 'none'; frame-ancestors 'none'; form-action 'self' https:"

// API is the policy for every response that is not a page: JSON, plain text,
// the redirect from `/`, a 404.
//
// Nothing may load and nothing may frame it. A response a browser is never
// meant to render as a document loses nothing to this, and one that ends up
// rendered anyway (an error body opened in a tab, a JSON document with markup
// in a string) cannot run or fetch anything.
const API = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// Set writes the security headers for one response, with the given
// Content-Security-Policy. Call it before anything writes the status.
//
//   - X-Frame-Options: DENY restates frame-ancestors 'none' for a browser that
//     predates it.
//   - X-Content-Type-Options: nosniff, so a body is only ever what its
//     Content-Type says. A JSON error carrying a string of markup must not be
//     promoted to a document.
//   - Referrer-Policy: no-referrer, because a dashboard URL can carry a seat
//     handle, a revision id or a search, and a link out to a vendor must not
//     hand it over.
func Set(h http.Header, policy string) {
	h.Set("Content-Security-Policy", policy)
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

// Apply is middleware that sets the [API] policy on every response before the
// next handler runs, so a handler that serves a page only has to replace it.
//
// `secure` says a browser reaches this deployment over https, which adds
// [HSTS]. It is the deployment's `api.external_url` that decides it and never
// the bind address or `r.TLS`: the engine ordinarily sits behind a
// TLS-terminating proxy, so the request it receives is plain http on a
// loopback socket, and a check on either would send the header from nowhere
// and from exactly the deployments that need it.
func Apply(next http.Handler, secure bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Set(w.Header(), API)
		if secure {
			w.Header().Set("Strict-Transport-Security", HSTS)
		}
		next.ServeHTTP(w, r)
	})
}

// HSTS is what an https deployment tells a browser about coming back.
//
// # Why a year, and why subdomains are not included
//
// A YEAR is what makes it worth setting at all: the header only protects a
// browser that has already been here once, so a short max-age leaves a window
// open on every device that has not visited recently — which is the device an
// attacker on a café network is waiting for.
//
// NO `includeSubDomains`, and that is a decision rather than an omission. This
// engine is one host and knows nothing about its neighbours: a deployment at
// `crewlet.example.com` sending it would commit every other name under
// `example.com` to https for a year, including ones served by somebody who
// never agreed to it and cannot undo it inside that year. An operator who
// wants it sets it at the proxy that actually owns the domain.
//
// NO `preload` for the sharper form of the same reason: preloading is a
// submission to a list shipped inside browsers, and it is close to
// irreversible. Nothing this engine emits should enrol a domain in that.
const HSTS = "max-age=31536000"

// inlineStyle and inlineScript find an inline block and its content.
//
// A regular expression over the engine's OWN rendered templates, never over
// input: the pages this reads are constants in this binary, rendered with
// representative views, so there is no adversarial markup to parse.
var (
	inlineStyle  = regexp.MustCompile(`(?is)<style\b([^>]*)>(.*?)</style>`)
	inlineScript = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
)

// ForTemplate builds the policy for a page rendered from an html/template: its
// inline style and script blocks are allowed by their sha256 hashes, and
// nothing else runs or loads.
//
// The hashes are taken from the template RENDERED, never from its source text.
// html/template strips comments inside a <script> or <style> block when it
// escapes the template, so the bytes a browser hashes are not the bytes in the
// Go source, and a hash of the source would refuse the page's own script.
//
// views are the data to render the template with, and between them they must
// reach every conditional branch that holds an inline block. The blocks
// themselves must not depend on the data, because a hash is fixed once: a page
// whose script varies per request needs a nonce rather than this, and the
// tests of each page check a real response's blocks against its real header.
//
// img-src 'self' lets a page show the product's mark from /static. A script
// with a src attribute is refused: the pages this serves are self-contained so
// they render when the dashboard bundle is not built.
func ForTemplate(page *template.Template, views ...any) (string, error) {
	if len(views) == 0 {
		return "", fmt.Errorf("pagepolicy: %s: render it with at least one view", page.Name())
	}
	styles, scripts := map[string]bool{}, map[string]bool{}
	for i, view := range views {
		var out bytes.Buffer
		if err := page.Execute(&out, view); err != nil {
			return "", fmt.Errorf("pagepolicy: %s: rendering view %d: %w", page.Name(), i, err)
		}
		if err := collect(page.Name(), inlineStyle, out.Bytes(), styles); err != nil {
			return "", err
		}
		if err := collect(page.Name(), inlineScript, out.Bytes(), scripts); err != nil {
			return "", err
		}
	}
	return "default-src 'none'; img-src 'self'; " +
		"style-src " + sources(styles) + "; script-src " + sources(scripts) + "; " +
		"base-uri 'none'; form-action 'none'; frame-ancestors 'none'", nil
}

// MustForTemplate is [ForTemplate] for a package-level policy, panicking on an
// error the way template.Must does: the template is a constant of this binary,
// so a failure is a defect that has to stop the build's tests, not a condition
// to handle at run time.
func MustForTemplate(page *template.Template, views ...any) string {
	policy, err := ForTemplate(page, views...)
	if err != nil {
		panic(err)
	}
	return policy
}

// Hash is the CSP source expression for one inline block's content.
func Hash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// InlineBlocks returns the content of every inline style and script block in a
// rendered page, in document order. Exported for the tests of the pages that
// use [ForTemplate], which check a real response against its own header.
func InlineBlocks(page []byte) (styles, scripts []string) {
	for _, m := range inlineStyle.FindAllSubmatch(page, -1) {
		styles = append(styles, string(m[2]))
	}
	for _, m := range inlineScript.FindAllSubmatch(page, -1) {
		scripts = append(scripts, string(m[2]))
	}
	return styles, scripts
}

// collect records the hash of every block the pattern finds.
func collect(name string, pattern *regexp.Regexp, page []byte, into map[string]bool) error {
	for _, m := range pattern.FindAllSubmatch(page, -1) {
		if srcAttribute.Match(m[1]) {
			return fmt.Errorf("pagepolicy: %s: a block loads from a src attribute; "+
				"a page served under a hash policy must inline what it runs", name)
		}
		into[Hash(string(m[2]))] = true
	}
	return nil
}

// srcAttribute finds a src attribute among a tag's attributes.
var srcAttribute = regexp.MustCompile(`(?i)(^|\s)src\s*=`)

// sources renders a set of hashes as a directive value, sorted so the header
// is byte-stable, or 'none' for a page with no block of that kind.
func sources(hashes map[string]bool) string {
	if len(hashes) == 0 {
		return "'none'"
	}
	return strings.Join(slices.Sorted(maps.Keys(hashes)), " ")
}
