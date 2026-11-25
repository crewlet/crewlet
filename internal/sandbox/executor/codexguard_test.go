package executor_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A HOME OR A TRUSTED REPOSITORY THAT SWITCHES CODEX'S NESTED HOOK OFF STARTS
// THE RUN WITHOUT IT, SAYING SO (D709): the hook carries checkout handing
// alone. Codex 0.171.0 runs no hook where the configuration it settles on
// holds the hooks feature false by either name, and not the run's where a
// layer other than a project's sets its state `enabled = false` (measured), so
// a launch whose `config/read` (as 0.171.0 gave it) says so starts Codex with
// no nested hook, its skip log naming the file and key once, the guard's home
// named as the box home's file. Otherwise the hook starts and nothing is said.
// Mutations, each red: refuse the run; declare the hook anyway; say nothing;
// read the layers, not the settled configuration; read one name of the
// feature; let a project's state count; name the guard's own home.
func TestCodexsNestedHookSwitchedOffStartsTheRunWithoutIt(t *testing.T) {
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
	const said = "crewlet: checkouts cloned during the run are not handed their instructions and skills: "
	for name, tc := range map[string]struct {
		answer string
		off    []string
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
			off: []string{"HOME/.codex/config.toml sets features.codex_hooks = false"}},
		"a trusted repository switches hooks off by the other name": {answer: answer(`{"features":{"hooks":false}}`,
			layer(flags, `{}`, ""), layer(project, `{"features":{"hooks":false}}`, ""), layer(user, `{}`, "")),
			off: []string{"WORK/.codex/config.toml sets features.hooks = false"}},
		"the home switches the run's hook off": {answer: answer(`{"features":{}}`, layer(flags, `{}`, ""),
			layer(user, `{"hooks":{"state":{`+key+`:{"enabled":false}}}}`, "")),
			off: []string{`HOME/.codex/config.toml sets hooks.state."/<session-flags>/config.toml:post_tool_use:0:0".enabled = false`}},
		"the system switches the run's hook off": {answer: answer(`{"features":{}}`, layer(flags, `{}`, ""),
			layer(system, `{"hooks":{"state":{`+key+`:{"enabled":false}}}}`, "")),
			off: []string{`/etc/codex/config.toml sets hooks.state.`}},
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
			if _, res := runToEnd(t, runner, box, req); !res.Success {
				t.Fatalf("the run failed: %s", res.Error)
			}
			server := strings.Join(recorded(t, record, "server-argv"), "\n")
			if hooked := strings.Contains(server, "hooks.PostToolUse"); hooked != (len(tc.off) == 0) {
				t.Errorf("Codex started with the nested hook: %v, want %v:\n%s", hooked, len(tc.off) == 0, server)
			}
			stderr := boxFile(t, box, executor.PathsFor(box).Err())
			if n := strings.Count(stderr, said); n != min(len(tc.off), 1) {
				t.Errorf("the skip log said the hook is off %d times, want %d:\n%s", n, min(len(tc.off), 1), stderr)
			}
			for _, want := range tc.off {
				if want = at.Replace(want); !strings.Contains(stderr, said+want) {
					t.Errorf("the skip log does not name %q:\n%s", want, stderr)
				}
			}
			if strings.Contains(stderr, "/tmp/guard-home") {
				t.Errorf("the skip log names the guard's own home, not the box's:\n%s", stderr)
			}
		})
	}
}

// A CODEX RUN WHOSE HOME TURNS A SEAT SKILL OFF IS REFUSED, through the
// launch itself: a real box whose ~/.codex/config.toml holds a
// `[[skills.config]]` entry with `enabled = false` naming the run's skill
// fails before Codex starts, naming the entry, its path resolved as Codex
// 0.171.0 resolves one (measured: through a link to the skills directory,
// from `~/`, relative to the Codex home, past `.` or a `..` whose directory
// is not there); one naming another skill or file runs ([codexSkillsOff]'s
// own cases hold the settling). Mutations, each red: drop the check from the
// launch; compare the path as written; skip `~/` or the relative base; clean
// nothing.
func TestACodexRunWhoseHomeTurnsASeatSkillOffIsRefused(t *testing.T) {
	t.Parallel()
	byPath := func(p string) map[string]string {
		return map[string]string{".codex/config.toml": "[[skills.config]]\npath = \"" + p + "\"\nenabled = false\n"}
	}
	for name, tc := range map[string]switchOffCase{
		"the run's skill": {plant: map[string]string{".codex/config.toml": "[[skills.config]]\nname = \"crewlet:mcp-github\"\nenabled = false\n"},
			fails: []string{"switches off a skill of the run's", `name "crewlet:mcp-github"`}},
		"another skill": {plant: map[string]string{".codex/config.toml": "[[skills.config]]\nname = \"home-skill\"\nenabled = false\n"}},
		"its file through a link": {plant: byPath("HOME/lnk/crewlet:mcp-github/SKILL.md"),
			fails: []string{`path "` + "HOME/lnk/crewlet:mcp-github/SKILL.md" + `"`}},
		"its file from the home": {plant: byPath("~/.codex/skills/crewlet:mcp-github/SKILL.md"),
			fails: []string{`path "~/.codex/skills/crewlet:mcp-github/SKILL.md"`}},
		"its file from the Codex home": {plant: byPath("skills/crewlet:mcp-github/SKILL.md"),
			fails: []string{`path "skills/crewlet:mcp-github/SKILL.md"`}},
		"its file past a missing directory": {plant: byPath("HOME/.codex/skills/./nodir/../crewlet:mcp-github/SKILL.md"),
			fails: []string{"nodir/../crewlet:mcp-github/SKILL.md"}},
		"another file through the link": {plant: byPath("HOME/lnk/crewlet:getting-unstuck/x.md")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box, _ := codexBox(t, "0.171.0")
			for k, v := range tc.plant {
				tc.plant[k] = strings.ReplaceAll(v, "HOME", box.Home())
			}
			for i, want := range tc.fails {
				tc.fails[i] = strings.ReplaceAll(want, "HOME", box.Home())
			}
			if err := os.Symlink(filepath.Join(box.Home(), ".codex", "skills"), filepath.Join(box.Home(), "lnk")); err != nil {
				t.Fatal(err)
			}
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

// A CODEX RUN WHOSE HOME SWITCHES THE BRIDGE OFF IS REFUSED, through the
// launch itself: a real box whose ~/.codex/config.toml holds the bridge's
// entry with `enabled = false` fails before Codex starts, naming the key,
// rather than the run's own entry switching it back on; another server's
// switch runs ([codexServersOff]'s own cases hold the rest). Mutation: drop
// the check from the launch, and this goes red.
func TestACodexRunWhoseHomeSwitchesTheBridgeOffIsRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]switchOffCase{
		"the bridge": {plant: map[string]string{".codex/config.toml": "[mcp_servers.crewlet]\nenabled = false\n"},
			fails: []string{"switches off Crewlet's own MCP server", "mcp_servers.crewlet.enabled = false"}},
		"another server": {plant: map[string]string{".codex/config.toml": "[mcp_servers.other]\ncommand = \"other\"\nenabled = false\n"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box, _ := codexBox(t, "0.171.0")
			plant(t, box, tc.plant)
			req := sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge}
			if len(tc.fails) == 0 {
				_, res := runToEnd(t, runner, box, req)
				checkSwitchOff(t, box, res, tc)
				return
			}
			_, err := runner.Start(t.Context(), box, req)
			if err == nil {
				t.Fatal("a run whose bridge the home switched off started")
			}
			for _, want := range tc.fails {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
		})
	}
}
