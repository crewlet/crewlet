package executor_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A SEAT'S EXECUTOR STARTS THE MCP SERVERS ITS OWN CONFIGURATION DECLARES beside
// the ones the engine hands it: no --strict-mcp-config, so the box's own
// servers, the repository's .mcp.json, a plugin's and the signed-in account's
// connectors load, as they do for a person running Claude Code.
//
// Mutation: append --strict-mcp-config beside every file of servers, and this
// fails.
func TestASeatsClaudeCodeRunStartsItsOwnMCPServersToo(t *testing.T) {
	t.Parallel()
	cmd := scriptOf(t, commandOf(claude(), sandbox.RunRequest{Brief: "x"}, executor.Paths{}, "/box/.crewlet/mcp.json"))
	if !strings.Contains(cmd, "--mcp-config '/box/.crewlet/mcp.json'") {
		t.Fatalf("the engine's servers are not handed to the run:\n%s", cmd)
	}
	if strings.Contains(cmd, "--strict-mcp-config") {
		t.Fatalf("a seat's executor is held to the engine's servers alone:\n%s", cmd)
	}
}
