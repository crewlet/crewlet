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

// copilotSpec is the spec a Copilot run's launch gives the core
// (internal/sandbox/executor's copilotCheckouts), its skills in skills.
func copilotSpec(skills string) checkoutSpec {
	return checkoutSpec{Instructions: append(names([]string{".github/copilot-instructions.md"}, []string{"AGENTS.md"},
		[]string{"CLAUDE.md"}, []string{"GEMINI.md"}), instructionSlot{Tree: &instructionTree{Tree: ".claude/rules",
		Suffix: ".md", Scoped: &treeScope{Keys: []string{"applyTo", "paths"}, Whole: []string{"**", "**/*", "*"}}}}),
		SkillDirs: []string{".github/skills", ".agents/skills", ".claude/skills"}, Skills: skills,
		Copied: filepath.Join(filepath.Dir(skills), "copied"), Reserved: []string{"crewlet"}}
}

// copilotLaunchRun is r launched under Copilot's spec, and the launch
// config's file, which copilot-launch reads.
func copilotLaunchRun(t *testing.T, r repoRun, first ...string) string {
	t.Helper()
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, First: first, checkoutSpec: copilotSpec(r.skills)})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if err := checkoutLaunch(config, r.work, &errs); err != nil || errs.Len() > 0 {
		t.Fatalf("the launch failed: %v\n%s", err, errs.String())
	}
	return config
}

// compose runs copilot-launch with args, and is how many sets it printed,
// what it said and the files it wrote: the instruction file, the directories
// and the names kept.
func (r repoRun) compose(t *testing.T, config string, args ...string) (n, said, agents, dirs, kept string) {
	t.Helper()
	var out, errs bytes.Buffer
	code, ok := runOnce(context.Background(), append([]string{"-adapter", "copilot-launch", "-config", config}, args...),
		os.Getenv, strings.NewReader(""), &out, &errs)
	if !ok || code != exitDone {
		t.Fatalf("copilot-launch %q exited %d (%v): %s", args, code, ok, errs.String())
	}
	return strings.TrimSpace(out.String()), errs.String(), read(t, r.agents), read(t, filepath.Join(r.state, "dirs")),
		read(t, filepath.Join(r.state, "kept"))
}

