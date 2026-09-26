package learning

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// attributingModels records whom each resolution was bound to.
type attributingModels struct {
	bound []Attribution
	who   *Attribution
}

func (m *attributingModels) For(who Attribution) Models {
	m.bound = append(m.bound, who)
	return &attributingModels{who: &who}
}

func (m *attributingModels) Head(*org.Role, phase.Phase) (chain.Member, error) {
	return chain.Member{Key: "aux", Provider: silentProvider{}}, nil
}

type silentProvider struct{}

func (silentProvider) Model() string { return "aux-model" }

func (silentProvider) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return &llm.Completion{Content: "NOOP"}, nil
}

// A WORKER'S RESOLUTION CARRIES ITS NAME AND ITS TURN to a Models that records
// spend, and a Models that records nothing is used as it is.
func TestAuxiliaryBindsTheCallersAttribution(t *testing.T) {
	t.Parallel()
	models := &attributingModels{}
	turn := Turn{Role: &org.Role{Name: "Dev"}}
	turn.Event.TurnID, turn.Event.WorkKey = "run-1", "wk-1"
	if _, err := auxiliary(models, turn.Role, turn.attribution(ProfilerSource)); err != nil {
		t.Fatalf("auxiliary: %v", err)
	}
	want := Attribution{Worker: ProfilerSource, TurnID: "run-1", WorkKey: "wk-1"}
	if len(models.bound) != 1 || models.bound[0] != want {
		t.Errorf("bound = %+v, want %+v", models.bound, want)
	}

	plain := staticModels{}
	if _, err := auxiliary(plain, turn.Role, want); err != nil {
		t.Fatalf("auxiliary over a Models that records nothing: %v", err)
	}
}

type staticModels struct{}

func (staticModels) Head(*org.Role, phase.Phase) (chain.Member, error) {
	return chain.Member{Key: "aux", Provider: silentProvider{}}, nil
}

// EVERY AUXILIARY RESOLUTION IN THIS PACKAGE GOES THROUGH [auxiliary].
//
// A worker that resolved its model with Head itself would work — the model is
// the same one — and its every call would be recorded as nobody's: no worker
// row, no turn. Nothing about that call looks wrong in review, so this is read
// off the source: a Head call on a worker's model registry anywhere but in
// auxiliary is the finding.
func TestEveryAuxiliaryResolutionIsAttributed(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		parsed, err := parser.ParseFile(fset, file, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				head, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || head.Sel.Name != "Head" || !namesModels(head.X) {
					return true
				}
				if fn.Name.Name == "auxiliary" {
					checked++
					return true
				}
				t.Errorf("%s resolves an auxiliary model with Head at %s — its calls "+
					"are recorded as nobody's. Resolve through auxiliary(models, role, "+
					"Attribution{Worker: …}) instead.", fn.Name.Name, fset.Position(call.Pos()))
				return true
			})
		}
	}
	// THE SEAM ITSELF IS SEEN, so a guard whose pattern stopped matching
	// the code it is about fails rather than passing over nothing.
	if checked != 1 {
		t.Errorf("found %d Head calls inside auxiliary, want its one — this guard no "+
			"longer recognises how a worker resolves its model", checked)
	}
}

// namesModels reports whether expr is a worker's model registry: a value or a
// field named `models`.
func namesModels(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == "models"
	case *ast.SelectorExpr:
		return e.Sel.Name == "models"
	}
	return false
}
