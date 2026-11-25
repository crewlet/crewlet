package executor_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// claudeCheckoutView is a stand-in `claude` 2.1.330 that records, in
// $HOME/observed, what the harness would load as it starts: the home's
// CLAUDE.md and skills, its argv, the settings it is handed and the hooks of
// every plugin it is started with. Where $HOME/clone-src exists it then clones
// it to ./late and runs the PostToolUse command hook of the checkouts' plugin
// as Claude Code runs it after a tool call: once for a subagent's call, then
// twice for the main agent's (observed/nested-sub, nested-1, nested-2), and
// keeps each copied skill as the harness would read it then.
const claudeCheckoutView = `#!/bin/sh
case "$1" in --version) echo '2.1.330 (Claude Code)'; exit 0;; esac
cfg=$CLAUDE_CONFIG_DIR rec=$HOME/observed settings= prev=
mkdir -p "$rec"
printf '%s\n' "$@" > "$rec/argv"
for a; do case "$prev" in --settings) settings=$a;; --plugin-dir) cat "$a/hooks/hooks.json" >> "$rec/hooks" 2> /dev/null;; esac; prev=$a; done
cat "$settings" > "$rec/settings"
cat "$cfg/CLAUDE.md" > "$rec/CLAUDE.md" 2> /dev/null
ls "$cfg/skills" > "$rec/skills" 2> /dev/null
cat "$cfg/skills/repo:check/SKILL.md" > "$rec/repo-check" 2> /dev/null
if [ -d "$HOME/clone-src" ]; then
	cp -R "$HOME/clone-src" ./late
	hc=$(sed -n 's/^ *"command": "\(.*-adapter claude-nested[^"]*\)",*$/\1/p' "$rec/hooks")
	printf '%s\n' '{"hook_event_name":"PostToolUse","tool_name":"Bash","agent_id":"a1"}' | sh -c "$hc" > "$rec/nested-sub" 2>> "$rec/nested.err"
	for i in 1 2; do printf '%s\n' '{"hook_event_name":"PostToolUse","tool_name":"Bash"}' | sh -c "$hc" > "$rec/nested-$i" 2>> "$rec/nested.err"; done
	cat "$cfg/skills/late:fix/SKILL.md" > "$rec/late-fix" 2> /dev/null
fi
echo 'harness: done' >&2
echo '{"type":"result","subtype":"success","is_error":false,"result":"done"}'
`

