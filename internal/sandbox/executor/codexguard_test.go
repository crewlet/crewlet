package executor_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A HOME OR A TRUSTED REPOSITORY CANNOT SWITCH CODEX'S NESTED HOOK OFF UNSEEN.
// Codex 0.171.0 runs no hook where the configuration it settles on holds the
// hooks feature false by either name, and not the run's where a layer other
// than a project's sets that hook's state `enabled = false` (measured), so a
// launch whose `config/read` (as 0.171.0 gave it) says so is refused before
// Codex starts, naming the file and the key, the guard's own home named as
// the box home's file. A feature true, set only in an untrusted project, or
// the state of another hook or in a project starts it. Mutations, each red:
// skip the hook check; read the layers but not the settled configuration;
// read one name of the feature; let a project's state count; name the guard's
// own home.
func TestCodexsNestedHookCannotBeSwitchedOffUnseen(t *testing.T) {
	t.Parallel()
	const key = `"/<session-flags>/config.toml:post_tool_use:0:0"`
	answer := func(settled string, layers ...string) string {
		return `{"id":1,"result":{}}` + "\n" + `{"id":2,"result":{"config":` + settled + `,"layers":[` +
			strings.Join(layers, ",") + `]}}`
	}
	layer := func(name, config, disabled string) string {
		l := `{"name":` + name + `,"version":"v","config":` + config
		if disabled != "" {
			l += `,"disabledReason":"` + disabled + `"`
		}
		return l + "}"
	}
	const (
		flags   = `{"type":"sessionFlags"}`
		user    = `{"type":"user","file":"/tmp/guard-home/config.toml","profile":null}`
		project = `{"type":"project","dotCodexFolder":"WORK/.codex"}`
		system  = `{"type":"system","file":"/etc/codex/config.toml"}`
	)
	const refusal = "Codex would run without the run's hook that hands a checkout cloned during it its instructions"
	for name, tc := range map[string]struct {
		answer string
		fails  []string
	}{
		"nothing switches it off": {answer: answer(`{"features":{"memories":false}}`, layer(flags, `{}`, ""),
			layer(user, `{"features":{"codex_hooks":true}}`, ""))},
		"an untrusted repository and another hook's state": {answer: answer(`{"features":{}}`, layer(flags, `{}`, ""),
			layer(project, `{"features":{"codex_hooks":false}}`, "To load project-local config, add it as a trusted project."),
			layer(user, `{"hooks":{"state":{"/home/config.toml:stop:0:0":{"enabled":false}}}}`, ""))},
		"a trusted repository sets its state off": {answer: answer(`{"features":{}}`, layer(flags, `{}`, ""),
			layer(project, `{"hooks":{"state":{`+key+`:{"enabled":false}}}}`, ""))},
		"the home switches hooks off": {answer: answer(`{"features":{"codex_hooks":false}}`, layer(flags, `{}`, ""),
			layer(user, `{"features":{"codex_hooks":false}}`, "")),
			fails: []string{refusal, "HOME/.codex/config.toml sets features.codex_hooks = false", "remove that key"}},
		"a trusted repository switches hooks off by the other name": {answer: answer(`{"features":{"hooks":false}}`,
			layer(flags, `{}`, ""), layer(project, `{"features":{"hooks":false}}`, ""), layer(user, `{}`, "")),
			fails: []string{refusal, "WORK/.codex/config.toml sets features.hooks = false"}},
		"the home switches the run's hook off": {answer: answer(`{"features":{}}`, layer(flags, `{}`, ""),
			layer(user, `{"hooks":{"state":{`+key+`:{"enabled":false}}}}`, "")),
			fails: []string{refusal, `HOME/.codex/config.toml sets hooks.state."/<session-flags>/config.toml:post_tool_use:0:0".enabled = false`}},
		"the system switches the run's hook off": {answer: answer(`{"features":{}}`, layer(flags, `{}`, ""),
			layer(system, `{"hooks":{"state":{`+key+`:{"enabled":false}}}}`, "")),
			fails: []string{refusal, `/etc/codex/config.toml sets hooks.state.`}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box, record := codexBox(t, "0.171.0")
			work, err := filepath.EvalSymlinks(filepath.Join(box.Home(), "workspace"))
			if err != nil {
				t.Fatal(err)
			}
			at := strings.NewReplacer("WORK", work, "HOME", box.Home())
			plantFile(t, filepath.Join(record, "config-read"), at.Replace(tc.answer))
			req := sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge}
			if len(tc.fails) == 0 {
				if _, res := runToEnd(t, runner, box, req); !res.Success {
					t.Fatalf("a run nothing switched the hook off of failed: %s", res.Error)
				}
				return
			}
			_, err = runner.Start(t.Context(), box, req)
			if err == nil {
				t.Fatal("a run whose nested hook the box switched off started")
			}
			for _, want := range tc.fails {
				if want = at.Replace(want); !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
			if strings.Contains(err.Error(), "/tmp/guard-home") {
				t.Errorf("the refusal names the guard's own home, not the box's:\n%v", err)
			}
			if _, err := os.Stat(filepath.Join(record, "server-argv")); !os.IsNotExist(err) {
				t.Errorf("Codex started for a refused run (%v)", err)
			}
		})
	}
}

// A CODEX RUN WHOSE HOME TURNS A SEAT SKILL OFF IS REFUSED, through the
// launch itself: a real box whose ~/.codex/config.toml holds a
// `[[skills.config]]` entry with `enabled = false` naming the run's skill
// fails before Codex starts, naming the entry; one naming another skill runs
// ([codexSkillsOff]'s own cases hold the matching). Mutation: drop the check
// from the launch, and this goes red.
func TestACodexRunWhoseHomeTurnsASeatSkillOffIsRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]switchOffCase{
		"the run's skill": {plant: map[string]string{".codex/config.toml": "[[skills.config]]\nname = \"crewlet:mcp-github\"\nenabled = false\n"},
			fails: []string{"switches off a skill of the run's", `name "crewlet:mcp-github"`}},
		"another skill": {plant: map[string]string{".codex/config.toml": "[[skills.config]]\nname = \"home-skill\"\nenabled = false\n"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box, _ := codexBox(t, "0.171.0")
			plant(t, box, tc.plant)
			req := sandbox.RunRequest{Brief: "go", Skills: handedSkills(), SkillLoads: skillWiring()}
			if len(tc.fails) == 0 {
				_, res := runToEnd(t, runner, box, req)
				checkSwitchOff(t, box, res, tc)
				return
			}
			_, err := runner.Start(t.Context(), box, req)
			if err == nil {
				t.Fatal("a run whose seat skill the home turned off started")
			}
			for _, want := range tc.fails {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
		})
	}
}
