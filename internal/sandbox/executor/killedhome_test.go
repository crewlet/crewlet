package executor_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A KILLED RUN'S ENTRIES LEAVE THE HOME FILE IT MERGED INTO AT THE NEXT RUN'S
// START, WHICHEVER HARNESS THAT RUN DRIVES. Codex, Kimi Code, Grok Build and
// Muse Code each lay a run's entries, its bridge's token among them, into a
// file of the box home's own and take them out as the run ends; a run killed
// first leaves them. The next run may drive another harness, so its clear
// takes them out: the home's own entries stay, an edit made since included
// (Kimi Code, Grok Build, Muse Code), and no file of the home holds the token.
// Muse's row plants what its killed run leaves, since its stand-in writes no
// settings. Mutations, each red: leave the menders out of the clear (Kimi
// Code's, Grok Build's and Muse Code's rows); leave Codex's restore out (its).
func TestAKilledRunsEntriesLeaveTheHomeAtAnotherHarnesssStart(t *testing.T) {
	t.Parallel()
	const token = "KILLED-RUN-TOKEN"
	bridge := map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP,
		URL: "http://engine.example.com/mcp/" + token, AlwaysLoaded: true}}
	next := func(t *testing.T, box sandbox.Sandbox) {
		t.Helper()
		if err := executor.NewClaudeCode().ClearArtifacts(t.Context(), box); err != nil {
			t.Fatalf("the next run's clear: %v", err)
		}
		homeHoldsNo(t, box, token)
	}
	killed := func(t *testing.T, runner *executor.Runner, box sandbox.Sandbox, steer sandbox.SteerWiring) {
		t.Helper()
		handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "work", MCPServers: bridge,
			Steer: steer})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		awaitOver(t, runner, box, handle, time.Minute, "the stand-in kills its script")
	}

	t.Run("kimi-code", func(t *testing.T) {
		t.Parallel()
		runner, box := standInBox(t, executor.NewKimi(), map[string]string{
			"kimi": "#!/bin/sh\ncase \"$1\" in --version) echo 2.7.3; exit 0;; esac\nexit 1\n",
			"node": "#!/bin/sh\nkill -KILL \"$PPID\"\n",
		})
		p := executor.PathsFor(box)
		plantFile(t, p.KimiMCPConfig(), `{"mcpServers":{"home-mcp":{"command":"home-mcp"}}}`)
		killed(t, runner, box, sandbox.SteerWiring{})
		var left struct {
			Servers map[string]any `json:"mcpServers"`
		}
		if raw := boxFile(t, box, p.KimiMCPConfig()); json.Unmarshal([]byte(raw), &left) != nil ||
			!strings.Contains(raw, token) {
			t.Fatalf("the killed run left the home's mcp.json as %q, not with its servers, so this case "+
				"shows nothing", raw)
		}
		left.Servers["added-later"] = map[string]any{"command": "added-later"}
		edited, err := json.Marshal(left)
		if err != nil {
			t.Fatal(err)
		}
		plantFile(t, p.KimiMCPConfig(), string(edited))
		next(t, box)
		if got := boxFile(t, box, p.KimiMCPConfig()); !strings.Contains(got, "home-mcp") ||
			!strings.Contains(got, "added-later") || strings.Contains(got, `"crewlet"`) {
			t.Errorf("after the next run's clear the home's mcp.json is %s, want the home's and the "+
				"person's servers alone", got)
		}
	})

	t.Run("grok-build", func(t *testing.T) {
		t.Parallel()
		runner, box := standInBox(t, executor.NewGrok(), map[string]string{"grok": "#!/bin/sh\n" +
			"case \"$1\" in --version) echo 'grok 1.0.92 (b06756fd6256)'; exit 0;; inspect) exit 1;; esac\n" +
			"kill -KILL \"$PPID\"; exit 1\n"})
		p := executor.PathsFor(box)
		plantFile(t, p.GrokConfig(), "model = \"grok-4.6\"\n\n[mcp_servers.homesrv]\ncommand = \"home-srv\"\n")
		killed(t, runner, box, loadsSteer)
		if !strings.Contains(boxFile(t, box, p.GrokConfig()), token) {
			t.Fatal("the killed run left the box's settings without its tables, so this case shows nothing")
		}
		plantFile(t, p.GrokConfig(), boxFile(t, box, p.GrokConfig())+"\n[mcp_servers.added]\ncommand = \"added\"\n")
		next(t, box)
		if got := boxFile(t, box, p.GrokConfig()); !strings.Contains(got, "home-srv") ||
			!strings.Contains(got, "[mcp_servers.added]") || strings.Contains(got, p.GrokNoteHelper()) {
			t.Errorf("after the next run's clear the box's settings are %s, want the box's and the "+
				"person's tables alone", got)
		}
	})

	t.Run("muse-code", func(t *testing.T) {
		t.Parallel()
		_, box := standInBox(t, executor.NewMuse(), map[string]string{"muse": museLoadsStandIn})
		p := executor.PathsFor(box)
		plantFile(t, p.MuseSettings(), `{"schema_version":1,"mcpServers":{"home":{"command":"home-server"},`+
			`"added-later":{"command":"added-later"},"crewlet-0ld000":{"type":"http","url":"http://e/mcp/`+token+`"}}}`)
		plantFile(t, p.MuseServersAdded(), "crewlet-0ld000\n")
		next(t, box)
		if got := boxFile(t, box, p.MuseSettings()); !strings.Contains(got, "home-server") ||
			!strings.Contains(got, "added-later") || strings.Contains(got, "crewlet-0ld000") {
			t.Errorf("after the next run's clear the home's settings are %s, want the home's and the "+
				"person's servers alone", got)
		}
		if got := boxFile(t, box, p.MuseServersAdded()); got != "" {
			t.Errorf("the record of the killed run's servers outlived the next run's clear: %q", got)
		}
	})

	t.Run("codex", func(t *testing.T) {
		t.Parallel()
		const own = "[mcp_servers.home]\ncommand = \"home-mcp\"\n"
		runner, box, record := codexBox(t, "0.171.0")
		p := executor.PathsFor(box)
		plant(t, box, map[string]string{".codex/config.toml": own})
		plantFile(t, filepath.Join(record, "block"), "")
		handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "go", MCPServers: bridge})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		waitForFile(t, filepath.Join(record, "working"), "the stand-in to work")
		killJob(t, p.BinDir()+"/codex exec")
		awaitOver(t, runner, box, handle, 15*time.Second, "its job was killed")
		if !strings.Contains(boxFile(t, box, p.CodexLiveConfig()), token) {
			t.Fatal("the killed run left the home's config.toml without its own, so this case shows nothing")
		}
		next(t, box)
		if got := boxFile(t, box, p.CodexLiveConfig()); got != own {
			t.Errorf("after the next run's clear the home's config.toml is\n%s\nwant its own:\n%s", got, own)
		}
		if _, err := os.Stat(p.CodexHomeConfig()); !os.IsNotExist(err) {
			t.Errorf("the held copy outlived the next run's clear (%v)", err)
		}
	})
}
