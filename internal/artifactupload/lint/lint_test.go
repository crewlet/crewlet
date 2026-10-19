package lint_test

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

const nonce = ` nonce="` + artifacts.NoncePlaceholder + `"`

// resolver copies every reference to _vendor/<host><path>, relative to the
// referencing file, recording what it was asked.
type resolver struct {
	asked []lint.Ref
	fail  error
}

func (r *resolver) resolve(ref lint.Ref) (string, error) {
	r.asked = append(r.asked, ref)
	if r.fail != nil {
		return "", r.fail
	}
	local := "_vendor/" + ref.URL.Host + ref.URL.Path
	rel, _ := relative(path.Dir(ref.From), local)
	return rel, nil
}

func relative(dir, target string) (string, error) {
	if dir == "." {
		return target, nil
	}
	up := strings.Repeat("../", strings.Count(dir, "/")+1)
	return up + target, nil
}

func opts(r *resolver) lint.Options {
	o := lint.Options{Hosts: lint.DefaultAllowlist()}
	if r != nil {
		o.Resolve = r.resolve
	}
	return o
}

func codes(t *testing.T, err error) []lint.Code {
	t.Helper()
	var refusal *lint.Refusal
	if !errors.As(err, &refusal) || !errors.Is(err, lint.ErrRefused) {
		t.Fatalf("answered %v, want a *lint.Refusal", err)
	}
	var out []lint.Code
	for _, f := range refusal.Findings {
		out = append(out, f.Code)
	}
	return out
}

func warned(p lint.Page, code lint.Code) bool {
	return slices.ContainsFunc(p.Warnings, func(f lint.Finding) bool { return f.Code == code })
}

