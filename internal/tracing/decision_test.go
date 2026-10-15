package tracing

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/crewlet/crewlet/internal/events"
)

// probe is a payload no build registers, which is all a hop needs: the trace
// is the envelope's, and an unknown type carries its envelope like any other.
type probe struct{}

func (*probe) EventType() string { return "tracing_decision_probe" }

// THE SAMPLING DECISION CROSSES THE QUEUE, both ways, whatever the node that
// restores it would have decided.
//
// The publisher and the restorer run OPPOSITE root rules — the publisher's
// node roots its trace under one, and the trace is continued on a node
// configured with the other — so the restored child can only come out as the
// publisher decided if the decision itself crossed the hop: through TraceOf
// into the envelope, through the event's JSON, and back through WithRemote
// into the sampler. A restorer that re-decided would answer with its own rule
// and fail the half it disagrees with.
//
// TWO SHAPES of event cross a hop. One published from inside a span names that
// span; one published BENEATH a span by code that opens none (a chat wake, a
// sandbox run's announcements) names it only as the parent and has no span id
// — a context the SDK does not count as a parent at all, which is what the
// `continued` root sampler is for.
//
// Mutations, each of which goes red here: force `trace.FlagsSampled` in
// WithRemote (the spanned, unsampled cases); let `continued` hand every root
// to its rule (both span-less cases); drop `omitempty`'s field from the
// envelope or its key from Trace (every unsampled case); stop TraceOf reading
// `IsSampled` (every unsampled case).
func TestTheSamplingDecisionCrossesTheQueue(t *testing.T) {
	for _, tc := range []struct {
		publisher, restorer string
		sampled             bool
	}{
		{samplerAlwaysOff, samplerAlwaysOn, false},
		{samplerAlwaysOn, samplerAlwaysOff, true},
	} {
		for _, beneath := range []bool{false, true} {
			name := fmt.Sprintf("rooted under %s, continued under %s, span-less=%v",
				tc.publisher, tc.restorer, beneath)
			t.Run(name, func(t *testing.T) {
				// THE PUBLISHING NODE: a root decided by its own rule.
				configure(t, map[string]string{SamplerVar: tc.publisher})
				ctx, root := Start(context.Background(), "test", "webhook.receive")
				published := TraceOf(ctx)
				root.End()
				if published.Unsampled == tc.sampled {
					t.Fatalf("the root under %s reports unsampled=%v — the premise",
						tc.publisher, published.Unsampled)
				}
				if beneath {
					published = published.Beneath()
				}
				wire, err := json.Marshal(events.New(probe{}, published))
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}

				// THE RESTORING NODE, configured the other way round.
				configure(t, map[string]string{SamplerVar: tc.restorer})
				var arrived events.Event
				if err := json.Unmarshal(wire, &arrived); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if got := arrived.Trace(); got != published {
					t.Fatalf("the envelope carried %+v across the hop, want %+v\n%s",
						got, published, wire)
				}
				ctx, child := Start(WithRemote(context.Background(), arrived.Trace()),
					"test", "agent.turn")
				defer child.End()

				if child.IsRecording() != tc.sampled {
					t.Errorf("the continuation recorded=%v under %s, want the root's "+
						"sampled=%v — a re-decided trace splits its tree at the collector",
						child.IsRecording(), tc.restorer, tc.sampled)
				}
				got := TraceOf(ctx)
				if got.TraceID != published.TraceID {
					t.Errorf("the continuation left the trace: %s, want %s",
						got.TraceID, published.TraceID)
				}
				if got.Unsampled == tc.sampled {
					t.Errorf("the continuation's events carry unsampled=%v, want %v — "+
						"the NEXT hop would re-decide", got.Unsampled, !tc.sampled)
				}
			})
		}
	}
}

// A ROOT MINTED WITHOUT A SPAN IS DECIDED BY THE ROOT RULE, not assumed
// sampled. An event published outside any span starts a trace of its own, and
// its decision has to be the one the operator's sampler would have made for a
// span-rooted trace — or a ratio governs only the traces that happen to start
// with a span.
//
// Mutation: drop the decision TraceOf takes for a minted root and the
// rule-says-no cases go red.
func TestAMintedRootIsDecidedByTheRootRule(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		sampled bool
	}{
		{"the default samples", nil, true},
		{"parentbased_always_off", map[string]string{SamplerVar: samplerAlwaysOff}, false},
		{"parentbased_always_on", map[string]string{SamplerVar: samplerAlwaysOn}, true},
		{"a ratio of 0", map[string]string{SamplerArgVar: "0"}, false},
		{"a ratio of 1", map[string]string{SamplerArgVar: "1"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configure(t, tc.env)
			got := TraceOf(context.Background())
			if len(got.TraceID) != 32 || len(got.SpanID) != 16 {
				t.Fatalf("a span-less context produced %+v", got)
			}
			if got.Unsampled == tc.sampled {
				t.Errorf("a minted root reports unsampled=%v, want %v", got.Unsampled, !tc.sampled)
			}
		})
	}
}

