package api_test

// THE CRAWL — what a browser fetches after the server hands it the shell.
//
// A browser does not stop at the files the shell names. The entry module
// imports chunks, a lazy screen is an `import()` of another, and beside each of
// those Vite writes a PRELOAD LIST (`__vite__mapDeps`) naming everything the
// lazy chunk needs, its own stylesheet included, which the browser fetches
// before it runs the chunk. A stylesheet asks for its faces. Each of those is a
// request that can answer 404 or the wrong type, and each failure is invisible
// until a reader opens the one screen that asks: a lazy chunk the embed does
// not carry leaves every other screen working.
//
// So the crawl follows every way the build writes a reference, and it does it
// against the SERVER, never the disk: the tree on disk is not the tree the
// binary embeds, and a type the server gets wrong is a failure the disk cannot
// show. Its own behaviour is certified below over a stand-in bundle shaped like
// Rolldown's real output, because the committed dashboard has no lazy chunk
// for it to be wrong about yet — a crawler that never followed an `import()`
// would pass over this build exactly as one that did.

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/static"
)

// dashboardBase is `base` in dashboard/vite.config.ts: the path every
// reference the build writes is rooted at.
//
// The preload list is written RELATIVE TO IT — `assets/Spend-1UNPEVFb.js`,
// with no `./` and no leading slash — and Vite's preload helper prepends it in
// the browser (`function(e){return"/static/dashboard/"+e}`). An entry
// resolved against the module holding the list would look one `assets/` too
// deep and report every lazy chunk missing.
const dashboardBase = "/static/dashboard/"

// servedAs is the type each kind of file the shell can reach must arrive
// with. A prefix, so a charset parameter passes; a browser refuses a module
// whose type is not JavaScript and a stylesheet whose type is not CSS.
var servedAs = map[string]string{
	".js":    "text/javascript",
	".css":   "text/css",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
}

// route is how one file came to be asked for. It is what a failure names,
// because "404" alone sends a reader to the wrong file: the fix for a missing
// chunk is in whatever still asks for it.
type route string

const (
	fromShell   route = "a src or href in the shell"
	fromImport  route = "a static import"
	fromDynamic route = "a dynamic import()"
	fromPreload route = "the preload list (__vite__mapDeps)"
	fromSheet   route = "a stylesheet's url()"
	fromLiteral route = "a /static/ path in a module"
)

// The references a built module carries, in the forms Rolldown writes them.
var (
	// staticImport is `import{a as b}from"./x.js"`, `import"./x.js"`,
	// `export{a}from"./x.js"` and `export*from"./x.js"`. A static specifier
	// is a string literal by the language's grammar, never a template, so a
	// backtick is not read. Only a RELATIVE specifier is: that is the only
	// kind the build writes between its own chunks, and it is what keeps
	// prose out — the entry is full of template literals that end in "from "
	// and close on the very next character.
	staticImport = regexp.MustCompile(`\b(?:from|import)\s*["'](\.\.?/[^"'\s]+)["']`)

	// dynamicImport is a lazy chunk: `import("./x.js")`, in any of the three
	// quotes, since Rolldown's minifier writes the specifier as a template
	// literal, backquoted. `$`, `{` and `}` are excluded so a template
	// with a substitution — a computed specifier, which names no one file —
	// is not read as a literal.
	dynamicImport = regexp.MustCompile("\\bimport\\s*\\(\\s*[\"'`](\\.\\.?/[^\"'`\\s${}]+)[\"'`]\\s*[,)]")

	// preloadList is the array Vite's `__vite__mapDeps` is declared over:
	// `__vite__mapDeps=(i,m=__vite__mapDeps,d=(m.f||(m.f=["assets/a.js",…])))`.
	// It is the ONLY place a lazy chunk's stylesheet is named — the chunk
	// itself carries no reference to it — so a crawl that read the imports
	// alone would never ask for one. Anchored on the declaration's `=`, which
	// a use of the helper (`__vite__mapDeps([0,1])`) does not have.
	preloadList = regexp.MustCompile(`__vite__mapDeps\s*=[^\[]*\[([^\]]*)\]`)

	// listEntry is one quoted path inside that array.
	listEntry = regexp.MustCompile("[\"'`]([^\"'`]+)[\"'`]")
)

