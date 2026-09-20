package ledger

import (
	"encoding/json"
	"strings"
	"testing"
)

// A turn that ASKED must not read like a turn that ANSWERED.
//
// Both end `done` and both carry whatever prose was in hand, so the decision
// alone cannot separate them — and the reader is the seat's own next turn on
// this thread, deciding what it still owes. Read as an answer, a question the
// seat asked becomes work it believes it finished.
func TestAnAskedQuestionDoesNotRenderAsAnAnswer(t *testing.T) {
	t.Parallel()
	answered := BuildSession(SessionInput{
		TurnID: "t1", Decision: "done", Reply: "Filed NIM-14.", Delivered: true,
	})
	asked := BuildSession(SessionInput{
		TurnID: "t2", Decision: "done", Reply: "Asked which repo.", Delivered: true,
		BlockedOn: "asked @founder which repo to file against; waiting on them",
	})

	if renderSession(answered) == renderSession(asked) {
		t.Fatal("a turn that asked rendered identically to one that answered")
	}
	got := renderSession(asked)
	if !strings.Contains(got, "waiting on them") {
		t.Errorf("the next turn is not told what the last one was blocked on:\n%s", got)
	}
	// And the ordinary turn stays clean: a line on every entry is a line
	// the reader learns to skip.
	if strings.Contains(renderSession(answered), "blocked") {
		t.Errorf("an unblocked turn rendered a blocked line:\n%s", renderSession(answered))
	}
}

// The account survives the round trip, because the entry is stored as a JSON
// blob and read back by a LATER turn in another process.
//
// Additive by construction: `omitempty` means rows written before the field
// existed decode with it empty and render exactly as they did.
func TestTheBlockedAccountSurvivesTheStoredBlob(t *testing.T) {
	t.Parallel()
	in := BuildSession(SessionInput{
		TurnID: "t1", Decision: "done", BlockedOn: "waiting on the security review",
	})
	blob, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Session
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.BlockedOn != in.BlockedOn {
		t.Errorf("blocked_on did not survive the blob: %q", out.BlockedOn)
	}
	// An entry with no account carries no key at all, so an older row and a
	// new unblocked one are byte-identical.
	bare, err := json.Marshal(BuildSession(SessionInput{TurnID: "t2", Decision: "done"}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(bare), "blocked_on") {
		t.Errorf("an unblocked entry wrote the key anyway: %s", bare)
	}
}
