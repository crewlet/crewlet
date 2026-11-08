package mcpapi

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// `POST /mcp-approvals/{id}` answers a call an agent's run made to a tool at
// Needs approval, held parked on the run's row (ADR-0077):
// `{"answer": "once"|"always"|"deny"}`, with an optional `_summary` for the
// revision `always` writes.
//
// WHO MAY ANSWER is decided twice. The route admits on the weakest honest
// precondition, `tools.approve` on the caller's own record, which keeps out an
// agent seat and the assistant (the row is a person's alone) and nothing else;
// the handler then reads the held call, learns whose run made it, and decides
// `tools.approve` on that seat: an administrator, or a lead of the seat's unit.
// Decided on the route alone, a lead would be undecidable there, since the
// seat is only known once the call is read.
//
// ALWAYS ALLOW IS A CONFIGURATION WRITE, decided as one (`config.write` at its
// recent step-up) and made before the answer is sent: the tool's level is set
// to allow by the server's own name for it, as `PUT /mcp-servers/{name}/tools`
// sets it, so an answer whose write is refused answers nothing; and the
// definition the call was made under is accepted, where the server changed
// the tool and lists that definition still (ADR-0078). Once and deny write
// neither the company nor a credential.
//
// ONE ANSWER PER CALL: a call no longer waiting (answered, or its run moved on
// to another question) is refused `409 stale_version`, and the node holding the
// seat records the first answer it takes and spends any other, so a held call
// is never made twice whatever reaches it.

// Approvals is where a held call is found and its answer delivered
// ([sandbox.AnswerDesk]).
type Approvals interface {
	// Held is the run whose row holds held call id, false where no active
	// run's does.
	Held(ctx context.Context, id string) (sandbox.PendingRun, bool, error)
	// Deliver puts the answer on the control topic of the seat holding the
	// run, answering the delivery's id.
	Deliver(ctx context.Context, given types.ExecutorAnswerGiven) (uuid.UUID, error)
}

// approvalRequest is the body the route reads.
type approvalRequest struct {
	Answer string `json:"answer"`
	// Summary is the revision's audit summary for `always`, as every
	// configuration write takes it; a summary naming the tool is written
	// where none is sent.
	Summary string `json:"_summary"`
}

// approvalShape is the body, as a refusal names it.
const approvalShape = `{"answer": "once"|"always"|"deny"}`

// maxApprovalBody bounds the body: an answer and a revision summary, which
// configuration writes hold to a line.
const maxApprovalBody = 8 << 10

// ApprovalAnswer is what the route answers: the call it answered, the answer
// and what the delivery says of it, and for `always` the configuration's own
// answer to the level written (`revision_id`, `epoch`, `warnings`).
type ApprovalAnswer struct {
	ID     string `json:"id"`
	Server string `json:"server"`
	Tool   string `json:"tool"`
	Seat   string `json:"seat"`
	// TurnID and LaunchID are the run and the question the answer is
	// pinned to, which the run's record is read against.
	TurnID   string `json:"turn_id"`
	LaunchID string `json:"launch_id"`
	Answer   string `json:"answer"`
	// Outcome is `pending` (the answer is on its way to the node holding
	// the seat, which makes or refuses the call and carries the run on) or
	// `unknown` (the delivery may or may not have landed: answering again
	// is safe, since the seat records one answer).
	Outcome statelog.Outcome `json:"outcome"`
	// AnswerID is the delivery's id, "" where none was minted.
	AnswerID string `json:"answer_id"`
	// Revision is the configuration's answer to the level `always` wrote,
	// null for `once` and `deny`.
	Revision map[string]any `json:"revision"`
}

