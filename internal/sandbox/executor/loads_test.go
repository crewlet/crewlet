package executor_test

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A RUN LOADS WHAT A PERSON'S SESSION OF ITS HARNESS LOADS: from the start
// directory and the box home, the instructions, settings, hooks, skills and
// MCP servers reach the harness beside Crewlet's own, whose hook and MCP
// server are still there; a home's own entry survives next to Crewlet's. One
// row per runner, each a real box with its recording stand-in, the run gone to
// its end ([loadsRows]).
func TestEveryRunLoadsWhatAPersonsSessionLoads(t *testing.T) {
	t.Parallel()
	for _, name := range slices.Sorted(maps.Keys(loadsRows)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			loadsRows[name](t)
		})
	}
}

// loadsRows is, per runner by name, its row of
// [TestEveryRunLoadsWhatAPersonsSessionLoads].
var loadsRows = map[string]func(t *testing.T){
	executor.ClaudeCodeName: claudeLoads,
}

// plant writes each file of files, by its path under box's home.
func plant(t *testing.T, box sandbox.Sandbox, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(box.Home(), rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// claudeView is a stand-in `claude` that records, in $HOME/observed.view, what
// Claude Code 2.1.330 reads as it starts: its working directory, argv and
// pinned variables, the run's --settings and --mcp-config files, and each
// home and start-directory file of a kind it loads, read where the harness
// reads it (the home's state through CLAUDE_CONFIG_DIR). It then writes that
// state where the harness does, and answers as a finished run.
const claudeView = `#!/bin/sh
case "$1" in --version) echo '2.1.330 (Claude Code)'; exit 0;; esac
cfg=$CLAUDE_CONFIG_DIR settings= mcp= prev=
for a; do case "$prev" in --settings) settings=$a;; --mcp-config) mcp=$a;; esac; prev=$a; done
{
	echo "== cwd $(pwd)"
	echo "== argv"; printf '%s\n' "$@"
	echo "== run settings"; cat "$settings"
	echo "== run mcp"; cat "$mcp"
	for f in "$cfg/.claude.json" "$cfg/settings.json" "$cfg/CLAUDE.md" "$cfg"/skills/*/SKILL.md \
		CLAUDE.md .claude/settings.json .mcp.json .claude/skills/*/SKILL.md; do
		echo "== $f"; cat "$f"
	done
} > "$HOME/observed.view" 2>&1
printf '{"numStartups":1,"was":%s}\n' "$(cat "$cfg/.claude.json" 2>/dev/null || echo null)" > "$cfg/.claude.json"
echo '{"type":"result","subtype":"success","is_error":false,"result":"done"}'
`

// claudeLoads is Claude Code's row. The home's `.claude.json`, which the
// pinned CLAUDE_CONFIG_DIR would hide, holds its user and local MCP servers;
// the home's `.claude/` its settings with a hook (and `disableAllHooks`, which
// the run's settings answer), CLAUDE.md and a skill; the start directory its
// own of each and an `.mcp.json`. A home with no `.claude.json` gets the one
// the harness writes, as a person's does; a `.claude.json` the configuration
// directory holds of its own is the one read, and left as it was.
//
// Mutations, each red: drop [claudeStateLink] from the script (the home's
// servers are gone, its state never written); link over a file the directory
// holds (the "own state" case); pass --strict-mcp-config or
// --setting-sources (argv); drop disableAllHooks from the notes' settings.
func claudeLoads(t *testing.T) {
	const home = `{"mcpServers":{"home-mcp":{"command":"home-mcp-server"}},` +
		`"projects":{"%s":{"mcpServers":{"local-mcp":{"command":"local-mcp-server"}}}}}`
	homeSettings := `{"disableAllHooks":true,"env":{"HOME_SETTING":"HOME-SETTINGS"},` +
		`"hooks":{"Stop":[{"hooks":[{"type":"command","command":"home-hook"}]}]}}`
	files := map[string]string{
		".claude/settings.json":                        homeSettings,
		".claude/CLAUDE.md":                            "HOME-INSTRUCTIONS",
		".claude/skills/home-skill/SKILL.md":           "---\nname: home-skill\n---\nHOME-SKILL",
		"workspace/CLAUDE.md":                          "REPO-INSTRUCTIONS",
		"workspace/.claude/settings.json":              `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"repo-hook"}]}]}}`,
		"workspace/.claude/skills/repo-skill/SKILL.md": "---\nname: repo-skill\n---\nREPO-SKILL",
		"workspace/.mcp.json":                          `{"mcpServers":{"repo-mcp":{"command":"repo-mcp-server"}}}`,
	}
	req := sandbox.RunRequest{Brief: "x", Steer: steerWiring,
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: steerWiring.URL}}}
	for name, tc := range map[string]struct {
		state string // the home's .claude.json, "" for none
		own   string // the configuration directory's own .claude.json, "" for none
		want  []string
	}{
		"the home's state": {state: home,
			want: []string{`"home-mcp"`, `"home-mcp-server"`, `"local-mcp"`}},
		"no state yet": {},
		"own state": {state: home, own: `{"mcpServers":{"own-mcp":{"command":"own-mcp-server"}}}`,
			want: []string{`"own-mcp"`}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := realBox(t)
			p := executor.PathsFor(box)
			writeBoxStandIn(t, box, p.BinDir()+"/claude", claudeView)
			plant(t, box, files)
			if tc.state != "" {
				plant(t, box, map[string]string{".claude.json": strings.Replace(tc.state, "%s",
					filepath.Join(box.Home(), sandbox.WorkspaceSubdir), 1)})
			}
			if tc.own != "" {
				plant(t, box, map[string]string{".claude/.claude.json": tc.own})
			}
			waitRun(t, runner, box, req)
			view := boxFile(t, box, box.Home()+"/observed.view")
			for _, want := range slices.Concat(tc.want, []string{
				"== cwd " + filepath.Join(box.Home(), sandbox.WorkspaceSubdir) + "\n",
				// Instructions, settings, hooks and skills, the home's and the start directory's.
				"HOME-INSTRUCTIONS", "HOME-SETTINGS", `"home-hook"`, "HOME-SKILL",
				"REPO-INSTRUCTIONS", `"repo-hook"`, "REPO-SKILL", `"repo-mcp-server"`,
				// Crewlet's own: its MCP server, and its hooks with every hook kept on.
				`"crewlet"`, steerWiring.URL, `"steer_take"`, `"disableAllHooks": false`,
			}) {
				if !strings.Contains(view, want) {
					t.Errorf("the harness did not find %q as it started:\n%s", want, view)
				}
			}
			if tc.state == "" || tc.own != "" {
				if strings.Contains(view, `"home-mcp"`) {
					t.Errorf("the harness read the home's state over its directory's own:\n%s", view)
				}
			}
			for _, never := range []string{"--setting-sources", "--strict-mcp-config", "--bare"} {
				if strings.Contains(view, "\n"+never+"\n") {
					t.Errorf("the harness was started with %s, which keeps a person's config out:\n%s", never, view)
				}
			}
			// The harness's own write lands where a person's session puts it.
			at := p.ClaudeState()
			if tc.own != "" {
				at = p.ClaudeConfigDir() + "/.claude.json"
				if state := boxFile(t, box, p.ClaudeState()); !strings.Contains(state, `"home-mcp"`) ||
					strings.Contains(state, "numStartups") {
					t.Errorf("the home's own state changed under a directory holding its own: %s", state)
				}
			}
			if state := boxFile(t, box, at); !strings.Contains(state, `"numStartups":1`) ||
				(tc.state != "" && tc.own == "" && !strings.Contains(state, `"home-mcp"`)) {
				t.Errorf("the harness's state at %s is not the one it read and wrote: %s", at, state)
			}
		})
	}
}
