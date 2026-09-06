package cliagent

import (
	"strings"
	"testing"
	"time"
)

// agentEntry builds an entry in the named mode over the `custom` profile, so
// the probes run without any CLI on the host.
func agentEntry(t *testing.T, agent string, mode bool) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "sub", Agent: agent, AgentMode: mode, StateDir: dir,
		Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{
			"binary": "crewlet-no-such-cli-binary", "complete_args": []any{"-p"},
			"output": "text", "model_args": []any{},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// AGENT MODE NEEDS A RUNNER, AND THE FAILURE HAS NO EARLIER SYMPTOM.
//
// An entry naming a CLI this build has no coding-agent runner for validates
// cleanly, appears in the schema, and reports a configured provider — and
// refuses at the moment a seat finally has work to do. `doctor` is what a
// deploy script gates on, so it is the last place to catch it.
func TestDoctorRefusesAgentModeWithNoRunner(t *testing.T) {
	t.Parallel()
	p := agentEntry(t, "codex", true)
	d := p.Diagnose(t.Context(), DiagnoseOptions{
		AgentRunners: []string{"claude-code", "opencode"},
		BridgeURL:    "https://engine.example.com",
	})
	joined := strings.Join(d.Problems, "\n")
	if !strings.Contains(joined, "coding-agent runner") {
		t.Fatalf("the report does not name the missing runner:\n%s", joined)
	}
	// The remedy names BOTH ways out, because either is legitimate: the
	// entry can go back to text mode, or point at a CLI that has one.
	if !strings.Contains(joined, "mode: text") {
		t.Errorf("the problem does not offer text mode:\n%s", joined)
	}
	if !strings.Contains(joined, "claude-code") {
		t.Errorf("the problem does not say which CLIs do have runners:\n%s", joined)
	}
	if !strings.Contains(strings.Join(d.AgentRuntime, "\n"), `none for "codex"`) {
		t.Errorf("the runtime lines do not report the gap: %v", d.AgentRuntime)
	}
}

// AGENT MODE NEEDS A BRIDGE THE BOX CAN DIAL. Without one every launch is
// refused, and a coding agent with none of the seat's tools could not answer
// anybody or submit its work even if one started.
func TestDoctorRefusesAgentModeWithNoBridge(t *testing.T) {
	t.Parallel()
	p := agentEntry(t, "claude-code", true)
	d := p.Diagnose(t.Context(), DiagnoseOptions{AgentRunners: []string{"claude-code"}})
	joined := strings.Join(d.Problems, "\n")
	if !strings.Contains(joined, bridgeURLVar) {
		t.Fatalf("the report does not name the variable that fixes it:\n%s", joined)
	}
	if !strings.Contains(strings.Join(d.AgentRuntime, "\n"), "tool bridge: unset") {
		t.Errorf("the runtime lines do not report the gap: %v", d.AgentRuntime)
	}
}

// A COMPLETE AGENT-MODE ENTRY REPORTS BOTH HALVES AND NEITHER AS A PROBLEM.
//
// A probe that only ever refuses is one an operator cannot satisfy, and the
// lines are what tells them the mode is actually wired.
func TestDoctorPassesACompleteAgentEntry(t *testing.T) {
	t.Parallel()
	p := agentEntry(t, "claude-code", true)
	d := p.Diagnose(t.Context(), DiagnoseOptions{
		AgentRunners: []string{"claude-code", "opencode"},
		BridgeURL:    "https://engine.example.com",
	})
	joined := strings.Join(d.Problems, "\n")
	for _, unwanted := range []string{"coding-agent runner", bridgeURLVar} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("a complete entry was faulted for %q:\n%s", unwanted, joined)
		}
	}
	lines := strings.Join(d.AgentRuntime, "\n")
	if !strings.Contains(lines, "is registered") || !strings.Contains(lines, "https://engine.example.com") {
		t.Errorf("the runtime lines do not report what is wired: %v", d.AgentRuntime)
	}
	if !strings.Contains(d.Mode, "agent") {
		t.Errorf("mode = %q", d.Mode)
	}
}

// A TEXT-MODE ENTRY REPORTS NOTHING ABOUT A MODE IT IS NOT IN.
//
// It runs as a subprocess of this engine and needs neither a runner nor a
// bridge; faulting it for their absence would make `doctor` red on every
// deployment that never asked for agent mode.
func TestDoctorSaysNothingAboutAgentRuntimeInTextMode(t *testing.T) {
	t.Parallel()
	p := agentEntry(t, "codex", false)
	d := p.Diagnose(t.Context(), DiagnoseOptions{})
	if len(d.AgentRuntime) != 0 {
		t.Errorf("a text-mode entry reported agent-runtime lines: %v", d.AgentRuntime)
	}
	joined := strings.Join(d.Problems, "\n")
	for _, unwanted := range []string{"coding-agent runner", bridgeURLVar} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("a text-mode entry was faulted for %q:\n%s", unwanted, joined)
		}
	}
	if !strings.Contains(d.Mode, "text") {
		t.Errorf("mode = %q", d.Mode)
	}
}

// THE REPORT SHOWS THE MODE, on every entry.
//
// Which mode an entry is in changes where its executor runs, what it can
// reach and what it costs — and it is one line in a config an operator may
// not have written. A doctor that reported everything except that would make
// the difference between a working seat and a refused one invisible.
func TestTheReportShowsTheMode(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	agentEntry(t, "claude-code", true).
		Diagnose(t.Context(), DiagnoseOptions{
			AgentRunners: []string{"claude-code"}, BridgeURL: "https://x.example",
		}).Render(&out)
	rendered := out.String()
	if !strings.Contains(rendered, "mode ") {
		t.Errorf("the rendered report has no mode line:\n%s", rendered)
	}
	if !strings.Contains(rendered, "agent runtime") {
		t.Errorf("the rendered report has no agent-runtime section:\n%s", rendered)
	}
}
