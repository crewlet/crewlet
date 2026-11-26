package main

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// grokSpec is the spec a Grok Build run's launch gives the core
// (internal/sandbox/executor's grokCheckouts), its skills in skills and the
// home's other vendor directories under home.
func grokSpec(skills, home string) checkoutSpec {
	var spec checkoutSpec
	for _, name := range []string{"Agents.md", "Claude.md", "CLAUDE.md", "CLAUDE.local.md", "AGENT.md", "AGENTS.md",
		".claude/CLAUDE.md", ".claude/CLAUDE.local.md"} {
		spec.Instructions = append(spec.Instructions, instructionSlot{Names: []string{name},
			slotOptions: slotOptions{Inside: true, Once: true}})
	}
	for _, dir := range []string{".grok/rules", ".claude/rules"} {
		spec.Instructions = append(spec.Instructions, instructionSlot{Tree: &instructionTree{Tree: dir, Suffix: ".md",
			Form: formGrok, Once: true}})
	}
	for _, v := range []string{".grok", ".agents", ".claude", ".cursor"} {
		spec.SkillDirs = append(spec.SkillDirs, skillDirSlot{Dir: v + "/skills", Recursive: true,
			skillWalkOptions: grokHomeWalk}, skillDirSlot{Dir: v + "/commands", skillWalkOptions: skillWalkOptions{Commands: true}})
		if v != ".grok" {
			spec.HomeSkills = append(spec.HomeSkills, skillDirSlot{Dir: filepath.Join(home, v, "skills"), Recursive: true,
				skillWalkOptions: grokHomeWalk})
		}
		spec.HomeSkills = append(spec.HomeSkills, skillDirSlot{Dir: filepath.Join(home, v, "commands"),
			skillWalkOptions: skillWalkOptions{Commands: true}})
	}
	spec.Skills, spec.Copied, spec.Reserved, spec.ListedAs = skills, filepath.Join(filepath.Dir(skills), "copied"),
		[]string{"crewlet"}, listedGrok
	return spec
}

// grokHome is the home a grokRun's spec names beside its skills.
func (r repoRun) grokHome() string { return filepath.Join(filepath.Dir(r.work), "home") }

// grokLaunch runs checkout-launch in r's working directory under Grok's
// spec, and is what it said.
func (r repoRun) grokLaunch(t *testing.T) string {
	t.Helper()
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, checkoutSpec: grokSpec(r.skills, r.grokHome())})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if err := checkoutLaunch(config, r.work, strings.NewReader(""), &errs); err != nil {
		t.Fatalf("the launch failed: %v\n%s", err, errs.String())
	}
	return errs.String()
}

// grokFire runs one grok-nested firing on input off the state the launch
// wrote: the context it handed, "" for none.
func (r repoRun) grokFire(t *testing.T, input string) string {
	t.Helper()
	var out, errs bytes.Buffer
	if code := runGrokNested(context.Background(), flags{config: filepath.Join(r.state, "nested.json")},
		strings.NewReader(input), &out, &errs, os.Getenv); code != exitDone {
		t.Fatalf("the firing exited %d: %s", code, errs.String())
	}
	if out.Len() == 0 {
		return ""
	}
	var answer struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil || answer.HookSpecificOutput.HookEventName != "PostToolUse" {
		t.Fatalf("the firing answered %s (%v)", out.String(), err)
	}
	return answer.HookSpecificOutput.AdditionalContext
}

// grokMainCall and grokSubCall are a PostToolUse input of Grok Build 1.0.92's main
// loop and of a subagent's call (measured, row k: only the latter carries
// subagentType).
const (
	grokMainCall = `{"hook_event_name":"PostToolUse","tool_name":"run_terminal_command","session_id":"m"}`
	grokSubCall  = `{"hook_event_name":"PostToolUse","tool_name":"run_terminal_command","session_id":"c",` +
		`"subagentType":"general-purpose"}`
)

// link makes a symbolic link at path, below root, to target.
func link(t *testing.T, root, path, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, path)); err != nil {
		t.Fatal(err)
	}
}

