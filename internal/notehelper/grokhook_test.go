package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// grokInput is testdata/grok/1.0.92/<name>: a hook's input as Grok Build 1.0.92
// handed it to a hook of the run's settings (captured through the runner's own
// settings, against a stand-in model).
func grokInput(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "grok", "1.0.92", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// noEnv is a hook environment the adapter reads nothing from.
func noEnv(string) string { return "" }

// grokMain is the session of every captured input: the run's own.
const grokMain = "11111111-2222-4333-8444-0000000000a2"

// grokThreadFile is a -thread-file naming session, as a run's script writes it.
func grokThreadFile(t *testing.T, session string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grok-thread")
	if err := os.WriteFile(path, []byte(session+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A GROK FIRING TAKES AT THE BRIDGE'S BOUNDARY FOR ITS EVENT AND HANDS THE ANSWER
// UNDER THAT EVENT'S NAME: PostToolUse and PostToolUseFailure after a batch,
// each answered as itself (Grok hands a failure's answer under its own name),
// and Stop at the finish, the Stop 1.0.92 fires again once its block was
// handed over (stop_hook_active) too, so a note that came since is not left
// for the review; the token alone on stderr.
//
// Mutations, each red: answer a PostToolUseFailure as a PostToolUse; take a
// Stop at the batch's boundary; take nothing at a Stop whose block is active.
func TestAGrokFiringTakesAtItsEventsBoundaryAndAnswersAsItsEvent(t *testing.T) {
	t.Parallel()
	answer := `{"crewlet_take":"[crewlet take t-1]","hookSpecificOutput":{"hookEventName":"PostToolUse",` +
		`"additionalContext":"## A note from ada\n\nUse main.\n\n[crewlet take t-1]"}}`
	for _, tc := range []struct {
		input, boundary, event string
	}{
		{"posttooluse-mcp.json", boundaryBatch, "PostToolUse"},
		{"posttoolusefailure-mcp.json", boundaryBatch, "PostToolUseFailure"},
		{"stop.json", boundaryFinish, ""},
		{"stop-active.json", boundaryFinish, ""},
	} {
		m := newFakeMCP(t, answer)
		code, stdout, stderr := fire(t, []string{"-adapter", "grok-hook", "-config", takeConfigFile(t, m),
			"-thread-file", grokThreadFile(t, grokMain)}, grokInput(t, tc.input), noEnv)
		if code != exitDone || stderr != "[crewlet take t-1]\n" {
			t.Fatalf("%s: exit %d, stderr %q; want 0 and the token alone", tc.input, code, stderr)
		}
		if len(m.calls) != 1 || m.calls[0]["event"] != tc.boundary {
			t.Errorf("%s: took %v, want one take at %s", tc.input, m.calls, tc.boundary)
		}
		var got struct {
			Output struct {
				Event string `json:"hookEventName"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(stdout), &got); err != nil || strings.Contains(stdout, "crewlet_take") {
			t.Fatalf("%s: stdout %q (%v), want the answer without its token's field", tc.input, stdout, err)
		}
		if tc.event != "" && got.Output.Event != tc.event {
			t.Errorf("%s: answered as %q, want %q", tc.input, got.Output.Event, tc.event)
		}
	}
}

// A GROK SUBAGENT'S FIRING TAKES NOTHING: hooks fire in a subagent's session
// too, and a note handed there binds one delegated piece of work and lands in a
// record no receipt reads, so only a firing in the session the run's script
// named takes; one whose hook names no such file takes nothing either.
//
// Mutations, each red: drop the session check; take where -thread-file is
// unset or unreadable.
func TestAGrokSubagentsFiringTakesNothing(t *testing.T) {
	t.Parallel()
	answer := `{"crewlet_take":"[crewlet take t-1]","hookSpecificOutput":{"hookEventName":"PostToolUse",` +
		`"additionalContext":"Use main.\n\n[crewlet take t-1]"}}`
	child := strings.ReplaceAll(grokInput(t, "posttooluse-mcp.json"), grokMain, "01a205c8-9b2b-79e3-9a56-a4594d1d97c3")
	for _, tc := range []struct {
		name, input string
		thread      []string
		takes       int
	}{
		{"the main session's", grokInput(t, "posttooluse-mcp.json"), []string{"-thread-file", grokThreadFile(t, grokMain)}, 1},
		{"a subagent's", child, []string{"-thread-file", grokThreadFile(t, grokMain)}, 0},
		{"a subagent's stop", strings.ReplaceAll(grokInput(t, "stop.json"), grokMain, "01a205c8-9b2b-79e3-9a56-a4594d1d97c3"),
			[]string{"-thread-file", grokThreadFile(t, grokMain)}, 0},
		{"no thread file", grokInput(t, "posttooluse-mcp.json"), nil, 0},
		{"an unreadable thread file", grokInput(t, "posttooluse-mcp.json"),
			[]string{"-thread-file", filepath.Join(t.TempDir(), "absent")}, 0},
	} {
		m := newFakeMCP(t, answer)
		args := append([]string{"-adapter", "grok-hook", "-config", takeConfigFile(t, m)}, tc.thread...)
		code, stdout, _ := fire(t, args, tc.input, noEnv)
		if code != exitDone || len(m.calls) != tc.takes || (tc.takes == 0) != (stdout == "") {
			t.Errorf("%s: exit %d, %d takes, answer %q; want %d takes", tc.name, code, len(m.calls), stdout, tc.takes)
		}
	}
}

// A GROK SKILL HOOK REPORTS A SKILL THE MODEL READ, BY ITS LISTED NAME, AND NO
// FILE IT FAILED TO READ: 1.0.92 fires PostToolUse for a read_file of a missing
// file too, its response a FileNotFound where a read's is a FileContent.
//
// Mutation: report on the event alone, or name the skill by its file's path.
func TestAGrokSkillHookReportsAReadSkillAndNoMissingOne(t *testing.T) {
	t.Parallel()
	m := newFakeMCP(t, "")
	cfg := takeConfigFile(t, m)
	for _, input := range []string{"posttooluse-skill-read.json", "posttooluse-skill-missing.json"} {
		_, stdout, _ := fire(t, []string{"-adapter", "grok-hook", "-config", cfg, "-skill-loads", "-loads-only"},
			grokInput(t, input), noEnv)
		if stdout != "" {
			t.Errorf("%s: a loads-only hook answered %q", input, stdout)
		}
	}
	if len(m.skills) != 1 || m.skills[0] != "crewlet:probe-skill" || len(m.calls) != 0 {
		t.Fatalf("skills %v, takes %d; want crewlet:probe-skill reported once and no take", m.skills, len(m.calls))
	}
}

// ONLY A RUN'S OWN SKILL FILE IS A SKILL'S LOAD: a file directly under the run's
// skills directory (the take config's) whose directory is the run's namespace,
// read by an absolute path or one relative to the run's directory; a file of
// the same shape under any other directory, a checkout's own `.grok/skills`
// included, is none, and so is every file where the config names no directory.
//
// Mutations, each red: drop the namespace check; match the directory by its
// suffix (`/.grok/skills/`) instead of the run's own; report with no
// directory configured.
func TestOnlyARunsOwnSkillFileIsASkillsLoad(t *testing.T) {
	t.Parallel()
	const skills = "/home/user/.grok/skills"
	for _, tc := range []struct{ dir, cwd, file, want string }{
		{skills, "/home/user", "/home/user/.grok/skills/crewlet:fix/SKILL.md", "crewlet:fix"},
		{skills, "/home/user", ".grok/skills/crewlet:fix/SKILL.md", "crewlet:fix"},
		{skills + "/", "/home/user", "/home/user/.grok/skills/crewlet:fix/SKILL.md", "crewlet:fix"},
		{skills, "/home/user", "/home/user/.grok/skills/fix/SKILL.md", ""},
		{skills, "/home/user", "/home/user/.grok/skills/crewlet:fix/notes.md", ""},
		{skills, "/home/user", "/home/user/repo/.grok/rules/crewlet:fix/SKILL.md", ""},
		{skills, "/home/user", "/home/user/workspace/repo/.grok/skills/crewlet:fix/SKILL.md", ""},
		{skills, "/home/user/workspace/repo", ".grok/skills/crewlet:fix/SKILL.md", ""},
		{"", "/home/user", "/home/user/.grok/skills/crewlet:fix/SKILL.md", ""},
		{"", "", "crewlet:fix/SKILL.md", ""},
		{skills, "/home/user", "", ""},
	} {
		if got := grokSkillRead(tc.dir, tc.cwd, tc.file); got != tc.want {
			t.Errorf("grokSkillRead(%q, %q, %q) = %q, want %q", tc.dir, tc.cwd, tc.file, got, tc.want)
		}
	}
}

// THE TAKE CONFIG'S SKILLS DIRECTORY IS READ UNDER THE KEY THE EXECUTOR WRITES
// IT BY (`skillsDir`, its oneShotTake's tag, which its own case holds), so a
// Grok run's skill reads are matched against the directory its launch wrote.
//
// Mutation: rename either tag.
func TestTheTakeConfigsSkillsDirectoryIsReadUnderTheExecutorsKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "take.json")
	if err := os.WriteFile(path, []byte(`{"skillsDir":"/home/box/.grok/skills"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := readTakeConfig(path); err != nil || c.SkillsDir != "/home/box/.grok/skills" {
		t.Errorf("the config read as %+v (%v), want its skills directory", c, err)
	}
}
