package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/analyst"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
)

// maxAnalysisAnswerBytes bounds an answer's body: a fingerprint and a word.
const maxAnalysisAnswerBytes = 1 << 10

// mountAnalysis registers the analyst's two writes (ADR-0157): a run now, and
// a person's answer to one of its findings. Neither writes config or a credential.
func (a *App) mountAnalysis(mux authz.Mux) error {
	router := authz.NewRouter(mux, authz.ContextGuard(authz.NoChart{}))
	policy := authz.Policy{Action: authz.ActionAdvisorRun}
	return errors.Join(
		router.Handle("POST /advisor/analysis", policy.Rewriting(), http.HandlerFunc(a.serveAnalysisRun)),
		router.Handle("POST /advisor/analysis/answers", policy.Rewriting(), http.HandlerFunc(a.serveAnalysisAnswer)),
	)
}

// noAnalyst answers on a node that keeps no record of the analyst.
func (a *App) noAnalyst(w http.ResponseWriter) bool {
	if a.analyst != nil {
		return false
	}
	httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{
		"detail": "this server keeps no record of the advisor's analyst"})
	return true
}

func (a *App) serveAnalysisRun(w http.ResponseWriter, r *http.Request) {
	if a.noAnalyst(w) {
		return
	}
	principal, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	run, err := a.analyst.Start(r.Context(), "person", principal.Seat)
	switch {
	case errors.Is(err, analyst.ErrRunning):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeConflict, map[string]string{
			"detail": "a run is already going; it is listed when it ends"})
	case errors.Is(err, config.ErrAnalystOff):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeConflict, map[string]string{
			"detail": err.Error()})
	case err != nil:
		httpjson.Refuse(w, err)
	default:
		httpjson.Write(w, http.StatusAccepted, run)
	}
}

func (a *App) serveAnalysisAnswer(w http.ResponseWriter, r *http.Request) {
	if a.noAnalyst(w) {
		return
	}
	principal, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	raw, err := httpjson.ReadBody(w, r, maxAnalysisAnswerBytes)
	if err != nil {
		return
	}
	var body struct {
		Fingerprint string          `json:"fingerprint"`
		Verdict     advisor.Verdict `json:"verdict"`
	}
	if err = json.Unmarshal(raw, &body); err != nil || body.Fingerprint == "" || !body.Verdict.Valid() {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": "send a finding's fingerprint and a verdict of useful, wrong or known"})
		return
	}
	switch err := a.analyst.Answer(r.Context(), body.Fingerprint, body.Verdict, principal.Seat); {
	case errors.Is(err, analyst.ErrNoFinding):
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{
			"detail": "the last run lists no finding with that fingerprint"})
	case err != nil:
		httpjson.Refuse(w, err)
	default:
		httpjson.Write(w, http.StatusOK, map[string]string{"fingerprint": body.Fingerprint,
			"verdict": string(body.Verdict)})
	}
}

// startAnalyst runs the analyst's cadence until Stop, on every node that
// serves the API: a run is a claim on the fleet's record, so one node runs it.
func (a *App) startAnalyst(ctx context.Context) {
	if a.analyst == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.analyst.Serve(ctx)
	}()
	a.stopAnalyst = func() {
		cancel()
		<-done
	}
}
