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

// museSpec is the spec a Muse Code run's launch gives the core
// (internal/sandbox/executor's museCheckouts), its skills in skills and the
// home's other skill directories in home.
func museSpec(skills string, home ...string) checkoutSpec {
	spec := checkoutSpec{Instructions: names([]string{"AGENTS.md", "CLAUDE.md", ".agents/AGENTS.md", ".claude/CLAUDE.md"}),
		SkillDirs: []skillDirSlot{{Dir: ".agents/skills"}, {Dir: ".claude/skills"}, {Dir: ".codex/skills"}},
		Skills:    skills, Copied: filepath.Join(filepath.Dir(skills), "copied"), Reserved: []string{"crewlet"}}
	for _, dir := range home {
		spec.HomeSkills = append(spec.HomeSkills, skillDirSlot{Dir: dir})
	}
	return spec
}

// museLaunch runs checkout-launch in r's working directory under Muse's spec
// and budget, the home file opening with own ("" for no own file): what it
// said on its error stream.
func (r repoRun) museLaunch(t *testing.T, own string, home ...string) string {
	t.Helper()
	ownFile := filepath.Join(t.TempDir(), "own.md")
	if own != "" {
		if err := os.WriteFile(ownFile, []byte(own), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents,
		Budget: &launchBudget{Form: budgetMuse, Own: ownFile}, checkoutSpec: museSpec(r.skills, home...)})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if err := checkoutLaunch(config, r.work, &errs); err != nil {
		t.Fatalf("the launch failed: %v\n%s", err, errs.String())
	}
	return errs.String()
}

// museReminder is the rules reminder Muse Code 1.4.25 renders of the home
// file, own then the sections as the run's script joins them, and the start
// directory's own AGENTS.md (measured, rows capa, capb and fr3), "" for
// neither: the reference the launch's count is held to.
func museReminder(own, sections, start string) string {
	var files []string
	body := func(text string) string {
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		return text
	}
	home := sections
	if own != "" && sections != "" {
		home = own + "\n\n" + sections
	} else if own != "" {
		home = own
	}
	if home != "" {
		files = append(files, `<rules-file scope="user" path="$CONFIG_DIR/AGENTS.md">`+"\n"+body(home)+"</rules-file>")
	}
	if start != "" {
		files = append(files, `<rules-file scope="project" path="AGENTS.md">`+"\n"+body(start)+"</rules-file>")
	}
	if len(files) == 0 {
		return ""
	}
	return "<system-reminder source=\"rules-file\">\nMuse Code loaded standing rules at session open. Follow " +
		"higher-priority instructions first. If user and project rules conflict, project rules win.\n\n" +
		strings.Join(files, "\n\n") + "\n</system-reminder>"
}

// THE LAUNCH COUNTS THE REMINDER AS MUSE CODE 1.4.25 RENDERS IT (measured, row
// capb: 186 + 55 + 20,059 + 13 + 2 + 46 + 10,000 + 13 + 19 = 30,393), so a
// checkout whose section brings the reminder to exactly 65,536 bytes is handed
// whole and one a byte longer is named instead, the home's own text and the
// start directory's AGENTS.md never cut for it.
//
// Mutation: count the opening, a tag, the close or the gap between files a
// byte short or long, or drop the line break Muse adds, and a row goes red.
func TestTheMuseLaunchCountsTheReminderAsMuseRendersIt(t *testing.T) {
	t.Parallel()
	if got := len(museReminder(strings.Repeat("h", 20058)+"\n", "", strings.Repeat("w", 9999)+"\n")); got != 30393 {
		t.Fatalf("the reference renders capb's files as %d bytes", got)
	}
	for _, c := range []struct {
		name, own, start string
		extra            int
		whole            bool
	}{
		{"exactly at the cap, with own text and the start directory's file", "OWN\n", "START\n", 0, true},
		{"a byte past the cap", "OWN\n", "START\n", 1, false},
		{"exactly at the cap, alone", "", "", 0, true},
		{"a byte past it, alone", "", "", 1, false},
		{"own text with no last break, at the cap", "OWN", "START", 0, true},
		{"a byte past it, with no last break", "OWN", "START", 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newRepoRun(t, nil)
			if c.start != "" {
				r.add(t, map[string]string{"AGENTS.md": c.start})
			}
			root := filepath.Join(r.workdir(t), "repo")
			label := "for `repo` with root " + root + "\n\n"
			size := museRulesCap - len(museReminder(c.own, label+"\n", c.start)) + len("\n") + c.extra
			text := strings.Repeat("x", size-1) + "\n"
			r.add(t, map[string]string{"repo/.git/": "", "repo/AGENTS.md": text})
			said := r.museLaunch(t, c.own)
			got := read(t, r.agents)
			if c.whole {
				if got != label+text || said != "" {
					t.Fatalf("the section is %d bytes (said %q), want the checkout whole", len(got), said)
				}
				if n := len(museReminder(c.own, got, c.start)); n != museRulesCap {
					t.Fatalf("the reminder renders %d bytes, want exactly %d", n, museRulesCap)
				}
				return
			}
			want := label + "read " + root + "/AGENTS.md for its instructions, which pass what Muse Code loads " +
				"at the start beside the home's and the start directory's own"
			if got != want || !strings.Contains(said, "the instructions of "+root+" are named, not handed") {
				t.Fatalf("the section is %d bytes, opening %q (said %q), want the checkout named", len(got),
					got[:min(len(got), 200)], said)
			}
		})
	}
}

