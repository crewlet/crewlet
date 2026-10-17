package engine

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
)

// namedTool is a tool as far as a registry can tell: a name, and nothing a
// call could reach.
type namedTool string

func (n namedTool) Name() string             { return string(n) }
func (namedTool) Description() string        { return "a tool with nothing behind it" }
func (namedTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (namedTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "ok"}, nil
}

// A SERVER'S TOOL LEAVING TAKES NO BUILTIN WITH IT. A server tool that shares
// a builtin's name was refused when it was filed, so the name in the registry
// is the builtin's; when the server stops offering it, the change names it as
// removed, and unregistering by name alone deleted the builtin from the epoch
// — a seat lost lookup_colleague because a vendor renamed a tool. An MCP
// tool of the name is still removed, which is what the change is for.
func TestAServerToolLeavingTakesNoBuiltinOfItsName(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	if err := reg.Register(namedTool("lookup_colleague"), tools.OriginBuiltin); err != nil {
		t.Fatalf("register the builtin: %v", err)
	}
	if err := reg.Register(namedTool("tracker_search"), tools.Origin("tracker")); err != nil {
		t.Fatalf("register the server's tool: %v", err)
	}
	file(t.Context(), reg, "tracker",
		mcp.Change{Removed: []string{"lookup_colleague", "tracker_search", "never_filed"}}, nil)

	if _, ok := reg.Lookup("lookup_colleague"); !ok {
		t.Error("a server that stopped offering a tool named like a builtin took the builtin with it")
	}
	if _, ok := reg.Lookup("tracker_search"); ok {
		t.Error("the server's own tool is still filed after the change removed it")
	}
}
