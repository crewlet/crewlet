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
// content) and refuses a run whose plugin a document's policies deny, naming
// the document, the rule and the plugin; a policy on another plugin, an answer
// OpenCode could not give and a run on no such policy go on. Mutations, each
// red: skip the check; check the notes' plugin alone; keep the run's own
// content in the asking environment.
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
				Steer: steerWiring, Skills: handedSkills(), SkillLoads: skillWiring(),
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