// reachedFile is one file the crawl fetched, and how.
type reachedFile struct {
	url  string
	by   string // the file that named it
	how  route
	body []byte
}

// crawled is every file the shell reached, in the order a browser working
// breadth-first would have asked for them, and every way that browser would
// have come away without something it was told to load.
type crawled struct {
	files []reachedFile
	// named counts what the shell itself named under /static/, reached or
	// not, so a floor can tell a shell that names nothing from a shell whose
	// names all failed.
	named    int
	problems []string
}

// reached reports whether the crawl fetched this path.
func (c crawled) reached(p string) bool {
	return slices.ContainsFunc(c.files, func(f reachedFile) bool { return f.url == p })
}

// ofKind is every file reached with this extension.
func (c crawled) ofKind(ext string) []reachedFile {
	var out []reachedFile
	for _, f := range c.files {
		if strings.EqualFold(path.Ext(f.url), ext) {
			out = append(out, f)
		}
	}
	return out
}

// crawlDashboard fetches the shell from the server and then everything it
// reaches, the way a browser would, and reports every failure rather than the
// first: a build that lost three chunks should say so once.
func crawlDashboard(t *testing.T, a *api.App) crawled {
	t.Helper()
	var c crawled

	res := fetch(t, a, "/dashboard", nil)
	shell, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil || res.StatusCode != http.StatusOK {
		c.problems = append(c.problems, fmt.Sprintf("/dashboard answered %d (%v), so there is no shell to follow",
			res.StatusCode, err))
		return c
	}
	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		c.problems = append(c.problems, fmt.Sprintf("/dashboard served as %q, want text/html — a browser "+
			"shows the markup as text rather than loading anything it names", got))
	}

	type ask struct {
		url, by string
		how     route
	}
	var queue []ask
	for _, m := range staticRef.FindAllStringSubmatch(string(shell), -1) {
		queue = append(queue, ask{m[1], "/dashboard", fromShell})
		c.named++
	}

	seen := map[string]bool{}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if seen[next.url] {
			continue
		}
		seen[next.url] = true
		where := fmt.Sprintf("%s, named by %s through %s", next.url, next.by, next.how)

		ext := strings.ToLower(path.Ext(next.url))
		want, known := servedAs[ext]
		if !known {
			c.problems = append(c.problems, fmt.Sprintf("%s: a kind this crawl does not know how to "+
				"check; teach servedAs its type rather than dropping the file", where))
			continue
		}
		res := fetch(t, a, next.url, nil)
		body, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		switch {
		case err != nil:
			c.problems = append(c.problems, fmt.Sprintf("%s: reading the answer: %v", where, err))
			continue
		case res.StatusCode != http.StatusOK:
			c.problems = append(c.problems, fmt.Sprintf("%s: answered %d — a browser asks for it and "+
				"the page loses whatever it holds, the whole screen when it is a chunk", where, res.StatusCode))
			continue
		case !strings.HasPrefix(res.Header.Get("Content-Type"), want):
			c.problems = append(c.problems, fmt.Sprintf("%s: served as %q, want %s — a browser refuses "+
				"the wrong type and the failure names the MIME rather than the file",
				where, res.Header.Get("Content-Type"), want))
		case len(body) == 0:
			c.problems = append(c.problems, fmt.Sprintf("%s: is empty", where))
		}
		c.files = append(c.files, reachedFile{url: next.url, by: next.by, how: next.how, body: body})

		follow := func(ref string, from string, how route) {
			target, err := resolve(from, ref)
			switch {
			case err != nil:
				c.problems = append(c.problems, fmt.Sprintf("%s names %q through %s, which is not a URL: %v",
					next.url, ref, how, err))
			case target != "":
				queue = append(queue, ask{target, next.url, how})
			}
		}
		switch ext {
		case ".js":
			text := string(body)
			for _, m := range staticImport.FindAllStringSubmatch(text, -1) {
				follow(m[1], next.url, fromImport)
			}
			for _, m := range dynamicImport.FindAllStringSubmatch(text, -1) {
				follow(m[1], next.url, fromDynamic)
			}
			for _, list := range preloadList.FindAllStringSubmatch(text, -1) {
				for _, m := range listEntry.FindAllStringSubmatch(list[1], -1) {
					follow(m[1], dashboardBase, fromPreload)
				}
			}
			// An asset a module imports — an image, a worker — arrives as
			// the absolute path the build rewrote it to.
			for _, m := range staticRef.FindAllStringSubmatch(text, -1) {
				follow(m[1], next.url, fromLiteral)
			}
		case ".css":
			for _, m := range cssURL.FindAllStringSubmatch(string(body), -1) {
				ref := strings.Trim(m[1], `"' `)
				// Inline data and a fragment naming something in this
				// document are not requests.
				if strings.HasPrefix(ref, "data:") || strings.HasPrefix(ref, "#") {
					continue
				}
				follow(ref, next.url, fromSheet)
			}
		}
	}
	return c
}

