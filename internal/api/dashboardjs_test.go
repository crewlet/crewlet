package api_test

// GATE G5 — the dashboard the binary embeds is a REAL, COMPLETE application.
//
// The build output (dashboard/ into static/dashboard, embedded by package
// `static`) is COMMITTED so `go build` works with no Node, and CI's `dashboard`
// job rebuilds and diffs it. This file asserts what a diff cannot see: every
// file a browser is sent to fetch (the crawl is in dashboardcrawl_test.go) is
// reachable from the server with a content type it accepts, since an ES module
// as text/plain is REFUSED with a MIME error; nothing served renders a price
// (TestTheDashboardRendersNoPrice, rule 19 of docs/reference/dashboard-design.md);
// and the size fits TestTheDashboardFitsItsBudget. The dashboard's own
// assertions run under Vitest (`make dashboard-test`), never from Go.

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
	"github.com/crewlet/crewlet/internal/regexlit"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/static"
)

// staticDir is package static's directory on disk: the dashboard's committed
// build output, which the package embeds all of but the trees
// [unembeddedTrees] declares.
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
// skipped or writing into the served tree. Each leaves a tree that looks
// plausible in a diff and serves a blank page.
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
	// plain node, and without it that gate silently has nothing to run. It is
	// built BESIDE the served tree and never into it: no page loads it, and
	// in static/dashboard it rode the embed into every engine binary.
	protocol := filepath.Join(staticDir(t), "protocol", "protocol.js")
	if _, err := os.Stat(protocol); err != nil {
		t.Errorf("no %s — internal/e2e's client replay has nothing to run against; "+
			"`npm run build` in dashboard/ emits it: %v", protocol, err)
	}
	if _, err := os.Stat(filepath.Join(served, "protocol.js")); err == nil {
		t.Errorf("static/dashboard holds protocol.js, which no page loads, and the embed carries " +
			"every file there into every engine binary: dashboard/vite.protocol.config.ts builds it " +
			"into static/protocol")
	}

	// The faces are embedded so the dashboard renders identically on a closed
	// network; a missing one falls back silently to a system font.
	//
	// ASKED FOR AND PRESENT, rather than a list of names, which the design
	// system owns (@crewlethq/tokens' stylesheet). Reading them out of the
	// stylesheet is also stronger: a list cannot see a face the CSS asks for
	// and the build did not emit.
	//
	// THE FACES ARE UNHASHED, UNDER `fonts/`: dashboard/vite.config.ts routes
	// every .woff2 to `fonts/[name][extname]` and everything else to
	// `assets/[name]-[hash]`. This reads whatever path the stylesheet names.
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
	// the design system's packages, the QR encoder, the Geist faces and the
	// Lucide drawings, all under licenses that require their text alongside,
	// and the release archives and image copy this file from here. The build
	// writes it (vite.config.ts), so one that lost `build.license` or the step
	// appending the fonts and glyphs serves perfectly and owes notices.
	//
	// AND WHAT IT NO LONGER COVERS IS GONE: a notice still naming Inter,
	// JetBrains Mono or Material Symbols was written from a file the build no
	// longer reads, so the step appending the real ones is not running either.
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
		"Apache License",                // their license (Apache-2.0)
		"## @crewlethq/ui: NOTICE",      // and the NOTICE it requires, appended by withPackageNotices
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
// asset's own name, a dash, Rolldown's content hash (eight base64url
// characters, its default) and the extension.
//
// EXACTLY eight, because a looser count passes ordinary words:
// `settings-overview-panel.js` satisfies eight-or-more. A bundler bump that
// changes the length fails here, and the fix is this pattern.
//
// It is only a SHAPE: nothing here can recompute the bundler's hash, so a
// fixed name ending in a dash and eight such characters (`use-keyboard.js`)
// passes. Every way a hash has actually gone missing names the file after its
// module alone (`index.js`, `react.js`), and fails.
var hashedName = regexp.MustCompile(`^.+-[A-Za-z0-9_-]{8}\.[A-Za-z0-9]+$`)

