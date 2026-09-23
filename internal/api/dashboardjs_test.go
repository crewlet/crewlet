package api_test

// GATE G5 — the dashboard the binary embeds is a REAL, COMPLETE application.
//
// The dashboard is a React + TypeScript app built by Vite (see dashboard/) into
// static/dashboard, which package `static` embeds. The build output is
// COMMITTED, so `go build ./...` and `go install …@latest` work on a clean
// checkout with no Node on the machine — an embed directive cannot run a
// bundler. CI rebuilds it and diffs the tree, so a committed bundle that does
// not match its source is a red build (.github/workflows/ci.yml, the
// `dashboard` job).
//
// What THIS file asserts is the half a rebuild-and-diff cannot see: that the
// committed tree is a coherent application, and that every file a browser is
// sent to fetch — from the shell, through every static import, every lazy
// `import()` and the preload list Vite writes beside one, to the faces a
// stylesheet asks for — is reachable over HTTP from the server itself, with a
// content type a browser will accept. The crawl that follows them is in
// dashboardcrawl_test.go, certified there over a bundle with lazy chunks.
//
// Those are different failures. A tree can be perfectly in step with its source
// and still be unservable — an ES module served as text/plain is REFUSED by the
// module loader, and the page then fails with a MIME error rather than a
// missing file, which sends a reader looking for the wrong problem.
//
// And one rule the dashboard keeps is checked here on the artefact as well as
// on the source, because the artefact is what a browser runs: nothing it
// serves renders a price (TestTheDashboardRendersNoPrice, rule 19 in
// docs/reference/dashboard-design.md).
//
// The dashboard's own assertions (its protocol, its router, its ordering
// rules, and the measured contrast of every colour token in both themes) run
// under Vitest — `npm test` in dashboard/, wired into `make dashboard-test` and
// its own CI job. They are not driven from Go any more: they were, because
// there was no package.json and no runner, and driving a real test runner from
// a Go subprocess would be a second way to run one suite.

import (
	"bytes"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/static"
)

// Paths, relative to this package's directory, which is where `go test` runs.
const (
	// The directory package static embeds, on disk.
	staticDir = "../../static"
	// The built dashboard the binary serves.
	servedTree = staticDir + "/dashboard"
)

