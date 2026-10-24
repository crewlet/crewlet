package configapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// faultyMission and faultyPolicy are a charter written before the charter's
// rules: a mission of 1100 characters, and a policy on two lines.
var (
	faultyMission = strings.Repeat("m", 1100)
	faultyPolicy  = "Every amount is exact.\n## Tools"
)

// faultyCharterDoc is a company whose stored charter carries both.
func faultyCharterDoc() string {
	return companyDoc + "mission: " + faultyMission + "\npolicies:\n  - A rule.\n  - \"" +
		strings.ReplaceAll(faultyPolicy, "\n", "\\n") + "\"\n"
}

// warnedAt reports whether a write's answer warns about place.
func warnedAt(body map[string]any, place string) bool {
	warnings, _ := body["warnings"].([]any)
	for _, w := range warnings {
		if m, ok := w.(map[string]any); ok && m["path"] == place {
			return true
		}
	}
	return false
}

// AN OLD CHARTER FAULT NEVER LOCKS A COMPANY OUT OF ITS SETTINGS. Every way
// of writing the document (a patch, the builder's whole-document write and an
// entity write) takes a change that leaves the faulty parts alone, and answers
// it with a warning at each; a write that edits a faulty part and leaves it at
// fault is refused naming it, and one that fixes it lands.
func TestAnOldCharterFaultNeverLocksTheCompanyOut(t *testing.T) {
	t.Parallel()
	summary := map[string]string{"X-Summary": "a change"}
	type write struct {
		method, path string
		body         func(s *surface) string
		headers      func(s *surface) map[string]string
		status       int
	}
	unrelated := map[string]write{
		"PATCH /config": {http.MethodPatch, "/config",
			func(*surface) string { return `{"timezone":"Europe/Berlin"}` },
			func(*surface) map[string]string { return summary }, http.StatusCreated},
		"PUT /config, the builder's whole document": {http.MethodPut, "/config",
			func(*surface) string {
				return faultyCharterDoc() + "timezone: Europe/Berlin\n"
			},
			func(*surface) map[string]string { return summary }, http.StatusCreated},
		"PUT /config/llm-providers/{id}": {http.MethodPut, "/config/llm-providers/zulu",
			func(*surface) string {
				return `{"type":"anthropic","model":"claude-opus-5","api_keys":["sk-literal","${ROTATED}"]}`
			},
			func(*surface) map[string]string { return summary }, http.StatusCreated},
	}
	for name, w := range unrelated {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seed(t, faultyCharterDoc())
			res := s.do(t, w.method, w.path, w.body(s), w.headers(s))
			if res.Code != w.status {
				t.Fatalf("a write that leaves the old charter alone = %d %s, want %d",
					res.Code, res.Body, w.status)
			}
			body := decode(t, res)
			if w.status == http.StatusCreated &&
				(!warnedAt(body, "mission") || !warnedAt(body, "policies[1]")) {
				t.Errorf("the answer does not warn about the old faults: %v", body["warnings"])
			}
		})
	}

	edits := map[string]struct {
		patch  string
		status int
	}{
		"editing the faulty policy, still two lines": {
			`{"policies":["A rule.","Every amount is exact.\n## Tooling"]}`, http.StatusBadRequest},
		"editing the mission, still too long": {
			`{"mission":"` + faultyMission + `!"}`, http.StatusBadRequest},
		"fixing the faulty policy": {
			`{"policies":["A rule.","Every amount is exact."]}`, http.StatusCreated},
	}
	for name, tc := range edits {
		t.Run("PATCH /config "+name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seed(t, faultyCharterDoc())
			res := s.do(t, http.MethodPatch, "/config", tc.patch, summary)
			body := decode(t, res)
			if res.Code != tc.status {
				t.Fatalf("= %d %v, want %d", res.Code, body, tc.status)
			}
			if tc.status == http.StatusBadRequest && body["error"] != string(httpjson.CodeValidationError) {
				t.Errorf("= %v, want validation_error", body)
			}
		})
	}
}
