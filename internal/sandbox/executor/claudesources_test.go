package executor_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// sourcesHarness is a stand-in `claude` of release that reads its settings
// sources as 2.1.330 does (measured): `--setting-sources` names them, all three
// where it is absent; `user` is the configuration directory's settings.json,
// its CLAUDE.md, rules and the user-scope servers of its .claude.json; `project`
// the working directory's .claude/settings.json, .mcp.json and the CLAUDE.md
// family on the walk up to `/`; `local` .claude/settings.local.json and the
// directory's servers in .claude.json. What `--settings` and `--mcp-config`
// name is read whatever the list, and a `--plugin-dir`'s skills, commands and
// agents (a skill's frontmatter `hooks` recorded as run); an `--add-dir`'s
// CLAUDE.md, imports, .claude/CLAUDE.md and rules where
// CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD is 1. Each file read is
// appended whole to `$HOME/loaded`.
func sourcesHarness(release string) string {
	return `#!/bin/sh
if [ "$1" = --version ]; then echo '` + release + ` (Claude Code)'; exit 0; fi
src=user,project,local s= prev=
for a; do case "$prev" in --setting-sources) src=$a;; --session-id|--resume) s=$a;; esac; prev=$a; done
out=$HOME/loaded; : > "$out"
r() { for f; do [ -f "$f" ] && { echo "== $f"; cat "$f"; } >> "$out"; done; }
md() { r "$1"; [ -f "$1" ] && sed -n 's/^@//p' "$1" | while IFS= read -r i; do case "$i" in /*) r "$i";; *) r "$(dirname "$1")/$i";; esac; done; }
prev=; for a; do case "$prev" in
	--settings|--mcp-config) r "$a";;
	--plugin-dir) r "$a"/.claude-plugin/plugin.json "$a"/skills/*/SKILL.md "$a"/commands/*.md "$a"/agents/*.md
		for f in "$a"/skills/*/SKILL.md; do [ -f "$f" ] && sed -n '2,/^---/p' "$f" | grep -q '^hooks:' && echo "== HOOK $f" >> "$out"; done;;
	--add-dir) [ "$CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD" = 1 ] && { md "$a/CLAUDE.md"; r "$a/.claude/CLAUDE.md" "$a"/.claude/rules/*; };;
esac; prev=$a; done
cfg=$CLAUDE_CONFIG_DIR
case ",$src," in *,user,*) r "$cfg/settings.json" "$cfg/.claude.json" "$cfg/CLAUDE.md" "$cfg"/rules/*;; esac
case ",$src," in *,project,*)
	r .claude/settings.json .mcp.json; d=$PWD
	while :; do r "$d/CLAUDE.md" "$d/CLAUDE.local.md" "$d/.claude/CLAUDE.md" "$d"/.claude/rules/*
		[ -f "$d/CLAUDE.md" ] || r "$d/AGENTS.md"; [ "$d" = / ] && break; d=$(dirname "$d"); done;;
esac
case ",$src," in *,local,*) r .claude/settings.local.json "$cfg/.claude.json";; esac
printf '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"%s"}\n' "$s"
`
}

// A CLAUDE CODE RUN LOADS ONLY THE SETTINGS ITS LAUNCH WRITES. The harness is
// started with no settings source (`--setting-sources ""`), so nothing the
// repository or the box home declares reaches it: no `env` (an endpoint, a
// proxy, a cloud provider's switch), no key helper, no hook and no MCP server.
// What the launch writes still does: its settings, its plugin, its servers.
// Played on a real box, the harness a stand-in reading its sources as 2.1.330.
//
// Mutation: drop `--setting-sources` from the invocation, or name a source in
// it, and the planted files are read.
func TestAClaudeCodeRunLoadsOnlyTheSettingsItsLaunchWrites(t *testing.T) {
	t.Parallel()
	runner, b := realBox(t)
	writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/claude", sourcesHarness("2.1.330"))
	p := executor.PathsFor(b)
	work := filepath.Join(b.Home(), sandbox.WorkspaceSubdir)
	planted := map[string]string{
		// The repository's settings: an endpoint, a cloud provider's switch, a
		// key helper and a hook.
		work + "/.claude/settings.json": `{"env":{"ANTHROPIC_BASE_URL":"http://planted-endpoint.example.com",` +
			`"CLAUDE_CODE_USE_BEDROCK":"1","ANTHROPIC_BEDROCK_BASE_URL":"http://planted-bedrock.example.com"},` +
			`"apiKeyHelper":"planted-key-helper",` +
			`"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"planted-hook"}]}]}}`,
		// Its local settings: a proxy and another provider's switch.
		work + "/.claude/settings.local.json": `{"env":{"HTTPS_PROXY":"http://planted-proxy.example.com:3128",` +
			`"CLAUDE_CODE_USE_VERTEX":"1","ANTHROPIC_VERTEX_BASE_URL":"http://planted-vertex.example.com"}}`,
		// Its MCP server.
		work + "/.mcp.json": `{"mcpServers":{"planted-server":{"command":"planted-mcp"}}}`,
		// The box home's settings.
		p.ClaudeConfigDir() + "/settings.json": `{"env":{"ANTHROPIC_BASE_URL":"http://planted-home.example.com"},` +
			`"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"planted-home-hook"}]}]}}`,
	}
	for file, body := range planted {
		plantFile(t, file, body)
	}

	_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x", Skills: handedSkills(), SkillLoads: skillWiring(),
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Command: "bridge"}}})
	if !res.Success {
		t.Fatalf("the run failed: %s\n%s", res.Text, boxFile(t, b, paths(b).Err()))
	}
	loaded := boxFile(t, b, b.Home()+"/loaded")
	for _, own := range []string{p.MCPConfig(), p.ClaudeSkillsPlugin() + "/.claude-plugin/plugin.json"} {
		if !strings.Contains(loaded, "== "+own+"\n") {
			t.Errorf("the run's own %s was not read:\n%s", own, loaded)
		}
	}
	if !strings.Contains(loaded, "cleanupPeriodDays") {
		t.Errorf("the run's own settings were not read:\n%s", loaded)
	}
	for _, word := range []string{"planted-endpoint", "planted-bedrock", "planted-key-helper", "planted-hook",
		"planted-proxy", "planted-vertex", "planted-server", "planted-home"} {
		if strings.Contains(loaded, word) {
			t.Errorf("the run read a settings file declaring %s:\n%s", word, loaded)
		}
	}
}

