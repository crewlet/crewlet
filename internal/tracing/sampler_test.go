package tracing

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// THE OPERATOR'S SAMPLER DECIDES A ROOT, AND THE NAME IS READ.
//
// OTEL_TRACES_SAMPLER was read by nothing: the provider was built with an
// explicit sampler, which overrides the one the SDK builds from the
// environment, so `parentbased_always_off` sampled everything. Each accepted
// name is checked by what a ROOT span does under it, and the parent-based half
// by what a child of a sampled remote parent does under the sampler that
// samples no root at all.
//
// Mutation: drop the OTEL_TRACES_SAMPLER switch so only the ratio is read, and
// the always_off case records its root.
func TestTheOperatorsSamplerDecidesARoot(t *testing.T) {
	for _, tc := range []struct {
		name, sampler, arg string
		recorded           bool
	}{
		{"unset samples everything", "", "", true},
		{"a ratio alone is parentbased_traceidratio", "", "0", false},
		{"parentbased_always_on", "parentbased_always_on", "", true},
		{"parentbased_always_off", "parentbased_always_off", "", false},
		{"case is not significant", "ParentBased_Always_Off", "", false},
		{"parentbased_traceidratio with no ratio is 1", "parentbased_traceidratio", "", true},
		{"parentbased_traceidratio with a ratio", "parentbased_traceidratio", "0", false},
		{"a ratio given to always_on is ignored", "parentbased_always_on", "0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configure(t, map[string]string{SamplerVar: tc.sampler, SamplerArgVar: tc.arg})

			ctx, span := Start(context.Background(), "test", "root")
			defer span.End()
			if span.IsRecording() != tc.recorded {
				t.Errorf("a root under %s=%q %s=%q recording = %v, want %v",
					SamplerVar, tc.sampler, SamplerArgVar, tc.arg,
					span.IsRecording(), tc.recorded)
			}
			// Sampled or not, the ids exist: the event store needs them.
			if len(TraceOf(ctx).TraceID) != 32 {
				t.Error("an unsampled root lost the ids the event store needs")
			}
			// THE ROOT RULE IS THE ONE THE PROVIDER RUNS, so a trace this
			// process roots without a span is decided alike.
			got := rootSampler().ShouldSample(sdktrace.SamplingParameters{
				TraceID: span.SpanContext().TraceID(),
			}).Decision == sdktrace.RecordAndSample
			if got != tc.recorded {
				t.Errorf("the root rule decides %v where the provider decided %v",
					got, tc.recorded)
			}
		})
	}

	// PARENT-BASED: a trace that arrives sampled stays sampled, even under
	// the sampler that samples no root of its own.
	configure(t, map[string]string{SamplerVar: samplerAlwaysOff})
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    trace.TraceID{1},
			SpanID:     trace.SpanID{1},
			TraceFlags: trace.FlagsSampled,
			Remote:     true,
		}))
	_, child := Start(ctx, "test", "child")
	defer child.End()
	if !child.IsRecording() {
		t.Error("a child of a sampled parent was dropped — half a tree at the collector")
	}
}

// A SAMPLER THAT IS NOT PARENT-BASED IS REFUSED BY NAME, with the spelling to
// use instead. Re-deciding a trace already in flight splits its tree, and an
// operator who wrote `always_on` almost always meant the parent-based one —
// which is named rather than silently substituted, because substituting is
// deciding for them.
func TestASamplerThatIsNotParentBasedIsRefused(t *testing.T) {
	configure(t, nil)
	before := otel.GetTracerProvider()

	for name, want := range map[string]string{
		"always_on":                 "parentbased_always_on",
		"always_off":                "parentbased_always_off",
		"TraceIDRatio":              "parentbased_traceidratio",
		"jaeger_remote":             "parentbased_always_on",
		"parentbased_jaeger_remote": "parentbased_traceidratio",
		"xray":                      "parentbased_always_off",
	} {
		_, err := Configure(context.Background(), Options{Env: envOf(map[string]string{
			SamplerVar: name,
		})})
		if err == nil {
			t.Errorf("%s=%s was accepted", SamplerVar, name)
			continue
		}
		for _, s := range []string{SamplerVar, want} {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("the refusal of %q does not name %q: %v", name, s, err)
			}
		}
	}
	if otel.GetTracerProvider() != before {
		t.Error("a refused sampler installed a provider")
	}
}

// ONLY W3C TRACE CONTEXT PROPAGATES. This engine writes no baggage, so the only
// baggage a context here could carry is what a third party put on a webhook —
// and a global propagator that extracted it would hand it to every outbound
// call that injects through the global propagator.
//
// Mutation: put propagation.Baggage{} back beside TraceContext and the inbound
// baggage survives the extract below.
func TestOnlyTraceContextPropagates(t *testing.T) {
	configure(t, nil)
	for _, p := range []propagation.TextMapPropagator{propagator(), otel.GetTextMapPropagator()} {
		if got := p.Fields(); !slices.Equal(got, []string{"traceparent", "tracestate"}) {
			t.Errorf("the propagator carries %v, want traceparent and tracestate alone", got)
		}
	}

	inbound := http.Header{}
	inbound.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	inbound.Set("baggage", "vendor.key=vendor-value")
	ctx := otel.GetTextMapPropagator().Extract(context.Background(),
		propagation.HeaderCarrier(inbound))
	if got := baggage.FromContext(ctx); got.Len() != 0 {
		t.Errorf("an inbound webhook's baggage was extracted: %v", got)
	}
	if !trace.SpanContextFromContext(ctx).IsValid() {
		t.Fatal("an inbound traceparent was not extracted")
	}

	member, err := baggage.NewMember("crewlet.key", "v")
	if err != nil {
		t.Fatal(err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatal(err)
	}
	outbound := http.Header{}
	otel.GetTextMapPropagator().Inject(baggage.ContextWithBaggage(ctx, bag),
		propagation.HeaderCarrier(outbound))
	if outbound.Get("baggage") != "" {
		t.Errorf("baggage was injected into an outbound call: %q", outbound.Get("baggage"))
	}
	if outbound.Get("traceparent") == "" {
		t.Error("the trace context was not injected")
	}
}
