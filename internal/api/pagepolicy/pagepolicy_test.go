package pagepolicy_test

import (
	"crypto/sha256"
	"encoding/base64"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/pagepolicy"
)

// ALL FOUR HEADERS, from the one call.
func TestSetWritesEveryHeader(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	pagepolicy.Set(h, "default-src 'none'")
	for name, want := range map[string]string{
		"Content-Security-Policy": "default-src 'none'",
		"X-Frame-Options":         "DENY",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
	} {
		if got := h.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// THE MIDDLEWARE SETS THE API POLICY BEFORE THE HANDLER RUNS, so a handler
// that writes a status straight away still sends it, and one that serves a
// page can replace it.
func TestApplyCoversAHandlerThatThoughtNothingAboutHeaders(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		want    string
	}{
		"a bare not found": {http.NotFound, pagepolicy.API},
		"a page that replaces it": {func(w http.ResponseWriter, _ *http.Request) {
			pagepolicy.Set(w.Header(), pagepolicy.Dashboard)
			w.WriteHeader(http.StatusOK)
		}, pagepolicy.Dashboard},
	} {
		rec := httptest.NewRecorder()
		pagepolicy.Apply(tc.handler, false).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if got := rec.Header().Get("Content-Security-Policy"); got != tc.want {
			t.Errorf("%s: policy = %q, want %q", name, got, tc.want)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q", name, got)
		}
	}
}

// THE DASHBOARD'S FORM-ACTION ALLOWS HTTPS, and the reason is a real flow: the
// per-seat GitHub App manifest is a form posted to a configurable code host.
// Losing the https: source leaves the Create app button doing nothing at all.
func TestTheDashboardPolicyLetsTheManifestFormReachACodeHost(t *testing.T) {
	t.Parallel()
	directives := map[string]string{}
	for _, part := range strings.Split(pagepolicy.Dashboard, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), " ")
		directives[name] = value
	}
	for name, want := range map[string]string{
		"default-src":     "'self'",
		"script-src":      "'self'",
		"style-src":       "'self'",
		"img-src":         "'self' data:",
		"font-src":        "'self'",
		"connect-src":     "'self'",
		"object-src":      "'none'",
		"base-uri":        "'none'",
		"frame-ancestors": "'none'",
		"form-action":     "'self' https:",
	} {
		if got := directives[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if len(directives) != 10 {
		t.Errorf("the dashboard policy has %d directives, want exactly the ten above: %s",
			len(directives), pagepolicy.Dashboard)
	}
}

// THE HASH IS OF THE RENDERED BLOCK, not of the template's source text.
//
// html/template drops a comment inside a <script> or <style> when it escapes
// the template, so the source and the bytes a browser receives differ. A hash
// of the source would refuse the page's own script; this template carries a
// comment in both blocks so a policy computed that way fails here.
func TestATemplatePolicyHashesWhatTheBrowserReceives(t *testing.T) {
	t.Parallel()
	page := template.Must(template.New("page").Parse(`<!doctype html>
<style>/* a note for the reader */ body { margin: 0; }</style>
<p>{{.}}</p>
<script>// a note for the reader
var x = 1;</script>`))

	policy, err := pagepolicy.ForTemplate(page, "hello")
	if err != nil {
		t.Fatalf("ForTemplate: %v", err)
	}
	var rendered strings.Builder
	if err := page.Execute(&rendered, "anything else"); err != nil {
		t.Fatalf("render: %v", err)
	}
	styles, scripts := pagepolicy.InlineBlocks([]byte(rendered.String()))
	if len(styles) != 1 || len(scripts) != 1 {
		t.Fatalf("found %d styles and %d scripts in the rendered page, want one each", len(styles), len(scripts))
	}
	for _, block := range append(styles, scripts...) {
		if !strings.Contains(policy, pagepolicy.Hash(block)) {
			t.Errorf("the policy does not allow the rendered block %q: %s", block, policy)
		}
	}
	source := "// a note for the reader\nvar x = 1;"
	if strings.Contains(policy, pagepolicy.Hash(source)) {
		t.Errorf("the policy hashed the template source, which is not what the browser receives")
	}
	for _, want := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(policy, want) {
			t.Errorf("the policy lacks %q: %s", want, policy)
		}
	}
}

