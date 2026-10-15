package api_test

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

// bodyReaders are the functions allowed to touch a request's body directly,
// by file (relative to internal/api) and function: the one reader every route
// goes through, and the one route that streams instead of reading.
var bodyReaders = map[string]string{
	// The reader itself: a size cap and a read deadline, in one place.
	"httpjson/httpjson.go": "ReadBody",
	// A file upload streams up to a gibibyte into the object store, which
	// no buffered read could carry; it paces the stream instead, renewing
	// the read deadline only while bytes keep arriving.
	"files.go": "serveFileUpload",
}

// TestEveryRouteReadsItsBodyThroughReadBody fails the build when a handler
// under internal/api reads a request's body any other way.
//
// The server sets no ReadTimeout, by design — one large enough for a 25 MiB
// webhook on a slow link is no bound on a small one — so a read is bounded in
// time only where it is made, and httpjson.ReadBody is where it is made. A
// route that read its body with io.ReadAll held its connection for as long as
// a client dribbled: the OTLP receiver's did, behind a token readable inside a
// box for a day. So a handler's *http.Request parameter may have its Body
// named only in bodyReaders.
//
// Syntactic and deliberately so: a parameter typed *http.Request is the only
// way a handler holds a request, so the check is on that parameter's Body,
// wherever in the function — closures included — it is named.
func TestEveryRouteReadsItsBodyThroughReadBody(t *testing.T) {
	t.Parallel()

	// A guard asserting an absence passes identically when the absence is
	// real and when the matcher has stopped matching, so the matcher runs
	// on sources whose verdicts are known first.
	for src, want := range map[string]int{
		`package p; import ("io"; "net/http"); func h(w http.ResponseWriter, r *http.Request) { io.ReadAll(r.Body) }`:             1,
		`package p; import "net/http"; func h(w http.ResponseWriter, req *http.Request) { go func() { _ = req.Body.Close() }() }`: 1,
		`package p; import "net/http"; var h = func(_ http.ResponseWriter, r *http.Request) { defer r.Body.Close() }`:             1,
		`package p; import "net/http"; func h(w http.ResponseWriter, r *http.Request) { _, _ = httpjson.ReadBody(w, r, 1) }`:      0,
		`package p; type reply struct{ Body []byte }; func h(r reply) []byte { return r.Body }`:                                   0,
		`package p; import "net/http"; func h(r *http.Request) { var x struct{ Body int }; _ = x.Body; _ = r.ContentLength }`:     0,
	} {
		if got := bodyReads(t, "control.go", src); len(got) != want {
			t.Errorf("control: %q has %d direct body reads, the matcher found %d", src, want, len(got))
		}
	}

	root := filepath.Join(sourcetree.Root(t), "internal", "api")
	var files int
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, read := range bodyReads(t, path, "") {
			if bodyReaders[rel] == read.function {
				continue
			}
			t.Errorf("internal/api/%s:%d: %s reads its request's body directly — read it "+
				"with httpjson.ReadBody, which bounds it in size and in time, or "+
				"declare it in bodyReaders with the reason it streams", rel, read.line, read.function)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 20 {
		t.Fatalf("read %d files under internal/api; the walk is not reading the tree", files)
	}
}

// bodyRead is one direct read of a request's body.
type bodyRead struct {
	function string
	line     int
}

// bodyReads is every place in one file a *http.Request parameter's Body is
// named, by the enclosing top-level function. src overrides the file's
// contents, for the controls.
func bodyReads(t *testing.T, path, src string) []bodyRead {
	t.Helper()
	fset := token.NewFileSet()
	var source any
	if src != "" {
		source = src
	}
	file, err := parser.ParseFile(fset, path, source, 0)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var out []bodyRead
	for _, decl := range file.Decls {
		name := "a package-level declaration"
		var root ast.Node = decl
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name = d.Name.Name
		case *ast.GenDecl:
		default:
			continue
		}
		requests := map[string]bool{}
		ast.Inspect(root, func(n ast.Node) bool {
			var params *ast.FieldList
			switch f := n.(type) {
			case *ast.FuncDecl:
				params = f.Type.Params
			case *ast.FuncLit:
				params = f.Type.Params
			case *ast.SelectorExpr:
				if id, ok := f.X.(*ast.Ident); ok && f.Sel.Name == "Body" && requests[id.Name] {
					out = append(out, bodyRead{function: name, line: fset.Position(f.Pos()).Line})
				}
				return true
			default:
				return true
			}
			for _, field := range params.List {
				if isHTTPRequest(field.Type) {
					for _, id := range field.Names {
						requests[id.Name] = true
					}
				}
			}
			return true
		})
	}
	return out
}

// isHTTPRequest reports the type *http.Request.
func isHTTPRequest(expr ast.Expr) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http" && sel.Sel.Name == "Request"
}
