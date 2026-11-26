package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// countingReader serves an endless body of `a` lines after head, counting
// every byte it hands over.
type countingReader struct {
	head io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.head.Read(p)
	if errors.Is(err, io.EOF) {
		for i := range p {
			p[i] = "a\n"[i%2]
		}
		n, err = len(p), nil
	}
	c.read += n
	return n, err
}

// A SKILL'S NAME IS READ NO FURTHER THAN ITS FRONTMATTER'S CLOSE: a home of
// skills with long bodies costs a firing their frontmatters alone, not
// [maxFrontmatter] each, whether the frontmatter names the skill or not.
//
// Mutation: read the bound whole before parsing, or read on past the close,
// and the read passes one buffer.
func TestASkillsNameIsReadNoFurtherThanItsFrontmattersClose(t *testing.T) {
	t.Parallel()
	exact := func(l string) bool { return l == "---" }
	for _, front := range []string{"---\nname: x\n---\n", "---\ndescription: d\n---\n"} {
		r := &countingReader{head: strings.NewReader(front)}
		var name string
		named := readFrontmatter(r, exact, func(l string) bool {
			n, ok := strings.CutPrefix(l, "name: ")
			name = n
			return ok
		})
		if named != strings.Contains(front, "name:") || (named && name != "x") {
			t.Errorf("%q: named %v as %q", front, named, name)
		}
		if r.read > 4096 {
			t.Errorf("%q: %d bytes were read for its name, past one buffer of 4,096", front, r.read)
		}
	}
}