// A CHECKOUT'S INSTRUCTIONS ARE WHAT A TRUSTED GROK SESSION STARTED IN IT
// LOADS (rows g4r, g4x, Grok Build 1.0.92): every one of its eight names that
// exists, in its order, a link followed only inside the checkout (Agents.md
// in, CLAUDE.md out); then .grok/rules and .claude/rules flat, `*.md` in any
// case, dot files too, in byte order, a file link inside followed, one out
// not, a file an earlier slot handed not again (lin.md), a whitespace file
// nothing, no subdirectory and no other suffix; whole.
//
// Mutation: follow a name's link out, walk a rules subdirectory, skip dot
// files, match the suffix's case, or hand a file twice, and the list goes red.
func TestAGrokCheckoutHandsWhatATrustedGrokSessionLoads(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	writeTree(t, outside, map[string]string{"out.md": "OUT\n"})
	writeTree(t, root, map[string]string{"sub/AGENTS.md": "SUB\n", "Claude.md": "CLAUDE-CAMEL\n",
		"CLAUDE.local.md": "LOCAL\n", "AGENT.md": "AGENT\n", "AGENTS.md": "AGENTS\n", "agents.md": "LOWER\n",
		".claude/CLAUDE.md": "DOT-CLAUDE\n", "GROK.md": "GROK\n", "docs/inner.md": "INNER\n",
		".grok/rules/b.md": "b\n", ".grok/rules/A.md": "A\n", ".grok/rules/B2.md": "B2\n", ".grok/rules/a2.md": "a2\n",
		".grok/rules/c.MD": "c\n", ".grok/rules/d.txt": "TXT\n", ".grok/rules/.hid.md": "hid\n",
		".grok/rules/emp.md": "  \n\n", ".grok/rules/sub/e.md": "SUBRULE\n", ".grok/rules/.dotdir/g.md": "DOTDIR\n",
		".claude/rules/x.md": "x\n", ".claude/rules/csub/y.md": "CSUB\n"})
	link(t, root, "Agents.md", "sub/AGENTS.md")
	link(t, root, "CLAUDE.md", filepath.Join(outside, "out.md"))
	link(t, root, ".grok/rules/lin.md", "../../sub/AGENTS.md")
	link(t, root, ".grok/rules/lin2.md", "../../docs/inner.md")
	link(t, root, ".grok/rules/lout.md", filepath.Join(outside, "out.md"))
	link(t, root, ".grok/rules/ldir", "../../.claude/rules/csub")
	said := &skips{stderr: &bytes.Buffer{}}
	parts, _ := instructionParts(root, 1<<30, grokSpec("", "").Instructions, said)
	var got []string
	for _, p := range parts {
		rel, _ := filepath.Rel(root, p.path)
		got = append(got, rel+"="+strings.TrimSpace(p.text))
	}
	want := []string{"Agents.md=SUB", "Claude.md=CLAUDE-CAMEL", "CLAUDE.local.md=LOCAL", "AGENT.md=AGENT",
		"AGENTS.md=AGENTS", ".claude/CLAUDE.md=DOT-CLAUDE", ".grok/rules/.hid.md=hid", ".grok/rules/A.md=A",
		".grok/rules/B2.md=B2", ".grok/rules/a2.md=a2", ".grok/rules/b.md=b", ".grok/rules/c.MD=c",
		".grok/rules/lin2.md=INNER", ".claude/rules/x.md=x"}
	if !slices.Equal(got, want) || len(said.lines) > 0 {
		t.Errorf("the checkout hands\n%q\nwant\n%q\n(said %q)", got, want, said.lines)
	}
}

