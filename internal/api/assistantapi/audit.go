package assistantapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE COMPLIANCE READ (decision D06): an administrator reading somebody else's
// conversation, with a reason, recorded before it opens.
//
// # Recorded first, and opened only for its record
//
// The read is a record in the conversation's own domain before anything is
// read, and the domain opens the conversation only for the receipt of a record
// that has applied on this node (assistantdomain.Reader.AuditTranscript), so
// there is no path that reads a conversation without leaving the line its
// owner is shown: who read it, when, and why. The runtime audit record naming
// the reader and the reason is published once the conversation opened and
// before it is answered.
//
// # Who may
//
// The authority table decides, at the route: `assistant.conversation.audit_read`
// is admin's, with a recent proof, a person present and never the assistant,
// so neither a machine token nor an assistant grant reaches it. A POST and not
// a GET, because it writes: a GET could be triggered cross-site and forge "an
// administrator read this" records the Origin gate never judged.

// AuditReads records and opens a compliance read: the domain's writer and
// audited reader, as [DomainAudits] composes them.
type AuditReads interface {
	RecordAuditRead(ctx context.Context, c assistantdomain.Call, conversation uuid.UUID,
		reason string) (assistantdomain.AuditReceipt, error)
	AuditTranscript(ctx context.Context, receipt assistantdomain.AuditReceipt) (
		assistantdomain.Transcript, error)
}

// DomainAudits is [AuditReads] over the assistant domain's writer and reader.
func DomainAudits(w *assistantdomain.Writer, r *assistantdomain.Reader) (AuditReads, error) {
	if w == nil || r == nil {
		return nil, errors.New("assistantapi: the compliance read needs the domain's " +
			"writer and reader")
	}
	return domainAudits{w: w, r: r}, nil
}

type domainAudits struct {
	w *assistantdomain.Writer
	r *assistantdomain.Reader
}

func (d domainAudits) RecordAuditRead(ctx context.Context, c assistantdomain.Call,
	conversation uuid.UUID, reason string) (assistantdomain.AuditReceipt, error) {
	return d.w.RecordAuditRead(ctx, c, conversation, reason)
}

func (d domainAudits) AuditTranscript(ctx context.Context,
	receipt assistantdomain.AuditReceipt) (assistantdomain.Transcript, error) {
	return d.r.AuditTranscript(ctx, receipt)
}

// Render is the assistant's own rendering of an opened conversation for the
// principal on ctx.
type Render func(ctx context.Context, t assistantdomain.Transcript) (wire.Transcript, error)

// AuditPublisher is where a compliance read's runtime audit record goes: the
// node's own queue, as every runtime audit record's.
type AuditPublisher = operator.AuditPublisher

// AuditFiles opens one file of a conversation for the receipt of a compliance
// read recorded beside it: *assistantdomain.FileStore as written.
type AuditFiles interface {
	AuditOpen(ctx context.Context, receipt assistantdomain.AuditReceipt, file uuid.UUID) (
		assistantdomain.File, []byte, error)
}

