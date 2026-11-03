package auth

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE CORS LISTS ARE THE HEADERS THE ROUTES READ AND SET, and a header a route
// starts reading cannot be forgotten in them.
//
// A permitted origin's browser refuses the whole request at the preflight when
// it carries a header [corsHeaders] does not name, and hides from the page's
// script every response header [corsExposed] does not name — and the engine
// sees neither, because the browser fails after the answer arrives. The lists
// were three request headers and no exposed ones while `/config` read
// `If-Match`, `If-None-Match` and `X-Summary` and answered an `ETag` and a
// `Retry-After` a client writes back: a page on an allowed origin could not
// write `/config` at all, nor read the tag its next write needed.
//
// So this walks every handler's source under internal/api for the request
// headers it reads (`r.Header.Get`, `.Values`, and the same on an http.Header
// parameter) and the response headers it sets (`w.Header().Set`, `.Add`, and
// the same on a header bound from `w.Header()` or passed as an http.Header),
// and holds each one to a list: offered to a permitted origin, or withheld
// below for a stated reason. It runs BOTH WAYS — an entry on any list that no
// route reads or sets any more is stale and fails too.
//
// What it cannot see is what a library sets on a route's behalf —
// http.ServeContent's range headers on the dashboard's own files, the MCP
// transport's session headers on `/operator/mcp` and `/mcp/` — and the
// webhook package is skipped whole: every header read there is a vendor
// server's, on routes no page is ever offered.
//
// Mutation: drop a header from corsHeaders or corsExposed, or add one no route
// reads, and this fails naming it; respell a constant a route reads its header
// by and it fails on the new name.
func TestTheCORSListsAreTheHeadersTheRoutesReadAndSet(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	reads, sets := routeHeaders(t, root)

	// THE CONTROLS: a header known to be read and one known to be set, so a
	// walk that read nothing cannot pass.
	if len(reads[http.CanonicalHeaderKey("Authorization")]) == 0 ||
		len(sets[http.CanonicalHeaderKey("ETag")]) == 0 {
		t.Fatalf("control: the walk found no read of Authorization or no ETag set "+
			"(%d reads, %d sets), so it is reading nothing", len(reads), len(sets))
	}

	hold := func(side string, seen map[string][]string, offered []string,
		withheld map[string]string) {
		t.Helper()
		offer := map[string]bool{}
		for _, name := range offered {
			offer[http.CanonicalHeaderKey(name)] = true
		}
		withhold := map[string]bool{}
		for name := range withheld {
			withhold[http.CanonicalHeaderKey(name)] = true
		}
		for name, where := range seen {
			if !offer[name] && !withhold[name] {
				t.Errorf("%s %s (%s) is on neither CORS list: offer it to a "+
					"permitted origin in cors.go, or withhold it in this gate "+
					"saying why", side, name, strings.Join(where, ", "))
			}
			if offer[name] && withhold[name] {
				t.Errorf("%s %s is both offered and withheld", side, name)
			}
		}
		for name := range offer {
			if len(seen[name]) == 0 {
				t.Errorf("%s %s is offered to a permitted origin and no route "+
					"%s it any more", side, name, map[string]string{
					"request header": "reads", "response header": "sets"}[side])
			}
		}
		for name := range withhold {
			if len(seen[name]) == 0 {
				t.Errorf("%s %s is withheld here and no route touches it any "+
					"more: drop it", side, name)
			}
		}
	}
	hold("request header", reads, corsHeaders, withheldRequest)
	hold("response header", sets, corsExposed, unexposedResponse)
}

