// Package pagepolicy is what a browser is told about every response this
// engine serves: which scripts, styles and connections a page may use, and
// that nobody may frame it, sniff it or learn where it was opened from.
//
// # Why it matters here more than on most servers
//
// The dashboard acts with the session cookie of whoever signed in to it, and
// anything that runs script on its origin acts with that session: the cookie
// is HttpOnly, so a script cannot read it, but every request it makes carries
// it. And the engine serves three kinds of HTML: the dashboard shell, the
// GitHub App landing page and the Slack OAuth landing page. The two landing
// pages are unauthenticated and render values an arriving browser supplies.
// While one listener serves everything they share the dashboard's origin; with
// a public listener (`api.public`) they are served on that socket instead,
// under an address — and possibly a scheme — of its own, and get the same
// headers there, [HSTS] decided by that address. A Content-Security-Policy is
// per response, so a policy on the shell alone would leave exactly those two
// pages as the way in.
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
//
// [HSTS] follows the address the listener is published at (`api.external_url`
// for api.port, `api.public.external_url` for the public and bridge-only
// listeners), never the bind address or `r.TLS`, since a TLS-terminating proxy
// hands the engine plain http. Each listener decides for itself: one flag for
// both sockets sent none on landings published over https beside a private
// http admin side. [ForTemplate] hashes the template as rendered, not its
// source, because html/template strips comments inside a script.
//
// # And the origin that holds no session
//
// The artifact origin (the api.artifacts listener, internal/artifactserve)
// serves pages nobody at this company necessarily wrote, so its policies live
// here too: [ArtifactDocument] for a document in an artifact frame, a strict
// script policy under a nonce fresh to the response; [ArtifactSubresource] for
// a file it loads; [ArtifactPublic] for the embedded runtime and kit. And the
// dashboard's own policy names that origin as the one thing it may frame
// ([DashboardFor]). One package, so every policy this engine writes can be
// read side by side.
//
// What [ArtifactDocument] leaves open (a page's own script may send what its
// viewer reads to any host) is stated on the artifact policy's declaration.
// The one gzip negotiation ([AcceptsGzip], [GzipTag], [GzipSmaller]) lives here
// for the dashboard's assets and the artifact origin alike.
//
// # And a repository's bytes on the dashboard's origin
//
// A file's bytes and a commit's ZIP, served under /git/ beside git, are
// somebody's content on the session's origin, so they are served inert
// ([Download], ADR-0153): text or an attachment, sandboxed, never framed.
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
// Everything it loads is its own, served from /static on this origin, and its
// index.html has no inline script or style, so each fetch directive is 'self':
//   - connect-src covers REST and the /ws/stream socket (CSP3 matches ws: and
//     wss: against 'self' on the same host).
//   - style-src still permits React's `style` prop, written through the CSSOM,
//     which style-src does not govern.
//   - img-src adds data: for images Vite inlines, and blob: for a native
//     artifact's image, fetched with the reader's credential and shown through
//     an object URL minted by script already on this origin: it widens nothing.
//   - object-src and base-uri are 'none': an injected base would re-point every
//     relative URL.
const Dashboard = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data: blob:; font-src 'self'; connect-src 'self'; object-src 'none'; " +
	// form-action is 'self' AND https:, and the https: is load bearing:
	// creating a seat's GitHub App posts a manifest form from the dashboard
	// (`postManifest`) to a host that is github.com or a GitHub Enterprise base
	// the company configures, so no static list names it. Browsers enforce
	// form-action on a form submitted into a new tab too, so 'self' alone makes
	// the button do nothing.
	"base-uri 'none'; frame-ancestors 'none'; form-action 'self' https:; " +
	// TRUSTED TYPES ARE ENFORCED WITH NO POLICY: every string handed to a markup
	// or script sink throws, and no code can create a policy to bless one. The
	// dashboard reaches no such sink, and dashboard/src/app/trustedTypes.test.ts
	// holds the source to that, so injected script finds no markup sink.
	"require-trusted-types-for 'script'; trusted-types 'none'"

// API is the policy for every response that is not a page: JSON, plain text,
// the redirect from `/`, a 404.
//
// Nothing may load and nothing may frame it. A response a browser is never
// meant to render as a document loses nothing to this, and one that ends up
// rendered anyway (an error body opened in a tab, a JSON document with markup
// in a string) cannot run or fetch anything.
const API = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// Inert is the policy of a response nothing may run in, load from or frame.
const Inert = "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; sandbox"

// Download sets the headers of a repository's bytes served on this origin
// (ADR-0153).
func Download(h http.Header) {
	Set(h, Inert)
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

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
// `secure` says a browser reaches THE LISTENER THIS WRAPS over https, which
// adds [HSTS]. It is the address that listener is published at that decides it
// — `api.external_url` for api.port, `api.public.external_url` for the public
// listener, which may be published under another scheme — and never the bind
// address or `r.TLS`: the engine ordinarily sits behind a TLS-terminating
// proxy, so the request it receives is plain http on a loopback socket, and a
// check on either would send the header from nowhere and from exactly the
// deployments that need it.
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
// A YEAR, because the header only protects a browser that has been here once,
// so a short max-age leaves a window on every device not seen lately.
//
// NO `includeSubDomains`, a decision rather than an omission: this engine is
// one host and knows nothing about its neighbours, and sending it at
// `crewlet.example.com` would commit every other name under `example.com` to
// https for a year, ones whose owners never agreed and cannot undo it. An
// operator sets it at the proxy that owns the domain. NO `preload` for the
// same reason: enrolling in a list shipped inside browsers is near
// irreversible.
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
// The hashes are of the template RENDERED, never its source: html/template
// strips comments inside <script> and <style> when it escapes, so a hash of
// the source would refuse the page's own script. views must between them reach
// every conditional branch holding an inline block, and the blocks must not
// depend on the data, since a hash is fixed once (a per-request script needs a
// nonce). img-src data: lets a page show the product's mark INLINED: these
// pages are self-contained and public, served where /static is not. A script
// with a src attribute is refused for the same reason.
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
	return "default-src 'none'; img-src data:; " +
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
