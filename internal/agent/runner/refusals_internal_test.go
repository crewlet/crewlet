package runner

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/mcp"
)

// A TURN COUNTS EVERY CALL THE ENGINE REFUSED, BY CLASS: the engine's own tools
// under no server, an MCP server's under its name, and never a success or a
// third-party server's own failure, which the engine does not classify.
func TestARefusedCallIsCountedByClass(t *testing.T) {
	t.Parallel()
	execs := []toolloop.Execution{
		{Name: "merge_change", Failed: true, Refusal: mcp.RefusalForbidden, Source: "builtin"},
		{Name: "merge_change", Failed: true, Refusal: mcp.RefusalForbidden, Source: "builtin"},
		{Name: "github_merge_pull_request", Failed: true, Refusal: mcp.RefusalUnavailable,
			Source: mcp.Origin("github")},
		{Name: "github_get_issue", Source: mcp.Origin("github")},
		{Name: "github_create_issue", Failed: true, Source: mcp.Origin("github")},
	}
	want := []types.RefusalTally{
		{Tool: "merge_change", Class: string(mcp.RefusalForbidden), Calls: 2},
		{Server: "github", Tool: "github_merge_pull_request", Class: string(mcp.RefusalUnavailable), Calls: 1},
	}
	if got := refusalTallies(execs); !reflect.DeepEqual(got, want) {
		t.Fatalf("refusals %+v, want %+v", got, want)
	}
}