// withheldRequest are the request headers a route reads that a permitted
// origin is NOT offered, each with why.
var withheldRequest = map[string]string{
	ReaderHeader: "same-origin only: the dashboard's alone, see corsHeaders",
	httpjson.EngineHeader: "never a request's: httpjson.Marked reads it off an " +
		"answer, the engine's own clients telling its answers from a proxy's",
	"Origin": "a forbidden header name: the browser sets it and no page can",
	"Access-Control-Request-Method": "the preflight's own, which the browser " +
		"sends and no page can",
	"Upgrade": "a forbidden header name, read on the socket's handshake, which " +
		"no preflight guards",
	"Accept-Encoding": "a forbidden header name: the browser negotiates it " +
		"for the dashboard's own files",
	"X-Forwarded-For": "a proxy's, honoured only from api.trusted_proxies — " +
		"a page has no business claiming an address",
	// SAME-ORIGIN ONLY, because no other origin can reach the one route that
	// reads it: the Dashboard Assistant's stream answers nothing but a
	// person's own browser session (iam.Assistable), and no answer here allows
	// another origin credentials. Offering it would widen every preflight's
	// answer for a request no permitted page can make.
	"Last-Event-ID": "same-origin only: the Dashboard Assistant's stream resumes " +
		"by it, and answers nothing but a browser session",
}

// unexposedResponse are the response headers a route sets that a permitted
// origin's script is NOT offered, each with why.
var unexposedResponse = map[string]string{
	"Content-Type":   "CORS-safelisted: readable without being listed",
	"Content-Length": "CORS-safelisted: readable without being listed",
	"Cache-Control":  "CORS-safelisted: readable without being listed",
	"Content-Security-Policy": "the browser's to enforce, never a page's " +
		"script's to read",
	"X-Frame-Options":           "the browser's to enforce",
	"X-Content-Type-Options":    "the browser's to enforce",
	"Referrer-Policy":           "the browser's to enforce",
	"Strict-Transport-Security": "the browser's to enforce",
	// The artifact origin's own isolation (internal/api/pagepolicy's
	// artifact policies), on its documents and files alike: each is the
	// browser's or a crawler's to obey, and none a page's script's to read.
	"Permissions-Policy":           "the browser's to enforce",
	"Cross-Origin-Opener-Policy":   "the browser's to enforce",
	"Cross-Origin-Resource-Policy": "the browser's to enforce",
	"Origin-Agent-Cluster":         "the browser's to enforce",
	"X-Robots-Tag":                 "a crawler's to obey, never a page's script's to read",
	"Vary":                         "a cache's key, never a client's next request",
	"WWW-Authenticate": "git's own credential challenge on the git transport, " +
		"which git answers with a person's token and no page's script reads",
	"Content-Encoding": "the browser decodes the body itself",
	"X-Accel-Buffering": "a reverse proxy's to obey — the assistant's event stream " +
		"asks it not to buffer — never a page's script's to read",
	"Access-Control-Allow-Origin": "the CORS protocol's own, read by the " +
		"browser",
	"Access-Control-Allow-Methods":  "the CORS protocol's own",
	"Access-Control-Allow-Headers":  "the CORS protocol's own",
	"Access-Control-Max-Age":        "the CORS protocol's own",
	"Access-Control-Expose-Headers": "the CORS protocol's own",
}

// routeHeaders walks internal/api's handlers and answers every request header
// read and every response header set, canonical, each with where.
func routeHeaders(t *testing.T, root string) (reads, sets map[string][]string) {
	t.Helper()
	reads, sets = map[string][]string{}, map[string][]string{}
	consts := &constants{root: root, byDir: map[string]map[string]string{}}
	apiDir := filepath.Join(root, "internal", "api")
	err := sourcetree.Walk(apiDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// THE WEBHOOK EDGE IS A VENDOR'S: every header read there
			// comes from GitHub's, Slack's or Atlassian's servers, on
			// routes no page is offered and no browser preflights.
			if d.Name() == "testdata" || path == filepath.Join(apiDir, "webhooks") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
		rel, _ := filepath.Rel(root, path)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			scanFunc(t, fset, rel, file, filepath.Dir(path), fn, consts, reads, sets)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", apiDir, err)
	}
	return reads, sets
}

// headerSide is which message a header value belongs to.
type headerSide int

