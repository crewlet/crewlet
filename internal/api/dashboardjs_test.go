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
// committed tree is a coherent application, and that every byte of it is
// reachable over HTTP from the server itself, with a content type a browser
// will accept.
//
// Those are different failures. A tree can be perfectly in step with its source
// and still be unservable — an ES module served as text/plain is REFUSED by the
// module loader, and the page then fails with a MIME error rather than a
// missing file, which sends a reader looking for the wrong problem.
//
// The dashboard's own ~200 assertions (its protocol, its router, its ordering
// rules, and the measured contrast of every colour token in both themes) run
// under Vitest — `npm test` in dashboard/, wired into `make test-dashboard` and
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
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
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

	// The shell must name a hashed entry script and a hashed stylesheet. A
	// build that emitted neither is a build whose output directory was not
	// cleaned; one that emitted an unhashed name is a build whose config lost
	// its cache-busting, which puts a stale module in every reader's browser.
	entry := regexp.MustCompile(`src="(/static/dashboard/assets/[^"]+\.js)"`)
	sheet := regexp.MustCompile(`href="(/static/dashboard/assets/[^"]+\.css)"`)
	if !entry.Match(shell) {
		t.Errorf("the shell names no hashed entry module:\n%s", shell)
	}
	if !sheet.Match(shell) {
		t.Errorf("the shell names no hashed stylesheet:\n%s", shell)
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
	for _, face := range []string{
		"fonts/inter-latin.woff2",
		"fonts/inter-latin-ext.woff2",
		"fonts/jetbrains-mono-latin.woff2",
		"fonts/jetbrains-mono-latin-ext.woff2",
		// The licence travels with the files it covers.
		"fonts/OFL.txt",
	} {
		if _, err := os.Stat(filepath.Join(servedTree, face)); err != nil {
			t.Errorf("%s is missing from the built tree: %v", face, err)
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

// staticRef matches a URL the shell or a stylesheet asks the server for.
var staticRef = regexp.MustCompile(`["'(](/static/[^"')\s]+)`)

// TestTheShellLoadsFromTheBinary does what a browser does.
//
// Fetch /dashboard, then fetch everything it names, then everything THOSE name
// — all from the server rather than from disk. An asset missing from the embed
// pattern, or served as the wrong type, takes the whole page with it.
func TestTheShellLoadsFromTheBinary(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{})

	body := mustFetch(t, a, "/dashboard", "text/html")
	queue := []string{}
	for _, m := range staticRef.FindAllStringSubmatch(string(body), -1) {
		queue = append(queue, m[1])
	}
	// A bundled shell names few assets by design — an entry module, a vendor
	// chunk, a stylesheet, an icon. The floor is what distinguishes that from
	// a shell that names NOTHING, which is what a build with a broken `base`
	// produces: relative URLs that resolve against whichever of `/` or
	// `/dashboard` the reader arrived at.
	if len(queue) < 2 {
		t.Fatalf("the shell asked for %d assets; it should name at least an "+
			"entry module and a stylesheet", len(queue))
	}

	done := map[string]bool{"/dashboard": true}
	scripts, sheets, fonts := 0, 0, 0
	for len(queue) > 0 {
		url := queue[0]
		queue = queue[1:]
		if done[url] {
			continue
		}
		done[url] = true

		want, known := map[string]string{
			".js":    "text/javascript",
			".css":   "text/css",
			".svg":   "image/svg+xml",
			".png":   "image/png",
			".ico":   "image/x-icon",
			".woff2": "font/woff2",
		}[strings.ToLower(path.Ext(url))]
		if !known {
			t.Errorf("%s: the shell asked for a kind this test does not know "+
				"how to check; teach it rather than dropping the asset", url)
			continue
		}
		data := mustFetch(t, a, url, want)

		switch path.Ext(url) {
		case ".js":
			scripts++
		case ".css":
			sheets++
			// A stylesheet's own references — the font faces above all,
			// which are the assets most likely to be left out of a commit.
			for _, m := range staticRef.FindAllStringSubmatch(string(data), -1) {
				queue = append(queue, m[1])
			}
		case ".woff2":
			fonts++
		}
	}

	if scripts < 1 {
		t.Errorf("no script reached from the shell")
	}
	if sheets < 1 {
		t.Errorf("no stylesheet reached from the shell")
	}
	// Every face the stylesheet declares has to be servable. One that is not
	// fails silently in a browser — the text simply renders in the fallback.
	if fonts < 4 {
		t.Errorf("only %d font faces reached from the stylesheet, want 4", fonts)
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
