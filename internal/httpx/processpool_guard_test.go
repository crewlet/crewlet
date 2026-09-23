package httpx_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// TestNoClientSitsOnTheProcessGlobalPool fails the build when any package in
// this module reaches for net/http's process-global client or transport.
//
// This package's doc already says the rule twice — every outbound client is
// built here, and a round tripper that wraps another takes [Transport] as its
// base — and [httpxtest] says the third half of it, that a test double
// wrapping an httptest.Server takes THAT SERVER'S transport. All three were
// prose, and prose is what the tree already had: gitlab, mattermost, datadog's
// conformance helper and anthropic's own test each carried a paragraph about
// it, and seven packages sat on the shared pool anyway. internal/sandbox lost
// a CI run to it.
//
// # Why this fails the build rather than reporting
//
// In production the shared pool is a performance bug: MaxIdleConnsPerHost is
// 2 for every client in the process at once, which is the whole reason this
// package exists. In TESTS it is a CORRECTNESS bug, and that is the half
// nobody expects — httptest.Server.Close ends by calling CloseIdleConnections
// on http.DefaultTransport, for every server in the binary rather than its
// own, and a bodyless response is handed back to the idle pool BEFORE its
// caller has it. A close landing in that window fails a request the server
// answered, with "net/http: HTTP/1.x transport connection broken: http:
// CloseIdleConnections called". It is rare, it is load-dependent, and it is
// reported as somebody else's flake.
//
// # What it flags
//
// In every package under internal/ and cmd/ except this one:
//
//   - http.DefaultTransport and http.DefaultClient, named anywhere;
//   - http.Get, http.Head, http.Post and http.PostForm, which are
//     http.DefaultClient under another spelling.
//
// The subject of the match is a LANGUAGE construct — a selector resolved
// against the file's own import of net/http — rather than a line or a regexp
// over source text. So a doc comment naming http.DefaultTransport (several
// legitimately do, this file included) is not a hit, and a package that
// renames the import is still covered.
//
// # Coverage boundary
//
// It does not see a client handed to a vendor SDK through a field it cannot
// name — internal/api/mcpbridge's MCP client transport is one, where an unset
// HTTPClient means http.DefaultClient and no selector says so — nor a
// transport reached by reflection, nor an &http.Client{} whose Transport
// field is simply absent. That last one is deliberate: the zero value is
// legitimate for a client that never makes a request, and four of them exist.
// What closes those gaps is [httpxtest] being the obvious thing to reach for,
// not a matcher that tries to read intent.
//
// If this rule ever legitimately goes away, DELETE this guard rather than
// weakening it. A guard kept alive past its subject is how an allowance list
// quietly grows.
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
// BOTH halves of the tree, production and test, unlike the subject guard next
// door: there the failure mode differs in kind between them, and here it is
// the test half that carries the correctness bug.
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
			file, err := parser.ParseFile(fset, path, nil, 0)
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
