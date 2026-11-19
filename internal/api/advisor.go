package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ADVISOR'S WRITES (ADR-0164): a person asks for a run now, or decides a
// finding (snooze it, dismiss it, reopen it, mark it useful). None writes
// config or a credential: a fix is sent by the person through the fix's own
// route, after the engine re-checks it.

// AdvisorWrites is the advisor log's writes a person's routes make.
type AdvisorWrites interface {
	duty.LogWrites
	RequestRun(ctx context.Context, c advisordomain.Call, r advisordomain.RunRequest) (statelog.Result, error)
	Release(ctx context.Context, c advisordomain.Call, key string, ifMatch uint64) (statelog.Result, error)
}

// AdvisorReads is the advisor log's reads the questions, the routes and the duty make.
type AdvisorReads interface {
	queries.AdvisorLog
	duty.LogReads
}

// AdvisorLog is the advisor log's two sides.
type AdvisorLog interface {
	AdvisorReads
	AdvisorWrites
}

type advisorLog struct {
	AdvisorReads
	AdvisorWrites
}

// advisorRuntime is a runtime that reaches the advisor log, and the claim on
// the duty's lease (nil on a node with no fleet).
type advisorRuntime interface {
	AdvisorLog() (AdvisorReads, AdvisorWrites, bool)
	AdvisorClaim() func(context.Context) (bool, error)
}

// maxAdvisorDecisionBytes bounds a decision's body: a word and a reason.
const maxAdvisorDecisionBytes = 1 << 10

// mountAdvisor registers the Advisor's writes.
func (a *App) mountAdvisor(mux authz.Mux) error {
	router := authz.NewRouter(mux, authz.ContextGuard(authz.NoChart{}))
	policy := authz.Policy{Action: authz.ActionAdvisorRun}
	return errors.Join(
		router.Handle("POST /advisor/runs", policy.Rewriting(), http.HandlerFunc(a.serveAdvisorRun)),
		router.Handle("POST /advisor/findings/{id}/decision", policy.Rewriting(),
			http.HandlerFunc(a.serveAdvisorDecision)),
		router.Handle("POST /advisor/findings/{id}/recheck", policy.Rewriting(),
			http.HandlerFunc(a.serveAdvisorRecheck)),
		router.Handle("POST /advisor/findings/{id}/applied", policy.Rewriting(),
			http.HandlerFunc(a.serveAdvisorApplied)),
	)
}

// noAdvisor answers on a node that reaches no advisor log.
func (a *App) noAdvisor(w http.ResponseWriter) bool {
	if a.advisor != nil {
		return false
	}
	httpjson.FailWith(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable, map[string]string{
		"detail": "this server reaches no copy of the Advisor's findings"})
	return true
}

// advisorCall is the call a person's write is published under: their own
// actor, and an operation derived from the request's key, the verb and the object.
func advisorCall(w http.ResponseWriter, r *http.Request, p iam.Principal, verb, object string,
	asks map[string]any) (advisordomain.Call, bool) {

	key, ok := opkey.Key(w, r, time.Now())
	if !ok {
		return advisordomain.Call{}, false
	}
	opID, err := statelog.KeyedOpID(key, verb, object, asks)
	if err != nil {
		refuseAdvisorLog(w, r, log, verb, err)
		return advisordomain.Call{}, false
	}
	return advisordomain.Call{By: iam.ActorFor(p), OpID: opID}, true
}

// serveAdvisorRun asks the duty to run everything now. A press while one is
// waiting joins it, answered 409 with the run it joins.
func (a *App) serveAdvisorRun(w http.ResponseWriter, r *http.Request) {
	if a.noAdvisor(w) {
		return
	}
	principal, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	pending, err := a.advisor.PendingRequests(r.Context(), statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		refuseAdvisorLog(w, r, log, "advisor_pending_requests", err)
		return
	}
	if len(pending) > 0 {
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeConflict, httpjson.Detail{
			"detail": "a run is already asked for; it runs within a minute", "running": map[string]any{
				"run_id": pending[0].ID, "started_at": pending[0].RequestedAt, "status": queries.RunRunning}})
		return
	}
	run := uuid.NewString()
	call, ok := advisorCall(w, r, principal, "advisor_run", run, nil)
	if !ok {
		return
	}
	if _, err := a.advisor.RequestRun(r.Context(), call, advisordomain.RunRequest{Run: run,
		By: principal.Seat}); err != nil {
		refuseAdvisorLog(w, r, log, "advisor_run", err)
		return
	}
	httpjson.Write(w, http.StatusAccepted, map[string]string{"run_id": run})
}

// Decision is what a person decides about a finding.
type Decision string

// The decisions.
const (
	DecisionSnooze  Decision = "snooze"
	DecisionDismiss Decision = "dismiss"
	DecisionReopen  Decision = "reopen"
	DecisionUseful  Decision = "useful"
)

// dismissReasons is each dismissal's word and the reason it closes with.
var dismissReasons = map[string]advisor.CloseReason{
	"wont_fix": advisor.ReasonDismissedWontFix,
	"wrong":    advisor.ReasonDismissedWrong,
	"known":    advisor.ReasonDismissedKnown,
}

