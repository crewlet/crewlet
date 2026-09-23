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

	"github.com/crewlet/crewlet/internal/clientsource"
)

// clientOnlyCodes are the query error codes the dashboard's socket produces
// itself, which no engine frame carries: a sent query that went unanswered,
// and a query still in flight when the client shut down.
var clientOnlyCodes = []string{"timeout", "closed"}

// THE ENGINE AND THE DASHBOARD SPEAK ONE QUERY ERROR VOCABULARY.
//
// They did not. The dashboard handled `no_event_store`, which no engine ever
// sent, and the reference documented two more codes nothing produced, so a
// screen carried a branch that could never run and an operator read about
// answers that could never arrive. The dashboard's `QueryErrorCode` union is
// what its screens branch on (they compare the error narrowed to it, so a code
// outside it is a type error), and this pins that union to the codes this
// package sends, read from its own source so a new code is covered without
// being listed here.
func TestTheDashboardKnowsExactlyTheQueryErrorCodesTheEngineSends(t *testing.T) {
	t.Parallel()
	want := append(engineCodes(t), clientOnlyCodes...)
	slices.Sort(want)
	got := dashboardCodes(t)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's QueryErrorCode union is %v; the engine sends %v and "+
			"the socket adds %v, so the two vocabularies have drifted. A code only "+
			"the dashboard knows is a branch that never runs; a code only the engine "+
			"sends is a failure every screen renders as unknown",
			got, engineCodes(t), clientOnlyCodes)
	}
}

// engineCodes is every string constant named Code… in this package's source.
func engineCodes(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	var codes []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, ident := range value.Names {
					if !strings.HasPrefix(ident.Name, "Code") || i >= len(value.Values) {
						continue
					}
					lit, ok := value.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					code, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("%s: %v", ident.Name, err)
					}
					codes = append(codes, code)
				}
			}
		}
	}
	if len(codes) == 0 {
		t.Fatal("no Code constant found in this package, so this test could not fail")
	}
	return codes
}

// dashboardCodes is the members of the dashboard's QueryErrorCode union.
//
// FOUND BY ITS NAME AND READ BY ITS SYNTAX, through [clientsource.Union]. This
// read `protocol/types.ts` by path and cut the union at the first `;` once its
// doc comments had been blanked by a regular expression — so a move of the
// file failed the gate for a drift that had not happened, and a comment
// quoting a code, or holding a semicolon, was one regex away from becoming a
// member or ending the union early. `internal/api/stream` is one level deeper
// than the directory [clientsource.Tree] is written against, so it joins the
// extra step itself.
func dashboardCodes(t *testing.T) []string {
	t.Helper()
	codes, err := clientsource.Union("../"+clientsource.Tree, "QueryErrorCode")
	if err != nil {
		// FAILS rather than skips: the dashboard source is committed, so a
		// missing declaration is a renamed one, and a skip would certify
		// nothing.
		t.Fatal(err)
	}
	if len(codes) == 0 {
		t.Fatal("the QueryErrorCode union has no members, so this test could not fail")
	}
	return codes
}
