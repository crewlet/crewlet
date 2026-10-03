package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// auditedSurface is [actSurface] auditing into a log the case reads.
func auditedSurface(t *testing.T) (*operator.Server, *auditLog) {
	t.Helper()
	audit := &auditLog{}
	work := &recordingWork{}
	s := newSurface(t, operator.Options{
		Halves: fixed(operator.Halves{
			Work: builtin.WorkDeps{Reader: stubWorkReader{}, Writer: work.writer},
		}),
		Org:   company,
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

// A PERSON'S ASSISTANT, THEIR OWN PRESS AND THEIR SCRIPT ARE AUDITED ALIKE.
// Every transport calls the one dispatch, and the audit is taken there — so
// the record of a person cannot depend on which surface they used, and a read
// over any of them is not recorded at all. The transport is the one field
// that differs, and MCP names no request.
//
// THE RECORD NAMES THE PRINCIPAL AS EVERY HISTORY ROW DOES: the author, its
// kind and the credential, [iam.ActorFor]'s three halves — never a seat
// resolved beside them.
func TestEveryTransportAuditsAWriteAndNoneAuditsARead(t *testing.T) {
	t.Parallel()
	s, audit := auditedSurface(t)

	key := newKey()
	status, answer := act(t, guarded(s), "founder", tracker.CreateWorkItemTool,
		"application/json", createBody(), key)
	if status != http.StatusOK {
		t.Fatalf("the act answered %d", status)
	}
	scoped, _ := answer["op_id"].(string)

	sess := dialOperator(t, s, founder)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: tracker.CreateWorkItemTool, Arguments: createArgs(),
	}); err != nil {
		t.Fatalf("create over MCP: %v", err)
	}
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: tracker.ListWorkItemsTool, Arguments: map[string]any{},
	}); err != nil {
		t.Fatalf("list over MCP: %v", err)
	}
	asFounder := iam.WithPrincipal(t.Context(), founder)
	workKey := newKey()
	for tool, args := range map[string]map[string]any{
		tracker.CreateWorkItemTool: createArgs(),
		tracker.ListWorkItemsTool:  {},
	} {
		if _, err := s.Dispatch(asFounder, operator.Call{
			Transport: types.TransportWork, Key: workKey, Tool: tool, Args: args,
		}); err != nil {
			t.Fatalf("dispatch %s over the human write surface: %v", tool, err)
		}
	}

	recorded := audit.published()
	if len(recorded) != 3 {
		t.Fatalf("three writes and two reads left %d audit records, want one per "+
			"write", len(recorded))
	}
	byTransport := map[string]types.OperatorActed{}
	for _, ev := range recorded {
		if ev.Source != types.OperatorSource || ev.Actor() != "jane-founder" {
			t.Errorf("a record carries source %q and actor %q", ev.Source, ev.Actor())
		}
		p := payloadOf(t, ev)
		byTransport[p.Transport] = p
	}
	for _, transport := range []string{types.TransportAct, types.TransportMCP,
		types.TransportWork} {

		p, ok := byTransport[transport]
		if !ok {
			t.Errorf("no record of the write over %s", transport)
			continue
		}
		if p.Tool != tracker.CreateWorkItemTool || p.ActorName != "jane-founder" ||
			p.ActorKind != string(iam.ActorHuman) || p.OperatorID != "jane.founder" ||
			p.Outcome != types.AuditApplied || p.Position == "" {

			t.Errorf("the %s record is %+v", transport, p)
		}
	}
	if got := byTransport[types.TransportAct].RequestID; got != scoped || got == key {
		t.Errorf("the act record names request %q; the answer named %q — one "+
			"operation, the caller's key scoped by who sent it", got, scoped)
	}
	if got := byTransport[types.TransportWork].RequestID; got != workKey {
		t.Errorf("the human write surface's record names request %q, want %q", got, workKey)
	}
	if got := byTransport[types.TransportMCP].RequestID; got != "" {
		t.Errorf("the MCP record names request %q, which MCP never sent", got)
	}
}

