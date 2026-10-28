package cliagent

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// modeEntry builds an entry in the named mode over the `custom` profile, so
// the probes run without any CLI on the host.
func modeEntry(t *testing.T, agent string, box bool) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	overrides := map[string]any{
		"binary": "crewlet-no-such-cli-binary", "complete_args": []any{"-p"},
		// A text stream has no events to tell apart, so a profile that
		// picks its answer out of one by event type drops the filter too.
		"output": "text", "event_type_path": []any{}, "text_events": []any{},
		"model_args": []any{},
	}
	// And everything else that reads such a stream: its requests, its
	// failure events, its structured limit reports, the cap's signature in
	// it.
	overrides["limit_events"] = nil
	if builtin, _ := cliprofile.Builtin(agent); builtin.MCP != nil {
		overrides["requests"] = nil
		overrides["cap"] = nil
		overrides["mcp"] = map[string]any{"failure_events": []any{}}
	}
	p, err := New(Config{
		Key: "sub", Agent: agent, Box: box, StateDir: dir,
		Timeout: time.Second, MaxConcurrent: 1, Overrides: overrides,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// BOX MODE NEEDS A RUNNER, AND THE FAILURE HAS NO EARLIER SYMPTOM.
//
// An entry naming a CLI this build has no coding-agent runner for validates
// cleanly, appears in the schema, and reports a configured provider — and
// refuses at the moment a seat finally has work to do. `doctor` is what a
// deploy script gates on, so it is the last place to catch it.
func TestDoctorRefusesBoxModeWithNoRunner(t *testing.T) {
	t.Parallel()
	p := modeEntry(t, "codex", true)
	d := p.Diagnose(t.Context(), DiagnoseOptions{
		AgentRunners: []string{"claude-code", "opencode"},
		BridgeURL:    "https://engine.example.com",
	})
	joined := strings.Join(d.Problems, "\n")
	if !strings.Contains(joined, "coding-agent runner") {
		t.Fatalf("the report does not name the missing runner:\n%s", joined)
	}
	// The remedy names BOTH ways out, because either is legitimate: the
	// entry can go back to host mode, or point at a CLI that has one.
	if !strings.Contains(joined, "mode: host") {
		t.Errorf("the problem does not offer host mode:\n%s", joined)
	}
	if !strings.Contains(joined, "claude-code") {
		t.Errorf("the problem does not say which CLIs do have runners:\n%s", joined)
	}
	if !strings.Contains(strings.Join(d.BoxRuntime, "\n"), `none for "codex"`) {
		t.Errorf("the runtime lines do not report the gap: %v", d.BoxRuntime)
	}
}

// BOX MODE NEEDS A BRIDGE THE BOX CAN DIAL. Without one every launch is
// refused, and a coding agent with none of the seat's tools could not answer
// anybody or submit its work even if one started.
func TestDoctorRefusesBoxModeWithNoBridge(t *testing.T) {
	t.Parallel()
	p := modeEntry(t, "claude-code", true)
	d := p.Diagnose(t.Context(), DiagnoseOptions{AgentRunners: []string{"claude-code"}})
	joined := strings.Join(d.Problems, "\n")
	if !strings.Contains(joined, bridgeURLVar) {
		t.Fatalf("the report does not name the variable that fixes it:\n%s", joined)
	}
	if !strings.Contains(strings.Join(d.BoxRuntime, "\n"), "tool bridge: unset") {
		t.Errorf("the runtime lines do not report the gap: %v", d.BoxRuntime)
	}
}

// A COMPLETE BOX-MODE ENTRY REPORTS BOTH HALVES AND NEITHER AS A PROBLEM.
//
// A probe that only ever refuses is one an operator cannot satisfy, and the
// lines are what tells them the mode is actually wired.
func TestDoctorPassesACompleteBoxEntry(t *testing.T) {
	t.Parallel()
	p := modeEntry(t, "claude-code", true)
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
	lines := strings.Join(d.BoxRuntime, "\n")
	if !strings.Contains(lines, "is registered") || !strings.Contains(lines, "https://engine.example.com") {
		t.Errorf("the runtime lines do not report what is wired: %v", d.BoxRuntime)
	}
	if !strings.HasPrefix(d.Mode, "box ") {
		t.Errorf("mode = %q", d.Mode)
	}
}

// A HOST-MODE ENTRY REPORTS NOTHING ABOUT A MODE IT IS NOT IN.
//
// It runs as a subprocess of this engine and needs neither a runner nor a
// bridge; faulting it for their absence would make `doctor` red on every
// deployment that never asked for box mode.
func TestDoctorSaysNothingAboutBoxRuntimeInHostMode(t *testing.T) {
	t.Parallel()
	p := modeEntry(t, "codex", false)
	d := p.Diagnose(t.Context(), DiagnoseOptions{})
	if len(d.BoxRuntime) != 0 {
		t.Errorf("a host-mode entry reported box-runtime lines: %v", d.BoxRuntime)
	}
	joined := strings.Join(d.Problems, "\n")
	for _, unwanted := range []string{"coding-agent runner", bridgeURLVar} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("a host-mode entry was faulted for %q:\n%s", unwanted, joined)
		}
	}
	if !strings.HasPrefix(d.Mode, "host ") {
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
	modeEntry(t, "claude-code", true).
		Diagnose(t.Context(), DiagnoseOptions{
			AgentRunners: []string{"claude-code"}, BridgeURL: "https://x.example",
		}).Render(&out)
	rendered := out.String()
	if !strings.Contains(rendered, "mode ") {
		t.Errorf("the rendered report has no mode line:\n%s", rendered)
	}
	if !strings.Contains(rendered, "box runtime") {
		t.Errorf("the rendered report has no box-runtime section:\n%s", rendered)
	}
}
