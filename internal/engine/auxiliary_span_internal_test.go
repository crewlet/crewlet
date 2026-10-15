package engine

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/tracing/tracingtest"
)

// EVERY AUXILIARY CALL IS SPANNED, and its spend record is filed under the
// span. Without the span, a call whose caller held none — reflection, a
// background pass, a person's question — was three unrelated roots: the
// provider's own span of it, the Claude API's beneath that, and the trace the
// record minted.

// spanSeen is a provider that remembers the span context it was called under —
// where a provider's own span of the call would nest.
type spanSeen struct {
	answeringProvider
	under trace.SpanContext
}

func (p *spanSeen) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	p.under = trace.SpanContextFromContext(ctx)
	return p.answeringProvider.Complete(ctx, req)
}

// spendEvent is the one auxiliary_spend event the rig's ledger published.
func (r *seamRig) spendEvent(t *testing.T) *events.Event {
	t.Helper()
	r.ledger.Flush(t.Context())
	r.pub.mu.Lock()
	defer r.pub.mu.Unlock()
	if len(r.pub.got) != 1 {
		t.Fatalf("the ledger published %d events, want one", len(r.pub.got))
	}
	return r.pub.got[0]
}

// A CALL MADE UNDER A SPAN IS A CHILD OF IT, and the provider and the record
// both see the call's own span — what the SDK's span of the call nests under,
// and what the record names as the span that spent the tokens.
//
// A billed failure keeps its bill on the span and is marked failed, because the
// record counts it and a span that dropped it would under-report exactly the
// calls a reader is hunting.
//
// Mutations, each red here: hand the provider the caller's context rather than
// the span's (the provider half); read the record's trace from the caller's
// context (the record half); drop the Fail (the failed case); drop any one
// attribute.
func TestAnAuxiliaryCallIsSpannedBeneathItsCaller(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		err    error
		failed bool
	}{
		{name: "answered"},
		{name: "billed and failed", err: errors.New("cut off"), failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider := &spanSeen{answeringProvider: answeringProvider{
				in: 700, out: 300, cacheRead: 200, err: tc.err, completion: true}}
			r := newSeamRig(provider, nil)
			caller := events.NewTrace()
			tracingtest.Watch(t, caller.TraceID)
			ctx := tracing.WithRemote(t.Context(), caller)

			member, err := r.seam.Auxiliary(dev, turnUse)
			if err != nil {
				t.Fatalf("Auxiliary: %v", err)
			}
			if _, err := member.Provider.Complete(ctx, llm.Request{}); (err != nil) != tc.failed {
				t.Fatalf("Complete = %v", err)
			}

			span := tracingtest.Only(t, caller.TraceID, auxiliarySpan)
			if got := span.Parent().SpanID().String(); got != caller.SpanID {
				t.Errorf("llm.auxiliary's parent is %s, want the caller's span %s", got, caller.SpanID)
			}
			if provider.under.SpanID() != span.SpanContext().SpanID() {
				t.Errorf("the provider was called under span %s, want llm.auxiliary's %s — "+
					"its own span of the call would not nest beneath this one",
					provider.under.SpanID(), span.SpanContext().SpanID())
			}
			for key, want := range map[string]string{
				"crewlet.stage":              string(types.AuxStageTurn),
				"crewlet.purpose":            string(types.AuxMemoryFilter),
				"crewlet.seat":               dev.Handle(),
				"crewlet.model":              "test-model",
				"crewlet.input_tokens":       "700",
				"crewlet.output_tokens":      "300",
				"crewlet.cache_read_tokens":  "200",
				"crewlet.cache_write_tokens": "0",
			} {
				if got := tracingtest.Attr(span, key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			if failed := span.Status().Code == codes.Error; failed != tc.failed {
				t.Errorf("span status %v, want failed=%v", span.Status(), tc.failed)
			}

			rec := r.spendEvent(t).Trace()
			want := events.TraceContext{TraceID: caller.TraceID,
				SpanID:       span.SpanContext().SpanID().String(),
				ParentSpanID: caller.SpanID}
			if rec != want {
				t.Errorf("the spend record is filed under %+v, want the call's span %+v", rec, want)
			}
		})
	}
}

// A CALL WHOSE CALLER HOLDS NO SPAN ROOTS ITS OWN, and the record names that
// trace: the provider's spans of the call and its spend are still one trace.
// This is the case that used to be three unrelated roots.
//
// Mutation, red here: open no span (the record mints a trace the provider was
// never called in).
func TestAnAuxiliaryCallWithNoCallerSpanRootsOneTrace(t *testing.T) {
	t.Parallel()
	tracingtest.WatchNamed(auxiliarySpan)
	provider := &spanSeen{answeringProvider: answeringProvider{in: 5, out: 5}}
	r := newSeamRig(provider, nil)
	member, err := r.seam.Auxiliary(dev, auxspend.Use{Stage: types.AuxStageBackground,
		Purpose: types.AuxEpisodeCompaction})
	if err != nil {
		t.Fatalf("Auxiliary: %v", err)
	}
	if _, err := member.Provider.Complete(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	rec := r.spendEvent(t).Trace()
	span := tracingtest.Only(t, rec.TraceID, auxiliarySpan)
	if span.Parent().IsValid() {
		t.Errorf("llm.auxiliary has parent %s, want a root", span.Parent().SpanID())
	}
	if rec.SpanID != span.SpanContext().SpanID().String() || rec.ParentSpanID != "" {
		t.Errorf("the record is filed under %+v, want the root llm.auxiliary %s",
			rec, span.SpanContext().SpanID())
	}
	if provider.under.TraceID().String() != rec.TraceID {
		t.Errorf("the provider was called in trace %s, the record names %s",
			provider.under.TraceID(), rec.TraceID)
	}
}

// A CALL THE TURN'S METER HOLDS IS NOT SPANNED: nobody made it.
//
// Mutation, red here: open the span before the gate.
func TestAHeldAuxiliaryCallOpensNoSpan(t *testing.T) {
	t.Parallel()
	r := newSeamRig(&answeringProvider{in: 1, out: 1}, nil)
	use := turnUse
	use.Budget = &turnBudget{held: &toolloop.BudgetError{
		Scope: "agent", Used: 2, Limit: 1, Period: period.Day, Window: "2026-09-23",
	}}
	use.Tally = auxspend.NewTally()
	caller := events.NewTrace()
	tracingtest.Watch(t, caller.TraceID)
	member, err := r.seam.Auxiliary(dev, use)
	if err != nil {
		t.Fatalf("Auxiliary: %v", err)
	}
	if _, err := member.Provider.Complete(tracing.WithRemote(t.Context(), caller), llm.Request{}); !errors.Is(err, toolloop.ErrBudgetExhausted) {
		t.Fatalf("Complete = %v, want the turn's refusal", err)
	}
	if spans := tracingtest.Named(caller.TraceID, auxiliarySpan); len(spans) != 0 {
		t.Errorf("a call the meter held opened %d llm.auxiliary span(s)", len(spans))
	}
}
