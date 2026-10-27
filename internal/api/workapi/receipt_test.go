package workapi_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/workapi"
	"github.com/crewlet/crewlet/internal/tools"
)

// A WORK RECEIPT STAYS THE TOOL'S JSON WHEN THE CALL'S ARGUMENTS WERE NOTED
// (ADR-0059): the work API decodes the tool's answer, so a note on the
// arguments rides as its own field beside the receipt's, never inside the
// answer it decodes.
//
// Mutation: carry the note in the tool's Output and the receipt does not decode.
func TestANoteRidesBesideAWorkReceipt(t *testing.T) {
	t.Parallel()
	receipt, err := workapi.ReceiptOf(tools.Result{Output: `{"key": "ENG-9", "outcome": "applied"}`,
		Note: "`blocking.extra` is not an argument"})
	if err != nil || receipt["key"] != "ENG-9" || receipt["arguments_note"] != "`blocking.extra` is not an argument" {
		t.Errorf("the receipt is %v (%v), want the tool's JSON and the note beside it", receipt, err)
	}
}
