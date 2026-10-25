package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/authz"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
)

// EVERY CURATED WRITE EXECUTES AS AN OPERATOR TOOL IT CAN STAND ON.
//
// Walked over the operator catalogue with every half on. A row of the
// catalogue's exposure table naming an operator tool this build does not serve
// is a curated write that fails on every call; a row's tool is never a proven
// read; and its verb is one the authority table lets an approval carry. Every
// other operator tool is the person's own through the generic change, on the
// act route, decided by its own verb (ADR-0051). And each catalogue write that
// executes as an operator tool is held to it: the destructive hint its tier is
// derived from is the tool's own annotation, and every argument the model may
// send is one the act tool declares, of the same type, so an approved request
// is one the tool reads whole. Mutation: name an operator tool that is not
// served, mark update_task not destructive, or declare an argument
// create_work_item does not take, and this fails.
func TestEveryCuratedWriteExecutesAsAnOperatorTool(t *testing.T) {
	t.Parallel()
	served := map[string]map[string]any{}
	for _, tool := range builtin.OperatorTools(operatorDeps(t)) {
		served[tool.Name()] = tool.Parameters()
	}
	if len(served) < 40 {
		t.Fatalf("the operator catalogue serves %d tools, so every half is not on", len(served))
	}
	for _, e := range catalog.Exposures() {
		if e.Tool == "" {
			continue
		}
		if _, ok := served[e.Tool]; !ok {
			t.Errorf("%s executes as the operator tool %s, which this build does not serve",
				e.Name, e.Tool)
			continue
		}
		if crewletmcp.ReadOnlyProven(builtin.AnnotationsFor(e.Tool)) {
			t.Errorf("%s is a proven read and the catalogue proposes it as a write", e.Tool)
		}
		if class, _ := authz.AssistOf(authz.Action(e.Tool)); class != authz.AssistApprove {
			t.Errorf("%s is %q to the assistant and a curated write proposes it", e.Tool, class)
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
