package tracing

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

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
// THREE SHAPES of event cross a hop. One published from inside a span names
// that span; one published BENEATH a span by code that opens none (a chat
// wake, a sandbox run's announcements) names it only as the parent; and a
// root minted where no span was open names neither — a context the SDK does
// not count as a parent at all, which is what the `continued` root sampler is
// for.
//
// Mutations, each of which goes red here: force `trace.FlagsSampled` in
// WithRemote (the spanned and beneath, unsampled cases); let `continued` hand
// every root to its rule (both minted cases); drop `omitempty`'s field from
// the envelope or its key from Trace (every unsampled case); stop TraceOf
// reading `IsSampled` (every unsampled case).
func TestTheSamplingDecisionCrossesTheQueue(t *testing.T) {
	for _, tc := range []struct {
		publisher, restorer string
		sampled             bool
	}{
		{samplerAlwaysOff, samplerAlwaysOn, false},
		{samplerAlwaysOn, samplerAlwaysOff, true},
	} {
		for _, shape := range []string{"spanned", "beneath", "minted"} {
			name := fmt.Sprintf("rooted under %s, continued under %s, %s",
				tc.publisher, tc.restorer, shape)
			t.Run(name, func(t *testing.T) {
				// THE PUBLISHING NODE: a root decided by its own rule.
				configure(t, map[string]string{SamplerVar: tc.publisher})
				ctx, root := Start(context.Background(), "test", "webhook.receive")
				published := TraceOf(ctx)
				root.End()
				switch shape {
				case "beneath":
					published = published.Beneath()
				case "minted":
					published = TraceOf(context.Background())
				}
				if published.Unsampled == tc.sampled {
					t.Fatalf("the root under %s reports unsampled=%v — the premise",
						tc.publisher, published.Unsampled)
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
			if len(got.TraceID) != 32 || got.SpanID != "" || got.ParentSpanID != "" {
				t.Fatalf("a span-less context produced %+v", got)
			}
			if got.Unsampled == tc.sampled {
				t.Errorf("a minted root reports unsampled=%v, want %v", got.Unsampled, !tc.sampled)
			}
		})
	}
}

// AN EVENT PUBLISHED ON A RESTORED TRACE IS PUBLISHED BENEATH THE SPAN THE
// TRACE WAS RESTORED TO, and claims no span of its own: the restored span ended
// on another node, and the code publishing here opened none. What is restored
// is the span the arriving event was published in — or, for one itself
// published beneath a span, that span — so the report is the arriving trace's
// own Beneath, decision included. Restored again, it continues the same span:
// a relay of a relay is beneath the span the first one was.
//
// The context it restores onto already holds a span with a parent of its own,
// which nothing of the restored trace may keep: WithRemote once set the span
// and left the parent, so an event published from the restored context claimed
// the old span's parent.
//
// Mutations: report a restored span as the context's own (every spanned case
// claims another node's span); restore a span-less trace with no span (the
// beneath cases lose their parent); read a stale parent off the context (the
// spanned cases name the caller's).
func TestAnEventOnARestoredTraceIsPublishedBeneathIt(t *testing.T) {
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
		{TraceID: traceID},
		{TraceID: traceID, Unsampled: true},
	} {
		t.Run(fmt.Sprintf("%+v", tc), func(t *testing.T) {
			reported := TraceOf(WithRemote(stale, tc))
			if reported != tc.Beneath() {
				t.Errorf("restored %+v, an event published there carries %+v, want %+v",
					tc, reported, tc.Beneath())
			}
			if again := TraceOf(WithRemote(stale, reported)); again != reported {
				t.Errorf("restoring what was reported, %+v, reports %+v — a relay of a "+
					"relay is not beneath the span the first one was", reported, again)
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
// The caller's span has to be REAL for the clearing to be tested at all: on a
// provider that opens no span the "caller's span" is the zero context, which
// renders nothing whether or not it is cleared. So the case configures its own
// provider and asserts the launching span is valid before it relies on it.
//
// Mutations: render it as `"00-" + TraceID + "-" + SpanID + "-01"` and the
// unsampled case goes red; stop clearing the caller's span and the no-trace
// and unparseable-trace cases render the caller's.
func TestATraceparentCarriesTheTracesDecision(t *testing.T) {
	configure(t, nil)
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
		// Published beneath a span, so a child started there is that
		// span's child too.
		{"beneath a span", events.TraceContext{TraceID: traceID, ParentSpanID: spanID, Unsampled: true},
			"00-" + traceID + "-" + spanID + "-00"},
		// No span to hang under, so nothing a child could parent itself to.
		{"no span", events.TraceContext{TraceID: traceID}, ""},
		{"no trace", events.TraceContext{}, ""},
		// A span named in a trace nothing can parse: WithRemote cannot use
		// it, so only the clearing stands between it and the caller's span.
		{"an unparseable trace", events.TraceContext{TraceID: "not-hex", SpanID: spanID}, ""},
		{"a malformed span", events.TraceContext{TraceID: traceID, SpanID: "span-1"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Under a context holding a span of its own, which tc replaces.
			ctx, span := Start(context.Background(), "test", "launching")
			defer span.End()
			if !span.SpanContext().IsValid() {
				t.Fatal("the launching span is not valid, so there is no caller's span " +
					"for the clearing to remove and the case tests nothing")
			}
			got := Traceparent(ctx, tc.trace)
			if got != tc.want {
				t.Fatalf("Traceparent(%+v) = %q, want %q", tc.trace, got, tc.want)
			}
			if got == "" {
				return
			}
			sc := trace.SpanContextFromContext(propagator().Extract(context.Background(),
				envCarrier{TraceparentVar: got}))
			parent := tc.trace.SpanID
			if parent == "" {
				parent = tc.trace.ParentSpanID
			}
			if sc.TraceID().String() != tc.trace.TraceID ||
				sc.SpanID().String() != parent ||
				sc.IsSampled() == tc.trace.Unsampled {
				t.Errorf("%q reads back as %s/%s sampled=%v, want %+v",
					got, sc.TraceID(), sc.SpanID(), sc.IsSampled(), tc.trace)
			}
		})
	}
}

// A PROCESS IS HANDED THE SPAN IT RUNS BENEATH, decision and vendor state
// included, under the variable names it reads — and nothing at all where
// there is no span, rather than a parent invented for it.
//
// The span is opened beneath a restored parent of each decision, so the case
// does not depend on whichever root rule another test left configured — and
// it is asserted to BE a span this process opened: on a provider that opens
// none, Start hands back the restored parent itself, and the case would be
// testing a remote span rather than the round it names.
//
// Mutations: inject into a MapCarrier and return it as is (every name is
// lower-case, so every case goes red); return an empty map instead of nil
// for no span (the span-less cases); force sampled in WithRemote (the
// unsampled case).
func TestAProcessIsHandedTheSpanItRunsBeneath(t *testing.T) {
	configure(t, nil)
	const traceID, parentID = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	for _, unsampled := range []bool{false, true} {
		parent := WithRemote(context.Background(),
			events.TraceContext{TraceID: traceID, SpanID: parentID, Unsampled: unsampled})
		ctx, span := Start(parent, "test", "llm.round")
		if sc := span.SpanContext(); sc.IsRemote() || sc.SpanID().String() == parentID {
			t.Fatalf("unsampled=%v: llm.round is the restored parent (remote=%v span=%s), "+
				"not a span this process opened", unsampled, sc.IsRemote(), sc.SpanID())
		}
		flags := "-01"
		if unsampled {
			flags = "-00"
		}
		want := "00-" + traceID + "-" + span.SpanContext().SpanID().String() + flags
		env := ProcessEnv(ctx)
		if env[TraceparentVar] != want {
			t.Errorf("unsampled=%v: %s = %q, want %q", unsampled, TraceparentVar, env[TraceparentVar], want)
		}
		if _, set := env[TracestateVar]; set || len(env) != 1 {
			t.Errorf("unsampled=%v: a trace with no vendor state set %v", unsampled, env)
		}
		span.End()
	}

	state, err := trace.ParseTraceState("vendor=opaque")
	if err != nil {
		t.Fatalf("ParseTraceState: %v", err)
	}
	withState := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled, TraceState: state,
	}))
	if got := ProcessEnv(withState)[TracestateVar]; got != "vendor=opaque" {
		t.Errorf("%s = %q, want the trace's vendor state", TracestateVar, got)
	}

	for name, ctx := range map[string]context.Context{
		"no span":       context.Background(),
		"a trace alone": WithRemote(context.Background(), events.TraceContext{TraceID: traceID}),
	} {
		if env := ProcessEnv(ctx); env != nil {
			t.Errorf("%s: handed %v, want nothing", name, env)
		}
	}
}

// Every name ProcessEnv can set is listed, because a layer of a child's
// environment the engine owns is told which names to leave alone from it.
func TestProcessEnvNamesEveryVariableItSets(t *testing.T) {
	want := []string{TraceparentVar, TracestateVar}
	if got := ProcessEnvNames(); !slices.Equal(got, want) {
		t.Errorf("ProcessEnvNames() = %v, want %v", got, want)
	}
}
