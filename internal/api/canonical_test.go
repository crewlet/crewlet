package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// A PATH THAT IS NOT CANONICAL IS REFUSED BEFORE ANYTHING READS IT.
//
// internal/authz's CanonicalPath was written for exactly this frame, and its
// own doc said it belonged in the outer middleware — and it was mounted
// nowhere, so the refusal the authz package documented never happened on a
// running node. What stood in for it was ServeMux's redirect, which kept a
// path like `/webhooks/../config` harmless only by accident: the guard read
// it as the EXEMPT webhook prefix and waved it through, and the mux read it as
// `/config` and redirected. Two layers deciding about two different paths is
// the thing a gate must never depend on.
//
// Each case names a guarded route reached through an exempt prefix, spelled
// both ways the dot segment can arrive, plus a doubled slash. Every one must be
// the envelope's 400 — not the mux's redirect, and not whatever the exempt
// prefix would have let through.
func TestANonCanonicalPathIsRefusedBeforeTheGuardReadsIt(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{})
	for _, path := range []string{
		"/webhooks/../config",
		"/webhooks/%2e%2e/config",
		"/static/../../etc/passwd",
		"//health",
	} {
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400 before routing\n%s", path, rec.Code,
				rec.Body.String())
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: the refusal is not JSON: %v", path, err)
		}
		if body["error"] != string(httpjson.CodeNonCanonicalPath) {
			t.Errorf("GET %s refused as %v, want %s", path, body["error"],
				httpjson.CodeNonCanonicalPath)
		}
	}
	// AND THE CANONICAL SPELLING STILL ANSWERS, or the case above would pass
	// on a surface that refused everything.
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /health = %d, want 200", rec.Code)
	}
}