// TestTheBuiltDashboardIsWhole checks the committed tree is an application
// rather than a half-finished build.
//
// Every one of these is a real failure mode of a committed build artifact: a
// merge that took one side's index.html and the other's assets, a `git add`
// that missed the fonts directory, a build run with the protocol config
// skipped. Each leaves a tree that looks plausible in a diff and serves a blank
// page.
func TestTheBuiltDashboardIsWhole(t *testing.T) {
	t.Parallel()
	shell, err := os.ReadFile(filepath.Join(servedTree, "index.html"))
	if err != nil {
		t.Fatalf("no built shell at %s — run `make dashboard`: %v", servedTree, err)
	}

	// The shell must name an entry script and a stylesheet under assets/. A
	// build that emitted neither is a build whose output directory was not
	// cleaned, which leaves a tree that diffs plausibly and serves a blank
	// page.
	//
	// It does not claim the names are hashed: `[^"]+` matches `index` as
	// happily as `index-Cd0p1oTg`. That claim is
	// TestEveryFileUnderAssetsIsContentHashed's, and it matters now — every
	// file under assets/ is served `immutable` for a year, so a name that
	// does not change with its bytes would pin a stale module in every
	// reader's browser until they cleared it.
	entry := regexp.MustCompile(`src="(/static/dashboard/assets/[^"]+\.js)"`)
	sheet := regexp.MustCompile(`href="(/static/dashboard/assets/[^"]+\.css)"`)
	if !entry.Match(shell) {
		t.Errorf("the shell names no entry module under assets/:\n%s", shell)
	}
	// FATAL, not an error: the submatch below indexes this match, so a shell
	// that lost its stylesheet used to panic the whole api_test binary one
	// line after printing the message that explains it.
	if !sheet.Match(shell) {
		t.Fatalf("the shell names no stylesheet under assets/:\n%s", shell)
	}

	// The protocol bundle is a SEPARATE build target and is easy to forget:
	// internal/e2e replays a real company's socket frames through it under
	// plain node, and without it that gate silently has nothing to run.
	if _, err := os.Stat(filepath.Join(servedTree, "protocol.js")); err != nil {
		t.Errorf("no protocol.js — internal/e2e's client replay has nothing to "+
			"run against; `npm run build` in dashboard/ emits it: %v", err)
	}

	// The faces are embedded rather than fetched, which is what makes the
	// dashboard render identically on a closed network. A missing one falls
	// back silently to a system font.
	//
	// ASKED FOR AND PRESENT, rather than a list of names. The names used to be
	// written out here; they are the design system's now, arriving through
	// @crewlethq/tokens' own stylesheet, so a spelled list would be a second
	// copy of a name this repository does not choose. Reading them out of the
	// stylesheet is also the stronger claim: a hardcoded list cannot see a
	// face the CSS asks for and the build did not emit, which is the failure
	// that renders a fallback font with nothing missing from the tree.
	//
	// THE FACES ARE UNHASHED, UNDER `fonts/`, which is the opposite of what
	// this said — it claimed they were "emitted under `assets/` with a content
	// hash". They never were: dashboard/vite.config.ts routes every .woff2 to
	// `fonts/[name][extname]` and everything else to `assets/[name]-[hash]`,
	// and the claim was already false the day it was written (901ddeb1, when
	// the faces were plain files under dashboard/public/fonts/). Nothing
	// depended on the wrong half, because this reads whatever path the
	// stylesheet names — which is why it went unnoticed.
	sheetPath := string(sheet.FindSubmatch(shell)[1])
	css, err := os.ReadFile(filepath.Join(servedTree, strings.TrimPrefix(sheetPath, "/static/dashboard/")))
	if err != nil {
		t.Fatalf("the shell names %s and the tree does not have it: %v", sheetPath, err)
	}
	faces := regexp.MustCompile(`url\(([^)]*\.woff2)\)`).FindAllSubmatch(css, -1)
	// A FLOOR, because a stylesheet that asks for no face at all passes every
	// assertion below it — which is exactly what dropping the font import
	// would look like. Two families at two subsets each is four.
	if len(faces) < 4 {
		t.Errorf("the stylesheet asks for %d faces; two families at two subsets each is four", len(faces))
	}
	for _, face := range faces {
		ref := strings.Trim(string(face[1]), `"'`)
		rel := strings.TrimPrefix(ref, "/static/dashboard/")
		if _, err := os.Stat(filepath.Join(servedTree, rel)); err != nil {
			t.Errorf("the stylesheet asks for %s and the built tree has no such file: %v", ref, err)
		}
		// AND UNDER fonts/, which nothing asserted. The .woff2 exception in
		// dashboard/vite.config.ts exists so these keep a stable path: the
		// notice this tree publishes states it ("served from
		// /static/dashboard/fonts/"), and OFL.txt is filed beside them on the
		// strength of it. Drop the exception and Vite content-hashes every
		// face into assets/ — where the loop above would still find each one,
		// because it follows whatever path the stylesheet names, so the whole
		// arrangement would come apart with every test still green.
		if dir := path.Dir(rel); dir != "fonts" {
			t.Errorf("the stylesheet asks for %s, which is under %q — the faces "+
				"are pinned to fonts/ by the .woff2 branch in "+
				"dashboard/vite.config.ts, and THIRD_PARTY_NOTICES.txt "+
				"publishes that path beside the OFL notice", ref, dir)
		}
	}
	// The licence travels with the files it covers, and both are inside what
	// the binary embeds. `fonts/OFL.txt` rather than a copy at the root: the
	// build emits it from `@crewlethq/tokens`' own `fonts/OFL.txt`, beside the
	// faces that package ships, so a font bump cannot leave the notice and the
	// files it covers describing different things.
	if _, err := os.Stat(filepath.Join(servedTree, "fonts", "OFL.txt")); err != nil {
		t.Errorf("the built tree carries embedded typefaces and no OFL notice: %v", err)
	}

	// THE NOTICES TRAVEL WITH WHAT THEY COVER. The bundle redistributes React,
	// the design system's three packages, the fonts and the Material Symbols
	// drawings, all under licenses that require their text alongside, and the
	// release archives and image copy this file from here. Written by the build
	// (vite.config.ts), so a build that lost `build.license` or the step
	// appending the fonts and the symbols leaves a tree that serves perfectly
	// and owes notices it no longer carries.
	notices, err := os.ReadFile(filepath.Join(servedTree, "THIRD_PARTY_NOTICES.txt"))
	if err != nil {
		t.Errorf("no THIRD_PARTY_NOTICES.txt in the built tree; `npm run build` in "+
			"dashboard/ writes it through build.license: %v", err)
	}
	for _, want := range []string{
		"## react - ",               // a bundled package, from build.license
		"## react-dom - ",           // and its renderer
		"## @crewlethq/ui",          // the design system's components
		"## @crewlethq/tokens",      // its palette, type and faces
		"## @crewlethq/icons",       // its glyphs and marks
		"SIL OPEN FONT LICENSE",     // the font license, appended by sourceNotices
		"The Inter Project Authors", // naming both faces
		"The JetBrains Mono Project Authors",
		"Apache License", // the Material Symbols drawings, appended too
		"Material Symbols",
	} {
		if err == nil && !bytes.Contains(notices, []byte(want)) {
			t.Errorf("THIRD_PARTY_NOTICES.txt does not carry %q", want)
		}
	}

	// NOTHING external at runtime. The tree this replaces pulled three font
	// families from a CDN, so an air-gapped engine — a supported deployment —
	// rendered in a fallback face the design was never measured against.
	if bytes.Contains(shell, []byte("//fonts.googleapis.com")) ||
		bytes.Contains(shell, []byte("//fonts.gstatic.com")) {
		t.Errorf("the shell reaches an external font CDN; the faces are embedded")
	}
}

