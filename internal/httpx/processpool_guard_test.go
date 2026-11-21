package httpx_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// TestNoClientSitsOnTheProcessGlobalPool fails the build when any package
// under internal/ and cmd/ except httpx names http.DefaultTransport or
// http.DefaultClient, or calls http.Get, Head, Post or PostForm (the same
// client). The shared pool is a performance bug in production
// (MaxIdleConnsPerHost 2 for every client) and a CORRECTNESS bug in tests:
// httptest.Server.Close calls CloseIdleConnections on the default transport
// for every server in the binary, failing a request the server answered.
// The match is a selector resolved against the file's net/http import, so doc
// comments and renamed imports are handled. Not seen: a client handed to an
// SDK through an unnameable field, reflection, or an &http.Client{} with no
// Transport (legitimate if it never requests). If the rule goes, DELETE this.
func TestNoClientSitsOnTheProcessGlobalPool(t *testing.T) {
	t.Parallel()

	root := sourcetree.Root(t)
	found := walkForGlobalPool(t, root)

	if found.files == 0 {
		t.Fatal("parsed no source files importing net/http — this guard was " +
			"certifying nothing. Check the module root and the internal/ and cmd/ globs")
	}
	// A guard asserting an ABSENCE passes identically when the thing is
	// absent and when the matcher has gone inert. This is what tells those
	// apart: the verdicts below are known, so a matcher that stopped
	// recognising a selector fails HERE rather than certifying a clean tree.
	for _, c := range []struct {
		src  string
		want bool
	}{
		{`package p; import "net/http"; var _ = http.DefaultTransport`, true},
		{`package p; import "net/http"; var _ = http.DefaultClient`, true},
		{`package p; import "net/http"; func f() { http.Get("u") }`, true},
		{`package p; import "net/http"; func f() { http.Post("u", "", nil) }`, true},
		{`package p; import nh "net/http"; var _ = nh.DefaultTransport`, true},
		// Not net/http at all — a local variable, another package's
		// identically named symbol, and this package's own Transport.
		{`package p; var http struct{ DefaultTransport int }; var _ = http.DefaultTransport`, false},
		{`package p; import "example.com/http"; var _ = http.DefaultClient`, false},
		{`package p; import "net/http"; var _ = http.NewRequest`, false},
		{`package p; import "net/http"; func f(c *http.Client) { c.Get("u") }`, false},
	} {
		if got := matchesInSource(t, c.src); got != c.want {
			t.Errorf("control: matcher returned %v for %q, want %v", got, c.src, c.want)
		}
	}

	for _, v := range found.hits {
		if why, known := allowedGlobalPool[v.pkgPath]; known {
			_ = why
			continue
		}
		t.Errorf("%s: %s reaches for %s.\n"+
			"\tbuild the client through internal/httpx, or in a test through "+
			"internal/httpx/httpxtest — every httptest.Server.Close in the binary "+
			"sweeps the process-global pool, and a bodyless response is in it while "+
			"its caller is still waiting", v.pos, v.pkgPath, v.what)
	}

	// The allowance is exact in BOTH directions. An entry whose site has
	// been fixed must be deleted, or the list becomes somewhere a future
	// violation hides behind a stale excuse.
	seen := map[string]bool{}
	for _, v := range found.hits {
		seen[v.pkgPath] = true
	}
	for pkgPath, why := range allowedGlobalPool {
		if !seen[pkgPath] {
			t.Errorf("allowedGlobalPool still excuses %s, but the walk no longer "+
				"finds a use there. Delete the entry — it was: %s", pkgPath, why)
		}
	}

	t.Logf("scanned %d files importing net/http under internal/ and cmd/: "+
		"%d uses of the process-global pool, %d package(s) allowed",
		found.files, len(found.hits), len(allowedGlobalPool))
}

// allowedGlobalPool is the closed set of packages that may name the
// process-global pool, each with the reason. It is a ratchet, not a
// permission: nothing goes in without a reason that survives review, and an
// entry whose site is gone fails the test above.
var allowedGlobalPool = map[string]string{
	"httpxtest": "its own test asserts the failure by CALLING " +
		"http.DefaultTransport.CloseIdleConnections — that is what a neighbour's " +
		"t.Cleanup does, and reproducing it is the whole assertion",
}

type poolHit struct {
	pos     string
	pkgPath string
	what    string
}

type poolWalk struct {
	files int
	hits  []poolHit
}

