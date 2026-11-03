package code

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// NO CODE READ ESCAPES ITS TRANSACTION.
//
// Every read here is served inside the ONE transaction the framework's read
// hands its callback — the checkpoint the answer names included — so the rows,
// the checkpoint and the level are one state of the estate. A statement on
// anything else would be outside the level, the refusal ladder and the
// coverage probe, and would pair rows of two instants; the knowledge base's
// reader shipped exactly that, five statements at five instants.
//
// A STRUCTURAL TEST, because the defect is structural — a behavioural one
// would have to catch two statements disagreeing across a concurrent write,
// a race a test can only lose reliably. It holds two things: every statement
// in every file that serves a Reader's reads — reader.go, and each file
// declaring a method of the Reader beside it, as changeread.go's reads of
// changes and retire.go's listing do — is issued on a parameter declared
// *sql.Tx, and the Reader holds no handle a statement could reach the estate
// through instead.
//
// Mutation: issue changeread.go's listing on a transaction of its own, and
// this names the statement.
func TestNoCodeReadEscapesItsTransaction(t *testing.T) {
	t.Parallel()

	// THE MATCHER, ON INPUT WHOSE VERDICT IS KNOWN.
	for name, tc := range map[string]struct {
		src   string
		clean bool
	}{
		"a statement on the callback's transaction": {clean: true, src: `
func (r *Reader) f(ctx context.Context) error {
	return r.log.Read(ctx, q, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, s).Scan()
	})
}`},
		"a statement on a helper's transaction": {clean: true, src: `
func helper(ctx context.Context, tx *sql.Tx) error { return tx.QueryRowContext(ctx, s).Scan() }`},
		"a statement on the bare connection": {src: `
func (r *Reader) f(ctx context.Context) error {
	return r.db.SQL().QueryRowContext(ctx, s).Scan()
}`},
		"a statement on a transaction of its own": {src: `
func (r *Reader) f(ctx context.Context, db *sql.DB) error {
	tx, _ := db.BeginTx(ctx, nil)
	return tx.QueryRowContext(ctx, s).Scan()
}`},
	} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "control.go", "package code\n"+tc.src, 0)
		if err != nil {
			t.Fatalf("control %q does not parse: %v", name, err)
		}
		if escapes := escapingStatements(fset, file); (len(escapes) == 0) != tc.clean {
			t.Errorf("control %q: the matcher reported %v, want clean=%v", name, escapes, tc.clean)
		}
	}

	fset := token.NewFileSet()
	files := readerFiles(t, fset)
	for _, name := range []string{"reader.go", "retire.go", "changeread.go"} {
		if files[name] == nil {
			t.Fatalf("%s declares no method of the Reader, so this guard no longer reads it", name)
		}
	}
	for name, file := range files {
		statements := 0
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && isStatement(call) {
				statements++
			}
			return true
		})
		if statements == 0 {
			t.Errorf("%s issues no statement this guard recognises, so it would pass having read nothing", name)
		}
		for _, escape := range escapingStatements(fset, file) {
			t.Errorf("%s is issued on something other than the transaction its read was "+
				"handed, so it is outside the level, the refusal ladder and the coverage "+
				"probe, and reads another instant than the rows beside it", escape)
		}
	}
	for _, decl := range files["reader.go"].Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "Reader" {
				continue
			}
			for _, field := range ts.Type.(*ast.StructType).Fields.List {
				if sel, ok := field.Type.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok && (pkg.Name == "store" || pkg.Name == "sql") {
						t.Errorf("the Reader holds a %s.%s — a handle a statement could reach "+
							"the estate through outside the framework's read", pkg.Name, sel.Sel.Name)
					}
				}
			}
		}
	}
}

// readerFiles is every non-test file of this package that declares a method of
// the Reader, parsed, by name: the files whose statements serve its reads.
func readerFiles(t *testing.T, fset *token.FileSet) map[string]*ast.File {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the package's files: %v", err)
	}
	out := map[string]*ast.File{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			if star, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
				if id, ok := star.X.(*ast.Ident); ok && id.Name == "Reader" {
					out[name] = file
				}
			}
		}
	}
	return out
}

// isStatement reports a call that issues SQL. The context variants alone:
// every statement here takes the read's context, and `Query` is also a method
// of statelog.Freshness, which builds the read rather than issuing one.
func isStatement(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "QueryContext", "QueryRowContext", "ExecContext":
		return true
	}
	return false
}

// escapingStatements is every statement in file issued on anything but a
// parameter declared *sql.Tx by a function that encloses it.
func escapingStatements(fset *token.FileSet, file *ast.File) []string {
	var out []string
	var walk func(n ast.Node, txs map[string]bool)
	walk = func(n ast.Node, txs map[string]bool) {
		ast.Inspect(n, func(m ast.Node) bool {
			var params *ast.FieldList
			var body *ast.BlockStmt
			switch f := m.(type) {
			case *ast.FuncDecl:
				params, body = f.Type.Params, f.Body
			case *ast.FuncLit:
				params, body = f.Type.Params, f.Body
			case *ast.CallExpr:
				if isStatement(f) {
					recv, ok := f.Fun.(*ast.SelectorExpr).X.(*ast.Ident)
					if !ok || !txs[recv.Name] {
						out = append(out, fset.Position(f.Pos()).String())
					}
				}
				return true
			default:
				return true
			}
			if m == n || body == nil {
				return true
			}
			inner := map[string]bool{}
			for k, v := range txs {
				inner[k] = v
			}
			for _, field := range params.List {
				star, ok := field.Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				sel, ok := star.X.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Tx" {
					continue
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "sql" {
					for _, name := range field.Names {
						inner[name.Name] = true
					}
				}
			}
			walk(body, inner)
			return false
		})
	}
	walk(file, map[string]bool{})
	return out
}