// A MUSE HOME IS READ AS MUSE CODE 1.4.25 LISTS IT (rows fn1-fn4): a skill
// directory directly in a home skill directory or the run's skills (Muse's rule
// walks them so), a dot one too (unmeasured, so not assumed unlisted), takes
// the name its frontmatter gives, read past a long body, or its directory's
// where that is empty; a directory holding no SKILL.md is no skill and takes no
// name, so the copies those hide are left out, said, and the rest are made.
//
// Mutation: skip dot directories in the rule's walk, take a directory
// without SKILL.md, or take an empty name, and this goes red.
func TestAMuseHomeIsReadAsMuseListsIt(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	home := filepath.Join(t.TempDir(), "config-skills")
	long := strings.Repeat("body line\n", 8<<10)
	for dir, body := range map[string]string{
		".dot/SKILL.md":       "---\nname: late:dot\n---\n",
		"late:empty/SKILL.md": "---\nname: \"\"\n---\n",
		"long/SKILL.md":       "---\ndescription: d\nname: 'late:long'\n---\n" + long,
		"late:bare/x.md":      "not a skill\n",
	} {
		if err := os.MkdirAll(filepath.Join(home, filepath.Dir(dir)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, dir), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.museLaunch(t, "", home)
	if err := os.MkdirAll(filepath.Join(r.skills, ".adot"), 0o700); err != nil { // ~/.agents/skills, the rule's walk
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.skills, ".adot", "SKILL.md"), []byte("---\nname: late:adot\n---\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"late/.git/": ""}
	for _, s := range []string{"dot", "long", "bare", "empty", "adot"} {
		files["late/.agents/skills/"+s+"/SKILL.md"] = "---\nname: " + s + "\ndescription: D" + s + ".\n---\n"
	}
	r.add(t, files)
	got := museContext(t, r.museFire(t, "s", "s"))
	for _, s := range []string{"dot", "long", "empty", "adot"} {
		at := map[string]string{"dot": filepath.Join(home, ".dot"), "long": filepath.Join(home, "long"),
			"empty": filepath.Join(home, "late:empty"), "adot": filepath.Join(r.skills, ".adot")}[s]
		line := "the skill " + r.workdir(t) + "/late/.agents/skills/" + s + " is left out: the home's " + at +
			", which Muse Code lists as late:" + s + ", takes the name late:" + s + " it would list the copy under"
		if !strings.Contains(got, "- "+line) {
			t.Errorf("the answer is\n%s\nwant %q", got, line)
		}
	}
	if !strings.Contains(got, "- late:bare: Dbare.") {
		t.Errorf("the answer is\n%s\nwant late:bare listed: a directory without SKILL.md takes no name", got)
	}
}

// A MUSE HOME THE FIRING CANNOT READ LEAVES EVERY COPY OUT, SAID: a home skill
// directory past the walk's bound of 1,024 skills may hold any name, so no
// copy is made on a guess that it does not.
//
// Mutation: skip a home directory that cannot be read, and the copy is made.
func TestAMuseHomeTheFiringCannotReadLeavesEveryCopyOut(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	home := filepath.Join(t.TempDir(), "config-skills")
	for i := range maxWalkSkills + 1 {
		dir := filepath.Join(home, fmt.Sprintf("s%04d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: n\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.museLaunch(t, "", home)
	r.add(t, map[string]string{"late/.git/": "",
		"late/.agents/skills/deploy/SKILL.md": "---\nname: deploy\ndescription: Deploys.\n---\n"})
	got := museContext(t, r.museFire(t, "s", "s"))
	if want := "the home's skills under " + home + " could not be read for the names they take"; !strings.Contains(got, want) {
		t.Errorf("the answer is\n%s\nwant %q", got, want)
	}
	if _, err := os.Lstat(filepath.Join(r.skills, "late:deploy")); err == nil {
		t.Error("a copy was made beside a home the firing could not read")
	}
}

// A LAUNCH NAMING A LISTING RULE THE HELPER DOES NOT KNOW IS REFUSED, naming
// the field, rather than run as the bare rule, which checks no home skill.
//
// Mutation: accept any listedAs, and the launch runs.
func TestALaunchRefusesAListingRuleItDoesNotKnow(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	spec := museSpec(r.skills)
	spec.ListedAs = "hermes"
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, checkoutSpec: spec})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	err := checkoutLaunch(config, r.work, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "listedAs") {
		t.Fatalf("the launch with listedAs hermes returned %v, want it refused naming listedAs", err)
	}
}

// A MUSE HOME SKILL NAMED IN A FORM NO LINE SETTLES LEAVES EVERY COPY OUT,
// SAID: a block scalar's name may be any copy's, so none is made on a guess.
//
// Mutation: read the skill under its directory's name instead, and the copies
// are made.
func TestAMuseHomeSkillNamedUnplainlyLeavesEveryCopyOut(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	home := filepath.Join(t.TempDir(), "config-skills")
	if err := os.MkdirAll(filepath.Join(home, "block"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "block", "SKILL.md"), []byte("---\nname: |\n  late:deploy\n---\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	r.museLaunch(t, "", home)
	r.add(t, map[string]string{"late/.git/": "",
		"late/.agents/skills/deploy/SKILL.md": "---\nname: deploy\ndescription: Deploys.\n---\n",
		"late/.agents/skills/lint/SKILL.md":   "---\nname: lint\ndescription: Lints.\n---\n"})
	got := museContext(t, r.museFire(t, "s", "s"))
	want := "the home's " + filepath.Join(home, "block") + " names its skill in a form the helper does not read plainly"
	if strings.Count(got, want) != 2 {
		t.Errorf("the answer is\n%s\nwant both copies left out: %q", got, want)
	}
	for _, name := range []string{"late:deploy", "late:lint"} {
		if _, err := os.Lstat(filepath.Join(r.skills, name)); err == nil {
			t.Errorf("the copy %s was made beside a home skill whose name may be its", name)
		}
	}
}

// A COPY'S PLACE ALREADY TAKEN IS NEVER LISTED AS THE RUN'S: the run's end
// removes every name its copied list holds, so an entry already at the
// copy's place, of any kind and under any listing rule, refuses the copy
// before it is listed, and the entry is as it was.
//
// Mutation: drop copyPlanned's place check, and the name is listed.
func TestACopysTakenPlaceIsNeverListedAsTheRuns(t *testing.T) {
	t.Parallel()
	for _, listedAs := range []string{"", listedGrok, listedKimi, listedMuse} {
		r := newRepoRun(t, map[string]string{"late/.git/": "", "late/.agents/skills/s/SKILL.md": "---\nname: s\n---\n"})
		root := filepath.Join(r.work, "late")
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		plan, _, err := planSkill(filepath.Join(root, ".agents/skills/s"), real, "late:s")
		if err != nil {
			t.Fatal(err)
		}
		mine := filepath.Join(r.skills, "late:s", "x.md")
		if err := os.MkdirAll(filepath.Dir(mine), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(mine, []byte("MINE"), 0o600); err != nil {
			t.Fatal(err)
		}
		spec := museSpec(r.skills)
		spec.ListedAs, spec.held = listedAs, &homeNames{read: true, taken: map[string]string{}}
		_, err = copyPlanned(plan, spec, filepath.Join(r.state, stagingDir), &skips{stderr: io.Discard})
		if err == nil || err.Error() != "the home's skills hold late:s already" {
			t.Errorf("listedAs %q: the copy over a person's entry returned %v", listedAs, err)
		}
		if copied, _ := os.ReadFile(spec.Copied); strings.Contains(string(copied), "late:s") {
			t.Errorf("listedAs %q: the person's entry was listed for removal: %q", listedAs, copied)
		}
		if raw, _ := os.ReadFile(mine); string(raw) != "MINE" {
			t.Errorf("listedAs %q: the person's entry reads %q", listedAs, raw)
		}
	}
}

// WHATEVER THE LISTING RULE, A NAME THE RUN'S SKILLS HOLD IS HELD: a plain
// file or a link leading nowhere is no skill any rule reads, yet a copy there
// would replace it or be listed and removed, so planning says the name is
// held, after the rule's own reasons (a copy this firing made keeps its
// wording).
//
// Mutation: drop homeHolds' place check for a ruled harness, and the name is
// free.
func TestARunsSkillsEntryHoldsItsNameWhateverTheRule(t *testing.T) {
	t.Parallel()
	skills := t.TempDir()
	if err := os.WriteFile(filepath.Join(skills, "late:file"), []byte("MINE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nowhere", filepath.Join(skills, "late:link")); err != nil {
		t.Fatal(err)
	}
	for _, listedAs := range []string{"", listedGrok, listedKimi, listedMuse} {
		spec := checkoutSpec{Skills: skills, ListedAs: listedAs}
		spec.held = &homeNames{}
		for _, name := range []string{"late:file", "late:link"} {
			if got, want := homeHolds(spec, name), "the home's skills hold "+name+" already"; got != want {
				t.Errorf("listedAs %q: %s is held for %q, want %q", listedAs, name, got, want)
			}
		}
		if got := homeHolds(spec, "late:free"); got != "" {
			t.Errorf("listedAs %q: a free name is held for %q", listedAs, got)
		}
	}
}