// serveApproval answers the held call its path names.
func (s *Service) serveApproval(w http.ResponseWriter, r *http.Request, rec *types.MCPServerActed) {
	ctx := r.Context()
	id := r.PathValue("id")
	rec.Approval = id
	if _, err := uuid.Parse(id); err != nil {
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNotFound,
			httpjson.Detail{"detail": "no call is held under " + quoted(id) + ": a held call's id " +
				"is the one the Inbox or GET /mcp-servers answers"})
		return
	}
	if s.approvals == nil {
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, detailRetrySeconds,
			httpjson.Detail{"detail": "this node serves no run record to answer a held call through; " +
				"ask through a node that serves the dashboard"})
		return
	}
	var req approvalRequest
	if !decodeBodyWithin(w, r, &req, approvalShape, maxApprovalBody) {
		return
	}
	if !slices.Contains(sandbox.HeldChoices, req.Answer) {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": "answer is " + quoted(req.Answer) + ", and a held call is " +
				"answered once, always or deny, as " + approvalShape})
		return
	}
	rec.Answer = req.Answer
	run, found, err := s.approvals.Held(ctx, id)
	switch {
	case err != nil:
		log.WarnContext(ctx, "mcp_approval_unread", "approval", id, "error", err.Error())
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, detailRetrySeconds,
			httpjson.Detail{"detail": "the runs could not be read, so nothing was answered; try again"})
		return
	case !found:
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNotFound,
			httpjson.Detail{"detail": "no call is held under " + quoted(id) + ": its run has ended, " +
				"or the id names none"})
		return
	}
	held := *run.HeldCall
	rec.Server, rec.Tool, rec.Seat = held.Server, held.Tool, run.AgentHandle
	if !slices.Contains(sandbox.ChoicesOf(held), req.Answer) {
		// A TOOL THAT ASKS BEFORE EVERY CALL has no level Always allow
		// could set, so it is answered once or deny alone.
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": "the call to " + held.Tool + " asks before every call, so it is " +
				"answered " + strings.Join(sandbox.ChoicesOf(held), " or ") + ", never " + quoted(req.Answer)})
		return
	}
	if !authz.Admit(w, r, s.guard, s.approverPolicy(run)) {
		return
	}
	if !slices.Contains(sandbox.Awaiting, run.Status) || run.LaunchID != held.LaunchID {
		// NOT WAITING ON THIS CALL ANY MORE: answered, or its run carried on
		// to another question. Nothing was answered.
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeStaleVersion,
			httpjson.Detail{"detail": "the call to " + held.Tool + " is not waiting for an answer " +
				"any more: it was answered, or its run has moved on. Nothing was answered."})
		return
	}
	answer := ApprovalAnswer{ID: id, Server: held.Server, Tool: held.Tool, Seat: run.AgentHandle,
		TurnID: run.TurnID, LaunchID: run.LaunchID, Answer: req.Answer}
	if req.Answer == sandbox.HeldAlways {
		revision, ok := s.allowAlways(w, r, run, req.Summary)
		if !ok {
			return
		}
		answer.Revision = revision
	}
	by := auth.AttributionOf(ctx)
	given := types.ExecutorAnswerGiven{
		TurnID: run.TurnID, Agent: run.AgentID, AgentHandle: run.AgentHandle,
		Choice: req.Answer, AnsweredBy: by.Name, AnsweredByKind: string(by.Kind),
		OperatorID: by.OperatorID,
		// THE QUESTION AND THE CALL THIS ANSWERS, off the row just read as
		// waiting on it: the seat records it only while the run still
		// waits on that call.
		LaunchID: run.LaunchID, Approval: id,
	}
	answer.Outcome = statelog.OutcomePending
	delivered, err := s.approvals.Deliver(ctx, given)
	if err != nil {
		// THE PUBLISH MAY HAVE LANDED: said as unknown, and safe to answer
		// again ([sandbox.AnswerDesk.Deliver]).
		log.WarnContext(ctx, "mcp_approval_delivery_unknown", "approval", id, "turn_id", run.TurnID,
			"seat", run.AgentHandle, "error", err.Error())
		answer.Outcome = statelog.OutcomeUnknown
	}
	if delivered != uuid.Nil {
		answer.AnswerID = delivered.String()
	}
	httpjson.Write(w, http.StatusAccepted, answer)
}

// approverPolicy is who may answer run's held call, decided now that the call
// is read. An SSH operate command is an administrator's or a person's the
// company names in integrations.ssh.approvers, at the step-up window; every
// other call is the seat's lead's or an administrator's, on the seat's own
// record, as answering its run's question is.
func (s *Service) approverPolicy(run sandbox.PendingRun) authz.Policy {
	if run.HeldCall.Approvers == sandbox.HeldApproversSSH {
		var named []string
		if company := s.company(); company != nil && company.Integrations.SSH != nil {
			named = company.Integrations.SSH.Approvers
		}
		return authz.Policy{Action: authz.ActionSSHOperateApprove,
			Object: func(*http.Request) authz.Object {
				return authz.Object{Kind: authz.KindPerson, ID: run.AgentID, Named: named}
			}}
	}
	return authz.Policy{Action: authz.ActionToolsApprove,
		Object: func(*http.Request) authz.Object {
			return authz.Object{Kind: authz.KindPerson, ID: run.AgentID, Owner: run.AgentHandle}
		}}
}

// allowAlways writes the held call's tool to allow on its server's entry, by
// the server's own name for it, as a configuration write: decided as
// `config.write` at its step-up, then written as `PUT /mcp-servers/{name}/tools`
// writes, and answered as that route answers a refusal. It reports the
// configuration's answer, or false where it answered a refusal itself.
func (s *Service) allowAlways(w http.ResponseWriter, r *http.Request, run sandbox.PendingRun,
	summary string) (map[string]any, bool) {

	held := *run.HeldCall
	if !authz.Admit(w, r, s.guard, authz.Policy{Action: authz.ActionConfigWrite}) {
		return nil, false
	}
	if s.tools == nil {
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, detailRetrySeconds,
			httpjson.Detail{"detail": "this node serves no configuration surface to set the tool's " +
				"level through; answer once, or ask through a node that serves the dashboard"})
		return nil, false
	}
	ctx := r.Context()
	by := auth.AttributionOf(ctx)
	var managed *configapi.ManagedError
	if err := s.tools.Authorize(by); errors.As(err, &managed) {
		configapi.RefuseManaged(w, managed)
		return nil, false
	}
	if summary = strings.TrimSpace(summary); summary == "" {
		summary = fmt.Sprintf("Always allow %s on MCP server %s, answering a call %s held for approval",
			held.Raw, held.Server, run.AgentHandle)
	}
	allow := config.MCPToolAllow
	write := configapi.MCPToolsWrite{
		Server: held.Server, Summary: summary, By: by,
		Change: configapi.MCPToolsChange{Levels: map[string]*config.MCPToolLevel{held.Raw: &allow}},
	}
	if principal, how := iam.From(ctx); how == iam.Resolved {
		write.Principal = &principal
	}
	written, err := s.tools.SetMCPTools(ctx, write)
	if err != nil {
		s.tools.RefuseMCPTools(w, r, held.Server, err)
		return nil, false
	}
	// THE DEFINITION THE CALL WAS MADE UNDER IS ACCEPTED WITH IT: the
	// level was decided on it (ADR-0078), so a tool held only because it
	// changed does not ask again.
	s.acceptHeld(ctx, held, by.Name)
	return maps.Clone(written.Answer), true
}
