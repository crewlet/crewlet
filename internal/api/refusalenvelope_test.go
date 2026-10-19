package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE SETTINGS SURFACES REFUSE IN THE ENVELOPE, AND A 503 SAYS WHEN.
//
// `/config` and `/secrets` wrote their own `{"error": code}` objects — a code
// and nothing a person could read — at some thirty sites, `/setup` declared a
// dozen codes of its own with no sentence behind any of them (two of them
// second spellings of codes the vocabulary already held), and a dozen 503s
// across these surfaces went out through the ordinary refusal writer with no
// Retry-After, so a client was told "come back" and not when. The webhook edge
// had an envelope of its own — `{"error": "invalid signature"}`, a code with a
// space in it, and `{"status": "unavailable", "reason": …}` — so it is walked
// too: a vendor reads only the status and the Retry-After, and the operator
// reading the body in a delivery log reads the same vocabulary as everywhere
// else. The operator surface is walked beside them, because its one refusal
// mapping is what the act route, the human write surface and every tool-less
// verb answer through: a status it chose by hand would be every write
// surface's.
//
// Each of those shapes is visible in the SOURCE and nowhere else — a route
// that builds its own body answers correctly on every case somebody thought
// to test — so this walks the source:
//
//   - a map literal carrying an "error" key is a hand-built envelope, which
//     has no `message` and no guarantee its code is on the table;
//   - an httpjson.Code(...) conversion is a code minted outside the table,
//     which [httpjson.Code.Message] answers with nothing;
//   - http.StatusServiceUnavailable named anywhere is a 503 written without
//     httpjson.Unavailable / UnavailableWith, the only writers that set the
//     header and the envelope together.
func TestTheSettingsSurfacesRefuseInTheEnvelope(t *testing.T) {
	t.Parallel()
	surfaces := []string{"assistantapi", "configapi", "secretsapi", "setupapi", "workapi",
		"operator", "webhooks"}
	walked := 0
	for _, dir := range surfaces {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			walked++
			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.KeyValueExpr:
					if lit, ok := node.Key.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if key, _ := strconv.Unquote(lit.Value); key == "error" {
							t.Errorf("%s: a hand-built refusal body — answer through "+
								"httpjson.Fail/FailWith/FailWithFields so it carries "+
								"the code's sentence", fset.Position(node.Pos()))
						}
					}
				case *ast.CallExpr:
					if isSelector(node.Fun, "httpjson", "Code") {
						t.Errorf("%s: a code minted outside internal/api/httpjson's "+
							"table, which carries no sentence — declare it there",
							fset.Position(node.Pos()))
					}
				case *ast.SelectorExpr:
					if isSelector(node, "http", "StatusServiceUnavailable") {
						t.Errorf("%s: a 503 written without httpjson.Unavailable "+
							"or UnavailableWith, which set the Retry-After and the "+
							"envelope together", fset.Position(node.Pos()))
					}
				}
				return true
			})
		}
	}
	// THE CONTROL: a walk that read nothing would pass on every tree.
	if walked < len(surfaces) {
		t.Fatalf("walked %d source files across %d surfaces, so the walk "+
			"certifies nothing", walked, len(surfaces))
	}
}