// A PAGE WITH NO SCRIPT MAY RUN NONE.
func TestAPageWithNoBlockOfAKindAllowsNone(t *testing.T) {
	t.Parallel()
	page := template.Must(template.New("plain").Parse(`<style>p{}</style><p>{{.}}</p>`))
	policy := pagepolicy.MustForTemplate(page, "x")
	if !strings.Contains(policy, "script-src 'none'") {
		t.Errorf("a page with no script allows some: %s", policy)
	}
	if strings.Contains(policy, "style-src 'none'") {
		t.Errorf("a page with a style block allows none: %s", policy)
	}
}

// EVERY BRANCH A VIEW REACHES IS HASHED, so a block behind a conditional is
// allowed as long as one view renders it.
func TestEveryViewContributesItsBlocks(t *testing.T) {
	t.Parallel()
	page := template.Must(template.New("branches").Parse(
		`{{if .}}<script>var on = 1;</script>{{else}}<script>var off = 1;</script>{{end}}`))
	policy := pagepolicy.MustForTemplate(page, true, false)
	for _, block := range []string{"var on = 1;", "var off = 1;"} {
		if !strings.Contains(policy, pagepolicy.Hash(block)) {
			t.Errorf("the policy lacks the hash of %q: %s", block, policy)
		}
	}
}

// A SCRIPT THAT LOADS FROM ELSEWHERE IS REFUSED: a hash covers content, and a
// src has none to hash.
func TestAnExternalScriptIsRefused(t *testing.T) {
	t.Parallel()
	page := template.Must(template.New("external").Parse(`<script src="/x.js"></script>`))
	if _, err := pagepolicy.ForTemplate(page, nil); err == nil {
		t.Error("a script with a src attribute was accepted into a hash policy")
	}
	if _, err := pagepolicy.ForTemplate(page); err == nil {
		t.Error("a policy was computed without rendering the page at all")
	}
}

// THE HASH IS THE CSP SOURCE EXPRESSION a browser compares against.
func TestHashIsTheSourceExpression(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte("body{}"))
	want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	if got := pagepolicy.Hash("body{}"); got != want {
		t.Errorf("Hash = %s, want %s", got, want)
	}
}

// HSTS FOLLOWS THE DEPLOYMENT'S OWN SCHEME, not the request's.
//
// The engine ordinarily sits behind a TLS-terminating proxy, so the request it
// receives is plain http on a loopback socket: a check on `r.TLS` or on the
// bind address would withhold the header from exactly the deployments that
// need it. And an http deployment must not send it at all — a browser that
// accepted one would refuse to reach this engine again for a year, over the
// only scheme it is served on.
func TestStrictTransportSecurityFollowsTheDeployment(t *testing.T) {
	t.Parallel()
	get := func(secure bool) string {
		rec := httptest.NewRecorder()
		pagepolicy.Apply(http.NotFoundHandler(), secure).
			ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		return rec.Header().Get("Strict-Transport-Security")
	}
	if got := get(false); got != "" {
		t.Errorf("an http deployment sent %q, which would make a browser refuse "+
			"to reach it again over the only scheme it serves", got)
	}
	if got := get(true); got != pagepolicy.HSTS {
		t.Errorf("an https deployment sent %q, want %q", got, pagepolicy.HSTS)
	}
	// AND IT COMMITS NO DOMAIN BUT THIS ONE. `includeSubDomains` would
	// bind every other name under the parent to https for a year,
	// including ones somebody else serves; `preload` is a submission to a
	// list shipped inside browsers and is close to irreversible.
	for _, directive := range []string{"includeSubDomains", "preload"} {
		if strings.Contains(pagepolicy.HSTS, directive) {
			t.Errorf("HSTS carries %s, which commits domains this engine does "+
				"not own and cannot undo", directive)
		}
	}
}
