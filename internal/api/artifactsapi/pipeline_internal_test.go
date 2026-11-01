package artifactsapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A NODE THAT TAKES NO UPLOADS SAYS SO, AND THAT NO WAIT CHANGES IT.
//
// The engine builds the upload pipeline once for the life of the process, so a
// node that could not — no object store, no coordination store to reserve
// bytes in — answers every later upload the same way. It was answered "this
// node takes no artifact uploads: " followed by the engine's own error, a
// configuration's words for whoever runs the node, under a Retry-After of two
// seconds that sent a client back for ever. It is a sentence for a person and
// no Retry-After; the error goes to the log.
//
// Mutation: put the error back in the detail, or answer the undecided hint
// again, and this goes red.
func TestANodeThatTakesNoUploadsSaysSoOnce(t *testing.T) {
	t.Parallel()
	s := &Service{uploads: func() (Pipeline, error) {
		return nil, errors.New("engine: this node takes no artifact uploads: it runs no object store")
	}}
	rec := httptest.NewRecorder()
	if _, ok := s.pipeline(rec, httptest.NewRequest(http.MethodPost, "/artifacts/uploads", nil)); ok {
		t.Fatal("a node with no pipeline handed one out")
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	detail, _ := body["detail"].(string)
	if rec.Code != http.StatusServiceUnavailable || body["error"] != "unavailable" ||
		!strings.Contains(detail, "can't check or publish artifacts") {
		t.Errorf("answered %d %v, want 503 unavailable saying uploads are not taken", rec.Code, body)
	}
	if strings.Contains(detail, "engine") || strings.Contains(detail, "object store") {
		t.Errorf("the engine's own error reached the detail: %q", detail)
	}
	if after := rec.Header().Get("Retry-After"); after != "" {
		t.Errorf("Retry-After = %q, but no wait gives this node a pipeline", after)
	}
}
