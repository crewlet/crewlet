package executor_test

import (
	"encoding/json"
	"os"
	"reflect"
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
// permission denies, the run's own rules ranked as OpenCode ranks them, or
// that switches the run's agent or bridge off, or whose opencode answers no
// documents, naming what stops it; a login profile's output is read past.
// Mutations, each red: skip the check; check the notes' plugin alone; keep the
// run's own content; skip the skills, the bridge, the permission or either
// switch; rank the run's top rules last; read the first match; pass on no
// answer; read from the first byte.
func TestAnOpenCodeRunsPluginsCannotBeStoppedByAPolicyUnseen(t *testing.T) {
	t.Parallel()
	const standIn = `#!/bin/sh
case "$*" in *config.get*) [ -z "${OPENCODE_CONFIG_CONTENT:-}" ] || exit 9; cat "$HOME/config-get.json" 2> /dev/null || exit 1 ;; esac
exit 0
`
	for name, tc := range map[string]struct {
		answer, profile string
		fails           []string
	}{
		"no policy": {answer: `[{"type":"document","path":"/h/opencode.json","info":{}}]`},
		"no answer": {fails: []string{"gave no configuration documents", "(exit 1:"}},
		"a login profile printing to stdout": {answer: `[{"type":"document","path":"/h/opencode.json","info":{}}]`,
			profile: "echo hello from the profile\necho '[not json'\n"},
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
		"a seat skill's permission denied in a document's own rules": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"permissions":[{"action":"skill","resource":"crewlet:*","effect":"deny"}]}}]`,
			fails: []string{`/h/opencode.json's permission denies skill "crewlet:*"`, "the seat skill crewlet:mcp-github"}},
		"a seat skill only asked for": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"permissions":[{"action":"skill","resource":"crewlet:*","effect":"ask"}]}}]`},
		"an agent's deny a later document's allow follows": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"agents":{"build":{"permissions":[{"action":"skill","resource":"*","effect":"deny"}]}}}},` +
			`{"type":"document","path":"/w/opencode.json","info":{"agents":{"build":{"permissions":[{"action":"*","resource":"crewlet:*","effect":"allow"}]}}}}]`},
		"another agent's and another skill's deny": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"agents":{"plan":{"permissions":[{"action":"skill","resource":"*","effect":"deny"}]},` +
			`"build":{"permissions":[{"action":"skill","resource":"home-skill","effect":"deny"}]}}}}]`},
		"the run's agent switched off": {answer: `[{"type":"document","path":"/w/opencode.json","info":{"agents":{"build":{"disabled":true}}}}]`,
			fails: []string{"/w/opencode.json switches the agent build off"}},
		"the bridge switched off": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"mcp":{"servers":{"crewlet":{"type":"remote","disabled":true}}}}}]`,
			fails: []string{"/h/opencode.json switches Crewlet's MCP server crewlet off"}},
		"another agent and server switched off": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"agents":{"plan":{"disabled":true}},"mcp":{"servers":{"other":{"disabled":true}}}}}]`},
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
			if tc.profile != "" {
				plant(t, box, map[string]string{".profile": tc.profile})
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

// A PERSON'S OPENCODE DENY STANDS IN THE RUN; THEIR ASK IS ALLOWED. The run's
// rules are its `*` allow, then every rule of each document's own top-level
// `permission` in config.get's order, an ask as allow, then its `question`
// deny: so with OpenCode's last match deciding, a person's deny still denies
// and what would ask a person a headless run cannot answer is allowed
// (measured on 2.3.2 under `opencode debug agents` and a run: a home's,
// repository's and config dir's deny, a pattern's, a narrower ask after a
// deny). A deny of the bridge's tools, at the top or the run's agent, refuses
// the run instead, since the run's agent rule would override it; a person
// with no rules runs as before. Mutations, each red: copy denies alone; drop
// the copies; copy an ask as ask; skip the bridge check; rank the run's agent
// rule in it.
func TestAPersonsOpenCodeDenyStandsInTheRunAndTheirAskIsAllowed(t *testing.T) {
	t.Parallel()
	const standIn = `#!/bin/sh
