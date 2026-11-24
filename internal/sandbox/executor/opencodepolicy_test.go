package executor_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A CONFIGURATION'S POLICY MAY NOT STOP AN OPENCODE RUN'S PLUGINS UNSEEN. The
// launch asks the box's OpenCode for its configuration documents (`opencode
// api --standalone config.get`, in the run's environment without its own
// content) and refuses a run whose plugin, seat skill or bridge a document's
// policies deny, or whose seat skill its agent's last matching `skill`
// permission denies, the run's own rules ranked as OpenCode ranks them, naming
// the document, the rule and what it stops; anything else goes on. Mutations,
// each red: skip the check; check the notes' plugin alone; keep the run's own
// content in the asking environment; skip the skills or the bridge; skip the
// permission; rank the run's top rules last; read the first match.
func TestAnOpenCodeRunsPluginsCannotBeStoppedByAPolicyUnseen(t *testing.T) {
	t.Parallel()
	const standIn = `#!/bin/sh
case "$*" in *config.get*) [ -z "${OPENCODE_CONFIG_CONTENT:-}" ] || exit 9; cat "$HOME/config-get.json" 2> /dev/null || exit 1 ;; esac
exit 0
`
	for name, tc := range map[string]struct {
		answer string
		fails  []string
	}{
		"no policy":      {answer: `[{"type":"document","path":"/h/opencode.json","info":{}}]`},
		"no answer":      {},
		"another plugin": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"policies":[{"action":"plugin.use","resource":"/elsewhere","effect":"deny"}]}}]`},
		"every plugin denied": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"policies":[{"action":"plugin.use","resource":"*","effect":"deny"}]}}]`,
			fails: []string{`/h/opencode.json's policies deny plugin.use "*"`, "opencode-steer", "opencode-tool-env", "opencode-skill-loads"}},
		"a seat skill denied": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"policies":[{"action":"integration.use","resource":"skill:crewlet:mcp-*","effect":"deny"}]}}]`,
			fails: []string{`skill.use "crewlet:mcp-*"`, "the seat skill crewlet:mcp-github"}},
		"the bridge denied": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"policies":[{"action":"mcp.use","resource":"crewlet","effect":"deny"}]}}]`,
			fails: []string{`mcp.use "crewlet"`, "Crewlet's MCP server crewlet"}},
		"another skill and server": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"policies":[{"action":"skill.use","resource":"home-skill","effect":"deny"},{"action":"mcp.use","resource":"other","effect":"deny"}]}}]`},
		"a seat skill's permission denied to the run's agent": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"agents":{"build":{"permissions":[{"action":"skill","resource":"crewlet:mcp-*","effect":"deny"}]}}}}]`,
			fails: []string{`/h/opencode.json's agent.build.permission denies skill "crewlet:mcp-*"`, "the seat skill crewlet:mcp-github"}},
		"a seat skill's permission denied before the run's own rules": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"permissions":[{"action":"skill","resource":"crewlet:*","effect":"deny"}]}}]`},
		"an agent's deny a later document's allow follows": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"agents":{"build":{"permissions":[{"action":"skill","resource":"*","effect":"deny"}]}}}},` +
			`{"type":"document","path":"/w/opencode.json","info":{"agents":{"build":{"permissions":[{"action":"*","resource":"crewlet:*","effect":"allow"}]}}}}]`},
		"another agent's and another skill's deny": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"agents":{"plan":{"permissions":[{"action":"skill","resource":"*","effect":"deny"}]},` +
			`"build":{"permissions":[{"action":"skill","resource":"home-skill","effect":"deny"}]}}}}]`},
		"the tool environment's plugin denied": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"policies":[{"action":"integration.use","resource":"plugin:*tool-env","effect":"deny"}]}}]`,
			fails: []string{`plugin.use "*tool-env"`, "opencode-tool-env"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, box := openCodeBox(t, t.TempDir())
			p := executor.PathsFor(box)
			writeBoxStandIn(t, box, p.BinDir()+"/opencode", standIn)
			if tc.answer != "" {
				plant(t, box, map[string]string{"config-get.json": tc.answer})
			}
			_, err := (executor.OpenCode{}).WriteConfig(t.Context(), box, sandbox.RunRequest{Brief: "go",
				Steer: steerWiring, Skills: handedSkills(), SkillLoads: skillWiring(), MCPServers: loadsBridge,
				Env: map[string]string{"OPENCODE_CONFIG_CONTENT": "{}"}}, executor.Session{}, p)
			if len(tc.fails) == 0 {
				if err != nil {
					t.Fatalf("a run no policy stops was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a run a policy stops was not refused")
			}
			for _, want := range tc.fails {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
		})
	}
}
