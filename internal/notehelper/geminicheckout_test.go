package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// launchWith runs checkout-launch in r's working directory under spec, its
// nested adapter handing skills alone where skillsOnly, listing each root in
// roots where it names a file.
func (r repoRun) launchWith(t *testing.T, spec checkoutSpec, skillsOnly bool, roots string) {
	t.Helper()
	raw, _ := json.Marshal(launchConfig{State: r.state, Agents: r.agents, NestedSkillsOnly: skillsOnly, Roots: roots,
		checkoutSpec: spec})
	config := filepath.Join(t.TempDir(), "launch.json")
	if err := os.WriteFile(config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if err := checkoutLaunch(config, r.work, strings.NewReader(""), &errs); err != nil || errs.Len() > 0 {
		t.Fatalf("the launch failed: %v\n%s", err, errs.String())
	}
}

// fireAs runs one firing of adapter off the state the launch wrote, and is
// the hook event it answered and the context it handed.
func (r repoRun) fireAs(t *testing.T, adapter func(context.Context, flags, io.Reader, io.Writer, io.Writer,
	func(string) string) int) (string, string) {
	t.Helper()
	var out, errs bytes.Buffer
	if code := adapter(context.Background(), flags{config: filepath.Join(r.state, "nested.json")},
		strings.NewReader(`{"hook_event_name":"x","tool_name":"run_shell_command"}`), &out, &errs, os.Getenv); code != exitDone ||
		errs.Len() > 0 {
		t.Fatalf("the firing exited %d: %s", code, errs.String())
	}
	var answer struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &answer); err != nil {
		t.Fatalf("the firing answered %q (%v)", out.String(), err)
	}
	return answer.HookSpecificOutput.HookEventName, answer.HookSpecificOutput.AdditionalContext
}

// A GEMINI CLI FIRING HANDS A CLONE'S SKILLS ALONE, AS `<tag>-<skill>`, AND
// THE LAUNCH LISTS EACH CHECKOUT'S ROOT: the launch writes every checkout it
// handed into the roots file, a line each, for the run's
// --include-directories, and copies its skills joined by a hyphen, as Gemini
// CLI 0.70.0 lists `a:b` (measured); a clone's firing answers AfterTool with
// the listing and none of its GEMINI.md, which Gemini CLI loads itself on a
// read.
//
// Mutation: drop the roots file, join with a colon, answer PostToolUse, or
// hand the clone's instructions, and a check goes red.
func TestAGeminiFiringHandsAClonesSkillsAloneAndTheLaunchListsTheRoots(t *testing.T) {
	t.Parallel()
	r := newRepoRun(t, map[string]string{"repo/.git/": "", "repo/GEMINI.md": "REPO\n",
		"repo/.agents/skills/check/SKILL.md": "---\nname: check\ndescription: Checks.\n---\n"})
	spec := checkoutSpec{SkillDirs: []skillDirSlot{{Dir: ".agents/skills"}, {Dir: ".gemini/skills"}}, Skills: r.skills,
		Copied: filepath.Join(filepath.Dir(r.skills), "copied"), Reserved: []string{"crewlet"}, SkillSep: "-"}
	roots := filepath.Join(t.TempDir(), "roots")
	r.launchWith(t, spec, true, roots)
	if got := read(t, roots); got != filepath.Join(r.workdir(t), "repo")+"\n" {
		t.Errorf("the roots file reads %q", got)
	}
	if _, err := os.Stat(filepath.Join(r.skills, "repo-check", "SKILL.md")); err != nil {
		t.Errorf("the launch copied no repo-check: %v", err)
	}
	r.add(t, map[string]string{"late/.git/": "", "late/GEMINI.md": "LATE-INSTRUCTIONS\n",
		"late/.gemini/skills/fix/SKILL.md": "---\nname: fix\ndescription: Fixes.\n---\n"})
	event, got := r.fireAs(t, runGeminiNested)
	want := "# Skills of " + filepath.Join(r.workdir(t), "late") + "\n\nEach is a set of local instructions to follow, " +
		"stored in its `SKILL.md` file; to use one, read that file.\n- late-fix: Fixes. (file: " + r.skills + "/late-fix/SKILL.md)"
	if event != "AfterTool" || got != want {
		t.Errorf("the firing answered %s with\n%q\nwant AfterTool with\n%q", event, got, want)
	}
}
