package api_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
)

// A STALE WRITE TO THE COMPANY IS LOGGED THROUGH THE APP ITSELF. configapi
// mounts only `/config` and `/config/` on the App's mux and routes the rest on
// a sub-mux of its own, so a log that decided by the mount's pattern would
// never see `PUT /config` or the Models page's removal. Through [api.New]:
// each, sent against a revision or a tag that has moved, answers a request id and logs
// exactly one line, naming the route the sub-mux served it on, and never the
// path, which names the model. NOT PARALLEL: it installs this process's log
// sink for its duration, which only a case running alone may do.
//
// Mutation: decide by the mount's pattern again, and neither logs.
func TestAStaleCompanyWriteLogsOneLineThroughTheApp(t *testing.T) {
	var out lockedBuffer
	logging.Configure(slog.LevelInfo, logging.FormatJSON, &out)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	sources := parityWorld(t)
	fx := newAssistAppWith(t, []iam.Grant{iam.GrantAdmin}, sources.mount)
	for _, tc := range []struct {
		method, path, ifMatch, body, route, code string
		status                                   int
	}{
		// The document: a base that is no longer the active revision.
		{http.MethodPut, "/config", `"a-revision-long-gone"`, rosterCompany, "PUT /config",
			"revision_advanced", http.StatusConflict},
		// The Models page's removal: a tag that is not the entry's own,
		// the 412 the owner met.
		{http.MethodDelete, "/config/llm-providers/zulu", `"llm-providers-long-gone"`, "",
			"DELETE /config/llm-providers/{id}", "version_moved", http.StatusPreconditionFailed},
	} {
		t.Run(tc.route, func(t *testing.T) {
			before := len(out.String())
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+fixtureToken)
			req.Header.Set("If-Match", tc.ifMatch)
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/yaml")
			}
			rec := httptest.NewRecorder()
			fx.app.ServeHTTP(rec, req)
			logged := out.String()[before:]
			if rec.Code != tc.status {
				t.Fatalf("%s %s against a moved revision = %d %s, want %d",
					tc.method, tc.path, rec.Code, rec.Body, tc.status)
			}
			id := rec.Header().Get("X-Request-Id")
			if id == "" {
				t.Errorf("the refusal answered no X-Request-Id")
			}
			var lines []map[string]any
			for _, raw := range strings.Split(strings.TrimSpace(logged), "\n") {
				var line map[string]any
				if json.Unmarshal([]byte(raw), &line) == nil && line["msg"] == "write_refused" {
					lines = append(lines, line)
				}
			}
			if len(lines) != 1 {
				t.Fatalf("the refusal logged %d lines, want one:\n%s", len(lines), logged)
			}
			if lines[0]["route"] != tc.route || lines[0]["method"] != tc.method ||
				lines[0]["status"] != float64(tc.status) || lines[0]["code"] != tc.code ||
				lines[0]["request_id"] != id || lines[0]["component"] != "api.config" {
				t.Errorf("the line = %v, want route %q, %s, %d %s, request id %q, on api.config",
					lines[0], tc.route, tc.method, tc.status, tc.code, id)
			}
			if strings.Contains(logged, "zulu") {
				t.Errorf("the log names the model the path did: %s", logged)
			}
		})
	}
}
