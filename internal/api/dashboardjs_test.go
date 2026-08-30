package api_test

// GATE G5 — the dashboard loads from this binary and passes its own suite.
//
// The dashboard was not rewritten. It is a zero-build ES-module app that
// talks to the server over one WebSocket, and the wire protocol it expects is
// FROZEN (adrs/502): the client ships unchanged and wins any
// disagreement about what a frame contains. That makes its ~350-assertion
// JavaScript suite the compatibility reference for the client's half of the
// contract — the only place the shape the browser actually parses is written
// down and checked.
//
// So it runs here, from Go, against THE TREE THIS BINARY EMBEDS. There is
// only one tree now: through the rewrite this was a copy of the Python one,
// held byte-identical by a test that retired with it.
//
// Two things are asserted, and they are different things:
//
//  1. The suites pass against static/dashboard, the tree package static
//     embeds (TestTheDashboardPassesItsOwnSuite).
//  2. Every asset the shell needs is reachable over HTTP from the server
//     itself, with a type a browser will accept
//     (TestTheShellLoadsFromTheBinary).
//
// (1) without (2) proves the bytes are present and nothing about the routes
// that hand them out — an ES module served as text/plain is REFUSED by the
// module loader, and the page fails with a MIME error rather than a missing
// file.

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/static"
)

// Paths, relative to this package's directory, which is where `go test` runs.
//
// The module root is the repository root, so two levels up reaches both the
// embedded tree and the suites that test it. There is ONE dashboard tree
// now: the Python copy this used to be held byte-identical to is gone, and
// with it the test that compared them.
const (
	// The directory package static embeds, on disk.
	staticDir = "../../static"
	// The dashboard sources embedded by package static, and the tree the
	// suites read by default.
	servedTree = staticDir + "/dashboard"
	// The JavaScript suites. Not Go, so not in a package directory — they
	// are a few hundred assertions over a vendored DOM, run from here
	// because this is what serves the tree they test.
	suiteDir = "../../tests/dashboard/js"
)

// suiteTimeout caps one suite's wall clock.
//
// Each is a few hundred pure-function assertions over a vendored DOM and
// finishes in well under a second; this exists so a hung node fails the build
// instead of stalling it. It matches the Python wrapper's cap deliberately —
// two runners with different patience would disagree about which suite is
// "slow", and the answer would depend on who ran it.
const suiteTimeout = 60 * time.Second

// dashboardRootEnv is how a suite is told which tree to test. Its one
// resolution point is tests/dashboard/js/dashboardRoot.mjs.
const dashboardRootEnv = "CREWLET_DASHBOARD_ROOT"

// inCI reports whether this is a CI run, where a skip is not a pass.
//
// Every CI provider worth the name sets this, GitHub Actions included.
func inCI() bool {
	switch strings.ToLower(os.Getenv("CI")) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// nodeBinary finds node, or explains its absence the right way for the run.
//
// Locally a missing node is somebody else's problem and the rest of the Go
// suite still runs. In CI it is a red build: this is the dashboard's ONLY
// coverage, so letting it go quiet would retire ~350 assertions with a green
// tick — the failure mode CONTRIBUTING names ("a skip is not a pass").
func nodeBinary(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err == nil {
		return node
	}
	if inCI() {
		t.Fatalf("node is not on PATH, so every dashboard suite would skip and "+
			"the build would still pass; the `test` job in "+
			".github/workflows/ci.yml must install it: %v", err)
	}
	t.Skip("node is not installed; the dashboard's JS suites need it")
	return ""
}

// suites are the suite files, DISCOVERED rather than listed, so a new
// *.test.mjs runs without anyone remembering to register it here.
func suites(t *testing.T) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(suiteDir, "*.test.mjs"))
	if err != nil {
		t.Fatalf("globbing %s: %v", suiteDir, err)
	}
	if len(found) == 0 {
		// Guard the glob: an empty run must not look like a passing one.
		t.Fatalf("no *.test.mjs suites found in %s", suiteDir)
	}
	sort.Strings(found)
	return found
}

func TestTheDashboardPassesItsOwnSuite(t *testing.T) {
	t.Parallel()
	node := nodeBinary(t)

	// Absolute, because the suite resolves a relative root against its own
	// directory (so a hand-run suite and a wrapper-run one agree), and this
	// one is relative to the Go package instead.
	root, err := filepath.Abs(servedTree)
	if err != nil {
		t.Fatalf("resolving the served tree: %v", err)
	}

	for _, suite := range suites(t) {
		name := strings.TrimSuffix(filepath.Base(suite), ".test.mjs")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), suiteTimeout)
			defer cancel()

			cmd := exec.CommandContext(ctx, node, suite)
			cmd.Env = append(os.Environ(), dashboardRootEnv+"="+root)
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("%s did not finish within %s:\n%s", name, suiteTimeout, out)
			}
			if err != nil {
				t.Fatalf("%s failed against %s:\n%s", name, root, out)
			}
			// The harness writes its count LAST, after the final test has
			// actually finished. Its absence means the process exited zero
			// without running anything, which an exit code cannot tell apart
			// from a clean run.
			if !bytes.Contains(out, []byte("passed")) {
				t.Fatalf("%s produced no result line:\n%s", name, out)
			}
		})
	}
}