// walkForGlobalPool parses every .go file under internal/ and cmd/ that
// imports net/http and collects the uses of its process-global client.
//
// BOTH halves of the tree, production and test: the test half carries the
// correctness bug.
//
// IMPORTS ARE READ FIRST and a file is parsed whole only if one is net/http,
// since every selector judged is on that import. They are PARSED, not
// searched as text (an import path can be spelled with escapes);
// parser.ImportsOnly stops at the first non-import declaration, and parsing
// the whole tree took ten seconds under the race detector.
func walkForGlobalPool(t *testing.T, root string) poolWalk {
	t.Helper()

	selfDir := filepath.Join(root, "internal", "httpx")
	fset := token.NewFileSet()
	var out poolWalk

	for _, tree := range []string{"internal", "cmd"} {
		err := sourcetree.Walk(filepath.Join(root, tree), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// testdata is not compiled.
				if filepath.Base(path) == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			// THIS package only, not its subtree: httpx builds the
			// shared transport by CLONING http.DefaultTransport, which
			// is the one legitimate naming of it in the tree. Skipping
			// the directory rather than the files would take
			// httpx/httpxtest with it, and that is precisely the
			// package whose own uses need watching.
			if filepath.Dir(path) == selfDir {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			imports, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
			if err != nil {
				t.Errorf("parse %s: %v", path, err)
				return nil
			}
			if _, ok := httpImportName(imports); !ok {
				return nil
			}
			file, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				t.Errorf("parse %s: %v", path, err)
				return nil
			}
			name, ok := httpImportName(file)
			if !ok {
				return nil
			}
			out.files++
			for _, use := range globalPoolUses(file, name) {
				out.hits = append(out.hits, poolHit{
					pos:     shortPos(root, fset.Position(use.Pos()).String()),
					pkgPath: file.Name.Name,
					what:    name + "." + use.Sel.Name,
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}
	return out
}

// THE WALK, ON A TREE WHOSE VERDICT IS KNOWN: a test file reaching for the
// pool under a renamed import, a production file importing net/http cleanly,
// this package's own files (which clone the pool, legitimately), and a file
// importing nothing of net/http whose body is not Go — so a walk that parsed
// past its imports would report it.
func TestThePoolWalkReadsEveryFileImportingNetHTTP(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for path, body := range map[string]string{
		"internal/p/p_test.go": "package p\n\nimport nh \"net/http\"\n\nvar _ = nh.DefaultClient\n",
		"cmd/q/q.go":           "package main\n\nimport \"net/http\"\n\nvar _ = http.NewRequest\n",
		"internal/httpx/httpx.go": "package httpx\n\nimport \"net/http\"\n\n" +
			"var _ = http.DefaultTransport\n",
		"internal/r/r.go": "package r\n\nimport \"strings\"\n\nthis is not Go\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	found := walkForGlobalPool(t, root)
	if found.files != 2 || len(found.hits) != 1 || found.hits[0].what != "nh.DefaultClient" ||
		found.hits[0].pkgPath != "p" {
		t.Errorf("files %d, hits %+v; want the two outside httpx, and p's nh.DefaultClient",
			found.files, found.hits)
	}
}

// httpImportName is the local name net/http is imported under, if it is.
func httpImportName(file *ast.File) (string, bool) {
	for _, imp := range file.Imports {
		if imp.Path == nil || imp.Path.Value != `"net/http"` {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				// A blank import names nothing; a dot import would
				// need identifier resolution rather than a
				// selector, and none exists in this tree.
				return "", false
			}
			return imp.Name.Name, true
		}
		return "http", true
	}
	return "", false
}

// globalPoolNames are the selectors that ARE the process-global pool.
//
// The four helpers are here because they are http.DefaultClient under another
// spelling: net/http's package-level Get, Head, Post and PostForm each call
// DefaultClient, so a test using them shares the pool without naming it.
var globalPoolNames = map[string]bool{
	"DefaultTransport": true, "DefaultClient": true,
	"Get": true, "Head": true, "Post": true, "PostForm": true,
}

// globalPoolUses finds every selector on the net/http import that names the
// process-global pool.
//
// It checks the QUALIFIER is the import's own name rather than any identifier
// spelled "http", so a local variable or another package's symbol is not a
// hit; it cannot tell a shadowed import apart, which no file in this tree
// does and which would be caught by the controls if one appeared.
func globalPoolUses(file *ast.File, name string) []*ast.SelectorExpr {
	var out []*ast.SelectorExpr
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != name || ident.Obj != nil {
			// ident.Obj != nil means the name resolves to something
			// declared in this file — a variable or parameter — so it
			// is not the import.
			return true
		}
		if globalPoolNames[sel.Sel.Name] {
			out = append(out, sel)
		}
		return true
	})
	return out
}

// matchesInSource runs the matcher over one snippet, for the controls.
func matchesInSource(t *testing.T, src string) bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "control.go", src, 0)
	if err != nil {
		t.Fatalf("parse control %q: %v", src, err)
	}
	name, ok := httpImportName(file)
	if !ok {
		return false
	}
	return len(globalPoolUses(file, name)) > 0
}

func shortPos(root, pos string) string {
	if rel, err := filepath.Rel(root, pos); err == nil {
		return rel
	}
	return pos
}
