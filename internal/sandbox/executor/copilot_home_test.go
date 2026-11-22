package executor_test

import (
	"os"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A COPILOT RUN LOADS ONLY WHAT ITS LAUNCH WRITES. Copilot 1.0.114 in prompt
// mode loads, from its home, every user extension, hook file, MCP server,
// installed plugin, skill (its own and the home's `.agents/skills`), custom
// agent and instruction file, and runs the package and rg in its cache
// (measured); its help names the language servers and saved model providers
// too. Whatever an earlier run or the image left in each is gone once the
// launch has cleared the box and written its config, one case per place; the
// login, the sessions and the run's own notes extension are where they are.
//
// Mutation: drop any place from Paths.CopilotLoaded, or the whole extensions
// directory for crewlet-notes alone, and its case fails.
func TestACopilotRunLoadsOnlyWhatItsLaunchWrites(t *testing.T) {
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
	h := p.CopilotHome()

	left := map[string]string{
		"a user extension":     h + "/extensions/left/extension.mjs",
		"a hook file":          h + "/hooks/left.json",
		"an MCP server":        h + "/mcp-config.json",
		"an installed plugin":  h + "/installed-plugins/_direct/left/plugin.json",
		"the plugins' lock":    h + "/installed-plugins.lock",
		"a skill":              h + "/skills/left/SKILL.md",
		"a home .agents skill": p.Home + "/.agents/skills/left/SKILL.md",
		"a custom agent":       h + "/agents/left.agent.md",
		"an instruction file":  h + "/instructions/left.instructions.md",
		"a language server":    h + "/lsp-config.json",
		"a model provider":     h + "/providers.json",
		"its cached package":   p.CopilotCache() + "/pkg/linux-x64/1.0.114/index.js",
		"its cached rg":        p.CopilotCache() + "/pkg/linux-x64/1.0.114/ripgrep/bin/linux-x64/rg",
	}
	kept := map[string]string{
		h + "/config.json": `{"copilotTokens":{"https://github.com:u":"gho_placeholder"}}`,
		h + "/session-state/0b5e0000-0000-4000-8000-000000000000/events.jsonl": "{}\n",
	}
	for _, file := range left {
		plantFile(t, file, "left by an earlier run")
	}
	for file, body := range kept {
		plantFile(t, file, body)
	}

	runner := executor.NewCopilot()
	if err = runner.ClearArtifacts(t.Context(), box); err != nil {
		t.Fatalf("ClearArtifacts: %v", err)
	}
	req := sandbox.RunRequest{Brief: "x", Limits: sandbox.Limits{MaxTurns: 3}}
	if _, err = (executor.Copilot{}).WriteConfig(t.Context(), box, req, executor.Session{}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	for place, file := range left {
		t.Run(place, func(t *testing.T) {
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Errorf("%s survived the launch (stat: %v), so the run would load it", file, err)
			}
		})
	}
	for file, body := range kept {
		if got, err := os.ReadFile(file); err != nil || string(got) != body { //nolint:gosec // the case's own box
			t.Errorf("%s holds %q (%v), want %q", file, got, err, body)
		}
	}
	if ext, err := os.ReadFile(p.CopilotExtension() + "/extension.mjs"); err != nil || //nolint:gosec // the case's own box
		!strings.Contains(string(ext), `"cap":3`) {
		t.Errorf("the run's own extension was not written after the clear: %v", err)
	}
}
