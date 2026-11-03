package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// stubLoader is a tool named as the skill loader: what arms a run's guard on
// its surface, its calls read by the guard by name.
type stubLoader struct{}

func (stubLoader) Name() string               { return toolname.LoadToolSkill }
func (stubLoader) Description() string        { return "loads a tool skill" }
func (stubLoader) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (stubLoader) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "loaded"}, nil
}

// continuingRun is a relaunch of swe's turn that continued the turn's kept
// session in a new box — its box handed the seat's tools under both of the
// job's bridge tokens, and its parked state armed with the required skill
// that guards the probe — over a bridge served at a test server's address,
// which restart puts a fresh process's bridge behind, holding no session.
// continued and fresh are the probe's server beneath each of the job's two
// endpoints, where the box reaches the probe.
type continuingRun struct {
	e                *Engine
	tool             *probeTool
	continued, fresh string
	restart          func()
	pending          sandbox.PendingStore
}

// launchContinuingRun launches swe's turn, keeps its job's session, relaunches
// the turn continuing it in a new box, and parks the turn on the relaunch as
// the engine persists a suspension.
func launchContinuingRun(t *testing.T) continuingRun {
	t.Helper()
	ctx := t.Context()
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	material := runtoken.OneKey("k", "test-key")
	// THE ROUTES A NODE MOUNTS for the bridge (internal/api, mcp.go): the
	// job's endpoint and each of its MCP servers beneath it.
	serve := func(b *mcpbridge.Bridge) {
		mux := http.NewServeMux()
		mux.Handle(mcpbridge.PathPrefix+"{token}", b.Handler())
		mux.Handle(mcpbridge.ServerPattern, b.Handler())
		server.Config.Handler = mux
	}
	launched := bridgeFor(t, mcpbridge.Options{Material: material, BaseURL: server.URL})
	serve(launched)

	c, seat := chainCompany(t, chainMember{key: "api", agent: "claude-code", model: "claude-golden",
		runIn: config.PlacementE2B})
	tool := &probeTool{}
	c.Tools = tools.NewRegistry()
	// THE PROBE IS AN MCP SERVER'S TOOL, never a builtin: every builtin is
	// decided by the authority table, and the surface a seat's recovery
	// rebuilds leaves off one the table refuses before any object is named
	// ([Company.withheldFrom]) — which a builtin with no row, as the probe
	// would be, always is. The loader has its row.
	if err := c.Tools.Register(tool, tools.Origin("probe")); err != nil {
		t.Fatalf("Register(probe): %v", err)
	}
	if err := c.Tools.Register(stubLoader{}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register(%s): %v", toolname.LoadToolSkill, err)
	}
	sessions := &keptLog{rows: map[string]executordomain.Session{}, archives: map[string][]byte{}}
	e, runners := launchRigRestoring(t, c, launched, sessions)
	e = withSessions(t, e)
	// THE REQUIRED SKILL THAT GUARDS THE PROBE, as the company's tool skills
	// arm every guard the seat's runner builds.
	registry := skills.NewRegistry()
	if _, err := registry.PutPage(skills.Skill{Key: "probe-rules", Title: "Probe rules",
		Summary: "how to probe", Body: "Probe with care.", Trigger: skills.Trigger{Tool: "probe"},
		Required: true, SourcePageID: "probe-rules-page"}); err != nil {
		t.Fatalf("PutPage: %v", err)
	}
	e.skills = registry
	jobs := runners["claude-code"]
	jobs.Carries = true
	turn := &turnctx.Turn{RunID: "t1", WorkKey: "wk-1", Reply: "tool",
		WorkSince: time.Now().UTC().Add(-time.Minute), Seat: seat, Org: c.Org, Calls: turnctx.NewCallLog()}
	launcher := &agentLauncher{engine: e, turn: turn, seat: seat, company: c}
	surface := func() *tools.Surface {
		return tools.NewSurface("execute", c.Tools.Snapshot(),
			[]string{"probe", toolname.LoadToolSkill}).ForTurn(turn)
	}
	if _, err := launcher.LaunchExecutor(ctx, runner.AgentRunRequest{Brief: "fix the failing test", Round: 1,
		Fence: e.seatFence(seat.Handle()), Surface: surface()}); err != nil {
		t.Fatalf("the turn's first launch: %v", err)
	}
	pending := e.sandbox.Load().pending
	first, _, err := pending.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// ITS JOB'S SESSION, KEPT, as a collection's capture records it.
	key := objstore.KeyAt(time.Now())
	kept := sandbox.SessionCapture{LaunchSeq: first.LaunchSeq, CaptureSeq: 1, Launch: first.LaunchID,
		State: sandbox.CaptureLanded, Final: true, Harness: "claude-code", HarnessVersion: "2.1.310",
		SessionID: "s-1", ArchiveFormat: sandbox.FakeArchiveFormat, Object: key.String(),
		ObjectHash: strings.Repeat("a", 64), ObjectSize: 14, Home: "/boxes/4f2a/home"}
	if _, err := pending.StampCapture(ctx, "t1", kept); err != nil {
		t.Fatalf("StampCapture: %v", err)
	}
	sessions.mu.Lock()
	sessions.rows["t1"] = executordomain.Session{Handle: seat.Handle(), Turn: "t1", Harness: "claude-code",
		HarnessVersion: "2.1.310", SessionID: "s-1",
		Object:        objstore.Object{Key: key, Hash: objstore.Hash(kept.ObjectHash), Size: kept.ObjectSize},
		ArchiveFormat: sandbox.FakeArchiveFormat, LaunchSeq: kept.LaunchSeq, CaptureSeq: kept.CaptureSeq,
		Home: kept.Home}
	sessions.archives[key.String()] = []byte("the transcript")
	sessions.mu.Unlock()
	// THE RELAUNCH, continuing that session in a new box.
	if _, err := launcher.LaunchExecutor(ctx, runner.AgentRunRequest{Brief: "the fresh brief", Round: 1,
		Fence: e.seatFence(seat.Handle()), Surface: surface(),
		Session: &runner.SessionContinuation{Basis: launcher.ProbeSession(ctx),
			Brief: "carry on in the box you left", Replaced: "carry on in a box that replaced yours"},
	}); err != nil {
		t.Fatalf("the relaunch: %v", err)
	}
	started := jobs.Started()
	last := started[len(started)-1]
	if last.Resume != "s-1" || last.FreshWiring == nil {
		t.Fatalf("the relaunch continues %q wired for a session of its own %+v, want the kept session "+
			"wired both ways", last.Resume, last.FreshWiring)
	}
	if last.MCPServers["probe"].URL == "" || last.FreshWiring.MCPServers["probe"].URL == "" {
		t.Fatalf("the relaunch handed the box no endpoint for the probe's server under one of its "+
			"tokens: %+v, fresh %+v", last.MCPServers, last.FreshWiring.MCPServers)
	}
	// THE SUSPENSION THE RELAUNCHING TURN WRITES: the skill armed for the
	// session its launch continued.
	blob, err := execstate.Encode(execstate.State{Version: execstate.Version, Round: 1,
		ActiveTools: []string{"probe", toolname.LoadToolSkill}, LoadedSkills: []string{"probe-rules"},
		Task: "fix the failing test"})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if ok, err := pending.MarkSuspended(ctx, "t1", sandbox.Suspension{State: blob, Iteration: 1}); err != nil || !ok {
		t.Fatalf("MarkSuspended: %v %v", ok, err)
	}
	return continuingRun{e: e, tool: tool, pending: pending,
		continued: last.MCPServers["probe"].URL,
		fresh:     last.FreshWiring.MCPServers["probe"].URL,
		restart: func() {
			e.bridge = bridgeFor(t, mcpbridge.Options{Material: material, BaseURL: server.URL})
			serve(e.bridge)
		}}
}

