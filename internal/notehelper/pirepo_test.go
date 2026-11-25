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

// piSpec is the spec a Pi run's launch gives the core
// (internal/sandbox/executor's piCheckouts and piFiringLimit), its skills in
// skills.
func piSpec(skills string) checkoutSpec {
	return checkoutSpec{Instructions: names([]string{"AGENTS.override.md", "AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"}),
		FiringLimit: limited(157_696),
		SkillDirs:   []skillDirSlot{{Dir: ".pi/skills", Recursive: true}, {Dir: ".agents/skills", Recursive: true}},
		Skills:      skills, Copied: filepath.Join(filepath.Dir(skills), "copied"), Reserved: []string{"crewlet"}}
}

// piLaunch runs checkout-launch in r's working directory under Pi's spec.
func (r repoRun) piLaunch(t *testing.T) {
	t.Helper()
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, checkoutSpec: piSpec(r.skills)})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if err := checkoutLaunch(config, r.work, &errs); err != nil || errs.Len() > 0 {
		t.Fatalf("the launch failed: %v\n%s", err, errs.String())
	}
}

// piFire runs one pi-nested firing off the state the launch wrote: what it
// printed and said.
func (r repoRun) piFire(t *testing.T) (out, said string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := runPiNested(context.Background(), flags{config: filepath.Join(r.state, "nested.json")},
		strings.NewReader("never read"), &stdout, &stderr, os.Getenv); code != exitDone {
		t.Fatalf("the firing exited %d: %s", code, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// A CHECKOUT CLONED DURING A PI RUN IS HANDED IN PI'S OWN SHAPE, ONCE: the
// first firing after the clone prints its root's instruction file, the first
// of Pi's five names, wrapped as Pi wraps a context file and labelled with the
// checkout, then its skills from both recursive directories listed by their
// copies' paths; a firing before it and every firing after print nothing.
//
// Mutation: drop the wrapping or its path, list no skills, or hand twice, and
// this goes red.
func TestACheckoutClonedDuringAPiRunIsHandedInPisOwnShapeOnce(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.piLaunch(t)
	if out, said := r.piFire(t); out != "" || said != "" {
		t.Errorf("a firing with no new checkout printed %q, said %q", out, said)
	}
	r.add(t, map[string]string{"late/.git/": "", "late/AGENTS.md": "LATE\n", "late/CLAUDE.md": "NOT-THIS\n",
		"late/.pi/skills/group/deep/SKILL.md": "---\nname: deep\ndescription: Deep.\n---\nDeep.\n",
		"late/.agents/skills/fix/SKILL.md":    "---\nname: fix\ndescription: Fixes.\n---\nFix.\n"})
	root := filepath.Join(r.workdir(t), "late")
	want := "for `late` with root " + root + "\n\n<project_instructions path=\"" + root + "/AGENTS.md\">\nLATE\n\n" +
		"</project_instructions>\n\n# Skills of " + root + "\n\nEach is a set of local instructions to follow, stored in " +
		"its `SKILL.md` file; to use one, read that file.\n" +
		"- late:deep: Deep. (file: " + r.skills + "/late:deep/SKILL.md)\n" +
		"- late:fix: Fixes. (file: " + r.skills + "/late:fix/SKILL.md)"
	if out, said := r.piFire(t); out != want || said != "" {
		t.Errorf("the firing after the clone printed\n%q\nwant\n%q\n(said %q)", out, want, said)
	}
	if got := read(t, r.skills+"/late:deep/SKILL.md"); got != "---\nname: \"late:deep\"\ndescription: Deep.\n---\nDeep.\n" {
		t.Errorf("the nested skill's copy reads %q", got)
	}
	if out, said := r.piFire(t); out != "" || said != "" {
		t.Errorf("a later firing printed %q, said %q", out, said)
	}
}

// A PI FIRING HANDS ITS CLONES' FILES WITHIN ITS BOUND, FRAMES COUNTED: of
// three clones in one firing, each file whose text, its path three times and
// 64 bytes fit what is left of the FiringLimit is handed whole in Pi's
// wrapping, in order, and one past it is left out whole, never cut, named in
// the answer and once in the skip log for the model to read there, though its
// text alone would fit; the wrapped files and their labels take no more than
// the bound. A later firing hands nothing.
//
// Mutation: hand past the bound, count the text alone, cut the file, or say
// nothing of it, and this goes red.
func TestAPiFiringHandsItsClonesFilesWithinItsBound(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	work := r.workdir(t)
	cost := func(clone, file, text string) int { return len(text) + 3*len(filepath.Join(work, clone, file)) + 64 }
	a, b, c := strings.Repeat("A", 300), strings.Repeat("B", 200), strings.Repeat("C", 100)
	spec := piSpec(r.skills)
	spec.FiringLimit = limited(cost("a", "AGENTS.md", a) + cost("c", "CLAUDE.md", c))
	if len(b) > cost("c", "CLAUDE.md", c) || cost("b", "AGENTS.md", b) <= cost("c", "CLAUDE.md", c) {
		t.Fatal("b's text must fit the room c takes and its frame must not")
	}
	r.launchWith(t, spec, false, "")
	r.add(t, map[string]string{"a/.git/": "", "a/AGENTS.md": a, "b/.git/": "", "b/AGENTS.md": b, "c/.git/": "",
		"c/CLAUDE.md": c})
	framed := func(clone, file, text string) string {
		root := filepath.Join(work, clone)
		return "for `" + clone + "` with root " + root + "\n\n<project_instructions path=\"" + root + "/" + file + "\">\n" +
			text + "\n</project_instructions>"
	}
	files := framed("a", "AGENTS.md", a) + "\n\n" + framed("c", "CLAUDE.md", c)
	line := "the instruction file " + filepath.Join(work, "b", "AGENTS.md") + " is left out: it passes what is " +
		"left of the firing's bound on instructions; read it at that path"
	if out, said := r.piFire(t); out != files+"\n\n"+skippedListing([]string{line}) || said != "" {
		t.Errorf("the firing printed\n%q\nwant\n%q\n(said %q)", out, files+"\n\n"+skippedListing([]string{line}), said)
	}
	if len(files) > *spec.FiringLimit {
		t.Errorf("the wrapped files take %d bytes, past the firing's bound of %d", len(files), *spec.FiringLimit)
	}
	if out, said := r.piFire(t); out != "" || said != "" {
		t.Errorf("a later firing printed %q, said %q", out, said)
	}
	if log := read(t, filepath.Join(r.state, skippedLog)); strings.Count(log, line) != 1 {
		t.Errorf("the skip log reads %q, want the file left out named once", log)
	}
}

// EACH CLONE NAMES THE FILE PI WOULD LOAD AT ITS ROOT: the first of
// AGENTS.override.md, AGENTS.md, AGENTS.MD, CLAUDE.md and CLAUDE.MD, by exact
// name (measured on 1.6.0), its path in the wrapping; a clone with neither
// instructions nor skills prints nothing.
//
// Mutation: read the names in another order or in any case, and a row goes red.
func TestAPiFiringNamesTheFilePiWouldLoad(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.piLaunch(t)
	for _, c := range []struct {
		files map[string]string
		file  string
	}{
		{map[string]string{"AGENTS.override.md": "O\n", "AGENTS.md": "A\n"}, "AGENTS.override.md"},
		{map[string]string{"AGENTS.MD": "U\n", "CLAUDE.md": "C\n"}, "AGENTS.MD"},
		{map[string]string{"CLAUDE.MD": "C\n", "claude.md": "lower\n"}, "CLAUDE.MD"},
		{map[string]string{"agents.md": "lower\n", "Claude.md": "mixed\n"}, ""},
	} {
		name := strings.ToLower(strings.ReplaceAll(c.file, ".", "-")) + "x"
		files := map[string]string{name + "/.git/": ""}
		for f, body := range c.files {
			files[name+"/"+f] = body
		}
		r.add(t, files)
		root := filepath.Join(r.workdir(t), name)
		want := ""
		if c.file != "" {
			want = "for `" + name + "` with root " + root + "\n\n<project_instructions path=\"" + root + "/" + c.file +
				"\">\n" + c.files[c.file] + "\n</project_instructions>"
		}
		if out, said := r.piFire(t); out != want || said != "" {
			t.Errorf("%v: printed %q, want %q (said %q)", c.files, out, want, said)
		}
	}
}

// A FIRING THAT CANNOT READ ITS CONFIG PRINTS NOTHING AND EXITS 0, saying
// why: the extension adds nothing to the tool's result, and the run goes on.
//
// Mutation: exit 1, or say nothing, and this goes red.
func TestAPiFiringWithoutItsConfigPrintsNothing(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	out, said := r.piFire(t)
	if out != "" || !strings.Contains(said, "notehelper: the nested config") {
		t.Errorf("printed %q, said %q", out, said)
	}
}

// TWO SKILLS OF ONE NAME IN ONE WALK ARE SAID: a walk reaching web/deploy and
// infra/deploy copies the first as `late:deploy` and leaves the second out,
// saying so with both paths (in the firing's answer after its skills and in
// the run's skip log, never on stderr), since no precedence of Pi's picks one;
// a name .agents/skills loses to .pi/skills is Pi's own order, and silent.
//
// Mutation: say nothing, say the cross-directory clash, or leave the line
// out of the answer or the log, and this goes red.
func TestTwoSkillsOfOneNameInOneWalkAreSaid(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.piLaunch(t)
	skill := "---\nname: deploy\ndescription: Deploys.\n---\n"
	r.add(t, map[string]string{"late/.git/": "",
		"late/.pi/skills/infra/deploy/SKILL.md": skill, "late/.pi/skills/web/deploy/SKILL.md": skill,
		"late/.agents/skills/deploy/SKILL.md": skill})
	root := filepath.Join(r.workdir(t), "late")
	out, said := r.piFire(t)
	if !strings.Contains(out, "- late:deploy: Deploys. (file: "+r.skills+"/late:deploy/SKILL.md)") {
		t.Errorf("the firing printed %q, want late:deploy listed", out)
	}
	line := "the skill " + root + "/.pi/skills/web/deploy is left out: " + root +
		"/.pi/skills/infra/deploy, found first, takes its name late:deploy"
	if said != "" {
		t.Errorf("the firing said %q on stderr, want the line in its log alone", said)
	}
	if !strings.HasSuffix(out, "\n\n# Left out of the repositories below the working directory\n\n- "+line) {
		t.Errorf("the firing printed %q, want its skills then the line", out)
	}
	if got := read(t, filepath.Join(r.state, "skipped")); got != "notehelper: "+line+"\n" {
		t.Errorf("the run's skip log holds %q, want the line once", got)
	}
}
