package executor_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// credit and runCredit are the trailers a run named turn-7 credits Crewlet
// with on its commits.
const (
	credit    = "Co-authored-by: " + executor.CoAuthor
	runCredit = executor.RunTrailer + ": turn-7"
)

// ONLY A RUN WHOSE REQUEST ASKS FOR ATTRIBUTION HAS ITS GIT POINTED AT THE
// HOOK: the hook is linked under every hook name in the run's own directory,
// and the wrapper exports the GIT_CONFIG_PARAMETERS item naming it after clearing the
// profile's variables and before the harness starts. A run that asks for none
// links no hook and exports no entry, so its git is exactly the box's.
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
			exported := strings.Index(script, "export GIT_CONFIG_PARAMETERS=")
			if (exported >= 0) != tc.credits {
				t.Fatalf("the run's git is pointed at a hooks directory: %v, want %v:\n%s",
					exported >= 0, tc.credits, script)
			}
			if !tc.credits {
				return
			}
			if !strings.Contains(script[exported:], "core.hooksPath="+p.GitHooks()) {
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

// A RUN'S COMMITS CREDIT CREWLET, AND ONLY THE RUN'S OWN, through a whole run
// in a real box whose harness stand-in runs git as an agent would. A commit of
// its own carries the co-author and the run once, however often amended,
// beside the harness's own co-author, and under --no-verify; the repository's
// hooks still run and refuse, from .git/hooks or its own core.hooksPath; a
// person's commit cherry-picked or amended is not credited; the run's own
// GIT_CONFIG entries hold, a profile setting git's parameters drops nothing; a run
// named by nothing credits the co-author alone, one asking for none nothing.
//
// Mutation: compare nothing before crediting and the person's commits are
// credited; hand no hook on and the repository's refusal is lost; export the
// entry around the shell rather than in it and the profile drops it.
func TestARunsCommitsCreditCrewletAndOnlyTheRunsOwn(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		// Not a skip: every machine running this suite holds its checkout in
		// git, and a skip would pass the build with the hook never run.
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
		"under a profile that sets git's parameters": {
			attribution: turn7, credits: 1, names: 1,
			profile: "export GIT_CONFIG_PARAMETERS=\"'core.hooksPath=/nowhere' 'alias.who=log -1 --format=who:%an'\"\n",
			script:  `echo a > a; git add a; git commit -qm "add a"; git who`,
			want:    []string{"who:seat\n"},
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
			runner, b := creditBoxes[executor.ClaudeCodeName](t)
			if tc.profile != "" {
				if err := b.WriteFile(t.Context(), b.Home()+"/.profile", []byte(tc.profile)); err != nil {
					t.Fatalf("write the profile: %v", err)
				}
			}
			if err := b.WriteFile(t.Context(), b.Home()+"/scenario.sh", []byte(scenarioPrelude+tc.script+scenarioEnd)); err != nil {
				t.Fatalf("write the scenario: %v", err)
			}
			runScenario(t, runner, b, sandbox.RunRequest{Brief: "commit", Env: tc.env, Attribution: tc.attribution},
				tc.credits, tc.names, tc.want)
		})
	}
}

