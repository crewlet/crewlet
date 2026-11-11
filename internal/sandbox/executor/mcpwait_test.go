package executor_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A BRIDGED ENTRY TELLS EITHER HARNESS HOW LONG TO WAIT FOR A CALL. The bridge
// keeps a running call's answer going, which answers every silence check a
// harness keeps on a call, so the engine hands each server it bridges the
// bridge's own bound for a call (sandbox.MCPServer.CallTimeout), and each
// harness's MCP settings carry it under that harness's own key, in
// milliseconds: Claude Code's per-server `timeout`, OpenCode's
// `timeout.execution`. A server with no wait carries no key and keeps the
// harness's own, and a wait past what a JavaScript timer holds is written as
// the most it holds.
//
// Mutation: skip either harness's key, or write the wait unclamped, and an
// entry here reads wrong.
func TestABridgedEntryTellsEitherHarnessHowLongToWait(t *testing.T) {
	t.Parallel()
	servers := map[string]sandbox.MCPServer{
		"crewlet": {Name: "crewlet", Transport: sandbox.TransportHTTP,
			URL: "https://engine.example.com/mcp/tok", AlwaysLoaded: true, CallTimeout: 1050 * time.Second},
		"github": {Name: "github", Transport: sandbox.TransportHTTP,
			URL: "https://engine.example.com/mcp/tok/server/github", CallTimeout: 30 * 24 * time.Hour},
		"files": {Name: "files", Transport: sandbox.TransportStdio, Command: "mcp-files"},
	}
	// WHAT EACH ENTRY'S WAIT READS, in milliseconds: nil for none at all.
	want := map[string]any{"crewlet": float64(1050000), "github": float64(2147483647), "files": nil}

	b := sandbox.NewFakeSandbox("box-1")
	path, err := executor.ClaudeCode{}.WriteConfig(t.Context(), b,
		sandbox.RunRequest{MCPServers: servers}, executor.PathsFor(b))
	if err != nil || path == "" {
		t.Fatalf("WriteConfig = %q, %v", path, err)
	}
	blob, err := b.ReadFile(t.Context(), path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var claude struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(blob, &claude); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, blob)
	}
	for name, ms := range want {
		got, has := claude.Servers[name]["timeout"]
		if ms == nil {
			if has {
				t.Errorf("Claude Code's %s entry waits %v, want the harness's own: the engine set no wait", name, got)
			}
			continue
		}
		if got != ms {
			t.Errorf("Claude Code's %s entry has timeout %v, want %v", name, got, ms)
		}
	}

	_, body := writeOpenCodeConfig(t, sandbox.RunRequest{MCPServers: servers})
	var opencode struct {
		MCP struct {
			Servers map[string]map[string]any `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(body), &opencode); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	for name, ms := range want {
		timeout, has := opencode.MCP.Servers[name]["timeout"]
		if ms == nil {
			if has {
				t.Errorf("OpenCode's %s entry waits %v, want the harness's own: the engine set no wait", name, timeout)
			}
			continue
		}
		if wait, ok := timeout.(map[string]any); !ok || wait["execution"] != ms {
			t.Errorf("OpenCode's %s entry has timeout %v, want {execution: %v}", name, timeout, ms)
		}
	}
}
