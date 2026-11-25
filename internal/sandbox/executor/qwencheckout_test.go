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
// starts (a scoped rule not, `**` included), its skills in ~/.qwen/skills as
// `repo-<skill>`, the run's settings hold the checkouts' PostToolUse hook and
// its config the firing's 192 KiB bound; after the run the home's QWEN.md is
// its own again and the copies are gone.
//
// Mutation: write no sections, hand a scoped rule, join the copy with a
// colon, leave the hook out, skip the restore, or drop the firing's bound on
// the way to the hook's config, and a check goes red.
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
	if nested := boxFile(t, box, p.QwenCheckouts()+"/nested.json"); !strings.Contains(nested, `"firingLimit":196608`) {
		t.Errorf("the hook's config holds no firingLimit of 192 KiB:\n%s", nested)
	}
}

// A QWEN CODE RUN HANDS A CHECKOUT THE CONTEXT FILES ITS SETTINGS NAME, as
// Qwen Code 0.28.4 settles context.fileName (measured, each row): the highest
// settings file holding the key decides, even null, empty or an empty list
// (QWEN.md then AGENTS.md); names trimmed, a blank one dropped, one with a
// slash a path from the root; blanks alone hand none, said once. The sections
// go into the first of the home's own files that is there, else the first,
// and no other; a directory made for it is removed after the run, one the home
// had, empty or not, kept. A value Qwen Code stops on refuses the run.
//
// Mutation: ignore the settings, let the home's win, skip a null one, keep a
// blank name, drop a slashed one, write the sections elsewhere, keep or remove
// a directory amiss, or start a run Qwen Code stops on: a row goes red.
func TestAQwenRunHandsACheckoutTheContextFilesItsSettingsName(t *testing.T) {
	t.Parallel()
	const blank = "context.fileName to blank names only, so Qwen Code loads no context file"
	candidates := []string{"QWEN.md", "CTX.md", "sub/X.md", "a/b/X.md"}
	for name, tc := range map[string]struct {
		home, work, into, want, refused, said, gone, kept string
		own                                               map[string]string
		emptyDir                                          bool
	}{
		"the working directory's list, commented": {home: `{"context":{"fileName":["AGENTS.md"]}}`,
			work: "// the working directory's\n{\"context\":{\"fileName\":[\"CTX.md\",\"QWEN.md\"]}}\n",
			into: "QWEN.md", want: "REPO-CTX\n\n\nREPO-QWEN\n"},
		"null over the home's": {home: `{"context":{"fileName":["CTX.md"]}}`, work: `{"context":{"fileName":null}}`,
			into: "QWEN.md", want: "REPO-QWEN\n\n\nREPO-AGENTS\n"},
		"an empty name": {home: `{"context":{"fileName":["CTX.md"]}}`, work: `{"context":{"fileName":""}}`,
			into: "QWEN.md", want: "REPO-QWEN\n\n\nREPO-AGENTS\n"},
		"an empty list": {home: `{"context":{"fileName":["CTX.md"]}}`, work: `{"context":{"fileName":[]}}`,
			into: "QWEN.md", want: "REPO-QWEN\n\n\nREPO-AGENTS\n"},
		"a blank entry and a padded": {home: `{}`, work: `{"context":{"fileName":["", " CTX.md "]}}`,
			into: "CTX.md", want: "REPO-CTX\n"},
		"a path below the root": {home: `{}`, work: `{"context":{"fileName":["a/b/X.md"]}}`,
			into: "a/b/X.md", want: "REPO-ABX\n", gone: "a"},
		"a path below the root, the home's directory there": {home: `{}`,
			work: `{"context":{"fileName":["sub/X.md"]}}`, into: "sub/X.md", want: "REPO-SUBX\n", kept: "sub/OWN.md",
			own: map[string]string{".qwen/sub/OWN.md": "HOME-SUB\n"}},
		"a path below the root, the home's empty directory there": {home: `{}`,
			work: `{"context":{"fileName":["sub/X.md"]}}`, into: "sub/X.md", want: "REPO-SUBX\n", kept: "sub",
			emptyDir: true},
		"no string": {home: `{}`, work: `{"context":{"fileName":[5, "CTX.md"]}}`,
			refused: "which Qwen Code 0.28.4 stops on"},
		"blanks alone": {home: `{}`, work: `{"context":{"fileName":["", " "]}}`, said: blank},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := qwenBox(t, `o=$HOME/observed; q=$HOME/.qwen
for f in QWEN.md CTX.md sub/X.md a/b/X.md; do mkdir -p "$o/$(dirname "$f")"; [ ! -f "$q/$f" ] || cp "$q/$f" "$o/$f"; done
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
			p := executor.PathsFor(box)
			if tc.emptyDir {
				if err := os.MkdirAll(filepath.Join(p.QwenDir(), "sub"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", Model: "qwen3-coder-plus",
				Env: map[string]string{"OPENAI_API_KEY": "key-placeholder"}})
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
			work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), sandbox.WorkspaceSubdir))
			if err != nil {
				t.Fatal(err)
			}
			sections := "for `repo` with root " + work + "/repo\n\n"
			for _, f := range candidates {
				got := boxFile(t, box, box.Home()+"/observed/"+f)
				switch {
				case f == tc.into && !strings.HasSuffix(got, sections+tc.want):
					t.Errorf("Qwen Code started on a home %s of\n%q\nwant it to end in\n%q", f, got, sections+tc.want)
				case f != tc.into && strings.Contains(got, sections):
					t.Errorf("the sections went into the home's %s too:\n%q", f, got)
				}
			}
			if got := boxFile(t, box, box.Home()+"/observed/QWEN.md"); !strings.HasPrefix(got, "HOME-OWN\n") {
				t.Errorf("the home's QWEN.md as Qwen Code started reads %q, want its own first", got)
			}
			if tc.gone != "" {
				if _, err := os.Lstat(filepath.Join(p.QwenDir(), tc.gone)); !os.IsNotExist(err) {
					t.Errorf("~/.qwen/%s, which the run made, is left after it (%v)", tc.gone, err)
				}
			}
			if tc.kept != "" {
				if _, err := os.Stat(filepath.Join(p.QwenDir(), tc.kept)); err != nil {
					t.Errorf("the home's own ~/.qwen/%s is gone after the run: %v", tc.kept, err)
				}
			}
			heldLeft(t, p)
		})
	}
}

// heldLeft fails t where a held home file, or a mark beside it, is left after
// a Qwen Code run.
func heldLeft(t *testing.T, p executor.Paths) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Dir(p.QwenHomeInstructions()))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(p.QwenHomeInstructions())) {
			t.Errorf("the run left %s behind", e.Name())
		}
	}
}

// A KILLED QWEN CODE RUN'S MADE DIRECTORY GOES WITH THE NEXT RUN'S CLEAR: a
// run killed while it held ~/.qwen/a/b/X.md, a file and directories it made,
// left the held marks (.live naming the file, .none, .made naming ~/.qwen/a);
// the next run in the box puts the home back before it starts, the file and
// the directories it made gone, the marks with them.
//
// Mutation: keep the made record past the put-back, or never read it, and
// the check goes red.
func TestAKilledQwenRunsMadeDirectoryGoesWithTheNextRunsClear(t *testing.T) {
	t.Parallel()
	runner, box := qwenBox(t, `echo '{"type":"crewlet_qwen_end","stop_reason":"end_turn"}'
`)
	p := executor.PathsFor(box)
	held, err := filepath.Rel(box.Home(), p.QwenHomeInstructions())
	if err != nil {
		t.Fatal(err)
	}
	plant(t, box, map[string]string{
		".qwen/a/b/X.md": "for `repo` with root /gone\n\nSTALE\n",
		held + ".live":   filepath.Join(p.QwenDir(), "a/b/X.md") + "\n",
		held + ".none":   "",
		held + ".made":   filepath.Join(p.QwenDir(), "a") + "\n",
	})
	if _, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "go", Model: "qwen3-coder-plus",
		Env: map[string]string{"OPENAI_API_KEY": "key-placeholder"}}); !res.Success {
		t.Fatalf("the run failed: %s\n%s", res.Error, boxFile(t, box, p.Err()))
	}
	if _, err := os.Lstat(filepath.Join(p.QwenDir(), "a")); !os.IsNotExist(err) {
		t.Errorf("~/.qwen/a, which the killed run made, is left after the next run (%v)", err)
	}
	heldLeft(t, p)
}