// EVERY SCRIPT ELEMENT CARRIES THE PLACEHOLDER AS THE FIRST NONCE A BROWSER
// READS, and every other byte of the page is the author's: upper case,
// unquoted and self-closing spellings included.
func TestEveryScriptCarriesTheNoncePlaceholder(t *testing.T) {
	t.Parallel()
	src := `<!doctype html><meta name="viewport" content="width=device-width">
<SCRIPT>let a = 1 < 2;</SCRIPT>
<script src=app.js></script>
<script type="module" src="./m.js"/></script>
<script nonce="mine">b()</script>`
	page, err := lint.HTML("index.html", []byte(src), opts(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `<!doctype html><meta name="viewport" content="width=device-width">
<SCRIPT` + nonce + `>let a = 1 < 2;</SCRIPT>
<script` + nonce + ` src=app.js></script>
<script` + nonce + ` type="module" src="./m.js"/></script>
<script` + nonce + ` nonce="mine">b()</script>`
	if string(page.Text) != want {
		t.Fatalf("the page reads\n%s\nwant\n%s", page.Text, want)
	}
	if !page.Policy.Nonced {
		t.Fatal("the policy does not say the page is nonced")
	}
	if !warned(page, lint.CodeAuthorNonce) {
		t.Fatal("an author nonce raised no warning")
	}
}

// A PAGE WITH NOTHING TO REWRITE IS ANSWERED BYTE FOR BYTE: the linter never
// reformats an author's page.
func TestAPageWithNothingToRewriteIsUnchanged(t *testing.T) {
	t.Parallel()
	src := "<!DOCTYPE html>\n<html><head><meta name=viewport content=x><title>T &amp; U</title>" +
		"<style>@media (prefers-color-scheme: dark) { body { color: #eee } }</style></head>" +
		"<body><p class='a'>Hi<!-- c --></p><textarea><script>x</script></textarea></body></html>\n"
	page, err := lint.HTML("index.html", []byte(src), opts(nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(page.Text) != src {
		t.Fatalf("an untouched page was rewritten:\n%s", page.Text)
	}
	if warned(page, lint.CodeNoViewport) {
		t.Fatal("a page with a viewport meta was warned for having none")
	}
}

// A SCRIPT FROM ANOTHER ORIGIN IS REFUSED unless it is an allowlisted CDN file
// at an exact version, whatever spelling a browser would resolve to another
// origin: protocol-relative, a backslash for a slash, surrounding spaces.
func TestARemoteScriptIsRefusedUnlessItCanBeCopiedIn(t *testing.T) {
	t.Parallel()
	for _, src := range []string{
		`<script src="https://evil.example/x.js"></script>`,
		`<script src="//evil.example/x.js"></script>`,
		`<script src="/\evil.example/x.js"></script>`,
		`<script src="  https://evil.example/x.js"></script>`,
		`<script src="data:text/javascript,alert(1)"></script>`,
		`<svg><script href="https://evil.example/x.js"></script></svg>`,
	} {
		_, err := lint.HTML("index.html", []byte(src), opts(&resolver{}))
		if got := codes(t, err); !slices.Contains(got, lint.CodeRemoteScript) {
			t.Errorf("%s answered %v, want remote_script", src, got)
		}
	}
}

// AN ALLOWLISTED FILE IS COPIED IN AND THE PAGE POINTS AT THE COPY, with the
// author's integrity handed to the copy and the rest of the tag kept.
func TestAnAllowlistedScriptIsRewrittenToTheVersionsCopy(t *testing.T) {
	t.Parallel()
	r := &resolver{}
	src := `<script src='https://cdn.jsdelivr.net/npm/chart.js@4.4.1/dist/chart.umd.min.js' ` +
		`integrity="sha384-abc" crossorigin="anonymous"></script>`
	page, err := lint.HTML("pages/index.html", []byte(src), opts(r))
	if err != nil {
		t.Fatal(err)
	}
	want := `<script` + nonce + ` src="../_vendor/cdn.jsdelivr.net/npm/chart.js@4.4.1/dist/` +
		`chart.umd.min.js" integrity="sha384-abc" crossorigin="anonymous"></script>`
	if string(page.Text) != want {
		t.Fatalf("the page reads\n%s\nwant\n%s", page.Text, want)
	}
	if len(r.asked) != 1 || r.asked[0].Integrity != "sha384-abc" || r.asked[0].Kind != lint.RefScript {
		t.Fatalf("the resolver was asked %+v", r.asked)
	}
}

// A PACKAGE PATH WITHOUT AN EXACT VERSION IS REFUSED, since the bytes behind a
// range or a tag change under the page.
func TestAnUnpinnedCDNReferenceIsRefused(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{
		"https://cdn.jsdelivr.net/npm/chart.js",
		"https://cdn.jsdelivr.net/npm/chart.js@4",
		"https://unpkg.com/react@^18.0.0/umd/react.production.min.js",
		"https://unpkg.com/@scope/pkg@latest/index.js",
		"https://cdn.tailwindcss.com",
		"https://code.jquery.com/jquery-latest.min.js",
	} {
		_, err := lint.HTML("index.html", []byte(`<script src="`+ref+`"></script>`), opts(&resolver{}))
		if got := codes(t, err); !slices.Contains(got, lint.CodeUnpinned) {
			t.Errorf("%s answered %v, want unpinned_version", ref, got)
		}
	}
	for _, ref := range []string{
		"https://unpkg.com/@scope/pkg@1.2.3-beta.1/index.js",
		"https://cdn.tailwindcss.com/3.4.17",
		"https://code.jquery.com/jquery-3.7.1.min.js",
		"https://cdnjs.cloudflare.com/ajax/libs/d3/7.8.5/d3.min.js",
	} {
		if _, err := lint.HTML("index.html", []byte(`<script src="`+ref+`"></script>`), opts(&resolver{})); err != nil {
			t.Errorf("%s was refused: %v", ref, err)
		}
	}
}

// AN ENGINE THAT COPIES NOTHING IN REFUSES AN ALLOWLISTED REFERENCE, and a copy
// that fails refuses the publish: the page would otherwise depend on a CDN at
// view time.
func TestAnAllowlistedReferenceNeedsACopy(t *testing.T) {
	t.Parallel()
	src := `<script src="https://cdn.jsdelivr.net/npm/a@1.0.0/a.js"></script>`
	if got := codes(t, func() error { _, err := lint.HTML("i.html", []byte(src), opts(nil)); return err }()); !slices.Contains(got, lint.CodeVendorOff) {
		t.Fatalf("vendoring off answered %v", got)
	}
	_, err := lint.HTML("i.html", []byte(src), opts(&resolver{fail: errors.New("integrity mismatch")}))
	if got := codes(t, err); !slices.Contains(got, lint.CodeVendorFailed) {
		t.Fatalf("a failed copy answered %v", got)
	}
}

// INLINE HANDLERS ARE ADMITTED BY THEIR DIGEST AND NOTHING WIDER: the policy
// names the SHA-256 of each handler's decoded value, once.
func TestInlineHandlersAreHashed(t *testing.T) {
	t.Parallel()
	src := `<button onclick="go(&quot;a&quot;)">a</button><a onmouseover='go("a")'>b</a>` +
		`<div onload="x()"></div>`
	page, err := lint.HTML("index.html", []byte(src), opts(nil))
	if err != nil {
		t.Fatal(err)
	}
	sum := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return base64.StdEncoding.EncodeToString(h[:])
	}
	want := []string{sum(`go("a")`), sum("x()")}
	slices.Sort(want)
	if !slices.Equal(page.Policy.HandlerHashes, want) {
		t.Fatalf("handler hashes %v, want %v", page.Policy.HandlerHashes, want)
	}
}

// THE FORBIDDEN SHAPES ARE REFUSED: a form posting elsewhere, a refresh to
// another origin, a javascript: URL, the placeholder in the source, a
// stylesheet from a host off the allowlist.
func TestTheForbiddenShapesAreRefused(t *testing.T) {
	t.Parallel()
	cases := map[string]lint.Code{
		`<form action="https://collect.example/x"></form>`:                  lint.CodeFormOffOrigin,
		`<button formaction="//collect.example/x">go</button>`:              lint.CodeFormOffOrigin,
		`<meta http-equiv="refresh" content="0; url=https://evil.example">`: lint.CodeRefreshOffOrigin,
		`<a href=" java\tscript:alert(1)">x</a>`:                            lint.CodeJavaScriptURL,
		`<p>` + artifacts.NoncePlaceholder + `</p>`:                         lint.CodePlaceholder,
		`<link rel="stylesheet" href="https://evil.example/a.css">`:         lint.CodeRemoteStylesheet,
		`<style>@import url("https://evil.example/a.css");</style>`:         lint.CodeRemoteStylesheet,
	}
	for src, want := range cases {
		src = strings.ReplaceAll(src, `\t`, "\t")
		_, err := lint.HTML("index.html", []byte(src), opts(&resolver{}))
		if got := codes(t, err); !slices.Contains(got, want) {
			t.Errorf("%s answered %v, want %s", src, got, want)
		}
	}
	if _, err := lint.HTML("index.html", []byte(`<form action="./submit"></form><a href="#x">x</a>`),
		opts(nil)); err != nil {
		t.Fatalf("an on-origin form and a fragment link were refused: %v", err)
	}
}

// EVERY REFUSAL IS REPORTED AT ONCE, with the line it is on, so an author fixes
// them all in one pass.
func TestEveryRefusalIsReportedWithItsLine(t *testing.T) {
	t.Parallel()
	src := "<p>\n<script src=\"https://evil.example/a.js\"></script>\n\n<form action=\"https://x.example\"></form>"
	_, err := lint.HTML("index.html", []byte(src), opts(&resolver{}))
	var refusal *lint.Refusal
	if !errors.As(err, &refusal) || len(refusal.Findings) != 2 {
		t.Fatalf("answered %v, want two findings", err)
	}
	if refusal.Findings[0].Line != 2 || refusal.Findings[1].Line != 4 {
		t.Fatalf("findings at lines %d and %d, want 2 and 4", refusal.Findings[0].Line, refusal.Findings[1].Line)
	}
}

// A SCRIPT THAT IMPORTS ANOTHER ORIGIN BY A LITERAL IS REFUSED, statically or
// dynamically, and an import in a comment or a string is no import.
func TestARemoteImportIsRefused(t *testing.T) {
	t.Parallel()
	for _, src := range []string{
		`<script type="module">import React from "https://esm.sh/react@18.2.0";</script>`,
		`<script type="module">export * from 'https://esm.sh/x';</script>`,
		`<script>const m = await import("https://esm.sh/x");</script>`,
	} {
		_, err := lint.HTML("index.html", []byte(src), opts(&resolver{}))
		if got := codes(t, err); !slices.Contains(got, lint.CodeRemoteImport) {
			t.Errorf("%s answered %v, want remote_import", src, got)
		}
	}
	ok := "<script type=\"module\">// import x from \"https://a.example\"\n" +
		"const s = 'import(\"https://b.example\")'; const r = /import \"https:\\/\\/c\"/;\n" +
		"import y from './local.js'; const t = `${import.meta.url}`;</script>"
	if _, err := lint.HTML("index.html", []byte(ok), opts(nil)); err != nil {
		t.Fatalf("imports in comments, strings and regular expressions were read: %v", err)
	}
	if _, err := lint.HTML("index.html", []byte(`<script type="application/json">{"u":"import('https://x')"}</script>`), opts(nil)); err != nil {
		t.Fatalf("a data block was read as script: %v", err)
	}
}

// AN IN-BROWSER TRANSFORM NEEDS STRING EVALUATION, and the policy says so: a
// text/babel script, or a copied compiler.
func TestAnInBrowserTransformAsksForEval(t *testing.T) {
	t.Parallel()
	page, err := lint.HTML("index.html", []byte(`<script type="text/babel">const a = <b/>;</script>`), opts(nil))
	if err != nil || !page.Policy.UnsafeEval {
		t.Fatalf("a text/babel page: eval %v, %v", page.Policy.UnsafeEval, err)
	}
	page, err = lint.HTML("index.html", []byte(`<script src="https://unpkg.com/@babel/standalone@7.24.0/babel.min.js"></script>`), opts(&resolver{}))
	if err != nil || !page.Policy.UnsafeEval {
		t.Fatalf("a page loading the babel compiler: eval %v, %v", page.Policy.UnsafeEval, err)
	}
	page, err = lint.HTML("index.html", []byte(`<script src="app.js"></script>`), opts(nil))
	if err != nil || page.Policy.UnsafeEval || page.Policy.WasmEval {
		t.Fatalf("a plain page asks for eval %v / wasm %v (%v)", page.Policy.UnsafeEval, page.Policy.WasmEval, err)
	}
	page, err = lint.HTML("index.html", []byte(`<script>WebAssembly.instantiateStreaming(fetch("a.wasm"))</script>`), opts(nil))
	if err != nil || !page.Policy.WasmEval {
		t.Fatalf("a page compiling WebAssembly: wasm %v, %v", page.Policy.WasmEval, err)
	}
}

// THE WARNINGS ARE ANSWERED ON SUCCESS: browser storage, a missing viewport,
// a resource the policy will not load, a password field.
func TestWarningsRideOnSuccess(t *testing.T) {
	t.Parallel()
	src := `<script>localStorage.setItem("a", 1)</script><img src="https://images.example/a.png">` +
		`<input type="password">`
	page, err := lint.HTML("index.html", []byte(src), opts(nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []lint.Code{lint.CodeBrowserStorage, lint.CodeNoViewport,
		lint.CodeRemoteBlocked, lint.CodePasswordField} {
		if !warned(page, code) {
			t.Errorf("no %s warning in %v", code, page.Warnings)
		}
	}
}

// A STYLE ELEMENT'S ALLOWLISTED REFERENCES ARE COPIED IN TOO, at the page's
// lines.
func TestAStyleElementsReferencesAreCopiedIn(t *testing.T) {
	t.Parallel()
	r := &resolver{}
	src := "<style>\n@import url(https://fonts.googleapis.com/css2?family=Inter);\n" +
		"b { background: url('https://images.example/x.png') }</style>"
	page, err := lint.HTML("index.html", []byte(src), opts(r))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page.Text), `@import url("_vendor/fonts.googleapis.com/css2");`) {
		t.Fatalf("the import was not rewritten:\n%s", page.Text)
	}
	if !warned(page, lint.CodeRemoteBlocked) || r.asked[0].Line != 2 {
		t.Fatalf("warnings %v, asked %+v", page.Warnings, r.asked)
	}
}

// SOURCE IS THE AUTHOR'S TEXT, AND THE STORED FORM LINTS BACK TO ITSELF: an
// edit reads the author's page, and a carried page's policy is the one its
// publish derived.
func TestTheStoredFormRoundTrips(t *testing.T) {
	t.Parallel()
	src := `<meta name=viewport><script src="https://cdn.jsdelivr.net/npm/a@1.0.0/a.js"></script>` +
		`<button onclick="go()">x</button><script nonce=x>y()</script>`
	page, err := lint.HTML("index.html", []byte(src), opts(&resolver{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(lint.Source(page.Text)), artifacts.NoncePlaceholder) {
		t.Fatal("the source form still carries a placeholder")
	}
	policy, err := lint.Stored("index.html", page.Text, opts(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(policy.HandlerHashes, page.Policy.HandlerHashes) || !policy.Nonced {
		t.Fatalf("the stored policy %+v differs from the published %+v", policy, page.Policy)
	}
	if _, err := lint.Stored("index.html", []byte(`<script>x()</script>`), opts(nil)); err == nil {
		t.Fatal("a page the linter never wrote was accepted as stored")
	}
}

// THE ALLOWLIST REFUSES WHAT IT DOES NOT NAME, and the kinds each host serves.
func TestTheAllowlistRefusesWhatItDoesNotName(t *testing.T) {
	t.Parallel()
	a := lint.DefaultAllowlist()
	check := func(raw string, k lint.RefKind) error {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return a.Check(u, k)
	}
	refused := []struct {
		url  string
		kind lint.RefKind
	}{
		{"http://cdn.jsdelivr.net/npm/a@1.0.0/a.js", lint.RefScript},
		{"https://cdn.jsdelivr.net/gh/user/repo@1.0.0/a.js", lint.RefScript},
		{"https://cdn.jsdelivr.net:8443/npm/a@1.0.0/a.js", lint.RefScript},
		{"https://fonts.googleapis.com/css2?family=A", lint.RefScript},
		{"https://cdn.tailwindcss.com/3.4.17", lint.RefStylesheet},
		{"https://cdn.jsdelivr.net.evil.example/npm/a@1.0.0/a.js", lint.RefScript},
	}
	for _, c := range refused {
		if err := check(c.url, c.kind); !errors.Is(err, lint.ErrNotAllowed) {
			t.Errorf("%s as %s answered %v, want a refusal", c.url, c.kind, err)
		}
	}
	if err := check("https://fonts.gstatic.com/s/inter/v13/a.woff2", lint.RefFont); err != nil {
		t.Errorf("a Google font was refused: %v", err)
	}
}

// A COPIED STYLESHEET'S RELATIVE REFERENCES ARE THE CDN'S FILES and are copied
// in; an author's are the artifact's and are left as written.
func TestACopiedStylesheetCopiesWhatItNames(t *testing.T) {
	t.Parallel()
	r := &resolver{}
	base, _ := url.Parse("https://cdnjs.cloudflare.com/ajax/libs/font-awesome/6.5.1/css/all.min.css")
	css := []byte(`@font-face{src:url(../webfonts/fa-solid-900.woff2) format("woff2")}` +
		`a{background:url("data:image/png;base64,AA")}`)
	out, _, err := lint.CSS("_vendor/cdnjs.cloudflare.com/ajax/libs/font-awesome/6.5.1/css/all.min.css", css, opts(r), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.asked) != 1 || r.asked[0].URL.Path != "/ajax/libs/font-awesome/6.5.1/webfonts/fa-solid-900.woff2" ||
		r.asked[0].Kind != lint.RefFont {
		t.Fatalf("the resolver was asked %+v", r.asked)
	}
	if strings.Contains(string(out), "../webfonts") || !strings.Contains(string(out), "data:image/png") {
		t.Fatalf("the stylesheet reads %s", out)
	}
	author := []byte(`a{background:url(img/a.png)}`)
	same, _, err := lint.CSS("style.css", author, opts(r), nil)
	if err != nil || string(same) != string(author) {
		t.Fatalf("an author's relative reference was rewritten: %s, %v", same, err)
	}
}

// LOCAL REFERENCES ARE RESOLVED AGAINST THE FILE AND NEVER LEAVE THE ROOT.
func TestLocalRefsResolveInsideTheVersion(t *testing.T) {
	t.Parallel()
	got := lint.LocalRefs("pages/index.html", []byte(`<script src="../_vendor/a.js"></script>`+
		`<link rel=stylesheet href="s.css?v=1"><script src="https://x.example/a.js"></script>`+
		`<script src="../../escape.js"></script>`), true)
	want := []string{"_vendor/a.js", "pages/s.css"}
	if !slices.Equal(got, want) {
		t.Fatalf("LocalRefs = %v, want %v", got, want)
	}
}

// THE SEARCH TEXT IS WHAT A READER READS, bounded at a character boundary.
func TestTextIsWhatAReaderReads(t *testing.T) {
	t.Parallel()
	got := lint.Text([]byte(`<title>Plan</title><script>var hidden</script><p>Q4   roadmap <b>é</b></p>`), 100)
	if got != "Plan Q4 roadmap é" {
		t.Fatalf("Text = %q", got)
	}
	if cut := lint.Text([]byte("<p>abcé</p>"), 4); cut != "abc" {
		t.Fatalf("a cut through a character answered %q", cut)
	}
}

// THE VERIFICATION HOLDS EVERY SCRIPT OF THE ANSWER, whatever the rewrite
// believed: a script without the placeholder as its nonce, a script still
// pointing at another origin, a stray placeholder or a miscount is refused.
func TestTheVerificationRefusesAPageItCannotAccountFor(t *testing.T) {
	t.Parallel()
	good := `<script` + nonce + ` src="a.js"></script>`
	if err := lint.Verify([]byte(good), 1); err != nil {
		t.Fatalf("a verified page was refused: %v", err)
	}
	for name, page := range map[string]string{
		"no placeholder":       `<script src="a.js"></script>` + `<p>` + artifacts.NoncePlaceholder + `</p>`,
		"a remote source":      `<script` + nonce + ` src="https://evil.example/a.js"></script>`,
		"a nonce read first":   `<script nonce="other"` + nonce + `></script>`,
		"a script not counted": good + `<script` + nonce + `></script>`,
	} {
		if err := lint.Verify([]byte(page), 1); err == nil {
			t.Errorf("a page with %s was verified", name)
		}
	}
}

// THE IMPORT SCAN READS THE LEXER'S TOKENS: a division is not a regular
// expression, a template's substitution is code, and every literal form of an
// import is found with its line.
func TestImportsFindsEveryLiteralSpecifier(t *testing.T) {
	t.Parallel()
	src := "const a = b / 2; const c = d / 3; // import 'no'\n" +
		"import x, { y as z } from \"./one.js\";\n" +
		"const t = `${ '\"' + import(\"./two.js\") }`;\n" +
		"export { q } from './three.js'; export const r = 1;\n" +
		"obj.import('nope'); const re = /\"import('no')\"/g; import './four.js'"
	got := lint.Imports([]byte(src))
	var specs []string
	for _, im := range got {
		specs = append(specs, im.Specifier)
	}
	want := []string{"./one.js", "./two.js", "./three.js", "./four.js"}
	if !slices.Equal(specs, want) {
		t.Fatalf("Imports = %v, want %v", specs, want)
	}
	if got[0].Line != 2 || got[3].Line != 5 {
		t.Fatalf("lines %d and %d, want 2 and 5", got[0].Line, got[3].Line)
	}
}
