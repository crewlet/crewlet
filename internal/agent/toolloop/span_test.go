package toolloop_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/tracing/tracingtest"
)

// spanSeen remembers the span context each call was made under — where a
// provider's own span of the call nests.
type spanSeen struct {
	*scriptedProvider
	under []trace.SpanContext
}

func (p *spanSeen) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	p.under = append(p.under, trace.SpanContextFromContext(ctx))
	return p.scriptedProvider.Complete(ctx, req)
}

// A ROUND'S SPAN IS THE PROVIDER'S PARENT AND CARRIES WHAT THE ROUND WAS
// BILLED — on a refused round too. The provider is handed `llm.round`'s own
// context, so the Anthropic SDK's span of the call nests beneath it rather
// than beside it; and a refusal the vendor charged for keeps its model and
// tokens on the span, as the phase record keeps them, rather than ending with
// nothing but an error — the refused rounds are exactly the ones a reader of
// the span is hunting.
//
// Mutations, each red here: call the provider on the loop's context (the
// parent half); drop the billed attributes on the failure path (the refused
// case); drop the Fail (the refused case's status).
func TestARoundSpanParentsItsCallAndCarriesItsBill(t *testing.T) {
	t.Parallel()
	refused := &llm.Completion{Model: "claude-x", InputTokens: 30, OutputTokens: 2,
		StopReason: llm.StopRefusal}
	for _, tc := range []struct {
		name     string
		provider *scriptedProvider
		input    string
		stop     string
		failed   bool
	}{
		{name: "answered", provider: &scriptedProvider{turns: []llm.Completion{
			{Model: "claude-x", Content: "done", InputTokens: 12, OutputTokens: 3,
				StopReason: llm.StopEnd},
		}}, input: "12", stop: string(llm.StopEnd)},
		{name: "refused and billed", provider: &scriptedProvider{failAt: 1,
			err: llm.Refused("anthropic", "claude-x", &llm.Refusal{Category: "cyber", Completion: refused})},
			input: "30", stop: string(llm.StopRefusal), failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			phase := events.NewTrace()
			tracingtest.Watch(t, phase.TraceID)
			p := &spanSeen{scriptedProvider: tc.provider}
			_, err := toolloop.Run(tracing.WithRemote(t.Context(), phase), toolloop.Config{
				Provider: p, Surface: &fakeSurface{}, MaxRounds: 1,
				Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
			})
			if (err != nil) != tc.failed {
				t.Fatalf("Run = %v", err)
			}
			span := tracingtest.Only(t, phase.TraceID, "llm.round")
			if got := span.Parent().SpanID().String(); got != phase.SpanID {
				t.Errorf("llm.round's parent is %s, want the phase's %s", got, phase.SpanID)
			}
			if len(p.under) != 1 || p.under[0].SpanID() != span.SpanContext().SpanID() {
				t.Errorf("the provider was called under %v, want llm.round's span %s",
					p.under, span.SpanContext().SpanID())
			}
			for key, want := range map[string]string{
				"crewlet.model":        "claude-x",
				"crewlet.input_tokens": tc.input,
				"crewlet.stop_reason":  tc.stop,
			} {
				if got := tracingtest.Attr(span, key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			if failed := span.Status().Code == codes.Error; failed != tc.failed {
				t.Errorf("span status %v, want failed=%v", span.Status(), tc.failed)
			}
		})
	}
}