// A CHECKOUT'S SKILLS ARE THOSE A TRUSTED GROK SESSION STARTED IN IT LISTS
// (rows g4s, g4x, g4y, Grok Build 1.0.92): .grok, .agents, .claude and .cursor
// skills walked on below a skill, dot directories and node_modules in, 6
// deep; their commands' `*.md` files (dot files too, `.MD` no, no subfolder);
// a skill reached twice by real path once; a no-frontmatter one too; and of
// those listed alike, the one whose own name is the name, or a command, first
// (a skill before the command of its level), another falling back to its own
// name, else not listed; a link out of the checkout left out (Grok follows
// it: the documented gap). Mutation: stop at a skill, skip dot directories or
// node_modules, walk 7 deep, take `.MD` or a command subfolder, settle names
// first come first served, or follow a link out, and the list goes red.
func TestAGrokCheckoutsSkillsAreThoseATrustedGrokSessionLists(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	sk := func(name string) string { return "---\nname: " + name + "\ndescription: D.\n---\nbody\n" }
	writeTree(t, outside, map[string]string{"o/SKILL.md": sk("out")})
	writeTree(t, root, map[string]string{
		".grok/skills/n1/inner/SKILL.md": sk("n1-inner"), ".grok/skills/n2/SKILL.md": sk("n2"),
		".grok/skills/n2/below/SKILL.md": sk("n2-below"), ".grok/skills/node_modules/nm/SKILL.md": sk("nm"),
		".grok/skills/.dot/dd/SKILL.md": sk("dd"), ".grok/skills/nofm/SKILL.md": "no frontmatter\n",
		".grok/skills/d6/l2/l3/l4/l5/l6/SKILL.md": sk("dep6"), ".grok/skills/d7/l2/l3/l4/l5/l6/l7/SKILL.md": sk("dep7"),
		".grok/skills/pdir/SKILL.md": sk("shared"), ".grok/skills/dup/SKILL.md": sk("dup"),
		".grok/commands/dup.md": "---\ndescription: C.\n---\n", ".grok/commands/cmdclash.md": sk("late-claude"),
		".grok/commands/.dotcmd.md": "body\n", ".grok/commands/Up.MD": "body\n", ".grok/commands/sub/c.md": "body\n",
		".agents/skills/late-agents/SKILL.md": sk("late-agents"), ".agents/skills/shared/SKILL.md": sk("shared"),
		".agents/skills/other-dir/SKILL.md": sk("Shared X"), ".claude/skills/Shared_X/SKILL.md": sk("shared-x"),
		".claude/skills/late-claude/SKILL.md": sk("late-claude"), ".cursor/commands/cu.md": "body\n",
		".codex/skills/cx/SKILL.md": sk("cx"),
	})
	link(t, root, ".grok/skills/lk", "../../.agents/skills/late-agents")
	link(t, root, ".grok/skills/lo", filepath.Join(outside, "o"))
	spec := grokSpec("", "")
	dirs, over := checkoutSkillDirs(root, spec)
	got := skillNames(t, root, grokSessionSkills(dirs))
	want := []string{"dd=.grok/skills/.dot/dd", "l6=.grok/skills/d6/l2/l3/l4/l5/l6", "dup=.grok/skills/dup",
		"lk=.grok/skills/lk", "inner=.grok/skills/n1/inner", "n2=.grok/skills/n2", "below=.grok/skills/n2/below",
		"nm=.grok/skills/node_modules/nm", "nofm=.grok/skills/nofm", "pdir=.grok/skills/pdir",
		".dotcmd=.grok/commands/.dotcmd.md", "cmdclash=.grok/commands/cmdclash.md",
		"other-dir=.agents/skills/other-dir", "shared=.agents/skills/shared", "Shared_X=.claude/skills/Shared_X",
		"cu=.cursor/commands/cu.md"}
	if !slices.Equal(got, want) || len(over) > 0 {
		t.Errorf("the checkout's skills are\n%q\nwant\n%q\n(over %q)", got, want, over)
	}
}

// A CLONE DURING A GROK RUN IS HANDED TO ITS MAIN LOOP ALONE, ONCE: a
// subagent's call (subagentType set, row k) hands nothing and claims nothing;
// the main loop's next call hands the checkout, each file as Grok's own
// `## From: <path>`, its skills listed by their copies' paths under the name
// Grok lists them by (late-check, row 4), a command copied as a skill and a
// bare skill given a name; a later call hands nothing.
//
// Mutation: hand to a subagent, drop `## From:`, list the written name, or
// copy a bare skill as it was, and this goes red.
func TestAGrokCloneIsHandedToTheMainLoopOnce(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	if said := r.grokLaunch(t); said != "" {
		t.Errorf("the launch said %q", said)
	}
	r.add(t, map[string]string{"late/.git/": "", "late/AGENTS.md": "LATE\n", "late/.grok/rules/r.md": "RULE\n",
		"late/.grok/skills/check/SKILL.md": "---\nname: check\ndescription: Checks.\n---\nCheck.\n",
		"late/.claude/commands/fix.md":     "Fix it.\n", "late/.agents/skills/bare/SKILL.md": "# Bare\n\nBare body.\n"})
	if got := r.grokFire(t, grokSubCall); got != "" {
		t.Errorf("a subagent's call was handed %q", got)
	}
	root := filepath.Join(r.workdir(t), "late")
	want := "for `late` with root " + root + "\n\n## From: " + root + "/AGENTS.md\nLATE\n\n\n## From: " + root +
		"/.grok/rules/r.md\nRULE\n\n\n# Skills of " + root + "\n\nEach is a set of local instructions to follow, " +
		"stored in its `SKILL.md` file; to use one, read that file.\n" +
		"- late-check: Checks. (file: " + r.skills + "/late:check/SKILL.md)\n" +
		"- late-bare (file: " + r.skills + "/late:bare/SKILL.md)\n" +
		"- late-fix (file: " + r.skills + "/late:fix/SKILL.md)"
	if got := r.grokFire(t, grokMainCall); got != want {
		t.Errorf("the main loop's call was handed\n%q\nwant\n%q", got, want)
	}
	if got := read(t, r.skills+"/late:bare/SKILL.md"); got != "---\nname: \"late:bare\"\n---\n# Bare\n\nBare body.\n" {
		t.Errorf("the bare skill's copy reads %q", got)
	}
	if got := read(t, r.skills+"/late:fix/SKILL.md"); got != "---\nname: \"late:fix\"\n---\nFix it.\n" {
		t.Errorf("the command's copy reads %q", got)
	}
	if got := r.grokFire(t, grokMainCall); got != "" {
		t.Errorf("a later call was handed %q", got)
	}
}

