package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// holdNames is every name expression a workerHold call passes, keyed by its
// source text, with the names it evaluates to (dutyNames' idiom): a hold is
// no duty, so dutyNames leaves it out, but its lease is named all the same.
// A company key's hold is named by every key companyKeyNames lists.
var holdNames = map[string][]string{
	"advisorDutyName":       {advisorDutyName},
	`"company-key-" + name`: companyKeyHolds(),
	"setupDutyName(kind)":   setupDutyNames(),
}

// companyKeyNames is every name expression a companyKey call passes, with the
// key it names.
var companyKeyNames = map[string]string{"iamdomain.BlindKeyName": iamdomain.BlindKeyName}

// companyKeyHolds is the hold's name for every company key.
func companyKeyHolds() []string {
	var out []string
	for _, name := range companyKeyNames {
		out = append(out, "company-key-"+name)
	}
	return out
}

// setupDutyNames is the provisioning hold of every integration kind.
func setupDutyNames() []string {
	var out []string
	for _, kind := range integration.Kinds {
		out = append(out, setupDutyName(kind))
	}
	return out
}

// EVERY HOLD TAKEN IS IN THE HOLD TABLE, and every name in it is taken, and
// every company key minted is in its table, so the lease test builds every
// hold this build takes rather than a list kept by hand.
//
// Mutation: add a workerHold of a new name, or mint a company key under a new
// name, and this is red until the tables say what it evaluates to.
func TestEveryHoldIsInTheHoldTable(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	var held []string
	for _, site := range dutyClaims(t, root).sites {
		if site.helper != "workerHold" {
			continue
		}
		held = append(held, site.name)
		if _, listed := holdNames[site.name]; !listed {
			t.Errorf("%s takes the hold %s, which holdNames does not list; add it with the names it evaluates "+
				"to", site.where, site.name)
		}
	}
	for expr := range holdNames {
		if !slices.Contains(held, expr) {
			t.Errorf("holdNames lists %q but no workerHold call passes it", expr)
		}
	}
	minted := companyKeyCalls(t, root)
	for _, site := range minted {
		if _, listed := companyKeyNames[site.name]; !listed {
			t.Errorf("%s mints the company key %s, which companyKeyNames does not list", site.where, site.name)
		}
	}
	for expr := range companyKeyNames {
		if !slices.ContainsFunc(minted, func(s dutySite) bool { return s.name == expr }) {
			t.Errorf("companyKeyNames lists %q but no companyKey call passes it", expr)
		}
	}
}

// companyKeyCalls is every call of companyKey in the module, with the name
// expression it passes.
func companyKeyCalls(t *testing.T, root string) []dutySite {
	t.Helper()
	var out []dutySite
	named := sourcetree.MustIdentifiers("companyKey")
	for _, f := range moduleFiles(t, root) {
		if !f.under("internal", "cmd") || !named.In(f.body) {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f.path, f.body, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", f.rel, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && calleeName(call.Fun) == "companyKey" && len(call.Args) > 1 {
				out = append(out, dutySite{where: fset.Position(call.Pos()).String(), name: exprText(fset, call.Args[1])})
			}
			return true
		})
	}
	if len(out) == 0 {
		t.Fatal("premise: no companyKey call found; the scan read nothing")
	}
	return out
}
