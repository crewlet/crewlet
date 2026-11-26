package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kimiSpec is the spec a Kimi Code run's launch gives the core
// (internal/sandbox/executor's kimiCheckouts), its skills in skills, the
// home's other user root (~/.agents/skills) beside it ([kimiAgentsSkills]),
// and its firing bound firing.
func kimiSpec(skills string, firing int) checkoutSpec {
	agents := kimiAgentsSkills(skills)
	return checkoutSpec{Instructions: []instructionSlot{{Names: []string{".kimi-code/AGENTS.md"}},
		{Names: []string{"AGENTS.md", "agents.md"}, slotOptions: slotOptions{SkipBlank: true}}},
		FiringLimit: limited(firing),
		SkillDirs:   []skillDirSlot{{Dir: ".kimi-code/skills", Recursive: true}, {Dir: ".agents/skills", Recursive: true}},
		Skills:      skills, Copied: filepath.Join(filepath.Dir(skills), "copied"), Reserved: []string{"crewlet"},
		ListedAs: listedKimi, HomeSkills: []skillDirSlot{
			{Dir: agents, Recursive: true, skillWalkOptions: skillWalkOptions{Every: true, Depth: 8}},
			{Dir: agents, skillWalkOptions: skillWalkOptions{Commands: true}}}}
}

// kimiAgentsSkills is the stand-in ~/.agents/skills of a run whose skills
// are skills.
func kimiAgentsSkills(skills string) string {
	return filepath.Join(filepath.Dir(skills), "agents-skills")
}

// kimiLaunch runs checkout-launch in r's working directory under Kimi's spec,
// each firing bound to firing bytes.
func (r repoRun) kimiLaunch(t *testing.T, firing int) {
	t.Helper()
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, checkoutSpec: kimiSpec(r.skills, firing)})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if err := checkoutLaunch(config, r.work, strings.NewReader(""), &errs); err != nil || errs.Len() > 0 {
		t.Fatalf("the launch failed: %v\n%s", err, errs.String())
	}
}

