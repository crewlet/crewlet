package pages_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// BOTH APPLY GATES ARE READ IN ONE TRANSACTION.
//
// [pages.Gates.GatedAt] is one answer made of two reads — the page's deletion
// marker and the writer's eviction window — and it shipped as two
// `Replicated().Read` calls under a comment saying it was one. Two calls are
// two snapshots, and two loads of the replicated peer an adoption swaps while
// readers run: the applier commits between them, a purge of the page can land
// between them, and the two halves then describe two states of the estate —
// possibly two files.
//
// A STRUCTURAL TEST, because the defect is structural, for the reason
// read_test.go gives: a behavioural one would have to commit a purge or swap
// the estate between two statements of one call, which is a race a test can
// only lose reliably. So it holds the shape instead: exactly one transaction,
// both gate tables named inside it, and every statement run on that
// transaction's own handle rather than on a pool a statement could reach
// around it through.
func TestGatedAtReadsBothGatesInOneTransaction(t *testing.T) {
	t.Parallel()

	// THE MATCHER, ON INPUT WHOSE VERDICT IS KNOWN. A guard asserting a
	// shape passes identically when the shape holds and when the guard has
	// stopped looking.
	for name, tc := range map[string]struct {
		src   string
		clean bool
	}{
		"the two calls this replaced": {src: `
func (g *Gates) GatedAt(ctx context.Context) error {
	if err := g.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT 1 FROM pages_deletions").Scan()
	}); err != nil {
		return err
	}
	return g.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT 1 FROM pages_evictions").Scan()
	})
}`},
		"a statement that reaches around the transaction": {src: `
func (g *Gates) GatedAt(ctx context.Context) error {
	return g.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM pages_deletions").Scan(); err != nil {
			return err
		}
		return g.db.Replicated().SQL().QueryRowContext(ctx,
			"SELECT 1 FROM pages_evictions").Scan()
	})
}`},
		"one transaction that reads one gate": {src: `
func (g *Gates) GatedAt(ctx context.Context) error {
	return g.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT 1 FROM pages_deletions").Scan()
	})
}`},
		"one transaction that reads both": {clean: true, src: `
func (g *Gates) GatedAt(ctx context.Context) error {
	return g.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM pages_deletions").Scan(); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, "SELECT 1 FROM pages_evictions").Scan()
	})
}`},
	} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "control.go", "package pages\n"+tc.src, 0)
		if err != nil {
			t.Fatalf("control %q does not parse: %v", name, err)
		}
		fn := gatedAt(file)
		if fn == nil {
			t.Fatalf("control %q declares no (*Gates).GatedAt", name)
		}
		if problems := oneTransaction(fset, fn); (len(problems) == 0) != tc.clean {
			t.Errorf("control %q: the matcher reported %q, want clean=%v",
				name, problems, tc.clean)
		}
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "rows.go", nil, 0)
	if err != nil {
		t.Fatalf("parse rows.go: %v", err)
	}
	fn := gatedAt(file)
	if fn == nil {
		t.Fatal("rows.go no longer declares (*Gates).GatedAt — if it moved, " +
			"move this guard with it rather than deleting it")
	}
	for _, problem := range oneTransaction(fset, fn) {
		t.Errorf("GatedAt %s — its two gates are then read at two instants, "+
			"and across an adoption from two files, so its answer pairs two "+
			"states of the estate rather than naming the gate one state holds",
			problem)
	}
}

// gatedAt finds the (*Gates).GatedAt declaration in a file.
func gatedAt(file *ast.File) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "GatedAt" || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if recv, ok := star.X.(*ast.Ident); ok && recv.Name == "Gates" {
			return fn
		}
	}
	return nil
}

// oneTransaction reports every way fn's body departs from ONE transaction
// that reads both gate tables through its own handle.
func oneTransaction(fset *token.FileSet, fn *ast.FuncDecl) []string {
	var opens, statements []*ast.CallExpr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "Read", "Tx":
			opens = append(opens, call)
		case "QueryRowContext", "QueryContext", "ExecContext":
			statements = append(statements, call)
		}
		return true
	})
	if len(opens) != 1 {
		return []string{fmt.Sprintf("opens %d transactions where it must open one", len(opens))}
	}
	body, ok := opens[0].Args[len(opens[0].Args)-1].(*ast.FuncLit)
	if !ok || len(body.Type.Params.List) != 1 || len(body.Type.Params.List[0].Names) != 1 {
		return []string{"hands its transaction something other than a function " +
			"literal taking the transaction, so what runs inside it cannot be checked"}
	}
	tx := body.Type.Params.List[0].Names[0].Name

	var problems []string
	for _, stmt := range statements {
		where := fset.Position(stmt.Pos()).String()
		if stmt.Pos() < body.Body.Pos() || stmt.End() > body.Body.End() {
			problems = append(problems, "issues a statement outside its transaction at "+where)
			continue
		}
		if recv, ok := stmt.Fun.(*ast.SelectorExpr).X.(*ast.Ident); !ok || recv.Name != tx {
			problems = append(problems, fmt.Sprintf("issues a statement at %s on "+
				"something other than its transaction %q", where, tx))
		}
	}
	var literals strings.Builder
	ast.Inspect(body.Body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			literals.WriteString(lit.Value)
		}
		return true
	})
	for _, table := range []string{"pages_deletions", "pages_evictions"} {
		if !strings.Contains(literals.String(), table) {
			problems = append(problems, "does not read "+table+" inside its transaction")
		}
	}
	return problems
}
