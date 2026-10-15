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

	"github.com/crewlet/crewlet/internal/api/httpjson"
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
	got := dashboardUnion(t, "QueryErrorCode")
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's QueryErrorCode union is %v; the engine sends %v and "+
			"the socket adds %v, so the two vocabularies have drifted. A code only "+
			"the dashboard knows is a branch that never runs; a code only the engine "+
			"sends is a failure every screen renders as unknown",
			got, engineCodes(t), clientOnlyCodes)
	}
}

// THE SOCKET AND REST SHARE ONE REFUSAL TABLE.
//
// The socket answers a query with a code, and the query surface's REST twin
// answers the same question with a code, and the dashboard asks over both —
// the socket while it is open, plain HTTP when it is deciding whether its
// token is the reason it is not. Two vocabularies there means one failure with
// two names, which is how the dashboard came to handle `no_event_store`: a
// code that existed on one side of the protocol and nowhere else, branched on
// by a screen, sent by nothing.
//
// So every code this package declares must be an entry on [httpjson.Code]'s
// table — the one the REST routes answer from, and the one that carries the
// sentence a person reads for each code. Read from this package's own source,
// so a code added here without an entry there fails without being listed.
func TestTheSocketAndRestShareOneTable(t *testing.T) {
	t.Parallel()
	if unknown := notOnTheRESTTable(engineCodes(t)); len(unknown) > 0 {
		t.Errorf("the socket answers with %v, which the REST refusal table does "+
			"not carry: add each to internal/api/httpjson with the sentence a "+
			"person is shown for it, or the same failure has two names and no copy",
			unknown)
	}
	// The control: the walk is worth having only if a code with no entry
	// fails it. `no_event_store` is the one that actually happened.
	if got := notOnTheRESTTable([]string{"no_event_store"}); len(got) != 1 {
		t.Errorf("a code with no entry on the REST table passed the walk (%v), "+
			"so this test is asserting nothing", got)
	}
}

// notOnTheRESTTable is the codes with no entry in the engine's one refusal
// vocabulary.
func notOnTheRESTTable(codes []string) []string {
	var out []string
	for _, code := range codes {
		if !httpjson.Code(code).Valid() {
			out = append(out, code)
		}
	}
	return out
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

// dashboardUnion is the members of the string-literal union type the
// dashboard declares under name.
//
// FOUND BY ITS NAME AND READ BY ITS SYNTAX, through [clientsource.Union]. This
// read `protocol/types.ts` by path and cut the union at the first `;` once its
// doc comments had been blanked by a regular expression — so a move of the
// file failed the gate for a drift that had not happened, and a comment
// quoting a code, or holding a semicolon, was one regex away from becoming a
// member or ending the union early.
func dashboardUnion(t *testing.T, name string) []string {
	t.Helper()
	members, err := clientsource.Union(clientsource.Tree(t), name)
	if err != nil {
		// FAILS rather than skips: the dashboard source is committed, so a
		// missing declaration is a renamed one, and a skip would certify
		// nothing.
		t.Fatal(err)
	}
	if len(members) == 0 {
		t.Fatalf("the %s union has no members, so this test could not fail", name)
	}
	return members
}
