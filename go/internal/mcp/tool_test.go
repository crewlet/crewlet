package mcp

import (
	"context"
	"strings"
	"testing"
)

func TestToolIdentity(t *testing.T) {
	t.Parallel()
	b := newTestBridge(t)
	spec := helperSpec(t, InstanceName("github", "Senior Dev"), "serve", map[string]string{
		helperToolsEnv: toolsJSON([3]string{"create_pr", "Open a PR", ""}),
	})
	spec.ToolPrefix = "gh_"
	change, err := b.Add(t.Context(), spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	tool := change.Added[0]

	if tool.Name() != "gh_create_pr" {
		t.Errorf("Name = %q: the catalogue name carries the prefix", tool.Name())
	}
	if tool.RawName() != "create_pr" {
		t.Errorf("RawName = %q: the wire name must not carry the engine's prefix", tool.RawName())
	}
	// Compared against the grammar rather than a literal: the literal belongs
	// in instance_test.go, which is where it is the assertion rather than a
	// second definition. (The guard test caught exactly this.)
	if want := InstanceName("github", "Senior Dev"); tool.Instance() != want {
		t.Errorf("Instance = %q, want %q: the instance names the PROCESS", tool.Instance(), want)
	}
	if tool.Server() != "github" {
		t.Errorf("Server = %q: the model only ever sees the bare name", tool.Server())
	}
	if tool.Origin() != "mcp:github" {
		t.Errorf("Origin = %q", tool.Origin())
	}
	if tool.Description() != "Open a PR" {
		t.Errorf("Description = %q", tool.Description())
	}
	if tool.Parameters()["type"] != "object" {
		t.Errorf("Parameters are not a JSON Schema object: %v", tool.Parameters())
	}

	// And the prefix must not reach the wire. The server has never heard of
	// it, so a call under the catalogue name is a tools/call for a tool that
	// does not exist — which the model reads as "this tool is broken" and
	// retries. The helper echoes the name it was asked for.
	res, err := tool.Call(t.Context(), nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(res.Output, "Result of create_pr") {
		t.Fatalf("the server was called as %q, not by its own name: %q",
			tool.Name(), res.Output)
	}
}

// TestToolCallReportsFailureToTheModelNotToTheCaller pins the split that makes
// a tool usable at all.
//
// A failing tool is ORDINARY: the server's own words go back as a tool message
// and the model is expected to react to them. Returning a Go error instead
// would unwind the phase over a permission denied.
func TestToolCallReportsFailureToTheModelNotToTheCaller(t *testing.T) {
	t.Parallel()
	c := mustConnect(t, helperSpec(t, "denier", "serve", map[string]string{
		helperCallEnv: "error",
	}))
	defs, err := c.listTools(t.Context())
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	tool := newTool(c, defs[0], Spec{Name: "denier"})

	res, err := tool.Call(t.Context(), map[string]any{"q": "x"})
	if err != nil {
		t.Fatalf("a refused tool must not be a Go error: %v", err)
	}
	if !res.Failed {
		t.Fatal("a refused tool was reported as a success")
	}
	// The server's reason has to survive: the model reads this output, and a
	// generic "tool execution failed" teaches it nothing it can act on.
	if !strings.Contains(res.Output, "permission denied") {
		t.Fatalf("output %q lost the server's reason", res.Output)
	}
	// And it must name the server and the tool, because an agent with a dozen
	// MCP servers cannot otherwise tell which one refused.
	if !strings.Contains(res.Output, "denier/search") {
		t.Fatalf("output %q does not name the server and tool", res.Output)
	}
}

// TestToolCallReturnsAnErrorWhenTheCallerIsGone is the other half of that
// split.
//
// A cancelled turn has nobody left to show a tool message to, and reporting a
// cancellation as a tool failure would teach the model the tool is broken.
func TestToolCallReturnsAnErrorWhenTheCallerIsGone(t *testing.T) {
	t.Parallel()
	c := mustConnect(t, helperSpec(t, "slow-tool", "serve", map[string]string{
		helperCallEnv: "hang",
	}))
	defs, err := c.listTools(t.Context())
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	tool := newTool(c, defs[0], Spec{Name: "slow-tool"})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res, err := tool.Call(ctx, nil)
	if err == nil {
		t.Fatalf("a cancelled turn must surface as an error, got %+v", res)
	}
	if res.Failed || res.Output != "" {
		t.Fatalf("a cancelled call also produced a model-visible result: %+v", res)
	}
}

func TestToolCallOnAStoppedServerIsAnOrdinaryFailure(t *testing.T) {
	t.Parallel()
	// The lifecycle point a live config edit creates: the surface still holds
	// the tool, the server behind it is gone. The model gets a failure it can
	// route around, not an unwound phase.
	c := mustConnect(t, helperSpec(t, "gone", "serve", nil))
	defs, err := c.listTools(t.Context())
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	tool := newTool(c, defs[0], Spec{Name: "gone"})
	if err := c.stop(t.Context()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	res, err := tool.Call(t.Context(), nil)
	if err != nil {
		t.Fatalf("calling a stopped server must not be a Go error: %v", err)
	}
	if !res.Failed || !strings.Contains(res.Output, "not running") {
		t.Fatalf("res = %+v", res)
	}
}
