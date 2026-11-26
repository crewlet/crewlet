package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openCodeSpec is the spec an OpenCode run's launch gives the core
// (internal/sandbox/executor's openCodeCheckouts), its skills in skills.
func openCodeSpec(skills string) checkoutSpec {
	walked := func(dir string) skillDirSlot {
		return skillDirSlot{Dir: dir, Recursive: true, skillWalkOptions: skillWalkOptions{Depth: 6}}
	}
	return checkoutSpec{Instructions: names([]string{"AGENTS.md", "CLAUDE.md"}),
		SkillDirs: []skillDirSlot{walked(".opencode/skills"), walked(".opencode/skill"), walked(".claude/skills"),
			walked(".agents/skills")},
		Skills: skills, Copied: filepath.Join(filepath.Dir(skills), "copied"), Reserved: []string{"crewlet"},
		ListedAs: listedOpenCode}
}

// openCodeLaunch runs checkout-launch in r's working directory under
// OpenCode's spec.
func (r repoRun) openCodeLaunch(t *testing.T) {
	t.Helper()
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, checkoutSpec: openCodeSpec(r.skills)})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if err := checkoutLaunch(config, r.work, &errs); err != nil || errs.Len() > 0 {
		t.Fatalf("the launch failed: %v\n%s", err, errs.String())
	}
}

