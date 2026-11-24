package executor

import (
	"encoding/json"
	"testing"
)

// A CHECKOUT SPEC REACHES THE HELPER IN THE SHAPE IT READS: Codex's slot of
// names is the list it always was, byte for byte, and a tree slot the object
// internal/notehelper's instructionSlot reads.
//
// Mutation: write a slot of names as an object, or drop the tree's scope, and
// a case goes red.
func TestACheckoutSpecReachesTheHelperInTheShapeItReads(t *testing.T) {
	t.Parallel()
	rules := checkoutSlot{Tree: &checkoutTree{Tree: ".claude/rules", Suffix: ".md",
		Scoped: &checkoutScope{Keys: []string{"applyTo", "paths"}, Whole: []string{"**", "**/*", "*"}}}}
	cases := []struct {
		name string
		spec checkoutSpec
		want string
	}{
		{"codex", codexCheckouts(1), `[["AGENTS.override.md","AGENTS.md"]]`},
		{"a tree after names", checkoutSpec{Instructions: []checkoutSlot{{Names: []string{"AGENTS.md"}}, rules}},
			`[["AGENTS.md"],{"tree":".claude/rules","suffix":".md","scoped":{"keys":["applyTo","paths"],"whole":["**","**/*","*"]}}]`},
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