case "$*" in *config.get*) cat "$HOME/config-get.json" 2> /dev/null || exit 1 ;; esac
exit 0
`
	allow, question := `{"action":"*","resource":"*","effect":"allow"}`, `{"action":"question","resource":"*","effect":"deny"}`
	for name, tc := range map[string]struct {
		answer string
		rules  string // the run's permissions, as written
		fails  []string
	}{
		"no rules": {answer: `[{"type":"document","path":"/h/opencode.json","info":{}}]`, rules: allow + "," + question},
		"a deny after an allow and a narrower ask": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"permissions":[` +
			`{"action":"shell","resource":"*","effect":"deny"},{"action":"shell","resource":"git *","effect":"ask"}]}},` +
			`{"type":"directory","path":"/h"},{"type":"document","path":"/w/opencode.json","info":{"permissions":[` +
			`{"action":"read","resource":"*.pem","effect":"deny"}]}}]`,
			rules: allow + `,{"action":"shell","resource":"*","effect":"deny"},{"action":"shell","resource":"git *","effect":"allow"},` +
				`{"action":"read","resource":"*.pem","effect":"deny"},` + question},
		"a repository's plural deny, as config.get lists it": {answer: `[{"type":"document","path":"/w/opencode.json","info":{"permissions":[` +
			`{"action":"shell","resource":"*","effect":"ask"},{"action":"shell","resource":"rm *","effect":"deny"}]}}]`,
			rules: allow + `,{"action":"shell","resource":"*","effect":"allow"},{"action":"shell","resource":"rm *","effect":"deny"},` + question},
		"an ask of the bridge's tools": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"permissions":[{"action":"crewlet_*","resource":"*","effect":"ask"}]}}]`,
			rules: allow + `,{"action":"crewlet_*","resource":"*","effect":"allow"},` + question},
		"a home deny of the bridge's tools": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"permissions":[{"action":"crewlet_*","resource":"*","effect":"deny"}]}}]`,
			fails: []string{`/h/opencode.json's permission denies "crewlet_*", which takes Crewlet's MCP server crewlet's tools away`}},
		"an agent's deny of every tool": {answer: `[{"type":"document","path":"/w/opencode.json","info":{"agents":{"build":{"permissions":[{"action":"*","resource":"*","effect":"deny"}]}}}}]`,
			fails: []string{`/w/opencode.json's agent.build.permission denies "*"`}},
		"a deny of the bridge a later rule allows": {answer: `[{"type":"document","path":"/h/opencode.json","info":{"permissions":[` +
			`{"action":"*","resource":"*","effect":"deny"},{"action":"crewlet_*","resource":"*","effect":"allow"}]}}]`,
			rules: allow + `,{"action":"*","resource":"*","effect":"deny"},{"action":"crewlet_*","resource":"*","effect":"allow"},` + question},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, box := openCodeBox(t, t.TempDir())
			p := executor.PathsFor(box)
			writeBoxStandIn(t, box, p.BinDir()+"/opencode", standIn)
			plant(t, box, map[string]string{"config-get.json": tc.answer})
			path, err := (executor.OpenCode{}).WriteConfig(t.Context(), box, sandbox.RunRequest{Brief: "go",
				MCPServers: loadsBridge}, executor.Session{}, p)
			if len(tc.fails) > 0 {
				if err == nil {
					t.Fatal("a run whose bridge a person denied was not refused")
				}
				for _, want := range tc.fails {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the refusal does not say %q:\n%v", want, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("WriteConfig: %v", err)
			}
			raw, err := os.ReadFile(path) //nolint:gosec // the run's configuration, under the test's box
			if err != nil {
				t.Fatal(err)
			}
			var cfg struct {
				Permissions json.RawMessage `json:"permissions"`
			}
			if err = json.Unmarshal(raw, &cfg); err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err = json.Unmarshal(cfg.Permissions, &got); err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal([]byte("["+tc.rules+"]"), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("the run's permissions are\n%s\nwant\n[%s]", cfg.Permissions, tc.rules)
			}
		})
	}
}