// WHOLE CHECKOUTS WHILE THEY FIT, EVERY OTHER NAMED, IN ORDER: a checkout too
// big alone is named and a later one that fits is still handed whole, the
// start directory's own AGENTS.md left as it is and the reminder within the
// cap; where even the names would cut the home's own text, the ones that do
// not fit are left out, said.
//
// Mutation: stop at the first that does not fit, or name none, and a row goes
// red.
func TestAMuseLaunchHandsWholeCheckoutsWhileTheyFitAndNamesTheRest(t *testing.T) {
	t.Parallel()
	start := "START-OWN " + strings.Repeat("w", 20000) + "\n"
	r := newRepoRun(t, map[string]string{"AGENTS.md": start,
		"a/.git/": "", "a/AGENTS.md": strings.Repeat("a", 50000) + "\n",
		"b/.git/": "", "b/CLAUDE.md": "B-SMALL\n"})
	said := r.museLaunch(t, "HOME-OWN\n")
	work := r.workdir(t)
	got := read(t, r.agents)
	want := "for `a` with root " + work + "/a\n\nread " + work + "/a/AGENTS.md for its instructions, which pass " +
		"what Muse Code loads at the start beside the home's and the start directory's own\n\nfor `b` with root " +
		work + "/b\n\nB-SMALL\n"
	if got != want {
		t.Errorf("the sections are\n%q\nwant\n%q", got, want)
	}
	if read(t, filepath.Join(r.work, "AGENTS.md")) != start || len(museReminder("HOME-OWN\n", got, start)) > museRulesCap {
		t.Error("the start directory's own file was changed or would be cut")
	}
	if !strings.Contains(said, "the instructions of "+work+"/a are named") || strings.Contains(said, work+"/b") {
		t.Errorf("the launch said %q, want a's naming alone", said)
	}

	full := newRepoRun(t, map[string]string{"c/.git/": "", "c/AGENTS.md": "C\n"})
	own := strings.Repeat("o", museRulesCap-len(museReminder("", "", ""))-300) + "\n"
	said = full.museLaunch(t, own)
	if got := read(t, full.agents); got != "" {
		t.Errorf("a home whose own text leaves no room for a name was handed %q", got)
	}
	if !strings.Contains(said, "the instructions of "+full.workdir(t)+"/c are left out: even naming them") {
		t.Errorf("the launch said %q, want c left out", said)
	}
}

// museFire runs one muse-nested firing off the state the launch wrote, its
// input's session_id session and MUSE_SESSION_ID main: what it printed.
func (r repoRun) museFire(t *testing.T, session, main string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	in := `{"hook_event_name":"PostToolUse","session_id":"` + session + `","tool_name":"bash"}`
	getenv := func(k string) string {
		if k == "MUSE_SESSION_ID" {
			return main
		}
		return ""
	}
	if code := runMuseNested(context.Background(), flags{config: filepath.Join(r.state, "nested.json")},
		strings.NewReader(in), &stdout, &stderr, getenv); code != exitDone || stderr.Len() > 0 {
		t.Fatalf("the firing exited %d: %s", code, stderr.String())
	}
	if n := stdout.Len(); n > museAnswerBytes {
		t.Fatalf("the firing printed %d bytes, past the %d Muse Code reads of a hook", n, museAnswerBytes)
	}
	return stdout.String()
}

// museContext is a muse-nested answer's context, "" for no answer.
func museContext(t *testing.T, out string) string {
	t.Helper()
	if out == "" {
		return ""
	}
	var answer struct {
		Output struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &answer); err != nil || answer.Output.Event != "PostToolUse" {
		t.Fatalf("the firing answered %q (%v)", out, err)
	}
	return answer.Output.Context
}

