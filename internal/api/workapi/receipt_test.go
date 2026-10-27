package workapi_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/workapi"
	"github.com/crewlet/crewlet/internal/tools"
)

// A WORK RECEIPT IS THE TOOL'S JSON AND NEVER THE MODEL'S NOTE (ADR-0059): the
// note on a call's arguments is worded for the model that
// wrote the call, and a person reads this receipt, so it carries neither the
// note nor a field for it.
//
// Mutation: carry the note as `arguments_note` and its text is in the receipt.
func TestAWorkReceiptCarriesNoModelNote(t *testing.T) {
	t.Parallel()
	const note = "`blocking.extra` is not an argument"
	receipt, err := workapi.ReceiptOf(tools.Result{Output: `{"key": "ENG-9", "outcome": "applied"}`,
		Note: note})
	if err != nil || receipt["key"] != "ENG-9" {
		t.Fatalf("the receipt is %v (%v), want the tool's JSON", receipt, err)
	}
	raw, _ := json.Marshal(receipt)
	if len(receipt) != 2 || strings.Contains(string(raw), "is not an argument") {
		t.Errorf("a person's receipt carries the model's note: %s", raw)
	}
}