// EVERY RUNNER'S COMMITS ARE CREDITED HOWEVER ITS SHELL CALLS GIT: each
// registered runner's whole run in a real box, its harness a stand-in running
// git as its model's shell would, under the run's environment and the pinned
// git. A commit is credited once through plain git, git by its absolute path,
// `--no-verify`, `-c` settings of its own, and appended GIT_CONFIG_COUNT
// entries blanking core.hooksPath (Gemini CLI's shell appends exactly these);
// the repository's own hooks are still handed on and still refuse.
//
// Mutations, each red: point git at the hook with a GIT_CONFIG_COUNT entry
// (the blank-entry cases go uncredited); keep the blank entry in the hook's
// lookup (the repository's refusal is lost); leave the run's item in the
// lookup (nothing handed on); drop a runner from creditBoxes (named).
func TestEveryRunnersCommitsAreCreditedHoweverItsShellCallsGit(t *testing.T) {
	t.Parallel()
	git := gittest.Require(t)
	const blanked = `n=${GIT_CONFIG_COUNT:-0}
for kv in credential.helper= core.fsmonitor= core.hooksPath= core.sshCommand= core.editor= sequence.editor= core.pager=cat; do
	export "GIT_CONFIG_KEY_$n=${kv%%=*}" "GIT_CONFIG_VALUE_$n=${kv#*=}"; n=$((n + 1))
done
export GIT_CONFIG_COUNT=$n GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_SYSTEM=/dev/null GIT_TERMINAL_PROMPT=0
`
	cases := map[string]struct {
		script string
		want   []string
	}{
		"plain git": {script: `echo a > a; git add a; git commit -qm "add a"`},
		"git by its absolute path": {
			script: `echo a > a; "$CREWLET_GIT" add a; "$CREWLET_GIT" commit -qm "add a"`,
		},
		"committed with --no-verify": {script: `echo a > a; git add a; git commit -q --no-verify -m "add a"`},
		"with -c settings of its own": {
			script: `echo a > a; git add a; git -c commit.verbose=false -c core.quotePath=false commit -qm "add a"`,
		},
		"under a harness's blank core.hooksPath": {
			script: blanked + `echo a > a; git add a; git commit -qm "add a"`,
		},
		"the repository's hooks under a harness's blank core.hooksPath": {
			script: blanked + `printf '#!/bin/sh\ngrep -q OK "$1" || { echo refused by the repository >&2; exit 1; }\n' > .git/hooks/commit-msg
chmod +x .git/hooks/commit-msg
echo a > a; git add a
if git commit -qm "not good enough" 2>/dev/null; then echo committed anyway; else echo refused; fi
git commit -qm "OK now"`,
			want: []string{"refused\n"},
		},
	}
	for _, name := range executor.Names() {
		boxed, ok := creditBoxes[name]
		if !ok {
			t.Errorf("runner %s has no box in creditBoxes, so nothing shows its commits credited", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for what, tc := range cases {
				t.Run(what, func(t *testing.T) {
					t.Parallel()
					runner, b := boxed(t)
					prelude := "export CREWLET_GIT=" + shellword.Quote(git) + "\nPATH=" + shellword.Quote(filepath.Dir(git)) + ":$PATH\n"
					if err := b.WriteFile(t.Context(), b.Home()+"/scenario.sh",
						[]byte(prelude+scenarioPrelude+tc.script+scenarioEnd)); err != nil {
						t.Fatalf("write the scenario: %v", err)
					}
					runScenario(t, runner, b, sandbox.RunRequest{Brief: "commit",
						Attribution: &sandbox.CommitAttribution{Run: "turn-7"}}, 1, 1, tc.want)
				})
			}
		})
	}
}

