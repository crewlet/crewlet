package cache

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// EVERY VALUE THE REBUILD AND HINT COUNTERS CARRY IS IN THEIR REFERENCE TEXT.
// The operator's reference for crewlet.code.cache.rebuilds,
// crewlet.code.cache.rebuild.refusals, crewlet.code.cache.hints and
// crewlet.code.cache.hint.hashes is the list a collector rule is written
// from, and each value has its own remedy: every RebuildReason under the
// first's reason, under the second's every reason a git process gives
// (gitReasons) and every rule a rebuild is refused by, and under the hint
// counters every hintOutcome the package declares (a hash's every one but
// `gone`, which is a naming's alone). `shape` was added to the text by hand
// once; the next reason, rule or outcome added to the code would have been a
// value no rule had a row for, failing nothing.
//
// Mutation: drop `shape` from either rebuild text or `unknown` from either
// hint text, or declare a third refusalRule or another hintOutcome, and this
// fails.
func TestEveryRebuildValueIsInTheMetricsReference(t *testing.T) {
	shows := map[string]string{}
	for _, inst := range metrics.Catalogue() {
		shows[inst.Name] = inst.Shows
	}
	rules := declared(t, "refusalRule")
	if len(rules) == 0 {
		t.Fatal("found no refusalRule constant — the walk reads nothing")
	}
	outcomes := declared(t, "hintOutcome")
	if !slices.Contains(outcomes, string(hintGone)) {
		t.Fatalf("found hint outcomes %q, without %q — the walk reads what it should not", outcomes, hintGone)
	}
	for name, values := range map[string][]string{
		metrics.CodeCacheRebuilds:        reasonsOf(RebuildReasons),
		metrics.CodeCacheRebuildRefusals: append(reasonsOf(gitReasons), rules...),
		metrics.CodeCacheHints:           outcomes,
		metrics.CodeCacheHintHashes: slices.DeleteFunc(slices.Clone(outcomes),
			func(o string) bool { return o == string(hintGone) }),
	} {
		text, ok := shows[name]
		if !ok {
			t.Fatalf("the catalogue has no %s instrument", name)
		}
		for _, v := range values {
			if !strings.Contains(text, "`"+v+"`") {
				t.Errorf("%s's reference text never names `%s`, a value it carries", name, v)
			}
		}
	}
}

// RebuildReasons is the enumeration the reference is held to, so a
// RebuildReason declared and left out of it would be a reason no surface is
// checked for — nor accepted by Valid.
func TestRebuildReasonsNamesEveryDeclaredReason(t *testing.T) {
	declaredReasons := declared(t, "RebuildReason")
	if len(declaredReasons) == 0 {
		t.Fatal("found no RebuildReason constant — the walk reads nothing")
	}
	for _, r := range declaredReasons {
		if !slices.Contains(RebuildReasons, RebuildReason(r)) {
			t.Errorf("RebuildReason %q is declared and missing from RebuildReasons", r)
		}
	}
	if len(RebuildReasons) != len(declaredReasons) {
		t.Errorf("RebuildReasons has %d values and the package declares %d", len(RebuildReasons),
			len(declaredReasons))
	}
}

// reasonsOf is rs as strings.
func reasonsOf(rs []RebuildReason) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return out
}

// declared is the value of every string constant of the named type this
// package's non-test files declare, read from the source so a constant
// nothing lists is found.
func declared(t *testing.T, typeName string) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != typeName {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: a %s constant whose value is not a string literal", name, typeName)
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					values = append(values, s)
				}
			}
		}
	}
	return values
}
