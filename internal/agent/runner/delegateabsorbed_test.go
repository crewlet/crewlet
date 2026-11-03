package runner_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/tools"
)

// leaveRoom is a builtin a worker may be granted, writing no shared surface: it
// stands in for a seat's `leave_channel`, which a family counts beside the
// joins it undoes.
type leaveRoom struct{}

func (leaveRoom) Name() string        { return "leave_room" }
func (leaveRoom) Description() string { return "Leave a room." }
func (leaveRoom) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"channel": map[string]any{"type": "string"}}}
}
func (leaveRoom) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "left"}, nil
}

// leavingWorker is a worker's model: it leaves room R, then submits.
type leavingWorker struct{}

func (leavingWorker) Model() string { return "scripted" }

func (leavingWorker) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	if req.Tools == nil {
		return nil, errors.New("leavingWorker: asked with no tools, so not by a worker")
	}
	return llmtest.Drive(ctx, req, llmtest.ConnOptions{}, func(c *llmtest.Conn) (llmtest.Response, error) {
		if !slices.Contains(c.OfferedNames(), subagent.SubmitTool) {
			return llmtest.Response{}, errors.New("leavingWorker: asked by something that is not a worker")
		}
		if len(c.Played()) == 0 {
			return llmtest.Response{Calls: []llmtest.Call{{Name: "leave_room", Args: `{"channel":"R"}`}}}, nil
		}
		return llmtest.Response{Calls: []llmtest.Call{{Name: subagent.SubmitTool, Args: `{"result":"left R"}`}}}, nil
	})
}

// A BRIDGED DELEGATE'S WORKERS ARE COUNTED ON ITS OWN RECORD. The real
// `delegate`, made over an executor's bridged surface the way the bridge makes
// a call — admitted, then run — forks its workers' call logs from the call's
// own log, so the call's result hands on what they called
// (toolloop.ToolResult.Absorbed), which the bridge records with the call and
// the run's log tallies. A log seeded from that record — the call by its
// identity, and what it absorbed — counts the worker's call exactly as the run's
// own log does: a later call to the family after the worker's leave is no
// retry of a call before it.
//
// Mutation: fork the workers from the tool's own turn rather than the one the
// call was admitted under, and the worker's call reaches the run's log and no
// record of the call; a resumed run counts one call fewer than its process did.
func TestABridgedDelegatesWorkersAreCountedOnItsOwnRecord(t *testing.T) {
	t.Parallel()
	launcher := &playLauncher{}
	reg := tools.NewRegistry()
	if err := reg.RegisterWith(leaveRoom{}, tools.OriginBuiltin,
		tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.No, OpenWorld: mcp.No}); err != nil {
		t.Fatal(err)
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: leavingWorker{}}})
	if err != nil {
		t.Fatal(err)
	}
	calls := turnctx.NewCallLog()
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}}, Role: role},
		Registry: reg, Models: models,
		Task:     "fan this out",
		Launcher: launcher,
		Subagent: &runner.SubagentConfig{Limits: shipped()},
		Turn: runner.Turn{RunID: "t-delegate", AgentID: "agent-1",
			Context: &turnctx.Turn{RunID: "t-delegate", WorkKey: "wk-delegate", Calls: calls}},
		Sessions: sessions,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	if w, _, err := r.Execute(context.Background(), 1, "", nil); err != nil || !w.Suspended {
		t.Fatalf("Execute = %+v, %v; want a parked run", w, err)
	}
	req := launcher.last(t)
	if !req.Surface.Activate("leave_room") {
		t.Fatal("the bridged surface does not resolve leave_room")
	}
	args := map[string]any{"tasks": []any{map[string]any{
		"id": "leave", "prompt": "leave R", "system_prompt": "you leave rooms",
		"tools": []any{"leave_room"}}}}

	// AS THE BRIDGE MAKES IT: admitted, then run.
	res, err := req.Surface.Admit(subagent.ToolName, args)(context.Background(),
		llm.ToolCall{Name: subagent.ToolName})
	if err != nil || res.Failed {
		t.Fatalf("the bridged delegate = %+v, %v", res, err)
	}
	other := map[string]any{"channel": "S"}
	live := calls.Ordinal("leave_room", other)
	if live != 1 {
		t.Fatalf("the run's own log counts %d of the worker's calls, want its one: the case proves nothing "+
			"(the delegate answered %q)", live, res.Output)
	}
	// EVERY CALL THE RUN'S LOG TOOK IN FROM THE DELEGATE, after the delegate
	// itself — the worker's leave, and its submission of its result — is what
	// the call's result carries, in the order the log took them in.
	entries := calls.Own()
	at := slices.IndexFunc(entries, func(c turnctx.Called) bool { return c.Tool == subagent.ToolName })
	if at < 0 {
		t.Fatalf("the run's own log holds %+v, without the delegate it admitted", entries)
	}
	var want []toolloop.Called
	for _, c := range entries[at+1:] {
		want = append(want, toolloop.Called{Tool: c.Tool, Digest: c.Digest})
	}
	leave := toolloop.Called{Tool: "leave_room", Digest: turnctx.ArgsDigest(map[string]any{"channel": "R"})}
	if !slices.Contains(want, leave) {
		t.Fatalf("the run's own log took in %+v from the delegate, without the worker's leave", want)
	}
	if !slices.Equal(res.Absorbed, want) {
		t.Errorf("the delegate's result carries %+v, want every call the run's log took in from it, %+v",
			res.Absorbed, want)
	}
	// WHAT THE LOG HOLDS OF THE CALL, as a resumed segment seeds it: the
	// delegate by its identity, and what its work absorbed.
	seeded := turnctx.NewCallLog()
	seeded.Seed(turnctx.SeededCall{Tool: subagent.ToolName,
		Digest: req.Surface.DigestOf(subagent.ToolName, args)})
	for _, a := range res.Absorbed {
		seeded.Seed(turnctx.SeededCall{Tool: a.Tool, Digest: a.Digest})
	}
	if got := seeded.Ordinal("leave_room", other); got != live {
		t.Errorf("a log seeded from the delegate's record counts %d of its worker's calls, where the run's "+
			"own log counts %d", got, live)
	}
}
