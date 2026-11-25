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
// directory's own file start at startPath (measured, rows capa, capb, fr3,
// esc1 and tp1-tp3), "" for neither: the reference the launch's count is
// held to, each body escaped as Muse escapes it.
func museReminder(own, sections, startPath, start string) string {
	var files []string
	escape := strings.NewReplacer("<", "&lt;", ">", "&gt;", "&", "&amp;", `"`, "&quot;", "'", "&apos;")
	body := func(text string) string {
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		return escape.Replace(text)
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
		files = append(files, `<rules-file scope="project" path="`+startPath+`">`+"\n"+body(start)+"</rules-file>")
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
	if got := len(museReminder(strings.Repeat("h", 20058)+"\n", "", "AGENTS.md", strings.Repeat("w", 9999)+"\n")); got != 30393 {
		t.Fatalf("the reference renders capb's files as %d bytes", got)
	}
	big := strings.Repeat("c", 30000) + "\n"
	for _, c := range []struct {
		name, own, startPath, start string
		files                       map[string]string
		extra                       int
		whole                       bool
	}{
		{"at the cap, with own text and the start directory's file", "OWN\n", "AGENTS.md", "START\n", nil, 0, true},
		{"a byte past the cap", "OWN\n", "AGENTS.md", "START\n", nil, 1, false},
		{"at the cap, alone", "", "", "", nil, 0, true},
		{"a byte past it, alone", "", "", "", nil, 1, false},
		{"own text with no last break, at the cap", "OWN", "AGENTS.md", "START", nil, 0, true},
		{"a byte past it, with no last break", "OWN", "AGENTS.md", "START", nil, 1, false},
		{"markup Muse escapes, at the cap", "a<b & 'c'\n", "AGENTS.md", "<x>\"y\"\n", nil, 0, true},
		{"markup Muse escapes, a byte past", "a<b & 'c'\n", "AGENTS.md", "<x>\"y\"\n", nil, 1, false},
		{"a start file in .agents, at the cap", "", ".agents/AGENTS.md", "START\n", nil, 0, true},
		{"a start file in .claude, a byte past", "", ".claude/CLAUDE.md", "START\n", nil, 1, false},
		{"a start AGENTS.md of whitespace beside a big CLAUDE.md, at the cap", "", "", "",
			map[string]string{"AGENTS.md": " \n\t\n", "CLAUDE.md": big}, 0, true},
		{"the same, a byte past", "", "", "", map[string]string{"AGENTS.md": " \n\t\n", "CLAUDE.md": big}, 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newRepoRun(t, c.files)
			if c.start != "" {
				r.add(t, map[string]string{c.startPath: c.start})
			}
			root := filepath.Join(r.workdir(t), "repo")
			label := "for `repo` with root " + root + "\n\n"
			size := museRulesCap - len(museReminder(c.own, label+"\n", c.startPath, c.start)) + len("\n") + c.extra
			text := strings.Repeat("x", size-1) + "\n"
			r.add(t, map[string]string{"repo/.git/": "", "repo/AGENTS.md": text})
			said := r.museLaunch(t, c.own)
			got := read(t, r.agents)
			if c.whole {
				if got != label+text || said != "" {
					t.Fatalf("the section is %d bytes (said %q), want the checkout whole", len(got), said)
				}
				if n := len(museReminder(c.own, got, c.startPath, c.start)); n != museRulesCap {
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
	if read(t, filepath.Join(r.work, "AGENTS.md")) != start || len(museReminder("HOME-OWN\n", got, "AGENTS.md", start)) > museRulesCap {
		t.Error("the start directory's own file was changed or would be cut")
	}
	if !strings.Contains(said, "the instructions of "+work+"/a are named") || strings.Contains(said, work+"/b") {
		t.Errorf("the launch said %q, want a's naming alone", said)
	}

	full := newRepoRun(t, map[string]string{"c/.git/": "", "c/AGENTS.md": "C\n"})
	own := strings.Repeat("o", museRulesCap-len(museReminder("", "", "", ""))-300) + "\n"
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
// it delivers nothing, JSON's escapes counted: a file of quotes (9 KB raw, 18
// KB escaped) and one of U+2028 after a word (12 KB raw, 24 KB escaped) fit
// the bound raw but not escaped, so each is named by path while a small one is
// still handed whole; the listings are cut, saying so.
//
// Mutation: count the context's bytes unescaped, or hand every checkout whole,
// and this goes red.
func TestAMuseAnswerStaysWithinWhatMuseReadsOfAHook(t *testing.T) {
	t.Parallel()
	for name, big := range map[string]string{"quotes": strings.Repeat("\"", 9000), "line separators": "BIG " + strings.Repeat("\u2028", 4000)} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRepoRun(t, nil)
			r.museLaunch(t, "")
			files := map[string]string{"small/.git/": "", "small/AGENTS.md": "SMALL\n",
				"big/.git/": "", "big/AGENTS.md": big + "\n"}
			for i := range 300 {
				skill := "s" + strings.Repeat("k", 40) + string(rune('a'+i%26)) + string(rune('a'+i/26))
				files["small/.agents/skills/"+skill+"/SKILL.md"] = "---\nname: x\ndescription: " +
					strings.Repeat("d", 60) + "\n---\n"
			}
			r.add(t, files)
			got := museContext(t, r.museFire(t, "s", "s"))
			work := r.workdir(t)
			for _, want := range []string{"for `big` with root " + work + "/big: read " + work + "/big/AGENTS.md",
				"for `small` with root " + work + "/small\n\n<rules-file scope=\"project\" path=\"" + work +
					"/small/AGENTS.md\">\nSMALL\n", museNestedListingCut} {
				if !strings.Contains(got, want) {
					t.Errorf("the answer does not hold %q", want)
				}
			}
			if log := read(t, filepath.Join(r.state, skippedLog)); !strings.Contains(log, "the skills of "+work+
				"/small are copied but not all listed") {
				t.Errorf("the skip log does not say the cut listing: %q", log)
			}
		})
	}
}

// A MUSE ANSWER FILLS MUSE'S CEILING TO THE BYTE: Muse Code 1.4.25 delivers a
// hook answer of 16,384 bytes and nothing of one of 16,385 (measured, rows
// hc1 and hc2), so a checkout whose whole answer is 16,384 bytes is handed
// whole and one a byte longer is named; a body's markup is escaped as Muse
// escapes a rules file's.
//
// Mutation: charge the first block a gap before it, or leave the bound a
// byte short or long, and this goes red.
func TestAMuseAnswerFillsMusesCeilingToTheByte(t *testing.T) {
	t.Parallel()
	handed := func(n int) []handedCheckout {
		return []handedCheckout{{name: "x", root: "/r", parts: []instructionPart{{path: "/r/AGENTS.md",
			text: "<&>" + strings.Repeat("a", n)}}}}
	}
	base, _ := museNestedAnswer(handed(0), nil)
	n := museAnswerBytes - len(base)
	whole, _ := museNestedAnswer(handed(n), nil)
	if len(whole) != museAnswerBytes || !strings.Contains(museContext(t, whole),
		"<rules-file scope=\"project\" path=\"/r/AGENTS.md\">\n&lt;&amp;&gt;aaa") {
		t.Errorf("an answer of the ceiling is %d bytes, opening %q; want %d, whole and escaped", len(whole),
			museContext(t, whole)[:80], museAnswerBytes)
	}
	// A LISTING ALONE IS CHARGED NO GAP BEFORE IT, a second one the blank line.
	listing := "# Skills of /r\n- a"
	if b := (&escapedBudget{left: museEscapedBytes(listing)}); b.take(listing) != listing || b.take("x") != "" {
		t.Error("a first block that fits the bound exactly was cut, or a second was taken past it")
	}
	named, _ := museNestedAnswer(handed(n+1), nil)
	if want := "for `x` with root /r: read /r/AGENTS.md for its instructions, which pass what this answer holds"; museContext(t, named) != want {
		t.Errorf("an answer a byte past the ceiling hands %q, want it named", museContext(t, named)[:min(80, len(museContext(t, named)))])
	}
}

// A FIRING WHOSE NAMES ALONE PASS THE BOUND KEEPS EVERY CHECKOUT IT DROPS: of
// 120 checkouts cloned at once, those whose names the answer cannot hold are
// claimed all the same, so each is a line in the run's skip log, and every
// one is in the answer or the log.
//
// Mutation: drop the lines for the dropped names, and this goes red.
func TestAMuseFiringPastItsBoundKeepsEveryCheckoutItDrops(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	r.museLaunch(t, "")
	files := map[string]string{}
	for i := range 120 {
		files[fmt.Sprintf("c%03d/.git/", i)] = ""
		files[fmt.Sprintf("c%03d/AGENTS.md", i)] = strings.Repeat("i", 400) + "\n"
	}
	r.add(t, files)
	got := museContext(t, r.museFire(t, "s", "s"))
	log := read(t, filepath.Join(r.state, skippedLog))
	work := r.workdir(t)
	dropped := 0
	for i := range 120 {
		root := fmt.Sprintf("%s/c%03d", work, i)
		in, kept := strings.Contains(got, root+": read "), strings.Contains(log, "the instructions of "+root+" are left out")
		if in == kept {
			t.Errorf("%s: named in the answer %v, in the skip log %v; want exactly one", root, in, kept)
		}
		if kept {
			dropped++
		}
	}
	if dropped == 0 {
		t.Error("no checkout was dropped: the case does not pass the bound")
	}
}

// A COPY A HOME SKILL IS LISTED BESIDE IS LEFT OUT, SAID: Muse Code 1.4.25
// lists a skill by its frontmatter's name and never lists two of one name
// (measured, rows fn1-fn4, so1-so6), so a copy whose name a skill of the home's
// other skill directories is listed under (here ~/.config/muse/skills/pdir,
// named late:deploy) is not made, said in the answer and the skip log once;
// one whose name is only a directory's there (late:lint, named other) is.
//
// Mutation: match directory names, or skip the homeSkills check, and this goes
// red.
func TestAMuseCopyAHomeSkillIsListedBesideIsLeftOutSaid(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, nil)
	home := filepath.Join(t.TempDir(), "config-skills")
	for dir, name := range map[string]string{"pdir": "late:deploy", "late:lint": "other"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, dir, "SKILL.md"), []byte("---\nname: \""+name+"\"\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.museLaunch(t, "", home)
	r.add(t, map[string]string{"late/.git/": "",
		"late/.agents/skills/deploy/SKILL.md": "---\nname: deploy\ndescription: Deploys.\n---\n",
		"late/.agents/skills/lint/SKILL.md":   "---\nname: lint\ndescription: Lints.\n---\n"})
	got := museContext(t, r.museFire(t, "s", "s"))
	line := "the skill " + r.workdir(t) + "/late/.agents/skills/deploy is left out: the home's " +
		filepath.Join(home, "pdir") + " is listed as late:deploy too, and Muse Code would not list both"
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

// A HOME SKILL'S NAME IS READ AS YAML READS A ONE-LINE SCALAR: a plain value
// up to a comment, trimmed; a quoted one's inside, a `#` in it kept and a
// comment after it dropped; so `name: repo:x  # mine` takes repo:x from a copy.
//
// Mutation: keep a plain value's comment, or cut a quoted value at its `#`.
func TestAHomeSkillsNameIsReadAsYAMLReadsIt(t *testing.T) {
	t.Parallel()
	for line, want := range map[string]string{
		`name: repo:x  # mine`:               "repo:x",
		`name: repo:x	# a tab before it`:     "repo:x",
		`name: repo:x#tag`:                   "repo:x#tag",
		`name: "repo:x # kept"  # a comment`: "repo:x # kept",
		`name: 'it''s # kept'`:               "it's # kept",
		`name: "repo:x"`:                     "repo:x",
		`name: # no value`:                   "",
		`name:   repo:x   `:                  "repo:x",
	} {
		got, ok := skillFrontName([]byte("---\n" + line + "\ndescription: d\n---\nbody\n"))
		if !ok || got != want {
			t.Errorf("%q reads as %q (%v), want %q", line, got, ok, want)
		}
	}
}
