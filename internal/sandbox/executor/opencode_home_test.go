package executor_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// openCodeHomePlants is every file a box's home can hold that OpenCode 2.3.2
// was measured to load into a run, by its path under the home: the global
// directory's configuration, plugins, agents, instructions and skills, the
// skill roots read from any home, Claude Code's instructions and skills, and
// what the project walk reads in the workspace's parent.
var openCodeHomePlants = map[string]string{
	"global configuration": ".config/opencode/opencode.json",
	"global plugin":        ".config/opencode/plugins/p/index.js",
	"global agent":         ".config/opencode/agents/a.md",
	"global instructions":  ".config/opencode/AGENTS.md",
	"global skill":         ".config/opencode/skills/s/SKILL.md",
	"agents skill":         ".agents/skills/s/SKILL.md",
	"claude instructions":  ".claude/CLAUDE.md",
	"claude skill":         ".claude/skills/s/SKILL.md",
	"home .opencode":       ".opencode/plugins/p.js",
	"home opencode.json":   "opencode.json",
	"home opencode.jsonc":  "opencode.jsonc",
	"home AGENTS.md":       "AGENTS.md",
	"home CLAUDE.md":       "CLAUDE.md",
	"home CONTEXT.md":      "CONTEXT.md",
}

// A RUN LOADS ONLY THE CONFIGURATION ITS LAUNCH WRITES: each file an earlier
// run could have left in the box's home is gone when the run's server starts,
// or, under the global directory, replaced by the run's own empty one
// (OPENCODE_CONFIG_DIR), as is anything an earlier run left in that one.
//
// Mutation: drop a path from the script's removal, or leave the directory
// unset or uncleared, and its row fails.
func TestARunLoadsNoConfigurationItsHomeHolds(t *testing.T) {
	t.Parallel()
	for name, rel := range openCodeHomePlants {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			planted := map[string]string{rel: "planted", ".crewlet/opencode-config/opencode.json": "planted"}
			run := runScript(t, scriptOpts{planted: planted})
			if run.code != 0 {
				t.Fatalf("exit %d:\n%s", run.code, run.stderr)
			}
			dir := readIn(t, run.dir, "serve-config-dir")
			if want := filepath.Join(run.home, ".crewlet", "opencode-config"); dir != want {
				t.Fatalf("the server read its global configuration from %q, want the run's own %q", dir, want)
			}
			if held := readIn(t, run.dir, "serve-config-dir-held"); held != "" {
				t.Errorf("the run's configuration directory held %q as the server started, want it empty", held)
			}
			if strings.HasPrefix(rel, ".config/opencode/") {
				return
			}
			if seen := readIn(t, run.dir, "serve-home"); strings.Contains(seen, "./"+rel+"\n") {
				t.Errorf("%s was still in the box's home as the server started", rel)
			}
		})
	}
}

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

// A PACK LOADS NOTHING A RUN LEFT: its standalone calls read a configuration
// directory inside the pack's own staging directory, which the pack makes
// anew and removes, set before its first call; never the box's global one or
// the run's.
//
// Mutation: drop the pack's OPENCODE_CONFIG_DIR, or set it after a call.
func TestAPackLoadsNoConfigurationARunLeft(t *testing.T) {
	t.Parallel()
	paths := executor.Paths{Home: "/home/crewlet"}
	staged := paths.SessionPackStaging("p1")
	pack := executor.OpenCode{}.Pack(paths, carriedOpenCode, staged, paths.SessionPackHeld("p1"), false)
	set := strings.Index(pack, "OPENCODE_CONFIG_DIR="+shellword.Quote(staged+"/opencode-config")+"; export OPENCODE_CONFIG_DIR")
	if set < 0 {
		t.Fatalf("the pack does not run on a configuration directory inside its staging one:\n%s", pack)
	}
	if first := strings.Index(pack, "opencode "); first < set {
		t.Errorf("the pack calls OpenCode before its configuration directory is set:\n%s", pack)
	}
}

// THE RUN READS ITS CONFIGURATION ONCE: its server runs with OpenCode's file
// watcher off, so an opencode.json written while it runs changes nothing it
// resolves (measured on 2.3.2: with the watcher on, a header written mid-run
// reached the server's providers; off, the server's providers stayed as
// checked).
//
// Mutation: drop OPENCODE_FILEWATCHER_DISABLE from the script.
func TestARunsServerNeverReloadsItsConfiguration(t *testing.T) {
	t.Parallel()
	run := runScript(t, scriptOpts{})
	if run.code != 0 {
		t.Fatalf("exit %d:\n%s", run.code, run.stderr)
	}
	if off := readIn(t, run.dir, "serve-watcher-off"); off != "1" {
		t.Errorf("the server ran with OPENCODE_FILEWATCHER_DISABLE=%q, want 1", off)
	}
}
