package builtin_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// catalogueSpy is the catalogue write side, keeping what it was handed.
type catalogueSpy struct {
	fields []tracker.FieldDef
}

func (s *catalogueSpy) WriteTypes(context.Context, string, []tracker.TaskType) (tracker.WriteResult, error) {
	return tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

func (s *catalogueSpy) WriteFields(_ context.Context, _ string, fields []tracker.FieldDef) (
	tracker.WriteResult, error) {

	s.fields = fields
	return tracker.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}}, nil
}

// A FIELD THE TOOL DECLARES LEAVES WHAT ITS SCHEMA CANNOT SAY TO THE STORED
// DECLARATION. The tool has no argument for the types a field applies to, its
// default or the configuration beside its options, so it marks them unstated
// and the writer keeps each from the declaration the id names — the rule
// internal/tracker's TestAFieldsWriteKeepsWhatItsCallerCannotState holds the
// writer to. A declaration that stated them as empty would clear them from
// every field an operator restated, and publish a restatement as a change.
//
// Mutation: drop the Unstated line from catalogueFields and this fails.
func TestAToolDeclaredFieldLeavesWhatItCannotSayToTheStoredOne(t *testing.T) {
	t.Parallel()
	spy := &catalogueSpy{}
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return spy },
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: "ops", Kind: tracker.AuthorOperator}, nil
			},
		},
	}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	got := callWork(t, reg, tracker.WriteWorkCatalogueTool, map[string]any{
		"fields": []any{map[string]any{
			"id": "f-sev", "slug": "severity", "name": "Severity", "type": "dropdown",
			"options": []any{map[string]any{"id": "o-high", "slug": "high", "name": "High"}},
		}},
	})
	if got.Failed {
		t.Fatalf("the declaration was refused: %s", got.Output)
	}
	if len(spy.fields) != 1 {
		t.Fatalf("the writer was handed %d fields, want 1", len(spy.fields))
	}
	want := tracker.Unstated{AppliesTo: true, Default: true, Config: true}
	if spy.fields[0].Unstated != want {
		t.Errorf("the tool's declaration leaves %+v unstated, want %+v", spy.fields[0].Unstated, want)
	}
}
