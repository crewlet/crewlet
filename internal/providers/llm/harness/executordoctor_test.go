package harness

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// doctorEntry builds an entry driving the named CLI over the `custom`
// profile's probes, so the diagnosis runs without any CLI on the host.
func doctorEntry(t *testing.T, agent string) *Provider {
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
	// failure events, its structured limit and sign-in reports, the cap's
	// signature in it.
	overrides["limit_events"] = nil
	overrides["limit_reset_events"] = nil
	overrides["auth_events"] = nil
	if builtin, _ := harnessprofile.Builtin(agent); builtin.MCP != nil {
		overrides["requests"] = nil
		overrides["cap"] = nil
		overrides["mcp"] = map[string]any{"failure_events": []any{}}
	}
	p, err := New(Config{
		Key: "sub", Harness: agent, StateDir: dir,
		Timeout: time.Second, MaxConcurrent: 1, Overrides: overrides,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// AN EXECUTOR NEEDS A BRIDGE THE BOX CAN DIAL, AND THE FAILURE HAS NO EARLIER
// SYMPTOM. A box that is not on the node's host dials the variable alone, so
// without it every launch there is refused, and a coding agent with none of
// the seat's tools could not answer anybody or submit its work even if one
// started. `doctor` is what a deploy script gates on, so it is the last place
// to catch it before an agent is waiting.
//
// Mutation: drop the bridge check, and the problem disappears.
func TestDoctorFaultsAnExecutorEntryWithNoBridge(t *testing.T) {
	t.Parallel()
	p := doctorEntry(t, "claude-code")
	d := p.Diagnose(t.Context(), DiagnoseOptions{
		ExecutorSeats: []string{"swe"}, ExecutorHarnesses: []string{"claude-code"},
		Cell: "e2b",
	})
	joined := strings.Join(d.Problems, "\n")
	if !strings.Contains(joined, bridgeURLVar) || !strings.Contains(joined, "e2b") {
		t.Fatalf("the report does not name the variable and the cell:\n%s", joined)
	}
	if !strings.Contains(joined, "providers.llm.sub") {
		t.Errorf("the problem does not name the entry it refuses launches on:\n%s", joined)
	}
	if !strings.Contains(strings.Join(d.Executor, "\n"), "tool bridge: unset") {
		t.Errorf("the executor lines do not report the gap: %v", d.Executor)
	}
}

// A DIRECT BOX NEEDS NO BRIDGE VARIABLE. It is a process tree on the node's own
// host and dials the node's own listener on loopback, which the node derives
// from the listener it binds — the quickstart's whole company. A doctor that
// faulted the unset variable here would send an operator to set something the
// launch never reads for this cell, and would be red on the shape the
// quickstart runs.
//
// Mutation: fault every unset variable whatever the cell, and this goes red.
func TestDoctorPassesADirectEntryWithNoBridgeVariable(t *testing.T) {
	t.Parallel()
	p := doctorEntry(t, "claude-code")
	d := p.Diagnose(t.Context(), DiagnoseOptions{
		ExecutorSeats: []string{"swe"}, ExecutorHarnesses: []string{"claude-code"},
		Cell: "direct",
	})
	for _, problem := range d.Problems {
		if strings.Contains(problem, bridgeURLVar) {
			t.Errorf("a direct entry was faulted for the variable: %s", problem)
		}
	}
	if !strings.Contains(strings.Join(d.Executor, "\n"), "own listener, on loopback") {
		t.Errorf("the executor lines do not say where a direct box dials: %v", d.Executor)
	}
}

// A CLI WITH NO IN-BOX RUNNER IN AN EXECUTOR CHAIN IS REPORTED, NOT FAULTED.
//
// The company refuses an agent seat whose chain holds no runner at all, so a
// CLI without one sits beside one that has it: every launch skips it, and it
// still serves the seat's phases on this host. A doctor that went red on it would be
// red on a configuration the engine runs. The line says which CLIs do have
// runners, so the operator knows where the executor actually lands.
//
// Mutation: report the missing runner as a problem, and this goes red.
func TestDoctorReportsANonRunnerExecutorMemberAsSkipped(t *testing.T) {
	t.Parallel()
	p := doctorEntry(t, "codex")
	d := p.Diagnose(t.Context(), DiagnoseOptions{
		ExecutorSeats:     []string{"swe"},
		ExecutorHarnesses: []string{"claude-code", "opencode"},
	})
	lines := strings.Join(d.Executor, "\n")
	if !strings.Contains(lines, `none for "codex"`) || !strings.Contains(lines, "skipped at launch") {
		t.Errorf("the executor lines do not report the skipped member: %v", d.Executor)
	}
	if !strings.Contains(lines, "claude-code") {
		t.Errorf("the line does not say which CLIs do have runners: %v", d.Executor)
	}
	// A bridge the run would never dial is not this entry's concern.
	for _, problem := range d.Problems {
		if strings.Contains(problem, "runner") || strings.Contains(problem, bridgeURLVar) {
			t.Errorf("a non-runner member was faulted: %s", problem)
		}
	}
}

// A COMPLETE EXECUTOR ENTRY REPORTS EVERY HALF AND NONE AS A PROBLEM.
//
// A probe that only ever refuses is one an operator cannot satisfy, and the
// lines are what tells them a seat's executor is actually wired — and which
// seats launch on this entry.
func TestDoctorPassesACompleteExecutorEntry(t *testing.T) {
	t.Parallel()
	p := doctorEntry(t, "claude-code")
	d := p.Diagnose(t.Context(), DiagnoseOptions{
		ExecutorSeats:     []string{"swe", "qa"},
		ExecutorHarnesses: []string{"claude-code", "opencode"},
		BridgeURL:         "https://engine.example.com",
	})
	for _, problem := range d.Problems {
		if strings.Contains(problem, "runner") || strings.Contains(problem, bridgeURLVar) {
			t.Errorf("a complete entry was faulted: %s", problem)
		}
	}
	lines := strings.Join(d.Executor, "\n")
	for _, want := range []string{"swe, qa", "is registered", "https://engine.example.com"} {
		if !strings.Contains(lines, want) {
			t.Errorf("the executor lines do not report %q: %v", want, d.Executor)
		}
	}
}

// AN ENTRY NO EXECUTOR CHAIN HOLDS REPORTS NOTHING ABOUT AN EXECUTOR.
//
// It serves the phases on this host, as a subprocess of this engine, and needs neither a
// runner nor a bridge; faulting it for their absence would make `doctor` red
// on an entry kept for a seat's reviewer or side passes.
func TestDoctorSaysNothingAboutAnExecutorOnAnEntryNoSeatLaunchesOn(t *testing.T) {
	t.Parallel()
	p := doctorEntry(t, "codex")
	d := p.Diagnose(t.Context(), DiagnoseOptions{})
	if len(d.Executor) != 0 {
		t.Errorf("an entry no executor chain holds reported executor lines: %v", d.Executor)
	}
	for _, problem := range d.Problems {
		if strings.Contains(problem, "runner") || strings.Contains(problem, bridgeURLVar) {
			t.Errorf("an entry no executor chain holds was faulted: %s", problem)
		}
	}
}

// THE RENDERED REPORT CARRIES THE EXECUTOR SECTION under its own label, so the
// operator reading `doctor` sees where a seat's run lands without reading the
// company document.
func TestTheReportShowsTheExecutorSection(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	doctorEntry(t, "claude-code").
		Diagnose(t.Context(), DiagnoseOptions{
			ExecutorSeats:     []string{"swe"},
			ExecutorHarnesses: []string{"claude-code"}, BridgeURL: "https://x.example",
		}).Render(&out)
	rendered := out.String()
	if !strings.Contains(rendered, "executor      : seats: swe") {
		t.Errorf("the rendered report has no executor section:\n%s", rendered)
	}
}