// staticRef matches a file under /static/ that the shell, a stylesheet or a
// module names by its absolute path: in a quote of any of the three kinds —
// Rolldown's minifier writes a module's strings as template literals — or in
// a url().
//
// A FILE, never a directory: the path must end in something other than a
// slash, with its closing quote right behind it. The preload helper every
// build with a lazy chunk carries holds the base itself — it returns the
// template literal /static/dashboard/ with a file name added — which is a
// prefix a name is joined onto and never a request, and reading it as one
// would fail the crawl over a fetch no browser makes.
var staticRef = regexp.MustCompile("[\"'`(](/static/[^\"'`)\\s]*[^/\"'`)\\s])[\"'`)]")

// TestTheShellLoadsFromTheBinary does what a browser does.
//
// Fetch /dashboard, then everything it names, then everything THOSE name —
// static imports, lazy chunks, the preload lists beside them, the faces a
// stylesheet asks for — all from the server rather than from disk
// (crawlDashboard). An asset missing from the embed, or served as the wrong
// type, takes the page with it, or, for a lazy chunk, the one screen that
// loads it while every other keeps working, which is the failure a reader
// finds before a test does.
func TestTheShellLoadsFromTheBinary(t *testing.T) {
	t.Parallel()
	c := crawlDashboard(t, newApp(t, api.Options{}))

	// A bundled shell names few assets by design: an entry module, a vendor
	// chunk, a stylesheet, an icon. The floor is what distinguishes that from
	// a shell that names NOTHING, which is what a build with a broken `base`
	// produces: relative URLs that resolve against whichever of `/` or
	// `/dashboard` the reader arrived at.
	if c.named < 2 {
		t.Fatalf("the shell asked for %d assets; it should name at least an "+
			"entry module and a stylesheet", c.named)
	}
	for _, p := range c.problems {
		t.Error(p)
	}

	if len(c.ofKind(".js")) < 1 {
		t.Errorf("no script reached from the shell")
	}
	if len(c.ofKind(".css")) < 1 {
		t.Errorf("no stylesheet reached from the shell")
	}
	// Every face the stylesheet declares has to be servable. One that is not
	// fails silently in a browser — the text simply renders in the fallback.
	if fonts := len(c.ofKind(".woff2")); fonts < 4 {
		t.Errorf("only %d font faces reached from the stylesheet, want 4", fonts)
	}
}

