package executor_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// WHAT A CHECKOUT BELOW THE START DIRECTORY BRINGS A QWEN CODE RUN: on a real
// box, a checkout at ./repo has its QWEN.md, AGENTS.md and always-on rules
// written under its root after the home's own ~/.qwen/QWEN.md as Qwen Code
// starts (a scoped rule not, `**` included), its skills in ~/.qwen/skills as `repo-<skill>`,
// and the run's settings hold the checkouts' PostToolUse hook; after the run
// the home's QWEN.md is its own again and the copies are gone.
//
// Mutation: write no sections, hand a scoped rule, join the copy with a
// colon, leave the hook out, or skip the restore, and a check goes red.
func TestWhatACheckoutBelowTheStartDirectoryBringsAQwenRun(t *testing.T) {
	t.Parallel()
	runner, box := qwenBox(t, `o=$HOME/observed; mkdir -p "$o"; cat "$HOME/.qwen/QWEN.md" > "$o/qwen-md"
ls "$HOME/.qwen/skills" > "$o/skills"; cat "$HOME/.crewlet/qwen-settings.json" > "$o/settings"
echo '{"type":"crewlet_qwen_end","stop_reason":"end_turn"}'
`)
	p := executor.PathsFor(box)
	plant(t, box, map[string]string{
		".qwen/QWEN.md":                              "HOME-OWN\n",
		"workspace/repo/.git/HEAD":                   "ref: refs/heads/main\n",
		"workspace/repo/.git/index":                  "DIRC",
		"workspace/repo/.git/refs/heads/main":        strings.Repeat("0", 40) + "\n",
		"workspace/repo/QWEN.md":                     "REPO-QWEN\n",
		"workspace/repo/AGENTS.md":                   "REPO-AGENTS\n",
		"workspace/repo/.qwen/rules/style.md":        "REPO-RULE\n",
		"workspace/repo/.qwen/rules/ts.md":           "---\npaths: [\"src/**\"]\n---\nTS-RULE\n",
		"workspace/repo/.qwen/rules/all.md":          "---\npaths: \"**\"\n---\nALL-RULE\n",
		"workspace/repo/.qwen/skills/check/SKILL.md": "---\nname: check\ndescription: Checks.\n---\nCheck.\n",
	})
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", Model: "qwen3-coder-plus",
		Env: map[string]string{"OPENAI_API_KEY": "key-placeholder"}})
	if !res.Success {
		t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, p.Err()))
	}
	work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), sandbox.WorkspaceSubdir))
	if err != nil {
		t.Fatal(err)
	}
	seen := func(name string) string { return boxFile(t, box, box.Home()+"/observed/"+name) }
	want := "HOME-OWN\n\n\nfor `repo` with root " + work + "/repo\n\nREPO-QWEN\n\n\nREPO-AGENTS\n\n\nREPO-RULE"
	if got := seen("qwen-md"); got != want {
		t.Errorf("Qwen Code started on a home QWEN.md of\n%q\nwant\n%q", got, want)
	}
	if skills := seen("skills"); skills != "repo-check\n" {
		t.Errorf("the home's skills as Qwen Code started are %q, want repo-check alone", skills)
	}
	if settings := seen("settings"); !strings.Contains(settings, "-adapter qwen-nested -config") {
		t.Errorf("the run's settings hold no checkouts' hook:\n%s", settings)
	}
	if got := boxFile(t, box, p.QwenDir()+"/QWEN.md"); got != "HOME-OWN\n" {
		t.Errorf("the home's QWEN.md reads %q after the run, want its own back", got)
	}
	if left := boxFile(t, box, p.QwenDir()+"/skills/repo-check/SKILL.md"); left != "" {
		t.Errorf("the checkout's skill stayed in the home after the run:\n%s", left)
	}
}

