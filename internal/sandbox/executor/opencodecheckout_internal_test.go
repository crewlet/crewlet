package executor

import (
	"slices"
	"testing"
)

// A RUN WHOSE ENVIRONMENT SWITCHES OPENCODE'S CLAUDE CODE FILES OFF HANDS
// NONE OF THEM: OpenCode 2.3.2 reads a checkout's CLAUDE.md and .claude/skills
// unless OPENCODE_DISABLE_CLAUDE_CODE (both), _PROMPT (the CLAUDE.md) or
// _SKILLS (the skills) is "1" or "true" in any case (measured, B2b's cc rows;
// its flag reader), so the spec drops that slot name or skill directory, and
// the home's .claude/skills from the names no copy takes; any other value, a
// padded "1" included, leaves both.
//
// Mutation: ignore a switch, read the umbrella for one half only, or take
// any non-empty value as set, and a row goes red.
func TestARunWhoseEnvironmentSwitchesClaudeCodeOffHandsNoneOfIt(t *testing.T) {
	t.Parallel()
	p := Paths{Home: "/home/box"}
	for name, tc := range map[string]struct {
		env            map[string]string
		claudeMD, dirs bool
	}{
		"none":         {nil, true, true},
		"all":          {map[string]string{"OPENCODE_DISABLE_CLAUDE_CODE": "1"}, false, false},
		"prompt, TRUE": {map[string]string{"OPENCODE_DISABLE_CLAUDE_CODE_PROMPT": "TRUE"}, false, true},
		"skills":       {map[string]string{"OPENCODE_DISABLE_CLAUDE_CODE_SKILLS": "true"}, true, false},
		"a padded 1":   {map[string]string{"OPENCODE_DISABLE_CLAUDE_CODE": " 1"}, true, true},
		"no":           {map[string]string{"OPENCODE_DISABLE_CLAUDE_CODE": "no"}, true, true},
	} {
		spec := openCodeCheckouts(p, tc.env)
		names := spec.Instructions[0].Names
		if got := slices.Contains(names, "CLAUDE.md"); got != tc.claudeMD || names[0] != "AGENTS.md" {
			t.Errorf("%s: the instruction names are %q, want CLAUDE.md %v", name, names, tc.claudeMD)
		}
		has := func(dirs []checkoutSkillDir, dir string) bool {
			return slices.ContainsFunc(dirs, func(d checkoutSkillDir) bool { return d.Dir == dir })
		}
		if has(spec.SkillDirs, ".claude/skills") != tc.dirs || has(spec.Listed, "/home/box/.claude/skills") != tc.dirs ||
			!has(spec.SkillDirs, ".agents/skills") || !has(spec.Listed, "/home/box/.config/opencode/skills") {
			t.Errorf("%s: the skill directories are %v, listed %v, want .claude/skills %v", name, spec.SkillDirs,
				spec.Listed, tc.dirs)
		}
	}
}
