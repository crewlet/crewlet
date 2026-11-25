package executor_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// WHAT A CHECKOUT BELOW THE START DIRECTORY BRINGS A GEMINI CLI RUN: on a real
// box, a checkout at ./repo is named with --include-directories, its skills
// are in ~/.gemini/skills as `repo-<skill>` as Gemini CLI starts and gone after,
// and the run's settings hold the checkouts' AfterTool hook, whose firing
// after a clone hands that clone's skills' listing; the home's GEMINI.md is
// never touched.
//
// Mutation: drop the roots, join the copy with a colon, keep the copies, or
// leave the hook out of the settings, and a check goes red.
func TestWhatACheckoutBelowTheStartDirectoryBringsAGeminiRun(t *testing.T) {
	t.Parallel()
	q := shellword.Quote
	runner, box := geminiBox(t, `o=$HOME/observed; mkdir -p "$o"; printf '%s\n' "$@" > "$o/argv"
ls "$HOME/.gemini/skills" > "$o/skills"; cp "$HOME/.gemini/settings.json" "$o/settings"
if [ -d "$HOME/clone-src" ]; then cp -R "$HOME/clone-src" ./late
  printf '%s\n' '{"hook_event_name":"AfterTool","tool_name":"run_shell_command"}' | "$HOME/.crewlet/gemini-notehelper" -adapter gemini-nested \
    -config "$HOME/.crewlet/gemini-checkouts/nested.json" > "$o/nested"; fi
cat > /dev/null; echo '{"type":"result","status":"success"}'
`)
	p := executor.PathsFor(box)
	plant(t, box, map[string]string{
		".gemini/GEMINI.md":                            "HOME-OWN\n",
		"workspace/repo/.git/HEAD":                     "ref: refs/heads/main\n",
		"workspace/repo/.git/index":                    "DIRC",
		"workspace/repo/.git/refs/heads/main":          strings.Repeat("0", 40) + "\n",
		"workspace/repo/GEMINI.md":                     "REPO-GEMINI\n",
		"workspace/repo/.gemini/skills/check/SKILL.md": "---\nname: check\ndescription: Checks.\n---\nCheck.\n",
		"clone-src/.git/HEAD":                          "ref: refs/heads/main\n",
		"clone-src/.git/index":                         "DIRC",
		"clone-src/.git/refs/heads/main":               strings.Repeat("0", 40) + "\n",
		"clone-src/.agents/skills/fix/SKILL.md":        "---\nname: fix\ndescription: Fixes.\n---\nFix.\n",
	})
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", Model: "gemini-2.5-pro",
		Env: map[string]string{"GEMINI_API_KEY": "key-placeholder"}})
	if !res.Success {
		t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, p.Err()))
	}
	work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), sandbox.WorkspaceSubdir))
	if err != nil {
		t.Fatal(err)
	}
	seen := func(name string) string { return boxFile(t, box, box.Home()+"/observed/"+name) }
	if argv := seen("argv"); !strings.Contains(argv, "--include-directories\n"+work+"/repo\n") {
		t.Errorf("Gemini CLI started without the checkout named:\n%s", argv)
	}
	if skills := seen("skills"); !strings.Contains(skills, "repo-check\n") || strings.Contains(skills, "repo:check") {
		t.Errorf("the home's skills as Gemini CLI started are %q, want repo-check among them", skills)
	}
	if settings := seen("settings"); !strings.Contains(settings, `"name": "crewlet-checkouts"`) ||
		!strings.Contains(settings, "-adapter gemini-nested -config "+q(p.GeminiCheckouts()+"/nested.json")) {
		t.Errorf("the run's settings hold no checkouts' hook:\n%s", settings)
	}
	if nested := seen("nested"); !strings.Contains(nested, `"hookEventName":"AfterTool"`) ||
		!strings.Contains(nested, "- late-fix: Fixes.") {
		t.Errorf("a clone's firing handed %q, want its skill's listing", nested)
	}
	if got := boxFile(t, box, p.GeminiDir()+"/GEMINI.md"); got != "HOME-OWN\n" {
		t.Errorf("the home's GEMINI.md reads %q after the run, want it untouched", got)
	}
	if left := boxFile(t, box, p.GeminiSkills()+"/repo-check/SKILL.md"); left != "" {
		t.Errorf("the checkout's skill stayed in the home after the run:\n%s", left)
	}
}

// A GEMINI CLI RUN WHOSE ONLY HOOK WOULD BE ITS CHECKOUTS' STARTS WITHOUT IT
// WHERE THE BOX SWITCHES IT OFF (D709): `hooksConfig.disabled` naming
// crewlet-checkouts, or `hooksConfig.enabled` false, starts a bare run, its
// error stream saying once, naming the file and the key, that a checkout
// cloned during it is not handed its skills; a run whose notes ride a hook is
// still refused, saying no such line.
//
// Mutation: refuse the bare run, log nothing, or log on a refused run, and a
// row goes red.
func TestAGeminiRunWhoseOnlyHookIsItsCheckoutsStartsWithoutIt(t *testing.T) {
	t.Parallel()
	const line = "crewlet: checkouts cloned during the run are not handed their skills: "
	for name, tc := range map[string]struct {
		settings, why string
		notes         bool
	}{
		"the hook disabled by name": {`{"hooksConfig":{"disabled":["crewlet-checkouts"]}}`,
			"/.gemini/settings.json names the hook crewlet-checkouts in hooksConfig.disabled", false},
		"every hook off": {`{"hooksConfig":{"enabled":false}}`,
			"/.gemini/settings.json sets hooksConfig.enabled to false", false},
		"every hook off, a run with notes": {`{"hooksConfig":{"enabled":false}}`, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := geminiBox(t, observedRun)
			plant(t, box, map[string]string{".gemini/settings.json": tc.settings})
			req := sandbox.RunRequest{Brief: "go", Model: "gemini-2.5-pro",
				Env: map[string]string{"GEMINI_API_KEY": "key-placeholder"}}
			if tc.notes {
				req.MCPServers, req.Steer = loadsBridge, loadsSteer
			}
			_, res := runToEnd(t, runner, box, req)
			stderr := boxFile(t, box, executor.PathsFor(box).Err())
			if res.Success == tc.notes {
				t.Fatalf("the run succeeded: %v, want %v:\n%s", res.Success, !tc.notes, stderr)
			}
			n := strings.Count(stderr, line)
			if want := map[bool]int{false: 1, true: 0}[tc.notes]; n != want || !strings.Contains(stderr, tc.why) {
				t.Errorf("the skip log said it %d times, want %d naming %q:\n%s", n, want, tc.why, stderr)
			}
		})
	}
}
