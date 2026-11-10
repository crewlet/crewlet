package sandbox

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

// A RUN'S LOG KEEPS WHAT ITS CALLS COMMITTED, each kind once, after the bound
// has dropped the call that committed it, and across the wire.
//
// Mutation: stop appendCall folding a call's kinds into the log, and the
// dropped call's kind is lost.
func TestARunsLogKeepsWhatItsCallsCommitted(t *testing.T) {
	t.Parallel()
	var log BridgeLog
	// PAST THE HEAD the bound keeps ([appendBounded]), so the committing
	// calls are among the ones it drops.
	head := MaxBridgeCalls/2 + 10
	for i := 1; i <= head; i++ {
		log.appendCall(BridgeCall{Name: fmt.Sprintf("read%03d", i), Seq: i}, nil)
	}
	log.appendCall(BridgeCall{Name: "create_work_item", Seq: head + 1, Committed: []string{"item"}}, nil)
	log.appendCall(BridgeCall{Name: "comment_on_work_item", Seq: head + 2,
		Committed: []string{"comment", "item"}}, nil)
	for i := head + 3; i <= MaxBridgeCalls+head+60; i++ {
		log.appendCall(BridgeCall{Name: fmt.Sprintf("read%03d", i), Seq: i}, nil)
	}
	if slices.ContainsFunc(log.Calls, func(c BridgeCall) bool { return c.Name == "create_work_item" }) {
		t.Fatal("the bound kept the committing call, so this case proves nothing about a dropped one")
	}
	if got := log.Changes(); !slices.Equal(got, []string{"item", "comment"}) {
		t.Errorf("changes = %v, want the item and the comment, each once", got)
	}

	raw, err := json.Marshal(log)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var read BridgeLog
	if err := json.Unmarshal(raw, &read); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := read.Changes(); !slices.Equal(got, []string{"item", "comment"}) {
		t.Errorf("read back, changes = %v", got)
	}
}

// A CALL AN OLDER BUILD'S STORE APPENDED carries its kinds on the call alone,
// and the log's reader still counts them.
func TestALogReadsTheKindsItsListedCallsCarry(t *testing.T) {
	t.Parallel()
	log := BridgeLog{Calls: []BridgeCall{{Name: "post_message", Committed: []string{"chat"}}, {Name: "get_page"}}}
	if got := log.Changes(); !slices.Equal(got, []string{"chat"}) {
		t.Errorf("changes = %v, want the chat post's", got)
	}
	if got := (BridgeLog{}).Changes(); len(got) != 0 {
		t.Errorf("an empty log committed %v", got)
	}
}
