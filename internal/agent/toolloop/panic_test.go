package toolloop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// A PANIC LEAVES THE ACCOUNT AN ERROR DOES.
//
// The caller's failure view is the loop's Progress, and the loop brings it up
// to date only when it publishes. A panic between two publishes — in a
// provider mid-answer, in a tool partway through a round, in the budget meter
// once the answer is billed — would take everything since with it. Each case
// below leaves what the matching error path leaves, and the panic goes on with
// its own value, which is what the turn's guard names in its breach.

// panicValue is a panic value a case can recognise by identity, so the one
// that comes out is provably the one that went in.
type panicValue struct{ where string }

// recovered runs run and returns what it panicked with, failing the case when
// it did not panic.
func recovered(t *testing.T, run func()) (value any) {
	t.Helper()
	defer func() { value = recover() }()
	run()
	t.Fatal("the loop returned instead of panicking")
	return nil
}

// panicsMidAnswer streams its fragments and then panics inside the call, the
// way a provider SDK that dereferences something it should not does.
type panicsMidAnswer struct {
	fragments []llm.Delta
	value     any
}

func (p *panicsMidAnswer) Model() string { return "panicky" }

func (p *panicsMidAnswer) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	for _, d := range p.fragments {
		req.Send(d)
	}
	panic(p.value)
}

func TestAPanicMidAnswerLeavesTheAttemptInFlight(t *testing.T) {
	t.Parallel()
	wrote := strings.Repeat("drafting the reply to the thread, ", 200) + "[where the SDK panicked]"
	value := &panicValue{"the provider"}
	progress := &toolloop.Progress{}
	got := recovered(t, func() {
		_, _ = toolloop.Run(t.Context(), toolloop.Config{
			Provider: &panicsMidAnswer{value: value, fragments: []llm.Delta{
				{Reasoning: "what to say"}, {Content: wrote},
			}},
			Surface: &fakeSurface{}, MaxRounds: 2, Progress: progress,
			// An hour, so only the first fragment publishes: the rest of the
			// attempt reaches the account through the panic alone.
			StreamPartials: true, PartialInterval: time.Hour,
			OnProgress: func(toolloop.Result) {},
		})
	})
	if got != value {
		t.Fatalf("the loop panicked with %v, want the provider's own value", got)
	}
	want := toolloop.Narration{Round: 1, Reasoning: "what to say", Content: wrote}
	if abandoned := progress.Snapshot().Abandoned; len(abandoned) != 1 || abandoned[0] != want {
		t.Errorf("the snapshot holds %d abandoned attempts; want the round in flight, whole",
			len(abandoned))
	}
}

// panicsOn is a surface whose one named tool panics, the way a tool handler
// with a bug does, and every other call succeeds.
type panicsOn struct {
	fakeSurface
	tool  string
	value any
}

func (s *panicsOn) Execute(ctx context.Context, call llm.ToolCall) (toolloop.ToolResult, error) {
	if call.Name == s.tool {
		panic(s.value)
	}
	return s.fakeSurface.Execute(ctx, call)
}

func TestAPanicInARoundsToolsLeavesTheCallsThatRan(t *testing.T) {
	t.Parallel()
	value := &panicValue{"a tool"}
	p := &scriptedProvider{turns: []llm.Completion{{
		Content: "reading, then posting", InputTokens: 40, OutputTokens: 10,
		ToolCalls: []llm.ToolCall{toolCall("1", "read"), toolCall("2", "post")},
	}}}
	s := &panicsOn{fakeSurface: fakeSurface{tools: []llm.ToolDef{def("read"), def("post")}},
		tool: "post", value: value}
	progress := &toolloop.Progress{}
	got := recovered(t, func() {
		_, _ = toolloop.Run(t.Context(), toolloop.Config{
			Provider: p, Surface: s, MaxRounds: 3, Progress: progress,
		})
	})
	if got != value {
		t.Fatalf("the loop panicked with %v, want the tool's own value", got)
	}
	snap := progress.Snapshot()
	if len(snap.Executions) != 1 || snap.Executions[0].Name != "read" || snap.Executions[0].Round != 1 {
		t.Errorf("executions = %+v, want the round-1 read that ran before the panic", snap.Executions)
	}
	if len(snap.Narration) != 1 || snap.Narration[0].Content != "reading, then posting" {
		t.Errorf("narration = %+v, want the round the panic came after", snap.Narration)
	}
	if snap.InputTokens != 40 || snap.OutputTokens != 10 || snap.RoundsUsed != 1 {
		t.Errorf("the snapshot reports %d/%d tokens over %d rounds; want the round's 40/10 over 1",
			snap.InputTokens, snap.OutputTokens, snap.RoundsUsed)
	}
}