// hashedName is the shape the build writes under assets/: the module's or
// asset's own name, a dash, Rolldown's content hash — eight characters of the
// base64url alphabet, its default — and the extension.
//
// EXACTLY eight, because a looser count passes ordinary words:
// `settings-overview-panel.js` satisfies eight-or-more. A bundler bump that
// changes the length fails here, loudly, and the fix is this pattern.
//
// It is a SHAPE, and a shape is all a Go test can check: the hash is the
// bundler's, over its own chunk graph, and nothing here can recompute it. So
// a fixed name that happens to end in a dash and eight such characters
// (`use-keyboard.js`) would pass. Every way a hash has actually gone missing
// from a build — `[hash]` dropped from a file-name pattern, an emitFile with
// a fixed name, a `public/assets/` directory Vite copies verbatim — names the
// file after its module alone (`index.js`, `react.js`,
// `rolldown-runtime.js`), and each of those fails.
var hashedName = regexp.MustCompile(`^.+-[A-Za-z0-9_-]{8}\.[A-Za-z0-9]+$`)

// TestEveryFileUnderAssetsIsContentHashed holds the built tree to the promise
// the server makes about it.
//
// internal/api/dashboard.go serves everything under dashboard/assets/ as
// `public, max-age=31536000, immutable` — a browser that has the file never
// asks for it again, reload or not. That is correct only because the build
// names every file there `<name>-<hash>.<ext>`, so different bytes are a
// different URL. A file under assets/ without one — a plugin's emitFile with
// a fixed name, a `public/assets/` directory Vite copies verbatim, a
// `chunkFileNames` that dropped `[hash]` — would be pinned in every reader's
// browser for a year with nothing to say the page is running old code.
//
// It reads what the BINARY embeds, the tree the server answers from, and
// checks the server's answer for each file as well as its name.
func TestEveryFileUnderAssetsIsContentHashed(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{})

	names := 0
	err := fs.WalkDir(static.FS(), "dashboard/assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		names++
		if !hashedName.MatchString(path.Base(p)) {
			t.Errorf("%s is under assets/ without a content hash in its name, and "+
				"everything there is served immutable for a year: a changed file "+
				"under this name would never reach a browser that has it. Emit it "+
				"as assets/[name]-[hash][extname], or outside assets/", p)
		}
		res := fetch(t, a, "/static/"+p, nil)
		if got := res.Header.Get("Cache-Control"); got != forever {
			t.Errorf("/static/%s: cache control = %q, want %q — the name is a "+
				"version, so there is nothing to revalidate", p, got, forever)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the embedded assets/: %v", err)
	}
	// A FLOOR: the entry module, the React chunk and the stylesheet. A build
	// that moved its output elsewhere would leave this walking an empty
	// directory — or none — and passing.
	if names < 3 {
		t.Errorf("the embedded assets/ holds %d files; the entry, the React chunk "+
			"and the stylesheet are three", names)
	}
}

