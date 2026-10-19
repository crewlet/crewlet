package builtin

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
)

// THE TOOL IS PART OF EVERY KEY A TURN DERIVES, so two tools called with the
// same arguments at the same count in one turn are two calls.
//
// [callOpKey] is the one derivation every keyed domain's tools share, and the
// arguments' digest names only what was typed: `{"channel": "eng"}` is what a
// join, a leave and a read of one room all say. Without the tool those calls
// derive one key, and what tells their writes apart is left to each store's
// own verb — the derivation's doc promises the key names the call, and this
// is what holds it. Outside a turn the key is the caller's own, whatever the
// tool.
//
// Mutation: drop the tool from callOpKey's identity and the two keys are one.
func TestTwoToolsCalledAlikeInOneTurnDeriveTwoKeys(t *testing.T) {
	t.Parallel()
	turn := &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", Calls: turnctx.NewCallLog()}
	args := map[string]any{"channel": "eng"}
	join := callOpKey(turn, "join_channel", nil, args, "")
	read := callOpKey(turn, "read_channel", nil, args, "")
	if join == "" || join == read {
		t.Fatalf("join_channel and read_channel with one argument set derived "+
			"%q and %q, want two keys", join, read)
	}
	if again := callOpKey(turn, "join_channel", nil, args, ""); again != join {
		t.Errorf("the same call derived %q and then %q, want one key", join, again)
	}
	const sent = "k0123456789abcdef"
	if got := callOpKey(nil, "join_channel", nil, args, sent); got != sent {
		t.Errorf("outside a turn the key is %q, want the caller's own %q", got, sent)
	}
}
