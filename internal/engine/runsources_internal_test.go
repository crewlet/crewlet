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
// Mutation: hand the launch no guidance or no catalogs, and this goes red.
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
	got := withRunsOwn(runner.AgentRunRequest{Surface: surface}, nil, sandbox.LaunchRequest{Model: "stub/m"})
	if want := map[string]string{"github": "The company's code host."}; !maps.Equal(got.MCPGuidance, want) ||
		got.Model != "stub/m" {
		t.Errorf("the launch is handed %q for %q, want %q for stub/m", got.MCPGuidance, got.Model, want)
	}
	if tools := got.MCPCatalogs["github"]; len(got.MCPCatalogs) != 1 || len(tools) != 1 ||
		tools[0].Name != "create_issue" {
		t.Errorf("the launch is handed the catalogs %+v, want github's create_issue", got.MCPCatalogs)
	}
}

// A LAUNCH IS HANDED THE LEADS ITS FRESH BRIEF OPENED WITH. A turn's first
// launch, and a send-back, carry no continuation to name what their fresh brief
// opened with, the turn's task, which the launch keeps for its job so that a
// later continuation of a session its run starts on that brief, which may be a
// summary, is told the task again.
//
// Mutation: hand the launch no fresh leads, and this goes red.
func TestALaunchIsHandedTheLeadsItsFreshBriefOpenedWith(t *testing.T) {
	t.Parallel()
	fresh := []sandbox.Lead{{Kind: sandbox.LeadKindFresh, Round: 1, Task: &sandbox.LeadTask{}}}
	got := withRunsOwn(runner.AgentRunRequest{FreshOwn: fresh}, nil, sandbox.LaunchRequest{Model: "stub/m"})
	if len(got.FreshOwn) != 1 || got.FreshOwn[0].Kind != sandbox.LeadKindFresh || got.Model != "stub/m" {
		t.Errorf("the launch is handed %+v for %q, want the fresh brief's task for stub/m", got.FreshOwn, got.Model)
	}
}
