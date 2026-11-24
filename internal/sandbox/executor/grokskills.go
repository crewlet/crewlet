package executor

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"

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
// its directory's name; its frontmatter's lines follow.
const grokHomeSkillMark = "crewlet-skill "

// grokHomeSkillLong stands in [grokHomeSkillsCommand]'s answer for a
// frontmatter longer than grokFrontmatterLimit.
const grokHomeSkillLong = "crewlet-skill-long"

// grokFrontmatterLimit bounds the bytes of one home skill's frontmatter the
// guard reads: a real one is a few lines, and the answer holds every home
// skill's. Grok reads a longer one whole (measured: a name past 500 lines
// held), so past it the name is unknown and [grokSkillsHidden] fails closed.
const grokFrontmatterLimit = 64 << 10

// grokHomeSkillsCommand is the shell printing, for each skill of the box
// home's ~/.grok/skills other than the run's own (`crewlet:*`, which the run
// writes afresh), the mark and its directory's name, then the lines of its
// frontmatter where it has one, for YAML to read ([grokHomeSkills]): the
// lines between a first line `---` and the next, as Grok 1.0.92 reads them
// (measured: an empty one, one never closed, or one closed by ` ---` or
// `...` names nothing).
func grokHomeSkillsCommand(paths Paths) string {
	return `for f in ` + shellword.Quote(paths.GrokSkills()) + `/*/SKILL.md; do [ -f "$f" ] || continue; ` +
		`d=${f%/SKILL.md}; d=${d##*/}; case "$d" in ` + shellword.Quote(sandbox.SkillNamespace+":") + `*) continue ;; esac; ` +
		`printf '` + grokHomeSkillMark + `%s\n' "$d"; ` +
		`LC_ALL=C awk -v max=` + strconv.Itoa(grokFrontmatterLimit) + ` 'NR == 1 { if ($0 !~ /^---[[:space:]]*$/) exit; next } ` +
		`/^---[[:space:]]*$/ { closed = 1; exit } { text = text $0 "\n"; if (length(text) > max) { long = 1; exit } } ` +
		`END { if (long) print "` + grokHomeSkillLong + `"; else if (closed) printf "%s", text }' "$f"; done; exit 0`
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
		if h.long {
			for _, s := range skills {
				if seat, _ := skillname.GrokListed(s.Listed(), true, s.Listed()); skillname.Grok(h.dir) == seat {
					said = append(said, fmt.Sprintf("%s/%s/SKILL.md, whose frontmatter runs past the %d bytes the "+
						"guard reads, so the name Grok lists it by is unknown, may hide the seat skill %s",
						paths.GrokSkills(), h.dir, grokFrontmatterLimit, s.Listed()))
				}
			}
			continue
		}
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
	dir, name   string
	named, long bool
}

// grokHomeSkills parses out, [grokHomeSkillsCommand]'s answer: each skill's
// frontmatter read as YAML, as Grok reads it, its `name` a string where it
// has one (measured: quoted and plain alike). Frontmatter YAML cannot read
// names nothing, so the skill keeps its directory's name.
func grokHomeSkills(out string) []grokHomeSkill {
	var skills []grokHomeSkill
	var front []string
	for line := range strings.Lines(out) {
		if dir, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), grokHomeSkillMark); ok {
			skills = append(skills, grokHomeSkill{dir: dir})
			front = append(front, "")
			continue
		}
		if len(front) > 0 {
			front[len(front)-1] += line
		}
	}
	for i := range skills {
		if front[i] == grokHomeSkillLong+"\n" {
			skills[i].long = true
			continue
		}
		var head struct {
			Name any `yaml:"name"`
		}
		if yaml.Unmarshal([]byte(front[i]), &head) != nil {
			continue
		}
		if name, ok := head.Name.(string); ok {
			skills[i].name, skills[i].named = name, true
		}
	}
	return skills
}

// grokSkillsSwitchedOff refuses a run handed skills where the box home's own
// settings (boxOwn, [grokBoxConfig]) switch one of them off: `[skills]
// disabled` naming its listed name (as [skillname.GrokListed] gives it; the
// raw `crewlet:<name>` matches nothing), or `[skills] ignore` naming its
// directory, `~/` for the home (both measured on 1.0.92; an ignore entry as
// a glob is read so too, unmeasured). Settings that do not read as TOML the
// merge refuses ([grokMergeConfig]).
func grokSkillsSwitchedOff(boxOwn []byte, skills []sandbox.Skill, paths Paths) error {
	if len(skills) == 0 {
		return nil
	}
	var settings struct {
		Skills struct {
			Disabled []string `toml:"disabled"`
			Ignore   []string `toml:"ignore"`
		} `toml:"skills"`
	}
	if decoded := toml.Unmarshal(boxOwn, &settings) == nil; !decoded {
		return nil
	}
	var said []string
	for _, s := range skills {
		listed, _ := skillname.GrokListed(s.Listed(), true, s.Listed())
		dir := paths.GrokSkills() + "/" + s.Listed()
		for _, d := range settings.Skills.Disabled {
			if d == listed {
				said = append(said, fmt.Sprintf("its [skills] disabled names %q, the seat skill %s", d, s.Listed()))
			}
		}
		for _, i := range settings.Skills.Ignore {
			at := i
			if rest, ok := strings.CutPrefix(i, "~/"); ok {
				at = paths.Home + "/" + rest
			}
			at = path.Clean(at)
			if ok, _ := path.Match(at, dir); ok || at == dir {
				said = append(said, fmt.Sprintf("its [skills] ignore names %q, the seat skill %s's directory", i,
					s.Listed()))
			}
		}
	}
	if len(said) == 0 {
		return nil
	}
	return fmt.Errorf("executor: the box home's Grok Build settings (%s) switch off a skill of the run's, which "+
		"Grok would then go without, with no word: %s; remove that entry", paths.GrokConfig(),
		strings.Join(said, "; "))
}