// auditRead is `POST /assistant/audit/conversations/{id}/read`.
func (s *Decisions) auditRead(w http.ResponseWriter, r *http.Request) {
	a, ok := s.recordAudit(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	t, err := s.audits.AuditTranscript(ctx, a.receipt)
	if !s.audited(w, r, a, err) {
		return
	}
	s.publishAudit(r, a, t.Conversation.Owner, uuid.Nil)
	rendered, err := s.render(ctx, t)
	if err != nil {
		s.log.ErrorContext(ctx, "assistant_audit_read_render_failed", "conversation",
			a.conversation, "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	httpjson.Write(w, http.StatusOK, rendered)
}

// auditDownload is
// `POST /assistant/audit/conversations/{id}/files/{file}/read`: one file of
// somebody else's conversation, recorded, audited and answered exactly as the
// transcript's compliance read is, and served as every download is.
func (s *Decisions) auditDownload(w http.ResponseWriter, r *http.Request) {
	file, ok := pathFile(w, r)
	if !ok {
		return
	}
	a, ok := s.recordAudit(w, r)
	if !ok {
		return
	}
	f, content, err := s.files.AuditOpen(r.Context(), a.receipt, file)
	if !s.audited(w, r, a, err) {
		return
	}
	s.publishAudit(r, a, f.Owner, f.ID)
	serveFile(w, f.Ref(), content)
}

// auditCall is one compliance read as it was recorded: who read, which
// conversation, why, under which operation, and the receipt the record
// answered.
type auditCall struct {
	by           iam.Actor
	conversation uuid.UUID
	reason       string
	key          string
	receipt      assistantdomain.AuditReceipt
}

// recordAudit reads a compliance read's request and records it beside the
// conversation, before anything is opened, answering the refusal itself and
// false where there is no read to go on with.
func (s *Decisions) recordAudit(w http.ResponseWriter, r *http.Request) (auditCall, bool) {
	reader, ok := auth.Caller(w, r)
	if !ok {
		return auditCall{}, false
	}
	id, ok := pathID(w, r)
	if !ok {
		return auditCall{}, false
	}
	var body wire.AuditReadRequest
	if !readBody(w, r, &body) || !speaks(w, body.ProtocolVersion) {
		return auditCall{}, false
	}
	reason := strings.TrimSpace(body.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > wire.AuditReasonMaxRunes {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": "a compliance read needs a reason of at most 500 " +
				"characters, which the conversation's owner is shown beside your name"})
		return auditCall{}, false
	}
	key, ok := opkey.Key(w, r, time.Now())
	if !ok {
		return auditCall{}, false
	}
	ctx := r.Context()
	by := iam.ActorFor(reader)
	receipt, err := s.audits.RecordAuditRead(ctx, assistantdomain.Call{By: by, OpID: key},
		id, reason)
	switch {
	case errors.Is(err, assistantdomain.ErrNotFound), errors.Is(err, assistantdomain.ErrPurged):
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return auditCall{}, false
	case errors.Is(err, assistantdomain.ErrInvalid):
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": err.Error()})
		return auditCall{}, false
	case err != nil:
		// A RECORD THE STORE REFUSED WROTE NOTHING, so it is no unknown
		// outcome: a condition of the node is told as one, in a person's
		// words under its own hint, and anything else is a fault.
		if !operator.AnswerCondition(ctx, w, s.log, "assistant_audit_read_condition", err,
			nil, "conversation", id, "op_id", key) {
			s.log.ErrorContext(ctx, "assistant_audit_read_failed", "conversation", id,
				"op_id", key, "error", err)
			httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		}
		return auditCall{}, false
	case receipt.Result().Outcome == "unknown":
		s.log.WarnContext(ctx, "assistant_audit_read_unconfirmed", "conversation", id,
			"op_id", key)
		httpjson.UnknownOutcome(w, authz.RetryUndecidedSeconds, key, false,
			httpjson.Detail{"detail": opkey.UnknownDetail(false)})
		return auditCall{}, false
	}
	return auditCall{by: by, conversation: id, reason: reason, key: key, receipt: receipt}, true
}

// audited answers what opening a recorded read refused, and reports whether
// the read goes on.
func (s *Decisions) audited(w http.ResponseWriter, r *http.Request, a auditCall, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, assistantdomain.ErrAuditPending):
		// RECORDED AND NOT YET APPLIED HERE, so not yet visible to the
		// owner on this node either: the same request again, under the same
		// operation, opens it once it is.
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, authz.RetryUndecidedSeconds,
			httpjson.Detail{"op_id": a.key, "detail": "the read was recorded and has not " +
				"reached this node yet; send the same request again"})
	case errors.Is(err, assistantdomain.ErrNotFound):
		// RECORDED, AND NO SUCH FILE IN THIS CONVERSATION: the read stays on
		// the owner's record as the attempt it was.
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
	case operator.AnswerCondition(r.Context(), w, s.log, "assistant_audit_read_condition", err,
		nil, "conversation", a.conversation, "op_id", a.key):
		// A CONDITION, in a person's words and under its own hint.
	default:
		s.log.ErrorContext(r.Context(), "assistant_audit_read_failed", "conversation",
			a.conversation, "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
	}
	return false
}

// publishAudit publishes a compliance read's runtime audit record, once what
// was read opened and before it is answered: file is uuid.Nil for a read of
// the transcript.
func (s *Decisions) publishAudit(r *http.Request, a auditCall, owner, file uuid.UUID) {
	ev := types.AssistantConversationRead{
		ActorName: a.by.Name, ActorKind: string(a.by.Kind), OperatorID: a.by.OperatorID,
		ConversationID: a.conversation.String(), OwnerID: owner.String(), Reason: a.reason,
	}
	if file != uuid.Nil {
		ev.FileID = file.String()
	}
	operator.Audit(r.Context(), s.audit, ev)
}