// WHAT A CHECKOUT BELOW THE START DIRECTORY BRINGS A CLAUDE CODE RUN: on a real
// box, a checkout at ./repo with every layer a repository brings. Its
// CLAUDE.md files and the rules Claude Code loads at start (not one scoped to
// paths) are imported after the home's CLAUDE.md, under its root, which the
// run puts back; its skills come as `repo:<skill>`; its settings, hooks,
// plugins, agents, MCP servers and AGENTS.md reach neither the command line
// nor the home. The checkouts' hook runs on settings that switch no hook,
// hands a later clone its listing once, reading no CLAUDE.md, and claims
// nothing in a subagent's firing. Mutation: drop the gate, copy the text,
// hand AGENTS.md or the scoped rule, skip the restore, claim in a subagent's
// firing, give the hook instructions to read, or keep the repository's hooks.
func TestWhatACheckoutBelowTheStartDirectoryBringsAClaudeCodeRun(t *testing.T) {
	t.Parallel()
	runner, box := realBox(t)
	p := executor.PathsFor(box)
	writeBoxStandIn(t, box, p.BinDir()+"/claude", claudeCheckoutView)
	plant(t, box, map[string]string{
		".claude/CLAUDE.md":                            "HOME-OWN\n",
		"workspace/repo/.git/HEAD":                     "ref: refs/heads/main\n",
		"workspace/repo/.git/index":                    "DIRC",
		"workspace/repo/.git/refs/heads/main":          strings.Repeat("0", 40) + "\n",
		"workspace/repo/CLAUDE.md":                     "REPO-CLAUDE\n",
		"workspace/repo/.claude/CLAUDE.md":             "REPO-DOT-CLAUDE\n",
		"workspace/repo/CLAUDE.local.md":               "REPO-LOCAL\n",
		"workspace/repo/.claude/CLAUDE.local.md":       "NOT-READ-LOCAL\n",
		"workspace/repo/AGENTS.md":                     "REPO-AGENTS\n",
		"workspace/repo/.claude/rules/style.md":        "---\ndescription: Style.\n---\nREPO-RULE\n",
		"workspace/repo/.claude/rules/ts.md":           "---\npaths: [\"src/**/*.ts\"]\n---\nTS-RULE\n",
		"workspace/repo/.claude/skills/check/SKILL.md": "---\nname: check\ndescription: Checks.\n---\nCheck.\n",
		"workspace/repo/.agents/skills/lint/SKILL.md":  "---\nname: lint\ndescription: Lints.\n---\nLint.\n",
		"workspace/repo/.claude/agents/helper.md":      "---\nname: helper\ndescription: REPO-AGENT\n---\n",
		"workspace/repo/.claude/settings.json":         `{"disableAllHooks":true,"enabledPlugins":{"repo-plugin@m":true},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"repo-hook"}]}]}}`,
		"workspace/repo/.mcp.json":                     `{"mcpServers":{"repo-mcp":{"command":"repo-mcp-server"}}}`,
		"clone-src/.git/HEAD":                          "ref: refs/heads/main\n",
		"clone-src/.git/index":                         "DIRC",
		"clone-src/.git/refs/heads/main":               strings.Repeat("0", 40) + "\n",
		"clone-src/CLAUDE.md":                          "LATE-CLAUDE\n",
		"clone-src/.claude/skills/fix/SKILL.md":        "---\nname: fix\ndescription: Fixes.\n---\nFix.\n",
	})
	waitRun(t, runner, box, sandbox.RunRequest{Brief: "x"})
	work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), sandbox.WorkspaceSubdir))
	if err != nil {
		t.Fatal(err)
	}
	seen := func(name string) string { return boxFile(t, box, box.Home()+"/observed/"+name) }

	// Instructions.
	// Imported by absolute path, never copied: Claude Code 2.1.310 and 2.1.330
	// then resolve a file's own `@AGENTS.md` against the checkout, as a session
	// started there does (measured, rows ref-agents and agents).
	want := "HOME-OWN\n\n\nfor `repo` with root " + work + "/repo\n\n@" + work + "/repo/CLAUDE.md\n@" + work +
		"/repo/.claude/CLAUDE.md\n@" + work + "/repo/CLAUDE.local.md\n@" + work + "/repo/.claude/rules/style.md"
	if got := seen("CLAUDE.md"); got != want {
		t.Errorf("instructions: Claude Code started on a home CLAUDE.md of\n%q\nwant\n%q", got, want)
	}
	if got := boxFile(t, box, p.ClaudeConfigDir()+"/CLAUDE.md"); got != "HOME-OWN\n" {
		t.Errorf("instructions: the home's CLAUDE.md after the run is %q, want its own back", got)
	}
	// Skills.
	if got := seen("skills"); got != "repo:check\n" {
		t.Errorf("skills: the home's skills as the harness started are %q, want repo:check alone", got)
	}
	if got := seen("repo-check"); got != "---\nname: \"repo:check\"\ndescription: Checks.\n---\nCheck.\n" {
		t.Errorf("skills: repo:check reads %q", got)
	}
	// Settings, hooks, plugins, agents and MCP servers.
	argv, settings := seen("argv"), seen("settings")
	for _, where := range []string{argv, settings} {
		for _, mark := range []string{"repo-hook", "repo-plugin", "repo-mcp", "REPO-AGENT", "REPO-AGENTS", "--add-dir", work + "/repo"} {
			if strings.Contains(where, mark) {
				t.Errorf("the checkout's %q reached the harness's command line or settings:\n%s", mark, where)
			}
		}
	}
	if _, err := os.Stat(p.ClaudeConfigDir() + "/agents"); !os.IsNotExist(err) {
		t.Errorf("the checkout's agents reached the home (%v)", err)
	}
	if !strings.Contains(argv, "--plugin-dir\n"+p.ClaudeCheckoutsPlugin()+"\n") || strings.Contains(settings, "disableAllHooks") {
		t.Errorf("hooks: the checkouts' plugin did not start, or the run's settings switch hooks:\n%s\n%s", argv, settings)
	}
	// A checkout cloned during the run.
	listing := "# Skills of " + work + "/late\n\nEach is a set of local instructions to follow, stored in its `SKILL.md` " +
		"file; to use one, read that file.\n- late:fix: Fixes. (file: " + p.ClaudeConfigDir() + "/skills/late:fix/SKILL.md)"
	var nested struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(seen("nested-1")), &nested); err != nil || nested.HookSpecificOutput.HookEventName != "PostToolUse" ||
		nested.HookSpecificOutput.AdditionalContext != listing {
		t.Errorf("clone: the first main firing answered %q (%v), want\n%q", seen("nested-1"), err, listing)
	}
	if got := seen("nested-sub") + seen("nested-2") + seen("nested.err"); got != "" {
		t.Errorf("clone: the subagent's firing, the later one or their errors said %q, want nothing", got)
	}
	if got := seen("late-fix"); !strings.HasPrefix(got, "---\nname: \"late:fix\"\n") {
		t.Errorf("clone: its skill reads %q", got)
	}
	// The run's end removes every copy it made from the home's skills.
	for _, copied := range []string{"repo:check", "late:fix"} {
		if _, err := os.Lstat(p.ClaudeConfigDir() + "/skills/" + copied); !os.IsNotExist(err) {
			t.Errorf("the copy %s is in the home's skills after the run (%v)", copied, err)
		}
	}
	// The nested hook hands skills alone, so its config names no CLAUDE.md to read.
	var nestedConfig struct {
		Instructions [][]string `json:"instructions"`
	}
	if err := json.Unmarshal([]byte(boxFile(t, box, p.ClaudeRepo()+"/nested.json")), &nestedConfig); err != nil ||
		len(nestedConfig.Instructions) > 0 {
		t.Errorf("clone: the nested hook's config names instructions %v (%v), which it would read only to drop",
			nestedConfig.Instructions, err)
	}
}

