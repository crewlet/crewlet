package mcp_test

import (
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/mcp"
)

// THE DASHBOARD STATES THE ENGINE'S TIMEOUT DEFAULTS.
//
// The MCP server wizard's start and call timeout fields say what an empty
// field means from `MCP_DEFAULT_STARTUP_TIMEOUT_SECONDS` and
// `MCP_DEFAULT_REQUEST_TIMEOUT_SECONDS` (`contract/mcp.ts`): a copy that
// drifted would tell an administrator a bound the engine does not apply.
//
// Mutation: change either default on either side, and this fails.
func TestTheDashboardStatesTheEnginesTimeoutDefaults(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		want float64
	}{
		{"MCP_DEFAULT_STARTUP_TIMEOUT_SECONDS", mcp.DefaultStartupTimeout.Seconds()},
		{"MCP_DEFAULT_REQUEST_TIMEOUT_SECONDS", mcp.DefaultRequestTimeout.Seconds()},
	} {
		got, err := clientsource.Scalar(clientsource.Tree(t), c.name)
		if err != nil {
			t.Fatal(err)
		}
		if want := strconv.FormatFloat(c.want, 'f', -1, 64); got != want {
			t.Errorf("the dashboard's %s is %s, want the engine's %s", c.name, got, want)
		}
	}
}
