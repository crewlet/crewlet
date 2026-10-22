package artifactdataapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A DATASTORE'S REFUSAL IS TOLD IN ITS SENTENCE.
//
// This surface cut "artifactdata: " from the front of a refusal and left the
// rest, so a field refused read "invalid: collection …", a write the caller's
// standing refused "forbidden: …", and a schema change that lost its race
// carried the framework's error and the broker subject after the datastore's
// sentence. A lost race is told in the state log's words for a person, as on
// every surface; the rest in the datastore's own sentence.
//
// Mutation: cut the front prefix only again, or drop the statelog arm, and a
// row goes red.
func TestADatastoresRefusalIsToldInItsSentence(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err  error
		says string
	}{
		"a field": {fmt.Errorf("%w: collection %q is not in the schema",
			artifactdata.ErrInvalid, "rows"), `collection "rows" is not in the schema`},
		"a standing": {fmt.Errorf("%w: only an editor undoes another's write",
			artifactdata.ErrForbidden), "only an editor undoes another's write"},
		"a query": {fmt.Errorf("%w: %w", artifactdata.ErrInvalid,
			fmt.Errorf("%w: 9 filters is more than 8", query.ErrInvalid)),
			"9 filters is more than 8"},
		"a lost race": {fmt.Errorf("%w: d-1: %w", artifactdata.ErrConflict,
			fmt.Errorf("%w: crewlet.artifacts.data.d-1 kept changing under this write",
				statelog.ErrConflict)), statelog.LostRace},
	} {
		if got := said(c.err); got != c.says {
			t.Errorf("%s is told %q, want %q", name, got, c.says)
		}
		for _, leak := range []string{"artifactdata", "query:", "statelog", "invalid:",
			"forbidden:", "crewlet."} {
			if strings.Contains(said(c.err), leak) {
				t.Errorf("%s is told with %q in it: %s", name, leak, said(c.err))
			}
		}
	}
}

// A BODY THAT DOES NOT DECODE IS TOLD IN WORDS ([plainjson.Unreadable]): the
// detail was encoding/json's own sentence (`json: unknown field "bogus"`).
// Mutation: answer err.Error() again and this goes red.
func TestABodyThatDoesNotDecodeIsToldInWords(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ body, says string }{
		{`{"bogus":1}`, `In the body, "bogus" isn't a field this takes.`},
		{`{"ops":"x"}`, `In the body, "ops" holds text where a list belongs.`},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
		var into struct {
			Ops []string `json:"ops"`
		}
		if decode(w, r, &into) {
			t.Fatalf("%s decoded", tc.body)
		}
		var answer map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &answer)
		if w.Code != http.StatusBadRequest || answer["detail"] != tc.says {
			t.Errorf("%s answered %d %v, want 400 saying %q", tc.body, w.Code,
				answer, tc.says)
		}
	}
}