// A GROK ANSWER HOLDS WHOLE CHECKOUTS WITHIN GROK'S 10,000-CHARACTER HOOK CUT
// (rows k, g4k2), and names every other: of three clones, the one too big for
// the cut is named by its files' paths to read, the two around it whole; the
// answer stays within the cut, counted as Grok counts it, in code points (row
// g4k3).
//
// Mutation: cut a checkout's text, drop the naming, stop at the first that
// does not fit, or count bytes or UTF-16 units, and this goes red.
func TestAGrokAnswerHoldsWholeCheckoutsWithinGroksCut(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.grokLaunch(t)
	c := strings.Repeat("\U0001F600", 5000) // 10,000 UTF-16 units, 20,000 bytes: fits only as code points
	r.add(t, map[string]string{"a/.git/": "", "a/AGENTS.md": "A-SMALL\n", "b/.git/": "",
		"b/AGENTS.md": strings.Repeat("b", 9700), "b/CLAUDE.local.md": "B-LOCAL\n", "c/.git/": "", "c/AGENTS.md": c})
	got := r.grokFire(t, grokMainCall)
	work := r.workdir(t)
	for _, want := range []string{"## From: " + work + "/a/AGENTS.md\nA-SMALL", "## From: " + work + "/c/AGENTS.md\n" + c,
		"for `b` with root " + work + "/b: read " + work + "/b/CLAUDE.local.md, " + work +
			"/b/AGENTS.md for its instructions"} {
		if !strings.Contains(got, want) {
			t.Errorf("the answer lacks %.80q", want)
		}
	}
	if strings.Contains(got, "B-LOCAL") || units(got) > grokNestedChars {
		t.Errorf("the answer is %d units, holding b's text: %t", units(got), strings.Contains(got, "B-LOCAL"))
	}
}

