package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
)

// EVERY ANSWER THIS SURFACE WRITES CARRIES AN ERROR CODE, the mux's own 404
// and 405 included.
//
// The CLI and the dashboard read a refusal with no code as NOT the engine's —
// something in front of it answered, and the node may still have done what was
// asked. The mux answered a route it does not serve with net/http's
// text/plain, so a purge sent to a node with no native tracker (where the
// route is absent by design) printed "whether the purge landed is unknown"
// with an -op-id retry that met the same 404 for ever; and so did every route
// a newer client asks an older node for, and every wrong method.
func TestAnUnknownRouteAndAWrongMethodAnswerJSONWithACode(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b})
	for _, tc := range []struct {
		name, method, path string
		status             int
		code               string
	}{
		{"a route nothing serves", http.MethodGet, "/no/such/route",
			http.StatusNotFound, "no_route"},
		{"the purge on a node with no native tracker", http.MethodPost,
			"/work/t-1/purge?confirm=t-1", http.StatusNotFound, "no_route"},
		{"a served route under another method", http.MethodGet,
			"/work/retention/ack", http.StatusMethodNotAllowed, "method_not_allowed"},
		{"a missing dashboard asset", http.MethodGet, "/static/no-such-asset.js",
			http.StatusNotFound, "no_route"},
	} {
		rec := send(t, a, tc.method, tc.path)
		if rec.Code != tc.status {
			t.Errorf("%s: %s %s = %d, want %d", tc.name, tc.method, tc.path,
				rec.Code, tc.status)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: content-type = %q, want application/json", tc.name, ct)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil ||
			body["error"] != tc.code {
			t.Errorf("%s: answered %q, want JSON with code %s", tc.name,
				rec.Body.String(), tc.code)
		}
	}
}
