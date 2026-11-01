package executor_test

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// credit and runCredit are the trailers a run named turn-7 credits Crewlet
// with on its commits.
const (
	credit    = "Co-authored-by: " + executor.CoAuthor
	runCredit = executor.RunTrailer + ": turn-7"
)

// ONLY A RUN WHOSE REQUEST ASKS FOR ATTRIBUTION HAS ITS GIT POINTED AT THE
// HOOK, and it is pointed there by the wrapper, after the statement that
// clears the profile's variables and before the harness starts: the hook is
// linked under every hook name in the run's own directory, and the run's
// GIT_CONFIG entry names that directory. A run that asks for none links no
// hook and exports no entry, so its git is exactly the box's.
//
// Mutation: export the entry whatever the request, or after the harness, or link
// no hook, and this goes red.
func TestOnlyARunAskingForAttributionHasItsGitPointedAtTheHook(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		attribution *sandbox.CommitAttribution
		credits     bool
	}{
		"asking for it":   {attribution: &sandbox.CommitAttribution{Run: "turn-7"}, credits: true},
		"asking for none": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := executor.NewClaudeCode()
			b := box(t, runner)
			p := paths(b)
			script := startAndScript(t, runner, b, sandbox.RunRequest{Brief: "fix it", Attribution: tc.attribution})

			linked := false
			for _, cmd := range b.Commands() {
				if strings.Contains(cmd, "ln -sf ../hook") {
					linked = true
					for _, hook := range []string{"prepare-commit-msg", "pre-commit", "commit-msg", "pre-push"} {
						if !strings.Contains(cmd, " "+hook+" ") {
							t.Errorf("the hook is not linked as %s, so the repository's own %s goes unrun:\n%s",
								hook, hook, cmd)
						}
					}
				}
			}
			if linked != tc.credits {
				t.Fatalf("the hook was linked: %v, want %v (%q)", linked, tc.credits, b.Commands())
			}
			exported := strings.Index(script, "GIT_CONFIG_VALUE_")
			if (exported >= 0) != tc.credits {
				t.Fatalf("the run's git is pointed at a hooks directory: %v, want %v:\n%s",
					exported >= 0, tc.credits, script)
			}
			if !tc.credits {
				return
			}
			if !strings.Contains(script[exported:], p.GitHooks()) {
				t.Errorf("the entry does not name the run's hooks directory %s:\n%s", p.GitHooks(), script)
			}
			launch := strings.Index(script, "claude -p")
			if launch < 0 || exported > launch {
				t.Errorf("the entry is not exported before the harness starts:\n%s", script)
			}
			if unset := strings.Index(script, "unset "); unset < 0 || unset > exported {
				t.Errorf("the entry is not exported after the profile's variables are cleared:\n%s", script)
			}
		})
	}
}

// A RUN NAMED BY SOMETHING THAT IS NOT ONE LINE IS REFUSED BEFORE IT STARTS:
// its name becomes a trailer's value, and a line break there would end the
// trailer and write the rest into the commit's message.
//
// Mutation: drop the check in attributionTrailers and the run starts.
func TestARunWhoseNameIsNotOneLineIsRefusedBeforeItStarts(t *testing.T) {
	t.Parallel()
	for name, run := range map[string]string{
		"a line break":      "turn-7\nSigned-off-by: someone <someone@example.com>",
		"a carriage return": "turn-7\rx",
		"padded":            " turn-7",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := executor.NewClaudeCode()
			b := box(t, runner)
			_, err := runner.Start(t.Context(), b, sandbox.RunRequest{Brief: "fix it",
				Attribution: &sandbox.CommitAttribution{Run: run}})
			if err == nil || !strings.Contains(err.Error(), executor.RunTrailer) {
				t.Fatalf("Start answered %v, want the run's name refused", err)
			}
			if started := b.Background(); len(started) != 0 {
				t.Fatalf("the run started anyway: %q", started)
			}
		})
	}
}

