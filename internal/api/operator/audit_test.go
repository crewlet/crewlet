package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// auditedSurface is [actSurface] auditing into a log the case reads.
func auditedSurface(t *testing.T) (*operator.Server, *auditLog) {
	t.Helper()
	audit := &auditLog{}
	work := &recordingWork{}
	s := newSurface(t, operator.Options{
		Work: builtin.WorkDeps{
			Reader: stubWorkReader{}, Writer: work.writer,
			Actor: operator.WorkActor(boundChart),
		},
		Org:   boundChart,
		Audit: audit,
	})
	return s, audit
}

// payloadOf decodes one audit record's body.
func payloadOf(t *testing.T, ev *events.Event) types.OperatorActed {
	t.Helper()
	acted, ok := events.DataAs[*types.OperatorActed](ev)
	if !ok {
		t.Fatalf("the record is a %s carrying %T, want an operator_acted", ev.Type, ev.Data)
	}
	return *acted
}

// A PERSON'S ASSISTANT IS AUDITED EXACTLY AS THEIR OWN PRESS IS. Both
// transports call the one dispatch, and the audit is taken there — so the
// record of a person cannot depend on which surface they used, and a read over
// either is not recorded at all. The transport is the one field that differs,
// and MCP names no request.
func TestBothTransportsAuditAWriteAndNeitherAuditsARead(t *testing.T) {
	t.Parallel()
	s, audit := auditedSurface(t)

	status, _ := act(t, guarded(s, false), "founder-secret", tracker.CreateWorkItemTool,
		"application/json", createBody(requestA))
	if status != http.StatusOK {
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
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: tracker.ListWorkItemsTool, Arguments: map[string]any{},
	}); err != nil {
		t.Fatalf("list over MCP: %v", err)
	}

	recorded := audit.published()
	if len(recorded) != 2 {
		t.Fatalf("two writes and a read left %d audit records, want one per write", len(recorded))
	}
	byTransport := map[string]types.OperatorActed{}
	for _, ev := range recorded {
		if ev.Source != types.OperatorSource || ev.Actor() != "founder" {
			t.Errorf("a record carries source %q and actor %q", ev.Source, ev.Actor())
		}
		p := payloadOf(t, ev)
		byTransport[p.Transport] = p
	}
	for _, transport := range []string{types.TransportAct, types.TransportMCP} {
		p, ok := byTransport[transport]
		if !ok {
			t.Errorf("no record of the write over %s", transport)
			continue
		}
		if p.Tool != tracker.CreateWorkItemTool || p.ActorSeat != "jane-founder" ||
			p.Outcome != types.AuditApplied || p.Position == "" {

			t.Errorf("the %s record is %+v", transport, p)
		}
	}
	if got := byTransport[types.TransportAct].RequestID; got != requestA {
		t.Errorf("the act record names request %q, want the request's own id", got)
	}
	if got := byTransport[types.TransportMCP].RequestID; got != "" {
		t.Errorf("the MCP record names request %q, which MCP never sent", got)
	}
}

// A SURFACE THAT WOULD WRITE AND RECORD NOTHING IS NOT BUILT. The audit is
// required exactly where there is something to audit: a surface with nothing
// to serve is nil and fine without one.
func TestASurfaceWithToolsAndNoAuditIsRefused(t *testing.T) {
	t.Parallel()
	s, err := operator.New(operator.Options{
		Work: builtin.WorkDeps{Reader: stubWorkReader{}, Actor: operator.WorkActor(nil)},
	})
	if !errors.Is(err, operator.ErrNoAudit) || s != nil {
		t.Fatalf("a surface with tools and no audit answered %v, %v; want ErrNoAudit", s, err)
	}
}

// THE RECORD IS WRITTEN AFTER THE CALLER HAS GONE. A call interrupted by a
// closed tab may still have landed, and it is the one an audit is opened to
// find — so the publish must not inherit the request's cancellation.
func TestAnAuditRecordOutlivesItsRequest(t *testing.T) {
	t.Parallel()
	audit := &auditLog{}
	ctx, cancel := context.WithCancel(auth.WithOperator(t.Context(), "founder"))
	cancel()
	operator.Audit(ctx, audit, types.NewOperatorActed(types.OperatorActed{
		OperatorID: "founder", Tool: tracker.CreateWorkItemTool, Outcome: types.AuditUnknown,
	}))
	if got := audit.published(); len(got) != 1 {
		t.Fatalf("a call whose caller hung up left %d audit records, want one", len(got))
	}
}

// WHAT BECAME OF A CALL is read the way the act transport answers it, so the
// record and the caller's answer cannot disagree.
func TestTheAuditReadsACallAsItsAnswerDoes(t *testing.T) {
	t.Parallel()
	answer := func(v map[string]any) string {
		raw, _ := json.Marshal(v)
		return string(raw)
	}
	cases := []struct {
		name     string
		result   tools.Result
		err      error
		outcome  types.AuditOutcome
		position string
		refusal  string
	}{
		{name: "an interrupted call may have landed",
			err: context.Canceled, outcome: types.AuditUnknown},
		{name: "a classified refusal names its class",
			result:  tools.Result{Failed: true, Refusal: crewletmcp.RefusalNotFound},
			outcome: types.AuditRefused, refusal: string(crewletmcp.RefusalNotFound)},
		{name: "a failure with no class is a failure",
			result: tools.Result{Failed: true}, outcome: types.AuditFailed},
		{name: "a stated outcome and position are the tool's own",
			result:  tools.Result{Output: answer(map[string]any{"outcome": "pending", "position": "L@1:9"})},
			outcome: types.AuditPending, position: "L@1:9"},
		{name: "an outcome this build does not know is unknown",
			result:  tools.Result{Output: answer(map[string]any{"outcome": "maybe"})},
			outcome: types.AuditUnknown},
		{name: "a write that appended nothing applied",
			result: tools.Result{Output: "{}"}, outcome: types.AuditApplied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			outcome, position, refusal := operator.AuditOutcomeOf(
				tracker.CreateWorkItemTool, tc.result, tc.err)
			if outcome != tc.outcome || position != tc.position || refusal != tc.refusal {
				t.Errorf("read as (%s, %q, %q), want (%s, %q, %q)", outcome, position,
					refusal, tc.outcome, tc.position, tc.refusal)
			}
			if !outcome.Valid() {
				t.Errorf("%q is not an audit outcome", outcome)
			}
		})
	}
}
