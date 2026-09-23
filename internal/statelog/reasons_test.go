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
	var declared []statelog.Reason
	for _, value := range declaredConstants(t, "outcome.go", "Reason") {
		declared = append(declared, statelog.Reason(value))
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

// TestGateActionsNamesEveryDeclaredAction is the same guard for the gate's
// remedies: the dashboard's copy of the actions is held to
// [statelog.GateActions] (internal/api's client gate), so an action declared
// and left out of it is a remedy the engine sends and no surface is checked for.
func TestGateActionsNamesEveryDeclaredAction(t *testing.T) {
	declared := declaredConstants(t, "gateremedy.go", "GateAction")
	if len(declared) == 0 {
		t.Fatal("found no GateAction constant in gateremedy.go — the walk reads nothing")
	}
	for _, a := range declared {
		if !slices.Contains(statelog.GateActions(), statelog.GateAction(a)) {
			t.Errorf("GateAction %q is declared and missing from statelog.GateActions()", a)
		}
		if !statelog.GateAction(a).Valid() {
			t.Errorf("GateAction %q is declared and not Valid()", a)
		}
	}
	if len(statelog.GateActions()) != len(declared) {
		t.Errorf("statelog.GateActions() has %d values and gateremedy.go declares %d",
			len(statelog.GateActions()), len(declared))
	}
}

// declaredConstants is the value of every constant of the named type declared
// in one file of this package.
func declaredConstants(t *testing.T, file, typ string) []string {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var out []string
	for _, decl := range parsed.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != typ {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok {
					t.Fatalf("a %s constant whose value is not a literal: %#v", typ, v)
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				out = append(out, value)
			}
		}
	}
	return out
}

// A REMEDY THAT IS DONE FIRST STILL FINISHES THE SAME GESTURE.
//
// A log refused `log_full` or `wrong_stream` cannot be finished by sending the
// gesture again straight away — and advising no operation id at all sent an
// operator who had raised the ceiling to run the eviction AFRESH, which wrote
// every log that already held the first record again and re-dated its
// eviction. The gesture's own id is how each of these is finished; only a new
// gesture's is not.
func TestAGateActionSaysWhetherTheGesturesOwnIDFinishesIt(t *testing.T) {
	keeps := map[statelog.GateAction]bool{
		statelog.GateRetrySameOp: true,
		statelog.GateOtherNode:   true,
		statelog.GateReanchor:    true,
		statelog.GateSetCapacity: true,
		statelog.GateNewGesture:  false,
		statelog.GateForce:       false,
		statelog.GateRestore:     false,
		statelog.GateWait:        false,
	}
	for _, a := range statelog.GateActions() {
		want, listed := keeps[a]
		if !listed {
			t.Errorf("action %q has no expectation here — decide whether its "+
				"gesture is finished under its own id", a)
			continue
		}
		if got := a.KeepsOperation(); got != want {
			t.Errorf("%q.KeepsOperation() = %v, want %v", a, got, want)
		}
		if got := slices.Contains(statelog.GateActionsKeepingOperation(), a); got != want {
			t.Errorf("GateActionsKeepingOperation() lists %q = %v, want %v", a, got, want)
		}
	}
}
