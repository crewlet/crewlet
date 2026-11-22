package executor

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// HandsSkills reports that an OpenCode run takes skills of its own.
func (OpenCode) HandsSkills() bool { return true }

// writeOpenCodeSkills writes each skill's SKILL.md into [Paths.OpenCodeSkills],
// in a directory named as the skill is listed ([sandbox.Skill.Listed]): OpenCode
// loads a skill by its directory's name.
func writeOpenCodeSkills(ctx context.Context, box sandbox.Sandbox, skills []sandbox.Skill, paths Paths) error {
	for _, s := range skills {
		file, err := skillFile(s.Listed(), s.Description, s.Body)
		if err != nil {
			return err
		}
		if err = box.WriteFile(ctx, paths.OpenCodeSkills()+"/"+s.Listed()+"/SKILL.md", file); err != nil {
			return fmt.Errorf("executor: writing the run's skill %q: %w", s.Listed(), err)
		}
	}
	return nil
}

// openCodeSkillsPlugin is opencode_skills.js, which reports each skill an
// OpenCode run's model loads to the run's bridge. Embedded so it matches this
// build's bridge; written as its directory's `index` module
// ([Paths.OpenCodeSkillsPlugin], [openCodeSteerEntry]).
//
//go:embed opencode_skills.js
var openCodeSkillsPlugin []byte
