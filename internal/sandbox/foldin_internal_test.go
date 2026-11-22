package sandbox

import (
	"encoding/json"
	"testing"
)

// A LATER BUILD'S KEYS IN A FOLD-IN RIDE THIS BUILD'S WRITE. A fold-in is a
// section of the record a later build may add fields to ([FoldIn.Extra]); a
// node of this build reads and writes the record whole on every write, so a key
// it does not know survives only where its decode kept it. And `fold_ins`
// rides an older build's write in the record's own Extra.
//
// Mutation: drop FoldIn's codec, and the later build's key is gone after one
// write.
func TestAFoldInKeepsALaterBuildsKeysThroughThisBuildsWrite(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"id":"0b6e8c2a-1f3d-5e4b-9a7c-2d1e3f4a5b6c","launch":"job-2","wake":"w-1","asker":"ceo",` +
		`"asker_kind":"human","conversation":"chat:C9","words":"redo the logo","cut":0,` +
		`"label":{"source":"chat_thread","trust":"colleague"},"put_to":["lead"],` +
		`"asked_at":"2026-11-21T09:00:00Z","answer":"","answered_by":"","answered_by_kind":"","escalated":"boss"}`)
	var f FoldIn
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Wake != "w-1" || f.Asker != "ceo" || !f.Open() || len(f.PutTo) != 1 {
		t.Fatalf("the fold-in decodes as %+v, want ceo's open request put to lead", f)
	}
	written, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Escalated json.RawMessage `json:"escalated"`
	}
	if err := json.Unmarshal(written, &back); err != nil {
		t.Fatal(err)
	}
	if string(back.Escalated) != `"boss"` {
		t.Errorf("this build wrote the fold-in as %s: a later build's key is lost", written)
	}
}