// kimiFire runs one kimi-nested firing off the state the launch wrote: what
// it printed and said.
func (r repoRun) kimiFire(t *testing.T) (out, said string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := runKimiNested(context.Background(), flags{config: filepath.Join(r.state, "nested.json")},
		strings.NewReader("never read"), &stdout, &stderr, os.Getenv); code != exitDone {
		t.Fatalf("the firing exited %d: %s", code, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// A CHECKOUT CLONED DURING A KIMI RUN IS HANDED IN KIMI'S OWN SHAPE, ONCE: the
// first firing after the clone prints its root's .kimi-code/AGENTS.md, then
// its AGENTS.md, each trimmed after Kimi's `<!-- From: <file> -->` (measured
// on 2.7.2), under the checkout's label, then the skills a walk of
// .kimi-code/skills and .agents/skills reaches listed by their copies' paths
// (.kimi-code's winning a name, as Kimi's own does); a firing before it and
// every one after print nothing.
//
// Mutation: drop the annotation or the trim, read agents.md first, walk flat,
// list no skills, or hand twice, and this goes red.
func TestACheckoutClonedDuringAKimiRunIsHandedInKimisOwnShapeOnce(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.kimiLaunch(t, 96<<10)
	if out, said := r.kimiFire(t); out != "" || said != "" {
		t.Errorf("a firing with no new checkout printed %q, said %q", out, said)
	}
	r.add(t, map[string]string{"late/.git/": "", "late/.kimi-code/AGENTS.md": "\nKIMI-DOT\n\n", "late/AGENTS.md": "LATE\n",
		"late/agents.md": "NOT-THIS\n", "late/CLAUDE.md": "NOT-THIS\n",
		"late/.kimi-code/skills/group/deep/SKILL.md": "---\nname: deep\ndescription: Deep.\n---\nDeep.\n",
		"late/.kimi-code/skills/fix/SKILL.md":        "---\nname: fix\ndescription: Kimi's.\n---\nFix.\n",
		"late/.agents/skills/fix/SKILL.md":           "---\nname: fix\ndescription: Not this one.\n---\n",
		"late/.claude/skills/c/SKILL.md":             "---\nname: c\ndescription: Claude's.\n---\n"})
	root := filepath.Join(r.workdir(t), "late")
	want := "for `late` with root " + root + "\n\n<!-- From: " + root + "/.kimi-code/AGENTS.md -->\nKIMI-DOT\n\n" +
		"<!-- From: " + root + "/AGENTS.md -->\nLATE\n\n# Skills of " + root + "\n\nEach is a set of local " +
		"instructions to follow, stored in its `SKILL.md` file; to use one, read that file.\n" +
		"- late:fix: Kimi's. (file: " + r.skills + "/late:fix/SKILL.md)\n" +
		"- late:deep: Deep. (file: " + r.skills + "/late:deep/SKILL.md)"
	if out, said := r.kimiFire(t); out != want || said != "" {
		t.Errorf("the firing after the clone printed\n%q\nwant\n%q\n(said %q)", out, want, said)
	}
	if got := read(t, r.skills+"/late:deep/SKILL.md"); got != "---\nname: \"late:deep\"\ndescription: Deep.\n---\nDeep.\n" {
		t.Errorf("the nested skill's copy reads %q", got)
	}
	if out, said := r.kimiFire(t); out != "" || said != "" {
		t.Errorf("a later firing printed %q, said %q", out, said)
	}
}

// EACH CLONE NAMES THE FILES KIMI WOULD LOAD AT ITS ROOT: its
// .kimi-code/AGENTS.md, then AGENTS.md else agents.md by exact case, a file of
// whitespace alone passed over for the next name as Kimi Code 2.7.2 passes it
// (measured: a blank AGENTS.md loaded agents.md), at the launch and in a
// firing alike; a clone with nothing to load prints nothing.
//
// Mutation: drop skipBlank, read Agents.md, or read agents.md first, and a row
// goes red.
func TestAKimiCheckoutNamesTheFilesKimiWouldLoad(t *testing.T) {
	t.Parallel()
	cases := []struct {
		files map[string]string
		want  []string
	}{
		{map[string]string{"AGENTS.md": " \n\t\n", "agents.md": "LOWER\n"}, []string{"agents.md"}},
		{map[string]string{"agents.md": "LOWER\n", "Agents.md": "MIXED\n"}, []string{"agents.md"}},
		{map[string]string{"AGENTS.md": "UPPER\n", "agents.md": "LOWER\n"}, []string{"AGENTS.md"}},
		{map[string]string{".kimi-code/AGENTS.md": "\n", "AGENTS.md": "UPPER\n"}, []string{"AGENTS.md"}},
		{map[string]string{"Agents.md": "MIXED\n", "AGENT.md": "ONE\n", "KIMI.md": "K\n"}, nil},
	}
	section := func(root string, files map[string]string, want []string) string {
		if len(want) == 0 {
			return ""
		}
		var parts []string
		for _, f := range want {
			parts = append(parts, "<!-- From: "+root+"/"+f+" -->\n"+strings.TrimSpace(files[f]))
		}
		return "for `" + filepath.Base(root) + "` with root " + root + "\n\n" + strings.Join(parts, "\n\n")
	}
	t.Run("a firing", func(t *testing.T) {
		t.Parallel()
		r := newRepoRun(t, nil)
		r.kimiLaunch(t, 96<<10)
		for i, c := range cases {
			name := "c" + string(rune('a'+i))
			files := map[string]string{name + "/.git/": ""}
			for f, body := range c.files {
				files[name+"/"+f] = body
			}
			r.add(t, files)
			want := section(filepath.Join(r.workdir(t), name), c.files, c.want)
			if out, said := r.kimiFire(t); out != want || said != "" {
				t.Errorf("%v: printed %q, want %q (said %q)", c.files, out, want, said)
			}
		}
	})
	t.Run("the launch", func(t *testing.T) {
		t.Parallel()
		r := newRepoRun(t, nil)
		var sections []string
		for i, c := range cases {
			name := "c" + string(rune('a'+i))
			files := map[string]string{name + "/.git/": ""}
			for f, body := range c.files {
				files[name+"/"+f] = body
			}
			r.add(t, files)
			root := filepath.Join(r.work, name)
			for _, f := range c.want {
				sections = append(sections, "for `"+name+"` with root "+root+"\n\n"+c.files[f])
			}
		}
		r.kimiLaunch(t, 96<<10)
		if got, want := read(t, r.agents), strings.ReplaceAll(strings.Join(sections, "\n\n"), r.work, r.workdir(t)); got != want {
			t.Errorf("the launch wrote\n%q\nwant\n%q", got, want)
		}
	})
}

// A KIMI FIRING HANDS WHOLE FILES WITHIN ITS BOUND AND NAMES THE REST: the
// bound keeps the text the driver steers within KAP's 1 MiB request body
// (internal/sandbox/executor's kimiFiringLimit), so a file past what is left
// of it is never cut but left out and named in the firing's listing, to be
// read at its path; one that fits after it is still handed.
//
// Mutation: drop the firing bound or name nothing, and this goes red.
func TestAKimiFiringHandsWholeFilesWithinItsBound(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	big := strings.Repeat("big line\n", 200)
	r.kimiLaunch(t, len(big))
	r.add(t, map[string]string{"late/.git/": "", "late/.kimi-code/AGENTS.md": big, "late/AGENTS.md": "SMALL\n"})
	root := filepath.Join(r.workdir(t), "late")
	out, _ := r.kimiFire(t)
	if strings.Contains(out, "big line") {
		t.Errorf("the firing handed the file past its bound:\n%s", out)
	}
	for _, want := range []string{"<!-- From: " + root + "/AGENTS.md -->\nSMALL",
		"- the instruction file " + root + "/.kimi-code/AGENTS.md is left out: it passes what is left of the firing's " +
			"bound on instructions; read it at that path"} {
		if !strings.Contains(out, want) {
			t.Errorf("the firing printed\n%s\nwant it to hold %q", out, want)
		}
	}
}

// A PERSON'S OWN SKILL IS NEVER HIDDEN BY A KIMI COPY: a copy whose name a
// skill in either user root Kimi lists, the run's skills or ~/.agents/skills,
// takes by Kimi's rule (its frontmatter name, case aside; a loose `.md` named
// by its file where it names none) is left out, said once, and the rest are
// copied; a SKILL.md naming nothing takes no name (Kimi lists it not).
//
// Mutation: compare names in their case, read no home root, take a nameless
// SKILL.md's directory name, or skip loose `.md` files: red.
func TestAKimiCopyNeverHidesAPersonsOwnSkill(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	agents := kimiAgentsSkills(r.skills)
	person := map[string]string{
		filepath.Join(r.skills, "mine/SKILL.md"):    "---\nname: Late:Fix\ndescription: Mine.\n---\n",
		filepath.Join(agents, "deep/SKILL.md"):      "---\nname: \"late:deep\"\ndescription: Mine.\n---\n",
		filepath.Join(agents, "late:loose.md"):      "A loose skill, named by its file.\n",
		filepath.Join(agents, "late:keep/SKILL.md"): "---\ndescription: Names nothing.\n---\n",
		filepath.Join(r.skills, "late:solo.md"):     "---\ndescription: Loose in the run's skills.\n---\n",
	}
	for path, body := range person {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.kimiLaunch(t, 96<<10)
	skill := func(name string) string { return "---\nname: " + name + "\ndescription: Theirs.\n---\n" }
	r.add(t, map[string]string{"late/.git/": "", "late/.agents/skills/fix/SKILL.md": skill("fix"),
		"late/.agents/skills/deep/SKILL.md": skill("deep"), "late/.agents/skills/loose/SKILL.md": skill("loose"),
		"late/.agents/skills/keep/SKILL.md": skill("keep"), "late/.agents/skills/solo/SKILL.md": skill("solo")})
	root := filepath.Join(r.workdir(t), "late")
	out, said := r.kimiFire(t)
	for _, want := range []string{
		"- the skill " + root + "/.agents/skills/fix is left out: the home's " + r.skills + "/mine, which Kimi Code " +
			"lists as Late:Fix, takes the name late:fix it would list the copy under",
		"- the skill " + root + "/.agents/skills/deep is left out: the home's " + agents + "/deep, which Kimi Code " +
			"lists as late:deep, takes the name late:deep it would list the copy under",
		"- the skill " + root + "/.agents/skills/loose is left out: the home's " + agents + "/late:loose.md, which " +
			"Kimi Code lists as late:loose, takes the name late:loose it would list the copy under",
		"- the skill " + root + "/.agents/skills/solo is left out: the home's " + r.skills + "/late:solo.md, which " +
			"Kimi Code lists as late:solo, takes the name late:solo it would list the copy under",
		"- late:keep: Theirs. (file: " + r.skills + "/late:keep/SKILL.md)",
	} {
		if strings.Count(out, want) != 1 {
			t.Errorf("the firing printed\n%s\nwant once %q", out, want)
		}
	}
	for _, gone := range []string{"late:fix", "late:deep", "late:loose", "late:solo"} {
		if _, err := os.Lstat(filepath.Join(r.skills, gone)); err == nil {
			t.Errorf("%s was copied over the person's own skill", gone)
		}
	}
	if said != "" {
		t.Errorf("the firing said %q on stderr; want its lines in the run's log alone", said)
	}
}

// A KIMI ANSWER KEEPS WHAT IT LEFT OUT BEFORE ITS SKILLS: where the skills'
// listing and the left-out lines pass one firing's bound, the left-out lines
// come whole, last, and the skills' listing before them is cut.
//
// Mutation: take the skills first, and this goes red.
func TestAKimiAnswerKeepsWhatItLeftOutBeforeItsSkills(t *testing.T) {
	t.Parallel()
	h := handedCheckout{name: "late", root: "/w/late"}
	for i := range 200 {
		h.skills = append(h.skills, handedSkill{name: fmt.Sprintf("late:s%d", i),
			description: strings.Repeat("d", maxSkillDescription), path: fmt.Sprintf("/h/skills/late:s%d/SKILL.md", i)})
	}
	skipped := []string{"the skill /w/late/.kimi/skills/x is left out: its name is taken"}
	answer := kimiNestedText([]handedCheckout{h}, skipped)
	skills, ok := strings.CutSuffix(answer, "\n\n"+skippedListing(skipped))
	if !ok || !strings.HasSuffix(skills, "\n"+listingCut) || len(answer) > maxListing {
		t.Errorf("the answer (%d bytes) is not the skills cut, then what was left out whole: ...%s", len(answer),
			answer[max(len(answer)-300, 0):])
	}
}
