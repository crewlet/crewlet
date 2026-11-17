package docsgate

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// claudeMDBudget is what CLAUDE.md may weigh. Every Claude Code session and
// every subagent working in this tree starts with it, and every later request
// of that context sends it again: at 67 KB it was some 23k tokens of each
// agent's starting context, injected 1,278 times in the week to 2026-11-17.
// The budget is the restructured file's size with a little room, so text that
// only some changes need goes where only they load it — a rule under
// .claude/rules/, scoped by its paths, or a skill under .claude/skills/.
const claudeMDBudget = 30 * 1024

// ruleBudget bounds one rule. A rule loads with the first file it names and
// stays in that context, so a broad one (architecture.md loads with any file
// under internal/) is close to always loaded for whoever writes code.
const ruleBudget = 16 * 1024

// skillDescriptionLimit is where Claude Code cuts a skill's description (with
// its when_to_use) in the listing that sits in every context. The description
// is all of a skill Claude sees until the skill is used, so it is how Claude
// decides to use it, and a cut one decides on half a sentence.
const skillDescriptionLimit = 1536

// worktreeTwin is where Claude Code keeps the worktrees it makes inside a
// checkout. A rule's pattern is matched against a path relative to the
// directory holding the rule's .claude folder, and a session started in the
// primary checkout reads and edits a task's files through such a worktree, so
// a rule scoped to internal/store/** alone never loads for that work.
const worktreeTwin = ".claude/worktrees/*/"

// TestTheAlwaysLoadedInstructionsStayWithinTheirBudget holds CLAUDE.md to
// claudeMDBudget.
func TestTheAlwaysLoadedInstructionsStayWithinTheirBudget(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > claudeMDBudget {
		t.Errorf("CLAUDE.md is %d bytes, over its budget of %d: move what only some changes need into a .claude/rules/ file "+
			"scoped to the paths those changes touch, or into a .claude/skills/ procedure", len(raw), claudeMDBudget)
	}
}

// TestEveryRuleLoadsOnlyWithTheFilesItNames holds each .claude/rules/ file to
// a paths list that names files in this tree. A rule without one loads at
// launch exactly as CLAUDE.md does, outside its budget; a pattern that matches
// nothing never loads its rule, silently, from the day its directory moved.
func TestEveryRuleLoadsOnlyWithTheFilesItNames(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	tree := treeFiles(t, root)
	rules := instructionFiles(t, root, ".claude/rules", ".md")
	if len(rules) == 0 {
		t.Fatal("found no .claude/rules/*.md, so this gate would pass on nothing")
	}
	for _, rule := range rules {
		raw, err := os.ReadFile(filepath.Join(root, rule))
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > ruleBudget {
			t.Errorf("%s is %d bytes, over the %d a rule may weigh: split it by the paths its parts need", rule, len(raw), ruleBudget)
		}
		var front struct {
			Paths []string `yaml:"paths"`
		}
		if err := frontMatter(raw, &front); err != nil {
			t.Errorf("%s: %v", rule, err)
			continue
		}
		if len(front.Paths) == 0 {
			t.Errorf("%s names no paths, so it loads at launch like CLAUDE.md, outside its budget: scope it, or move it into CLAUDE.md", rule)
			continue
		}
		roots, twins := map[string]bool{}, map[string]bool{}
		for _, p := range front.Paths {
			if rest, ok := strings.CutPrefix(p, worktreeTwin); ok {
				twins[rest] = true
				continue
			}
			roots[p] = true
			alts, err := expandBraces(p)
			if err != nil {
				t.Errorf("%s: pattern %q: %v", rule, p, err)
				continue
			}
			for _, alt := range alts {
				matches, err := globMatcher(alt)
				if err != nil {
					t.Errorf("%s: pattern %q: %v", rule, alt, err)
					continue
				}
				if !slices.ContainsFunc(tree, matches) {
					t.Errorf("%s: %q matches no file in the tree, so it never loads the rule: fix the pattern or drop it", rule, alt)
				}
			}
		}
		for _, p := range sortedKeys(roots) {
			if !twins[p] {
				t.Errorf("%s: %q has no twin %q, so the rule never loads for a task's files in a worktree", rule, p, worktreeTwin+p)
			}
		}
		for _, p := range sortedKeys(twins) {
			if !roots[p] {
				t.Errorf("%s: %q twins no pattern of the rule's own: add %q or drop the twin", rule, worktreeTwin+p, p)
			}
		}
	}
}

// TestEverySkillSaysWhenToUseIt holds each .claude/skills/<name>/SKILL.md to
// its directory's name and a description Claude Code shows whole.
func TestEverySkillSaysWhenToUseIt(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	skills := instructionFiles(t, root, ".claude/skills", "SKILL.md")
	if len(skills) == 0 {
		t.Fatal("found no .claude/skills/*/SKILL.md, so this gate would pass on nothing")
	}
	for _, skill := range skills {
		raw, err := os.ReadFile(filepath.Join(root, skill))
		if err != nil {
			t.Fatal(err)
		}
		var front struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
			WhenToUse   string `yaml:"when_to_use"`
		}
		if err := frontMatter(raw, &front); err != nil {
			t.Errorf("%s: %v", skill, err)
			continue
		}
		if dir := path.Base(path.Dir(skill)); front.Name != dir {
			t.Errorf("%s: name %q is not its directory %q, so the skill answers to two names", skill, front.Name, dir)
		}
		if strings.TrimSpace(front.Description) == "" {
			t.Errorf("%s has no description, so Claude cannot tell when to use it", skill)
		}
		if n := len([]rune(front.Description + front.WhenToUse)); n > skillDescriptionLimit {
			t.Errorf("%s: description and when_to_use run %d characters, past the %d Claude Code shows", skill, n, skillDescriptionLimit)
		}
	}
}