func TestEveryStaticFileIsInTheBinary(t *testing.T) {
	t.Parallel()
	// The embed pattern is a LIST, and a list is a thing that goes stale. It
	// named `dashboard` and nothing else, so the two icons one directory above
	// it — the favicon and the sidebar brand, both asked for by the shell —
	// were simply absent from the binary. Every module and stylesheet served
	// perfectly; the page just had no logo.
	//
	// So the pattern is checked against the directory rather than trusted:
	// anything on disk that a request could name must be in the FS, with the
	// same bytes. Go sources are excluded because they are the package, not
	// its assets.
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

// staticRef matches a URL the shell or a stylesheet asks the server for.
// Anything else — the font CDN, a data: URI — is not this binary's to serve.
var staticRef = regexp.MustCompile(`["'(](/static/[^"')\s]+)`)

// moduleImport matches a relative ES-module specifier: the `from "./x.js"` and
// bare `import "./x.js"` forms, plus the dynamic `import("./x.js")` the views
// are loaded with.
var moduleImport = regexp.MustCompile(`(?:from|import)\s*\(?\s*["'](\.[^"']+)["']`)

func TestTheShellLoadsFromTheBinary(t *testing.T) {
	t.Parallel()
	// What a browser does, done by the test: fetch /dashboard, then fetch
	// everything it names, then everything THOSE name, all from the server
	// rather than from disk. A module missing from the embed pattern, or
	// served as the wrong type, kills the whole app — the module graph is
	// loaded eagerly and one failed import takes the page with it.
	a := newApp(t, api.Options{})

	body := mustFetch(t, a, "/dashboard", "text/html")
	queue := []string{}
	for _, m := range staticRef.FindAllStringSubmatch(string(body), -1) {
		queue = append(queue, m[1])
	}
	if len(queue) < 10 {
		t.Fatalf("the shell asked for only %d assets; it should name a "+
			"stylesheet per room and an entry module", len(queue))
	}

	done := map[string]bool{"/dashboard": true}
	modules, sheets := 0, 0
	for len(queue) > 0 {
		url := queue[0]
		queue = queue[1:]
		if done[url] {
			continue
		}
		done[url] = true

		want, known := map[string]string{
			".js":  "text/javascript",
			".css": "text/css",
			".svg": "image/svg+xml",
			".png": "image/png",
		}[strings.ToLower(path.Ext(url))]
		if !known {
			t.Errorf("%s: the shell asked for a kind this test does not know "+
				"how to check; teach it rather than dropping the asset", url)
			continue
		}
		data := mustFetch(t, a, url, want)

		switch path.Ext(url) {
		case ".js":
			modules++
			for _, m := range moduleImport.FindAllStringSubmatch(string(data), -1) {
				queue = append(queue, path.Join(path.Dir(url), m[1]))
			}
		case ".css":
			sheets++
			for _, m := range staticRef.FindAllStringSubmatch(string(data), -1) {
				queue = append(queue, m[1])
			}
		}
	}

	// Counts rather than an exact manifest: a new view module must not need a
	// test edit, but the graph collapsing to the entry module alone — which is
	// what a broken specifier regex looks like — must fail.
	if modules < 30 {
		t.Errorf("only %d modules reached from the shell; the dashboard has "+
			"more than that, so the graph walk stopped early", modules)
	}
	if sheets < 10 {
		t.Errorf("only %d stylesheets reached from the shell; there is one per "+
			"room plus the shared four", sheets)
	}
}

func TestTheFaviconServesFromTheBinary(t *testing.T) {
	t.Parallel()
	// /favicon.ico is asked for UNPROMPTED — by browsers on a bare origin
	// hit, by bookmark managers, by anything that never parses the shell —
	// so no graph walk from /dashboard can prove it serves. The route
	// existed and answered 404 for every one of those requests, because the
	// file it reads (dashboard/favicon.ico) was simply absent from the tree:
	// the serving tests passed on a MapFS that had it, and nothing checked
	// the real one.
	a := newApp(t, api.Options{})
	mustFetch(t, a, "/favicon.ico", "image/x-icon")
}

// mustFetch gets one asset from the server and returns its bytes, failing with
// the reason a browser would have given.
func mustFetch(t *testing.T, a *api.App, url, wantType string) []byte {
	t.Helper()
	res := fetch(t, a, url, nil)
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d (the shell needs it and the binary does not have it)",
			url, res.StatusCode)
	}
	if kind := res.Header.Get("Content-Type"); !strings.HasPrefix(kind, wantType) {
		t.Fatalf("%s: served as %q, which a browser refuses; wanted %s",
			url, kind, wantType)
	}
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("%s: reading the body: %v", url, err)
	}
	if len(data) == 0 {
		t.Fatalf("%s: served empty", url)
	}
	return data
}
