package mcpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/mcpapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// heldID is the held call the cases answer.
const heldID = "0190f3a8-0000-7000-8000-000000000001"

// fakeApprovals is the answer desk over one run holding one call.
type fakeApprovals struct {
	run sandbox.PendingRun

	mu        sync.Mutex
	delivered []types.ExecutorAnswerGiven
}

func (f *fakeApprovals) Held(_ context.Context, id string) (sandbox.PendingRun, bool, error) {
	if f.run.HeldCall == nil || f.run.HeldCall.ID != id {
		return sandbox.PendingRun{}, false, nil
	}
	return f.run, true, nil
}

func (f *fakeApprovals) Deliver(_ context.Context, given types.ExecutorAnswerGiven) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, given)
	return uuid.Must(uuid.NewV7()), nil
}

// waitingRun is swe's run, parked on a call to tracker's close_issue.
func waitingRun() sandbox.PendingRun {
	return sandbox.PendingRun{TurnID: "t1", LaunchID: "launch-1", AgentHandle: "swe",
		AgentID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("swe")).String(), Status: sandbox.StatusAwaiting,
		HeldCall: &sandbox.HeldCall{ID: heldID, LaunchID: "launch-1", Server: "tracker",
			Tool: "tr_close_issue", Raw: "close_issue", Digest: "sha256:d1", HeldAt: time.Now().UTC()}}
}

// leadOf is a chart in which lead leads seat and nobody else.
type leadOf struct{ lead, seat string }

func (c leadOf) Leads(_ context.Context, actor, subject string) (bool, error) {
	return actor == c.lead && subject == c.seat, nil
}
func (leadOf) LeadsProject(context.Context, string, string) (bool, error)   { return false, nil }
func (leadOf) LeadsUnit(context.Context, string, string) (bool, error)      { return false, nil }
func (leadOf) LeadsContainer(context.Context, string, string) (bool, error) { return false, nil }
func (leadOf) MemberOf(context.Context, string, string) (bool, error)       { return false, nil }
func (c leadOf) LeadsAnyone(_ context.Context, actor string) (bool, error) {
	return actor == c.lead, nil
}

// approvalSurface is the surface over desk and docs, decided over the chart in
// which "cto" leads "swe".
func approvalSurface(t *testing.T, desk *fakeApprovals, docs *fakeToolDocuments, audit *recordedAudit) *mcpapi.Service {
	t.Helper()
	opts := mcpapi.Options{Company: func() *config.Company { return nil },
		Registry: &fakeRegistry{}, Detail: listedDetail(), Restarter: &fakeRestarter{}, Checker: &fakeChecker{},
		Tools: docs, Values: valueStore(t), SignIns: &fakeSignIns{}, Audit: audit,
		Guard: authz.ContextGuard(leadOf{lead: "cto", seat: "swe"})}
	if desk != nil {
		opts.Approvals = desk
	}
	svc, err := mcpapi.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// answer answers the held call as.
func answer(t *testing.T, svc *mcpapi.Service, as iam.Principal, path, body string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(iam.WithPrincipal(t.Context(), as), http.MethodPost, path,
		strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w.Code, decoded(t, w.Body.String())
}

// seated is a person bound to seat, holding grants, whose proof is d old.
func seated(seat string, d time.Duration, grants ...iam.Grant) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: seat + ".person", Kind: iam.KindPerson, Seat: seat,
		Stage: iam.StageActive, Grants: grants, ReauthAt: time.Now().Add(-d).Add(time.Hour)}
}

// A HELD CALL IS ANSWERED BY ITS SEAT'S LEAD OR AN ADMINISTRATOR, a person
// alone. Once and deny are delivered to the seat holding the run, naming the
// question and the held call, and audited by the tool, never the arguments;
// always writes the tool's level to allow by the server's own name for it, as
// a configuration write at a recent step-up, before it answers.
//
// Mutation: deliver without the held call's id, or answer always without
// writing the level, and this is red.
func TestAHeldCallIsAnsweredByItsSeatsLeadOrAnAdministrator(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, answer string
		as           iam.Principal
		writes       bool
	}{
		{"the seat's lead, once", sandbox.HeldOnce, seated("cto", 3*time.Hour), false},
		{"an administrator, deny", sandbox.HeldDeny, proved(3 * time.Hour), false},
		{"an administrator at a recent step-up, always", sandbox.HeldAlways, proved(time.Minute), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			desk, docs, audit := &fakeApprovals{run: waitingRun()}, &fakeToolDocuments{}, &recordedAudit{}
			svc := approvalSurface(t, desk, docs, audit)
			code, body := answer(t, svc, c.as, "/mcp-approvals/"+heldID, `{"answer": "`+c.answer+`"}`)
			if code != http.StatusAccepted || body["outcome"] != "pending" || body["answer"] != c.answer ||
				body["tool"] != "tr_close_issue" || body["seat"] != "swe" {
				t.Fatalf("POST = %d %v", code, body)
			}
			if got, want := keysOf(body), []string{"answer", "answer_id", "id", "launch_id", "outcome",
				"revision", "seat", "server", "tool", "turn_id"}; strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("the answer's keys are %v, want exactly %v", got, want)
			}
			if len(desk.delivered) != 1 || desk.delivered[0].Approval != heldID ||
				desk.delivered[0].LaunchID != "launch-1" || desk.delivered[0].Choice != c.answer {
				t.Fatalf("delivered %+v, want the answer naming the held call and its question", desk.delivered)
			}
			if c.writes {
				write := docs.last(t)
				if write.Server != "tracker" || len(write.Change.Levels) != 1 ||
					*write.Change.Levels["close_issue"] != config.MCPToolAllow || write.Summary == "" {
					t.Fatalf("always wrote %+v, want close_issue set to allow", write)
				}
			} else if len(docs.writes) != 0 {
				t.Fatalf("%s wrote %+v", c.answer, docs.writes)
			}
			row := audit.last(t)
			if row.Action != types.MCPServerApprove || row.Server != "tracker" || row.Tool != "tr_close_issue" ||
				row.Answer != c.answer || row.Approval != heldID || row.Seat != "swe" || row.Outcome != types.AuditApplied {
				t.Errorf("audited %+v", row)
			}
		})
	}
}

