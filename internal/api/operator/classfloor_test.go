package operator_test

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/caveat"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
)

// EVERY CLASS'S OWN SENTENCE STANDS ALONE (ADR-0060).
//
// A refusal nobody has worded for a person is answered with no `detail`, so
// the message its code carries is the whole of what that person reads, on the
// act route, the work surface and every client of either. A message that
// points at "the detail" then points at nothing: `quota_exceeded` told a
// person past a datastore's limit to ask an operator to raise "the limit the
// detail names", and no detail named one. This answers every class this build
// knows through the one refusal mapping, with no person's words, and holds
// each answer to carrying no detail and a message that leans on none.
//
// Mutation: put "the limit the detail names" back into quota_exceeded's
// message, and its class goes red.
func TestEveryClassesOwnSentenceStandsAlone(t *testing.T) {
	t.Parallel()
	for _, class := range crewletmcp.Refusals {
		name := string(class)
		rec := httptest.NewRecorder()
		operator.Fail(rec, crewletmcp.Classify(class, errors.New("refused")),
			caveat.Refusal{}, "op", nil)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s answered %d with a body that is not JSON: %q", name, rec.Code,
				rec.Body)
		}
		if detail, has := body["detail"]; has {
			t.Errorf("%s answered a detail nobody worded for a person: %v", name, detail)
		}
		message, _ := body["message"].(string)
		if message == "" || strings.Contains(strings.ToLower(message), "detail") {
			t.Errorf("%s answered the sentence %q, which does not stand alone: the "+
				"answer carries no detail", name, message)
		}
	}
}