// TestTheShellFitsTheDashboardPolicy checks the committed shell and every
// stylesheet it reaches use nothing the dashboard's Content-Security-Policy
// refuses.
//
// The policy allows scripts, styles, fonts and images from this origin only,
// and no inline script, inline style or event-handler attribute. A browser
// enforces that with nothing on screen but a console violation, so a build that
// started inlining a theme bootstrap script, a critical-CSS block or a font
// from a CDN would serve a blank or unstyled page while every other test here
// passed. This reads what the binary serves and fails on the first of those.
func TestTheShellFitsTheDashboardPolicy(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{})

	res := fetch(t, a, "/dashboard", nil)
	if got := res.Header.Get("Content-Security-Policy"); got != pagepolicy.Dashboard {
		t.Errorf("the shell is served under %q, want the dashboard policy", got)
	}
	shell := mustFetch(t, a, "/dashboard", "text/html")

	styles, scripts := pagepolicy.InlineBlocks(shell)
	if len(styles) > 0 {
		t.Errorf("the shell has %d inline <style> blocks, which style-src 'self' refuses; "+
			"keep styles in the bundled stylesheet", len(styles))
	}
	for _, body := range scripts {
		if strings.TrimSpace(body) != "" {
			t.Errorf("the shell has an inline script, which script-src 'self' refuses; "+
				"move it into the bundle:\n%s", body)
		}
	}
	if m := inlineAttribute.Find(shell); m != nil {
		t.Errorf("the shell carries %q, an inline style or event handler the policy refuses", m)
	}
	for _, m := range shellReference.FindAllSubmatch(shell, -1) {
		if url := string(m[1]); !sameOrigin(url) {
			t.Errorf("the shell loads %s from another origin, which the policy refuses", url)
		}
	}

	// EVERY stylesheet the shell reaches, a lazy chunk's included: the
	// policy governs the whole document, so a sheet a screen loads later is
	// refused exactly as the one the shell links is — and it is refused on
	// that screen alone, where nothing else here would ever look.
	sheets := crawlDashboard(t, a).ofKind(".css")
	if len(sheets) == 0 {
		t.Error("no stylesheet was reached, so no url() was checked")
	}
	for _, sheet := range sheets {
		for _, u := range cssURL.FindAllSubmatch(sheet.body, -1) {
			ref := strings.Trim(string(u[1]), `"' `)
			if resolved, err := resolve(sheet.url, ref); err == nil && resolved == "" &&
				!strings.HasPrefix(ref, "data:") {
				t.Errorf("%s loads %s from another origin, which font-src and img-src refuse",
					sheet.url, ref)
			}
		}
	}
}

var (
	// inlineAttribute matches a style attribute or an on* event handler.
	inlineAttribute = regexp.MustCompile(`(?i)\s(style|on[a-z]+)\s*=`)
	// shellReference matches every src and href the shell names.
	shellReference = regexp.MustCompile(`(?i)\s(?:src|href)\s*=\s*["']([^"']+)["']`)
	// cssURL matches a url() in a stylesheet.
	cssURL = regexp.MustCompile(`url\(([^)]*)\)`)
)

// sameOrigin reports whether a URL resolves against the page's own origin:
// a path, never a scheme or a protocol-relative host.
func sameOrigin(url string) bool {
	return strings.HasPrefix(url, "/") && !strings.HasPrefix(url, "//")
}

// TestTheDesignSystemCascadesInOrder checks the served stylesheet carries the
// design system's baseline BEFORE the components that sit on it.
//
// Order is what decides a tie, and there is one. The baseline's `:focus-visible`
// rule and a component's `.crewlet-btn` rule each count as a single class, so
// whichever is written later wins every property they share. The baseline sets
// `border-radius`, which means a baseline written last squares off every
// button, input and dialog the moment a reader tabs to it, which is visible to
// somebody using the keyboard and to nothing else in this suite.
//
// It is an ORDERING of two files, so nothing but the built artifact can show
// it: the source says `import` and the cascade says which import was evaluated
// first, which is the bundler's answer rather than the author's. This reads
// what the browser is handed.
func TestTheDesignSystemCascadesInOrder(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{})
	shell := mustFetch(t, a, "/dashboard", "text/html")

	sheets := 0
	for _, m := range staticRef.FindAllSubmatch(shell, -1) {
		if !strings.HasSuffix(string(m[1]), ".css") {
			continue
		}
		sheets++
		sheet := mustFetch(t, a, string(m[1]), "text/css")

		// The baseline's own rule: `:focus-visible` as a whole selector, not a
		// component's `.crewlet-x:focus-visible`.
		base := baselineFocus.FindIndex(sheet)
		first := componentRule.FindIndex(sheet)
		if base == nil {
			t.Fatalf("%s carries no bare :focus-visible rule, so the design system's "+
				"baseline is not in the bundle at all; main.tsx imports "+
				"@crewlethq/tokens/css/base", m[1])
		}
		if first == nil {
			t.Fatalf("%s carries no component rule, so this scan proves nothing "+
				"about an order it cannot see", m[1])
		}
		if base[0] > first[0] {
			t.Errorf("%s writes the design system's baseline after the component "+
				"stylesheets (%d > %d), so the baseline wins every tie: a focused "+
				"control takes the baseline's radius. Import the five "+
				"@crewlethq/tokens stylesheets above every module import in main.tsx",
				m[1], base[0], first[0])
		}
	}
	if sheets == 0 {
		t.Error("the shell named no stylesheet, so nothing was measured")
	}
}