// TestCLAUDEMDNamesEveryRuleAndSkill holds CLAUDE.md's table of rules and
// skills to .claude/ both ways: a rule CLAUDE.md does not name is found only
// by the files that load it, and a name whose file is gone sends a reader
// nowhere.
func TestCLAUDEMDNamesEveryRuleAndSkill(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	raw, err := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]bool{}
	for _, m := range instructionRefRE.FindAllString(string(raw), -1) {
		named[m] = true
	}
	present := map[string]bool{}
	for _, rule := range instructionFiles(t, root, ".claude/rules", ".md") {
		present[rule] = true
	}
	for _, skill := range instructionFiles(t, root, ".claude/skills", "SKILL.md") {
		present[path.Dir(skill)+"/"] = true
	}
	for _, p := range sortedKeys(present) {
		if !named[p] {
			t.Errorf("CLAUDE.md does not name %s: add it to \"Where the Rest Lives\"", p)
		}
	}
	for _, p := range sortedKeys(named) {
		if !present[p] {
			t.Errorf("CLAUDE.md names %s, which does not exist", p)
		}
	}
}

var instructionRefRE = regexp.MustCompile(`\.claude/(?:rules/[a-z0-9-]+\.md|skills/[a-z0-9-]+/)`)

// instructionFiles is every file under dir (relative to root) whose name ends
// in suffix, as slash paths from root.
func instructionFiles(t *testing.T, root, dir, suffix string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, suffix) {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// treeFiles is every file of this tree, as slash paths from root: the
// sourcetree walk, so a nested worktree's copies never satisfy a pattern.
func treeFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := sourcetree.Walk(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// frontMatter decodes the YAML between a file's leading "---" lines into v.
func frontMatter(raw []byte, v any) error {
	rest, ok := bytes.CutPrefix(raw, []byte("---\n"))
	if !ok {
		return fmt.Errorf("no front matter: the file must open with a line holding only ---")
	}
	body, _, ok := bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		return fmt.Errorf("front matter never closes with a line holding only ---")
	}
	if err := yaml.Unmarshal(body, v); err != nil {
		return fmt.Errorf("front matter: %w", err)
	}
	return nil
}

// expandBraces expands brace groups the way Claude Code does before it
// matches (`src/*.{ts,tsx}` is two patterns). A nested group is refused
// rather than expanded by a guess.
func expandBraces(p string) ([]string, error) {
	open := strings.IndexByte(p, '{')
	if open < 0 {
		if strings.ContainsRune(p, '}') {
			return nil, fmt.Errorf("a } with no {")
		}
		return []string{p}, nil
	}
	end := strings.IndexByte(p[open:], '}')
	if end < 0 {
		return nil, fmt.Errorf("a { that never closes")
	}
	end += open
	group := p[open+1 : end]
	if strings.ContainsRune(group, '{') {
		return nil, fmt.Errorf("a nested brace group")
	}
	var out []string
	for _, alt := range strings.Split(group, ",") {
		if alt == "" {
			return nil, fmt.Errorf("an empty alternative in {%s}", group)
		}
		tails, err := expandBraces(p[:open] + alt + p[end+1:])
		if err != nil {
			return nil, err
		}
		out = append(out, tails...)
	}
	return out, nil
}

// globMatcher compiles one expanded pattern under the rules Claude Code
// documents: `**` spans any number of directories, `*` any run within one,
// a pattern with no slash matches a file's name at any depth, and a leading
// slash anchors one to the base directory. A bracket expression or a `?`
// is refused, so a rule says nothing this gate reads differently.
func globMatcher(p string) (func(string) bool, error) {
	if strings.ContainsAny(p, "[]?{}") {
		return nil, fmt.Errorf("uses a bracket expression, a ? or a brace this gate does not read")
	}
	anywhere := !strings.Contains(strings.TrimPrefix(p, "/"), "/")
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	var re strings.Builder
	re.WriteString("^")
	for i, seg := range segs {
		last := i == len(segs)-1
		if seg == "**" {
			if last {
				re.WriteString(".*")
			} else {
				re.WriteString("(?:[^/]+/)*")
			}
			continue
		}
		re.WriteString(strings.ReplaceAll(regexp.QuoteMeta(seg), `\*`, `[^/]*`))
		if !last {
			re.WriteString("/")
		}
	}
	re.WriteString("$")
	compiled, err := regexp.Compile(re.String())
	if err != nil {
		return nil, err
	}
	if anywhere && !strings.HasPrefix(p, "/") {
		return func(file string) bool { return compiled.MatchString(path.Base(file)) }, nil
	}
	return compiled.MatchString, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
