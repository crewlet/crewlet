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
// else is the node's own fault (the control): `500` with no Retry-After, where
// it was a 503 in the fixed sentence, which asked a person to come back for a
// fault no wait clears.
//
// Mutation: answer a condition in the fixed sentence again and its rows go
// red; answer the control as a 503 and its row does.
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
		status int
		says   any
	}{
		"a write on a full log": {func(w http.ResponseWriter, r *http.Request) {
			writeFailed(w, r, "op-1", full)
		}, http.StatusServiceUnavailable, fullSaid},
		"a read on a node behind": {func(w http.ResponseWriter, r *http.Request) {
			readFailed(w, r, behind)
		}, http.StatusServiceUnavailable, behindSaid},
		"a read that faulted": {func(w http.ResponseWriter, r *http.Request) {
			readFailed(w, r, fmt.Errorf("read /var/lib/crewlet/replicated.db: disk I/O"))
		}, http.StatusInternalServerError, nil},
	} {
		rec := httptest.NewRecorder()
		c.answer(rec, httptest.NewRequest(http.MethodGet, "/artifacts/a/data", nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != c.status || body["detail"] != c.says {
			t.Errorf("%s answered %d %v, want %d with %v", name, rec.Code, body, c.status, c.says)
		}
		if c.status == http.StatusInternalServerError && rec.Header().Get("Retry-After") != "" {
			t.Errorf("%s carried a Retry-After for a fault", name)
		}
		if text := rec.Body.String(); strings.Contains(text, "10.0.0.7") ||
			strings.Contains(text, "statelog") || strings.Contains(text, "/var/lib") {
			t.Errorf("%s carried its error's own text: %s", name, text)
		}
	}
}
