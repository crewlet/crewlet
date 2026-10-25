package assistantapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE DECISION ROUTES: approving and declining a proposal, a page's claim on
// and report of a page action, and an administrator's audited read. Each is a
// thin adapter over internal/assistant/proposal/approval (or, for the read,
// the domain's audited reader), so what a decision does is decided once and
// every route answers it in the engine's envelope.

// The routes, as the API mounts them.
const (
	ApprovePattern   = "POST /assistant/proposals/{id}/approve"
	DeclinePattern   = "POST /assistant/proposals/{id}/decline"
	ActionPattern    = "PUT /assistant/actions/{id}"
	AuditReadPattern = "POST /assistant/audit/conversations/{id}/read"
)

// DecisionBodyMax bounds a decision's body: a version, a digest, a short
// report or an audit reason, each a few hundred bytes, with room for a client
// that pretty-prints.
const DecisionBodyMax = 16 << 10

// DecisionsDeps is what the decision routes are built from.
type DecisionsDeps struct {
	// Approver carries a decision out. Required.
	Approver *approval.Approver
	// Audits records and opens a compliance read. Required.
	Audits AuditReads
	// Render turns an opened conversation into the transcript a reader is
	// answered with: the assistant's own rendering, cards re-checked
	// against the reader's apps. Required.
	Render Render
	// Audit is where the runtime audit record of a compliance read goes.
	// Required.
	Audit AuditPublisher
	// Log is where a route logs; nil is the default logger.
	Log *slog.Logger
}

// Decisions serves the decision routes.
type Decisions struct {
	approver *approval.Approver
	audits   AuditReads
	render   Render
	audit    AuditPublisher
	log      *slog.Logger
}

// NewDecisions builds the decision routes, refusing a missing dependency by
// name.
func NewDecisions(d DecisionsDeps) (*Decisions, error) {
	switch {
	case d.Approver == nil:
		return nil, errors.New("assistantapi: the approver is required: a proposal is " +
			"decided only through it")
	case d.Audits == nil:
		return nil, errors.New("assistantapi: the audited reader is required for the " +
			"compliance read")
	case d.Render == nil:
		return nil, errors.New("assistantapi: the transcript renderer is required for " +
			"the compliance read")
	case d.Audit == nil:
		return nil, errors.New("assistantapi: an audit publisher is required: every " +
			"compliance read is an audit record")
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Decisions{approver: d.Approver, audits: d.Audits, render: d.Render,
		audit: d.Audit, log: log.With("component", "assistant_decisions")}, nil
}

// Mount mounts the decision routes on the authority table.
func (s *Decisions) Mount(router *authz.Router) error {
	for _, route := range []struct {
		pattern string
		action  authz.Action
		h       http.HandlerFunc
	}{
		{ApprovePattern, authz.ActionAssistantApprove, s.approve},
		{DeclinePattern, authz.ActionAssistantDecline, s.decline},
		// A PAGE'S CLAIM AND REPORT CARRY AN APPROVAL OUT, so they are the
		// approval's verb: the person's own tab acting on the action their
		// own approval sent it.
		{ActionPattern, authz.ActionAssistantApprove, s.report},
		{AuditReadPattern, authz.ActionAssistantAuditRead, s.auditRead},
	} {
		if err := router.Handle(route.pattern, authz.Policy{Action: route.action},
			route.h); err != nil {
			return fmt.Errorf("assistantapi: mount %s: %w", route.pattern, err)
		}
	}
	return nil
}

// approve is `POST /assistant/proposals/{id}/approve`.
func (s *Decisions) approve(w http.ResponseWriter, r *http.Request) {
	id, body, key, ok := s.decision(w, r)
	if !ok {
		return
	}
	answer, err := s.approver.Approve(r.Context(), approval.Approval{Proposal: id,
		Digest: body.Digest, Key: key})
	if err != nil {
		s.refuse(w, r, key, err)
		return
	}
	httpjson.Write(w, http.StatusAccepted, answer)
}

// decision reads what an approval or a decline names: the proposal in the
// path, the digest in the body, and the operation in the header, which is
// required, because a decision whose answer was lost is retried as the same
// decision.
func (s *Decisions) decision(w http.ResponseWriter, r *http.Request) (uuid.UUID,
	wire.ProposalDecision, string, bool) {

	id, ok := pathID(w, r)
	if !ok {
		return uuid.Nil, wire.ProposalDecision{}, "", false
	}
	var body wire.ProposalDecision
	if !readBody(w, r, &body) || !speaks(w, body.ProtocolVersion) {
		return uuid.Nil, wire.ProposalDecision{}, "", false
	}
	if !wire.ValidDigest(body.Digest) {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": "the digest is the one the proposal's card shows: " +
				wire.DigestPrefix + " and 64 lowercase hex digits"})
		return uuid.Nil, wire.ProposalDecision{}, "", false
	}
	key, ok := opkey.Require(w, r)
	return id, body, key, ok
}

