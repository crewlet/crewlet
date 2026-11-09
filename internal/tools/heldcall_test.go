package tools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// recordingHolder is a [tools.Holder] that records what it was handed and
// answers held.
type recordingHolder struct {
	held []tools.HeldCall
}

func (h *recordingHolder) Hold(_ context.Context, _ *turnctx.Turn, call tools.HeldCall) (tools.Result, bool) {
	h.held = append(h.held, call)
	return tools.Result{Output: "held for a person"}, true
}

// approvalRegistry is a registry with one MCP tool at Needs approval.
func approvalRegistry(t *testing.T) (*tools.Registry, *fakeTool) {
	t.Helper()
	reg := tools.NewRegistry()
	closer := tool("close_issue")
	if err := reg.RegisterWith(closer, tools.Origin("tracker"), tools.Annotations{},
		func(e *tools.Entry) { e.NeedsApproval = true }); err != nil {
		t.Fatal(err)
	}
	return reg, closer
}

// A CALL TO A TOOL AT NEEDS APPROVAL NEVER RUNS ON THE SURFACE IT REACHES. An
// executor's surface hands it to its holder, which records it as the run's
// question, and answers it held and failed, its arguments left out of the
// surface's record; every other phase's surface refuses it as forbidden and
// tells the model the seat's own run may ask. Only a call a person approved
// runs, through [tools.Surface.RunApproved], once, and is recorded whole.
//
// Mutation: drop the approval gate from Surface.execute, and the tool runs on
// both surfaces.
func TestACallToAToolAtNeedsApprovalNeverRunsUntilAPersonApprovesIt(t *testing.T) {
	t.Parallel()
	reg, closer := approvalRegistry(t)
	call := llm.ToolCall{Name: "close_issue", Arguments: map[string]any{"id": "7"}}

	host := tools.NewSurface("review", reg.Snapshot(), []string{"close_issue"})
	res, err := host.Execute(t.Context(), call)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Failed || res.Refusal() != tools.RefusalForbidden || res.Held ||
		!strings.Contains(res.Output, "own run, its executor, can ask") {
		t.Fatalf("a host phase answered %+v, want it refused as forbidden, saying the seat's run may ask", res)
	}

	holder := &recordingHolder{}
	exec := tools.NewSurface("execute", reg.Snapshot(), []string{"close_issue"}).WithHolder(holder)
	res, err = exec.Execute(t.Context(), call)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Failed || !res.Held || res.Output != "held for a person" {
		t.Fatalf("the executor's surface answered %+v, want the holder's answer, held and failed", res)
	}
	if len(holder.held) != 1 || holder.held[0].Args["id"] != "7" || !holder.held[0].Entry.NeedsApproval {
		t.Fatalf("the holder was handed %+v, want the call with its arguments", holder.held)
	}
	if len(closer.seen) != 0 {
		t.Fatalf("the tool ran %d time(s) before anybody approved it", len(closer.seen))
	}
	recorded := exec.Calls()
	if len(recorded) != 1 || recorded[0].Args != nil || !recorded[0].Failed {
		t.Fatalf("the held call is recorded as %+v, want it failed and without its arguments", recorded)
	}

	approved, err := exec.RunApproved(t.Context(), "close_issue", map[string]any{"id": "7"})
	if err != nil {
		t.Fatal(err)
	}
	if approved.Failed || approved.Held || len(closer.seen) != 1 || closer.seen[0]["id"] != "7" {
		t.Fatalf("the approved call answered %+v after %d run(s), want it made once", approved, len(closer.seen))
	}
	if len(holder.held) != 1 {
		t.Fatal("an approved call was held again")
	}
}

// AN APPROVED CALL RUNS ONLY A TOOL THE SURFACE STILL FILES: one blocked or
// removed since it was held is in no universe, and answers not found.
func TestAnApprovedCallRunsOnlyAToolTheSurfaceStillFiles(t *testing.T) {
	t.Parallel()
	exec := tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil)
	res, err := exec.RunApproved(t.Context(), "close_issue", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Failed || res.Refusal() != tools.RefusalNotFound {
		t.Fatalf("an approved call to a tool no longer filed answered %+v, want not found", res)
	}
}

// A BUILTIN WITH FIXED APPROVAL IS HELD: an engine tool filed as asking before
// every call reaches its executor's holder as a tool at Needs approval does,
// marked fixed, never runs until a person approves it, and RequireApproval,
// which only ever widens an MCP tool's wait, leaves it as it is. A builtin
// filed without it runs at once.
//
// Mutation: drop the option's NeedsApproval, and the call runs unasked.
func TestABuiltinWithFixedApprovalIsHeld(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	operate, plain := tool("ssh_operate"), tool("ssh_read")
	if err := reg.RegisterWith(operate, tools.OriginBuiltin, tools.Annotations{}, tools.AsksBeforeEveryCall); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterWith(plain, tools.OriginBuiltin, tools.Annotations{}); err != nil {
		t.Fatal(err)
	}
	if reg.RequireApproval("ssh_operate") {
		t.Error("RequireApproval reported a builtin it does not govern")
	}
	holder := &recordingHolder{}
	exec := tools.NewSurface("execute", reg.Snapshot(), []string{"ssh_operate", "ssh_read"}).WithHolder(holder)
	res, err := exec.Execute(t.Context(), llm.ToolCall{Name: "ssh_operate", Arguments: map[string]any{"host": "api-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Held || len(holder.held) != 1 || !holder.held[0].Entry.ApprovalFixed || len(operate.seen) != 0 {
		t.Fatalf("the operate call answered %+v (held %+v, ran %d), want it held, fixed, not run",
			res, holder.held, len(operate.seen))
	}
	if res, err := exec.Execute(t.Context(), llm.ToolCall{Name: "ssh_read", Arguments: map[string]any{}}); err != nil ||
		res.Held || len(plain.seen) != 1 {
		t.Fatalf("a builtin that does not ask answered %+v, %v", res, err)
	}
}
