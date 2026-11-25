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
	want := "HOME-OWN\n\n\nfor `repo` with root " + work + "/repo\n\nREPO-QWEN\n\n\nREPO-AGENTS\n\n\nREPO-RULE\n"
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
