package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/authz"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
)

// EVERY OPERATOR TOOL HAS ONE ASSISTANT EXPOSURE, and the decision can stand.
//
// Walked over the operator catalogue with every half on, so a tool added to
// it is a red build that says "classify this tool for the assistant". A row
// naming no tool is a decision about nothing; a write row's tool is never a
// proven read; a tool whose verb the authority table keeps from the assistant
// can only be Never. And each catalogue write that executes as an operator tool
// is held to it: the destructive hint its tier is derived from is the tool's
// own annotation, and every argument the model may send is one the act tool
// declares, of the same type, so an approved request is one the tool reads
// whole. Mutation: add a tool with no row, mark update_task not destructive,
// or declare an argument create_work_item does not take, and this fails.
func TestEveryOperatorToolHasOneAssistantExposure(t *testing.T) {
	t.Parallel()
	served := map[string]map[string]any{}
	for _, tool := range builtin.OperatorTools(operatorDeps(t)) {
		served[tool.Name()] = tool.Parameters()
	}
	if len(served) < 40 {
		t.Fatalf("the operator catalogue serves %d tools, so every half is not on", len(served))
	}
	rows := map[string][]catalog.Exposure{}
	for _, e := range catalog.Exposures() {
		if e.Tool != "" {
			rows[e.Tool] = append(rows[e.Tool], e)
		}
	}
	for name := range served {
		got := rows[name]
		if len(got) != 1 {
			t.Errorf("%s has %d assistant exposure rows; give it exactly one in "+
				"internal/assistant/catalog", name, len(got))
			continue
		}
		e := got[0]
		if e.As == catalog.AsWriteTool && crewletmcp.ReadOnlyProven(builtin.AnnotationsFor(name)) {
			t.Errorf("%s is a proven read and the catalogue proposes it as a write", name)
		}
		if class, _ := authz.AssistOf(authz.Action(name)); class == authz.AssistNever &&
			e.As != catalog.AsNever {
			t.Errorf("%s is kept from the assistant by the authority table and exposed as %s",
				name, e.As)
		}
	}
	for name := range rows {
		if _, ok := served[name]; !ok {
			t.Errorf("the catalogue decides about the operator tool %s, which this build "+
				"does not serve", name)
		}
	}
	for _, tool := range catalog.Tools() {
		if tool.Kind != catalog.KindWrite {
			continue
		}
		params, ok := served[tool.Act]
		if !ok && strings.Contains(tool.Act, ".") {
			// A VERB NO TOOL IS NAMED FOR (the principles'): the write is
			// sent on its own route, which internal/api's route walk holds.
			continue
		}
		if !ok {
			t.Errorf("%s executes as %s, which the operator catalogue does not serve",
				tool.Name, tool.Act)
			continue
		}
		destructive := builtin.AnnotationsFor(tool.Act).Destructive == crewletmcp.Yes
		if tool.Destructive != destructive {
			t.Errorf("%s states destructive %v and %s's annotation says %v", tool.Name,
				tool.Destructive, tool.Act, destructive)
		}
		if len(tool.Routes) != 1 || tool.Routes[0] != catalog.ActRoute {
			continue // the shares execute on the artifacts surface, not the act route
		}
		actProps, _ := params["properties"].(map[string]any)
		props, _ := tool.Parameters["properties"].(map[string]any)
		for arg, p := range props {
			theirs, declared := actProps[arg].(map[string]any)
			if !declared {
				t.Errorf("%s takes %q, which %s does not declare", tool.Name, arg, tool.Act)
				continue
			}
			if !sameType(p.(map[string]any)["type"], theirs["type"]) {
				t.Errorf("%s takes %q as %v and %s as %v", tool.Name, arg,
					p.(map[string]any)["type"], tool.Act, theirs["type"])
			}
		}
	}
}

// sameType reports whether a catalogue argument's type is one the act tool's
// schema admits: the same type, or one of a list it names (a nullable field).
func sameType(ours, theirs any) bool {
	if ours == theirs {
		return true
	}
	switch list := theirs.(type) {
	case []any:
		for _, t := range list {
			if t == ours {
				return true
			}
		}
	case []string:
		for _, t := range list {
			if t == ours {
				return true
			}
		}
	}
	return false
}
