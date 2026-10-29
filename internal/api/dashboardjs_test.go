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
// docs/reference/dashboard-design.md). And one property only the artefact
// has at all: its size, held to one budget by TestTheDashboardFitsItsBudget.
//
// The dashboard's own assertions (its protocol, its router, its ordering
// rules, and the measured contrast of every colour token in both themes) run
// under Vitest — `npm test` in dashboard/, wired into `make dashboard-test` and
// its own CI job. They are not driven from Go any more: they were, because
// there was no package.json and no runner, and driving a real test runner from
// a Go subprocess would be a second way to run one suite.

import (
	"bytes"
	"fmt"
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
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/static"
)

// staticDir is the directory package static embeds, on disk.
func staticDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(sourcetree.Root(t), "static")
}

// servedTree is the built dashboard the binary serves.
func servedTree(t *testing.T) string {
	t.Helper()
	return filepath.Join(staticDir(t), "dashboard")
}

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
	served := servedTree(t)
	shell, err := os.ReadFile(filepath.Join(served, "index.html"))
	if err != nil {
		t.Fatalf("no built shell at %s — run `make dashboard`: %v", served, err)
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
	if _, err := os.Stat(filepath.Join(served, "protocol.js")); err != nil {
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
	css, err := os.ReadFile(filepath.Join(served, strings.TrimPrefix(sheetPath, "/static/dashboard/")))
	if err != nil {
		t.Fatalf("the shell names %s and the tree does not have it: %v", sheetPath, err)
	}
	faces := regexp.MustCompile(`url\(([^)]*\.woff2)\)`).FindAllSubmatch(css, -1)
	// EXACTLY THE FOUR GEIST FACES: Geist and Geist Mono, at two subsets each.
	// A stylesheet that asks for no face at all passes every assertion below
	// it — which is exactly what dropping the font import would look like —
	// and one that asks for a FIFTH is a family the design system no longer
	// ships (0.5.0 retired Inter and JetBrains Mono) arriving from somewhere
	// it should not. The subsets are the package's to name, so only the
	// family prefix is read: two faces of `geist-mono-*`, two of `geist-*`.
	families := map[string]int{}
	for _, face := range faces {
		name := path.Base(strings.Trim(string(face[1]), `"'`))
		switch {
		case strings.HasPrefix(name, "geist-mono-"):
			families["Geist Mono"]++
		case strings.HasPrefix(name, "geist-"):
			families["Geist"]++
		default:
			t.Errorf("the stylesheet asks for %s, which is not a Geist face — the "+
				"design system's families are Geist and Geist Mono", name)
		}
	}
	if len(faces) != 4 || families["Geist"] != 2 || families["Geist Mono"] != 2 {
		t.Errorf("the stylesheet asks for %d faces (%v); the design system ships "+
			"exactly four — Geist and Geist Mono at two subsets each", len(faces), families)
	}
	for _, face := range faces {
		ref := strings.Trim(string(face[1]), `"'`)
		rel := strings.TrimPrefix(ref, "/static/dashboard/")
		if _, err := os.Stat(filepath.Join(served, rel)); err != nil {
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
	if _, err := os.Stat(filepath.Join(served, "fonts", "OFL.txt")); err != nil {
		t.Errorf("the built tree carries embedded typefaces and no OFL notice: %v", err)
	}

	// THE NOTICES TRAVEL WITH WHAT THEY COVER. The bundle redistributes React,
	// the design system's three packages, the QR encoder the authenticator
	// setup draws with, the Geist faces and the Lucide drawings, all under
	// licenses that require their text alongside, and the release archives
	// and image copy this file from here. Written by the build
	// (vite.config.ts), so a build that lost `build.license` or the step
	// appending the fonts and the glyphs leaves a tree that serves perfectly
	// and owes notices it no longer carries.
	//
	// AND WHAT IT NO LONGER COVERS IS GONE. The faces were Inter and JetBrains
	// Mono and the glyphs Material Symbols until 0.5.0; a notice still naming
	// them is a notice written from a file the build no longer reads, which
	// means the step that appends the real ones is not running either.
	notices, err := os.ReadFile(filepath.Join(served, "THIRD_PARTY_NOTICES.txt"))
	if err != nil {
		t.Errorf("no THIRD_PARTY_NOTICES.txt in the built tree; `npm run build` in "+
			"dashboard/ writes it through build.license: %v", err)
	}
	for _, want := range []string{
		"## react - ",                   // a bundled package, from build.license
		"## react-dom - ",               // and its renderer
		"## @crewlethq/ui",              // the design system's components
		"## @crewlethq/tokens",          // its palette, type and faces
		"## @crewlethq/icons",           // its glyphs and marks
		"## uqr - ",                     // the QR encoder (MIT, Project Nayuki's code)
		"SIL OPEN FONT LICENSE",         // the font license, appended by sourceNotices
		"The Geist Project Authors",     // naming the faces
		"ISC License",                   // the Lucide drawings, appended too
		"Lucide Icons and Contributors", // naming who holds them
	} {
		if err == nil && !bytes.Contains(notices, []byte(want)) {
			t.Errorf("THIRD_PARTY_NOTICES.txt does not carry %q", want)
		}
	}
	for _, gone := range []string{
		"The Inter Project Authors",
		"The JetBrains Mono Project Authors",
		"Apache License",
		"Material Symbols",
	} {
		if err == nil && bytes.Contains(notices, []byte(gone)) {
			t.Errorf("THIRD_PARTY_NOTICES.txt still carries %q, which nothing in "+
				"this build redistributes any more", gone)
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
// ([embeddedDashboard], crawled once for the suite). An asset missing from the
// embed, or served as the wrong type, takes the page with it, or, for a lazy
// chunk, the one screen that loads it while every other keeps working, which
// is the failure a reader finds before a test does.
func TestTheShellLoadsFromTheBinary(t *testing.T) {
	t.Parallel()
	c := embeddedDashboard.crawl

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
	a := embeddedDashboard.app

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
	a := embeddedDashboard.app

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
	sheets := embeddedDashboard.crawl.ofKind(".css")
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
//
// The arrangement it holds is main.tsx's: the kit's token sheets (variables,
// themes, density, fonts, base) above every module import, the components'
// own sheets with the modules that import them, and this application's sheets
// last. There is no alias layer between the kit and ours any more — every
// sheet here reads the kit's tokens under their own names — so what is left
// to order is exactly this one tie.
func TestTheDesignSystemCascadesInOrder(t *testing.T) {
	t.Parallel()
	a := embeddedDashboard.app
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
		// AND NOTHING RESTATES IT BELOW THEM. A bare focus rule written after
		// the components is the same tie lost the other way: `:focus { outline:
		// none }` erased the baseline's ring from every control relying on it,
		// and the `:focus-visible` copy that answered it drew a second ring
		// on the field inside every kit Input and a clipped one on the command
		// palette's field, which the kit draws with none.
		for _, at := range bareFocusRules(sheet) {
			if at < first[0] {
				continue
			}
			t.Errorf("%s declares a bare focus rule at %d, after the first component "+
				"rule (%d): %q. It wins every one-class tie against a component's "+
				"own focus treatment; leave the ring to @crewlethq/tokens/css/base, "+
				"and give a control that resets its outline its ring back by name",
				m[1], at, first[0], strings.TrimSpace(string(sheet[at:min(len(sheet), at+48)])))
		}
	}
	if sheets == 0 {
		t.Error("the shell named no stylesheet, so nothing was measured")
	}
}

// TestNoLazyStylesheetCarriesTheDesignSystem holds the other half of that
// order: every design system rule is in the sheet the SHELL links, and none
// arrives with a lazy chunk.
//
// The dashboard is split into a chunk per workspace, and a uilet component
// imports its own stylesheet as a side effect of its module — so a component
// reached only from one workspace takes its sheet into that workspace's chunk,
// and a browser appends that sheet when the chunk loads, AFTER the dashboard's
// own. Every one-class tie the dashboard's rules win against a component's is
// then lost the moment a reader first opens that workspace, on that screen and
// every screen after it, and on no screen before. The source says nothing
// about it and a first-paint screenshot cannot see it. vite.config.ts's
// `designSystemSheet` is what prevents it: the whole set in one sheet above
// ours, and every per-component import answered empty.
func TestNoLazyStylesheetCarriesTheDesignSystem(t *testing.T) {
	t.Parallel()
	c := embeddedDashboard.crawl
	for _, p := range c.problems {
		t.Error(p)
	}
	chunks := 0
	for _, f := range c.ofKind(".js") {
		if f.how == fromDynamic {
			chunks++
		}
	}
	// The premise: a build with no lazy chunk has no lazy sheet either, and
	// would pass below having checked nothing.
	if chunks == 0 {
		t.Fatal("no lazy chunk was reached, so the dashboard is not code-split and " +
			"this test measures nothing; app/lazyScreen.ts imports each workspace with import()")
	}
	for _, sheet := range c.ofKind(".css") {
		if sheet.how == fromShell {
			continue
		}
		if at := componentRule.FindIndex(sheet.body); at != nil {
			t.Errorf("%s, loaded with a lazy chunk (named by %s), carries a design system rule: %q. "+
				"It is appended after the dashboard's own sheets and wins every tie they used to; "+
				"vite.config.ts's designSystemSheet must answer that component's stylesheet import",
				sheet.url, sheet.by, strings.TrimSpace(string(sheet.body[at[0]:min(len(sheet.body), at[0]+48)])))
		}
	}
}

var (
	// baselineFocus matches the baseline's own focus rule: `:focus-visible` as
	// a complete selector, which is how it is told from a component's.
	baselineFocus = regexp.MustCompile(`(?:^|[{}])\s*:focus-visible\s*\{`)
	// componentRule matches the first rule of any design system component.
	componentRule = regexp.MustCompile(`\.crewlet-[a-z]`)
)

// bareFocusRules returns where each focus rule with no subject of its own
// starts: `:focus` or `:focus-visible` as a complete selector, alone or in a
// list, which is the one-class shape that ties with a component.
//
// READ FROM THE SELECTOR LIST, NOT FROM A PATTERN OVER THE TEXT. A comma inside
// a functional pseudo-class separates that function's arguments, not the
// rule's selectors: `.crewlet-arrange-frame:is(:hover,:focus-visible)` has
// one subject, the frame, and a pattern that took the comma for a selector
// boundary read its `:focus-visible` as a bare rule. So a prelude is split at
// the commas outside every parenthesis, and an at-rule's prelude (`@media`)
// is never a selector list at all.
func bareFocusRules(sheet []byte) []int {
	var found []int
	start, depth := 0, 0
	var quote byte
	for i := 0; i < len(sheet); i++ {
		c := sheet[i]
		switch {
		case quote != 0:
			switch c {
			case '\\':
				i++
			case quote:
				quote = 0
			}
		case c == '/' && i+1 < len(sheet) && sheet[i+1] == '*':
			end := bytes.Index(sheet[i+2:], []byte("*/"))
			if end < 0 {
				return found
			}
			i += end + 3
		case c == '"' || c == '\'':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth > 0:
		case c == '{':
			if prelude := sheet[start:i]; !bytes.HasPrefix(bytes.TrimSpace(prelude), []byte("@")) {
				part := start
				for j := start; j <= i; j++ {
					if j < i && sheet[j] == '(' {
						depth++
					} else if j < i && sheet[j] == ')' {
						depth--
					}
					if j == i || (depth == 0 && sheet[j] == ',') {
						if sel := strings.TrimSpace(string(sheet[part:j])); sel == ":focus" || sel == ":focus-visible" {
							found = append(found, part+strings.Index(string(sheet[part:j]), ":"))
						}
						part = j + 1
					}
				}
				depth = 0
			}
			start = i + 1
		case c == '}' || c == ';':
			start = i + 1
		}
	}
	return found
}

// TestBareFocusRulesReadTheSelectorList holds the reading the cascade check
// depends on: a bare rule is found alone, in a list and inside an at-rule,
// and a component's own focus treatment is never one, whatever its
// functional pseudo-classes hold.
func TestBareFocusRulesReadTheSelectorList(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		css  string
		want int
	}{
		{":focus-visible{outline:2px solid}", 1},
		{"a,:focus{outline:none}", 1},
		{":focus , :focus-visible{outline:none}", 2},
		{"@media (min-width:1px){:focus-visible{outline:0}}", 1},
		{".crewlet-arrange-frame.is-editing:is(:hover,:focus-visible,:has(:focus-visible)):after{outline:0}", 0},
		{".crewlet-btn:focus-visible{outline:0}", 0},
		{".x:not(:focus),.y{color:red}", 0},
		{".x{content:\":focus-visible{\"}", 0},
		{"/* :focus{ */.x{color:red}", 0},
	} {
		if got := len(bareFocusRules([]byte(tc.css))); got != tc.want {
			t.Errorf("bareFocusRules(%q) found %d, want %d", tc.css, got, tc.want)
		}
	}
}

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

// priceExemptions is every span the bundle scan admits although a price form
// matches inside it. Each is the design system's sheet model
// (@crewlethq/ui SheetModel) handling an artifact sheet's own cells, which are
// the author's data drawn in the format the author declared for the column,
// never a figure the engine recorded: the carve-out rule 19 of
// docs/reference/dashboard-design.md makes for a sheet's cells, as it does for
// a record shown verbatim.
//
// EACH IS WRITTEN AS THE WHOLE CONSTRUCT, not as a word inside it, so a
// currency format or a sign anywhere else is still refused, and a rewrite of
// the sheet model that changes the construct's shape turns the gate red for a
// reader to look at again rather than passing it.
var priceExemptions = []*regexp.Regexp{
	// A number column formatted in the currency its schema declares: the code
	// is the column's own `currency`, chosen by its `numberStyle`.
	regexp.MustCompile(`\w+\.numberStyle===` + quoted("currency") + `&&\w+\.currency\?` +
		`new Intl\.NumberFormat\(\w+,\{style:` + quoted("currency") + `,currency:\w+\.currency[,}]`),
	// A currency sign stripped from typed or pasted input before it is parsed
	// as a number: a character class of a regular expression literal that
	// holds currency signs and nothing else, which matches what a person
	// typed and never draws anything.
	regexp.MustCompile(`/(?:\^?\[[$€£¥]+\]\$?\|)*\^?\[[$€£¥]+\]\$?/[dgimsuvy]*`),
}

// quoted is a regular expression for word as a string literal in any of the
// three quotes the minifier writes.
func quoted(word string) string {
	return "[\"'`]" + regexp.QuoteMeta(word) + "[\"'`]"
}

// A priceFound is one place a module holds a price, with the text around it,
// since a byte offset into a minified megabyte tells a reader nothing.
type priceFound struct {
	what, near string
}

// priceFormNeeds is what each of priceForms cannot match without
// (sourcetree.Required), by index. Every form but the field name's opens with
// a class or `\b`, which leaves the regexp package no prefix to jump to, so
// each ran over every byte of every module; a module that holds nothing a form
// needs is not handed to it, which cannot change what it finds. The big
// chunks hold most of what the forms need, so this skips the small ones.
var priceFormNeeds = func() []sourcetree.Prefilter {
	out := make([]sourcetree.Prefilter, len(priceForms))
	for i, form := range priceForms {
		out[i] = sourcetree.Required(form.re)
	}
	return out
}()

// pricesInModule is every place a built module holds a price, outside the
// spans priceExemptions admits.
func pricesInModule(module []byte) []priceFound {
	var admitted [][]int
	for _, re := range priceExemptions {
		admitted = append(admitted, re.FindAllIndex(module, -1)...)
	}
	inAdmitted := func(at []int) bool {
		return slices.ContainsFunc(admitted, func(span []int) bool {
			return span[0] <= at[0] && at[1] <= span[1]
		})
	}
	var out []priceFound
	for i, form := range priceForms {
		if !priceFormNeeds[i].Admits(module) {
			continue
		}
		for _, at := range form.re.FindAllIndex(module, -1) {
			if inAdmitted(at) {
				continue
			}
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
	a := embeddedDashboard.app
	c := embeddedDashboard.crawl
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
		// The sheet model's admitted spans admit only themselves.
		{"a currency format beside a sheet column's",
			"e.numberStyle===`currency`&&e.currency?new Intl.NumberFormat(t,{style:`currency`,currency:e.currency,...r}):0;" +
				"var c=new Intl.NumberFormat(t,{style:`currency`,currency:n})",
			"asks Intl for its currency style"},
		{"a currency format with a fixed code in the sheet's shape",
			"e.numberStyle===`currency`&&e.currency?new Intl.NumberFormat(t,{style:`currency`,currency:`EUR`})",
			"asks Intl for its currency style"},
		{"a sign written back after a stripped one", "r=r.replace(/^[$€£¥]/g,``)+`€`",
			"carries a currency sign"},
		{"a sign class that holds more than signs", "r=r.replace(/[€a]/g,``)", "carries a currency sign"},
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
		// The design system's sheet model, as the build writes it today: a
		// number column in the currency its schema declares, and the signs it
		// strips from typed input before parsing a number.
		{"a sheet column's declared currency", "return e.numberStyle===`percent`?new Intl.NumberFormat(t,{style:`percent`,...r}):" +
			"e.numberStyle===`currency`&&e.currency?new Intl.NumberFormat(t,{style:`currency`,currency:e.currency,...r}):new Intl.NumberFormat(t,r)}"},
		{"the signs a sheet strips from input", "if(i&&(r=r.slice(0,-1)),r=r.replace(/^[$€£¥]|[$€£¥]$/g,``),/^-?\\d{1,3}(,\\d{3})+$/.test(r))"},
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
	a := embeddedDashboard.app
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
	static := staticDir(t)
	embedded := embeddedTree(t)
	err := sourcetree.Walk(static, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".go") {
			return err
		}
		rel, err := filepath.Rel(static, p)
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
				"pattern in static/static.go", rel, static)
			return nil
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: %d bytes embedded, %d on disk", rel, len(got), len(want))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", static, err)
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

// The dashboard's size budget: ONE set of numbers, and this file is where
// they live. dashboard/vite.config.ts's chunkSizeWarningLimit is derived from
// lazyChunkBudget and names this test as its authority; the prose in
// docs/reference/dashboard-design.md ("How it is built") quotes these values.
//
// Script and stylesheet sizes are what the engine SENDS a browser that asks
// for gzip, read off its own answer (servedSize): the file compressed at
// gzip.BestCompression (internal/api/dashboard.go compresses each text file
// once, at that level), or the file as it is where that is not smaller. A raw
// byte count would measure minified whitespace and identifiers the wire never
// carries, and a count recomputed here would agree with the server only while
// it kept the server's level and rule in step by hand.
const (
	// initialBudget is the entry, its static import graph and the stylesheet
	// the shell links: everything a reader downloads before the first screen
	// can draw. 300 KiB is under two seconds on a 1.5 Mbit/s link, the slow
	// end of what a person opening a dashboard over a phone tether gets. The
	// single-bundle build this replaced sent 403 KB here, the org builder —
	// about 43% of the source — among it; the frame alone (the sidebar, the
	// header, the palette, the socket client, React and the design system's
	// sheet) measured about 230 KB when this budget was set. It had grown to
	// 297 KiB by the time the rich editor came, which adds about 4 KiB here
	// although its code is fetched only to edit: the shell sends every design
	// system stylesheet in its one sheet, by rule (see
	// TestNoLazyStylesheetCarriesTheDesignSystem), so the editor's 1.8 KiB of
	// rules ride the first paint, and the Markdown grammar the reader's
	// renderer and the editor share became a chunk of its own. It moved to
	// 310 KiB then, still under two seconds on that link. The create pages
	// took the load to 314.5 KiB, and it came back without a change here:
	// their rules (3.1 KiB) arrive with them in create.css, and the command
	// palette (about 10 KiB), mounted only while open, became a chunk of its
	// own, which left it at 300.2 KiB. The settings tables, catalogue tiles
	// and numbered lists of uilet 0.39.0 took it to 310.6 KiB: their rules
	// (0.5 KiB) ride the one sheet by the same rule, and the shared screen
	// helpers that grew with them (QueryState's empty-state action, the seat
	// people helpers, 1.0 KiB) are in the entry's graph. 312 KiB, still under
	// two seconds on that link. The kit's lists, page parts and navigation
	// pieces (uilet 0.40 and 0.41) moved the dashboard's own rules into that
	// sheet rather than adding to it, and the rail head's status, the
	// engine's state beside the company's name, brought its rules and its
	// component into the shell: 312.1 KiB. 314 KiB, the same two seconds.
	// uilet 0.46 and 0.47's editing pieces (the inline editor, the character
	// count, the list's undo, drag, limit and narrow-row rules, the one-line
	// textarea, the passage and compact registers) ride the one sheet by the
	// same rule (1.4 KiB), and FormField's limit brought the character count
	// into the entry's graph with it (1.1 KiB): 315.1 KiB. 316 KiB, still
	// under two seconds on that link. Artifact folders and the company brand
	// (uilet 0.49) took it to 320.3 KiB. The one sheet carries 4.3 KiB more:
	// the brand scope's rules (1.3 KiB), which restyle a doc or a board in the
	// adopted brand and are design-system rules by the same rule, and the
	// folder tile, selection, logo, colour, contrast, type, inline list and
	// phone sheet rules. The brand page's and the folder screens' own rules
	// load with those screens (brand.css, folders.css), the brand's contract
	// rows stay in the lazy brand code (contract/brand.ts), and the theme
	// palettes a logo slot reads stay in its lazy chunk (uilet 0.49.1). What
	// the frame gained is the folder keys (0.6 KiB) and the Brand row, the
	// folder and brand routes and the kind's words (0.5 KiB). 321 KiB, still
	// under two seconds on that link. The brand setup wizard (uilet 0.50)
	// brought the setup layout's and the vertical stepper's rules into the
	// one sheet by the same rule (0.8 KiB); the setup page itself loads with
	// the brand's screens: 321.1 KiB. 322 KiB, still under two seconds on
	// that link. uilet 0.51 took it to 322.6 KiB with no module newly in the
	// first paint: the bundler now splits Select (with app/fill.tsx) and Kbd
	// out of the entry into chunks of their own, each compressed apart (about
	// 1.1 KiB), beside 0.2 KiB of new kit code and 0.3 KiB of polished kit
	// rules in the one sheet. 323 KiB, still under two seconds on that link.
	// The Dashboard Assistant took it to 336.4 KiB: the kit's assistant
	// pieces (the cards, the aside, the peek rail, the time range picker)
	// ride the one sheet by the same rule; the Ask button's Orbit mark is
	// about 8.8 KiB while it is imported from the icons package's main entry,
	// which brings the vendor marks with it (the `@crewlethq/icons/product-mark`
	// entry recovers it); and the frame's own half of the assistant (the
	// button, the page context registry, the bus, the aside's arithmetic) is
	// the rest. The drawer, its store, its reducer, its cards and its stream
	// client are a chunk of their own, fetched when the panel first opens.
	// 337 KiB, still under two seconds on that link. uilet 0.56.0 measured
	// 336.0 KiB here: its icons package's main entry now tree-shakes to the
	// one mark asked for, so the Orbit mark costs the same through it as
	// through `@crewlethq/icons/product-mark`, which the frame imports anyway.
	// The assistant's page actions took it to 337.8 KiB (measured with and
	// without them on that release): the registry merging every mounted
	// screen's actions, the undo a view change leaves, the contract's table
	// of page actions, the question the palette queues for the panel, and the
	// object kinds a peek is named by, which a page's selection reads. Every
	// screen's own actions are in that screen's chunk. 338 KiB, still under
	// two seconds on that link. uilet 0.57.1 measured 338.1 KiB with the same
	// dashboard source: the design system's one stylesheet, which first paint
	// carries whole, gained the proposal card's decision refusal and
	// unconfirmed outcome, the non-modal overlay aside and table cells that
	// wrap. 339 KiB, still under two seconds on that link. uilet 0.63.0
	// measured 340.4 KiB with the same dashboard source: the deep work card,
	// the file list and the attachment chip join the one stylesheet, and the
	// labelled value field moves the form field, the text area and the
	// character count into the shared chunk the entry already loads. Moving
	// the setup form and its dialogs onto that field took it to 340.3 KiB.
	// 341 KiB, still under two seconds on that link. One vocabulary for seat
	// states and the plain sentence for each failure kind, which the frame's
	// shared seat words carry into the entry, took it to 341.1 KiB. 342 KiB,
	// still under two seconds on that link. uilet 0.66.0 and choosing screen
	// shapes by the screen's width took it to 345.6 KiB, the one sheet (the
	// 0.64.1 alignment and spacing pass across every component family, the
	// stacking grid and the screen width hooks) 115.5 KiB of it. 346 KiB,
	// still under two seconds on that link. uilet 0.67.0's product marks took
	// it to 346.6 KiB with the same dashboard source: each mark now carries a
	// 16 grid drawing beside its master, and the Ask button draws the
	// assistant's spark, whose motion and soon rules ride the one sheet. 347
	// KiB, still under two seconds on that link. uilet 0.69.0 took it to 348.3
	// KiB while the entry's own script shrank by 2 KiB (the drawer's Needs you
	// reads gone): BriefCard's rules ride the one sheet, and the release splits
	// the status card and suggestion row code BriefCard shares into chunks of
	// their own, each one more request's overhead in the entry's graph. 349
	// KiB, still under two seconds on that link. A read named in the answer
	// rather than drawn as a card (ADR-0061): the renderer saying which run
	// of prose opens its block, in the shared Markdown module the entry
	// loads, took it to 349.1 KiB. 350 KiB, still under two seconds on that
	// link. A model's usage limit as one condition per entry (ADR-0064): the
	// queue's wording of it and the CLI names it is said in, the time of day
	// it lifts, its remedy in the link table, and the store keeping a row a
	// push leaves as it was, all in the attention queue and the store the
	// frame loads, took it to 351.0 KiB. 352 KiB, still under two seconds on
	// that link. Steering a turn whose coding run is out, merged beside it (the
	// shared write hook reading a press label's own sentence for its receipt,
	// `landed`, which says when a note is read, in the seats chunk the entry
	// loads), measured 350.9 KiB with it (359,293 bytes), under 351 KiB:
	// 351 KiB again, still under two seconds on that link.
	initialBudget = 351 << 10
	// lazyChunkBudget holds each chunk the page fetches later — a workspace,
	// the org builder, a stylesheet a preload list names. A chunk is what a
	// reader waits for on the first click into a workspace the idle prefetch
	// has not reached yet, so it gets half the initial budget. The largest,
	// Settings, measured about 60 KB when this was set.
	lazyChunkBudget = 150 << 10
	// fontBudget is every face the build embeds, counted as served: woff2 is
	// compressed already and the engine never recompresses it. The four Geist
	// and Geist Mono faces (latin and latin-ext) measure 83,736 bytes, so 90 KB
	// is the four of them and room for the design system to re-cut them — and
	// a FIFTH face, which every text-bearing screen would fetch, does not fit.
	fontBudget = 90_000
	// treeBudget is the whole of static/dashboard, raw, which is what every
	// engine binary and image carries whether or not anybody opens the page.
	// It measured about 2.3 MB when first set at 3 MiB. The artifacts
	// workspace (the gallery, the viewer, the native sheet, presence and
	// version compare) took it from about 2.85 MiB to about 3.05 MiB, the
	// screen that room was left for, and it moved to 3.5 MiB then: room for
	// the screens still to come, while a bundle that doubled from the 2.3 MB
	// baseline is still refused. The rich editor that Doclet's pages and doc
	// tabs are edited in took it from about 3.23 MiB to about 3.53 MiB: its
	// chunk is about 270 KB raw, ProseMirror being 228 KB of it, and the
	// notices of the ten packages it brings add 15 KB. That chunk is fetched
	// only when somebody edits, so no reader's first paint pays for it, and it
	// moved to 4 MiB then, which still refuses a doubled baseline. Artifact
	// folders and the company brand took it from about 3.99 MiB to about
	// 4.14 MiB: the brand page, its adoption review and the brand kind's own
	// view are about 65 KB, the folder screens, the Move dialog and the
	// gallery's selection about 50 KB, and the design system's new rules in
	// the one sheet about 41 KB raw. Each of those screens is fetched only
	// when it is opened, and it moved to 4.25 MiB then, which still refuses a
	// doubled baseline. The Dashboard Assistant took it from 4285.7 KiB to
	// 4400.5 KiB: its panel is a chunk of about 51 KiB fetched when the
	// panel first opens, and the rest is the kit's assistant components and
	// rules and the frame code that came with them. It moved to 4.3 MiB
	// then, which still refuses a doubled baseline. The assistant's page
	// actions took it to 4427.2 KiB, 26.6 KiB of it theirs (measured with
	// and without them on uilet 0.56.0): each screen's parser, gate and
	// setters in that screen's chunk, the claim and report flow in the
	// panel's, and the address helper and contract table they share. 4432
	// KiB, which still refuses a doubled baseline. uilet 0.57.2 measured
	// 4432.4 KiB, up from 4431.6 KiB with the same dashboard source but the
	// drawer's one prop: the release drops the result table's first column
	// truncation and a flush side panel body's scrolling, and the drawer
	// keeps a scrolling body for its offline notice. 4433 KiB, which still
	// refuses a doubled baseline. Telling a person the assistant's model
	// cannot answer before they ask took it to 4434.2 KiB: the drawer's
	// notice, its words and the viewer's model member, in the panel's chunk
	// and the frame's. 4435 KiB, which still refuses a doubled baseline.
	// Folding the assistant's supporting reads and moving the Ask button's
	// dot onto its mark, on uilet 0.60.0, took it to 4438.7 KiB: the kit's
	// card fold and IndicatorAnchor and their rules in the one sheet, and
	// the panel's fold store and summaries. 4439 KiB, which still refuses a
	// doubled baseline. Drawing the assistant at work with the kit's
	// ThinkingIndicator in place of the caret and the skeleton, and the new
	// SendButton's held tint, running arc and glyph cross fade, on uilet
	// 0.61.0, took it to 4444.1 KiB: their rules in the one sheet, the
	// Assistant mark in the panel's chunk and the indicator's status words.
	// 4445 KiB, which still refuses a doubled baseline. Telling the
	// assistant which draft is open took it from 4444.3 KiB to 4479.1 KiB:
	// the one draft hook and its parser in the frame, each editing screen's
	// field list, help and setters in that screen's chunk (about thirty
	// screens), the card's file thumbnail in the panel's, and the brand and
	// integration field lists. 4480 KiB, which still refuses a doubled
	// baseline. uilet 0.63.0 measured 4483.0 KiB with the same dashboard
	// source: the assistant's deep work card, file list and attachment chip,
	// and their rules in the one sheet. Moving the dashboard's own labelled
	// field and its secret completion onto uilet's ValueField took it back
	// to 4480.0 KiB (the field, its completion helper and its list's rules
	// leave the bundle). 4481 KiB, which still refuses a doubled baseline.
	// The assistant's deep work, delivered files and image attachments took
	// it from 4480.0 KiB to 4491.9 KiB on uilet 0.63.0: in the panel's chunk,
	// the run card's mapping and its nested steps, the file card, the
	// composer's pick, drop and paste with their bounds and uploads, and the
	// preview hook the draft file card now shares. 4493 KiB, which still
	// refuses a doubled baseline. Merging main's MCP tool route (#371) beside
	// the assistant's card rendering took it to 4499.8 KiB: the live view's
	// CLI runs, their calls in flight and a run drawn before its first round.
	// 4501 KiB, which still refuses a doubled baseline. uilet 0.64.0 and a
	// card value drawn as what it is (a list as its items, a body as code,
	// page text as an inert preview, a colour beside its chip) took it to
	// 4503.4 KiB, in the panel's chunk and ChangeList's block rules in the one
	// sheet. 4504 KiB, which still refuses a doubled baseline. Marking the
	// open conversation in the assistant's history, showing each one's last
	// activity and telling the assistant of a brand draft in progress took it
	// to 4504.1 KiB: the menu's checked answers and the summary's member in
	// the panel's chunk, and the Brand page's read-only draft and its help in
	// that page's. 4505 KiB, which still refuses a doubled baseline. Telling
	// the assistant what each page action takes, built from each screen's
	// own declaration (the address keys' schema in the entry's registry, the
	// org builder's operation vocabulary, an open draft's fields), and each
	// action's own revision took it to 4509.5 KiB. 4510 KiB, which still
	// refuses a doubled baseline. Telling the assistant what the Inbox's
	// "Needs you" holds, from the model the page draws it with, and reading
	// dates on the company clock took it to 4510.4 KiB, in the Inbox's chunk
	// and the shared format module. 4511 KiB, which still refuses a doubled
	// baseline. Drawing the assistant's cards in a person's terms (a seat cell
	// by its name, a refusal by its kind, a pending change as pending, an
	// administrator's technical rows), in the panel's chunk, took it to 4511.5
	// KiB. 4512 KiB, which still refuses a doubled baseline. One vocabulary for
	// seat states, the plain sentence for each failure kind with the engine's
	// message behind a disclosure, the assistant's credit in every history and
	// the org chart's reveal of a new seat took it to 4513.6 KiB. 4514 KiB,
	// which still refuses a doubled baseline. uilet 0.66.0 (the 0.64.1
	// alignment and spacing pass across every component family, the stacking
	// grid and the screen width hooks, a card title registered once,
	// ProposalCard's danger tone and details, the thread's jumps and
	// near-future times) and choosing screen shapes by the screen's width took
	// it to 4545.4 KiB. 4546 KiB, which still refuses a doubled baseline.
	// Drawing a brand draft in its own cards with one next step, who last
	// changed it and a confirmed discard, in the Brand page's chunk less the
	// kit view's copy of the cards, which now shares the page's board, took it
	// to 4547.1 KiB. 4548 KiB, which still refuses a doubled baseline. Saying
	// which revision each node runs, a refusal included, took it to 4553.1
	// KiB: the one rule a node's standing is read by (the sidebar's Nodes
	// figure counts by it, so it is in the frame), its words beside the
	// Settings screens that say them, the Nodes screen's Running panel and
	// Lag fact, the Configuration screen's revision states, the after-save
	// strip and the ceiling and charter followers settling on a refusal
	// rather than "applying…", and the tab's last push kept through a
	// dropped socket. 4554 KiB, which still refuses a doubled baseline. uilet
	// 0.67.0's product marks (a 16 grid drawing beside each master, the spark's
	// motion and the soon keyline) and its artifact kit took it to 4557.3 KiB.
	// 4558 KiB, which still refuses a doubled baseline. Keeping an answered
	// inbox row legible in place with its answer under it, the closed Answered
	// group of the last week's answers and the answered ask's pane with its
	// four steps, on uilet 0.68.0's inbox row outcome and status and its
	// collapsible group, took it to 4566.4 KiB. 4567 KiB, which still refuses
	// a doubled baseline. A brief of the page or its open record in the
	// drawer, drawn by uilet 0.69.0's BriefCard, with the record each screen
	// has open reported to the assistant, took it to 4574.6 KiB. 4575 KiB,
	// which still refuses a doubled baseline. Reading a landed write's
	// caveats in a person's words off the act answer, each with whether the
	// person must act, so the toast stays only when they must (ADR-0060),
	// took it to 4576.5 KiB. 4577 KiB, which still refuses a doubled
	// baseline. Telling a change nobody could confirm in the dashboard's
	// own words, rather than the operation key's retry rule for a client
	// (ADR-0060), took it to 4577.4 KiB. 4578 KiB, which still refuses a
	// doubled baseline. Sortable column heads with a default order, aligned
	// columns, the filter pill in place of a tag of three spans, waits said
	// as times and a link-only row opened by Enter, on uilet 0.70.0
	// (FilterPill, the grid's align and icon columns, the value trigger),
	// took it to 4581.4 KiB. 4582 KiB, which still refuses a doubled
	// baseline. uilet 0.71.0's toast that stays up for as long as its words
	// take to read took it to 4582.1 KiB. 4583 KiB, which still refuses a
	// doubled baseline. A brief that says its verdict once and ranks its
	// rows, on uilet 0.72.0, and the record chip and words-sized table
	// columns uilet 0.73.0 ships beside it, took it to 4585.1 KiB. 4586 KiB,
	// which still refuses a doubled baseline. A read named in the answer
	// rather than drawn as a card (ADR-0061): its chip and peek, the mention
	// rule, the oversize card and a page that says it is one, less the folded
	// read cards they replace, took it to 4591.9 KiB. 4592 KiB, which still
	// refuses a doubled baseline. A limit's refusal naming where its ceiling
	// is raised (ADR-0060's remedy): the link table and its gate, the press
	// drawn on every surface a spent budget is shown on, a section arrived
	// at on Budgets, each person's assistant ceiling edited there, and the
	// one rule a refused question goes back in the box by, took it to
	// 4601.7 KiB. 4602 KiB, which still refuses a doubled baseline. The
	// review's fixes (a restore decided once per turn in the session, a
	// section matched by its path and name, Home's spent week through the
	// remedy gate) took it to 4602.0 KiB, past it by bytes. 4603 KiB, which
	// still refuses a doubled baseline. A model's usage limit as one
	// condition per entry (ADR-0064): its wording, the CLI names and its
	// remedy, the store keeping a row a push leaves as it was, and the Inbox
	// keeping the rows it drew, took it to 4606.9 KiB. 4607 KiB, which still
	// refuses a doubled baseline. The review's fixes (a failure's own text
	// and the spend by model entry kept to an admin, the provider split
	// offered to an admin alone, and the card that says so) took it to
	// 4607.0 KiB, past it by bytes. 4608 KiB, which still refuses a doubled
	// baseline. Saying what became of an answer to a parked coding run (sent,
	// recorded while its seat is busy and on what, answered, or not used and
	// why) off the run's own record, drawn with uilet 0.74.0's pending tone
	// while it is on its way, took it to 4610.7 KiB. 4611 KiB, which still
	// refuses a doubled baseline. Drawing what prose names with its mark
	// (the recogniser, the link hosts and project keys the frame reads, and
	// uilet 0.75.0's InlineRef with every vendor's mark) took it to
	// 4624.2 KiB, all of it in the lazy chunks that render Markdown: the
	// flattening moved out of the renderer (`lib/markdownText.ts`), so the
	// first load fell to 349.1 KiB. 4625 KiB, which still refuses a doubled
	// baseline. Linking only what the dashboard built in the assistant and the
	// Inbox (each address rebuilt on the company's own host), placing a bare
	// `#11` by its message, a bare `owner/repo` under the company's owners and
	// uilet 0.75.1's smaller marks took it to 4629.3 KiB, again all in the lazy
	// chunks that render Markdown: the first load is 348.9 KiB. 4630 KiB, which
	// still refuses a doubled baseline. Saying that a person's stop ended a
	// seat's coding run (a stopped turn's tag and a stopped run's span drawn
	// closed rather than failed, a seat whose last turn "was stopped", the
	// task's "stopped by a person" pill and the pause dialog's words) and
	// steering a turn whose coding run is out (Steer offered on a parked turn,
	// and a note read inside a run drawn on the run's bar in the words of its
	// step), merged beside it, took it to 4631.8 KiB. 4632 KiB, which still
	// refuses a doubled baseline. The hardening of inline links merged beside
	// that (a cited address drawn by the checked rule, a reply scoped by its
	// own words, references linked only under the company's owners, a
	// Confluence page rebuilt from its space and id, and uilet 0.75.2's
	// unclipped marks) took it to 4632.7 KiB, again in the lazy chunks that
	// render Markdown, the first load 348.8 KiB. 4633 KiB, which still
	// refuses a doubled baseline. A coding run's question asked and answered
	// as a decision (the run's ask drawn with the decision components, its
	// options read back with the chosen one marked, and its answered history
	// in the Inbox's Answered group), all in lazy chunks, took it to
	// 4637.3 KiB. 4638 KiB, which still refuses a doubled baseline. Its
	// review's fixes (a run's question drawn whole and clamped with Show all
	// rather than split, an answer naming the question it answers, a choice
	// read back with the words beside it, and the focus kept on the outcome)
	// took it to 4638.7 KiB. 4639 KiB, which still refuses a doubled
	// baseline. A model's cloud or gateway sign-in (auth.mode env) named on the
	// Models screen, its words in the shared models module every chunk that
	// imports it carries, took it to 4639.1 KiB. The Audit log's row for a
	// model gesture (`model_acted`, its action and its entry linked to the
	// models) took it to 4639.4 KiB.
	// 4640 KiB, which still refuses a doubled baseline. Settings › Models built
	// on the engine's model answers (the list with its alarms, By job, a page
	// per model, the one wizard that adds and edits a model, and the test,
	// retry, device sign-in and removal flows), with uilet 0.76.0's choice
	// cards and setup wizard, took it to 4758.3 KiB: the Settings chunk the
	// first load does not fetch grew from 192 KiB to 303 KiB, and the first
	// load is 350.1 KiB. 4759 KiB, which still refuses a doubled baseline.
	// The device sign-in statuses held to the engine's set, with uilet
	// 0.76.1, kept it within 4759 KiB. The secret dialogs reading whether the
	// company switched to a write (`reload`), and saying so when it did not
	// rather than closing on the 200 (#457), took it to 4760.1 KiB. 4761 KiB,
	// which still refuses a doubled baseline. The models round (the Datadog
	// keys tested together, each failure named at its own key; one reading of
	// a /config failure by whether the engine answered, its sentence shared
	// by every screen that writes the company; and the Models fixes from the
	// owner's review: a removal on its entry's own tag, Review's list of who
	// changes model, an edit keeping its seats; an answer read as the
	// engine's only where it carries the engine's mark; every path segment
	// through one rule; rgb channels and trailing slashes read in linear
	// time; and uilet 0.77.0, its compact cards and CheckGrid in place of the
	// seat grid) took it to 4773.5 KiB. 4774 KiB, which still refuses a
	// doubled baseline.
	treeBudget = 4<<20 + 678<<10
)

// TestTheDashboardFitsItsBudget holds the built dashboard to the four numbers
// above, measured over what the server actually sends: the crawl
// TestTheShellLoadsFromTheBinary makes, partitioned by initialLoad into what a
// reader waits for before the first paint and what a screen fetches later.
//
// It exists because the cost of a bundle is paid by every reader on every cold
// load, and nothing else in the build notices it growing. The split that made
// each workspace its own chunk is undone by ONE static import of a screen's
// module from the frame — the bundler follows it and pulls the whole workspace
// back into the entry with every test still green — and a design-system bump
// that doubled its sheet or added a face would ride in the same way. A failure
// lists the files by size, largest first, because the fix is almost always in
// the biggest one.
func TestTheDashboardFitsItsBudget(t *testing.T) {
	t.Parallel()
	c := embeddedDashboard.crawl
	if len(c.problems) > 0 {
		t.Fatalf("the crawl could not read %d of the files the shell reaches, so their "+
			"size is unknown; TestTheShellLoadsFromTheBinary names them", len(c.problems))
	}
	initial := initialLoad(c)

	type measured struct {
		url  string
		size int
	}
	var first []measured
	firstTotal, lazy := 0, 0
	for _, f := range c.files {
		ext := path.Ext(f.url)
		if ext != ".js" && ext != ".css" {
			continue
		}
		size := servedSize(t, embeddedDashboard.app, f.url)
		if initial[f.url] {
			first = append(first, measured{f.url, size})
			firstTotal += size
			continue
		}
		lazy++
		if size > lazyChunkBudget {
			t.Errorf("%s is %s gzipped, over the %s a lazy chunk may be: it is what a reader "+
				"waits for on the first click into its screen. Split it — a section of a "+
				"workspace can be its own chunk, as the org builder is (app/lazyScreen.ts)",
				f.url, kib(size), kib(lazyChunkBudget))
		}
	}
	// FLOORS, so the budget cannot pass by measuring nothing: the initial load
	// is at least the entry, the React chunk and the stylesheet, and the page
	// splits into lazy chunks at all — a build that folded every screen back
	// into the entry would otherwise pass the chunk budget vacuously while
	// failing only the initial one, and a crawl that lost the lazy half would
	// pass both.
	if len(first) < 3 {
		t.Fatalf("the initial load is %d files; the entry, the React chunk and the "+
			"stylesheet are three", len(first))
	}
	if lazy < 5 {
		t.Errorf("the crawl reached %d lazy chunks; every workspace is one "+
			"(app/lazyScreen.ts), so the page has stopped splitting", lazy)
	}
	if firstTotal > initialBudget {
		slices.SortFunc(first, func(a, b measured) int { return b.size - a.size })
		var list strings.Builder
		for _, m := range first {
			fmt.Fprintf(&list, "\n  %9s  %s", kib(m.size), m.url)
		}
		t.Errorf("the initial load is %s gzipped, over its %s budget — a reader downloads all "+
			"of it before the first screen draws. The usual cause is a static import of a "+
			"screen's module from outside routes/, which pulls its whole workspace into the "+
			"entry (app/lazy.test.tsx refuses the ones it can see). Largest first:%s",
			kib(firstTotal), kib(initialBudget), list.String())
	}

	fonts, faces, tree := 0, 0, 0
	err := fs.WalkDir(static.FS(), "dashboard", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		tree += int(info.Size())
		if path.Ext(p) == ".woff2" {
			fonts += int(info.Size())
			faces++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the embedded dashboard: %v", err)
	}
	if faces == 0 {
		t.Error("the embedded dashboard carries no woff2 face, so the font budget measured nothing")
	}
	if fonts > fontBudget {
		t.Errorf("the %d embedded faces are %d bytes, over the %d the page may fetch in faces: "+
			"every screen with text asks for them", faces, fonts, fontBudget)
	}
	if tree > treeBudget {
		t.Errorf("static/dashboard is %s, over its %s: every engine binary and image carries "+
			"it, opened or not", kib(tree), kib(treeBudget))
	}
}

// kib renders a byte count the way the budgets are written.
func kib(n int) string {
	return fmt.Sprintf("%.1f KiB", float64(n)/1024)
}
