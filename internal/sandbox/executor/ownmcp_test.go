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
// Mutation: append --strict-mcp-config beside every file of servers, as the
// runner used to, and this fails.
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

// A RUN HELD TO THE SERVERS IT IS HANDED — the assistant's deep work — starts
// no other, whether or not it is handed any.
//
// Mutation: append --strict-mcp-config only beside a file of servers, and the
// run handed none starts every server its configuration declares.
func TestARunHeldToItsServersStartsNoOther(t *testing.T) {
	t.Parallel()
	for _, configPath := range []string{"/box/.crewlet/mcp.json", ""} {
		cmd := scriptOf(t, commandOf(claude(), sandbox.RunRequest{Brief: "x", OnlyMCPServers: true},
			executor.Paths{}, configPath))
		if !strings.Contains(cmd, "--strict-mcp-config") {
			t.Fatalf("handed %q, a run held to its servers may start the box's and the "+
				"repository's too:\n%s", configPath, cmd)
		}
	}
}

// A HARNESS THAT CANNOT BE HELD TO THE SERVERS IT IS HANDED REFUSES A RUN THAT
// ASKS IT TO, rather than start it on every server its configuration adds up
// to: OpenCode, which adds together every configuration document it reads.
//
// Mutation: drop the mcpHolder gate from Runner.Start, and the run starts.
func TestARunnerThatCannotHoldItsServersRefusesToPretend(t *testing.T) {
	t.Parallel()
	runner := executor.NewOpenCode()
	b := box(t, runner)
	_, err := runner.Start(t.Context(), b, sandbox.RunRequest{Brief: "x", OnlyMCPServers: true})
	if err == nil {
		t.Fatal("OpenCode started a run held to the servers it was handed, which it cannot keep to")
	}
	if !strings.Contains(err.Error(), "opencode") {
		t.Errorf("the refusal does not name the harness: %v", err)
	}
}