// A CLAUDE CODE RUN STARTS ITS CHECKOUTS' HOOK BY ITS RELEASE AND ITS BOX: on
// 2.1.310 or later, or a version the script cannot read, with the note helper
// in the box, the run starts the checkouts' plugin; without the helper it does
// not and says the box has none; an older release does not and its error
// stream says why. Every row is handed every run's settings, which switch no
// hook, as their pinned copy from the pin's floor. Under every sh a box may
// have.
//
// Mutation: start the plugin without the helper or on an older release, or
// hand other settings, and a row goes red.
func TestAClaudeCodeRunStartsItsCheckoutsHookByItsRelease(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for _, tc := range []struct {
			version               string
			helper, hooks, pinned bool
			said                  string
		}{
			{"2.1.309 (Claude Code)", true, false, false, "older than 2.1.310"},
			{"2.1.310 (Claude Code)", true, true, true, ""},
			{"2.1.330 (Claude Code)", true, true, true, ""},
			{"Claude Code, a version of its own", true, true, true, ""},
			{"2.1.310 (Claude Code)", false, false, true, "no note helper"},
		} {
			t.Run(fmt.Sprintf("%s, helper %v", tc.version, tc.helper), func(t *testing.T) {
				p := executor.Paths{Home: t.TempDir()}
				if err := os.MkdirAll(p.WorkDir(), 0o700); err != nil { // as a launch leaves it
					t.Fatal(err)
				}
				if tc.helper {
					if err := os.MkdirAll(filepath.Dir(p.ClaudeNoteHelper()), 0o700); err != nil {
						t.Fatal(err)
					}
					writeStandIn(t, p.ClaudeNoteHelper(), "#!/bin/sh\nexit 0\n")
				}
				args, stderr := scriptArgv(t, sh, scriptOf(t, commandOf(claude(), sandbox.RunRequest{Brief: "x"}, p, "")), tc.version)
				argv := strings.Join(args, "\n")
				settings := p.ClaudeSettings()
				if tc.pinned {
					settings = pinnedOf(p, settings)
				}

				if started := strings.Contains(argv, "--plugin-dir\n"+p.ClaudeCheckoutsPlugin()+"\n"); started != tc.hooks ||
					after(args, "--settings") != settings {
					t.Errorf("the checkouts' plugin started %v with settings %q, want %v with %q (argv %q)",
						started, after(args, "--settings"), tc.hooks, settings, args)
				}
				if (tc.said == "") != !strings.Contains(stderr, "crewlet:") || !strings.Contains(stderr, tc.said) {
					t.Errorf("the run's error stream said %q, want %q", stderr, tc.said)
				}
			})
		}
	})
}