// A HELD CALL IS ANSWERED BY NOBODY ELSE, ONCE: a person who neither leads the
// seat nor administers is refused before anything is delivered, and so are an
// agent seat and the assistant; always at a stale proof is refused step-up and
// writes nothing; a second answer, once the run waits on the call no more, is
// refused stale_version; an id no run holds is 404; a node with no desk is 503.
//
// Mutation: decide the route on the caller's own record alone, and the
// colleague is admitted.
func TestAHeldCallIsAnsweredByNobodyElseAndOnce(t *testing.T) {
	t.Parallel()
	answered := waitingRun()
	answered.Status = sandbox.StatusAnswered
	moved := waitingRun()
	moved.LaunchID = "launch-2"
	assistant := proved(time.Minute)
	assistant.Via, assistant.AssistScope = iam.AssistName(uuid.Must(uuid.NewV7())), iam.AssistScopeExec
	agent := iam.Principal{ID: uuid.New(), Login: "swe", Kind: iam.KindSeat, Seat: "swe",
		Stage: iam.StageActive, Grants: iam.AllGrants, ReauthAt: time.Now().Add(time.Hour)}
	for _, c := range []struct {
		name, path, answer string
		as                 iam.Principal
		desk               *fakeApprovals
		code               int
		error              string
	}{
		{"a colleague", "/mcp-approvals/" + heldID, sandbox.HeldOnce, seated("ana", time.Minute),
			&fakeApprovals{run: waitingRun()}, http.StatusForbidden, "unauthorized"},
		{"the agent seat itself", "/mcp-approvals/" + heldID, sandbox.HeldOnce, agent,
			&fakeApprovals{run: waitingRun()}, http.StatusForbidden, "unauthorized"},
		{"the assistant", "/mcp-approvals/" + heldID, sandbox.HeldOnce, assistant,
			&fakeApprovals{run: waitingRun()}, http.StatusForbidden, ""},
		{"always at a stale proof", "/mcp-approvals/" + heldID, sandbox.HeldAlways, proved(3 * time.Hour),
			&fakeApprovals{run: waitingRun()}, http.StatusForbidden, "step_up_required"},
		{"a second answer", "/mcp-approvals/" + heldID, sandbox.HeldOnce, proved(time.Minute),
			&fakeApprovals{run: answered}, http.StatusConflict, "stale_version"},
		{"a run moved on to another question", "/mcp-approvals/" + heldID, sandbox.HeldDeny,
			proved(time.Minute), &fakeApprovals{run: moved}, http.StatusConflict, "stale_version"},
		{"an id no run holds", "/mcp-approvals/" + uuid.Must(uuid.NewV7()).String(), sandbox.HeldOnce,
			proved(time.Minute), &fakeApprovals{run: waitingRun()}, http.StatusNotFound, "not_found"},
		{"an answer no held call offers", "/mcp-approvals/" + heldID, "maybe", proved(time.Minute),
			&fakeApprovals{run: waitingRun()}, http.StatusBadRequest, "invalid_body"},
		{"a node with no desk", "/mcp-approvals/" + heldID, sandbox.HeldOnce, proved(time.Minute),
			nil, http.StatusServiceUnavailable, "unavailable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			docs, audit := &fakeToolDocuments{}, &recordedAudit{}
			svc := approvalSurface(t, c.desk, docs, audit)
			code, body := answer(t, svc, c.as, c.path, `{"answer": "`+c.answer+`"}`)
			if code != c.code || (c.error != "" && body["error"] != c.error) {
				t.Fatalf("POST = %d %v, want %d %s", code, body, c.code, c.error)
			}
			if c.desk != nil && len(c.desk.delivered) != 0 {
				t.Errorf("a refused answer was delivered: %+v", c.desk.delivered)
			}
			if len(docs.writes) != 0 {
				t.Errorf("a refused answer wrote %+v", docs.writes)
			}
			if row := audit.last(t); row.Action != types.MCPServerApprove || row.Outcome == types.AuditApplied {
				t.Errorf("the refusal audited %+v", row)
			}
		})
	}
}
