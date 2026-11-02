package assistantapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A BODY THAT DOES NOT DECODE IS TOLD IN WORDS, as every other surface that
// reads a caller's JSON tells it (internal/plainjson's Unreadable), and never
// in encoding/json's own, which names Go's types and struct fields. Each case
// is held to the whole sentence a person reads, as the envelope writes it
// (httpjson.Sentence opens it with a capital and ends it with a full stop), as
// every other surface's case is. Mutation: answer the decoder's err.Error()
// again and every case goes red.
func TestABodyThatDoesNotDecodeIsToldInWords(t *testing.T) {
	t.Parallel()
	const remedy = " Send one JSON object of the route's own fields."
	for name, tc := range map[string]struct{ body, says string }{
		"a field the route does not take": {`{"protocol_version": 1, "bogus": 1}`,
			`In the body, "bogus" isn't a field this takes.` + remedy},
		"a value of the wrong kind": {`{"protocol_version": "one"}`,
			`In the body, "protocol_version" holds text where a whole number belongs.` +
				remedy},
		"a document that is not JSON": {`{"protocol_version": `,
			"The body isn't valid JSON: it ends too soon." + remedy},
		"something after the object": {`{"protocol_version": 1} {}`,
			"The body has more after its JSON value, which is all it may hold."},
	} {
		r := httptest.NewRequest(http.MethodPost, "/assistant/conversations",
			strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		var v struct {
			ProtocolVersion int `json:"protocol_version"`
		}
		if readBodyWithin(w, r, &v, 1<<10) {
			t.Fatalf("%s: read as a body", name)
		}
		var answer struct{ Error, Detail string }
		if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
			t.Fatalf("%s: the answer is not the envelope: %s", name, w.Body)
		}
		if w.Code != http.StatusBadRequest || answer.Error != "invalid_body" ||
			answer.Detail != tc.says {
			t.Errorf("%s answered %d %s, want 400 invalid_body saying %q",
				name, w.Code, w.Body, tc.says)
		}
	}
}
