package executor_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// hermesLoadsStandIn is the `hermes` a Hermes run's row runs: it answers the
// release and the plugin's enabling, writes the run's overlay as the carry
// script's `overlay` would, and for `chat` records its argv, environment and
// brief with the overlay it was handed, then answers as a finished run.
const hermesLoadsStandIn = "#!/bin/sh\n" +
	"case \"$1\" in --version) echo 'Hermes Agent v0.23.4 (2026.9.24)'; exit 0;; chat) ;;\n" +
	"--run-module) shift 4; [ \"$2\" = overlay ] || exit 0; mkdir -p \"$4\" && cp \"$3\" \"$4/config.yaml\" && " +
	"echo 'crewlet-carry ok'; exit 0;;\n" +
	"*) exit 0;; esac\n" +
	"cp \"$HERMES_MANAGED_DIR/config.yaml\" \"$HOME/observed.overlay\"\n" + observedRun

// hermesLoads is the Hermes Agent row of TestEveryRunLoadsWhatAPersonsSessionLoads.
// The box home holds a person's Hermes home (config.yaml with its own MCP
// server, .env, SOUL.md, memories, skills, a plugin, hooks, cron) and the
// start directory its instructions: as `hermes chat` starts, every file is as
// the home wrote it, HERMES_HOME is the home's own, the run's settings are an
// overlay naming the bridge, and the run's skill and plugin sit beside the
// home's. Mutations, each red: pin HERMES_HOME elsewhere; write the run's
// settings into the home's config.yaml; clear the home's skills or plugins.
func hermesLoads(t *testing.T) {
	c := homeCase{kept: []string{".hermes/config.yaml", ".hermes/.env", ".hermes/SOUL.md",
		".hermes/memories/MEMORY.md", ".hermes/skills/kept/SKILL.md", ".hermes/plugins/kept/plugin.yaml",
		".hermes/hooks/kept/HOOK.yaml", ".hermes/cron/jobs.json", ".bashrc", ".profile", "workspace/AGENTS.md",
		"workspace/.hermes.md"}}
	runner, box := standInBox(t, executor.NewHermes(), map[string]string{"hermes": hermesLoadsStandIn})
	above := fillHome(t, box, c)
	runObserved(t, runner, box, sandbox.RunRequest{Brief: "go", MCPServers: loadsBridge, Skills: loadsSkills,
		SkillLoads: loadsSkillLoads})
	checkHome(t, box, c, above)

	p := executor.PathsFor(box)
	if got := observedEnv(t, box)["HERMES_HOME"]; got != p.HermesHome() {
		t.Errorf("hermes started with HERMES_HOME=%q, want the home's own %s", got, p.HermesHome())
	}
	if overlay := boxFile(t, box, box.Home()+"/observed.overlay"); !strings.Contains(overlay, codexBridgeURL) {
		t.Errorf("the run's overlay names no bridge:\n%s", overlay)
	}
	for _, own := range []string{p.HermesSkills() + "/crewlet:deploy/SKILL.md", p.HermesPlugin()} {
		if _, err := os.Stat(own); err != nil {
			t.Errorf("the run's own %s is not beside the home's: %v", filepath.Base(own), err)
		}
	}
}