// AN UNKNOWN OUTCOME IS WRITTEN ONE WAY.
//
// A write that may have landed and a refusal that wrote nothing are both 503s,
// and every surface wrote the first for itself: `/iam` said it in
// a sentence and nowhere else, `/worklet` carried `outcome` on some routes and
// not others, so a client branching on the field (`crewlet worklet purge`)
// told an operator whose purge may have landed that it wrote nothing.
// httpjson.UnknownOutcome writes `outcome`, `op_id` and `unvouched` together,
// and `unvouched` is the one key only an unknown carries — so a body written
// with that key anywhere else under internal/api is an unknown written by
// hand, which is the shape that drifted. A log line naming it as an attribute
// is not a body, and is left alone.
//
// Mutation: put `detail["unvouched"] = true` back into any surface's own
// unknown and this goes red.
func TestAnUnknownOutcomeIsWrittenOneWay(t *testing.T) {
	t.Parallel()
	walked := 0
	err := sourcetree.Walk(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "httpjson" || d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		walked++
		unvouched := func(expr ast.Expr) bool {
			lit, ok := expr.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return false
			}
			key, _ := strconv.Unquote(lit.Value)
			return key == "unvouched"
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.KeyValueExpr:
				if unvouched(node.Key) {
					t.Errorf("%s: an unknown outcome written by hand — answer it "+
						"with httpjson.UnknownOutcome, which carries `outcome` "+
						"beside it", fset.Position(node.Pos()))
				}
			case *ast.IndexExpr:
				if unvouched(node.Index) {
					t.Errorf("%s: an unknown outcome written by hand — answer it "+
						"with httpjson.UnknownOutcome, which carries `outcome` "+
						"beside it", fset.Position(node.Pos()))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/api: %v", err)
	}
	// THE CONTROL: the surfaces that write through a state log alone are
	// dozens of files, and a walk rooted in the wrong place reads none.
	if walked < 50 {
		t.Fatalf("walked %d source files, which is not internal/api", walked)
	}
}

// isSelector reports whether expr is pkg.name.
func isSelector(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

// A REFUSAL ON AUTHORITY IS WRITTEN BY THE ONE MAPPING.
//
// The authority table answers every refusal it makes through ONE function —
// authz.RefusalFields, which [authz.EnvelopeRefusal] and every surface that
// adds fields of its own call — and that function is what chooses between
// `unauthorized`, `step_up_required` and `token_refused`. A surface that spells
// one of those codes for itself agrees with the mapping only until the mapping
// answers its refusal another way, and then answers it wrongly without a test
// noticing: the overview's mirrors, the socket's handshake and its query and
// watch frames, the query surface's REST answer and the sign-in surface's
// step-up each wrote `unauthorized` or `step_up_required` themselves, and two
// of them were already wrong — a machine token was told to step up. So no
// non-test file under internal/api hands one of the three codes to a call,
// assigns it, or returns it; the readers — a list of the codes a transport can
// answer, a comparison against one — are left alone. The socket's own
// vocabulary spells `unauthorized` as an untyped constant of its own, so in
// internal/api/stream that constant may be named only where it is declared and
// in RefusedFor, the one place the mapping's code meets the vocabulary.
//
// Mutation: answer serveFrom's stage refusal with httpjson.CodeUnauthorized
// again and this goes red naming rest.go; reply a query refusal
// queryError(req, CodeUnauthorized) in the socket and it names socket.go.
func TestARefusalOnAuthorityIsWrittenByTheOneMapping(t *testing.T) {
	t.Parallel()
	const httpjsonPath = "github.com/crewlet/crewlet/internal/api/httpjson"
	codes := map[string]bool{"CodeUnauthorized": true, "CodeStepUpRequired": true,
		"CodeTokenRefused": true}
	walked := 0
	err := sourcetree.Walk(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		walked++
		// HOW THIS FILE NAMES ONE OF THE THREE: through the import, under
		// whatever name it gives the package, or bare inside httpjson.
		as := ""
		for _, spec := range file.Imports {
			if imported, _ := strconv.Unquote(spec.Path.Value); imported == httpjsonPath {
				as = "httpjson"
				if spec.Name != nil {
					as = spec.Name.Name
				}
			}
		}
		spells := func(expr ast.Expr) bool {
			switch e := ast.Unparen(expr).(type) {
			case *ast.SelectorExpr:
				pkg, ok := e.X.(*ast.Ident)
				return ok && as != "" && pkg.Name == as && codes[e.Sel.Name]
			case *ast.Ident:
				return file.Name.Name == "httpjson" && codes[e.Name]
			}
			return false
		}
		refuse := func(n ast.Node, how string) {
			t.Errorf("%s: %s one of the codes a refusal on authority is "+
				"answered with — answer the decision through "+
				"authz.RefusalFields or authz.EnvelopeRefusal",
				fset.Position(n.Pos()), how)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				for _, arg := range node.Args {
					if spells(arg) {
						refuse(arg, "hands a call")
					}
				}
			case *ast.AssignStmt:
				for _, rhs := range node.Rhs {
					if spells(rhs) {
						refuse(rhs, "assigns")
					}
				}
			case *ast.ValueSpec:
				for _, value := range node.Values {
					if spells(value) {
						refuse(value, "declares")
					}
				}
			case *ast.ReturnStmt:
				for _, result := range node.Results {
					if spells(result) {
						refuse(result, "returns")
					}
				}
			}
			return true
		})
		if file.Name.Name == "stream" {
			for _, decl := range file.Decls {
				if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
					continue
				}
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil &&
					fn.Name.Name == "RefusedFor" {
					continue
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok && id.Name == "CodeUnauthorized" {
						t.Errorf("%s: the socket's own `unauthorized` named outside "+
							"its declaration and RefusedFor — answer the decision "+
							"through stream.RefusedFor", fset.Position(id.Pos()))
					}
					return true
				})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/api: %v", err)
	}
	if walked < 50 {
		t.Fatalf("walked %d source files, which is not internal/api", walked)
	}
}
