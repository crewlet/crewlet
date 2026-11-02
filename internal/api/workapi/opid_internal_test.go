package workapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN OPERATION THAT CANNOT BE DERIVED IS A FAULT, ANSWERED BEFORE THE WRITE.
//
// [keyedOp] answers every error [statelog.KeyedOpID] gives — a key that
// carries no instant, a verb that is not a name — as `500 internal_error`, and
// hands its caller no id to publish. It used to read the key's instant without
// asking whether there was one, so a key outside the grammar derived an id
// minted at the epoch: published, and answered `unknown` by every ledger that
// had ever swept a row, on every attempt.
//
// Mutation: hand back the id and true whatever the error, and both rows go red.
func TestAnOperationThatCannotBeDerivedIsAFault(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ key, verb string }{
		"a key that carries no instant": {"not-an-operation-id", "purge"},
		"a verb that is not a name":     {statelog.NewOpID(time.Now(), ""), "chat.erase"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/worklet/items/t-1/purge", nil)
			opID, ok := keyedOp(w, r, c.key, c.verb, "t-1",
				map[string]any{"reason": "retention"})
			if ok || opID != "" {
				t.Fatalf("derived %q and went on, want the write refused before it "+
					"was made", opID)
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("the answer is not JSON: %v: %s", err, w.Body.String())
			}
			if w.Code != http.StatusInternalServerError ||
				body["error"] != string(httpjson.CodeInternalError) {
				t.Errorf("answered %d %v, want 500 %s", w.Code, body,
					httpjson.CodeInternalError)
			}
		})
	}
}