const (
	notAHeader headerSide = iota
	requestSide
	responseSide
	// parameterSide is an http.Header a helper was handed: this API's
	// helpers read a request's (acceptsGzip) and write a response's
	// (pagepolicy.Set), so a read of one is a request's and a write a
	// response's.
	parameterSide
)

// scanFunc records every header read and set in one function, its closures
// included.
func scanFunc(t *testing.T, fset *token.FileSet, rel string, file *ast.File, dir string,
	fn *ast.FuncDecl, consts *constants, reads, sets map[string][]string) {
	t.Helper()
	bound := map[string]headerSide{}
	params := func(ft *ast.FuncType) {
		for _, field := range ft.Params.List {
			if isHTTPHeaderType(field.Type) {
				for _, name := range field.Names {
					bound[name.Name] = parameterSide
				}
			}
		}
	}
	params(fn.Type)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			params(n.Type)
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && i < len(n.Rhs) {
					if side := sideOf(n.Rhs[i], nil); side != notAHeader {
						bound[id.Name] = side
					}
				}
			}
		}
		return true
	})
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		verb := sel.Sel.Name
		side := sideOf(sel.X, bound)
		var into map[string][]string
		switch {
		case (verb == "Get" || verb == "Values") &&
			(side == requestSide || side == parameterSide):
			into = reads
		case (verb == "Set" || verb == "Add") &&
			(side == responseSide || side == parameterSide):
			into = sets
		default:
			return true
		}
		where := fmt.Sprintf("%s:%d", rel, fset.Position(call.Pos()).Line)
		name, ok := consts.value(t, file, dir, call.Args[0])
		if !ok {
			t.Errorf("%s: a header named by something other than a constant "+
				"(%s) — the gate cannot hold what it cannot read; name it by a "+
				"literal or a constant", where, shape(call.Args[0]))
			return true
		}
		key := http.CanonicalHeaderKey(name)
		into[key] = append(into[key], where)
		return true
	})
}

// sideOf says which message a header expression is: `r.Header` a request's,
// `w.Header()` a response's, a name bound to either the same, and an
// http.Header parameter its own kind.
func sideOf(x ast.Expr, bound map[string]headerSide) headerSide {
	switch x := x.(type) {
	case *ast.SelectorExpr:
		if x.Sel.Name == "Header" {
			return requestSide
		}
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Header" &&
			len(x.Args) == 0 {
			return responseSide
		}
	case *ast.Ident:
		return bound[x.Name]
	}
	return notAHeader
}

func isHTTPHeaderType(x ast.Expr) bool {
	sel, ok := x.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http" && sel.Sel.Name == "Header"
}

// shape names an argument's syntax for a refusal.
func shape(x ast.Expr) string { return fmt.Sprintf("%T", x) }

// constants folds the string constants of this module's packages, read from
// their source, so a header named by a constant — opkey.Header, ReaderHeader,
// authapi's link secrets — is held by its value.
type constants struct {
	root  string
	byDir map[string]map[string]string
}

const modulePath = "github.com/crewlet/crewlet"

// value is the string an argument evaluates to, when it is a literal or a
// constant of this module.
func (c *constants) value(t *testing.T, file *ast.File, dir string, x ast.Expr) (string, bool) {
	t.Helper()
	switch x := x.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		v, err := strconv.Unquote(x.Value)
		return v, err == nil
	case *ast.Ident:
		v, ok := c.of(t, dir)[x.Name]
		return v, ok
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		for _, imp := range file.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			name := filepath.Base(path)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			if name != pkg.Name || !strings.HasPrefix(path, modulePath+"/") {
				continue
			}
			v, ok := c.of(t, filepath.Join(c.root,
				filepath.FromSlash(strings.TrimPrefix(path, modulePath+"/"))))[x.Sel.Name]
			return v, ok
		}
	}
	return "", false
}