// A COPY THAT A PERSON'S SKILL WOULD HIDE, OR RENAME, IS LEFT OUT, SAID
// (rows x1-x7): Grok lists a copy `late:foo` as late-foo, so a home skill
// whose name or directory rewrites to it (late-foo; late-bar-x named
// late-bar), one of the home's other vendor directories (.claude/skills),
// a home command, and another checkout's copy this firing made (`a:b-c` and
// `a-b:c`) each keep the name, and the copy is left out with a line naming
// both; an unrelated home skill leaves the copy be.
//
// Mutation: compare written names, read the run's skills alone, or check
// only at the plan, and a row goes red.
func TestAGrokCopyAPersonsSkillWouldHideIsLeftOutSaid(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	sk := func(name string) string { return "---\nname: " + name + "\ndescription: D.\n---\n" }
	writeTree(t, r.skills, map[string]string{"late-foo/SKILL.md": sk("late-foo"), "late-bar-x/SKILL.md": sk("late-bar"),
		"other/SKILL.md": sk("other")})
	writeTree(t, r.grokHome(), map[string]string{".claude/skills/group/late-baz/SKILL.md": sk("late-baz"),
		".cursor/commands/late-cmd.md": "body\n"})
	skills := func(names ...string) map[string]string {
		out := map[string]string{}
		for _, n := range names {
			out[".grok/skills/"+n+"/SKILL.md"] = sk(n)
		}
		return out
	}
	r.add(t, map[string]string{"late/.git/": "", "a/.git/": "", "a-b/.git/": ""})
	r.add(t, prefixed("late/", skills("foo", "bar", "baz", "cmd", "kept")))
	r.add(t, prefixed("a/", skills("b-c")))
	r.add(t, prefixed("a-b/", skills("c")))
	said := r.grokLaunch(t)
	for _, want := range []string{"late/.grok/skills/foo is left out: the home's " + r.skills + "/late-foo, which Grok Build lists as late-foo",
		"late/.grok/skills/bar is left out: the home's " + r.skills + "/late-bar-x, which Grok Build lists as late-bar",
		"late/.grok/skills/baz is left out: the home's " + r.grokHome() + "/.claude/skills/group/late-baz",
		"late/.grok/skills/cmd is left out: the home's " + r.grokHome() + "/.cursor/commands/late-cmd.md",
		"a-b/.grok/skills/c is left out: the run's copy " + r.skills + "/a:b-c takes the name a-b-c already"} {
		if !strings.Contains(said, want) {
			t.Errorf("the launch did not say %q; it said\n%s", want, said)
		}
	}
	if _, err := os.Stat(r.skills + "/late:kept/SKILL.md"); err != nil {
		t.Errorf("the unhidden copy is missing: %v", err)
	}
	for _, gone := range []string{"late:foo", "late:bar", "late:baz", "late:cmd", "a-b:c"} {
		if _, err := os.Stat(r.skills + "/" + gone); err == nil {
			t.Errorf("the hidden copy %s was made", gone)
		}
	}
}

// prefixed is files under dir.
func prefixed(dir string, files map[string]string) map[string]string {
	out := map[string]string{}
	for rel, body := range files {
		out[dir+rel] = body
	}
	return out
}

// GROK'S FORMS KEEP THEIR SHAPE ON THE WIRE, and Codex's and Pi's theirs: a
// list of names stays a list unless an option is set, a tree's and a skill
// directory's options appear only when set.
//
// Mutation: write an option unset, or drop one set, and the round trip goes
// red.
func TestGroksFormsKeepTheirShapeOnTheWire(t *testing.T) {
	t.Parallel()
	wire := `[["AGENTS.md"],{"names":["Agents.md"],"inside":true,"once":true},` +
		`{"tree":".grok/rules","suffix":".md","form":"grok","once":true}]`
	var slots []instructionSlot
	if err := json.Unmarshal([]byte(wire), &slots); err != nil {
		t.Fatal(err)
	}
	if again, err := json.Marshal(slots); err != nil || string(again) != wire {
		t.Errorf("the slots read back as %s (%v), want %s", again, err, wire)
	}
	dirsWire := `[".codex/skills",{"dir":".pi/skills","recursive":true},` +
		`{"dir":".grok/skills","recursive":true,"through":true,"every":true,"depth":6},{"dir":".grok/commands","commands":true}]`
	var dirs []skillDirSlot
	if err := json.Unmarshal([]byte(dirsWire), &dirs); err != nil {
		t.Fatal(err)
	}
	if again, err := json.Marshal(dirs); err != nil || string(again) != dirsWire {
		t.Errorf("the skill directories read back as %s (%v), want %s", again, err, dirsWire)
	}
	if err := json.Unmarshal([]byte(`[{"dir":"x","depth":-1}]`), &dirs); err == nil {
		t.Error("a negative depth was read, want it refused")
	}
	bad := `[{"tree":".grok/rules","suffix":".md","form":"grok","scoped":{"keys":["paths"],"whole":["**"]}}]`
	if err := json.Unmarshal([]byte(bad), &slots); err == nil {
		t.Error("a grok tree with a scope was read, want it refused: Grok takes every file")
	}
}

// A GROK LAUNCH WITH A .grok/rules TREE HANDS IT: the launch config Grok's
// runner writes (its tree slots carry the "grok" form, which the core now
// requires) is read and the rule handed, under the checkout's root.
//
// Mutation: leave the form empty, and the launch is refused.
func TestAGrokLaunchWithARulesTreeHandsIt(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"repo/.git/": "", "repo/.grok/rules/r.md": "RULE\n"})
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, checkoutSpec: grokSpec(r.skills, r.grokHome())})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if err := checkoutLaunch(config, r.work, strings.NewReader(""), &errs); err != nil {
		t.Fatalf("the launch failed: %v\n%s", err, errs.String())
	}
	if got, want := read(t, r.agents), "for `repo` with root "+filepath.Join(r.workdir(t), "repo")+"\n\nRULE\n"; got != want {
		t.Errorf("the launch wrote %q, want %q", got, want)
	}
}

