package executor_test

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// THE BOX'S DATABASE HOLDS ONLY THE LOGIN THE RUN WAS HANDED: every login it
// held before (an earlier run's import, or one stored since) is removed
// through the run's server before the import, and a login OpenCode would not
// remove ends the run before its client starts.
//
// Mutation: skip the removal, or import before it, or carry on past a login
// still held.
func TestABoxsDatabaseHoldsOnlyTheLoginItWasHanded(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, login: seededLogin, env: []string{"FAKE_CREDS=cred_1 cred_planted"}})
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		if removed := readIn(t, run.dir, "removed"); removed != "cred_1\ncred_planted\n" {
			t.Errorf("removed %q, want every login the database held", removed)
		}
		if held := readIn(t, run.dir, "run-creds"); strings.TrimSpace(held) != "" {
			t.Errorf("the database still held %q as the run's client started", held)
		}
		last := -1
		for i, c := range run.calls {
			if strings.HasPrefix(c, "api --server") && strings.Contains(c, " credential.") {
				last = i
			}
		}
		if imp := run.callIndex("auth import --server"); last < 0 || imp < last {
			t.Errorf("the login was not imported after the database was emptied:\n%s", strings.Join(run.calls, "\n"))
		}

		run = runScript(t, scriptOpts{shell: sh, login: seededLogin,
			env: []string{"FAKE_CREDS=cred_planted", "FAKE_STUCK_CRED=cred_planted"}})
		if run.code == 0 || run.callIndex("run ") >= 0 || run.callIndex("auth import") >= 0 {
			t.Errorf("a database keeping a login it was told to remove still ran (exit %d):\n%s",
				run.code, strings.Join(run.calls, "\n"))
		}
		if !strings.Contains(run.stderr, "kept a login") {
			t.Errorf("the refusal does not say why: %s", run.stderr)
		}
	})
}

// openCodeHomeLoads is what a box's home holds for an OpenCode run, by its
// path under the home: the global directory's configuration (a server of the
// home's), plugin, instructions and skill, the skill roots and instructions
// read from any home, and what the project walk reads above the checkout.
var openCodeHomeLoads = map[string]string{
	".config/opencode/opencode.json":                 `{"mcp":{"servers":{"home-mcp":{"type":"local","command":["home-mcp"]}}}}`,
	".config/opencode/plugins/home-hook/index.js":    `export default { id: "home.hook" }`,
	".config/opencode/AGENTS.md":                     "HOME-INSTRUCTIONS",
	".config/opencode/skills/home-skill/SKILL.md":    "HOME-SKILL",
	".agents/skills/agents-skill/SKILL.md":           "AGENTS-SKILL",
	".claude/skills/claude-skill/SKILL.md":           "CLAUDE-SKILL",
	".claude/CLAUDE.md":                              "CLAUDE-INSTRUCTIONS",
	"AGENTS.md":                                      "WALK-INSTRUCTIONS",
	".opencode/plugins/walk-hook.js":                 `export default { id: "walk.hook" }`,
	"workspace/opencode.json":                        `{"mcp":{"servers":{"repo-mcp":{"type":"local","command":["repo-mcp"]}}}}`,
	"workspace/.opencode/plugins/repo-hook.js":       `export default { id: "repo.hook" }`,
	"workspace/AGENTS.md":                            "REPO-INSTRUCTIONS",
	"workspace/.opencode/skills/repo-skill/SKILL.md": "REPO-SKILL",
}

// openCodeServeView is the stand-in server's record of what it would load:
// where it starts, the global directory it is pointed at, and each file under
// the home with its content.
const openCodeServeView = `printf '%s' "$PWD" > "$d/serve-pwd"
  printf '%s' "${OPENCODE_CONFIG_DIR-unset}" > "$d/serve-config-dir"
  (cd "$HOME" && find . -path ./.crewlet -prune -o -type f -print | sort | while IFS= read -r f; do printf '%s %s\n' "$f" "$(cat "$f")"; done) > "$d/serve-home"
`

