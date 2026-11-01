package assistantapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/authz"
)

// fail answers an orchestrator error in the engine's envelope: the one mapping
// from what the assistant reports to a status and a code, so every route
// refuses alike.
//
// A CONDITION OF THE NODE IS TOLD AS ONE ([operator.AnswerCondition]): a copy
// behind its log, a log that refused the append, a coordination store or a
// broker that did not answer is `503 unavailable` in a person's words for it,
// under its own hint, and with none where no wait clears it. Anything no
// condition claims is a fault, `500 internal_error`, its words the log's. Both
// were `503 assistant_unavailable` under a Retry-After of two seconds and no
// detail, so a full log was asked again every two seconds and a broken store
// was said to be coming back. `assistant_unavailable` is left to the
// assistant's own reasons ([assistant.Unavailable]), which no wait it knows
// clears.
func (s *Surface) fail(w http.ResponseWriter, r *http.Request, err error) {
	var (
		busy        *assistant.Busy
		unavailable *assistant.Unavailable
		refused     *assistant.NotAvailable
		budget      *assistant.BudgetRefused
		notReady    *assistant.ModelNotReady
	)
	switch {
	case errors.Is(err, assistant.ErrNotFound):
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
	case errors.As(err, &refused):
		httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeAssistantNotAvailable,
			httpjson.Detail{"reason": string(refused.Reason), "detail": refused.Detail})
	case errors.Is(err, assistant.ErrTurnRunning):
		httpjson.Fail(w, http.StatusConflict, httpjson.CodeAssistantTurnRunning)
	case errors.As(err, &busy):
		httpjson.ThrottledWith(w, httpjson.CodeAssistantBusy, busy.RetryAfter,
			httpjson.Detail{"detail": busy.Reason})
	case errors.As(err, &budget):
		detail := httpjson.Detail{"scope": budget.Scope, "window": budget.Window}
		// WHERE THE CEILING THAT REFUSED IT IS RAISED (ADR-0060), so the
		// drawer offers the way there rather than only the wait.
		if remedy := budget.Remedy(); !remedy.IsZero() {
			detail["remedy"] = remedy
		}
		wait := time.Minute
		if !budget.ResetsAt.IsZero() {
			detail["resets_at"] = budget.ResetsAt.UTC().Format(time.RFC3339)
			wait = budget.ResetsAt.Sub(s.now())
		}
		httpjson.ThrottledWith(w, httpjson.CodeBudgetExhausted, wait, detail)
	case errors.As(err, &notReady):
		s.log.InfoContext(r.Context(), "assistant_model_not_ready", "path", r.URL.Path,
			"code", string(notReady.Notice.Code), "error", notReady.Cause)
		detail := httpjson.Detail{"reason": string(notReady.Notice.Code),
			"detail": notReady.Notice.Message}
		if notReady.Notice.Hint != "" {
			detail["hint"] = notReady.Notice.Hint
		}
		if notReady.Notice.RetryAt != nil {
			detail["retry_at"] = notReady.Notice.RetryAt.UTC().Format(time.RFC3339)
		}
		httpjson.UnavailableWith(w, httpjson.CodeAssistantModelNotReady,
			httpjson.RetrySeconds(notReady.RetryAfter), detail)
	case errors.As(err, &unavailable):
		if operator.AnswerCondition(r.Context(), w, s.log, "assistant_condition",
			unavailable.Cause, nil, "path", r.URL.Path) {
			return
		}
		s.log.WarnContext(r.Context(), "assistant_unavailable", "path", r.URL.Path,
			"error", unavailable.Cause)
		httpjson.UnavailableWith(w, httpjson.CodeAssistantUnavailable, 0, nil)
	case errors.Is(err, assistant.ErrInvalid):
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": strings.TrimPrefix(err.Error(),
				assistant.ErrInvalid.Error()+": ")})
	case errors.Is(err, assistant.ErrOutcomeUnknown):
		// A WRITE OF THE ENGINE'S OWN THAT A READ CARRIED, whose outcome
		// nobody can confirm: a turn whose proposals all lapsed is closed
		// expired as its transcript is read, under an operation every node
		// derives alike, so the read asked again sends the same close and
		// the log answers it with the first where that one landed. Not the
		// unknown answer of a write the caller sent ([Surface.failWrite]):
		// this caller sent no key, and has only the read to ask again.
		s.log.WarnContext(r.Context(), "assistant_read_unsettled", "path", r.URL.Path,
			"error", err)
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, authz.RetryUndecidedSeconds,
			httpjson.Detail{"detail": unsettledRead})
	case operator.AnswerCondition(r.Context(), w, s.log, "assistant_condition", err, nil,
		"path", r.URL.Path):
		// A CONDITION, in a person's words and under its own hint.
	default:
		s.log.ErrorContext(r.Context(), "assistant_route_failed", "path", r.URL.Path,
			"error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
	}
}

// unsettledRead is what a read is told whose own write of the engine's it
// could not confirm.
const unsettledRead = "This couldn't be brought up to date just now. Try again in a moment."

// failWrite is [Surface.fail] for a write sent under the operation key: a
// write whose outcome nobody can establish ([assistant.ErrOutcomeUnknown]) is
// answered as the unknown it is, `outcome: unknown` and the key a retry sends
// again, and never as a condition of the node. It may have landed, so "try
// again in a moment" under a fresh key could make the change twice.
func (s *Surface) failWrite(w http.ResponseWriter, r *http.Request, key string, err error) {
	if !errors.Is(err, assistant.ErrOutcomeUnknown) {
		s.fail(w, r, err)
		return
	}
	s.log.WarnContext(r.Context(), "assistant_write_unconfirmed", "path", r.URL.Path,
		"op_id", key, "error", err)
	httpjson.UnknownOutcome(w, authz.RetryUndecidedSeconds, key, false,
		httpjson.Detail{"detail": opkey.UnknownDetail(false)})
}