// A COPILOT LAUNCH KEEPS THE FIRST LAUNCH SETS AND POINTS AT THE REST: a
// checkout's set is its section of the home file and its .github/instructions
// (a real directory, named for COPILOT_CUSTOM_INSTRUCTIONS_DIRS); a checkout
// with neither has none. Keeping all hands every one; keeping fewer drops the
// LAST first, each dropped one named by a line saying its instructions arrive
// on Copilot's own lazy load, its directory out of the list; keeping none
// still names each. A stored list keeps exactly the sets it names, and a
// missing one keeps none.
//
// Mutation: drop the first set rather than the last, keep a dropped set's
// directory, hand a linked directory, or omit the pointer line.
func TestACopilotLaunchKeepsTheFirstSetsAndPointsAtTheRest(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{
		"alpha/.git/":     "",
		"alpha/AGENTS.md": "ALPHA\n",
		"alpha/.github/instructions/a.instructions.md": "A\n",
		"beta/.git/": "",
		"beta/.github/instructions/b.instructions.md": "B\n",
		"gamma/.git/":                 "",
		"gamma/CLAUDE.md":             "GAMMA\n",
		"delta/.git/":                 "",
		"delta/README.md":             "no instructions\n",
		"elsewhere/x.instructions.md": "X\n",
		"linked/.git/":                "",
		"linked/.github/":             "",
	})
	work := r.workdir(t)
	if err := os.Symlink(filepath.Join(work, "elsewhere"), filepath.Join(work, "linked/.github/instructions")); err != nil {
		t.Fatal(err)
	}
	config := copilotLaunchRun(t, r, "beta")
	section := func(name, text string) string {
		return "for `" + name + "` with root " + work + "/" + name + "\n\n" + text
	}
	pointer := func(name string) string { return "for `" + name + "` with root " + work + "/" + name + copilotPointer }
	dir := func(name string) string { return work + "/" + name + "/.github/instructions" }

	for name, tc := range map[string]struct {
		args               []string
		agents, dirs, kept string
	}{
		"all": {args: nil, agents: section("alpha", "ALPHA\n") + "\n\n" + section("gamma", "GAMMA\n"),
			dirs: dir("beta") + "," + dir("alpha"), kept: "beta\nalpha\ngamma\n"},
		"the last dropped": {args: []string{"-keep", "2"}, agents: section("alpha", "ALPHA\n") + "\n\n" + pointer("gamma"),
			dirs: dir("beta") + "," + dir("alpha"), kept: "beta\nalpha\n"},
		"one kept": {args: []string{"-keep", "1"}, agents: pointer("alpha") + "\n\n" + pointer("gamma"),
			dirs: dir("beta"), kept: "beta\n"},
		"none kept": {args: []string{"-keep", "0"}, agents: pointer("beta") + "\n\n" + pointer("alpha") + "\n\n" + pointer("gamma")},
	} {
		t.Run(name, func(t *testing.T) {
			n, said, agents, dirs, kept := r.compose(t, config, tc.args...)
			if n != "3" || said != "" {
				t.Errorf("copilot-launch counted %q sets and said %q, want 3 (beta, alpha, gamma) and nothing", n, said)
			}
			if agents != tc.agents || dirs != tc.dirs || kept != tc.kept {
				t.Errorf("copilot-launch wrote\n%q\ndirs %q, kept %q\nwant\n%q\ndirs %q, kept %q", agents, dirs, kept,
					tc.agents, tc.dirs, tc.kept)
			}
		})
	}
	t.Run("a stored list", func(t *testing.T) {
		stored := filepath.Join(t.TempDir(), "stored")
		if err := os.WriteFile(stored, []byte("gamma\ngone\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, agents, dirs, kept := r.compose(t, config, "-kept", stored)
		if want := pointer("beta") + "\n\n" + pointer("alpha") + "\n\n" + section("gamma", "GAMMA\n"); agents != want ||
			dirs != "" || kept != "gamma\n" {
			t.Errorf("a stored list kept\n%q\ndirs %q, kept %q\nwant gamma's section alone, no directory", agents, dirs, kept)
		}
		_, _, _, dirs, kept = r.compose(t, config, "-kept", stored+".missing")
		if dirs != "" || kept != "" {
			t.Errorf("a missing stored list kept dirs %q and %q, want none", dirs, kept)
		}
	})
}

// A ROOT HOLDING A COMMA KEEPS ITS INSTRUCTION DIRECTORY OUT OF COPILOT'S
// LIST, said: COPILOT_CUSTOM_INSTRUCTIONS_DIRS splits on `,`, so naming it
// would name two directories that are not there.
//
// Mutation: name the directory anyway, or say nothing.
func TestARootHoldingACommaKeepsItsDirectoryOutOfCopilotsList(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{
		"a,b/.git/": "",
		"a,b/.github/instructions/x.instructions.md": "X\n",
	})
	config := copilotLaunchRun(t, r)
	var out, errs bytes.Buffer
	if code, _ := runOnce(context.Background(), []string{"-adapter", "copilot-launch", "-config", config}, os.Getenv,
		strings.NewReader(""), &out, &errs); code != exitDone {
		t.Fatalf("copilot-launch exited %d: %s", code, errs.String())
	}
	if dirs := read(t, filepath.Join(r.state, "dirs")); dirs != "" || !strings.Contains(errs.String(), "holds a comma") ||
		strings.TrimSpace(out.String()) != "0" {
		t.Errorf("a root holding a comma handed %q with %q sets and said %q, want nothing handed, said", dirs,
			out.String(), errs.String())
	}
}