// resolve is ref as a browser resolves it against the file that names it:
// the path on this origin it then asks for, or "" for another origin, which
// TestTheShellFitsTheDashboardPolicy is about rather than this.
func resolve(from, ref string) (string, error) {
	base, err := url.Parse(from)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	u := base.ResolveReference(r)
	if u.Scheme != "" || u.Host != "" {
		return "", nil
	}
	return u.Path, nil
}

// crawlFixture is a stand-in bundle with every reference form the crawl reads,
// written the way Rolldown writes them: the entry and chunk text below is cut
// from a real build of this dashboard with two screens made lazy, renamed and
// shortened but not reshaped.
//
// And two forms it must NOT follow, because following either asks for a file
// that does not exist: the preload helper's base, a template literal holding
// /static/dashboard/ that a name is joined onto, and prose that happens to
// end in "from " right before a template literal closes, which the real entry
// is full of.
func crawlFixture() fstest.MapFS {
	return fstest.MapFS{
		"dashboard/index.html": {Data: []byte(`<!doctype html>
<link rel="icon" type="image/svg+xml" href="/static/crewlet-icon.svg" />
<script type="module" crossorigin src="/static/dashboard/assets/index-Bmgzty1J.js"></script>
<link rel="modulepreload" crossorigin href="/static/dashboard/assets/rolldown-runtime-hePW80VL.js">
<link rel="modulepreload" crossorigin href="/static/dashboard/assets/react-wiHys0m2.js">
<link rel="stylesheet" crossorigin href="/static/dashboard/assets/index-BS_xpF9S.css">
<div id="root"></div>`)},
		"crewlet-icon.svg": {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},

		// The entry: two static imports, the preload helper and its list, two
		// lazy screens, an imported asset, and one of the sentences ending in
		// "from " that the real entry carries dozens of.
		"dashboard/assets/index-Bmgzty1J.js": {Data: []byte(
			`import{n as e,r as t}from"./rolldown-runtime-hePW80VL.js";` +
				`import{i as n,n as r,r as i,t as a}from"./react-wiHys0m2.js";` +
				"var h=(0,m.jsx)(`p`,{children:[`Copied from `,(0,m.jsx)(`code`,{children:o})]});" +
				"var VL=`modulepreload`,HL=function(e){return`/static/dashboard/`+e},UL={};" +
				`const __vite__mapDeps=(i,m=__vite__mapDeps,d=(m.f||(m.f=["assets/Spend-1UNPEVFb.js",` +
				`"assets/rolldown-runtime-hePW80VL.js","assets/react-wiHys0m2.js","assets/Budgets-OCFe1hZV.js",` +
				`"assets/Budgets-DEpC4reb.css"])))=>i.map(i=>d[i]);` +
				"GL=(0,o.lazy)(()=>WL(()=>import(`./Spend-1UNPEVFb.js`).then(e=>({default:e.Spend})),__vite__mapDeps([0,1,2])))," +
				"KL=(0,o.lazy)(()=>WL(()=>import(`./Budgets-OCFe1hZV.js`).then(e=>({default:e.Budgets})),__vite__mapDeps([3,1,2,4])))," +
				"qL=(0,m.jsx)(`img`,{src:`/static/dashboard/assets/brand-Kj12Hg34.svg`,alt:`Crewlet`});",
		)},
		"dashboard/assets/rolldown-runtime-hePW80VL.js": {Data: []byte(
			`var e=Object.create,t=Object.defineProperty;export{e as n,t as r};`,
		)},
		"dashboard/assets/react-wiHys0m2.js": {Data: []byte(
			`import{t as e}from"./rolldown-runtime-hePW80VL.js";var t=Symbol.for("react.element");export{t as i};`,
		)},
		"dashboard/assets/index-BS_xpF9S.css": {Data: []byte(
			`@font-face{font-family:Inter;src:url(/static/dashboard/fonts/inter-latin.woff2)format("woff2")}`,
		)},
		"dashboard/assets/brand-Kj12Hg34.svg": {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		"dashboard/fonts/inter-latin.woff2":   {Data: []byte("wOF2 inter")},

		// A lazy screen that imports back into the entry, as every real one
		// does, and has a lazy chunk of its own in single quotes.
		"dashboard/assets/Spend-1UNPEVFb.js": {Data: []byte(
			`import{r as e}from"./rolldown-runtime-hePW80VL.js";import{i as t}from"./react-wiHys0m2.js";` +
				`import{A as r}from"./index-Bmgzty1J.js";var s=()=>import('./Chart-Ab12Cd34.js');export{s as Spend};`,
		)},
		// A bare side-effect import, then a re-export.
		"dashboard/assets/Chart-Ab12Cd34.js":  {Data: []byte(`import"./shared-Qw12Er34.js";export const Chart=1;`)},
		"dashboard/assets/shared-Qw12Er34.js": {Data: []byte(`export*from"./leaf-Zx12Cv34.js";`)},
		"dashboard/assets/leaf-Zx12Cv34.js":   {Data: []byte(`export const leaf=1;`)},

		// The other lazy screen, whose stylesheet only the preload list names,
		// and which asks for a face RELATIVE to itself, plus two url()s that
		// are not requests at all.
		"dashboard/assets/Budgets-OCFe1hZV.js": {Data: []byte(
			`import{i as t}from"./react-wiHys0m2.js";export const Budgets=()=>t;`,
		)},
		"dashboard/assets/Budgets-DEpC4reb.css": {Data: []byte(
			`.meter{background:url(data:image/png;base64,AAAA);filter:url(#glow)}` +
				`@font-face{font-family:Mono;src:url("../fonts/mono-latin.woff2")}`,
		)},
		"dashboard/fonts/mono-latin.woff2": {Data: []byte("wOF2 mono")},
	}
}

// TestTheCrawlFollowsEveryReferenceTheBuildWrites certifies the crawler itself:
// over a bundle shaped like Rolldown's output, it reaches exactly the files a
// browser would fetch — each by the route that first named it — and nothing
// else.
//
// EXACTLY, in both directions. A file it misses is a reference form it cannot
// read, which over the real build would be a lazy chunk it silently never
// checked; a file it adds is a string it mistook for a reference, which would
// fail the real build over a request no browser makes.
func TestTheCrawlFollowsEveryReferenceTheBuildWrites(t *testing.T) {
	t.Parallel()
	c := crawlDashboard(t, newApp(t, api.Options{Assets: crawlFixture()}))
	for _, p := range c.problems {
		t.Error(p)
	}

	const assets = dashboardBase + "assets/"
	want := map[string]route{
		"/static/crewlet-icon.svg":                fromShell,
		assets + "index-Bmgzty1J.js":              fromShell,
		assets + "rolldown-runtime-hePW80VL.js":   fromShell,
		assets + "react-wiHys0m2.js":              fromShell,
		assets + "index-BS_xpF9S.css":             fromShell,
		dashboardBase + "fonts/inter-latin.woff2": fromSheet,
		assets + "Spend-1UNPEVFb.js":              fromDynamic,
		assets + "Budgets-OCFe1hZV.js":            fromDynamic,
		assets + "Budgets-DEpC4reb.css":           fromPreload,
		assets + "brand-Kj12Hg34.svg":             fromLiteral,
		assets + "Chart-Ab12Cd34.js":              fromDynamic,
		assets + "shared-Qw12Er34.js":             fromImport,
		assets + "leaf-Zx12Cv34.js":               fromImport,
		dashboardBase + "fonts/mono-latin.woff2":  fromSheet,
	}
	got := map[string]route{}
	for _, f := range c.files {
		got[f.url] = f.how
	}
	for u, how := range want {
		switch g, ok := got[u]; {
		case !ok:
			t.Errorf("the crawl never reached %s, which a browser asks for through %s", u, how)
		case g != how:
			t.Errorf("%s was reached through %s, want %s", u, g, how)
		}
	}
	for u, how := range got {
		if _, ok := want[u]; !ok {
			t.Errorf("the crawl asked for %s through %s, which no browser would", u, how)
		}
	}
	if c.named != 5 {
		t.Errorf("the shell named %d files under /static/, want 5", c.named)
	}
}

// TestTheCrawlNamesEveryFileABrowserWouldMiss is the crawler's red half: take
// one file out of a bundle that crawls clean, and it must name that file AND
// the file still asking for it, since that is where the fix is.
//
// Each case removes a file reachable ONLY through the form it names, so the
// case fails if the crawl stopped reading that form — a crawler that never
// followed `import()` reports nothing when a lazy chunk goes missing, which is
// exactly the defect a crawl that reads the shell and its stylesheets alone
// had.
func TestTheCrawlNamesEveryFileABrowserWouldMiss(t *testing.T) {
	t.Parallel()
	const assets = dashboardBase + "assets/"
	for _, tc := range []struct {
		name    string
		missing string // the path, under the embedded tree
		by      string // the file that must be named as still asking for it
	}{
		{"a lazy chunk", "dashboard/assets/Budgets-OCFe1hZV.js", assets + "index-Bmgzty1J.js"},
		{"a lazy chunk's stylesheet", "dashboard/assets/Budgets-DEpC4reb.css", assets + "index-Bmgzty1J.js"},
		{"a lazy chunk of a lazy chunk", "dashboard/assets/Chart-Ab12Cd34.js", assets + "Spend-1UNPEVFb.js"},
		{"a side-effect import", "dashboard/assets/shared-Qw12Er34.js", assets + "Chart-Ab12Cd34.js"},
		{"a re-export", "dashboard/assets/leaf-Zx12Cv34.js", assets + "shared-Qw12Er34.js"},
		{"an asset a module imports", "dashboard/assets/brand-Kj12Hg34.svg", assets + "index-Bmgzty1J.js"},
		{"a face a lazy stylesheet asks for", "dashboard/fonts/mono-latin.woff2", assets + "Budgets-DEpC4reb.css"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree := crawlFixture()
			delete(tree, tc.missing)
			c := crawlDashboard(t, newApp(t, api.Options{Assets: tree}))

			missing := "/static/" + tc.missing
			var named []string
			for _, p := range c.problems {
				if strings.HasPrefix(p, missing+", named by "+tc.by) {
					named = append(named, p)
				}
			}
			if len(named) != 1 || len(c.problems) != 1 {
				t.Errorf("with %s gone the crawl reported %q; want exactly one problem naming it "+
					"and %s", missing, c.problems, tc.by)
			}
			if len(named) == 1 && !strings.Contains(named[0], "answered 404") {
				t.Errorf("the problem does not say what the browser got: %s", named[0])
			}
		})
	}
}

