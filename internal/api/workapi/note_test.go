package workapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/modelnote"
)

// THE WORK API TELLS THE MODEL THE NOTE AND NEVER THE PERSON (ADR-0059,
// ADR-0060), through the route itself: an edit that succeeds while one of its
// arguments was not read as sent (a key `checklist` does not take) answers the
// person the receipt and nothing of the note, and the note is told to the
// model that made the request, in process (internal/modelnote).
//
// Mutation: drop TellNote from answerTool and the model hears nothing; carry
// the note into the receipt and the body names it.
func TestTheWorkAPITellsTheModelTheNoteAndNeverThePerson(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	var heard *modelnote.Note
	expecting := func(ctx context.Context) context.Context {
		ctx, heard = modelnote.Expect(as(admin("ana"))(ctx))
		return ctx
	}
	got := r.do(expecting, http.MethodPatch, "/worklet/items/ENG-1", map[string]any{
		"checklist": map[string]any{"op": "remove_list", "list": "l-1", "bogus": "x"}})
	if got.status != http.StatusOK {
		t.Fatalf("the edit answered %d: %v", got.status, got.body)
	}
	if !strings.Contains(heard.Said(), "checklist.bogus") {
		t.Errorf("the model heard %q, want the note on `checklist.bogus`", heard.Said())
	}
	if raw, _ := json.Marshal(got.body); strings.Contains(string(raw), "bogus") {
		t.Errorf("the person's answer carries the model's note: %s", raw)
	}
}
