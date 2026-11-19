package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REFUSAL THAT SAYS ITS REASON IS IN THIS NODE'S LOG WRITES IT THERE: an
// advisor log read or write that failed for the engine's own reason answers
// 500 and logs one line naming what was being done and why; a copy that could
// not serve it now answers 503 with its hint; a malformed write answers 400.
// Never the request-body refusal's 400. Mutation: answer the 500 without the
// line, and the first row fails.
func TestAnAdvisorRefusalThatPromisesALogLineWritesOne(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		err    error
		status int
		logged string
	}{
		{"the engine's own fault", errors.New("the store's disk is full"), http.StatusInternalServerError, "api_advisor_failed"},
		{"no copy serves it now", fmt.Errorf("estate: %w", &statelog.Unavailable{Reason: statelog.ReasonBehind,
			Detail: "the copy is behind"}), http.StatusServiceUnavailable, ""},
		{"a malformed write", fmt.Errorf("%w: run is a uuid", advisordomain.ErrInvalid), http.StatusBadRequest, ""},
	} {
		var logged bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))
		rec := httptest.NewRecorder()
		refuseAdvisorLog(rec, httptest.NewRequest(http.MethodPost, "/advisor/runs", nil), logger, "advisor_run", c.err)
		if rec.Code != c.status {
			t.Errorf("%s: answered %d, want %d", c.name, rec.Code, c.status)
		}
		if c.logged == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal(logged.Bytes(), &line); err != nil {
			t.Fatalf("%s: logged %q, want one line: %v", c.name, logged.String(), err)
		}
		if line["msg"] != c.logged || line["what"] != "advisor_run" || !strings.Contains(fmt.Sprint(line["error"]), "disk is full") {
			t.Errorf("%s: logged %v", c.name, line)
		}
	}
}
