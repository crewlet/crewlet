package operator_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/tracing/tracingtest"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY OPERATOR CALL IS ONE TRACE, under a `tool.call` span of its own, on
// both transports. The tool runs inside the span — so a model call it makes is
// an `llm.auxiliary` span beneath it, with its provider's spans under that —
// and the call's audit record is filed under it. Without it a person's
// question rooted one trace for its model call, another for the call's spend
// and a third for its audit record.
//
// Mutations, each red here: open no span (every half); run the tool on the
// request's context (the tool half); audit on the request's context (the
// record half); drop the transport attribute.
func TestEveryOperatorCallIsOneTraceUnderItsOwnToolCall(t *testing.T) {
	t.Parallel()
	tracingtest.WatchNamed("tool.call")
	audit := &auditLog{}
	work := &recordingWork{}
	s := newSurface(t, operator.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: work.writer,
			Actor: operator.WorkActor(boundChart, nil),
		},
		Org:   boundChart,
		Audit: audit,
	})

	if status, _ := act(t, guarded(s, false), "founder-secret", tracker.CreateWorkItemTool,
		"application/json", createBody(requestA)); status != http.StatusOK {
		t.Fatalf("the act answered %d", status)
	}
	sess := dialOperator(t, s, "founder")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      tracker.CreateWorkItemTool,
		Arguments: map[string]any{"title": "Rotate the signing key", "project": "ENG"},
	}); err != nil {
		t.Fatalf("create over MCP: %v", err)
	}

	recorded := audit.published()
	if len(recorded) != 2 {
		t.Fatalf("two writes left %d audit records", len(recorded))
	}
	work.mu.Lock()
	under := append(work.under[:0:0], work.under...)
	work.mu.Unlock()
	if len(under) != 2 {
		t.Fatalf("the tool ran %d times, want twice", len(under))
	}
	for i, ev := range recorded {
		rec := ev.Trace()
		span := tracingtest.Only(t, rec.TraceID, "tool.call")
		transport := payloadOf(t, ev).Transport
		if span.Parent().IsValid() {
			t.Errorf("the %s call's tool.call has parent %s, want a root", transport, span.Parent().SpanID())
		}
		if rec.SpanID != span.SpanContext().SpanID().String() || rec.ParentSpanID != "" {
			t.Errorf("the %s audit record is filed under %+v, want the call's span %s",
				transport, rec, span.SpanContext().SpanID())
		}
		if under[i].SpanID() != span.SpanContext().SpanID() {
			t.Errorf("the %s call's tool ran under span %s, want its tool.call %s",
				transport, under[i].SpanID(), span.SpanContext().SpanID())
		}
		for key, want := range map[string]string{
			"crewlet.tool":         tracker.CreateWorkItemTool,
			"crewlet.transport":    transport,
			"crewlet.tool_failed":  "false",
			"crewlet.tool_outcome": "ok",
		} {
			if got := tracingtest.Attr(span, key); got != want {
				t.Errorf("the %s call's %s = %q, want %q", transport, key, got, want)
			}
		}
	}
	if a, b := recorded[0].Trace().TraceID, recorded[1].Trace().TraceID; a == b {
		t.Errorf("two calls share trace %s; each is a root of its own", a)
	}
}
