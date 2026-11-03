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

// EVERY VALUE THE CHECK COUNTERS CARRY IS IN THEIR REFERENCE TEXT: every
// fileKind, checkOutcome and checkTrigger the package declares under
// crewlet.code.cache.checks — a pack's files' kinds as constants, and every
// derived file's as its declaration's Owned.File (derived.go) — and every
// checkTrigger under its duration: the rows a collector rule for a damaged
// file is written from. A declaration the check and the scrub pass over
// (Check.hashed: the memo's, whose every read holds its entry) is no value
// the counter carries, and naming it there would send an operator looking for
// a check that never runs.
//
// Mutation: declare another checkOutcome, add an owned file to a hashed
// declaration with a File the text does not name, or drop `midx-rev` from the
// text, and this fails.
func TestEveryCheckValueIsInTheMetricsReference(t *testing.T) {
	shows := map[string]string{}
	for _, inst := range metrics.Catalogue() {
		shows[inst.Name] = inst.Shows
	}
	kinds, outcomes, triggers := declared(t, "fileKind"), declared(t, "checkOutcome"), declared(t, "checkTrigger")
	if !slices.Contains(kinds, string(fileIdx)) || !slices.Contains(outcomes, string(checkReplaced)) ||
		!slices.Contains(triggers, string(triggerFirst)) {
		t.Fatalf("the walk found files %q, outcomes %q and triggers %q — it reads what it should not", kinds,
			outcomes, triggers)
	}
	derived := 0
	for _, d := range DerivedFiles() {
		if !d.Check.hashed() {
			continue // no value the counter carries
		}
		for _, o := range d.Owns {
			kinds, derived = append(kinds, o.File), derived+1
		}
	}
	if derived == 0 {
		t.Fatal("no derived file is one the check hashes — the walk reads nothing of the declarations")
	}
	for name, values := range map[string][]string{
		metrics.CodeCacheChecks:        slices.Concat(kinds, outcomes, triggers),
		metrics.CodeCacheCheckDuration: triggers,
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

// EVERY VALUE THE REPORTS COUNTER CARRIES IS IN ITS REFERENCE TEXT: every
// reportPart and reportOutcome the package declares under
// crewlet.code.cache.reports — `again` is what a disk that keeps losing data
// under the cache reads as, and a value the text does not name is one no
// collector rule has a row for.
//
// Mutation: declare another reportOutcome, or drop `stale` from the text, and
// this fails.
func TestEveryReportValueIsInTheMetricsReference(t *testing.T) {
	var text string
	for _, inst := range metrics.Catalogue() {
		if inst.Name == metrics.CodeCacheReports {
			text = inst.Shows
		}
	}
	if text == "" {
		t.Fatalf("the catalogue has no %s instrument", metrics.CodeCacheReports)
	}
	parts, outcomes := declared(t, "reportPart"), declared(t, "reportOutcome")
	if !slices.Contains(parts, string(reportChunk)) || !slices.Contains(outcomes, string(reportAgain)) {
		t.Fatalf("the walk found parts %q and outcomes %q — it reads what it should not", parts, outcomes)
	}
	for _, v := range slices.Concat(parts, outcomes) {
		if !strings.Contains(text, "`"+v+"`") {
			t.Errorf("%s's reference text never names `%s`, a value it carries", metrics.CodeCacheReports, v)
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