// manyFiles is n files under dir, by path relative to it.
func manyFiles(dir string, n int) map[string]string {
	out := map[string]string{}
	for i := range n {
		out[filepath.Join(dir, "f"+strconv.Itoa(i)+".js")] = "x\n"
	}
	return out
}

// A SKILL WALK COUNTS DIRECTORIES AND SKILLS, NEVER THE FILES BESIDE THEM:
// a Grok skill holding 1,100 files (a package's node_modules) and a Pi tree
// with a directory of 1,100 files are walked whole, their skills found, as
// Grok and Pi list them; a tree past 4,096 directories is left out whole,
// said, its bound named.
//
// Mutation: count every entry again, or drop the directory bound, and a row
// goes red.
func TestASkillWalkCountsDirectoriesNotFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sk := "---\nname: s\ndescription: S.\n---\n"
	files := map[string]string{".grok/skills/big/SKILL.md": sk, ".grok/skills/small/SKILL.md": sk,
		".pi/skills/assets/readme.md": "x\n", ".pi/skills/kept/SKILL.md": sk}
	maps.Copy(files, manyFiles(".grok/skills/big/node_modules/pkg", 1100))
	maps.Copy(files, manyFiles(".pi/skills/assets", 1100))
	writeTree(t, root, files)
	grok := []skillDirSlot{{Dir: ".grok/skills", Recursive: true, skillWalkOptions: grokHomeWalk}}
	dirs, over := checkoutSkillDirs(root, checkoutSpec{SkillDirs: grok})
	if got := skillNames(t, root, dirs); !slices.Equal(got, []string{"big=.grok/skills/big", "small=.grok/skills/small"}) ||
		len(over) > 0 {
		t.Errorf("Grok's walk found %q (said %q), want big and small", got, over)
	}
	dirs, over = checkoutSkillDirs(root, checkoutSpec{SkillDirs: piSkillDirs})
	if got := skillNames(t, root, dirs); !slices.Equal(got, []string{"kept=.pi/skills/kept"}) || len(over) > 0 {
		t.Errorf("Pi's walk found %q (said %q), want kept", got, over)
	}
	wide := t.TempDir()
	for i := range maxWalkDirs + 1 {
		if err := os.MkdirAll(filepath.Join(wide, ".pi/skills/d"+strconv.Itoa(i/64), strconv.Itoa(i%64)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTree(t, wide, map[string]string{".pi/skills/a/SKILL.md": sk})
	dirs, over = checkoutSkillDirs(wide, checkoutSpec{SkillDirs: piSkillDirs})
	if len(dirs) != 0 || len(over) != 1 || !strings.Contains(over[0], "passes the bound of 4096 directories read") {
		t.Errorf("a walk past the directory bound found %d skills and said %q, want it left out whole", len(dirs), over)
	}
}

// A GROK COPY IS NAMED AFTER THE NAME GROK LISTS ITS SKILL UNDER, so two
// skills of one directory name in a walked tree (a/setup listed a-setup,
// b/setup listed b-setup) are both copied; and a home whose skill holds 1,100
// files (node_modules) still takes copies: its files are no names.
//
// Mutation: name a copy after its directory, or count the home's files, and
// this goes red.
func TestAGrokCopyIsNamedAsGrokListsIt(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	home := map[string]string{".claude/skills/js/SKILL.md": "---\nname: js\ndescription: J.\n---\n"}
	maps.Copy(home, manyFiles(".claude/skills/js/node_modules/pkg", 1100))
	writeTree(t, r.grokHome(), home)
	r.add(t, map[string]string{"late/.git/": "",
		"late/.grok/skills/a/setup/SKILL.md": "---\nname: a-setup\ndescription: A.\n---\n",
		"late/.grok/skills/b/setup/SKILL.md": "---\nname: b-setup\ndescription: B.\n---\n"})
	if said := r.grokLaunch(t); said != "" {
		t.Errorf("the launch said %q", said)
	}
	for _, copied := range []string{"late:a-setup", "late:b-setup"} {
		if _, err := os.Stat(filepath.Join(r.skills, copied, "SKILL.md")); err != nil {
			t.Errorf("the copy %s is missing: %v", copied, err)
		}
	}
}
