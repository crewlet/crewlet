package doclet

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strings"
	"testing"
)

// NO READ HERE TOUCHES THE BARE CONNECTION.
//
// D119: every multi-statement answer runs inside one `BEGIN DEFERRED`, or a
// `Get` reads head, comments, history, children and ancestors at different
// instants (a comment thread from after the revision it reported). The
// transaction comes from [statelog.Reader.Read], so this guard protects the
// snapshot and the read level at once: a statement that escapes the callback is
// served with no refusal ladder and no coverage probe in front of it.
//
// A STRUCTURAL TEST, because the defect is structural: a behavioural one would
// have to catch two statements disagreeing across a concurrent write, a race a
// test can only lose reliably.
func TestNoPageReadEscapesItsTransaction(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "read.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse read.go: %v", err)
	}

	// THE MATCHER, ON INPUT WHOSE VERDICT IS KNOWN. A guard asserting an
	// absence passes identically when the thing is absent and when the
	// guard has gone inert.
	for _, positive := range []string{
		"part.SQL().QueryContext(ctx, q)",
		"r.db.Node().SQL().QueryRowContext(ctx, q)",
	} {
		if !readsOffATransaction(positive) {
			t.Errorf("control: %q reads off the bare connection and the matcher "+
				"did not flag it", positive)
		}
	}
	if readsOffATransaction("tx.QueryContext(ctx, q)") {
		t.Error("control: a transaction read was flagged, so this guard would " +
			"fail on the correct shape")
	}

	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var buf strings.Builder
		if err := printer.Fprint(&buf, fset, call.Fun); err != nil {
			return true
		}
		if readsOffATransaction(buf.String()) {
			found = append(found, fset.Position(call.Pos()).String()+" "+buf.String())
		}
		return true
	})
	for _, site := range found {
		t.Errorf("%s reads off the bare connection, so it is outside the "+
			"transaction the framework read opens — and outside the level, the "+
			"refusal ladder and the coverage probe with it", site)
	}
}

// readsOffATransaction reports whether an expression reaches the store's
// connection rather than the transaction handed to a read.
func readsOffATransaction(expr string) bool {
	return strings.Contains(expr, ".SQL().Query") ||
		strings.Contains(expr, ".SQL().Exec")
}
