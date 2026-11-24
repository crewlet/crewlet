package executor_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A HOME CANNOT SWITCH A MUSE RUN'S PLUGIN OR SEAT SKILLS OFF UNSEEN, AND THE
// RUN NEVER RESETS A PERSON'S CHOICE. Muse Code 1.4.25 runs no hook of the
// notes plugin where the home's settings set its `plugins.activation` "off",
// installed.json records it disabled, or the settings record a hook of it
// rejected, loads no plugin where installed.json cannot be read, and lists no
// skill its `skills.activation.user` sets "off", each with no word (measured).
// So such a run is refused before the script's remove, install and approve,
// naming the file and the key, the records left as they were; a stale copy
// nobody switched off is still refreshed, and a checkout copy's switch is the
// person's own. Mutations, each red: drop the check; read the records after
// the reinstall (or not at all); refuse a checkout copy's switch.
func TestAMuseRunsPluginAndSkillsCannotBeSwitchedOffUnseen(t *testing.T) {
	t.Parallel()
	const settingsOff = `{"schema_version":1,"plugins":{"activation":{"crewlet-notes":"off"}}}`
	const rejected = `{"schema_version":1,"runtime_capabilities":{"plugin:crewlet-notes:hook:stop":` +
		`{"enabled":false,"trusted_definition_hash":"sha256:00"}}}`
	const stale = `{"version":1,"plugins":{"crewlet-notes":{"enabled":true,"package_sha256":"0ld"}}}`
	const disabled = `{"version":1,"plugins":{"crewlet-notes":{"enabled":false,"package_sha256":"0ld"}}}`
	skill := handedSkills()[0].Listed()
	notes := sandbox.RunRequest{Brief: "x", Steer: steerWiring, Skills: handedSkills(), SkillLoads: skillWiring(),
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: steerWiring.URL}}}
	for name, tc := range map[string]struct {
		settings, installed string
		dir                 bool
		fails               []string
	}{
		"a stale copy nobody switched off is refreshed": {settings: `{"schema_version":1,"plugins":{"activation":` +
			`{"crewlet-notes":"on","x":"off"}},"skills":{"activation":{"user":{"WORK/.agents/skills/` + skill +
			`/SKILL.md":"off"}}}}`, installed: stale},
		"the home's settings switch the plugin off": {settings: settingsOff, installed: stale,
			fails: []string{`settings.json sets plugins.activation.crewlet-notes to "off"`}},
		"the plugin store records it disabled": {settings: `{"schema_version":1}`, installed: disabled,
			fails: []string{`installed.json sets plugins.crewlet-notes.enabled to false`, "muse plugins disable"}},
		"the settings record a hook of it rejected": {settings: rejected, installed: stale,
			fails: []string{`runtime_capabilities."plugin:crewlet-notes:hook:stop".enabled to false`, "muse plugins reject"}},
		"the plugin store does not read": {settings: `{"schema_version":1}`, installed: `{"version":1,`,
			fails: []string{"installed.json does not read as JSON"}},
		"the plugin store is no file": {settings: `{"schema_version":1}`, dir: true,
			fails: []string{"installed.json is there but no regular file"}},
		"the home's settings switch a seat skill off": {settings: `{"schema_version":1,"skills":{"activation":` +
			`{"user":{"HOME/.agents/skills/` + skill + `/SKILL.md":"off"}}}}`, installed: stale,
			fails: []string{`sets skills.activation.user["HOME/.agents/skills/` + skill + `/SKILL.md"] to "off"`}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := standInBox(t, executor.NewMuse(), map[string]string{"muse": museLoadsStandIn})
			p := executor.PathsFor(box)
			at := strings.NewReplacer("HOME", box.Home(), "WORK", filepath.Join(box.Home(), sandbox.WorkspaceSubdir))
			plantFile(t, p.MuseSettings(), at.Replace(tc.settings))
			if tc.dir {
				if err := os.MkdirAll(p.MuseInstalled(), 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				plantFile(t, p.MuseInstalled(), tc.installed)
			}
			if len(tc.fails) == 0 {
				runObserved(t, runner, box, notes)
				calls := boxFile(t, box, box.Home()+"/muse-calls")
				for _, step := range []string{"plugins remove crewlet-notes", "plugins install ", "plugins approve crewlet-notes"} {
					if !strings.Contains(calls, step) {
						t.Errorf("a stale copy nobody switched off was not refreshed: no %q in\n%s", step, calls)
					}
				}
				return
			}
			_, err := runner.Start(t.Context(), box, notes)
			if err == nil {
				t.Fatal("a run the home switched off started")
			}
			for _, want := range tc.fails {
				if want = at.Replace(want); !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
			if calls, _ := os.ReadFile(filepath.Join(box.Home(), "muse-calls")); strings.Contains(string(calls), "plugins") {
				t.Errorf("a refused run touched the plugin's records:\n%s", calls)
			}
			if got := boxFile(t, box, p.MuseSettings()); got != at.Replace(tc.settings) {
				t.Errorf("a refused run changed the home's settings to %s", got)
			}
			if !tc.dir {
				if got := boxFile(t, box, p.MuseInstalled()); got != tc.installed {
					t.Errorf("a refused run changed the plugin store to %s", got)
				}
			}
		})
	}
}
