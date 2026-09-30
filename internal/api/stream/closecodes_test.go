package stream_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE ENGINE AND THE DASHBOARD CLOSE A SOCKET WITH ONE SET OF NUMBERS.
//
// A 44xx close is the one thing the dashboard's socket branches on that no
// frame carries: 4401 reconnects (the browser may hold a newer cookie than the
// socket was opened with) and 4403 STOPS and tells the reader why, because
// reconnecting reaches the same person with the same access. The numbers were
// two literals, one per side, with nothing holding them together — so an
// engine that renumbered one would have a dashboard reconnecting for ever
// against a withdrawn grant, or stopping dead over a cookie that merely
// expired, with no test anywhere to say so.
//
// Read from the dashboard's SOURCE by declaration, and held in both
// directions: each dashboard constant equals the engine's, and every 4000-range
// close code the engine declares has a dashboard constant — a code the engine
// sends and the dashboard has no name for is a close it handles as a network
// blip.
func TestTheDashboardClosesOnTheEnginesCloseCodes(t *testing.T) {
	t.Parallel()
	dashboard := map[string]websocket.StatusCode{
		"CLOSE_UNAUTHENTICATED": stream.CloseUnauthenticated,
		"CLOSE_FORBIDDEN":       stream.CloseUnauthorized,
	}
	read := map[int]string{}
	for name, engine := range dashboard {
		body, err := clientsource.Declaration(clientsource.Tree(t),
			`(?m)^\s*(?:export\s+)?const\s+`+name+`\s*=\s*(\d+)\s*;`)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		value, err := strconv.Atoi(body)
		if err != nil {
			t.Fatalf("%s = %q, which is not a number", name, body)
		}
		if value != int(engine) {
			t.Errorf("the dashboard's %s is %d and the engine closes with %d: one "+
				"side was renumbered without the other", name, value, int(engine))
		}
		read[value] = name
	}

	for _, code := range engineApplicationCloseCodes(t) {
		if _, named := read[code]; !named {
			t.Errorf("the engine declares close code %d, which the dashboard has no "+
				"constant for — it would handle that close as a network blip", code)
		}
	}
}

// engineApplicationCloseCodes is every close code in the 4000-4999 range this
// package declares or converts to, read from its own source so a new one is
// covered without being listed here.
func engineApplicationCloseCodes(t *testing.T) []int {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	var codes []int
	application := func(lit ast.Expr) {
		basic, ok := lit.(*ast.BasicLit)
		if !ok || basic.Kind != token.INT {
			return
		}
		n, err := strconv.Atoi(basic.Value)
		if err == nil && n >= 4000 && n <= 4999 && !slices.Contains(codes, n) {
			codes = append(codes, n)
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.ValueSpec:
				// A constant of the close-code type — the declared form.
				if node.Type == nil || !isCloseCodeType(node.Type) {
					return true
				}
				for _, v := range node.Values {
					application(v)
				}
			case *ast.CallExpr:
				// A literal converted in place — `websocket.StatusCode(4409)`
				// — which a walk of the declarations alone would miss.
				if len(node.Args) == 1 && isCloseCodeType(node.Fun) {
					application(node.Args[0])
				}
			}
			return true
		})
	}
	if len(codes) == 0 {
		t.Fatal("the walk found no application close code in this package, so this " +
			"test could not fail")
	}
	return codes
}

// isCloseCodeType reports websocket.StatusCode, the type every close code is.
func isCloseCodeType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == "websocket" && sel.Sel.Name == "StatusCode"
}
