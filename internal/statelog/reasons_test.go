package statelog_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// TestEveryRefusalReasonIsInTheMetricsReference holds the refusal counter's
// reference text to the values the counter can carry.
//
// The counter labels every refused write with its [statelog.Reason], or with
// `conflict`, `exists` or `error` beside them ([statelog.Publisher]'s observe),
// and each value has its own remedy. The reference an operator writes collector
// rules from read as a complete list and left out the two reasons the same
// series added, so a rule written from it had no row for either.
func TestEveryRefusalReasonIsInTheMetricsReference(t *testing.T) {
	var shows string
	for _, inst := range metrics.Catalogue() {
		if inst.Name == metrics.StatelogPublishRefusals {
			shows = inst.Shows
		}
	}
	if shows == "" {
		t.Fatalf("the catalogue has no %s instrument", metrics.StatelogPublishRefusals)
	}
	labels := []string{"conflict", "exists", "error"}
	for _, r := range statelog.Reasons() {
		labels = append(labels, string(r))
	}
	for _, label := range labels {
		if !strings.Contains(shows, "`"+label+"`") {
			t.Errorf("%s's reference text never names `%s`, a value its reason "+
				"label carries", metrics.StatelogPublishRefusals, label)
		}
	}
}

// TestReasonsNamesEveryDeclaredReason keeps the enumeration the reference is
// held to from being the next thing to drift: a Reason constant declared and
// left out of [statelog.Reasons] would be a refusal no surface is checked for.
func TestReasonsNamesEveryDeclaredReason(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "outcome.go", nil, 0)
	if err != nil {
		t.Fatalf("parse outcome.go: %v", err)
	}
	var declared []statelog.Reason
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Reason" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok {
					t.Fatalf("a Reason constant whose value is not a literal: %#v", v)
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				declared = append(declared, statelog.Reason(value))
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no Reason constant in outcome.go — the walk reads nothing")
	}
	for _, r := range declared {
		if !slices.Contains(statelog.Reasons(), r) {
			t.Errorf("Reason %q is declared and missing from statelog.Reasons()", r)
		}
		if !r.Valid() {
			t.Errorf("Reason %q is declared and not Valid()", r)
		}
	}
	if len(statelog.Reasons()) != len(declared) {
		t.Errorf("statelog.Reasons() has %d values and outcome.go declares %d",
			len(statelog.Reasons()), len(declared))
	}
	if statelog.Reason("gated").Valid() {
		t.Error("`gated` has no producer and must not read as a reason this build names")
	}
}
