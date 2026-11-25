package executor

import (
	"encoding/json"
	"testing"
)

// A CHECKOUT SPEC REACHES THE HELPER IN THE SHAPE IT READS: Codex's slot of
// names is the list it always was, byte for byte, and Claude Code's rules a
// tree slot after its names, the object internal/notehelper's
// instructionSlot reads, naming its form.
//
// Mutation: write a slot of names as an object, or drop the tree's form or
// scope, and a case goes red.
func TestACheckoutSpecReachesTheHelperInTheShapeItReads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		spec checkoutSpec
		want string
	}{
		{"codex", codexCheckouts(1), `[["AGENTS.override.md","AGENTS.md"]]`},
		{"claude", claudeCheckouts, `[["CLAUDE.md"],[".claude/CLAUDE.md"],["CLAUDE.local.md"],` +
			`{"tree":".claude/rules","suffix":".md","form":"claude","scoped":{"keys":["paths"],"whole":["**"]}}]`},
	}
	for _, c := range cases {
		var launch struct {
			Instructions json.RawMessage `json:"instructions"`
		}
		if err := json.Unmarshal(checkoutHome{}.launchConfig(c.spec, nil), &launch); err != nil {
			t.Fatal(err)
		}
		if got := string(launch.Instructions); got != c.want {
			t.Errorf("%s: the launch config's instructions are %s, want %s", c.name, got, c.want)
		}
	}
}
