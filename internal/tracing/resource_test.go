package tracing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"

	"github.com/crewlet/crewlet/internal/events"
)

func attr(res *resource.Resource, key attribute.Key) string {
	v, _ := res.Set().Value(key)
	return v.AsString()
}

// THE OPERATOR'S RESOURCE ATTRIBUTES WIN OVER THIS PACKAGE'S FALLBACKS.
//
// The fallbacks were merged LAST, so `crewlet` overwrote the service.name an
// operator wrote into OTEL_RESOURCE_ATTRIBUTES and the node id overwrote their
// service.instance.id — beside a comment promising the variable kept working.
// And OTEL_SERVICE_NAME wins over OTEL_RESOURCE_ATTRIBUTES's service.name, as
// the specification says.
//
// Mutation: put resource.WithFromEnv() before resource.WithAttributes and the
// first operator case goes red on every key.
//
// NOT PARALLEL: the SDK's detector reads the process environment.
func TestTheOperatorsResourceAttributesWin(t *testing.T) {
	opts := Options{NodeID: "node-a", Version: "v9"}
	for _, tc := range []struct {
		name, attrs, service string
		want                 map[attribute.Key]string
	}{
		{"nothing set: the fallbacks", "", "", map[attribute.Key]string{
			semconv.ServiceNameKey:       "crewlet",
			semconv.ServiceVersionKey:    "v9",
			semconv.ServiceInstanceIDKey: "node-a",
		}},
		{"OTEL_RESOURCE_ATTRIBUTES wins over every fallback",
			"service.name=acme-crewlet,service.version=ops-1,service.instance.id=pod-7,deployment.environment.name=prod",
			"", map[attribute.Key]string{
				semconv.ServiceNameKey:        "acme-crewlet",
				semconv.ServiceVersionKey:     "ops-1",
				semconv.ServiceInstanceIDKey:  "pod-7",
				"deployment.environment.name": "prod",
			}},
		{"OTEL_SERVICE_NAME wins over both",
			"service.name=from-attributes", "from-service-name", map[attribute.Key]string{
				semconv.ServiceNameKey:       "from-service-name",
				semconv.ServiceInstanceIDKey: "node-a",
			}},
		{"a malformed entry is skipped and the rest kept",
			"team=platform,no-equals", "", map[attribute.Key]string{
				"team":                 "platform",
				semconv.ServiceNameKey: "crewlet",
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(ResourceAttributesVar, tc.attrs)
			t.Setenv(ServiceNameVar, tc.service)
			res, err := resourceFor(context.Background(), opts)
			if err != nil {
				t.Fatalf("resourceFor: %v", err)
			}
			for key, want := range tc.want {
				if got := attr(res, key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			if attr(res, semconv.TelemetrySDKLanguageKey) != "go" {
				t.Error("the SDK's own telemetry.sdk.* attributes are missing")
			}
			if res.SchemaURL() != semconv.SchemaURL {
				t.Errorf("schema = %q, want %q — a resource merged from two "+
					"schema URLs is refused outright", res.SchemaURL(), semconv.SchemaURL)
			}
		})
	}
}

// A FAILURE'S TEXT IS BOUNDED ON THE SPAN, at the bound the same text already
// has on an event.
//
// A provider chain that exhausted carries every attempt's body, and a collector
// refuses an export request past its size limit WHOLE — so one unbounded
// failure takes the batch's other spans with it, and the retry sends the same
// refused batch again.
//
// Mutation: go back to span.RecordError(err) and SetStatus(codes.Error,
// err.Error()), and both bounds below go red.
func TestAFailureIsRecordedBoundedAndTypedAsRecordErrorTypesIt(t *testing.T) {
	t.Parallel()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	tracer := tp.Tracer("test")

	huge := fmt.Errorf("chain exhausted: %w",
		errors.New(strings.Repeat("an attempt's whole body ", events.MaxDiagnosticBytes)))
	_, failed := tracer.Start(context.Background(), "failed")
	Fail(failed, huge)
	failed.End()

	_, reference := tracer.Start(context.Background(), "reference")
	reference.RecordError(huge)
	reference.End()

	_, short := tracer.Start(context.Background(), "short")
	Fail(short, errors.New("refused"))
	short.End()

	// A NAMED error type too, which RecordError names by package path and
	// name rather than by its spelling.
	_, named := tracer.Start(context.Background(), "named")
	Fail(named, namedFailure{"refused"})
	named.End()
	_, namedReference := tracer.Start(context.Background(), "named-reference")
	namedReference.RecordError(namedFailure{"refused"})
	namedReference.End()

	ended := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans.Ended() {
		ended[s.Name()] = s
	}
	const ceiling = events.MaxDiagnosticBytes + len("…")

	got := ended["failed"]
	if got.Status().Code != codes.Error {
		t.Errorf("status = %v, want Error", got.Status().Code)
	}
	if n := len(got.Status().Description); n > ceiling {
		t.Errorf("the status description is %d bytes, past %d", n, ceiling)
	}
	if !strings.HasPrefix(got.Status().Description, "chain exhausted: ") ||
		!strings.HasSuffix(got.Status().Description, "…") {
		t.Error("the cut did not keep the head, or was not marked")
	}
	message, typ := exception(t, got)
	if len(message) > ceiling {
		t.Errorf("exception.message is %d bytes, past %d", len(message), ceiling)
	}
	if _, refType := exception(t, ended["reference"]); typ != refType {
		t.Errorf("exception.type = %q, RecordError would say %q — a backend "+
			"grouping failures by type would split them", typ, refType)
	}

	if d := ended["short"].Status().Description; d != "refused" {
		t.Errorf("a short message was changed: %q", d)
	}
	_, namedType := exception(t, ended["named"])
	if _, want := exception(t, ended["named-reference"]); namedType != want {
		t.Errorf("exception.type = %q, RecordError would say %q", namedType, want)
	}

	// A nil error and a span nothing records are both nothing at all.
	Fail(failed, nil)
	_, unsampled := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.NeverSample())).Tracer("t").Start(context.Background(), "x")
	Fail(unsampled, huge)
}

// namedFailure is an error of a named, non-pointer type.
type namedFailure struct{ msg string }

func (e namedFailure) Error() string { return e.msg }

func exception(t *testing.T, s sdktrace.ReadOnlySpan) (message, typ string) {
	t.Helper()
	for _, e := range s.Events() {
		if e.Name != semconv.ExceptionEventName {
			continue
		}
		for _, a := range e.Attributes {
			switch a.Key {
			case semconv.ExceptionMessageKey:
				message = a.Value.AsString()
			case semconv.ExceptionTypeKey:
				typ = a.Value.AsString()
			}
		}
		return message, typ
	}
	t.Fatalf("%s recorded no exception event", s.Name())
	return "", ""
}