// panicsOnSpend is a budget meter with a bug.
type panicsOnSpend struct{ value any }

func (m panicsOnSpend) Spend(context.Context, int) (toolloop.SpendOutcome, error) {
	panic(m.value)
}

func TestAPanicAfterTheAnswerLeavesItUncommittedAndBilled(t *testing.T) {
	t.Parallel()
	value := &panicValue{"the meter"}
	p := &scriptedProvider{turns: []llm.Completion{{
		Model: "served-model", InputTokens: 30, OutputTokens: 20,
		ReasoningContent: "the answer is ready", Content: "Posting it now.",
		ToolCalls: []llm.ToolCall{toolCall("1", "post")},
	}}}
	s := &fakeSurface{tools: []llm.ToolDef{def("post")}}
	progress := &toolloop.Progress{}
	got := recovered(t, func() {
		_, _ = toolloop.Run(t.Context(), toolloop.Config{
			Provider: p, Surface: s, MaxRounds: 3, Progress: progress, Budget: panicsOnSpend{value},
		})
	})
	if got != value {
		t.Fatalf("the loop panicked with %v, want the meter's own value", got)
	}
	snap := progress.Snapshot()
	want := toolloop.Narration{Round: 1, Reasoning: "the answer is ready", Content: "Posting it now."}
	if len(snap.Abandoned) != 1 || snap.Abandoned[0] != want {
		t.Errorf("abandoned = %+v, want the answer its round never committed", snap.Abandoned)
	}
	if snap.InputTokens != 30 || snap.OutputTokens != 20 || snap.RoundsUsed != 1 || snap.Model != "served-model" {
		t.Errorf("the snapshot reports %d/%d tokens over %d rounds on %q; want the billed 30/20 over 1 "+
			"on served-model", snap.InputTokens, snap.OutputTokens, snap.RoundsUsed, snap.Model)
	}
	if len(snap.Narration) != 0 || len(s.ran) != 0 {
		t.Errorf("the round went on past the panic: narration %+v, ran %v", snap.Narration, s.ran)
	}
}

