package cliagent

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
	"github.com/crewlet/crewlet/internal/tracing"
)

// round opens the span a call runs beneath — the tool loop's `llm.round` —
// under a remote parent carrying the given decision and vendor state, so what
// the span decides is the parent's rather than whichever root rule the
// process holds.
func round(t *testing.T, unsampled bool, state string) (context.Context, trace.SpanContext) {
	t.Helper()
	parent := tracing.WithRemote(t.Context(), events.TraceContext{
		TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", Unsampled: unsampled,
	})
	if state != "" {
		ts, err := trace.ParseTraceState(state)
		if err != nil {
			t.Fatalf("ParseTraceState: %v", err)
		}
		parent = trace.ContextWithRemoteSpanContext(parent,
			trace.SpanContextFromContext(parent).WithTraceState(ts))
	}
	ctx, span := tracing.Start(parent, "test", "llm.round")
	t.Cleanup(func() { span.End() })
	return ctx, span.SpanContext()
}

// traceparentOf is the W3C value a call beneath sc must be handed.
func traceparentOf(sc trace.SpanContext) string {
	flags := "-00"
	if sc.IsSampled() {
		flags = "-01"
	}
	return "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + flags
}

// tracedProvider is [fakeProvider] for a CLI that declares it reads trace
// context from its environment.
func tracedProvider(t *testing.T, env map[string]string) *Provider {
	t.Helper()
	return fakeProvider(t, env, map[string]any{"env_trace_context": true})
}

// THE ROUND'S TRACE CONTEXT REACHES A CLI THAT READS IT — its own span, its
// own decision, its vendor state — and a value cli.env wrote under either name
// does not: a fixed TRACEPARENT there would name one parent for every call the
// entry ever makes.
func TestTheRoundsTraceContextReachesTheCLI(t *testing.T) {
	t.Parallel()
	for _, unsampled := range []bool{false, true} {
		p := tracedProvider(t, map[string]string{
			"FAKE_DUMP_ENV": "1",
			"TRACEPARENT":   "00-11111111111111111111111111111111-2222222222222222-01",
			"TRACESTATE":    "operator=wrote-this",
		})
		ctx, sc := round(t, unsampled, "vendor=opaque")
		comp, err := p.Complete(ctx, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if got, want := envValue(comp.Content, "TRACEPARENT"), traceparentOf(sc); got != want {
			t.Errorf("unsampled=%v: TRACEPARENT = %q, want the round's %q", unsampled, got, want)
		}
		if got := envValue(comp.Content, "TRACESTATE"); got != "vendor=opaque" {
			t.Errorf("unsampled=%v: TRACESTATE = %q, want the trace's own vendor state", unsampled, got)
		}
	}
}

// No span, no parent — and not the one cli.env wrote either. A CLI handed none
// roots a trace of its own, which is honest; a parent invented for it is not.
func TestACallWithNoSpanIsHandedNoTraceContext(t *testing.T) {
	t.Parallel()
	p := tracedProvider(t, map[string]string{
		"FAKE_DUMP_ENV": "1",
		"TRACEPARENT":   "00-11111111111111111111111111111111-2222222222222222-01",
		"TRACESTATE":    "operator=wrote-this",
	})
	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	for _, name := range cliprofile.TraceContextEnv {
		if strings.Contains(comp.Content, name+"=") {
			t.Errorf("%s reached a call made beneath no span:\n%s", name, comp.Content)
		}
	}
}

// A CLI nobody has measured reading trace context is handed none: the
// declaration is the profile's, not a default.
func TestACLIThatReadsNoTraceContextIsHandedNone(t *testing.T) {
	t.Parallel()
	p := fakeProvider(t, map[string]string{"FAKE_DUMP_ENV": "1"}, nil)
	ctx, _ := round(t, false, "")
	comp, err := p.Complete(ctx, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if strings.Contains(comp.Content, "TRACEPARENT=") {
		t.Errorf("a CLI whose profile does not read trace context was handed it:\n%s", comp.Content)
	}
}

// A CLAUDE CODE CALL IS HANDED TRACE CONTEXT AND NO TELEMETRY DESTINATION.
//
// The built-in profile switches the CLI's propagation on — the beta and the
// gateway switch — and leaves its telemetry off, so it forwards the round's
// traceparent to the API and exports nothing from the engine host. What the
// engine's own exporter is configured with — its collector's endpoint and the
// ingest key in its headers — and a telemetry switch exported on the host
// never reach it: a vendor's CLI holding the company's collector credential
// is the leak the allowlist exists to prevent.
func TestAClaudeCodeCallIsHandedTraceContextAndNoTelemetryDestination(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example.com:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer%20collector-ingest-key")
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("CLAUDE_CODE_ENABLE_TELEMETRY", "1")

	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "claude", Agent: "claude-code", StateDir: dir,
		Overrides: map[string]any{
			"binary":             os.Args[0],
			"complete_args":      []any{"-test.run=TestCLIAgentFakeCLI"},
			"model_args":         []any{},
			"system_prompt_args": []any{},
			"output":             "text",
		},
		Timeout: 20 * time.Second, MaxConcurrent: 1,
		Env:  fakeChildEnv(map[string]string{"FAKE_DUMP_ENV": "1"}),
		Auth: Auth{Mode: AuthSubscription},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, sc := round(t, false, "")
	comp, err := p.Complete(ctx, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got, want := envValue(comp.Content, "TRACEPARENT"), traceparentOf(sc); got != want {
		t.Errorf("TRACEPARENT = %q, want the round's %q", got, want)
	}
	for _, name := range []string{"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA", "CLAUDE_CODE_PROPAGATE_TRACEPARENT"} {
		if envValue(comp.Content, name) != "1" {
			t.Errorf("%s is not on, so the CLI forwards no trace context:\n%s", name, comp.Content)
		}
	}
	for line := range strings.SplitSeq(comp.Content, "\n") {
		name, _, _ := strings.Cut(line, "=")
		if strings.HasPrefix(name, "OTEL_") || name == "CLAUDE_CODE_ENABLE_TELEMETRY" {
			t.Errorf("%s reached the CLI: it would export spans of its own from the engine host", line)
		}
	}
}

// The profile names the variables the engine owns once a CLI declares trace
// context, and the tracing package renders them: one list, spelled twice
// because the profile table is a leaf config imports, held here to one.
func TestTheProfileOwnsExactlyTheVariablesTracingSets(t *testing.T) {
	t.Parallel()
	want := tracing.ProcessEnvNames()
	got := slices.Sorted(slices.Values(cliprofile.TraceContextEnv))
	if !slices.Equal(got, want) {
		t.Errorf("cliprofile.TraceContextEnv = %v, tracing sets %v", got, want)
	}
}
