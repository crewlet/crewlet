package llmapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
)

// RetryBudget is the longest a probe may take: the provider park's own probe
// request, one short call, which answers in seconds on a vendor that is up.
const RetryBudget = 60 * time.Second

// retryRequest is the body `POST /llm/providers/{key}/retry` reads.
type retryRequest struct {
	Mode coord.ModelRetryMode `json:"mode"`
}

// Still is the window a probe met again.
type Still struct {
	Until    time.Time  `json:"until"`
	ResetsAt *time.Time `json:"resets_at"`
	Window   string     `json:"window"`
	Said     string     `json:"said,omitempty"`
}

// retry serves POST /llm/providers/{key}/retry: `probe` makes one call on the
// answering node and clears every node's sit-out only if it answered;
// `clear` clears it at once, and every parked chain naming the entry probes
// again.
func (s *Service) retry(w http.ResponseWriter, r *http.Request, rec *types.ModelActed) {
	key := r.PathValue("key")
	var req retryRequest
	if !decode(w, r, &req, `{"mode": "probe" | "clear"}`) {
		return
	}
	if !req.Mode.Valid() {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": "mode must be probe or clear"})
		return
	}
	if req.Mode == coord.RetryClear {
		rec.Action = types.ModelClear
	}
	_, active, ok := s.activeEntry(w, r, key)
	if !ok {
		return
	}
	if active == nil {
		notFound(w, key)
		return
	}
	retried, err := s.engine.RetryModel(r.Context(), key, req.Mode, actor(r))
	switch {
	case errors.Is(err, ErrNoSuchModel):
		notFound(w, key)
		return
	case errors.Is(err, ErrNotSittingOut):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNotSittingOut, map[string]string{
			"detail": "no live node reports " + key + " sitting out"})
		return
	case err != nil:
		s.fail(w, "retry the model", err)
		return
	}
	var still *Still
	if sit := retried.Still; sit != nil {
		still = &Still{Until: sit.Until, Window: sit.Window, Said: sit.Said}
		if !sit.ResetsAt.IsZero() {
			at := sit.ResetsAt
			still.ResetsAt = &at
		}
	}
	answer := map[string]any{
		"cleared": retried.Cleared, "mode": req.Mode, "node": retried.Node, "still": still,
	}
	if retried.Failed != "" {
		// A PROBE THAT FAILED FOR ANOTHER REASON clears nothing, and says
		// why, so the person tests the model rather than retrying again.
		answer["failed"] = retried.Failed
	}
	httpjson.Write(w, http.StatusOK, answer)
}
