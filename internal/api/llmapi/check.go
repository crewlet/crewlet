package llmapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
)

// CheckBudget is the longest the doctor over the API may take: three CLI runs
// at most (the smoke run, the shell probe and the web probe), each measured at
// 3 to 20 s, beside the version and executor checks, which are local.
const CheckBudget = 180 * time.Second

// checkRequest is the body `POST /llm/providers/{key}/check` reads. It carries
// no credential value: a typed credential is tested by the credential route's
// dry run, so a value never reaches a route an assistant may be granted.
type checkRequest struct {
	Smoke  bool            `json:"smoke"`
	Entity json.RawMessage `json:"entity"`
}

// check serves POST /llm/providers/{key}/check: the doctor over the API, on
// the active entry or a draft, on this node.
func (s *Service) check(w http.ResponseWriter, r *http.Request, _ *types.ModelActed) {
	key := r.PathValue("key")
	var req checkRequest
	if !decode(w, r, &req, `{"smoke": true, "entity": {...}}`) {
		return
	}
	_, active, ok := s.activeEntry(w, r, key)
	if !ok {
		return
	}
	var draft *config.LLMProvider
	if len(req.Entity) > 0 && string(req.Entity) != "null" {
		// A DRAFT IS DECIDED AS THE WRITE IT STANDS FOR ([mayRunDraft]),
		// before it is read any further.
		if !mayRunDraft(w, r, s.guard) {
			return
		}
		// A DRAFT RESOLVES ITS ${VAR}s THROUGH THE STORE, as an apply would:
		// it is checked as the company would run it once written.
		next, err := s.config.CheckEntity(r.Context(), configapi.EntityLLMProviders, key,
			req.Entity, active == nil, actor(r))
		if !refuseDraft(w, err) {
			return
		}
		entry := next.Providers.LLM[key]
		draft = &entry
	} else if active == nil {
		notFound(w, key)
		return
	}
	checked, err := s.engine.CheckModel(r.Context(), key, draft, nil, req.Smoke)
	switch {
	case errors.Is(err, ErrNoSuchModel):
		notFound(w, key)
		return
	case err != nil:
		s.fail(w, "check the model", err)
		return
	}
	httpjson.Write(w, http.StatusOK, answerOf(checked))
}