// A RUN'S COMMITS CREDIT CREWLET, AND ONLY THE RUN'S OWN — through a whole
// run in a real box, with a stand-in for the harness that runs git the way an
// agent would: the hook is installed by Start, the wrapper points git at it
// after the login profile, and git runs it.
//
//   - A commit of its own carries the co-author and the run, once, however
//     often it is amended, beside the harness's own co-author line, and with
//     --no-verify too, which skips commit-msg but not prepare-commit-msg.
//   - The repository's hooks still run and still refuse, from its hooks
//     directory or from its own core.hooksPath.
//   - A person's commit the run cherry-picks or amends is not credited: its
//     author is the person, and Crewlet did not write it.
//   - Entries of the run's own environment hold beside the run's, and a
//     profile that resets the count does not drop it.
//   - A run named by nothing credits the co-author alone, and a run asking for
//     no attribution credits nothing — the case that proves this can fail.
//
// Mutation: compare nothing before crediting and the person's commits are
// credited; hand no hook on and the repository's refusal is lost; export the
// entry around the shell rather than in it and the profile drops it.
func TestARunsCommitsCreditCrewletAndOnlyTheRunsOwn(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		// NOT A SKIP: every machine that runs this suite holds its checkout
		// in git, and a skip would pass the build with the hook never run.
		t.Fatalf("git is not on PATH, so the commit attribution hook would go unrun: %v", err)
	}
	turn7 := &sandbox.CommitAttribution{Run: "turn-7"}
	for name, tc := range map[string]struct {
		attribution *sandbox.CommitAttribution
		env         map[string]string
		profile     string
		script      string
		credits     int
		names       int
		want        []string
	}{
		"a commit of its own": {
			attribution: turn7, credits: 1, names: 1,
			script: `echo a > a; git add a; git commit -qm "add a"`,
		},
		"amended again and again": {
			attribution: turn7, credits: 1, names: 1,
			script: `echo a > a; git add a; git commit -qm "add a"
git commit -q --amend --no-edit; git commit -q --amend --no-edit`,
		},
		"beside the harness's own co-author": {
			attribution: turn7, credits: 1, names: 1,
			script: `echo a > a; git add a
git commit -qm "add a" -m "Co-Authored-By: Claude <noreply@anthropic.com>"`,
			want: []string{"Co-Authored-By: Claude <noreply@anthropic.com>\n" + credit},
		},
		"committed with --no-verify": {
			attribution: turn7, credits: 1, names: 1,
			script: `echo a > a; git add a; git commit -q --no-verify -m "add a"`,
		},
		"under the repository's own hooks": {
			attribution: turn7, credits: 1, names: 1,
			script: `printf '#!/bin/sh\ngrep -q OK "$1" || { echo refused by the repository >&2; exit 1; }\n' > .git/hooks/commit-msg
printf '#!/bin/sh\ngit interpret-trailers --in-place --trailer "Refs: repository-hook" "$1"\n' > .git/hooks/prepare-commit-msg
chmod +x .git/hooks/commit-msg .git/hooks/prepare-commit-msg
echo a > a; git add a
if git commit -qm "not good enough" 2>/dev/null; then echo committed anyway; else echo refused; fi
git commit -qm "OK now"`,
			want: []string{"refused\n", "Refs: repository-hook\n" + credit},
		},
		"under the repository's own core.hooksPath": {
			attribution: turn7, credits: 1, names: 1,
			script: `mkdir .husky
printf '#!/bin/sh\ntouch "$HOME/husky-ran"\n' > .husky/pre-commit; chmod +x .husky/pre-commit
git config core.hooksPath .husky
mkdir sub; echo a > sub/a; git add sub; (cd sub && git commit -qm "add a")
[ -f "$HOME/husky-ran" ] && echo husky ran`,
			want: []string{"husky ran\n"},
		},
		"a person's commit, cherry-picked": {
			attribution: turn7, credits: 0, names: 0,
			script: `echo a > a; git add a; git commit -qm base
git checkout -qb side; echo p > p; git add p; person git commit -qm "the person's work"
git checkout -q -; echo b > b; git add b; git commit -qm "on the base"
git cherry-pick side >/dev/null; echo "author $(git log -1 --format=%an)"`,
			want: []string{"author person\n"},
		},
		"a person's commit, amended": {
			attribution: turn7, credits: 0, names: 0,
			script: `echo p > p; git add p; person git commit -qm "the person's work"
git commit -q --amend --no-edit; echo "author $(git log -1 --format=%an)"`,
			want: []string{"author person\n"},
		},
		"a merge of its own": {
			attribution: turn7, credits: 1, names: 1,
			script: `echo a > a; git add a; git commit -qm base
git checkout -qb side; echo s > s; git add s; git commit -qm side
git checkout -q -; echo b > b; git add b; git commit -qm main
git merge -q --no-ff -m "merge side" side`,
		},
		"beside entries of the run's own environment": {
			attribution: turn7, credits: 1, names: 1,
			env: map[string]string{"GIT_CONFIG_COUNT": "1",
				"GIT_CONFIG_KEY_0": "alias.who", "GIT_CONFIG_VALUE_0": "log -1 --format=who:%an"},
			script: `echo a > a; git add a; git commit -qm "add a"; git who`,
			want:   []string{"who:seat\n"},
		},
		"under a profile that resets the count": {
			attribution: turn7, credits: 1, names: 1,
			profile: "export GIT_CONFIG_COUNT=0\n",
			script:  `echo a > a; git add a; git commit -qm "add a"`,
		},
		"named by nothing": {
			attribution: &sandbox.CommitAttribution{}, credits: 1, names: 0,
			script: `echo a > a; git add a; git commit -qm "add a"`,
		},
		"asking for none": {
			credits: 0, names: 0,
			script: `echo a > a; git add a; git commit -qm "add a"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, b := realBox(t)
			p := executor.PathsFor(b)
			if tc.profile != "" {
				if err := b.WriteFile(t.Context(), b.Home()+"/.profile", []byte(tc.profile)); err != nil {
					t.Fatalf("write the profile: %v", err)
				}
			}
			if err := b.WriteFile(t.Context(), b.Home()+"/scenario.sh", []byte(scenarioPrelude+tc.script+scenarioEnd)); err != nil {
				t.Fatalf("write the scenario: %v", err)
			}
			writeBoxStandIn(t, b, p.BinDir()+"/claude",
				"#!/bin/sh\nsh \"$HOME/scenario.sh\" > \"$HOME/observed\" 2> \"$HOME/observed.err\"\n")

			handle, err := runner.Start(t.Context(), b, sandbox.RunRequest{Brief: "commit",
				Env: tc.env, Attribution: tc.attribution})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			deadline := time.Now().Add(60 * time.Second)
			for {
				state, err := runner.Poll(t.Context(), b, handle)
				if err != nil {
					t.Fatalf("Poll: %v", err)
				}
				if state.Over() {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the run never finished")
				}
				time.Sleep(20 * time.Millisecond)
			}

			observed, _ := b.ReadFile(t.Context(), b.Home()+"/observed")
			errs, _ := b.ReadFile(t.Context(), b.Home()+"/observed.err")
			report := "\n--- observed\n" + string(observed) + "--- stderr\n" + string(errs)
			_, message, ok := strings.Cut(string(observed), scenarioMessage)
			if !ok {
				t.Fatalf("the scenario never reached its last commit's message:%s", report)
			}
			if got := strings.Count(message, credit); got != tc.credits {
				t.Errorf("the commit credits Crewlet %d times, want %d:%s", got, tc.credits, report)
			}
			if got := strings.Count(message, runCredit); got != tc.names {
				t.Errorf("the commit names the run %d times, want %d:%s", got, tc.names, report)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(observed), want) {
					t.Errorf("the scenario did not show %q:%s", want, report)
				}
			}
		})
	}
}

// scenarioPrelude opens every scenario: a fresh repository in the box's home,
// made under the run's environment (git fires a hook as it writes the
// repository's first ref), committing as the seat — the identity a setup
// recipe gives the run — and blind to the machine's own system configuration.
// person runs a command as a person, outside the run's hooks: their commit
// was made somewhere else. Through env, because an assignment before a shell
// function's name is not certain to reach the commands it runs.
const scenarioPrelude = `set -e
export GIT_CONFIG_NOSYSTEM=1
cd "$HOME"; git init -q repo; cd repo
git config user.name seat; git config user.email seat@example.com; git config commit.gpgsign false
person() {
	env GIT_CONFIG_COUNT=0 GIT_AUTHOR_NAME=person GIT_AUTHOR_EMAIL=person@example.com \
		GIT_COMMITTER_NAME=person GIT_COMMITTER_EMAIL=person@example.com "$@"
}
`

// scenarioMessage marks where a scenario prints its last commit's message.
const scenarioMessage = "== last commit message\n"

// scenarioEnd closes every scenario by printing its last commit's message.
const scenarioEnd = "\necho '== last commit message'; git log -1 --format=%B\n"