// THE RECORD IS WRITTEN AFTER THE CALLER HAS GONE. A call interrupted by a
// closed tab may still have landed, and it is the one an audit is opened to
// find — so the publish must not inherit the request's cancellation.
func TestAnAuditRecordOutlivesItsRequest(t *testing.T) {
	t.Parallel()
	audit := &auditLog{}
	ctx, cancel := context.WithCancel(iam.WithPrincipal(t.Context(), founder))
	cancel()
	operator.Audit(ctx, audit, operator.Acted(ctx, types.TransportAct,
		tracker.CreateWorkItemTool, newKey(), types.AuditUnknown, "", ""))
	got := audit.published()
	if len(got) != 1 {
		t.Fatalf("a call whose caller hung up left %d audit records, want one", len(got))
	}
	if p := payloadOf(t, got[0]); p.ActorName != "jane-founder" || p.OperatorID != "jane.founder" {
		t.Errorf("the record names %+v, want the request's principal", p)
	}
}

// WHAT BECAME OF A CALL is read the way the transports answer it, so the
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
			result: tools.Result{Failed: true, Cause: crewletmcp.Classify(
				crewletmcp.RefusalNotFound, tracker.ErrNoTask)},
			outcome: types.AuditRefused, refusal: string(crewletmcp.RefusalNotFound)},
		{name: "a tool's write nobody can vouch for may have landed",
			result: tools.Result{Failed: true, Cause: &builtin.UnknownOutcome{
				OpID: newKey()}},
			outcome: types.AuditUnknown},
		{name: "a failure with no class is a failure",
			result: tools.Result{Failed: true}, outcome: types.AuditFailed},
		// A FAULT IS THE NODE BREAKING, not the tool turning the call down,
		// so it is the outcome that says so — and names no class, as the
		// unclassified failure beside it does not.
		{name: "a fault of the node is a failure",
			result: tools.Result{Failed: true, Cause: crewletmcp.Classify(
				crewletmcp.RefusalInternalError, errors.New("disk I/O error"))},
			outcome: types.AuditFailed},
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

// A GESTURE MADE WITHOUT A TOOL IS AUDITED AS A TOOL'S CALL WOULD BE: a
// writer's refusal by its class, a write that may have landed as `unknown`, and
// a write that answered as its own outcome and position.
func TestAToolLessGestureIsAuditedAsItsWriterAnswered(t *testing.T) {
	t.Parallel()
	at := statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 9}
	cases := []struct {
		name     string
		outcome  statelog.Outcome
		position statelog.Position
		err      error
		want     types.AuditOutcome
		refusal  string
		at       string
	}{
		{name: "a refusal the writer marked", err: tracker.ErrNoTask,
			want: types.AuditRefused, refusal: string(crewletmcp.RefusalNotFound)},
		{name: "a write that may have landed", err: crewletmcp.ErrOutcomeUnknown,
			want: types.AuditUnknown},
		{name: "a write that met a fault of the node",
			err:  errors.New("tracker: read the ledger: disk I/O error"),
			want: types.AuditFailed},
		{name: "a write the log refused", err: &statelog.Unavailable{
			Reason: statelog.ReasonBehind}, want: types.AuditRefused,
			refusal: string(crewletmcp.RefusalUnavailable)},
		{name: "a write that landed", outcome: statelog.OutcomeApplied, position: at,
			want: types.AuditApplied, at: at.String()},
		{name: "a write that changed nothing", want: types.AuditApplied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			audit := &auditLog{}
			ctx := iam.WithPrincipal(t.Context(), founder)
			operator.AuditGesture(ctx, audit, types.TransportWork, "purge_work_item",
				"k", tc.outcome, tc.position, tc.err)
			got := audit.published()
			if len(got) != 1 {
				t.Fatalf("one gesture left %d records", len(got))
			}
			p := payloadOf(t, got[0])
			if p.Outcome != tc.want || p.Refusal != tc.refusal || p.Position != tc.at {
				t.Errorf("audited as (%s, %q, %q), want (%s, %q, %q)", p.Outcome,
					p.Refusal, p.Position, tc.want, tc.refusal, tc.at)
			}
		})
	}
}

