package mcpbridge_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/tracing/tracingtest"
)

// sendsTraceparent adds a traceparent to every request, as Claude Code does on
// an MCP request it makes from inside one of its tool spans.
type sendsTraceparent struct {
	base  http.RoundTripper
	value string
}

func (s sendsTraceparent) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("traceparent", s.value)
	return s.base.RoundTrip(req)
}

// dialSending connects as [dial] does, every request carrying traceparent.
func dialSending(t *testing.T, url, traceparent string) *mcp.ClientSession {
	t.Helper()
	pool := httpxtest.Pool(t)
	client := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: url,
		HTTPClient: &http.Client{Transport: sendsTraceparent{
			base: pool.Transport, value: traceparent,
		}},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// A BRIDGED CALL NESTS UNDER THE BOX'S OWN SPAN — the one its request names —
// when that span is in the run's trace, and under the span that launched the
// run otherwise: never in a trace the run was not launched in. A request with
// no traceparent, one naming another trace and one that is malformed all
// leave the call under the run's own parent; the run's sampling decision
// stands whatever flag the box sends.
//
// Mutations, each red here: dispatch on the request's context (every case —
// the call roots a trace of its own); follow any valid traceparent (the
// foreign case — the call lands in another trace); ignore the request's
// header (the in-trace case).
func TestABridgedCallNestsUnderTheBoxsSpanOnlyInItsRunsTrace(t *testing.T) {
	t.Parallel()
	const boxSpan = "b7ad6b7169203331"
	foreign := events.NewTrace()
	for _, tc := range []struct {
		name string
		// header builds the request's traceparent from the run's trace.
		header func(run events.TraceContext) string
		want   func(run events.TraceContext) string
	}{
		{
			name:   "no traceparent",
			header: func(events.TraceContext) string { return "" },
			want:   func(run events.TraceContext) string { return run.SpanID },
		},
		{
			name: "the box's span in the run's trace",
			// -01 against a run decided -00 below: the box's flag
			// does not override the run's decision.
			header: func(run events.TraceContext) string { return "00-" + run.TraceID + "-" + boxSpan + "-01" },
			want:   func(events.TraceContext) string { return boxSpan },
		},
		{
			name:   "a span in another trace",
			header: func(events.TraceContext) string { return "00-" + foreign.TraceID + "-" + boxSpan + "-01" },
			want:   func(run events.TraceContext) string { return run.SpanID },
		},
		{
			name:   "a malformed traceparent",
			header: func(run events.TraceContext) string { return "00-" + run.TraceID + "-zz-01" },
			want:   func(run events.TraceContext) string { return run.SpanID },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			run := events.NewTrace()
			f.session.Trace = run
			tracingtest.Watch(t, run.TraceID)
			tracingtest.Watch(t, foreign.TraceID)
			url := f.open(t)

			var sess *mcp.ClientSession
			if h := tc.header(run); h != "" {
				sess = dialSending(t, url, h)
			} else {
				sess = dial(t, url)
			}
			callTool(t, sess, "read_page", map[string]any{})

			span := tracingtest.Only(t, run.TraceID, "tool.call")
			if got := span.Parent().SpanID().String(); got != tc.want(run) {
				t.Errorf("the bridged tool.call's parent is %s, want %s", got, tc.want(run))
			}
			for _, s := range tracingtest.Named(foreign.TraceID, "tool.call") {
				t.Errorf("a bridged call was filed in a trace its run was not launched in: %s",
					s.SpanContext().SpanID())
			}
		})
	}

	// THE RUN'S DECISION STANDS: an unsampled run's call records no span
	// even when its box sends a sampled traceparent in the run's trace.
	t.Run("an unsampled run", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		run := events.NewTrace()
		run.Unsampled = true
		f.session.Trace = run
		tracingtest.Watch(t, run.TraceID)
		sess := dialSending(t, f.open(t), "00-"+run.TraceID+"-"+boxSpan+"-01")
		callTool(t, sess, "read_page", map[string]any{})
		if spans := tracingtest.Spans(run.TraceID); len(spans) != 0 {
			t.Errorf("an unsampled run's bridged call recorded %v", tracingtest.Names(run.TraceID))
		}
	})
}