// A QWEN CODE RUN HANDS A CHECKOUT THE CONTEXT FILES ITS SETTINGS NAME, as
// Qwen Code 0.28.4 settles context.fileName (measured, each row): the highest
// settings file holding the key decides, comments allowed, even null, empty
// or an empty list, which leave QWEN.md then AGENTS.md; names trimmed, a blank
// one dropped, one with a slash a path from the root; the sections go into
// the first of the home's own files that is there. A value Qwen Code stops on
// (no string), or blanks alone, refuses the run, naming the file.
//
// Mutation: ignore the settings, let the home's win, skip a null or empty
// one, keep a blank name, drop a slashed one, write the sections elsewhere,
// or start a run Qwen Code stops on, and a row goes red.
func TestAQwenRunHandsACheckoutTheContextFilesItsSettingsName(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		home, work, want, refused string
	}{
		"the working directory's list, commented": {`{"context":{"fileName":["AGENTS.md"]}}`,
			"// the working directory's\n{\"context\":{\"fileName\":[\"CTX.md\",\"QWEN.md\"]}}\n",
			"REPO-CTX\n\n\nREPO-QWEN\n", ""},
		"null over the home's":       {`{"context":{"fileName":["CTX.md"]}}`, `{"context":{"fileName":null}}`, "REPO-QWEN\n\n\nREPO-AGENTS\n", ""},
		"an empty name":              {`{"context":{"fileName":["CTX.md"]}}`, `{"context":{"fileName":""}}`, "REPO-QWEN\n\n\nREPO-AGENTS\n", ""},
		"an empty list":              {`{"context":{"fileName":["CTX.md"]}}`, `{"context":{"fileName":[]}}`, "REPO-QWEN\n\n\nREPO-AGENTS\n", ""},
		"a blank entry and a padded": {`{}`, `{"context":{"fileName":["", " CTX.md "]}}`, "REPO-CTX\n", ""},
		"a path below the root":      {`{}`, `{"context":{"fileName":["sub/X.md"]}}`, "REPO-SUBX\n", ""},
		"no string":                  {`{}`, `{"context":{"fileName":[5, "CTX.md"]}}`, "", "which Qwen Code 0.28.4 stops on"},
		"blanks alone":               {`{}`, `{"context":{"fileName":["", " "]}}`, "", "blank names only"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := qwenBox(t, `o=$HOME/observed; mkdir -p "$o"; cat "$HOME/.qwen/QWEN.md" "$HOME/.qwen/CTX.md" "$HOME/.qwen/sub/X.md" > "$o/home-md" 2> /dev/null
echo '{"type":"crewlet_qwen_end","stop_reason":"end_turn"}'
`)
			plant(t, box, map[string]string{
				".qwen/QWEN.md":                       "HOME-OWN\n",
				".qwen/settings.json":                 tc.home,
				"workspace/.qwen/settings.json":       tc.work,
				"workspace/repo/.git/HEAD":            "ref: refs/heads/main\n",
				"workspace/repo/.git/index":           "DIRC",
				"workspace/repo/.git/refs/heads/main": strings.Repeat("0", 40) + "\n",
				"workspace/repo/CTX.md":               "REPO-CTX\n",
				"workspace/repo/QWEN.md":              "REPO-QWEN\n",
				"workspace/repo/AGENTS.md":            "REPO-AGENTS\n",
				"workspace/repo/sub/X.md":             "REPO-SUBX\n",
			})
			_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", Model: "qwen3-coder-plus",
				Env: map[string]string{"OPENAI_API_KEY": "key-placeholder"}})
			p := executor.PathsFor(box)
			stderr := boxFile(t, box, p.Err())
			if tc.refused != "" {
				if res.Success || !strings.Contains(stderr, tc.refused) || !strings.Contains(stderr, "/workspace/.qwen/settings.json") {
					t.Fatalf("the run succeeded: %v, want it refused saying %q, naming the file:\n%s", res.Success, tc.refused, stderr)
				}
				return
			}
			if !res.Success {
				t.Fatalf("the run failed: %s\n%s", res.Error, stderr)
			}
			work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), sandbox.WorkspaceSubdir))
			if err != nil {
				t.Fatal(err)
			}
			sections := "for `repo` with root " + work + "/repo\n\n" + tc.want
			if got := boxFile(t, box, box.Home()+"/observed/home-md"); !strings.Contains(got, sections) {
				t.Errorf("Qwen Code started on home context files of\n%q\nwant them to hold\n%q", got, sections)
			}
		})
	}
}
