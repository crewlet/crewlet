package executor_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A CLAUDE CODE RUN'S SERVERS ARE THE ENGINE'S, ITS PLUGIN'S AND ITS ACCOUNT'S:
// the run reads no settings source, so neither the repository's .mcp.json nor
// the box's own servers start ([TestAClaudeCodeRunLoadsOnlyTheSettingsItsLaunchWrites]),
// and no --strict-mcp-config, which would also drop the servers of the run's
// own plugin and the claude.ai connectors of the signed-in account.
//
// Mutation: append --strict-mcp-config beside every file of servers, and this
// fails.
func TestAClaudeCodeRunIsNotHeldToTheEnginesServersAlone(t *testing.T) {
	t.Parallel()
	cmd := scriptOf(t, commandOf(claude(), sandbox.RunRequest{Brief: "x"}, executor.Paths{}, "/box/.crewlet/mcp.json"))
	if !strings.Contains(cmd, "--mcp-config '/box/.crewlet/mcp.json'") {
		t.Fatalf("the engine's servers are not handed to the run:\n%s", cmd)
	}
	if strings.Contains(cmd, "--strict-mcp-config") {
		t.Fatalf("a seat's executor is held to the engine's servers alone:\n%s", cmd)
	}
}