// of is one package directory's top-level string constants that are a
// literal, parsed once.
func (c *constants) of(t *testing.T, dir string) map[string]string {
	t.Helper()
	if got, ok := c.byDir[dir]; ok {
		return got
	}
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", filepath.Join(dir, name), err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, ident := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if v, err := strconv.Unquote(lit.Value); err == nil {
							out[ident.Name] = v
						}
					}
				}
			}
		}
	}
	c.byDir[dir] = out
	return out
}

// THE LISTS ARE WRITTEN ONCE EACH. A name listed twice is answered twice in
// the preflight, which a browser tolerates and a reader of the list does not.
func TestTheCORSListsNameEachHeaderOnce(t *testing.T) {
	t.Parallel()
	for name, list := range map[string][]string{
		"corsHeaders": corsHeaders, "corsExposed": corsExposed,
	} {
		seen := map[string]bool{}
		for _, h := range list {
			key := http.CanonicalHeaderKey(h)
			if seen[key] {
				t.Errorf("%s names %s twice", name, h)
			}
			seen[key] = true
		}
	}
}

// A PERMITTED ORIGIN MAY SEND EVERY METHOD A ROUTE SERVES, AND NO OTHER.
//
// Only GET, HEAD and POST leave a browser without asking; every other method a
// script sends is preflighted, and a preflight that does not name it refuses
// the whole request in the browser, where the engine never sees it. The set
// was written down beside the routes rather than read off them, and it left
// `OPTIONS` out while `OPTIONS /config` answers the `Allow` and `Accept-Patch`
// [corsExposed] offers: a page on an allowed origin was offered both headers
// and could never make the request that carries them.
//
// So this reads the method of every route pattern this API's handler source
// registers — a pattern is a string that opens with its method and a space,
// whole or as the head of a concatenation — and holds [corsMethods] to that
// set in both directions. A GET pattern serves HEAD too, as the mux does, and
// a pattern with no method constrains none, so it adds nothing. The webhook
// package is skipped for the reason the header walk skips it.
//
// Mutation: drop OPTIONS from corsMethods and this fails naming it; add a
// method no route serves and it fails naming that.
func TestTheCORSMethodsAreTheMethodsTheRoutesServe(t *testing.T) {
	t.Parallel()
	served := routeMethods(t, sourcetree.Root(t))
	// THE CONTROLS: the one OPTIONS route and a PATCH, so a walk that read
	// nothing, or only the top-level table, cannot pass.
	for _, method := range []string{http.MethodOptions, http.MethodPatch} {
		if len(served[method]) == 0 {
			t.Fatalf("control: the walk found no %s route (%d methods in all), so "+
				"it is not reading the route table", method, len(served))
		}
	}
	offered := map[string]bool{}
	for _, method := range corsMethods {
		offered[method] = true
		if len(served[method]) == 0 {
			t.Errorf("a permitted origin's preflight names %s and no route serves it",
				method)
		}
	}
	for method, where := range served {
		if !offered[method] {
			t.Errorf("%s is served (%s) and a permitted origin's preflight does not "+
				"name it, so the browser refuses every such request", method,
				strings.Join(where, ", "))
		}
	}
}

// routeMethods answers the method of every route pattern internal/api's
// handler source spells, each with where.
func routeMethods(t *testing.T, root string) map[string][]string {
	t.Helper()
	methods := map[string]bool{
		http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
		http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
		http.MethodOptions: true, http.MethodConnect: true, http.MethodTrace: true,
	}
	out := map[string][]string{}
	apiDir := filepath.Join(root, "internal", "api")
	err := sourcetree.Walk(apiDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || path == filepath.Join(apiDir, "webhooks") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			method, rest, found := strings.Cut(value, " ")
			if !found || !methods[method] || (rest != "" && !strings.HasPrefix(rest, "/")) {
				return true
			}
			where := fmt.Sprintf("%s:%d", rel, fset.Position(lit.Pos()).Line)
			out[method] = append(out[method], where)
			if method == http.MethodGet {
				out[http.MethodHead] = append(out[http.MethodHead], where+" (a GET)")
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", apiDir, err)
	}
	return out
}