// TestEveryFileUnderAssetsIsContentHashed holds the built tree to the promise
// the server makes about it.
//
// internal/api/dashboard.go serves everything under dashboard/assets/ as
// `public, max-age=31536000, immutable`, which is correct only because the
// build names every file there `<name>-<hash>.<ext>`, so different bytes are a
// different URL. A file without a hash (an emitFile with a fixed name, a
// `public/assets/` directory Vite copies verbatim, a `chunkFileNames` that
// dropped `[hash]`) would be pinned in every browser for a year.
//
// It reads what the BINARY embeds and checks the server's answer for each
// file as well as its name.
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
// and no inline script, style or event-handler attribute. A browser enforces it
// with only a console violation, so an inlined theme script, critical-CSS block
// or CDN font would serve a blank page with every other test green.
// TestTheBootScreenPaintsBeforeTheAppsSheet holds the shell to the order a slow
// connection needs: a stylesheet in the head blocks every paint until it
// arrives, so the app's sheet ahead of the boot screen leaves a blank page.
func TestTheBootScreenPaintsBeforeTheAppsSheet(t *testing.T) {
	t.Parallel()
	shell := string(mustFetch(t, embeddedDashboard.app, "/dashboard", "text/html"))
	head, body, ok := strings.Cut(shell, "</head>")
	if !ok {
		t.Fatalf("the shell has no head:\n%s", shell)
	}
	if strings.Contains(head, `rel="stylesheet"`) && regexp.MustCompile(`href="[^"]*/assets/[^"]+\.css"`).MatchString(head) {
		t.Errorf("the app's sheet is in the head, where it holds back the boot screen:\n%s", head)
	}
	if !strings.Contains(head, `href="/static/dashboard/boot.css"`) {
		t.Errorf("the head names no boot sheet, so the boot screen is unstyled until the app's arrives:\n%s", head)
	}
	root := strings.Index(body, `id="root"`)
	sheet := regexp.MustCompile(`href="[^"]*/assets/[^"]+\.css"`).FindStringIndex(body)
	if root < 0 || sheet == nil || sheet[0] < root {
		t.Errorf("the app's sheet must follow the root element, so nothing above it waits for it:\n%s", body)
	}
	mustFetch(t, embeddedDashboard.app, "/static/dashboard/boot.css", "text/css")
}

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
// The baseline's `:focus-visible` rule and a component's `.crewlet-btn` rule
// each count as one class, so whichever is written later wins every property
// they share; written last, the baseline's `border-radius` squares off every
// button, input and dialog a keyboard user tabs to. Only the built artifact
// shows the order (the bundler's). This holds main.tsx's arrangement: the
// kit's token sheets above every module import, the components' sheets with
// their modules, this application's sheets last.
func TestTheDesignSystemCascadesInOrder(t *testing.T) {
	t.Parallel()
	a := embeddedDashboard.app
	shell := mustFetch(t, a, "/dashboard", "text/html")

	sheets := 0
	for _, m := range staticRef.FindAllSubmatch(shell, -1) {
		// THE BOOT SCREEN'S SHEET STANDS ALONE, ahead of the design system on purpose
		// (TestTheBootScreenPaintsBeforeTheAppsSheet), so the cascade is the app's sheet.
		if !strings.HasSuffix(string(m[1]), ".css") || !strings.Contains(string(m[1]), "/assets/") {
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
// A uilet component imports its own stylesheet as a side effect of its
// module, so one reached only from a workspace takes its sheet into that
// workspace's chunk, appended when the chunk loads, AFTER the dashboard's own.
// Every one-class tie the dashboard's rules win against a component's is then
// lost from the first open of that workspace on, which the source and a
// first-paint screenshot cannot show. vite.config.ts's `designSystemSheet`
// prevents it: the whole set in one sheet above ours, and every per-component
// import answered empty.
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
	// exist only to go with it. A bare `currency:` is data (a growth account's
	// currency), which the source scan admits only in the growth modules.
	{"asks Intl for its currency style", regexp.MustCompile("[\"'`]currency[\"'`]")},
	{"passes Intl a currency option", regexp.MustCompile(`\bcurrency(?:Display|Sign)\s*:`)},
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
// matches inside it, each one of the two carve-outs rule 19 of
// docs/reference/dashboard-design.md makes: the design system's sheet model
// (@crewlethq/ui SheetModel) drawing an artifact sheet's own cells in the
// format the author declared, and the design system's Money drawing a growth
// integration's vendor money, the one way the dashboard draws an amount.
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
	// Money's formatter: Intl's currency style for an amount the kit has
	// already rounded exactly, its display option the caller's or a symbol.
	regexp.MustCompile(`new Intl\.NumberFormat\(\w+,\{style:` + quoted("currency") + `,currency:\w+,` +
		`currencyDisplay:\w+\.currencyDisplay\?\?` + quoted("symbol") + `[,}]`),
	// Money's ISO 4217 table: every code with a minor unit, a hundred and
	// more, in one string. A shorter run of codes is a screen naming them.
	regexp.MustCompile("[\"'`](?:[A-Z]{3} ){100,}[A-Z]{3}[\"'`]"),
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
// (regexlit.Required), by index. Every form but the field name's opens with
// a class or `\b`, which leaves the regexp package no prefix to jump to, so
// each ran over every byte of every module; a module that holds nothing a form
// needs is not handed to it, which cannot change what it finds. The big
// chunks hold most of what the forms need, so this skips the small ones.
var priceFormNeeds = func() []regexlit.Prefilter {
	out := make([]regexlit.Prefilter, len(priceForms))
	for i, form := range priceForms {
		out[i] = regexlit.Required(form.re)
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
// A price is on the wire only where reported (a subscription harness); beside
// a token count covering all calls it reads as the company's spend and is a
// fraction of it, so the client declares and draws none. money.test.tsx holds
// the source; this holds the artefact, which can carry what the source no
// longer does. It scans every script of the TestTheShellLoadsFromTheBinary
// crawl, lazy chunks included; protocol.js (static/protocol) is served by nobody.
func TestTheDashboardRendersNoPrice(t *testing.T) {
	t.Parallel()
	c := embeddedDashboard.crawl
	// A crawl that lost a file would scan less than the engine serves and
	// call it clean. TestTheShellLoadsFromTheBinary names each problem; this
	// only refuses to vouch for a bundle it could not read whole.
	if len(c.problems) > 0 {
		t.Fatalf("the crawl could not read %d of the files the shell reaches, so a "+
			"price in one of them would go unseen; TestTheShellLoadsFromTheBinary names them",
			len(c.problems))
	}
	modules := map[string][]byte{}
	for _, f := range c.ofKind(".js") {
		modules[f.url] = f.body
	}
	// A FLOOR: the entry and the React chunk the build splits out. A crawl
	// that reached nothing would otherwise pass.
	if len(modules) < 2 {
		t.Fatalf("scanned %d modules; the entry and the React chunk are two", len(modules))
	}
	for url, body := range modules {
		for _, p := range pricesInModule(body) {
			t.Errorf("%s %s: …%s… — the dashboard draws tokens for what agents spend, "+
				"and money only through the kit's Money (rule 19 in "+
				"docs/reference/dashboard-design.md); remove it from dashboard/src, "+
				"then `make dashboard`", url, p.what, p.near)
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
		// Money's admitted spans admit only themselves.
		{"a currency format with its own display", "new Intl.NumberFormat(n,{style:`currency`,currency:a,currencyDisplay:`code`})",
			"asks Intl for its currency style"},
		{"a display option outside Money's props", "f({micros:e,currencyDisplay:`code`})", "passes Intl a currency option"},
		{"a short run of codes", "var c=`USD EUR GBP JPY`.split(` `);", "names a currency by its code"},
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
		// The design system's Money, as the build writes it today: its
		// formatter and its ISO table, and a growth account's currency
		// carried as data.
		{"Money's formatter", "s=new Intl.NumberFormat(n,{style:`currency`,currency:a,currencyDisplay:r.currencyDisplay??`symbol`,signDisplay:r.signDisplay??`auto`})"},
		{"Money's ISO table", "Zt=new Set(`" + strings.Repeat("AED ", 60) + "EUR GBP USD " + strings.Repeat("ZAR ", 60) + "ZWL`.split(` `))"},
		{"an account's currency as data", "t.push({account:e.id,currency:e.currency,per_change:n})"},
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

// unembeddedTrees are the directories under static/ that the dashboard's build
// writes and the binary deliberately does not carry, each with why: the one
// exception TestEveryStaticFileIsInTheBinary makes, holding each to the
// opposite rule, that none of its files is in the binary.
//
// Declared here rather than moved out of static/, because static/ is where
// every build target of dashboard/ is committed: `make dashboard` writes it,
// `make dashboard-check` judges it, and .gitattributes keeps it out of text
// merges. The embed takes the targets a browser asks for; a target that is
// build output and no asset stays beside them, and is named here.
var unembeddedTrees = map[string]string{
	"protocol": "the wire protocol alone (dashboard/vite.protocol.config.ts), which internal/e2e's " +
		"replay reads from disk and no page loads: embedded, it was 124 KiB of a test's module in " +
		"every engine binary and image",
}

// TestEveryStaticFileIsInTheBinary guards the embed pattern against the tree.
//
// The pattern is a LIST, and a list is a thing that goes stale. It named
// `dashboard` and nothing else once, so the two icons one directory above it —
// the favicon and the sidebar brand, both asked for by the shell — were simply
// absent from the binary. Every module and stylesheet served perfectly; the
// page just had no logo.
//
// A tree [unembeddedTrees] declares is held the other way, since a pattern
// that took it would put it back in every binary. The declaration is exact in
// both directions: an entry whose tree holds no file excuses nothing today and
// would let the next file to land there pass unread, so it is deleted.
func TestEveryStaticFileIsInTheBinary(t *testing.T) {
	t.Parallel()
	static := staticDir(t)
	embedded := embeddedTree(t)
	unembedded := map[string]int{}
	err := sourcetree.Walk(static, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".go") {
			return err
		}
		rel, err := filepath.Rel(static, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		got, ok := embedded[rel]
		if tree, _, below := strings.Cut(rel, "/"); below && unembeddedTrees[tree] != "" {
			unembedded[tree]++
			if ok {
				t.Errorf("%s is in the binary, and nothing under static/%s/ is to be: %s; take it out "+
					"of the embed pattern in static/static.go", rel, tree, unembeddedTrees[tree])
			}
			return nil
		}
		if !ok {
			t.Errorf("%s is in %s but not in the binary; add it to the embed "+
				"pattern in static/static.go", rel, static)
			return nil
		}
		want, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: %d bytes embedded, %d on disk", rel, len(got), len(want))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", static, err)
	}
	for tree, why := range unembeddedTrees {
		if unembedded[tree] == 0 {
			t.Errorf("unembeddedTrees excuses static/%s/, which holds no file: delete the entry. It was: %s",
				tree, why)
		}
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
// docs/reference/dashboard-design.md ("How it is built") quotes these values,
// and TestTheDesignReferenceQuotesTheBudgets holds its table to them.
//
// Script and stylesheet sizes are what the engine SENDS a browser that asks
// for gzip, read off its own answer (servedSize): the file compressed at
// gzip.BestCompression (internal/api/dashboard.go), or as it is where that is
// not smaller. A raw count would measure whitespace the wire never carries,
// and one recomputed here would agree with the server only while kept in step
// by hand.
const (
	// initialBudget is the entry, its static import graph and the stylesheet
	// the shell links: everything a reader downloads before the first screen
	// can draw. It began at 300 KiB, under two seconds on a 1.5 Mbit/s link
	// (the slow end of a phone tether), and has moved up with each kit release
	// and frame feature; the last measure was 444.0 KiB, about 2.43 seconds,
	// on kit 0.188.0, whose one sheet carries the fleet primitives' rules.
	// The one sheet carries every design-system rule by rule (see
	// TestNoLazyStylesheetCarriesTheDesignSystem), so kit releases grow it even
	// when no screen joins the entry. A raise names the measured size and its
	// cause in its commit; a screen's module in the entry is the failure to fix
	// rather than raise for.
	initialBudget = 445 << 10
	// lazyChunkBudget holds each chunk the page fetches later — a workspace,
	// the org builder, a stylesheet a preload list names. A chunk is what a
	// reader waits for on the first click into a workspace the idle prefetch
	// has not reached yet, so it got half the initial budget, set when that
	// was 300 KiB, and stayed put as the initial budget grew. The largest,
	// Settings, measured about 60 KB when this was set.
	lazyChunkBudget = 150 << 10
	// fontBudget is every face the build embeds, counted as served: woff2 is
	// compressed already and the engine never recompresses it. The four Geist
	// and Geist Mono faces (latin and latin-ext) measure 83,736 bytes, so 90 KB
	// is the four of them and room for the design system to re-cut them — and
	// a FIFTH face, which every text-bearing screen would fetch, does not fit.
	fontBudget = 90_000
	// treeBudget is the whole of static/dashboard, raw, which every engine
	// binary and image carries whether or not anybody opens the page. It began
	// at 3 MiB over a 2.3 MB baseline and is set so a bundle that doubled the
	// baseline is still refused; screens fetched only when opened (the rich
	// editor, the brand and folder screens, the assistant's panel, Gitlet) and
	// kit releases account for its growth, and the last measure was 7483.3 KiB,
	// with each integration tab and its settings form given an address. A
	// raise names the measured size and its cause in its commit.
	treeBudget = 5<<20 + 2364<<10
)

// TestTheDashboardFitsItsBudget holds the built dashboard to the four numbers
// above, measured over what the server actually sends: the crawl
// TestTheShellLoadsFromTheBinary makes, partitioned by initialLoad into what a
// reader waits for before the first paint and what a screen fetches later.
//
// Nothing else notices a bundle growing. ONE static import of a screen's
// module from the frame pulls the whole workspace back into the entry with
// every other test green, and a design-system bump that doubled its sheet or
// added a face rides in the same way. A failure lists the files by size,
// largest first, because the fix is almost always in the biggest one.
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

// TestGitletIsThreeChunks holds Gitlet to three lazy chunks, by address (app/lazyScreen.ts): its
// section pages, a repository's and a change's, each named by Rolldown after its index.ts's
// directory. One that folded back would grow the first click into Gitlet.
func TestGitletIsThreeChunks(t *testing.T) {
	t.Parallel()
	c := embeddedDashboard.crawl
	initial := initialLoad(c)
	named := regexp.MustCompile(`^/static/dashboard/assets/(gitlet|repo|change)-[A-Za-z0-9_-]{8}\.js$`)
	found := map[string][]string{}
	for _, f := range c.files {
		m := named.FindStringSubmatch(f.url)
		if m == nil {
			continue
		}
		found[m[1]] = append(found[m[1]], f.url)
		if initial[f.url] {
			t.Errorf("%s is in the initial load; Gitlet's chunks are fetched by its screens", f.url)
		}
		t.Logf("%s: %s gzipped", f.url, kib(servedSize(t, embeddedDashboard.app, f.url)))
	}
	for _, chunk := range []string{"gitlet", "repo", "change"} {
		if len(found[chunk]) != 1 {
			t.Errorf("the crawl reached %d %s-*.js chunks, want exactly one: %v",
				len(found[chunk]), chunk, found[chunk])
		}
	}
}

// TestTheDesignReferenceQuotesTheBudgets holds the design reference's budget
// table (docs/reference/dashboard-design.md, "The bundle has a budget") to the
// four constants above: a row for each and none other, each quoting its
// constant in the unit the row is written in. The page is where a reader
// sizing a deployment's first paint learns the budget, and it drifts unless
// something reads it.
//
// Mutation: move initialBudget and leave the page, and its row quotes the old
// value; drop a row, and the table has three.
func TestTheDesignReferenceQuotesTheBudgets(t *testing.T) {
	t.Parallel()
	page := filepath.Join(sourcetree.Root(t), "docs", "reference", "dashboard-design.md")
	raw, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	// Each row by the words its first cell opens with, and the budget it
	// quotes in the unit the page writes it in.
	want := []struct{ row, budget string }{
		{"The initial load:", quotedBudget(initialBudget, 1<<10, "KiB")},
		{"Each chunk fetched later:", quotedBudget(lazyChunkBudget, 1<<10, "KiB")},
		{"Every font face", quotedBudget(fontBudget, 1000, "KB")},
		{"The whole of `static/dashboard`", quotedBudget(treeBudget, 1<<10, "KiB")},
	}
	rows := budgetTable(t, string(raw))
	if len(rows) != len(want) {
		t.Errorf("%s: the budget table has %d rows, want one for each of the %d budgets",
			page, len(rows), len(want))
	}
	for _, w := range want {
		found := false
		for _, r := range rows {
			if !strings.HasPrefix(r[0], w.row) {
				continue
			}
			found = true
			if r[1] != w.budget {
				t.Errorf("%s: the budget table quotes %q for %q, want its constant, %q",
					page, r[1], w.row, w.budget)
			}
		}
		if !found {
			t.Errorf("%s: the budget table has no row opening %q", page, w.row)
		}
	}
}

// quotedBudget is a budget of n bytes as the design reference writes it: in
// units of size bytes, the unit named.
func quotedBudget(n, size int, unit string) string {
	return fmt.Sprintf("%g %s", float64(n)/float64(size), unit)
}

// budgetTable is the rows of the design reference's table under "The bundle
// has a budget", each as its four cells trimmed.
func budgetTable(t *testing.T, page string) [][]string {
	t.Helper()
	const title = "**The bundle has a budget, measured as the engine sends it.**"
	start := strings.Index(page, title)
	if start < 0 {
		t.Fatalf("the design reference has no paragraph %s", title)
	}
	var rows [][]string
	inTable := false
	for _, line := range strings.Split(page[start:], "\n") {
		if !strings.HasPrefix(line, "|") {
			if inTable {
				break
			}
			continue
		}
		if !inTable {
			inTable = true
			continue // the header
		}
		if strings.HasPrefix(line, "|---") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 4 {
			t.Fatalf("the budget table's row %q has %d cells, want 4", line, len(cells))
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		rows = append(rows, cells)
	}
	if !inTable {
		t.Fatalf("the design reference's paragraph %s is followed by no table", title)
	}
	return rows
}

// kib renders a byte count the way the budgets are written.
func kib(n int) string {
	return fmt.Sprintf("%.1f KiB", float64(n)/1024)
}
