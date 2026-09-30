package httpjson_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// fallbackMux is a mux with one GET route, whose handler writes a 404 of its
// own, behind [httpjson.Mux].
func fallbackMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /things/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no_thing"}`))
	})
	return httpjson.Mux(mux)
}

// THE MUX'S OWN 404 AND 405 ARE JSON WITH A CODE, like every other answer the
// engine writes. They were net/http's text/plain, which every client of this
// engine reads as something IN FRONT of the node — so the engine's own "there
// is nothing here" read as a gateway that may have swallowed a write.
func TestTheMuxsOwnRefusalsAreJSONWithACode(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		method, path string
		status       int
		code         httpjson.Code
		allow        string
	}{
		"a path nothing serves":         {http.MethodGet, "/nowhere", http.StatusNotFound, httpjson.CodeNoRoute, ""},
		"a served path, another method": {http.MethodPost, "/things/7", http.StatusMethodNotAllowed, httpjson.CodeMethodNotAllowed, "GET, HEAD"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			fallbackMux().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != tc.status {
				t.Fatalf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.status)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content-type = %q, want application/json", ct)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("the answer is not JSON (%v): %q", err, rec.Body.String())
			}
			if body["error"] != string(tc.code) || body["detail"] == "" {
				t.Errorf("the answer is %v, want code %s with a detail", body, tc.code)
			}
			if got := rec.Header().Get("Allow"); got != tc.allow {
				t.Errorf("Allow = %q, want %q — the mux's own answer to which "+
					"methods it takes", got, tc.allow)
			}
		})
	}
}

// A ROUTE'S OWN ANSWER AND THE MUX'S REDIRECT PASS THROUGH UNTOUCHED: only the
// mux's refusals are rewritten, never a handler's 404 and never a 3xx.
func TestOnlyTheMuxsOwnRefusalsAreRewritten(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	fallbackMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/things/7", nil))
	if rec.Code != http.StatusNotFound || rec.Body.String() != `{"error":"no_thing"}` {
		t.Errorf("the route's own 404 came back %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	fallbackMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/a/../nowhere", nil))
	if rec.Code/100 != 3 || rec.Header().Get("Location") != "/nowhere" {
		t.Errorf("the path-cleaning redirect came back %d to %q", rec.Code,
			rec.Header().Get("Location"))
	}
}