// WHAT WITHREMOTE RESTORES, TRACEOF REPORTS BACK, field for field — the
// parent and the decision included, and a trace restored with no span id kept
// rather than swapped for a fresh root.
//
// The context it restores onto already holds a span with a parent of its own,
// which is the case WithRemote used to get wrong: it set the span and left the
// parent, so an event published from the restored context claimed the old
// span's parent.
//
// Mutations: drop the parent WithRemote sets (the stale parent survives); drop
// the span-less case from TraceOf (it mints a trace the event was never in).
func TestARestoredTraceIsReportedBackUnchanged(t *testing.T) {
	configure(t, nil)
	outer, outerSpan := Start(context.Background(), "test", "outer")
	defer outerSpan.End()
	stale, inner := Start(outer, "test", "inner")
	defer inner.End()

	const (
		traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		spanID  = "00f067aa0ba902b7"
		parent  = "b7ad6b7169203331"
	)
	for _, tc := range []events.TraceContext{
		{TraceID: traceID, SpanID: spanID, ParentSpanID: parent},
		{TraceID: traceID, SpanID: spanID, ParentSpanID: parent, Unsampled: true},
		{TraceID: traceID, SpanID: spanID},
		{TraceID: traceID, ParentSpanID: parent},
		{TraceID: traceID, ParentSpanID: parent, Unsampled: true},
	} {
		t.Run(fmt.Sprintf("%+v", tc), func(t *testing.T) {
			if got := TraceOf(WithRemote(stale, tc)); got != tc {
				t.Errorf("restored %+v, reported %+v", tc, got)
			}
		})
	}
}

// THE TRACEPARENT A CHILD PROCESS IS HANDED CARRIES THE TRACE'S OWN DECISION,
// and it reads back, through the propagator every inbound request is read
// with, as the span context it was rendered from.
//
// The sandbox used to format it by hand with `-01`, so every coding box was
// told its run was sampled. And it is rendered from tc ALONE: the caller's own
// context holds a span here, which a trace tc cannot name must not leak into.
//
// Mutations: render it as `"00-" + TraceID + "-" + SpanID + "-01"` and the
// unsampled case goes red; stop clearing the caller's span and the no-trace
// case renders the caller's.
func TestATraceparentCarriesTheTracesDecision(t *testing.T) {
	const traceID, spanID = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	for _, tc := range []struct {
		name  string
		trace events.TraceContext
		want  string
	}{
		{"sampled", events.TraceContext{TraceID: traceID, SpanID: spanID},
			"00-" + traceID + "-" + spanID + "-01"},
		{"unsampled", events.TraceContext{TraceID: traceID, SpanID: spanID, Unsampled: true},
			"00-" + traceID + "-" + spanID + "-00"},
		// No span to hang under, so nothing a child could parent itself to.
		{"no span", events.TraceContext{TraceID: traceID}, ""},
		{"no trace", events.TraceContext{}, ""},
		{"a malformed span", events.TraceContext{TraceID: traceID, SpanID: "span-1"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Under a context holding a span of its own, which tc replaces.
			ctx, span := Start(context.Background(), "test", "launching")
			defer span.End()
			got := Traceparent(ctx, tc.trace)
			if got != tc.want {
				t.Fatalf("Traceparent(%+v) = %q, want %q", tc.trace, got, tc.want)
			}
			if got == "" {
				return
			}
			sc := trace.SpanContextFromContext(propagator().Extract(context.Background(),
				propagation.MapCarrier{traceparentHeader: got}))
			if sc.TraceID().String() != tc.trace.TraceID ||
				sc.SpanID().String() != tc.trace.SpanID ||
				sc.IsSampled() == tc.trace.Unsampled {
				t.Errorf("%q reads back as %s/%s sampled=%v, want %+v",
					got, sc.TraceID(), sc.SpanID(), sc.IsSampled(), tc.trace)
			}
		})
	}
}
