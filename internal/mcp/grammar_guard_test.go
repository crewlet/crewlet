package mcp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestInstanceSeparatorHasOneDefinition fails the build on a hand-built
// per-role instance name.
//
// The queue's topic grammar carries the same guard for the same reason: a
// producer and a consumer that disagree about a name never raise. The lookup
// simply misses, and a server the prompt advertises answers "not configured
// for this role" — which reads as a config problem and is not one.
//
// SCOPE, stated because a guard whose reach is assumed is worse than none:
// this walks THIS PACKAGE ONLY, source and tests. When the engine grows a
// caller that builds instance names — the seat cascade will — this test must
// be widened to that package or the same drift becomes possible one directory
// away.
func TestInstanceSeparatorHasOneDefinition(t *testing.T) {
	t.Parallel()
	// Two files may spell the separator: the grammar, and the grammar's own
	// test — where the literal IS the assertion. Anywhere else it is a second
	// definition wearing a string literal.
	allowed := map[string]bool{
		"instance.go":           true,
		"instance_test.go":      true,
		"grammar_guard_test.go": true,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || allowed[name] {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.Contains(value, "::") {
				return true
			}
			offenders = append(offenders,
				fset.Position(lit.Pos()).String()+": "+lit.Value)
			return true
		})
	}
	for _, o := range offenders {
		t.Errorf("hand-built instance separator: %s — use InstanceName / ServerName from instance.go", o)
	}
}
