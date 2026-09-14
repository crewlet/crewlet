package stream_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// dashboardProtocol is the dashboard's own declaration of the wire, as SOURCE:
// the dashboard's source is committed, so it is in every checkout, and the
// built bundle would be a step behind any change a branch makes to it.
const dashboardProtocol = "../../../dashboard/src/protocol/types.ts"

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
func dashboardCodes(t *testing.T) []string {
	t.Helper()
	source, err := os.ReadFile(filepath.FromSlash(dashboardProtocol))
	if err != nil {
		// FAILS rather than skips: the dashboard source is committed, so a
		// missing file is a moved file, and a skip would certify nothing.
		t.Fatalf("read the dashboard's protocol types: %v", err)
	}
	text := string(source)
	const head = "export type QueryErrorCode ="
	start := strings.Index(text, head)
	if start < 0 {
		t.Fatalf("%s declares no QueryErrorCode union", dashboardProtocol)
	}
	// The members' doc comments are prose: they quote words ("there is
	// nothing") and may carry a semicolon of their own. They go BEFORE the
	// union's terminating semicolon is looked for, or a comment could end the
	// union early or add a quoted word as a member.
	body := regexp.MustCompile(`(?s)/\*.*?\*/|//[^\n]*`).ReplaceAllString(text[start+len(head):], "")
	end := strings.Index(body, ";")
	if end < 0 {
		t.Fatalf("the QueryErrorCode union in %s never ends", dashboardProtocol)
	}
	body = body[:end]
	var codes []string
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(body, -1) {
		codes = append(codes, m[1])
	}
	if len(codes) == 0 {
		t.Fatal("the QueryErrorCode union has no members, so this test could not fail")
	}
	return codes
}
