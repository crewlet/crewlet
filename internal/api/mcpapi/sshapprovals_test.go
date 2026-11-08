package mcpapi_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/mcpapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// operateRun is swe's run, parked on an SSH operate command.
func operateRun() sandbox.PendingRun {
	run := waitingRun()
	run.HeldCall.Server, run.HeldCall.Tool, run.HeldCall.Raw = config.ToolServerName, "ssh_operate", "ssh_operate"
	run.HeldCall.Fixed, run.HeldCall.Approvers = true, sandbox.HeldApproversSSH
	return run
}

// operateSurface is the approval surface over a company naming ops-lead as an
// SSH approver, in a chart where cto leads swe.
func operateSurface(t *testing.T, desk *fakeApprovals) *mcpapi.Service {
	t.Helper()
	company := &config.Company{Integrations: config.Integrations{SSH: &config.SSH{Approvers: []string{"ops-lead"}}}}
	svc, err := mcpapi.New(mcpapi.Options{Company: func() *config.Company { return company },
		Registry: &fakeRegistry{}, Detail: listedDetail(), Restarter: &fakeRestarter{}, Checker: &fakeChecker{},
		Tools: &fakeToolDocuments{}, Values: valueStore(t), SignIns: &fakeSignIns{}, Audit: &recordedAudit{},
		Approvals: desk, Guard: authz.ContextGuard(leadOf{lead: "cto", seat: "swe"})})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// ONLY AN ADMINISTRATOR OR A NAMED APPROVER ANSWERS AN OPERATE CALL: the seat's
// lead, who answers its other held calls, is refused, and so are a colleague,
// the agent seat and the assistant, before anything is delivered.
//
// Mutation: decide an operate call as tools.approve, and the lead is admitted.
func TestOnlyAnAdministratorOrANamedApproverAnswersAnOperateCall(t *testing.T) {
	t.Parallel()
	assistant := proved(time.Minute)
	assistant.Via, assistant.AssistScope = iam.AssistName(uuid.Must(uuid.NewV7())), iam.AssistScopeExec
	for _, c := range []struct {
		name string
		as   iam.Principal
		code int
	}{
		{"an administrator", proved(time.Minute), http.StatusAccepted},
		{"the named approver", seated("ops-lead", time.Minute), http.StatusAccepted},
		{"the seat's lead", seated("cto", time.Minute), http.StatusForbidden},
		{"a colleague", seated("ana", time.Minute), http.StatusForbidden},
		{"the assistant", assistant, http.StatusForbidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			desk := &fakeApprovals{run: operateRun()}
			code, body := answer(t, operateSurface(t, desk), c.as, "/mcp-approvals/"+heldID, `{"answer": "once"}`)
			if code != c.code {
				t.Fatalf("POST = %d %v, want %d", code, body, c.code)
			}
			if delivered := len(desk.delivered) == 1; delivered != (c.code == http.StatusAccepted) {
				t.Errorf("delivered %+v", desk.delivered)
			}
		})
	}
}

// APPROVING AN OPERATE CALL NEEDS A STEP-UP: an administrator or the named
// approver whose proof is older than the step-up window is refused step-up,
// and nothing is delivered.
func TestApprovingAnOperateCallNeedsAStepUp(t *testing.T) {
	t.Parallel()
	for name, as := range map[string]iam.Principal{
		"an administrator at a stale proof":   proved(3 * time.Hour),
		"the named approver at a stale proof": seated("ops-lead", 3*time.Hour),
	} {
		desk := &fakeApprovals{run: operateRun()}
		code, body := answer(t, operateSurface(t, desk), as, "/mcp-approvals/"+heldID, `{"answer": "once"}`)
		if code != http.StatusForbidden || body["error"] != "step_up_required" || len(desk.delivered) != 0 {
			t.Errorf("%s: POST = %d %v, delivered %d", name, code, body, len(desk.delivered))
		}
	}
}

// AN OPERATE CALL OFFERS NO ALWAYS: its decision is Allow once and Deny alone,
// a held call naming SSH approvers offering always is refused on the row, and
// the route refuses always for it before deciding who answers.
func TestAnOperateCallOffersNoAlways(t *testing.T) {
	t.Parallel()
	if ids := sandbox.HeldFixedDecision("May sre run it?", nil).OptionIDs(); len(ids) != 2 ||
		ids[0] != sandbox.HeldOnce || ids[1] != sandbox.HeldDeny {
		t.Errorf("an operate call offers %v", ids)
	}
	desk := &fakeApprovals{run: operateRun()}
	code, body := answer(t, operateSurface(t, desk), proved(time.Minute), "/mcp-approvals/"+heldID, `{"answer": "always"}`)
	if code != http.StatusBadRequest || body["error"] != "invalid_body" || len(desk.delivered) != 0 {
		t.Errorf("always on an operate call = %d %v", code, body)
	}
}