// TestEveryFileUnderAssetsIsReachedFromTheShell is the converse of the crawl,
// over the real build: every file the build wrote under assets/ must be one the
// shell reaches.
//
// Vite writes nothing there that the page does not load — the entry, its
// chunks, the lazy chunks, their stylesheets and the assets they import — so a
// file nothing reaches is one of two things, and both are worth a red build.
// Either the build has started naming a file in a form this crawl does not
// read, in which case every check above is passing over a chunk it never
// fetched; or the file is dead weight in every binary, which the embed and the
// drift gate both carry without complaint.
func TestEveryFileUnderAssetsIsReachedFromTheShell(t *testing.T) {
	t.Parallel()
	c := crawlDashboard(t, newApp(t, api.Options{}))
	for _, p := range c.problems {
		t.Error(p)
	}

	files := 0
	err := fs.WalkDir(static.FS(), "dashboard/assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		if !c.reached("/static/" + p) {
			t.Errorf("%s is embedded and nothing the shell reaches names it: teach the crawl the "+
				"form the build now names it in (dashboardcrawl_test.go), or stop the build "+
				"emitting a file the page never loads", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the embedded assets/: %v", err)
	}
	// A FLOOR, for the reason TestEveryFileUnderAssetsIsContentHashed gives:
	// an empty directory satisfies "every file" with nothing checked.
	if files < 3 {
		t.Errorf("the embedded assets/ holds %d files; the entry, the React chunk and the "+
			"stylesheet are three", files)
	}
}
