package engine

import (
	"maps"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// A LAUNCH IS HANDED WHAT ITS BRIDGED SERVERS SAY THEY ARE FOR. OpenCode hands
// a session in the box that holds its copy the instructions of the MCP
// servers the run's bridge serves it, before its first request, so the launch
// that judges that request in the box is handed them to charge, the rest of
// its request as the launcher built it.
//
// Mutation: hand the launch no guidance, and this goes red.
func TestALaunchIsHandedWhatItsBridgedServersSay(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	reg := tools.NewRegistry()
	if err := reg.Register(waitedTool{name: "create_issue"}, tools.Origin("github")); err != nil {
		t.Fatalf("Register(create_issue): %v", err)
	}
	reg.DescribeServers(map[string]string{"github": "The company's code host."})
	surface := tools.NewSurface("execute", reg.Snapshot(), []string{"create_issue"}).
		ForTurn(&turnctx.Turn{RunID: "run-1", Seat: seat})
	got := withRunSources(runner.AgentRunRequest{Surface: surface}, sandbox.LaunchRequest{Model: "stub/m"})
	if want := map[string]string{"github": "The company's code host."}; !maps.Equal(got.MCPGuidance, want) ||
		got.Model != "stub/m" {
		t.Errorf("the launch is handed %q for %q, want %q for stub/m", got.MCPGuidance, got.Model, want)
	}
}
