package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// A QWEN CODE FIRING HANDS A CLONE'S INSTRUCTIONS, RULES AND SKILLS: Qwen Code
// 0.28.4 loads nothing of a checkout below its start directory, so the
// firing answers PostToolUse with each of its files framed as Qwen Code
// frames a context file (`--- Context from: <path> ---`), QWEN.md, AGENTS.md
// and its always-on rules in order, then its skills' listing, joined by a
// hyphen.
//
// Mutation: frame a file otherwise, drop the rules or AGENTS.md, or join
// with a colon, and the context goes red.
func TestAQwenFiringHandsAClonesInstructionsRulesAndSkills(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	spec := checkoutSpec{Instructions: append(names([]string{"QWEN.md"}, []string{"AGENTS.md"}), qwenRules()),
		SkillDirs: []skillDirSlot{{Dir: ".qwen/skills"}, {Dir: ".agents/skills"}}, Skills: r.skills,
		Copied: filepath.Join(filepath.Dir(r.skills), "copied"), Reserved: []string{"crewlet"}, SkillSep: "-"}
	r.launchWith(t, spec, false, "")
	r.add(t, map[string]string{"late/.git/": "", "late/QWEN.md": "LATE-QWEN", "late/AGENTS.md": "LATE-AGENTS",
		"late/.qwen/rules/r.md": "LATE-RULE", "late/.qwen/rules/s.md": "---\npaths: [\"src/**\"]\n---\nSCOPED",
		"late/.qwen/skills/fix/SKILL.md": "---\nname: fix\ndescription: Fixes.\n---\n"})
	event, got := r.fireAs(t, runQwenNested)
	late := filepath.Join(r.workdir(t), "late")
	frame := func(file, text string) string {
		return "--- Context from: " + filepath.Join(late, file) + " ---\n" + text + "\n--- End of Context from: " +
			filepath.Join(late, file) + " ---"
	}
	want := strings.Join([]string{frame("QWEN.md", "LATE-QWEN"), frame("AGENTS.md", "LATE-AGENTS"),
		frame(".qwen/rules/r.md", "LATE-RULE"), "# Skills of " + late + "\n\nEach is a set of local instructions to " +
			"follow, stored in its `SKILL.md` file; to use one, read that file.\n- late-fix: Fixes. (file: " + r.skills +
			"/late-fix/SKILL.md)"}, "\n\n")
	if event != "PostToolUse" || got != want {
		t.Errorf("the firing answered %s with\n%q\nwant PostToolUse with\n%q", event, got, want)
	}
}

// A QWEN CODE FIRING HANDS ITS FILES WITHIN ITS BOUND, FRAMES COUNTED: each
// file whose text, its path three times and 64 bytes fit what is left of the
// firing's FiringLimit is framed whole, in order, and one past it is left out
// whole, never cut, its path named in the answer and in the skip log, for the
// model to read there; a file whose text alone would fit but not with its
// frame is left out too.
//
// Mutation: hand past the bound, count the text alone, cut the file, or say
// nothing of it, and the answer or the log goes red.
func TestAQwenFiringHandsItsFilesWithinItsBound(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	late := filepath.Join(r.workdir(t), "late")
	cost := func(file, text string) int { return len(text) + 3*len(filepath.Join(late, file)) + 64 }
	qwen, agents, rule := strings.Repeat("Q", 15), strings.Repeat("A", 40), "RULE"
	spec := checkoutSpec{Instructions: append(names([]string{"QWEN.md"}, []string{"AGENTS.md"}), qwenRules()),
		FiringLimit: limited(cost("QWEN.md", qwen) + cost(".qwen/rules/r.md", rule)), Reserved: []string{"crewlet"},
		SkillSep: "-"}
	if len(agents) > cost(".qwen/rules/r.md", rule) || cost("AGENTS.md", agents) <= cost(".qwen/rules/r.md", rule) {
		t.Fatal("AGENTS.md's text must fit the room the rule takes and its frame must not")
	}
	r.launchWith(t, spec, false, "")
	r.add(t, map[string]string{"late/.git/": "", "late/QWEN.md": qwen, "late/AGENTS.md": agents,
		"late/.qwen/rules/r.md": rule})
	_, got := r.fireAs(t, runQwenNested)
	frame := func(file, text string) string {
		return "--- Context from: " + filepath.Join(late, file) + " ---\n" + text + "\n--- End of Context from: " +
			filepath.Join(late, file) + " ---"
	}
	line := "the instruction file " + filepath.Join(late, "AGENTS.md") + " is left out: it passes what is left of " +
		"the firing's bound on instructions; read it at that path"
	want := strings.Join([]string{frame("QWEN.md", qwen), frame(".qwen/rules/r.md", rule),
		skippedListing([]string{line})}, "\n\n")
	if got != want {
		t.Errorf("the firing answered\n%q\nwant\n%q", got, want)
	}
	if log := read(t, filepath.Join(r.state, skippedLog)); !strings.Contains(log, line) {
		t.Errorf("the skip log reads %q, want the file left out named", log)
	}
}