var (
	// baselineFocus matches the baseline's own focus rule: `:focus-visible` as
	// a complete selector, which is how it is told from a component's.
	baselineFocus = regexp.MustCompile(`(?:^|[{}])\s*:focus-visible\s*\{`)
	// componentRule matches the first rule of any design system component.
	componentRule = regexp.MustCompile(`\.crewlet-[a-z]`)
)

// protocolModule is the second build target: the socket client alone, which
// internal/e2e replays captured frames through. No page imports it, so the
// crawl from the shell never reaches it, and a gate over "everything the
// engine serves as a script" has to name it.
const protocolModule = dashboardBase + "protocol.js"

// A priceForm is one shape a price takes in a built module.
type priceForm struct {
	what string
	re   *regexp.Regexp
}

// priceForms is every shape the bundle scan refuses, written as Rolldown's
// minifier writes it — any of three quotes around a string, a template literal
// for most of them, and no whitespace it does not need.
//
// ONE FORM IS DELIBERATELY LEFT TO THE SOURCE SCAN: a dollar sign
// concatenated onto a value, `"$"+n`. React's own key escaping is written
// exactly that way — a one-character dollar string plus `e.replace(…)` — so
// in a bundle the two cannot be told apart, and a rule that fired on React's
// chunk would have to exempt React by name. dashboard/src/money.test.tsx reads
// that form in the parsed source, where a string in the dashboard's own code
// is distinguishable from one in a dependency's.
var priceForms = []priceForm{
	// The engine's price fields however a client would spell them:
	// `cost_usd` and `total_cost_usd` off the wire, `priced_calls`, and the
	// camel-cased copy a parser makes.
	{"names the engine's price field", regexp.MustCompile(`(?i)cost_?usd|priced_?calls`)},
	// Intl's currency formatting: the style value, and the option keys that
	// exist only to go with it.
	{"asks Intl for its currency style", regexp.MustCompile("[\"'`]currency[\"'`]")},
	{"passes Intl a currency option", regexp.MustCompile(`\bcurrency(?:Display|Sign)?\s*:`)},
	// A currency by its code, as a whole word — `costUSD` is the field rule's.
	{"names a currency by its code", regexp.MustCompile(`\b(?:USD|EUR|GBP)\b`)},
	// The euro and the pound mean nothing but money here, so they are refused
	// anywhere, escaped or not: the minifier may write either as `€`.
	{"carries a currency sign", regexp.MustCompile(
		`[€£]|\\u(?:20[aA][cC]|00[aA]3|\{20[aA][cC]\}|\{[aA]3\})|\\x[aA]3`)},
	// The dollar sign is NOT refused on its own: it opens every `${VAR}`
	// reference the dashboard explains. What is refused is a dollar sign in
	// front of a value — a template substitution right behind one, or a JSX
	// child that is a value right behind a text run ending in one. The braced
	// reference is a text run followed by the CONSTANT `{VAR}`, which the
	// last class excludes by refusing a quote.
	{"writes a dollar sign before a substitution", regexp.MustCompile(`\$\$\{`)},
	{"writes a dollar sign before a rendered value", regexp.MustCompile(
		"[\\[,]\\s*[\"'`][^\"'`\\n]*\\$\\s*[\"'`]\\s*,\\s*[^\"'`\\s\\]]")},
}

// A priceFound is one place a module holds a price, with the text around it,
// since a byte offset into a minified megabyte tells a reader nothing.
type priceFound struct {
	what, near string
}

