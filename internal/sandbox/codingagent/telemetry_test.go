package codingagent_test

import (
	"maps"
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// What these tests protect.
//
// A coding agent in a box exports to the engine's receiver only if its runner
// switches its export on, and every switch Claude Code needs fails the same
// silent way when it is missing: the run works, the receiver is configured,
// and no span ever arrives. The values were measured against Claude Code
// 2.1.300 run headless against a stand-in API and collector — with the three
// gates and nothing else it exported the run's spans, nested under the
// TRACEPARENT it was handed; without any one of them it exported none.

// CLAUDE CODE EXPORTS ITS TRACES, AND ONLY ITS TRACES.
//
// Mutation: drop any of the three gates and its case goes red; declare a
// metrics or logs exporter other than `none` and the double-count case does.
func TestClaudeCodeSwitchesItsTracesOnAndNothingElse(t *testing.T) {
	t.Parallel()
	env := codingagent.ClaudeCode{}.Telemetry()
	for name, want := range map[string]string{
		// The gate every Claude Code export sits behind.
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
		// The beta its spans sit behind: telemetry without it exports
		// logs and no span.
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA": "1",
		// An exporter list has no default; empty exports nothing.
		"OTEL_TRACES_EXPORTER": "otlp",
		// A gateway or a cloud platform hears the round's traceparent
		// too, as the engine's own SDK sends it whatever its base URL.
		"CLAUDE_CODE_PROPAGATE_TRACEPARENT": "1",
		// Its metrics count what the engine already records for the
		// run, and its log events are its content channel.
		"OTEL_METRICS_EXPORTER": "none",
		"OTEL_LOGS_EXPORTER":    "none",
		// The launching turn decided the trace; an image-baked
		// always_on would re-decide it.
		"OTEL_TRACES_SAMPLER": "parentbased_always_on",
	} {
		if got := env[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// NO CONTENT LEAVES THE BOX ON A SPAN: every one of Claude Code's content
// switches is declared off, because an image can declare it on and the
// content of a run belongs in the event store.
//
// Mutation: drop one name from the declaration and its case goes red.
func TestClaudeCodeIsToldToExportNoContent(t *testing.T) {
	t.Parallel()
	env := codingagent.ClaudeCode{}.Telemetry()
	for _, name := range []string{
		"OTEL_LOG_USER_PROMPTS", "OTEL_LOG_TOOL_DETAILS", "OTEL_LOG_TOOL_CONTENT",
		"OTEL_LOG_ASSISTANT_RESPONSES", "OTEL_LOG_RAW_API_BODIES",
	} {
		if got := env[name]; got != "false" {
			t.Errorf("%s = %q, want \"false\"", name, got)
		}
	}
}

// CLAUDE CODE'S EXIT WAITS AS LONG AS THE RECEIVER MAY TAKE TO ANSWER its last
// export. The CLI gives up after two seconds by default while the receiver
// forwards before it answers, for up to its forward timeout — so every run
// against a backend slower than two seconds lost the spans of its own ending.
//
// Mutation: declare the CLI's two-second default and this goes red.
func TestClaudeCodeWaitsForItsLastExportToBeAnswered(t *testing.T) {
	t.Parallel()
	raw := codingagent.ClaudeCode{}.Telemetry()["CLAUDE_CODE_OTEL_SHUTDOWN_TIMEOUT_MS"]
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("CLAUDE_CODE_OTEL_SHUTDOWN_TIMEOUT_MS = %q, not milliseconds", raw)
	}
	if got := time.Duration(ms) * time.Millisecond; got <= sandbox.OtelForwardTimeout {
		t.Errorf("Claude Code stops flushing after %v, which the receiver's own "+
			"forward (%v) can outlast", got, sandbox.OtelForwardTimeout)
	}
}

// OPENCODE DECLARES NOTHING, so a run of it is minted no endpoint: it exports
// no OTLP, and a token in a box whose agent never uses it is a credential
// nothing needs.
func TestOpenCodeDeclaresNoTelemetry(t *testing.T) {
	t.Parallel()
	if env := (codingagent.OpenCode{}).Telemetry(); env != nil {
		t.Errorf("OpenCode declares %v", env)
	}
}

// THE RUNNER SURFACES ITS CLI'S DECLARATION, fresh each time, so the launch
// adding the receiver's variables to it cannot change what the next run is
// declared.
func TestTheRunnerHandsOutItsCLIsDeclarationFresh(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewClaudeCode()
	first := runner.Telemetry()
	if !maps.Equal(first, codingagent.ClaudeCode{}.Telemetry()) {
		t.Fatalf("the runner declares %v, its CLI %v", first, codingagent.ClaudeCode{}.Telemetry())
	}
	first["CLAUDE_CODE_ENABLE_TELEMETRY"] = "0"
	if runner.Telemetry()["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" {
		t.Error("a caller's write reached the next run's declaration")
	}
	if codingagent.NewOpenCode().Telemetry() != nil {
		t.Error("the OpenCode runner declares telemetry its CLI does not")
	}
}