// openCodeFire runs one opencode-nested firing off the state the launch
// wrote: what it printed and said.
func (r repoRun) openCodeFire(t *testing.T) (out, said string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := runOpenCodeNested(context.Background(), flags{config: filepath.Join(r.state, "nested.json")},
		strings.NewReader("never read"), &stdout, &stderr, os.Getenv); code != exitDone {
		t.Fatalf("the firing exited %d: %s", code, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// A CHECKOUT CLONED DURING AN OPENCODE RUN IS HANDED IN OPENCODE'S OWN SHAPE,
// ONCE: the first firing after the clone prints its root's AGENTS.md, which
// OpenCode loads over its CLAUDE.md, after "Instructions from: <file>" as
// OpenCode frames an instruction file (measured on 2.3.2), labelled with the
// checkout, then the skills of its four directories, walked no deeper than 6,
// listed by their copies' paths; a firing before it and every firing after
// print nothing.
//
// Mutation: drop the framing or its path, read CLAUDE.md first, list no
// skills, walk past 6, or hand twice, and this goes red.
func TestACheckoutClonedDuringAnOpenCodeRunIsHandedInOpenCodesOwnShapeOnce(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.openCodeLaunch(t)
	if out, said := r.openCodeFire(t); out != "" || said != "" {
		t.Errorf("a firing with no new checkout printed %q, said %q", out, said)
	}
	skill := func(name string) string { return "---\nname: " + name + "\ndescription: D" + name + ".\n---\nBody.\n" }
	r.add(t, map[string]string{"late/.git/": "", "late/AGENTS.md": "LATE\n", "late/CLAUDE.md": "NOT-THIS\n",
		"late/.opencode/skills/a/b/c/d/e/six/SKILL.md":     skill("six"),
		"late/.opencode/skills/a/b/c/d/e/f/seven/SKILL.md": skill("seven"),
		"late/.opencode/skill/one/SKILL.md":                skill("one"),
		"late/.claude/skills/cl/SKILL.md":                  skill("cl"),
		"late/.agents/skills/ag/SKILL.md":                  skill("ag")})
	root := filepath.Join(r.workdir(t), "late")
	listed := func(name string) string {
		return "- late:" + name + ": D" + name + ". (file: " + r.skills + "/late:" + name + "/SKILL.md)"
	}
	want := "for `late` with root " + root + "\n\nInstructions from: " + root + "/AGENTS.md\nLATE\n\n\n# Skills of " +
		root + "\n\nEach is a set of local instructions to follow, stored in its `SKILL.md` file; to use one, " +
		"read that file.\n" + strings.Join([]string{listed("six"), listed("one"), listed("cl"), listed("ag")}, "\n")
	if out, said := r.openCodeFire(t); out != want || said != "" {
		t.Errorf("the firing after the clone printed\n%q\nwant\n%q\n(said %q)", out, want, said)
	}
	if out, said := r.openCodeFire(t); out != "" || said != "" {
		t.Errorf("a later firing printed %q, said %q", out, said)
	}
}

// A CLONE WITH CLAUDE.MD ALONE IS HANDED IT, as OpenCode loads it where a
// root holds no AGENTS.md (measured on 2.3.2).
//
// Mutation: read AGENTS.md alone, and this goes red.
func TestAnOpenCodeFiringHandsClaudeMDWithoutAgentsMD(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.openCodeLaunch(t)
	r.add(t, map[string]string{"late/.git/": "", "late/CLAUDE.md": "CL\n"})
	root := filepath.Join(r.workdir(t), "late")
	want := "for `late` with root " + root + "\n\nInstructions from: " + root + "/CLAUDE.md\nCL\n"
	if out, said := r.openCodeFire(t); out != want || said != "" {
		t.Errorf("printed %q, want %q (said %q)", out, want, said)
	}
}

// A COPY NEVER TAKES THE NAME OF A SKILL THE HARNESS LISTS ELSEWHERE: OpenCode
// 2.3.2 lists two skills of one name and loads neither (measured), so a
// clone's skill whose copy would be named as a skill of the spec's
// homeSkills (the home's and the working directory's) is left out, named
// in the answer and the skip log, whether the listed skill's frontmatter or,
// with none, its directory gives the name; the others are copied.
//
// Mutation: skip the listed names, read only the frontmatter, or only the
// directory, and the answer or the copies go red.
func TestACopyNeverTakesTheNameOfASkillTheHarnessListsElsewhere(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	home := t.TempDir()
	writeTree(t, home, map[string]string{
		"skills/mine/SKILL.md":      "---\nname: \"late:fix\"\ndescription: Mine.\n---\n",
		"skills/late:bare/SKILL.md": "---\ndescription: No name.\n---\n",
	})
	writeTree(t, r.work, map[string]string{".opencode/skills/w/SKILL.md": "---\nname: late:other\ndescription: W.\n---\n"})
	spec := openCodeSpec(r.skills)
	deep := skillWalkOptions{Depth: 6}
	spec.HomeSkills = []skillDirSlot{{Dir: filepath.Join(home, "skills"), Recursive: true, skillWalkOptions: deep},
		{Dir: filepath.Join(r.work, ".opencode/skills"), Recursive: true, skillWalkOptions: deep}}
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, checkoutSpec: spec})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if err := checkoutLaunch(config, r.work, &errs); err != nil {
		t.Fatalf("the launch failed: %v\n%s", err, errs.String())
	}
	skill := func(name string) string { return "---\nname: " + name + "\ndescription: " + name + ".\n---\n" }
	r.add(t, map[string]string{"late/.git/": "", "late/.agents/skills/fix/SKILL.md": skill("fix"),
		"late/.agents/skills/bare/SKILL.md": skill("bare"), "late/.agents/skills/other/SKILL.md": skill("other"),
		"late/.agents/skills/ok/SKILL.md": skill("ok")})
	out, _ := r.openCodeFire(t)
	late := filepath.Join(r.workdir(t), "late")
	log := read(t, filepath.Join(r.state, skippedLog))
	for _, s := range []string{"fix", "bare", "other"} {
		src := filepath.Join(late, ".agents/skills", s)
		if !strings.Contains(out, "the skill "+src+" is left out") || !strings.Contains(log, "the skill "+src+" is left out") {
			t.Errorf("%s: the answer or the skip log does not name it left out:\n%s\n%s", s, out, log)
		}
		if _, err := os.Stat(filepath.Join(r.skills, "late:"+s)); !os.IsNotExist(err) {
			t.Errorf("%s: a copy late:%s was made over a listed skill's name (%v)", s, s, err)
		}
	}
	if _, err := os.Stat(filepath.Join(r.skills, "late:ok", "SKILL.md")); err != nil || !strings.Contains(out, "- late:ok:") {
		t.Errorf("the skill no listed one names was not copied and listed (%v):\n%s", err, out)
	}
}
