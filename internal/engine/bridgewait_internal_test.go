package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// waitedTool is a tool that declares the longest one call of it runs, as
// `delegate` and a seat's MCP server's tools do ([mcpbridge.Bridge.CallTimeout]).
type waitedTool struct {
	name  string
	bound time.Duration
}

func (w waitedTool) Name() string               { return w.name }
func (w waitedTool) Description() string        { return w.name + " takes its time" }
func (w waitedTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (w waitedTool) CallBound() time.Duration   { return w.bound }
func (w waitedTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: w.name + " done"}, nil
}

// EVERY SERVER A BOX IS HANDED WAITS FOR A CALL AS LONG AS ITS BRIDGE CAN TAKE
// TO ANSWER ONE. The bridge keeps a running call's answer going, which answers
// every silence check a harness keeps on a call, so the harness's own tool
// timeout is all that bounds its wait, and the launch hands it the bridge's
// bound: on the engine's own server, and on each of the seat's MCP servers the
// box reaches through the bridge, whose calls the same bridge keeps going and
// answers.
//
// Mutation: hand the box no wait, or hand it to the engine's own server alone,
// and an entry here waits on its harness's own default.
func TestEveryServerABoxIsHandedWaitsAsLongAsItsBridgeCanTakeToAnswer(t *testing.T) {
	t.Parallel()
	c, seat := chainCompany(t, chainMember{key: "api", agent: "claude-code",
		model: "claude-golden", runIn: config.PlacementE2B})
	e, jobs := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	l := launcherOn(e, c, seat, "t1")
	req := launchRequest(l, nil)
	// A SURFACE WITH A TOOL BOUND PAST THE ENGINE'S ONE-CALL CEILING, and one
	// of the seat's MCP servers' tools on it, so the box is handed that
	// server too.
	reg := tools.NewRegistry()
	if err := reg.Register(waitedTool{name: "long_report", bound: 3 * time.Hour}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register(long_report): %v", err)
	}
	if err := reg.Register(waitedTool{name: "create_issue"}, tools.Origin("github")); err != nil {
		t.Fatalf("Register(create_issue): %v", err)
	}
	req.Surface = tools.NewSurface("execute", reg.Snapshot(), []string{"create_issue"}).ForTurn(l.turn)
	if _, err := l.LaunchExecutor(t.Context(), req); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	started := jobs.Started()
	if len(started) != 1 {
		t.Fatalf("%d jobs started, want 1", len(started))
	}
	want := e.bridge.CallTimeout(req.Surface)
	if want < 3*time.Hour {
		t.Fatalf("the bridge's wait for a call is %s, under the surface's longest tool", want)
	}
	for _, name := range []string{config.ToolServerName, "github"} {
		server, handed := started[0].MCPServers[name]
		if !handed {
			t.Errorf("the box was not handed the bridge's %s server", name)
			continue
		}
		if server.CallTimeout != want {
			t.Errorf("the box's %s server waits %s for a call, want its bridge's %s", name, server.CallTimeout, want)
		}
	}
}

// A RELAUNCH'S BOX WAITS AS LONG AS ITS BRIDGE CAN TAKE TO ANSWER A CALL,
// WHICHEVER SESSION IT STARTS. A relaunch that continues the turn's kept
// session wires its box twice: under the token of the session it continues,
// and under the job's other token for a session the box starts of its own
// where it cannot restore that one. Which of the two its harness is handed is
// the box's to find out as it starts, so the servers of both wait for a call
// as long as the bridge can take to answer one.
//
// Mutation: hand the wiring for a session of the box's own no wait, and a box
// that starts one waits on its harness's own default.
func TestARelaunchsBoxWaitsAsLongAsItsBridgeCanTakeToAnswerWhicheverSessionItStarts(t *testing.T) {
	t.Parallel()
	k := newKeptLaunch(t, 40000, 0, 0)
	req := launchRequest(k.l, nil)
	req.Brief = "the fresh brief"
	req.Session = &runner.SessionContinuation{Basis: k.l.ProbeSession(k.ctx),
		Brief: "carry on in the box you left", Replaced: "carry on in a box that replaced yours"}
	if _, err := k.l.LaunchExecutor(k.ctx, req); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	started := k.jobs.Started()
	last := started[len(started)-1]
	if last.Resume != "s-1" || last.FreshWiring == nil {
		t.Fatalf("the relaunch continues %q wired for a session of its own %+v, want the kept "+
			"session wired both ways", last.Resume, last.FreshWiring)
	}
	want := k.e.bridge.CallTimeout(req.Surface)
	if want <= 0 {
		t.Fatalf("the bridge's wait for a call is %s", want)
	}
	for wiring, servers := range map[string]map[string]sandbox.MCPServer{
		"the session it continues":   last.MCPServers,
		"a session of the box's own": last.FreshWiring.MCPServers,
	} {
		server, handed := servers[config.ToolServerName]
		if !handed {
			t.Errorf("the box's wiring for %s has no %s server", wiring, config.ToolServerName)
			continue
		}
		if server.CallTimeout != want {
			t.Errorf("the box's wiring for %s waits %s for a call, want its bridge's %s",
				wiring, server.CallTimeout, want)
		}
	}
}
