package executor

import (
	"encoding/json"
	"testing"
)

// A CHECKOUT SPEC REACHES THE HELPER IN THE SHAPE IT READS: Codex's slot of
// names and its skill directories are the lists they always were, byte for
// byte, a tree slot and a recursive skill directory the objects
// internal/notehelper's instructionSlot and skillDirSlot read.
//
// Mutation: write a slot of names or a plain skill directory as an object, or
// drop the tree's scope or recursive, and a case goes red.
func TestACheckoutSpecReachesTheHelperInTheShapeItReads(t *testing.T) {
	t.Parallel()
	rules := checkoutSlot{Tree: &checkoutTree{Tree: ".claude/rules", Suffix: ".md",
		Scoped: &checkoutScope{Keys: []string{"applyTo", "paths"}, Whole: []string{"**", "**/*", "*"}}}}
	cases := []struct {
		name             string
		spec             checkoutSpec
		instructions, sd string
	}{
		{"codex", codexCheckouts(1), `[["AGENTS.override.md","AGENTS.md"]]`, `[".codex/skills",".agents/skills"]`},
		{"a tree after names, a recursive directory", checkoutSpec{
			Instructions: []checkoutSlot{{Names: []string{"AGENTS.md"}}, rules},
			SkillDirs:    []checkoutSkillDir{{Dir: ".pi/skills", Recursive: true}, {Dir: ".agents/skills"}}},
			`[["AGENTS.md"],{"tree":".claude/rules","suffix":".md","scoped":{"keys":["applyTo","paths"],"whole":["**","**/*","*"]}}]`,
			`[{"dir":".pi/skills","recursive":true},".agents/skills"]`},
	}
	for _, c := range cases {
		var launch struct {
			Instructions json.RawMessage `json:"instructions"`
			SkillDirs    json.RawMessage `json:"skillDirs"`
		}
		if err := json.Unmarshal(checkoutHome{}.launchConfig(c.spec, nil), &launch); err != nil {
			t.Fatal(err)
		}
		if got := string(launch.Instructions); got != c.instructions {
			t.Errorf("%s: the launch config's instructions are %s, want %s", c.name, got, c.instructions)
		}
		if got := string(launch.SkillDirs); got != c.sd {
			t.Errorf("%s: the launch config's skill directories are %s, want %s", c.name, got, c.sd)
		}
	}
}