// THE HOME'S CLAUDE.md COMES BACK HOWEVER THE RUN ENDS: once the gate has
// written the checkouts' sections after the home's own, an exit before the
// harness starts (here the NO_PROXY merge failing) and a teardown's SIGTERM
// while the harness runs both leave `.claude/CLAUDE.md` as the home had it,
// and no held copy behind. Under every sh a box may have.
//
// Mutation: drop the EXIT trap, or the TERM trap's put-back, and a row goes red.
func TestTheHomesClaudeMDComesBackHoweverTheRunEnds(t *testing.T) {
	t.Parallel()
	const harness = `#!/bin/sh
case "$1" in --version) echo '2.1.330 (Claude Code)'; exit 0;; esac
cat "$HOME/.claude/CLAUDE.md" > "$FAKE_DIR/seen"
kill -TERM "$PPID"
exec sleep 30
`
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for _, tc := range []struct {
			name string
			code int
			// early sets the run up to exit before its harness starts.
			early func(t *testing.T, p executor.Paths)
		}{
			{"an exit before the harness", 1, func(t *testing.T, p executor.Paths) {
				settings := pinnedOf(p, p.ClaudeSettings())
				if err := os.WriteFile(settings, []byte(`{"env":{"NO_PROXY":"`+executor.ClaudeNoProxyHolder+`"}}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(settings+".np", 0o700); err != nil { // the merge cannot write its copy
					t.Fatal(err)
				}
			}},
			{"a teardown's SIGTERM", 143, nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				p := executor.Paths{Home: t.TempDir()}
				for _, dir := range []string{p.WorkDir(), p.ClaudeConfigDir(), filepath.Dir(p.ClaudeNoteHelper())} {
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(p.ClaudeConfigDir()+"/CLAUDE.md", []byte("HOME-OWN\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				writeStandIn(t, p.ClaudeNoteHelper(), "#!/bin/sh\nprintf 'for `repo`\\n' > "+
					shellword.Quote(p.ClaudeRepoInstructions())+"\n")
				if tc.early != nil {
					tc.early(t, p)
				}
				dir := t.TempDir()
				writeStandIn(t, filepath.Join(dir, "claude"), harness)
				argv := sh.argv
				if len(argv) == 0 {
					argv = []string{"sh"}
				}
				script := scriptOf(t, commandOf(claude(), sandbox.RunRequest{Brief: "x"}, p, ""))
				cmd := exec.Command(argv[0], append(append([]string{}, argv[1:]...), "-c", sh.prelude+script)...) //nolint:gosec // the script under test
				cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "FAKE_DIR=" + dir, "HOME=" + p.Home}
				var stderr strings.Builder
				cmd.Stdin, cmd.Stderr = strings.NewReader("x"), &stderr
				err := cmd.Run()
				if code := cmd.ProcessState.ExitCode(); code != tc.code {
					t.Fatalf("the script exited %d (%v), want %d:\n%s", code, err, tc.code, stderr.String())
				}
				if tc.early == nil {
					if seen, _ := os.ReadFile(filepath.Join(dir, "seen")); !strings.Contains(string(seen), "for `repo`") { //nolint:gosec // the case's own dir
						t.Fatalf("the harness started on a home CLAUDE.md of %q, without the checkouts' sections", seen)
					}
				}
				if got, _ := os.ReadFile(p.ClaudeConfigDir() + "/CLAUDE.md"); string(got) != "HOME-OWN\n" { //nolint:gosec // the case's own dir
					t.Errorf("the home's CLAUDE.md after the run is %q, want its own back", got)
				}
				if _, err := os.Stat(p.ClaudeHomeInstructions()); !os.IsNotExist(err) {
					t.Errorf("a held copy of the home's CLAUDE.md is left behind (%v)", err)
				}
			})
		}
	})
}

// A CLAUDE CODE RUN SAYS WHAT ITS HOOK LEFT OUT ONCE IT STOPS, AND ONLY ITS
// OWN: a line a firing kept in the checkout state during the run reaches the
// run's error stream once the harness has stopped, once; a line an earlier
// run killed before printing left there is said by neither this run nor one
// below the checkouts' floor, whose gate never ran. Under every sh a box may
// have.
//
// Mutation: say the log before the gate, or never, and a row goes red.
func TestAClaudeCodeRunSaysWhatItsHookLeftOutOnceItStops(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for _, tc := range []struct {
			version string
			says    bool
		}{{"2.1.310 (Claude Code)", true}, {"2.1.309 (Claude Code)", false}} {
			t.Run(tc.version, func(t *testing.T) {
				p := executor.Paths{Home: t.TempDir()}
				for _, dir := range []string{p.ClaudeRepo(), filepath.Dir(p.ClaudeNoteHelper())} {
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(p.ClaudeRepo()+"/skipped", []byte("STALE-LINE\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				writeStandIn(t, p.ClaudeNoteHelper(), "#!/bin/sh\nexit 0\n")
				dir := t.TempDir()
				writeStandIn(t, filepath.Join(dir, "claude"), "#!/bin/sh\n"+
					`case "$1" in --version) echo '`+tc.version+`'; exit 0;; esac`+"\n"+
					`[ -d `+shellword.Quote(p.ClaudeRepo())+` ] && echo 'the skill late/.claude/skills/x is left out' >> `+
					shellword.Quote(p.ClaudeRepo()+"/skipped")+"\nexit 0\n")
				argv := sh.argv
				if len(argv) == 0 {
					argv = []string{"sh"}
				}
				script := scriptOf(t, commandOf(claude(), sandbox.RunRequest{Brief: "x"}, p, ""))
				cmd := exec.Command(argv[0], append(append([]string{}, argv[1:]...), "-c", sh.prelude+script)...) //nolint:gosec // the script under test
				cmd.Env = []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "HOME=" + p.Home}
				var stderr strings.Builder
				cmd.Stdin, cmd.Stderr = strings.NewReader("x"), &stderr
				if err := cmd.Run(); err != nil {
					t.Fatalf("the script: %v\n%s", err, stderr.String())
				}
				said := stderr.String()
				if strings.Contains(said, "STALE-LINE") {
					t.Errorf("the run said an earlier run's left-out line:\n%s", said)
				}
				if got := strings.Count(said, "the skill late/.claude/skills/x is left out"); got != map[bool]int{true: 1}[tc.says] {
					t.Errorf("the run said its hook's left-out line %d times, want %d:\n%s", got, map[bool]int{true: 1}[tc.says], said)
				}
			})
		}
	})
}

// A BARE RUN WHOSE BOX SWITCHES HOOKS OFF IS STILL HANDED ITS CHECKOUTS AT
// LAUNCH (D685): on a real box whose home sets disableAllHooks, a run with no
// bridge, notes or skills starts, its checkout's CLAUDE.md imported, without
// the checkouts' plugin, and its skip log says once, after the harness ran,
// that a checkout cloned during the run is not handed its skills, naming the
// file and key. A run with another hook of the engine's is refused
// ([TestClaudeCodesHooksCannotBeSwitchedOffUnseen]).
//
// Mutation: refuse it, start the plugin, skip the launch's handing, or say it
// on stderr at the gate rather than in the skip log, and the case goes red.
func TestABareRunWhoseBoxSwitchesHooksOffIsHandedItsCheckoutsAtLaunch(t *testing.T) {
	t.Parallel()
	runner, box := realBox(t)
	p := executor.PathsFor(box)
	writeBoxStandIn(t, box, p.BinDir()+"/claude", claudeCheckoutView)
	plant(t, box, map[string]string{
		".claude/settings.json":               `{"disableAllHooks": true}`,
		"workspace/repo/.git/HEAD":            "ref: refs/heads/main\n",
		"workspace/repo/.git/index":           "DIRC",
		"workspace/repo/.git/refs/heads/main": strings.Repeat("0", 40) + "\n",
		"workspace/repo/CLAUDE.md":            "REPO-CLAUDE\n",
	})
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "x"})
	if !res.Success {
		t.Fatalf("the run did not succeed: %+v\n%s", res, boxFile(t, box, p.Err()))
	}
	work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), sandbox.WorkspaceSubdir))
	if err != nil {
		t.Fatal(err)
	}
	seen := func(name string) string { return boxFile(t, box, box.Home()+"/observed/"+name) }
	if got := seen("CLAUDE.md"); !strings.Contains(got, "@"+work+"/repo/CLAUDE.md") {
		t.Errorf("the launch handed no checkout: Claude Code started on a home CLAUDE.md of\n%s", got)
	}
	if argv := seen("argv"); strings.Contains(argv, p.ClaudeCheckoutsPlugin()) {
		t.Errorf("the checkouts' plugin started where hooks are off:\n%s", argv)
	}
	stderr := boxFile(t, box, p.Err())
	const line = "crewlet: checkouts cloned during the run are not handed their skills: "
	why := "/.claude/settings.json sets disableAllHooks to true, so Claude Code runs no hook of this run\n"
	if n, ran := strings.Count(stderr, line), strings.Index(stderr, "harness: done"); n != 1 || ran < 0 ||
		strings.Index(stderr, line) < ran || !strings.Contains(stderr, why) {
		t.Errorf("the skip log said it %d times, want once after the harness ran:\n%s", n, stderr)
	}
}

// A RUN WHOSE CHECKOUTS' HOOK WOULD BE ITS ONLY HOOK STARTS WITHOUT IT WHERE
// THE BOX SWITCHES HOOKS OFF, AND ONE WITH ANOTHER HOOK IS REFUSED (D685):
// with the launch's findings written ([executor.Paths.ClaudeHooksOff],
// [executor.Paths.ClaudeHooksOffBy]), a run on 2.1.310 with the helper and no
// other hook of the engine's starts without the checkouts' plugin, its skip
// log saying once that a later clone is not handed its skills; with its
// notes' hooks it is refused, naming why, saying no such line; with hooks on
// it starts the plugin. Under every sh a box may have.
//
// Mutation: refuse the checkouts' hook alone, start it anyway, log nothing, or
// let its gate forget the notes' mark, and a row goes red.
func TestARunWhoseOnlyHookIsItsCheckoutsStartsWithoutItWhereHooksAreOff(t *testing.T) {
	t.Parallel()
	const off = "/home/box/.claude/settings.json sets disableAllHooks to true"
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for _, tc := range []struct {
			name, version            string
			hooksOff, helper         bool
			refused, plugin, notSaid bool
			steer                    sandbox.SteerWiring
		}{
			{"its checkouts' hook alone", "2.1.310 (Claude Code)", true, true, false, false, false, sandbox.SteerWiring{}},
			{"its checkouts' hook alone, hooks on", "2.1.310 (Claude Code)", false, true, false, true, true,
				sandbox.SteerWiring{}},
			{"no helper", "2.1.310 (Claude Code)", true, false, false, false, true, sandbox.SteerWiring{}},
			{"below the checkouts' floor", "2.1.309 (Claude Code)", true, true, false, false, true, sandbox.SteerWiring{}},
			{"its notes' hooks beside the checkouts'", "2.1.310 (Claude Code)", true, true, true, false, true, steerWiring},
			{"its notes' hooks, no helper", "2.1.310 (Claude Code)", true, false, true, false, true, steerWiring},
		} {
			t.Run(tc.name, func(t *testing.T) {
				p := executor.Paths{Home: t.TempDir()}
				for _, dir := range []string{p.WorkDir(), filepath.Dir(p.ClaudeNoteHelper())} {
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if tc.hooksOff {
					for _, f := range []string{p.ClaudeHooksOff(), p.ClaudeHooksOffBy()} {
						if err := os.WriteFile(f, []byte(off), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				if tc.helper {
					writeStandIn(t, p.ClaudeNoteHelper(), "#!/bin/sh\nexit 0\n")
				}
				script := scriptOf(t, commandOf(claude(), sandbox.RunRequest{Brief: "x", Steer: tc.steer}, p, ""))
				args, stderr, err := claudeScriptRun(t, sh, script, tc.version)
				if refused := err != nil && args == nil; refused != tc.refused {
					t.Fatalf("the run was refused: %v, want %v (script error %v, argv %q)\n%s", refused, tc.refused,
						err, args, stderr)
				}
				if tc.refused && !strings.Contains(stderr, off+". The run was refused") {
					t.Errorf("the refusal does not say why:\n%s", stderr)
				}
				if plugin := slices.Contains(args, p.ClaudeCheckoutsPlugin()); !tc.refused && plugin != tc.plugin {
					t.Errorf("the checkouts' plugin started: %v, want %v (argv %q)", plugin, tc.plugin, args)
				}
				said := strings.Count(stderr, "crewlet: checkouts cloned during the run are not handed their skills: "+
					off+", so Claude Code runs no hook of this run\n")
				if want := map[bool]int{false: 1, true: 0}[tc.notSaid]; said != want {
					t.Errorf("the skip log said the checkouts' hook is not started %d times, want %d:\n%s", said, want,
						stderr)
				}
			})
		}
	})
}
