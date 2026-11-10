package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/insights"
)

// maxInsightDecisionBytes bounds a decision's body: a class, a week and a word.
const maxInsightDecisionBytes = 1 << 10

// mountInsights registers an administrator's answer to a queued insight record
// (ADR-0119): send it at its own random delay, or decline it. A send is data
// leaving the company, so it is the fleet-operate verb, step-up included.
func (a *App) mountInsights(mount func(string, authz.Effect, http.HandlerFunc)) {
	mount("POST /diagnostics/insights/decisions", authz.EffectRewrites, a.serveInsightDecision)
}

func (a *App) serveInsightDecision(w http.ResponseWriter, r *http.Request) {
	if a.insights == nil {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{
			"detail": "this server keeps no record of the company's insights"})
		return
	}
	raw, err := httpjson.ReadBody(w, r, maxInsightDecisionBytes)
	if err != nil {
		return
	}
	var body struct {
		Class  string `json:"class"`
		Week   string `json:"week"`
		Choice string `json:"choice"`
	}
	if err = json.Unmarshal(raw, &body); err != nil || body.Choice != "send" && body.Choice != "decline" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": "send a class, a week and a choice of send or decline"})
		return
	}
	record, err := a.insights.Decide(r.Context(), body.Class, body.Week, body.Choice == "send")
	switch {
	case errors.Is(err, insights.ErrNotQueued):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeConflict, map[string]string{
			"detail": "this record is not waiting for a decision: sharing is not in review mode, or it was decided"})
		return
	case err != nil:
		httpjson.Refuse(w, err)
		return
	}
	httpjson.Write(w, http.StatusOK, record)
}

// startInsights runs the sharer until Stop, on every node that serves the API:
// every move is a claim on the fleet's record, so one node makes each.
func (a *App) startInsights(ctx context.Context) {
	if a.insights == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.insights.Run(ctx)
	}()
	a.stopInsights = func() {
		cancel()
		<-done
	}
}
