package api

import (
	"context"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/coord"
)

// Resetting a token counter over HTTP.
//
// The counter is FLEET state — it lives in the coordination store, so that a
// company's cap is one number rather than one per node. That is what makes
// this route necessary rather than a convenience: on the default topology the
// coordination store is the engine's own embedded broker, so a CLI run while
// the engine is down cannot reach it, and one run while the engine is UP must
// not try — a second server on the same store directory is accepted rather
// than refused, and two writers on one JetStream store is corruption, not
// contention.
//
// So the reset goes where the counter is reachable: through a node that
// already holds it. `crewlet budgets reset` is a client of this route.

// budgetResetter is the slice of the coordination store this route needs.
//
// Declared here, by the consumer, and deliberately not [coord.Budgets]: a
// route that could also Charge would eventually be given a reason to.
type budgetResetter interface {
	Reset(ctx context.Context, scope string) (int, error)
	Usage(ctx context.Context) ([]coord.Usage, error)
}

// serveBudgetReset answers POST /budgets/reset.
//
// `?scope=` names one counter; its absence clears every one. Absence rather
// than an empty string is NOT a distinction this route makes — both mean "all"
// — because the alternative is an operator typing `-scope ""` and being told
// nothing matched while the counters they meant to clear kept refusing turns.
func (a *App) serveBudgetReset(w http.ResponseWriter, r *http.Request) {
	// WHO IS CLEARING A SPEND CEILING, resolved before anything is read
	// and refused three-valued: an unreadable identity estate answers 503
	// rather than letting an irreversible operator action land
	// unattributed.
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	scope := r.URL.Query().Get("scope")

	// READ FIRST, so the answer NAMES what it cleared. A count alone
	// leaves an operator unable to tell "reset the seat I meant" from
	// "reset a scope that was already empty" — and this is an irreversible
	// operator action against a spend ceiling.
	before, err := a.budgets.Usage(r.Context())
	if err != nil {
		log.Warn("api_budget_reset_failed", "stage", "read", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeBudgetUnreadable)
		return
	}
	var cleared []string
	for _, row := range before {
		if scope == "" || row.Scope == scope {
			cleared = append(cleared, row.Scope)
		}
	}

	n, err := a.budgets.Reset(r.Context(), scope)
	if err != nil {
		log.Warn("api_budget_reset_failed", "stage", "reset", "scope", scope, "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeBudgetResetFailed)
		return
	}
	operator := auth.OperatorID(caller)
	log.Info("budget_reset", "operator", operator, "scope", scope, "cleared", n)
	if cleared == nil {
		cleared = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"cleared": n, "scopes": cleared})
}