// refuse answers a decision's error in the engine's envelope: the one mapping
// every decision route refuses by.
func (s *Decisions) refuse(w http.ResponseWriter, r *http.Request, key string, err error) {
	var (
		decided     *approval.Decided
		unconfirmed *approval.Unconfirmed
	)
	switch {
	case errors.Is(err, approval.ErrNotFound):
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
	case errors.As(err, &decided):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeProposalAlreadyDecided,
			httpjson.Detail{"state": string(decided.State)})
	case errors.Is(err, approval.ErrExpired):
		httpjson.Fail(w, http.StatusGone, httpjson.CodeProposalExpired)
	case errors.Is(err, approval.ErrDigestMismatch):
		httpjson.Fail(w, http.StatusConflict, httpjson.CodeProposalDigestMismatch)
	case errors.Is(err, approval.ErrStale):
		httpjson.Fail(w, http.StatusConflict, httpjson.CodeProposalStale)
	case errors.Is(err, approval.ErrActionClaimed):
		httpjson.Fail(w, http.StatusConflict, httpjson.CodeActionAlreadyClaimed)
	case errors.Is(err, approval.ErrStepUp):
		// THE STEP-UP THE WRITE'S OWN VERB, OR A SHARE TO THE WHOLE
		// COMPANY, ASKS (D09), answered on the approval itself: nothing was
		// recorded, and the dashboard's step-up dialog replays this request.
		httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeStepUpRequired,
			authz.StepUpDetail(authz.Decision{Reason: authz.ReasonStepUp,
				Recency: iam.RecencyStepUp}))
	case errors.Is(err, approval.ErrNotAPerson):
		s.log.InfoContext(r.Context(), "assistant_decision_not_a_person", "path", r.URL.Path,
			"error", err)
		httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeAssistantNotAvailable,
			httpjson.Detail{"reason": string(wire.ViewerNotAPerson)})
	case errors.Is(err, approval.ErrInvalidReport):
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": err.Error()})
	case errors.As(err, &unconfirmed):
		// NOTHING IS BUILT ON A DECISION NOBODY CAN CONFIRM: the answer
		// names the operation, and the same request sent again is the same
		// decision.
		s.log.WarnContext(r.Context(), "assistant_decision_unconfirmed", "path", r.URL.Path,
			"op_id", key, "error", err)
		httpjson.UnknownOutcome(w, authz.RetryUndecidedSeconds, key, false,
			httpjson.Detail{"detail": opkey.UnknownDetail(false)})
	default:
		s.log.ErrorContext(r.Context(), "assistant_decision_failed", "path", r.URL.Path,
			"error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
	}
}

// pathID is the route's `{id}`, answered 404 when it is no id at all: an id
// that names nothing and one that is not an id are the same to a caller.
func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil || id == uuid.Nil {
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return uuid.Nil, false
	}
	return id, true
}

// readBody reads a JSON body into v: declared JSON, within
// [DecisionBodyMax], exactly one object, and no field v does not declare.
//
// JSON AND NOTHING ELSE, for the act route's reason: a cross-site form can post
// text/plain without a preflight, and the Origin gate in front of every guarded
// write is the second lock, never the only one.
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if charset, named := params["charset"]; err != nil || media != "application/json" ||
		(named && !strings.EqualFold(charset, "utf-8")) {
		httpjson.FailWithFields(w, http.StatusUnsupportedMediaType,
			httpjson.CodeUnsupportedMediaType, httpjson.Detail{
				"detail": "the body must be declared Content-Type: application/json"})
		return false
	}
	raw, err := httpjson.ReadBody(w, r, DecisionBodyMax)
	if err != nil {
		httpjson.Refuse(w, err)
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": "the body must be one JSON object of the route's " +
				"own fields: " + err.Error()})
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": "the body carries something after its JSON object"})
		return false
	}
	return true
}

// speaks refuses a body written against another protocol version than this
// build's, which reloading the dashboard fixes.
func speaks(w http.ResponseWriter, version int) bool {
	if version == wire.ProtocolVersion {
		return true
	}
	httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeAssistantProtocolMismatch,
		httpjson.Detail{"protocol_version": wire.ProtocolVersion})
	return false
}
