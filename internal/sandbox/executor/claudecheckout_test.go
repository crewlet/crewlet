package executor_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// twice for the main agent's (observed/nested-sub, nested-1, nested-2).
const claudeCheckoutView = `#!/bin/sh
case "$1" in --version) echo '2.1.330 (Claude Code)'; exit 0;; esac
cfg=$CLAUDE_CONFIG_DIR rec=$HOME/observed settings= prev=
mkdir -p "$rec"
printf '%s\n' "$@" > "$rec/argv"
for a; do case "$prev" in --settings) settings=$a;; --plugin-dir) cat "$a/hooks/hooks.json" >> "$rec/hooks" 2> /dev/null;; esac; prev=$a; done
cat "$settings" > "$rec/settings"
cat "$cfg/CLAUDE.md" > "$rec/CLAUDE.md" 2> /dev/null
ls "$cfg/skills" > "$rec/skills" 2> /dev/null
if [ -d "$HOME/clone-src" ]; then
	cp -R "$HOME/clone-src" ./late
	hc=$(sed -n 's/^ *"command": "\(.*-adapter claude-nested[^"]*\)",*$/\1/p' "$rec/hooks")
	printf '%s\n' '{"hook_event_name":"PostToolUse","tool_name":"Bash","agent_id":"a1"}' | sh -c "$hc" > "$rec/nested-sub" 2>> "$rec/nested.err"
	for i in 1 2; do printf '%s\n' '{"hook_event_name":"PostToolUse","tool_name":"Bash"}' | sh -c "$hc" > "$rec/nested-$i" 2>> "$rec/nested.err"; done
fi
echo '{"type":"result","subtype":"success","is_error":false,"result":"done"}'
`

// WHAT A CHECKOUT BELOW THE START DIRECTORY BRINGS A CLAUDE CODE RUN: on a real
// box, a checkout at ./repo with every layer a repository brings. Its
// CLAUDE.md, .claude/CLAUDE.md and CLAUDE.local.md are imported after the
// home's own CLAUDE.md, under its root, which the run puts back; its skills come
// as `repo:<skill>`; its settings, hooks, plugins, agents, MCP servers and
// AGENTS.md reach neither the command line nor the home. The checkouts' hook
// runs with hooks on, hands a later clone its skills' listing once, reading no
// CLAUDE.md, and claims nothing in a subagent's firing.
//
// Mutation: drop the gate, copy the text, hand AGENTS.md, skip the restore,
// claim in a subagent's firing, give the hook instructions to read, or leave
// hooks as the repository sets them.
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
		"/repo/.claude/CLAUDE.md\n@" + work + "/repo/CLAUDE.local.md"
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
	if got := boxFile(t, box, p.ClaudeConfigDir()+"/skills/repo:check/SKILL.md"); got != "---\nname: \"repo:check\"\ndescription: Checks.\n---\nCheck.\n" {
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
	if !strings.Contains(argv, "--plugin-dir\n"+p.ClaudeCheckoutsPlugin()+"\n") || !strings.Contains(settings, `"disableAllHooks": false`) {
		t.Errorf("hooks: the checkouts' plugin did not start with hooks on:\n%s\n%s", argv, settings)
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
	if got := boxFile(t, box, p.ClaudeConfigDir()+"/skills/late:fix/SKILL.md"); !strings.HasPrefix(got, "---\nname: \"late:fix\"\n") {
		t.Errorf("clone: its skill reads %q", got)
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
// in the box, the run starts the checkouts' plugin and hands the harness the
// settings with hooks on, as their pinned copy; without the helper it starts
// neither and says the box has none; an older release is handed every run's
// settings, below the pin's floor too, and its error stream says why. Under
// every sh a box may have.
//
// Mutation: start the plugin without the helper or on an older release, or
// leave the settings as every run's, and a row goes red.
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
				if tc.hooks {
					settings = p.ClaudeHooksSettings()
				}
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
				settings := pinnedOf(p, p.ClaudeHooksSettings())
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
