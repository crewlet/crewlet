package artifactdataapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CONDITION OF THE NODE IS TOLD IN A PERSON'S WORDS FOR IT, under its own
// hint, on a datastore's write and its read alike.
//
// Each answered one fixed sentence — "this node could not write the
// artifact's data" — whatever the condition was, a log at its ceiling under no
// Retry-After included, and logged nothing at all. Now a condition is told in
// [builtin.ConditionSentence], whose advice agrees with the hint, and anything
// else keeps the fixed sentence (the control).
//
// Mutation: answer a condition in the fixed sentence again and its rows go
// red; tell the control a condition's words and its row does.
func TestAConditionIsToldInAPersonsWords(t *testing.T) {
	t.Parallel()
	const detail = "raise stream.artifact_data_max_bytes on 10.0.0.7"
	full := fmt.Errorf("artifactdata: write: %w", &statelog.Unavailable{
		Reason: statelog.ReasonLogFull, Detail: detail})
	behind := fmt.Errorf("artifactdata: read: %w", &statelog.Refused{
		Code: statelog.RefuseBehind, Level: statelog.ReadSession, Detail: detail,
		RetryAfter: 1})
	fullSaid, _ := builtin.ConditionSentence(full)
	behindSaid, _ := builtin.ConditionSentence(behind)
	for name, c := range map[string]struct {
		answer func(http.ResponseWriter, *http.Request)
		says   string
	}{
		"a write on a full log": {func(w http.ResponseWriter, r *http.Request) {
			writeFailed(w, r, "op-1", full)
		}, fullSaid},
		"a read on a node behind": {func(w http.ResponseWriter, r *http.Request) {
			readFailed(w, r, behind)
		}, behindSaid},
		"a read that faulted": {func(w http.ResponseWriter, r *http.Request) {
			readFailed(w, r, fmt.Errorf("read /var/lib/crewlet/replicated.db: disk I/O"))
		}, "This node could not read the artifact's data."},
	} {
		rec := httptest.NewRecorder()
		c.answer(rec, httptest.NewRequest(http.MethodGet, "/artifacts/a/data", nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusServiceUnavailable || body["detail"] != c.says {
			t.Errorf("%s answered %d %v, want 503 with %q", name, rec.Code, body, c.says)
		}
		if text := rec.Body.String(); strings.Contains(text, "10.0.0.7") ||
			strings.Contains(text, "statelog") || strings.Contains(text, "/var/lib") {
			t.Errorf("%s carried its error's own text: %s", name, text)
		}
	}
}