// pricesInModule is every place a built module holds a price.
func pricesInModule(module []byte) []priceFound {
	var out []priceFound
	for _, form := range priceForms {
		for _, at := range form.re.FindAllIndex(module, -1) {
			from, to := max(0, at[0]-60), min(len(module), at[1]+40)
			out = append(out, priceFound{form.what, string(module[from:to])})
		}
	}
	return out
}

// TestTheDashboardRendersNoPrice holds what the engine SERVES to rule 19 of
// docs/reference/dashboard-design.md: the dashboard measures spend in tokens,
// and never in money.
//
// The engine records a price where one is reported — only a subscription
// coding CLI quotes one — and it is on the wire. A currency covering that
// minority of calls, beside a token count covering all of them, reads as the
// company's spend and is a fraction of it, so the client declares no price
// field and draws none. dashboard/src/money.test.tsx holds the source and the
// rendered screens; this holds the artefact, because the bundle is what a
// browser runs and a committed bundle can carry what the source no longer does
// — which is exactly how this landed: red on the build that still parsed
// `cost_usd` into a `costUSD` nothing drew.
//
// Every module the shell reaches, lazy chunks included, through the crawl
// TestTheShellLoadsFromTheBinary uses, and protocol.js, which no page imports.
func TestTheDashboardRendersNoPrice(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{})
	c := crawlDashboard(t, a)
	// A crawl that lost a file would scan less than the engine serves and
	// call it clean. TestTheShellLoadsFromTheBinary names each problem; this
	// only refuses to vouch for a bundle it could not read whole.
	if len(c.problems) > 0 {
		t.Fatalf("the crawl could not read %d of the files the shell reaches, so a "+
			"price in one of them would go unseen; TestTheShellLoadsFromTheBinary names them",
			len(c.problems))
	}
	modules := map[string][]byte{protocolModule: mustFetch(t, a, protocolModule, "text/javascript")}
	for _, f := range c.ofKind(".js") {
		modules[f.url] = f.body
	}
	// A FLOOR: the entry, the React chunk the build splits out, and
	// protocol.js. A crawl that reached nothing would otherwise pass.
	if len(modules) < 3 {
		t.Fatalf("scanned %d modules; the entry, the React chunk and protocol.js are three",
			len(modules))
	}
	for url, body := range modules {
		for _, p := range pricesInModule(body) {
			t.Errorf("%s %s: …%s… — the dashboard renders tokens and never money "+
				"(rule 19 in docs/reference/dashboard-design.md); remove it from "+
				"dashboard/src, then `make dashboard`", url, p.what, p.near)
		}
	}
}

