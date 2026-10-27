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
// off the receipt it answers a person with (ADR-0060). A tool that wrote the
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
	// AND EVERY PARTIAL OUTCOME'S KEY, which only [partial] writes beside a
	// caveat. `note` is left out: it is also an argument's name.
	keys := map[string]int{}
	for _, key := range []string{builtin.WarningsKey, builtin.DependenciesFailedKey,
		builtin.MoveStoppedKey, builtin.SubtreeStoppedKey} {
		keys[strconv.Quote(key)] = 0
	}
	fset := token.NewFileSet()
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
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if _, watched := keys[lit.Value]; !watched {
				return true
			}
			keys[lit.Value]++
			if name != "caveats.go" {
				t.Errorf("%s writes %s itself: a tool's caveats and partial "+
					"outcomes reach its answer only through caveated and partial, "+
					"which write the model's words and hand a person's surface the "+
					"person's", fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
	for key, found := range keys {
		if found != 1 {
			t.Errorf("found %s %d times, want once at its constant: the walk is "+
				"reading the wrong source", key, found)
		}
	}
}
