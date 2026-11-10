package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/insights"
)

// AN ADMINISTRATOR'S DECISION MOVES ONLY A QUEUED RECORD: a send schedules it,
// a record that is not waiting is refused 409, a body that says neither is 400,
// and a server keeping no record answers 404.
func TestAnInsightDecisionMovesOnlyAQueuedRecord(t *testing.T) {
	t.Parallel()
	store := memory.NewFleet()
	if _, _, err := store.ClaimInsightRecord(t.Context(), coord.InsightRecord{Class: "wake.noop_share",
		Week: "2030-W02", Status: coord.InsightQueued, Day: "2030-01-08"},
		func(coord.InsightRecord) bool { return true }); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2030, 1, 8, 9, 0, 0, 0, time.UTC)
	a := &App{insights: &insights.Sharer{Store: store,
		Consent: func(context.Context) (config.InsightsSharing, error) {
			return config.InsightsSharing{Share: config.InsightsReview, Accepted: insights.SchemaVersion}, nil
		},
		Delay: func() time.Duration { return time.Hour }, Now: func() time.Time { return now }}}
	decide := func(app *App, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		app.serveInsightDecision(w, httptest.NewRequest(http.MethodPost, "/diagnostics/insights/decisions",
			strings.NewReader(body)))
		return w
	}
	if w := decide(a, `{"class":"wake.noop_share","week":"2030-W02","choice":"maybe"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a choice of neither = %d, want 400", w.Code)
	}
	if w := decide(a, `{"class":"review.idle","week":"2030-W02","choice":"send"}`); w.Code != http.StatusConflict {
		t.Errorf("a record that is not waiting = %d, want 409", w.Code)
	}
	if w := decide(a, `{"class":"wake.noop_share","week":"2030-W02","choice":"send"}`); w.Code != http.StatusOK {
		t.Fatalf("a send = %d: %s", w.Code, w.Body.String())
	}
	if r, _, _ := store.InsightRecord(t.Context(), "wake.noop_share", "2030-W02"); r.Status != coord.InsightScheduled ||
		!r.SendAt.Equal(now.Add(time.Hour)) {
		t.Errorf("after a send the record is %s at %s, want scheduled an hour on", r.Status, r.SendAt)
	}
	if w := decide(&App{}, `{"class":"wake.noop_share","week":"2030-W02","choice":"send"}`); w.Code != http.StatusNotFound {
		t.Errorf("a server keeping no record = %d, want 404", w.Code)
	}
}