// guardedCallRefused calls the probe tool, which the required skill guards,
// through session, answering whether the run's guard refused it. Named apart
// from the package's `probe` type (retention_internal_test.go), which shares
// the package's one namespace with every test file.
func guardedCallRefused(t *testing.T, session *mcp.ClientSession) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "probe",
		Arguments: map[string]any{"value": "after"}})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	return res.IsError
}

// A RUN TAKEN OVER IS HELD TO THE SESSION ITS BOX SAYS IT STARTED, as its record
// names the brief its launch handed it. A node that takes a run over before its
// box first reached the bridge rebuilds the run's surface from its record, which
// says the launch continued a session and nothing of what the box made of it;
// the box then says it on its first request to the reopened bridge, by the
// token its harness dials. Dialled on the token of a session of its own, the
// run's guard holds no skill — its model read none — and a call the required
// skill guards is refused until it loads it, the job's record saying its box
// started its own; dialled on the continued session's token, the guard holds
// the skill the launch armed, the call is made, and the record says the box
// took the session up.
//
// Mutation: arm the reopened run as continued whatever its box says, leave the
// box's word out of the reopened session, or read the record's brief the other
// way round, and a case goes red.
func TestARunTakenOverIsHeldToTheSessionItsBoxSaysAsItsRecordNamesTheBrief(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		fellBack bool
		want     sandbox.SessionStart
	}{
		"its box took the session up":          {want: sandbox.SessionStartContinued},
		"its box started a session of its own": {fellBack: true, want: sandbox.SessionStartFresh},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run := launchContinuingRun(t)
			run.restart()
			row, _, err := run.pending.Get(t.Context(), "t1")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if row.Launch.Brief != sandbox.BriefContinuedNewBox || row.Launch.SessionStart != "" {
				t.Fatalf("the record names brief %q and says %q, want the new box's continuation and no "+
					"word from the box yet", row.Launch.Brief, row.Launch.SessionStart)
			}

			// THE SEAT'S RECOVERY, with the run's row as it reads it.
			run.e.reopenAgentRun(t.Context(), row)

			endpoint := run.continued
			if tc.fellBack {
				endpoint = run.fresh
			}
			session, err := connect(t, endpoint)
			if err != nil {
				t.Fatalf("the box's own endpoint does not reach the taken-over run: %v", err)
			}
			if refused := guardedCallRefused(t, session); refused != tc.fellBack {
				t.Fatalf("the guarded call was refused %v, want %v: the guard is the session's the box says "+
					"it started", refused, tc.fellBack)
			}
			if made := len(run.tool.values()); made != map[bool]int{false: 1, true: 0}[tc.fellBack] {
				t.Errorf("the probe ran %d times", made)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				got, _, err := run.pending.Get(t.Context(), "t1")
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if got.Launch.SessionStart == tc.want {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the job's record says %q, want %q", got.Launch.SessionStart, tc.want)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
