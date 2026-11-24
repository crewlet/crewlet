package executor

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// AN AGENT THAT CONNECTS ITS OWN SERVER UNDER THE BRIDGE'S NAME STOPS THE RUN.
// Copilot CLI 1.0.114 connects a repository custom agent's own `mcp-servers`
// entry `crewlet` for that agent, which then serves `crewlet-submit_work` in
// its calls (measured); it announces the connection under the agent's id
// before the agent's first call. The extension logs the error that fails the
// run, naming the agent, then aborts, and refuses the agent's calls under the
// bridge's names meanwhile. The bridge's own connection (no agent id), an
// agent's server of another name, and another agent's calls pass.
//
// Mutations, each red: stop on the bridge's own status; stop on any server an
// agent connects; leave the taken agent's calls through.
func TestAnAgentThatConnectsItsOwnServerUnderTheBridgesNameStopsTheRun(t *testing.T) {
	p := startCopilotProbe(t, copilotExtensionSettings{Bridges: []string{"crewlet"}})
	p.expect("joined")
	p.expectArmed()
	settled := func(why string) {
		t.Helper()
		p.hook("onPreToolUse", map[string]any{"agentId": "sub-2", "toolName": "crewlet-submit_work"})
		if r := p.expect("hooked"); r.Output != nil {
			t.Fatalf("%s: another agent's bridge call was answered %v", why, r.Output)
		}
	}
	p.event("session.mcp_server_status_changed", "", map[string]any{"serverName": "crewlet", "status": "connected"})
	settled("the bridge's own connection")
	p.event("subagent.started", "sub-1", map[string]any{"agentName": "sha\"dow <x>"})
	p.event("session.mcp_server_status_changed", "sub-1", map[string]any{"serverName": "github", "status": "pending"})
	settled("an agent's server of another name")
	p.event("session.mcp_server_status_changed", "sub-1", map[string]any{"serverName": "crewlet", "status": "pending"})
	logged := p.expect("log")
	if !strings.HasPrefix(logged.Message, copilotUnbridgedMessage) || logged.Level != "error" ||
		!strings.Contains(logged.Message, "the custom agent shadowx ") {
		t.Fatalf("logged %+v, want the error naming the agent", logged)
	}
	p.expect("abort")
	p.hook("onPreToolUse", map[string]any{"agentId": "sub-1", "toolName": "crewlet-submit_work"})
	if r := p.expect("hooked"); r.Output["permissionDecision"] != "deny" {
		t.Fatalf("the agent's bridge call was answered %v, want it refused", r.Output)
	}
	p.hook("onPreToolUse", map[string]any{"agentId": "sub-1", "toolName": "view"})
	if r := p.expect("hooked"); r.Output != nil {
		t.Fatalf("the agent's built-in call was answered %v", r.Output)
	}
}

// A RUN WITH A BRIDGE IS GUARDED: its script names the guard in the stream, its
// extension is written with the bridge's name though it reads no notes and has
// no cap, and a run with no bridge has neither.
//
// Mutation: write the extension for notes and caps alone, or drop the mark.
func TestACopilotRunWithABridgeIsGuarded(t *testing.T) {
	paths := Paths{Home: "/box"}
	bridged := sandbox.RunRequest{Brief: "x", MCPServers: map[string]sandbox.MCPServer{
		"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:7/mcp/tok", AlwaysLoaded: true}}}
	mark := `{"type":"` + copilotBridgeGuardMark + `"}`
	if script := copilotScript(bridged, Session{ID: "s"}, paths, paths.MCPConfig(), time.Second); !strings.Contains(script, mark) {
		t.Errorf("a run with a bridge names no guard:\n%s", script)
	}
	box := sandbox.NewFakeSandbox("box-1")
	p := PathsFor(box)
	if _, err := (Copilot{}).WriteConfig(t.Context(), box, bridged, Session{}, p); err != nil {
		t.Fatal(err)
	}
	ext, err := box.ReadFile(t.Context(), p.CopilotExtension()+"/extension.mjs")
	if err != nil || !bytes.Contains(ext, []byte(`"bridges":["crewlet"]`)) {
		t.Errorf("the extension (%v) is not written to guard the bridge", err)
	}
	plain := sandbox.RunRequest{Brief: "x", MCPServers: map[string]sandbox.MCPServer{
		"tools": {Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:7/x"}}}
	if script := copilotScript(plain, Session{ID: "s"}, paths, paths.MCPConfig(), time.Second); strings.Contains(script, mark) {
		t.Errorf("a run with no bridge names the guard")
	}
	box2 := sandbox.NewFakeSandbox("box-2")
	q := PathsFor(box2)
	if _, err := (Copilot{}).WriteConfig(t.Context(), box2, plain, Session{}, q); err != nil {
		t.Fatal(err)
	}
	if got, _ := box2.ReadFile(t.Context(), q.CopilotExtension()+"/extension.mjs"); got != nil {
		t.Errorf("a run with no bridge, notes or cap has an extension")
	}
}

// A GUARDED RUN FAILS IN THE GUARD'S WORDS, OR CLOSED. The extension's error
// fails the run whatever it answered; a run whose script named the guard and
// whose extension never ran fails, nothing having held the bridge's names; the
// same run with the extension's line is its answer.
//
// Mutations, each red: drop either check; read the error as any session error
// after another.
func TestAGuardedCopilotRunFailsInTheGuardsWordsOrClosed(t *testing.T) {
	mark := `{"type":"` + copilotBridgeGuardMark + `"}` + "\n"
	stream := copilotFixture(t, "tool.stream.jsonl")
	read := func(head string, tail ...string) sandbox.Result {
		dec := &copilotEvents{}
		all := append([]byte(head), stream...)
		for _, l := range tail {
			all = append(all, l...)
		}
		_ = eachLine(bytes.NewReader(all), dec, maxLineBytes)
		return dec.Result()
	}
	if res := read(mark); res.Success || !strings.Contains(res.Error, "never ran") {
		t.Errorf("a guard that never ran: %+v, want the failure", res)
	}
	if res := read(mark + copilotArmedLine(t)); !res.Success {
		t.Errorf("a guarded run with its extension: %+v, want the answer", res)
	}
	stopped := `{"type":"session.error","data":{"errorType":"notification","message":"` + copilotUnbridgedMessage +
		`: the custom agent shadow connected its own MCP server crewlet"}}` + "\n" +
		`{"type":"session.error","data":{"errorType":"query","message":"later"}}` + "\n"
	if res := read(mark+copilotArmedLine(t), stopped); res.Success || !strings.HasPrefix(res.Error, copilotShadowed) {
		t.Errorf("a stopped run: %+v, want the guard's words", res)
	}
}
