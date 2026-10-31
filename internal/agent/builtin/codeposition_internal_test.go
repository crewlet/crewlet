package builtin

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY CODE WRITE STATES ITS OUTCOME AND ITS POSITION AS THE ACT TRANSPORT
// READS THEM: `outcome`, and `position` as the string
// `<stream>@<generation>:<sequence>`, the two keys every write tool's answer
// carries for read-your-writes. The act transport lifts them beside the
// answer (operator.receiptOf) by decoding the answer into a string each, and
// the dashboard raises its read floor on the code log to that position — so a
// position spelled as the position's object made the whole decode fail: the
// outcome went unread (a pending write reported applied) and the reads after
// the press were asked at no floor, drawn from before it.
//
// Decoded here exactly as the transport decodes them, for every answer a code
// write tool gives.
//
// Mutation: spell any answer's position as statelog.Position again → red.
func TestEveryCodeWriteStatesItsOutcomeAndPositionAsTheActTransportReadsThem(t *testing.T) {
	t.Parallel()
	at := statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: 42}
	pending, text := statelog.OutcomePending, positionText(at)
	for name, answer := range map[string]any{
		"a change's lifecycle": codeChangeWriteAnswer{Outcome: pending, Position: text},
		"a conversation write": codeConversationAnswer{Outcome: pending, Position: text},
		"applied suggestions":  codeAppliedAnswer{Outcome: pending, Position: text},
		"an engine write":      codeBranchAnswer{Outcome: pending, Position: text},
		"a repository's write": codeRepoWriteAnswer{Outcome: pending, Position: text},
	} {
		out, err := json.Marshal(answer)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// THE TRANSPORT'S OWN READING (operator.receiptOf).
		var stated struct {
			Outcome  *string `json:"outcome"`
			Position *string `json:"position"`
		}
		if err := json.Unmarshal(out, &stated); err != nil {
			t.Errorf("%s answers %s, which the act transport cannot read: %v", name, out, err)
			continue
		}
		if stated.Outcome == nil || *stated.Outcome != string(pending) {
			t.Errorf("%s states its outcome as %v, want %q", name, stated.Outcome, pending)
		}
		if stated.Position == nil || *stated.Position != at.String() {
			t.Errorf("%s states its position as %v, want %q", name, stated.Position, at.String())
		}
	}
	if positionText(statelog.Position{}) != "" {
		t.Error("a write that landed nowhere states a position")
	}
}
