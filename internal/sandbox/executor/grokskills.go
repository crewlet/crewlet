package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
	"github.com/crewlet/crewlet/internal/skillname"
)

// A HOME SKILL MAY NOT HIDE A SEAT SKILL UNSEEN. Grok Build 1.0.92 lists each
// skill of ~/.grok/skills under its rewritten name ([skillname.GrokListed]);
// where two rewrite alike, the one whose directory's name rewrites to it
// keeps the name, and the other falls back to its rewritten directory name or,
// that taken too, is dropped with no word, at launch and mid-run (measured: a
// home `late-foo` hid the seat's `late:foo` whichever was written first). A
// seat skill's directory is its name, so a home skill whose name and
// directory both rewrite to the seat's hides it, and the run is refused before
// it starts, naming the home skill and the seat skill ([grokSkillsHidden]).
// Where the home's directory only rewrites to it, which of the two keeps the
// name was not measured, and the run is refused there too.

// grokHomeSkillMark opens each home skill in [grokHomeSkillsCommand]'s answer:
// its directory's name; a `name:` line of its frontmatter may follow.
const grokHomeSkillMark = "crewlet-skill "

// grokHomeSkillsCommand is the shell printing, for each skill of the box
// home's ~/.grok/skills other than the run's own (`crewlet:*`, which the run
// writes afresh), the mark and its directory's name, then the `name:` line of
// its frontmatter where it has one.
func grokHomeSkillsCommand(paths Paths) string {
	return `for f in ` + shellword.Quote(paths.GrokSkills()) + `/*/SKILL.md; do [ -f "$f" ] || continue; ` +
		`d=${f%/SKILL.md}; d=${d##*/}; case "$d" in ` + shellword.Quote(sandbox.SkillNamespace+":") + `*) continue ;; esac; ` +
		`printf '` + grokHomeSkillMark + `%s\n' "$d"; ` +
		`sed -n '1{/^---[[:space:]]*$/!q;};2,/^---[[:space:]]*$/{/^name:/p;}' "$f" | head -n 1; done; exit 0`
}

// grokSkillsHidden refuses a run handed skills where a skill of the box
// home's ~/.grok/skills would hide one of them from Grok's listing.
func grokSkillsHidden(ctx context.Context, box sandbox.Sandbox, skills []sandbox.Skill, paths Paths) error {
	if len(skills) == 0 {
		return nil
	}
	res, err := box.Exec(ctx, grokHomeSkillsCommand(paths), sandbox.ExecOptions{})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
	}
	if err != nil {
		return fmt.Errorf("executor: reading the box home's Grok Build skills: %w", err)
	}
	var said []string
	for _, h := range grokHomeSkills(res.Stdout) {
		listed, ok := skillname.GrokListed(h.name, h.named, h.dir)
		if !ok || skillname.Grok(h.dir) != listed {
			continue
		}
		for _, s := range skills {
			if seat, _ := skillname.GrokListed(s.Listed(), true, s.Listed()); seat == listed {
				said = append(said, fmt.Sprintf("%s/%s/SKILL.md, listed as %s, hides the seat skill %s",
					paths.GrokSkills(), h.dir, listed, s.Listed()))
			}
		}
	}
	if len(said) == 0 {
		return nil
	}
	return fmt.Errorf("executor: a skill of the box home would hide one of the run's from Grok Build, which "+
		"lists one skill a name and drops the other with no word: %s; rename or remove that skill",
		strings.Join(said, "; "))
}

// grokHomeSkill is one skill of [grokHomeSkillsCommand]'s answer.
type grokHomeSkill struct {
	dir, name string
	named     bool
}

// grokHomeSkills parses out, [grokHomeSkillsCommand]'s answer: a `name:`
// value's quotes taken off as YAML does (measured: quoted and plain alike).
func grokHomeSkills(out string) []grokHomeSkill {
	var skills []grokHomeSkill
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\r\n")
		if dir, ok := strings.CutPrefix(line, grokHomeSkillMark); ok {
			skills = append(skills, grokHomeSkill{dir: dir})
			continue
		}
		value, ok := strings.CutPrefix(line, "name:")
		if !ok || len(skills) == 0 {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		skills[len(skills)-1].name, skills[len(skills)-1].named = value, true
	}
	return skills
}