// A CHECKOUT CLONED DURING A MUSE RUN IS HANDED ONCE, TO THE MAIN LOOP ALONE:
// a subagent's firing (its session_id is its own thread's, not
// MUSE_SESSION_ID) prints nothing and claims nothing; the main loop's next
// firing hands the file Muse would load, wrapped as Muse wraps a rules file,
// and the skills listed by name and path; a later firing prints nothing.
//
// Mutation: hand on a subagent's firing, claim there, or drop the wrapping,
// and this goes red.
func TestACheckoutClonedDuringAMuseRunIsHandedOnceToTheMainLoop(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.museLaunch(t, "")
	r.add(t, map[string]string{"late/.git/": "", "late/CLAUDE.md": "LATE\n", "late/.claude/CLAUDE.md": "NOT-THIS\n",
		"late/.agents/skills/fix/SKILL.md": "---\nname: fix\ndescription: Fixes.\n---\nFix.\n"})
	if out := r.museFire(t, "child-thread", "main-session"); out != "" {
		t.Fatalf("a subagent's firing printed %q", out)
	}
	root := filepath.Join(r.workdir(t), "late")
	want := "for `late` with root " + root + "\n\n<rules-file scope=\"project\" path=\"" + root + "/CLAUDE.md\">\n" +
		"LATE\n</rules-file>\n\n# Skills of " + root + "\n\nEach is a set of local instructions to follow, stored in " +
		"its `SKILL.md` file; to use one, read that file.\n- late:fix: Fixes. (file: " + r.skills + "/late:fix/SKILL.md)"
	if got := museContext(t, r.museFire(t, "main-session", "main-session")); got != want {
		t.Errorf("the main loop's firing handed\n%q\nwant\n%q", got, want)
	}
	if out := r.museFire(t, "main-session", "main-session"); out != "" {
		t.Errorf("a later firing printed %q", out)
	}
}

// A MUSE ANSWER NEVER PASSES THE 16,384 BYTES MUSE READS OF A HOOK, past which
// it delivers nothing: checkouts whose files would pass it are named by path,
// whole ones still handed beside them, JSON's escapes counted (a text of
// U+2028, six bytes escaped, and of quotes); the listings are cut, saying so.
//
// Mutation: count the context's bytes unescaped, or hand every checkout whole,
// and this goes red.
func TestAMuseAnswerStaysWithinWhatMuseReadsOfAHook(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.museLaunch(t, "")
	skills := map[string]string{"small/.git/": "", "small/AGENTS.md": "SMALL\n",
		"big/.git/": "", "big/AGENTS.md": strings.Repeat(" \"", 3000) + "\n",
		"wide/.git/": "", "wide/AGENTS.md": strings.Repeat("y", 17000) + "\n"}
	for i := range 300 {
		name := "s" + strings.Repeat("k", 40) + string(rune('a'+i%26)) + string(rune('a'+i/26))
		skills["small/.agents/skills/"+name+"/SKILL.md"] = "---\nname: x\ndescription: " + strings.Repeat("d", 60) + "\n---\n"
	}
	r.add(t, skills)
	got := museContext(t, r.museFire(t, "s", "s"))
	work := r.workdir(t)
	for _, want := range []string{"for `big` with root " + work + "/big: read " + work + "/big/AGENTS.md",
		"for `wide` with root " + work + "/wide: read " + work + "/wide/AGENTS.md",
		"for `small` with root " + work + "/small\n\n<rules-file scope=\"project\" path=\"" + work + "/small/AGENTS.md\">\nSMALL\n",
		museNestedListingCut} {
		if !strings.Contains(got, want) {
			t.Errorf("the answer does not hold %q", want)
		}
	}
}

// A COPY A HOME SKILL WOULD HIDE IS LEFT OUT, SAID: Muse Code 1.4.25 lists a
// skill of ~/.config/muse/skills or of the start directory's skill directories
// before one of ~/.agents/skills (measured), so a copy whose name one of those
// holds is not made, and the firing says so in its answer and the run's skip
// log; a copy whose name nothing holds is made.
//
// Mutation: check the run's skills directory alone, and this goes red.
func TestAMuseCopyAHomeSkillWouldHideIsLeftOutSaid(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	home := filepath.Join(t.TempDir(), "config-skills")
	if err := os.MkdirAll(filepath.Join(home, "late:deploy"), 0o700); err != nil {
		t.Fatal(err)
	}
	r.museLaunch(t, "", home)
	r.add(t, map[string]string{"late/.git/": "",
		"late/.agents/skills/deploy/SKILL.md": "---\nname: deploy\ndescription: Deploys.\n---\n",
		"late/.agents/skills/lint/SKILL.md":   "---\nname: lint\ndescription: Lints.\n---\n"})
	got := museContext(t, r.museFire(t, "s", "s"))
	line := "the skill " + r.workdir(t) + "/late/.agents/skills/deploy is left out: the home's " +
		filepath.Join(home, "late:deploy") + " takes the name late:deploy in its place"
	if !strings.Contains(got, "- "+line) || !strings.Contains(got, "- late:lint: Lints.") {
		t.Errorf("the answer is\n%s\nwant the hidden copy said and the other listed", got)
	}
	if _, err := os.Lstat(filepath.Join(r.skills, "late:deploy")); err == nil {
		t.Error("the hidden copy was made")
	}
	if log := read(t, filepath.Join(r.state, skippedLog)); strings.Count(log, line) != 1 {
		t.Errorf("the skip log holds %q, want the line once", log)
	}
}
