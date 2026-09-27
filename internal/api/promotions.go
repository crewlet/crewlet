package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/learning"
)

// The skill-promotion ledger, for an operator.
//
// The ledger is the fleet's record of every convergence the promotion pass
// acted on — drafted, rejected by a lead, declined by the model, or refused by
// the knowledge base — and it keeps each for good: a record is what stops the
// pass drafting again what a lead said no to. So a record the pass got wrong
// is wrong for good too, unless a person can see it and clear it. A
// Confluence draft a page restriction hides from the org token answers 404
// exactly as a deleted one does, and is recorded as rejected.
//
// THROUGH A NODE, for the reason `crewlet budgets reset` goes through one: the
// ledger lives in the coordination store, which on the default topology is
// inside the engine's own process and binds no socket.
//
// `GET /learning/promotions` reads it, rendered by internal/learning
// ([learning.PromotionReports]) because the value is that package's
// vocabulary. `POST /learning/promotions/clear` deletes one record at the
// version the route read, so the next pass drafts that convergence as though
// it had never been drafted. A POST, so the anonymous-read posture never
// opens it: clearing a lead's recorded decision is not a read.

// promotionLedger is the slice of the coordination store these routes need.
//
// Declared here, by the consumer, and deliberately not [coord.Promotions]: a
// route that could also create or update a record would be a second writer of
// a vocabulary only internal/learning composes.
type promotionLedger interface {
	AllPromotions(ctx context.Context) ([]coord.PromotionRecord, error)
	Promotions(ctx context.Context, unit string) ([]coord.PromotionRecord, error)
	DeletePromotion(ctx context.Context, unit, fingerprint string, version uint64) (bool, error)
}

// mountPromotions registers the ledger's two routes.
func (a *App) mountPromotions(mux *http.ServeMux) {
	mux.Handle("GET /learning/promotions", http.HandlerFunc(a.serveListPromotions))
	mux.Handle("POST /learning/promotions/clear", http.HandlerFunc(a.serveClearPromotion))
}

// serveListPromotions answers GET /learning/promotions: every record, or the
// records of `?unit=` alone.
//
// EVERY UNIT BY DEFAULT, rather than the units the company runs now: a record
// filed under a unit since renamed still stands for its convergence, and it is
// exactly the kind an operator comes here to find.
func (a *App) serveListPromotions(w http.ResponseWriter, r *http.Request) {
	unit := r.URL.Query().Get("unit")
	var held []coord.PromotionRecord
	var err error
	if unit == "" {
		held, err = a.promotions.AllPromotions(r.Context())
	} else {
		held, err = a.promotions.Promotions(r.Context(), unit)
	}
	if err != nil {
		promotionsUnreadable(w, "read", unit, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"promotions": learning.PromotionReports(held),
	})
}

// serveClearPromotion answers POST /learning/promotions/clear: `?unit=` and
// `?fingerprint=` name the record, and the answer is the record as it was
// cleared.
//
// READ FIRST, AND DELETED AT THE VERSION READ. The answer names what it
// cleared, and a record the pass moved between the read and the delete — to a
// lead's rejection, say — is refused rather than deleted unseen.
func (a *App) serveClearPromotion(w http.ResponseWriter, r *http.Request) {
	unit, fingerprint := r.URL.Query().Get("unit"), r.URL.Query().Get("fingerprint")
	if unit == "" || fingerprint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "promotion_address_required",
			"detail": "name the record by its unit and its fingerprint, as " +
				"GET /learning/promotions lists them",
		})
		return
	}
	held, err := a.promotions.Promotions(r.Context(), unit)
	if err != nil {
		promotionsUnreadable(w, "read", unit, err)
		return
	}
	var found *coord.PromotionRecord
	for i := range held {
		if held[i].Fingerprint == fingerprint {
			found = &held[i]
		}
	}
	if found == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error":  "promotion_not_found",
			"detail": "unit " + unit + " holds no record " + fingerprint,
		})
		return
	}
	deleted, err := a.promotions.DeletePromotion(r.Context(), unit, fingerprint, found.Version)
	if err != nil {
		promotionsUnreadable(w, "clear", unit, err)
		return
	}
	if !deleted {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "promotion_changed",
			"detail": "the record changed or went while it was being cleared, " +
				"and nothing was deleted",
			"hint": "read the ledger again, and clear the record as it is now",
		})
		return
	}
	report := learning.PromotionReports([]coord.PromotionRecord{*found})[0]
	operator, _ := auth.OperatorFrom(r.Context())
	log.Info("promotion_cleared", "operator", operator, "unit", unit,
		"fingerprint", fingerprint, "state", report.State, "title", report.Title,
		"page_id", report.PageID)
	writeJSON(w, http.StatusOK, map[string]any{"cleared": report})
}

// promotionsUnreadable answers a ledger the coordination store could not
// serve.
//
// 503 FOR AN UNREACHABLE STORE, which passes, and 500 for anything else. The
// store's own words go to the log rather than the caller, as every other
// route's failure here does.
func promotionsUnreadable(w http.ResponseWriter, stage, unit string, err error) {
	log.Warn("api_promotions_failed", "stage", stage, "unit", unit, "error", err)
	if errors.Is(err, coord.ErrUnavailable) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":  "promotions_unavailable",
			"detail": "the coordination store could not be reached",
			"hint":   "ask again once the node's coordination store answers",
		})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal_error"})
}