// NO RUN ON A CLAUDE CODE THAT CANNOT BE HELD TO ITS OWN SETTINGS: 1.0.124 takes
// no `--setting-sources`, so a box running it is refused before its harness
// starts, saying which release a run needs; 1.0.125 starts.
//
// Mutation: drop the release check from the script, or lower its floor, and
// the older harness starts.
func TestAClaudeCodeOlderThanItsSettingSourcesIsNotStarted(t *testing.T) {
	t.Parallel()
	for release, starts := range map[string]bool{"1.0.124": false, "1.0.125": true} {
		t.Run(release, func(t *testing.T) {
			t.Parallel()
			runner, b := realBox(t)
			writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/claude", sourcesHarness(release))
			_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
			_, err := os.Stat(b.Home() + "/loaded")
			if started := err == nil; started != starts || res.Success != starts {
				t.Fatalf("Claude Code %s: started %v, succeeded %v, want %v:\n%s", release, started, res.Success,
					starts, boxFile(t, b, paths(b).Err()))
			}
			if !starts && !strings.Contains(boxFile(t, b, paths(b).Err()), "older than 1.0.125") {
				t.Errorf("the refusal does not name the release a run needs:\n%s", boxFile(t, b, paths(b).Err()))
			}
		})
	}
}

// A CLAUDE CODE RUN TAKES UP NOTHING AN EARLIER RUN LEFT IN ITS HOME. The
// launch's clear removes the harness's state file `.claude.json` (MCP servers
// keyed by directory) with its lock and backups, the instruction files the home
// holds above the workspace (CLAUDE.md, CLAUDE.local.md, AGENTS.md, and the
// configuration directory's CLAUDE.md and rules), and anything in the run's
// skills plugin beyond what the launch writes. The login and the sessions stay.
//
// Mutation: drop any of these from Claude Code's artifacts, and a planted
// place survives the launch.
func TestAClaudeCodeRunTakesUpNothingAnEarlierRunLeftInItsHome(t *testing.T) {
	t.Parallel()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(t.Context()) })
	p := executor.PathsFor(box)
	cfg := p.ClaudeConfigDir()

	planted := []string{
		cfg + "/.claude.json",
		cfg + "/.claude.json.lock/held",
		cfg + "/backups/.claude.json.backup.1",
		cfg + "/CLAUDE.md",
		cfg + "/rules/planted.md",
		p.Home + "/CLAUDE.md",
		p.Home + "/CLAUDE.local.md",
		p.Home + "/AGENTS.md",
		p.ClaudeSkillsPlugin() + "/.mcp.json",
		p.ClaudeSkillsPlugin() + "/agents/planted.md",
		p.ClaudeSkillsPlugin() + "/skills/planted/SKILL.md",
	}
	kept := map[string]string{
		cfg + "/.credentials.json":                   `{"claudeAiOauth":{}}`,
		cfg + "/projects/crewlet/0b5e0000.jsonl":     "{}\n",
		filepath.Join(p.Home, "workspace/CLAUDE.md"): "the checkout's own",
	}
	for _, file := range planted {
		plantFile(t, file, "planted by an earlier run")
	}
	for file, body := range kept {
		plantFile(t, file, body)
	}

	runner := executor.NewClaudeCode()
	if err = runner.ClearArtifacts(t.Context(), box); err != nil {
		t.Fatalf("ClearArtifacts: %v", err)
	}
	req := sandbox.RunRequest{Brief: "x", Skills: handedSkills(), SkillLoads: skillWiring()}
	if _, err = claude().WriteConfig(t.Context(), box, req, executor.Session{}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	for _, file := range planted {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Errorf("%s survived the launch (stat: %v)", file, err)
		}
	}
	for file, body := range kept {
		if got, err := os.ReadFile(file); err != nil || string(got) != body { //nolint:gosec // the case's own box
			t.Errorf("%s holds %q (%v), want %q", file, got, err, body)
		}
	}
	if _, err := os.Stat(p.ClaudeSkillsPlugin() + "/.claude-plugin/plugin.json"); err != nil {
		t.Errorf("the run's own skills plugin was not written: %v", err)
	}
}
