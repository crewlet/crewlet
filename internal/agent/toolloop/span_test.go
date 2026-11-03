package toolloop_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
)

// A RUN'S SPAN CARRIES WHAT THE RUN CAME TO: the model, the whole run's spend,
// its calls and how it ended. It is the one place a trace backend shows a harness
// run's latency beside its cost, and a run drives many requests — a span that
// closed as the harness exited, before the run's spend was summed, carried neither.
//
// Not parallel: the tracer provider is process-wide, and this case installs a
// recorder for its own run while every parallel case is still paused.
func TestARunsSpanCarriesItsModelSpendCallsAndOutcome(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = provider.Shutdown(context.WithoutCancel(t.Context()))
	})

	surf := newSurface("look")
	prov := &llmtest.Script{ModelName: "model-a", Runs: []llmtest.Run{{Responses: []llmtest.Response{
		{Calls: []llmtest.Call{{Name: "look"}, {Name: "look"}}, Usage: llm.Usage{Input: 100, Output: 10}},
		{Text: "done", Usage: llm.Usage{Input: 200, Output: 20, CacheRead: 50, CacheWrite: 5}},
	}}}}
	if _, err := run(t, toolloop.Config{Provider: prov, Surface: surf}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var runs []sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.Name() == "llm.run" {
			runs = append(runs, span)
		}
	}
	if len(runs) != 1 {
		t.Fatalf("recorded %d llm.run spans, want the one run", len(runs))
	}
	got := map[string]any{}
	for _, kv := range runs[0].Attributes() {
		got[string(kv.Key)] = kv.Value.AsInterface()
	}
	want := map[string]any{
		"crewlet.model":              "model-a",
		"crewlet.input_tokens":       int64(300),
		"crewlet.output_tokens":      int64(30),
		"crewlet.cache_read_tokens":  int64(50),
		"crewlet.cache_write_tokens": int64(5),
		"crewlet.tool_calls":         int64(2),
		"crewlet.outcome":            string(toolloop.OutcomeEnd),
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %v, want %v", key, got[key], value)
		}
	}
}