// openCodeLoads is OpenCode's row of [TestEveryRunLoadsWhatAPersonsSessionLoads]:
// the run's server starts in the start directory with every file of
// [openCodeHomeLoads] in place and no global directory of its own, so OpenCode
// reads the home's as a person's session does; Crewlet's configuration, handed
// above them, still declares its bridge on, its notes and skill plugins and
// its instructions, and names no server or plugin of theirs.
//
// Mutations, each red: point OPENCODE_CONFIG_DIR at a directory of the run's
// own; remove a home path before the server; drop `disabled: false`.
func openCodeLoads(t *testing.T) {
	runner, b := openCodeBox(t, t.TempDir())
	p := executor.PathsFor(b)
	dir := t.TempDir()
	standIn := strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`, "d="+shellword.Quote(dir), 1)
	standIn = strings.Replace(standIn, "serve)\n", "serve)\n  "+openCodeServeView, 1)
	writeBoxStandIn(t, b, p.BinDir()+"/opencode", standIn)
	for rel, body := range openCodeHomeLoads {
		if err := b.WriteFile(t.Context(), b.Home()+"/"+rel, []byte(body)); err != nil {
			t.Fatalf("plant %s: %v", rel, err)
		}
	}
	handle, err := runner.Start(t.Context(), b, sandbox.RunRequest{Brief: "begin",
		MCPServers: map[string]sandbox.MCPServer{"crewlet-bridge": {Name: "crewlet-bridge",
			Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:7/mcp/run-token-placeholder", AlwaysLoaded: true}},
		Steer: steerWiring, Skills: handedSkills(), SkillLoads: skillWiring()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitOver(t, runner, b, handle, time.Minute, "a stand-in recording its view takes seconds")
	if !strings.Contains(readIn(t, dir, "calls"), "\nrun ") {
		t.Fatalf("the run never reached its client:\n%s\n%s", readIn(t, dir, "calls"), boxFile(t, b, p.Err()))
	}

	if got, want := readIn(t, dir, "serve-pwd"), b.Home()+"/"+sandbox.WorkspaceSubdir; got != want {
		t.Errorf("the server started in %q, want the start directory %q", got, want)
	}
	if got := readIn(t, dir, "serve-config-dir"); got != "unset" {
		t.Errorf("the server was pointed at OPENCODE_CONFIG_DIR=%q, want the home's own global directory", got)
	}
	seen := readIn(t, dir, "serve-home")
	for rel, body := range openCodeHomeLoads {
		if !strings.Contains(seen, "./"+rel+" "+body+"\n") {
			t.Errorf("%s was not in the box as planted when the server started:\n%s", rel, seen)
		}
	}

	var handed struct {
		MCP struct {
			Servers map[string]struct {
				Disabled *bool `json:"disabled"`
			} `json:"servers"`
		} `json:"mcp"`
		Plugins      []json.RawMessage `json:"plugins"`
		Skills       []string          `json:"skills"`
		Instructions []string          `json:"instructions"`
	}
	raw := readIn(t, dir, "serve-config")
	if err := json.Unmarshal([]byte(raw), &handed); err != nil {
		t.Fatalf("the server was handed no configuration (%v):\n%s", err, raw)
	}
	if len(handed.MCP.Servers) != 1 {
		t.Errorf("the run's configuration declares servers %v, want its bridge alone beside the home's", slices.Sorted(maps.Keys(handed.MCP.Servers)))
	}
	if s, ok := handed.MCP.Servers["crewlet-bridge"]; !ok || s.Disabled == nil || *s.Disabled {
		t.Errorf("the run's bridge is not declared on (disabled: false):\n%s", raw)
	}
	var plugins []string
	for _, entry := range handed.Plugins {
		var named openCodePlugin
		if json.Unmarshal(entry, &named) != nil {
			t.Errorf("the run's configuration names plugin %s, which is no plugin of its own", entry)
			continue
		}
		plugins = append(plugins, named.Package)
	}
	if want := []string{p.OpenCodeSteerPlugin(), p.OpenCodeSkillsPlugin(), p.OpenCodeToolEnvPlugin()}; !slices.Equal(plugins, want) {
		t.Errorf("the run's configuration names plugins %v, want its own %v", plugins, want)
	}
	if !slices.Equal(handed.Skills, []string{p.OpenCodeSkills()}) || !slices.Equal(handed.Instructions, []string{p.System()}) {
		t.Errorf("the run's skills %v and instructions %v are not its own alone", handed.Skills, handed.Instructions)
	}
}