// serveAdvisorDecision records a person's decision on one finding, decided
// from the finding as the log holds it now and refused where it moved.
func (a *App) serveAdvisorDecision(w http.ResponseWriter, r *http.Request) {
	if a.noAdvisor(w) {
		return
	}
	principal, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	raw, err := httpjson.ReadBody(w, r, maxAdvisorDecisionBytes)
	if err != nil {
		return
	}
	var body struct {
		Decision Decision `json:"decision"`
		Reason   string   `json:"reason"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || !slices.Contains([]Decision{DecisionSnooze,
		DecisionDismiss, DecisionReopen, DecisionUseful}, body.Decision) {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": "send a decision of snooze, dismiss, reopen or useful"})
		return
	}
	reason, known := dismissReasons[body.Reason]
	if body.Decision == DecisionDismiss && !known {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": "a dismissal names its reason: wont_fix, wrong or known"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	row, _, err := a.advisor.Finding(r.Context(), id, statelog.Freshness{Level: statelog.ReadSession})
	if errors.Is(err, advisordomain.ErrNotFound) {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{
			"detail": "no finding of the Advisor's has this id"})
		return
	}
	if err != nil {
		refuseAdvisorLog(w, r, log, "advisor_finding", err)
		return
	}
	call, ok := advisorCall(w, r, principal, "advisor_"+string(body.Decision), id,
		map[string]any{"reason": body.Reason})
	if !ok {
		return
	}
	if err := a.decide(r.Context(), call, row, body.Decision, reason); err != nil {
		refuseDecision(w, r, err)
		return
	}
	finding, err := a.advisorFinding(r.Context(), id)
	if err != nil {
		refuseAdvisorLog(w, r, log, "advisor_finding", err)
		return
	}
	httpjson.Write(w, http.StatusOK, finding)
}

// decide publishes decision on row, conditioned on the key as row read it.
func (a *App) decide(ctx context.Context, call advisordomain.Call, row advisordomain.FindingRow,
	decision Decision, reason advisor.CloseReason) error {

	now := time.Now().UTC()
	state := advisordomain.State(row.State)
	move := advisordomain.Move{Episode: row.ID}
	switch decision {
	case DecisionSnooze:
		move.Event, move.State, move.SnoozedUntil = advisordomain.EventSnoozed, state, now.Add(advisor.SnoozeFor)
	case DecisionUseful:
		move.Event, move.State = advisordomain.EventUseful, state
	case DecisionReopen:
		if advisor.State(state).Closed() {
			_, err := a.advisor.Release(ctx, call, row.Key, row.KeyVersion)
			return err
		}
		move.Event, move.State = advisordomain.EventUnsnoozed, state
	case DecisionDismiss:
		span, forGood := advisor.HoldFor(reason)
		hold := &advisordomain.Hold{Reason: string(reason), Severity: row.Severity}
		if !forGood {
			hold.Until = now.Add(span)
		}
		move.Event, move.State, move.Reason, move.Hold = advisordomain.EventClosed, advisordomain.StateDismissed,
			advisordomain.CloseReason(reason), hold
	}
	_, err := a.advisor.Move(ctx, call, row.Key, move, row.KeyVersion)
	return err
}

// refuseDecision says why a decision was not recorded, in the log's own words
// where they are the caller's to act on.
func refuseDecision(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, advisordomain.ErrStaleVersion), errors.Is(err, advisordomain.ErrIllegalMove):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeConflict, map[string]string{
			"detail": "the finding changed since it was read: " + advisordomain.Sentence(err)})
	default:
		refuseAdvisorLog(w, r, log, "advisor_decision", err)
	}
}

// refuseAdvisorLog answers a read or a write of the advisor log that failed:
// 400 for what the log refuses as malformed, in its own words; 503 where no
// copy could serve it now, with the state log's hint; and otherwise 500,
// whose answer says the reason is in this node's log, SO IT IS WRITTEN THERE,
// under what was being done. Never the request-body refusal, whose 400 would
// blame a body for a store's failure.
func refuseAdvisorLog(w http.ResponseWriter, r *http.Request, logger *slog.Logger, what string, err error) {
	var read *statelog.Refused
	var write *statelog.Unavailable
	switch {
	case errors.Is(err, advisordomain.ErrInvalid):
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": advisordomain.Sentence(err)})
	case errors.As(err, &read), errors.As(err, &write), errors.Is(err, queries.ErrUnavailable):
		stream.LogUnavailable(r.Context(), logger, "api_advisor_unavailable", err, "what", what)
		u := stream.UnavailableOf(err)
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, u.RetryAfter, u.Fields())
	default:
		stream.LogFailed(r.Context(), logger, "api_advisor_failed", err, "what", what)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
	}
}

// startAdvisor runs the duty's loop until Stop, on every node that reaches the
// log: the lease makes one of them run it.
func (a *App) startAdvisor(ctx context.Context) {
	if a.advisorDuty == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.advisorDuty.Serve(ctx)
	}()
	a.stopAdvisor = func() {
		cancel()
		<-done
	}
}
