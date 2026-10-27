package builtin_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
)

// ONE WRITER OF A TOOL'S WARNINGS. A tool's answer carries its caveats in the
// model's words under [builtin.WarningsKey], and the act route takes that key
// off the receipt it answers a person with (ADR-0059). A tool that wrote the
// key itself (a nested `warnings` under one half of its answer, a list of
// sentences that never became caveats) would hand a model's advice to a
// person's screen, or a person's surface nothing at all: the incident behind
// this was exactly such a list, rendered verbatim as a toast that stayed.
//
// So the spelling appears once in this package's source, at the constant, and
// every caveat reaches an answer through the one helper that writes both
// phrasings from the same values.
func TestOnlyTheCaveatHelperWritesAToolsWarnings(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	quoted := strconv.Quote(builtin.WarningsKey)
	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || lit.Value != quoted {
				return true
			}
			found++
			if name != "caveats.go" {
				t.Errorf("%s writes %s itself: a tool's caveats reach its answer "+
					"only through caveated, which writes the model's words and "+
					"hands a person's surface the person's", fset.Position(lit.Pos()), quoted)
			}
			return true
		})
	}
	if found != 1 {
		t.Errorf("found %s %d times, want once at WarningsKey: the walk is "+
			"reading the wrong source", quoted, found)
	}
}
