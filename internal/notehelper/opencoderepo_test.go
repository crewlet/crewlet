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
	walked := func(dir string) skillDirSlot { return skillDirSlot{Dir: dir, Recursive: true, Depth: 6} }
	return checkoutSpec{Instructions: names([]string{"AGENTS.md", "CLAUDE.md"}),
		SkillDirs: []skillDirSlot{walked(".opencode/skills"), walked(".opencode/skill"), walked(".claude/skills"),
			walked(".agents/skills")},
		Skills: skills, Copied: filepath.Join(filepath.Dir(skills), "copied"), Reserved: []string{"crewlet"}}
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