// TestThePriceScanReadsWhatTheMinifierWrites certifies the scan itself, in
// both directions, over modules written the way the build writes them: each
// form it refuses must be found as exactly that form, and each dollar sign the
// dashboard and React really do ship must not be. A scan that stopped reading
// a form passes a bundle that prices something; a scan that cried wolf over
// React's chunk is one somebody would switch off.
func TestThePriceScanReadsWhatTheMinifierWrites(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, module, want string
	}{
		{"a parsed price", "sandboxId:String(t.sandbox_id??``),costUSD:Ry(t.cost_usd),",
			"names the engine's price field"},
		{"the priced-call count", "f.totals.priced_calls>0&&(0,m.jsxs)(`p`,{})",
			"names the engine's price field"},
		{"Intl's currency style", "new Intl.NumberFormat(void 0,{style:`currency`,currency:e})",
			"asks Intl for its currency style"},
		{"an Intl currency option", "e.toLocaleString(void 0,{currencyDisplay:`code`})",
			"passes Intl a currency option"},
		{"a currency code", `children:[e," USD"]`, "names a currency by its code"},
		{"the euro sign", "children:[e,` €`]", "carries a currency sign"},
		{"an escaped euro", `children:[e,"€"]`, "carries a currency sign"},
		{"an escaped pound", `var p="\xA3";`, "carries a currency sign"},
		{"a dollar in a template", "var s=`$${e.toFixed(2)}`;", "writes a dollar sign before a substitution"},
		{"a dollar in JSX", "(0,m.jsxs)(`b`,{children:[`$`,e]})", "writes a dollar sign before a rendered value"},
		{"a spaced dollar in JSX", `(0,m.jsxs)("b",{children:["US$ ",t.amount]})`,
			"writes a dollar sign before a rendered value"},
	} {
		t.Run("finds "+tc.name, func(t *testing.T) {
			t.Parallel()
			found := pricesInModule([]byte(tc.module))
			if !slices.ContainsFunc(found, func(p priceFound) bool { return p.what == tc.want }) {
				t.Errorf("%s: found %v, want a finding that it %s", tc.module, found, tc.want)
			}
		})
	}
	for _, tc := range []struct{ name, module string }{
		// React's key escaping and its hydration markers, verbatim.
		{"React's key escaping", "function se(e){var t={\"=\":`=0`,\":\":`=2`};return`$`+e.replace(/[=:]/g,function(e){return t[e]})}"},
		{"React's marker set", "for(var i=0;i<n.length;i++)t[`$`+n[i]]=!0;"},
		{"React's comment markers", "if(n===`$`||n===`$?`||n===`$~`||n===`$!`||n===`&`)"},
		// The dashboard's own dollar signs, as the build writes them today.
		{"a braced reference in JSX", "(0,m.jsxs)(U,{children:[`$`,`{VAR}`]})"},
		{"a reference's opening sign", "w=C===``&&n.trimStart().startsWith(`$`)"},
		{"an escaped reference", "var r=`\\${${e}}`;"},
		{"a word that holds a code", "var e={focusUSDC:1};"},
	} {
		t.Run("leaves "+tc.name, func(t *testing.T) {
			t.Parallel()
			if found := pricesInModule([]byte(tc.module)); len(found) > 0 {
				t.Errorf("%s: found %v in a module that prices nothing", tc.module, found)
			}
		})
	}
}

// TestTheNoticesAreServedAsText checks a running engine answers its notices,
// from the binary, as text a browser shows.
func TestTheNoticesAreServedAsText(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{})
	for _, url := range []string{
		"/static/dashboard/THIRD_PARTY_NOTICES.txt",
		"/static/dashboard/fonts/OFL.txt",
	} {
		mustFetch(t, a, url, "text/plain; charset=utf-8")
	}
}

// TestEveryStaticFileIsInTheBinary guards the embed pattern against the tree.
//
// The pattern is a LIST, and a list is a thing that goes stale. It named
// `dashboard` and nothing else once, so the two icons one directory above it —
// the favicon and the sidebar brand, both asked for by the shell — were simply
// absent from the binary. Every module and stylesheet served perfectly; the
// page just had no logo.
func TestEveryStaticFileIsInTheBinary(t *testing.T) {
	t.Parallel()
	embedded := embeddedTree(t)
	err := filepath.WalkDir(staticDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".go") {
			return err
		}
		rel, err := filepath.Rel(staticDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		want, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		got, ok := embedded[rel]
		if !ok {
			t.Errorf("%s is in %s but not in the binary; add it to the embed "+
				"pattern in static/static.go", rel, staticDir)
			return nil
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: %d bytes embedded, %d on disk", rel, len(got), len(want))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", staticDir, err)
	}
}

// embeddedTree reads the whole embedded FS into a path -> bytes map.
func embeddedTree(t *testing.T) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := fs.WalkDir(static.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(static.FS(), p)
		if err != nil {
			return err
		}
		out[p] = data
		return nil
	})
	if err != nil {
		t.Fatalf("walking the embedded tree: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the embedded static tree is empty")
	}
	return out
}

// mustFetch gets one asset and insists on its type.
func mustFetch(t *testing.T, a *api.App, url, wantType string) []byte {
	t.Helper()
	res := fetch(t, a, url, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d — the shell names it, so the binary must serve it",
			url, res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, wantType) {
		t.Errorf("%s served as %q, want %s — a browser refuses the wrong type "+
			"and the failure names the MIME rather than the file", url, got, wantType)
	}
	body := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	_ = res.Body.Close()
	if len(body) == 0 {
		t.Errorf("%s is empty", url)
	}
	return body
}
