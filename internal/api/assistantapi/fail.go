package assistantapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/assistant"
)

// fail answers an orchestrator error in the engine's envelope: the one mapping
// from what the assistant reports to a status and a code, so every route
// refuses alike.
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
		s.log.WarnContext(r.Context(), "assistant_unavailable", "path", r.URL.Path,
			"error", unavailable.Cause)
		httpjson.UnavailableWith(w, httpjson.CodeAssistantUnavailable,
			httpjson.RetrySeconds(unavailable.RetryAfter), nil)
	case errors.Is(err, assistant.ErrInvalid):
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": strings.TrimPrefix(err.Error(),
				assistant.ErrInvalid.Error()+": ")})
	default:
		s.log.ErrorContext(r.Context(), "assistant_route_failed", "path", r.URL.Path,
			"error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
	}
}
