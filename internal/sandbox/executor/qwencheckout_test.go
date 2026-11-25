package executor_test

import (
	"maps"
	"os"
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
// settings file holding the key decides, even null, empty or an empty list
// (QWEN.md then AGENTS.md); names trimmed, a blank one dropped, one with a
// slash a path from the root, its home directory made and removed after the
// run where the home had none; blanks alone hand none, said once. The
// sections go into the first of the home's own files that is there. A value
// Qwen Code stops on (no string) refuses the run, naming the file.
//
// Mutation: ignore the settings, let the home's win, skip a null one, keep a
// blank name, drop a slashed one, keep or remove a directory amiss, refuse
// or hand blanks alone, or start a run Qwen Code stops on, and a row goes red.
func TestAQwenRunHandsACheckoutTheContextFilesItsSettingsName(t *testing.T) {
	t.Parallel()
	const blank = "context.fileName to blank names only, so Qwen Code loads no context file"
	for name, tc := range map[string]struct {
		home, work, want, refused, said, gone, kept string
		own                                         map[string]string
	}{
		"the working directory's list, commented": {home: `{"context":{"fileName":["AGENTS.md"]}}`,
			work: "// the working directory's\n{\"context\":{\"fileName\":[\"CTX.md\",\"QWEN.md\"]}}\n",
			want: "REPO-CTX\n\n\nREPO-QWEN\n"},
		"null over the home's": {home: `{"context":{"fileName":["CTX.md"]}}`, work: `{"context":{"fileName":null}}`,
			want: "REPO-QWEN\n\n\nREPO-AGENTS\n"},
		"an empty name": {home: `{"context":{"fileName":["CTX.md"]}}`, work: `{"context":{"fileName":""}}`,
			want: "REPO-QWEN\n\n\nREPO-AGENTS\n"},
		"an empty list": {home: `{"context":{"fileName":["CTX.md"]}}`, work: `{"context":{"fileName":[]}}`,
			want: "REPO-QWEN\n\n\nREPO-AGENTS\n"},
		"a blank entry and a padded": {home: `{}`, work: `{"context":{"fileName":["", " CTX.md "]}}`, want: "REPO-CTX\n"},
		"a path below the root": {home: `{}`, work: `{"context":{"fileName":["a/b/X.md"]}}`, want: "REPO-ABX\n",
			gone: "a"},
		"a path below the root, the home's directory there": {home: `{}`,
			work: `{"context":{"fileName":["sub/X.md"]}}`, want: "REPO-SUBX\n", kept: "sub/OWN.md",
			own: map[string]string{".qwen/sub/OWN.md": "HOME-SUB\n"}},
		"no string": {home: `{}`, work: `{"context":{"fileName":[5, "CTX.md"]}}`,
			refused: "which Qwen Code 0.28.4 stops on"},
		"blanks alone": {home: `{}`, work: `{"context":{"fileName":["", " "]}}`, said: blank},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := qwenBox(t, `o=$HOME/observed; mkdir -p "$o"; q=$HOME/.qwen
cat "$q/QWEN.md" "$q/CTX.md" "$q/sub/X.md" "$q/a/b/X.md" > "$o/home-md" 2> /dev/null
echo '{"type":"crewlet_qwen_end","stop_reason":"end_turn"}'
`)
			files := map[string]string{
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
				"workspace/repo/a/b/X.md":             "REPO-ABX\n",
			}
			maps.Copy(files, tc.own)
			plant(t, box, files)
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
			if n := strings.Count(stderr, blank); n != map[bool]int{false: 0, true: 1}[tc.said != ""] ||
				(n == 1 && !strings.Contains(stderr, "/workspace/.qwen/settings.json sets context.fileName")) {
				t.Errorf("the run's error stream says the blank names %d times, naming the file and key once where "+
					"they are blank:\n%s", n, stderr)
			}
			got := boxFile(t, box, box.Home()+"/observed/home-md")
			if tc.want == "" {
				if got != "HOME-OWN\n" {
					t.Errorf("Qwen Code started on home context files of\n%q\nwant the home's own alone", got)
				}
			} else {
				work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), sandbox.WorkspaceSubdir))
				if err != nil {
					t.Fatal(err)
				}
				if sections := "for `repo` with root " + work + "/repo\n\n" + tc.want; !strings.Contains(got, sections) {
					t.Errorf("Qwen Code started on home context files of\n%q\nwant them to hold\n%q", got, sections)
				}
			}
			if tc.gone != "" {
				if _, err := os.Lstat(filepath.Join(p.QwenDir(), tc.gone)); !os.IsNotExist(err) {
					t.Errorf("~/.qwen/%s, which the run made, is left after it (%v)", tc.gone, err)
				}
			}
			if tc.kept != "" {
				if got := boxFile(t, box, filepath.Join(p.QwenDir(), tc.kept)); got != "HOME-SUB\n" {
					t.Errorf("the home's own ~/.qwen/%s reads %q after the run, want it kept", tc.kept, got)
				}
			}
			if entries, _ := os.ReadDir(filepath.Dir(p.QwenHomeInstructions())); len(entries) > 0 {
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), filepath.Base(p.QwenHomeInstructions())) {
						t.Errorf("the run left %s behind", e.Name())
					}
				}
			}
		})
	}
}
