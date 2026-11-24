package executor_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A HOME CANNOT SWITCH A MUSE RUN'S NOTES PLUGIN OFF UNSEEN. Muse Code 1.4.25
// runs no hook of a plugin the home's `plugins.activation` sets "off", through
// the run's own install and approval, with no word (measured), so a run with
// the plugin (wired to its notes or its skills' loads) is refused before it
// starts, naming the file and the key, and the home's settings are left as
// they were; the plugin "on", another plugin off and a run without the plugin
// start. Mutations, each red: drop the check; refuse a run without the
// plugin.
func TestAMuseRunsNotesPluginCannotBeSwitchedOffUnseen(t *testing.T) {
	t.Parallel()
	const off = `{"schema_version":1,"plugins":{"activation":{"crewlet-notes":"off"}}}`
	notes := sandbox.RunRequest{Brief: "x", Steer: steerWiring,
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: steerWiring.URL}}}
	plain := sandbox.RunRequest{Brief: "x",
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://e/mcp"}}}
	for name, tc := range map[string]struct {
		home    string
		req     sandbox.RunRequest
		refused bool
	}{
		"the plugin is on":         {home: `{"schema_version":1,"plugins":{"activation":{"crewlet-notes":"on","x":"off"}}}`, req: notes},
		"the home switches it off": {home: off, req: notes, refused: true},
		"a run without the plugin": {home: off, req: plain},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := standInBox(t, executor.NewMuse(), map[string]string{"muse": museLoadsStandIn})
			p := executor.PathsFor(box)
			plantFile(t, p.MuseSettings(), tc.home)
			if !tc.refused {
				runObserved(t, runner, box, tc.req)
				return
			}
			_, err := runner.Start(t.Context(), box, tc.req)
			if err == nil {
				t.Fatal("a run whose notes plugin the home switched off started")
			}
			for _, want := range []string{p.MuseSettings(), `plugins.activation.crewlet-notes to "off"`, "remove that key"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q:\n%v", want, err)
				}
			}
			if got := boxFile(t, box, p.MuseSettings()); got != tc.home {
				t.Errorf("a refused run changed the home's settings to %s", got)
			}
		})
	}
}
