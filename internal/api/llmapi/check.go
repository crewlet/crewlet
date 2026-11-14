package llmapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/textcut"
)

// CheckBudget is the longest the doctor over the API may take: four harness
// runs at most (the smoke run, the shell probe, the file probe where the shell
// was refused, and the web probe), each measured at 3 to 20 s, beside the
// version and executor checks, which are local — twice the measured worst with
// room for a slow vendor. The check runs on its node's own time, so the budget
// does not have to fit the silence a proxy in front of the API allows.
const CheckBudget = 180 * time.Second

// checkRequest is the body `POST /llm/providers/{key}/check` reads. It carries
// no credential value: a typed credential is tested by the credential route's
// dry run, so a value never reaches a route an assistant may be granted.
type checkRequest struct {
	Smoke  bool            `json:"smoke"`
	Entity json.RawMessage `json:"entity"`
}

// check serves POST /llm/providers/{key}/check: the doctor over the API, on
// the active entry or a draft, run on this node's own time
// ([Engine.StartModelCheck]) and answered `202` with it running — `200` where
// it ended within the engine's hold — its outcome read from any node at `GET
// /llm/providers/{key}/check` (the `llm_check` question).
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
		if !configapi.RefuseDraft(w, err) {
			return
		}
		entry := next.Providers.LLM[key]
		draft = &entry
	} else if active == nil {
		notFound(w, key)
		return
	}
	smoke := req.Smoke
	ask := ModelCheckAsk{Entry: key, Draft: draft != nil, Smoke: smoke, By: actor(r), Budget: CheckBudget}
	started, err := s.engine.StartModelCheck(r.Context(), ask, func(ctx context.Context) (json.RawMessage, error) {
		checked, err := s.engine.CheckModel(ctx, key, draft, nil, smoke)
		switch {
		case errors.Is(err, ErrNoSuchModel):
			return nil, fmt.Errorf("the company no longer declares %q, so there is nothing to check", key)
		case err != nil:
			return nil, err
		}
		return boundedAnswer(answerOf(checked))
	})
	var running *ModelCheckInProgressError
	switch {
	case errors.As(err, &running):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeCheckInProgress,
			httpjson.Detail{"running": queries.ModelCheckOf(running.Running, s.now())})
		return
	case errors.Is(err, ErrModelChecksStopped):
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(StoppingRetryAfter),
			httpjson.Detail{"detail": "this node is stopping: check the model on another node"})
		return
	case err != nil && r.Context().Err() != nil:
		// THE PERSON LEFT while the route held: the check goes on, and its
		// record says how it ends to whoever asks next.
		return
	case err != nil:
		s.fail(w, "check the model", err)
		return
	}
	status := http.StatusOK
	if started.Running() {
		status = http.StatusAccepted
		w.Header().Set("Location", "/llm/providers/"+url.PathEscape(key)+"/check")
	}
	httpjson.Write(w, status, queries.ModelCheckOf(started, s.now()))
}

// boundedAnswer is a check's answer as its record keeps it: whole where it
// fits [coord.MaxModelCheckAnswerBytes], and otherwise with the longest of its
// lines' details and remedies cut in half, again and again, until it does.
// The doctor's lines quote what the harness said, which a model answering at
// length makes long; a check's verdict and its every line survive the cut.
// The lines are cut in a copy, never in the caller's answer.
func boundedAnswer(a CheckAnswer) (json.RawMessage, error) {
	a.Lines = slices.Clone(a.Lines)
	for {
		raw, err := json.Marshal(a)
		if err != nil {
			return nil, fmt.Errorf("render the check's answer: %w", err)
		}
		if len(raw) <= coord.MaxModelCheckAnswerBytes {
			return raw, nil
		}
		var longest *string
		for i := range a.Lines {
			for _, field := range []*string{&a.Lines[i].Detail, &a.Lines[i].Remedy} {
				if longest == nil || len(*field) > len(*longest) {
					longest = field
				}
			}
		}
		if longest == nil || len(*longest) <= minCutAnswerField {
			return nil, fmt.Errorf("the check's answer is %d bytes with every line already cut, "+
				"past the %d its record keeps", len(raw), coord.MaxModelCheckAnswerBytes)
		}
		*longest = textcut.Within(*longest, len(*longest)/2)
	}
}

// minCutAnswerField is the shortest a line's detail or remedy is cut to: past
// it a cut leaves no sentence, and a dozen lines of it fit any answer's bound
// many times over, so reaching it means the answer is long for another reason.
const minCutAnswerField = 256