// creditBoxes is, for every registered runner by name, a real box whose
// harness stand-in runs $HOME/scenario.sh as the harness's model would: from
// the harness process the run's wrapper started, so under the environment it
// gave, its output in $HOME/observed and $HOME/observed.err.
var creditBoxes = map[string]func(t *testing.T) (*executor.Runner, sandbox.Sandbox){
	// It answers `--version` without running the scenario: every run's script
	// asks the release first.
	executor.ClaudeCodeName: func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
		runner, b := realBox(t)
		writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/claude",
			"#!/bin/sh\ncase \"$1\" in --version) echo '2.1.310 (Claude Code)'; exit 0;; esac\n"+
				"sh \"$HOME/scenario.sh\" > \"$HOME/observed\" 2> \"$HOME/observed.err\"\n")
		return runner, b
	},
	// The app-server's stand-in runs the scenario before it serves: the
	// model's shell commands run under the app-server's environment, which
	// the wrapper gave it ([codexStandIn] answers `codex-cli 0.171.0`).
	executor.CodexName: func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
		runner, b, _ := codexBox(t, "0.171.0")
		path := executor.PathsFor(b).BinDir() + "/codex"
		standIn, err := b.ReadFile(t.Context(), path)
		if err != nil || !strings.Contains(string(standIn), "\napp-server)\n") {
			t.Fatalf("the codex stand-in has no app-server branch (%v)", err)
		}
		writeBoxStandIn(t, b, path, strings.Replace(string(standIn), "\napp-server)\n", "\napp-server)\n  "+scenarioRun, 1))
		return runner, b
	},
	executor.GeminiCLIName: geminiCreditBox,
	// Its helper's stand-in runs the scenario: Qwen Code and its shell run
	// under the helper's environment, which the wrapper gave it.
	executor.QwenCodeName: func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
		return qwenBox(t, "sh \"$HOME/scenario.sh\" > \"$HOME/observed\" 2> \"$HOME/observed.err\"\n"+
			"echo '{\"type\":\"crewlet_qwen_end\",\"stop_reason\":\"end_turn\"}'\n")
	},
	executor.OpenCodeName: func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
		runner, b := openCodeBox(t, t.TempDir())
		standIn := strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`, "d="+shellword.Quote(t.TempDir()), 1)
		standIn = strings.Replace(standIn, "run)\n  authed\n",
			"run)\n  sh \"$HOME/scenario.sh\" > \"$HOME/observed\" 2> \"$HOME/observed.err\"\n  authed\n", 1)
		writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/opencode", standIn)
		return runner, b
	},
	// One `copilot` process is the run; its stand-in answers the release check
	// every run's script asks first, then runs the scenario.
	executor.CopilotName: func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
		return standInBox(t, executor.NewCopilot(), map[string]string{"copilot": "#!/bin/sh\n" +
			"case \"$1\" in --version) echo 'GitHub Copilot CLI 1.0.114.'; exit 0;; esac\n" + scenarioRun})
	},
	// One `grok` process is the run, and its shell inherits its environment
	// whole; its stand-in answers the release check every run's script asks
	// first and fails the guard's `inspect`, which starts the run, then runs
	// the scenario.
	executor.GrokName: func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
		return standInBox(t, executor.NewGrok(), map[string]string{"grok": "#!/bin/sh\n" +
			"case \"$1\" in --version) echo 'grok 1.0.92 (b06756fd6256)'; exit 0;; inspect) exit 1;; esac\n" +
			scenarioRun})
	},
	// One `muse exec` is the run, and its shell inherits its environment whole;
	// the script's other calls (the release, `muse mcp` and `muse plugins`
	// wiring the run) only answer.
	executor.MuseName: func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
		return standInBox(t, executor.NewMuse(), map[string]string{"muse": "#!/bin/sh\n" +
			"case \"$1\" in --version) echo 'Muse Code 1.4.25 (1.4.25-R9361.1)'; exit 0;; exec) ;; *) exit 0;; esac\n" +
			scenarioRun})
	},
	// Kimi Code and its shell run under the driver's environment, which the
	// wrapper gave it (the driver starts `kimi web` with its own), so the
	// driver's `node` stand-in runs the scenario.
	executor.KimiName: func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
		return standInBox(t, executor.NewKimi(), map[string]string{
			"kimi": "#!/bin/sh\ncase \"$1\" in --version) echo 2.7.2; exit 0;; esac\nexit 1\n",
			"node": "#!/bin/sh\n" + scenarioRun,
		})
	},
	// Hermes's model runs its shell in the gateway the client starts, under
	// the client's environment, so the client's stand-in runs the scenario. It
	// writes the run's overlay as the carry script's `overlay` would, with no
	// managed scope of the box's.
	executor.HermesName: func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
		return standInBox(t, executor.NewHermes(), map[string]string{"hermes": "#!/bin/sh\n" +
			"case \"$1\" in --version) echo 'Hermes Agent v0.23.3 (2026.9.24)'; exit 0;; chat) ;;\n" +
			"--run-module) shift 4; [ \"$2\" = overlay ] || exit 0; mkdir -p \"$4\" && cp \"$3\" \"$4/config.yaml\" && echo 'crewlet-carry ok'; exit 0;;\n" +
			"*) exit 0;; esac\n" + scenarioRun})
	},
	// One `pi` process is the run, and its bash tool's commands run under the
	// environment its extension hands them; the stand-in answers the release
	// check every run's script asks first, then runs the scenario.
	executor.PiName: func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
		return standInBox(t, executor.NewPi(), map[string]string{"pi": "#!/bin/sh\n" +
			"case \"$1\" in --version) echo 1.6.0; exit 0;; esac\n" + scenarioRun})
	},
	// One `cursor-agent` process is the run, its shell's commands under its
	// environment; the stand-in runs the scenario and ends on a result.
	executor.CursorAgentName: func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
		return standInBox(t, executor.NewCursorAgent(), cursorStandIns(t, scenarioRun))
	},
}

// scenarioRun is a stand-in's line running $HOME/scenario.sh as its harness's
// model would, its output where [runScenario] reads it.
const scenarioRun = "sh \"$HOME/scenario.sh\" > \"$HOME/observed\" 2> \"$HOME/observed.err\"\n"

// standInBox is a real box with runner's plumbing installed and each stand-in
// on the box's own PATH under its name.
func standInBox(t *testing.T, runner *executor.Runner, standIns map[string]string) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	b, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { b.Close(t.Context()) })
	if err = runner.Install(t.Context(), b); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for name, script := range standIns {
		writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/"+name, script)
	}
	return runner, b
}

// geminiCreditBox is creditBoxes' Gemini CLI box ([geminiBox]): its stand-in
// runs the scenario.
func geminiCreditBox(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	return geminiBox(t, "sh \"$HOME/scenario.sh\" > \"$HOME/observed\" 2> \"$HOME/observed.err\"\n"+
		"echo '{\"type\":\"result\",\"status\":\"success\"}'\n")
}

// geminiBox is a real box with a Gemini CLI runner's plumbing and a stand-in
// `gemini` that prints its release when asked, writes a prompt when asked to
// export one (`false` and `0` ask for none, as Gemini CLI reads them), and
// otherwise runs run.
func geminiBox(t *testing.T, run string) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	return geminiBoxAt(t, geminiCapturedRelease, run)
}

// geminiCapturedRelease is the Gemini CLI release whose output the suite's
// fixtures were captured from (testdata/gemini/0.70.0).
const geminiCapturedRelease = "0.70.0"

// geminiBoxAt is [geminiBox] whose Gemini CLI prints release to `--version`,
// as the run's script reads it before its gate.
func geminiBoxAt(t *testing.T, release, run string) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	b, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { b.Close(t.Context()) })
	runner := executor.NewGeminiCLI()
	if err = runner.Install(t.Context(), b); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// NODE ON THE BOX'S OWN PATH, as an image that runs Gemini CLI has it: a
	// login shell's profile may reset PATH to the system's.
	writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/node", "#!/bin/sh\nexec "+shellword.Quote(geminiNode(t))+` "$@"`+"\n")
	writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/gemini",
		"#!/bin/sh\ncase \"$1\" in --version) echo "+shellword.Quote(release)+"; exit 0;; esac\n"+
			"case ${GEMINI_WRITE_SYSTEM_MD:-false} in false|0) ;; *) echo prompt > \"$GEMINI_WRITE_SYSTEM_MD\"; exit 42;; esac\n"+run)
	return runner, b
}

// runScenario runs req in b to its end and holds the scenario's last commit
// to credits co-author lines and names lines naming the run, and its output
// to holding every want.
func runScenario(t *testing.T, runner *executor.Runner, b sandbox.Sandbox, req sandbox.RunRequest,
	credits, names int, want []string) {
	t.Helper()
	handle, err := runner.Start(t.Context(), b, req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitOver(t, runner, b, handle, time.Minute, "a scenario's few commits take seconds, so the run is stuck")
	observed, _ := b.ReadFile(t.Context(), b.Home()+"/observed")
	errs, _ := b.ReadFile(t.Context(), b.Home()+"/observed.err")
	harness, _ := b.ReadFile(t.Context(), executor.PathsFor(b).Err())
	report := "\n--- observed\n" + string(observed) + "--- stderr\n" + string(errs) + "--- harness\n" + string(harness)
	_, message, ok := strings.Cut(string(observed), scenarioMessage)
	if !ok {
		t.Fatalf("the scenario never reached its last commit's message:%s", report)
	}
	if got := strings.Count(message, credit); got != credits {
		t.Errorf("the commit credits Crewlet %d times, want %d:%s", got, credits, report)
	}
	if got := strings.Count(message, runCredit); got != names {
		t.Errorf("the commit names the run %d times, want %d:%s", got, names, report)
	}
	for _, w := range want {
		if !strings.Contains(string(observed), w) {
			t.Errorf("the scenario did not show %q:%s", w, report)
		}
	}
}

// scenarioPrelude opens every scenario: a fresh repository in the box's home,
// made under the run's environment (git fires a hook writing its first ref),
// committing as the seat, the identity a setup recipe gives the run, and blind
// to the machine's system configuration. person runs a command as a person,
// outside the run's hooks, through env: an assignment before a shell
// function's name is not certain to reach the commands it runs.
const scenarioPrelude = `set -e
export GIT_CONFIG_NOSYSTEM=1
cd "$HOME"; git init -q repo; cd repo
git config user.name seat; git config user.email seat@example.com; git config commit.gpgsign false
person() {
	env GIT_CONFIG_COUNT=0 GIT_CONFIG_PARAMETERS= GIT_AUTHOR_NAME=person GIT_AUTHOR_EMAIL=person@example.com \
		GIT_COMMITTER_NAME=person GIT_COMMITTER_EMAIL=person@example.com "$@"
}
`

// scenarioMessage marks where a scenario prints its last commit's message.
const scenarioMessage = "== last commit message\n"

// scenarioEnd closes every scenario by printing its last commit's message.
const scenarioEnd = "\necho '== last commit message'; git log -1 --format=%B\n"