// A FENCE THAT CLOSES PARTWAY THROUGH A ROUND LEAVES THE CALLS THAT RAN.
//
// The round's calls before the fence closed reached outside the engine, and
// the loop returns with no publish after them — so the snapshot the caller
// publishes on this error is the only record that holds them.
func TestAFenceThatClosesMidRoundLeavesTheCallsThatRan(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("seat lost")
	p := &scriptedProvider{turns: []llm.Completion{{
		Content: "posting the summary", InputTokens: 12,
		ToolCalls: []llm.ToolCall{toolCall("1", "post"), toolCall("2", "post")},
	}}}
	s := &fakeSurface{tools: []llm.ToolDef{def("post")}}
	progress := &toolloop.Progress{}
	// Open at the top of the round and before the first call, closed before
	// the second: a lease that moved while the round's calls were running.
	checks := 0
	_, err := toolloop.Run(t.Context(), toolloop.Config{
		Provider: p, Surface: s, MaxRounds: 3, Progress: progress,
		Fence: func() error {
			checks++
			if checks <= 2 {
				return nil
			}
			return sentinel
		},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the fence's own sentinel", err)
	}
	snap := progress.Snapshot()
	if len(snap.Executions) != 1 || snap.Executions[0].Name != "post" {
		t.Errorf("executions = %+v, want the post that ran before the fence closed", snap.Executions)
	}
	if snap.InputTokens != 12 || snap.RoundsUsed != 1 {
		t.Errorf("the snapshot reports %d input tokens over %d rounds; want 12 over 1",
			snap.InputTokens, snap.RoundsUsed)
	}
}

// A PANICKING ROUND ENDS ITS SPAN, FAILED.
//
// The round's span is ended after its provider call returns, so a call that
// panics would leave it open, and an open span is never exported: the trace
// would show the phase with no round at the point it broke.
//
// Not parallel: it installs a tracer provider for the process, which a
// parallel case would share while it ran.
func TestAPanickingRoundEndsItsSpanFailed(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	value := &panicValue{"the provider"}
	const phase = "panicking-round-span"
	_ = recovered(t, func() {
		_, _ = toolloop.Run(t.Context(), toolloop.Config{
			Provider: &panicsMidAnswer{value: value},
			Surface:  &fakeSurface{phaseVal: phase}, MaxRounds: 2,
		})
	})

	var rounds []sdktrace.ReadOnlySpan
	for _, span := range spans.Ended() {
		for _, attr := range span.Attributes() {
			if span.Name() == "llm.round" && attr.Key == "crewlet.phase" && attr.Value.AsString() == phase {
				rounds = append(rounds, span)
			}
		}
	}
	if len(rounds) != 1 {
		t.Fatalf("%d round spans ended for the panicking round; want 1", len(rounds))
	}
	if status := rounds[0].Status(); status.Code != codes.Error || !strings.Contains(status.Description, "panic") {
		t.Errorf("the round's span ended with status %v %q; want an error naming the panic",
			status.Code, status.Description)
	}
}

// A ROUND'S TOKENS COUNT FROM THE MOMENT ITS ANSWER ARRIVES.
//
// The provider billed them when it answered, so an account taken at any point
// after that — here, a panic out of the span processor as the round's span
// ends — carries them and the answer its round never committed.
//
// Not parallel, for the reason the case above gives.
func TestAPanicAsTheRoundsSpanEndsLeavesItsTokensCounted(t *testing.T) {
	const phase = "panicking-span-processor"
	value := &panicValue{"a span processor"}
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(panicsOnEnd{phase: phase, value: value})))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	p := &scriptedProvider{turns: []llm.Completion{{
		InputTokens: 25, OutputTokens: 5, Content: "Filing the ticket.",
		ToolCalls: []llm.ToolCall{toolCall("1", "file")},
	}}}
	progress := &toolloop.Progress{}
	got := recovered(t, func() {
		_, _ = toolloop.Run(t.Context(), toolloop.Config{
			Provider: p, Surface: &fakeSurface{tools: []llm.ToolDef{def("file")}, phaseVal: phase},
			MaxRounds: 2, Progress: progress,
		})
	})
	if got != value {
		t.Fatalf("the loop panicked with %v, want the span processor's own value", got)
	}
	snap := progress.Snapshot()
	if snap.InputTokens != 25 || snap.OutputTokens != 5 {
		t.Errorf("the snapshot counts %d/%d tokens; want the 25/5 the provider billed", snap.InputTokens,
			snap.OutputTokens)
	}
	if len(snap.Abandoned) != 1 || snap.Abandoned[0].Content != "Filing the ticket." {
		t.Errorf("abandoned = %+v, want the answer its round never committed", snap.Abandoned)
	}
}

// panicsOnEnd is a span processor that panics as one phase's round span ends.
type panicsOnEnd struct {
	phase string
	value any
}

func (p panicsOnEnd) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (p panicsOnEnd) Shutdown(context.Context) error                  { return nil }
func (p panicsOnEnd) ForceFlush(context.Context) error                { return nil }

func (p panicsOnEnd) OnEnd(span sdktrace.ReadOnlySpan) {
	for _, attr := range span.Attributes() {
		if span.Name() == "llm.round" && attr.Key == "crewlet.phase" && attr.Value.AsString() == p.phase {
			panic(p.value)
		}
	}
}