// renamingChart is a chart a config apply replaces on EVERY read: the seat
// the founder is bound to is renamed, and another seat takes its old handle,
// and round again — the worst case of an apply landing while one call runs.
func renamingChart() func() *org.Organization {
	var mu sync.Mutex
	reads := 0
	names := [][]string{{"Jane Founder", "Pat Successor"}, {"Pat Successor", "Jane Founder"}}
	return func() *org.Organization {
		mu.Lock()
		pick := names[reads%len(names)]
		reads++
		mu.Unlock()
		o := &org.Organization{Name: "Nimbus"}
		for _, name := range pick {
			o.Roles = append(o.Roles, &org.Role{Name: name, Kind: org.KindHuman})
		}
		o.Normalize()
		return o
	}
}

// ONE CALL IS MADE BY ONE PRINCIPAL, whatever an apply or a directory rebind
// does while it runs. The principal the guard resolved is a value on the
// request, and the dispatch, the tool's actor and the audit record all read
// that one value — so a chart re-read on every access cannot admit a call as
// one person, write it as another and audit it as a third, which is what
// resolving the seat at each frame did.
func TestACallIsMadeAndAuditedAsThePrincipalItWasAdmittedAs(t *testing.T) {
	t.Parallel()
	for _, transport := range []string{types.TransportAct, types.TransportMCP} {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			audit := &auditLog{}
			work := &recordingWork{}
			s := newSurface(t, operator.Options{
				Halves: fixed(operator.Halves{
					Work: builtin.WorkDeps{Reader: stubWorkReader{}, Writer: work.writer},
				}),
				Org:   renamingChart(),
				Audit: audit,
			})
			switch transport {
			case types.TransportAct:
				status, answer := act(t, guarded(s), "founder",
					tracker.CreateWorkItemTool, "application/json", createBody(), newKey())
				if status != http.StatusOK {
					t.Fatalf("the act answered %d %v", status, answer)
				}
			case types.TransportMCP:
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				if _, err := dialOperator(t, s, founder).CallTool(ctx, &mcp.CallToolParams{
					Name: tracker.CreateWorkItemTool, Arguments: createArgs(),
				}); err != nil {
					t.Fatalf("create over MCP: %v", err)
				}
			}
			actors, _ := work.writes()
			recorded := audit.published()
			if len(actors) != 1 || len(recorded) != 1 {
				t.Fatalf("one call wrote %d times and was audited %d times",
					len(actors), len(recorded))
			}
			written, audited := actors[0], payloadOf(t, recorded[0])
			if written.Handle != "jane-founder" || audited.ActorName != written.Handle ||
				audited.ActorKind != string(written.Kind) ||
				audited.OperatorID != written.OperatorID {

				t.Errorf("the call was admitted as jane-founder, wrote as %+v and "+
					"was audited as %+v", written, audited)
			}
		})
	}
}

// A FAILED PUBLISH IS LOGGED, NEVER ANSWERED: the call it describes already
// happened, and failing its answer would tell the caller a write that landed
// did not.
func TestAnAuditThatCannotPublishDoesNotFailTheCall(t *testing.T) {
	t.Parallel()
	audit := &auditLog{err: errors.New("the broker is unreachable")}
	work := &recordingWork{}
	s := newSurface(t, operator.Options{
		Halves: fixed(operator.Halves{
			Work: builtin.WorkDeps{Reader: stubWorkReader{}, Writer: work.writer},
		}),
		Org:   company,
		Audit: audit,
	})
	status, answer := act(t, guarded(s), "founder", tracker.CreateWorkItemTool,
		"application/json", createBody(), newKey())
	if status != http.StatusOK {
		t.Fatalf("a write that landed answered %d %v because its audit record "+
			"could not be published", status, answer)
	}
	if actors, _ := work.writes(); len(actors) != 1 {
		t.Errorf("the write was made %d times", len(actors))
	}
}
