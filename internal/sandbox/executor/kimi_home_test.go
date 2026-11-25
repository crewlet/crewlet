package executor_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// kimiLoadsStandIn is the `node` a Kimi run's row runs its driver under: it
// records where it started, its argv and environment, the driver's settings,
// the home's MCP servers, the run's agent profile and every file under the box
// home as Kimi Code would start. The `kimi` beside it answers the release.
const kimiLoadsStandIn = `#!/bin/sh
s="$HOME/seen"; mkdir -p "$s"
pwd > "$s/pwd"; printf '%s\n' "$@" > "$s/argv"; env > "$s/env"
cat "$2" > "$s/settings"
cat "$KIMI_CODE_HOME/mcp.json" > "$s/mcp"
cat "$KIMI_CODE_HOME"/agents/crewlet/*.md > "$s/profile"
(cd "$HOME" && find . -path ./seen -prune -o -type f -print) | sort > "$s/files"
`

// kimiLoads is the Kimi Code row of TestEveryRunLoadsWhatAPersonsSessionLoads.
// The start directory holds a repository's instructions, skills, agent,
// permission rules and MCP servers, and the home a person's own instructions,
// skills, plugin, saved trust, config.toml with its hook beside the login, and
// MCP servers: as the driver starts every one is there, config.toml as the home
// wrote it, the workspace trusted, the home's MCP file holding its own server
// beside the run's, the profile offering every server's tools, and the driver
// told the servers its guard holds to that file. After the run the home's MCP
// file is its own again; a later run finds the earlier profile gone, the rest
// there. Crewlet writes no Kimi hook. Mutations, each red: clear any of the
// home's places again; rewrite config.toml; write the MCP file whole; unset the
// trust; drop the restore.
func kimiLoads(t *testing.T) {
	runner, box := standInBox(t, executor.NewKimi(), map[string]string{
		"kimi": "#!/bin/sh\ncase \"$1\" in --version) echo 2.7.2; exit 0;; esac\nexit 1\n",
		"node": kimiLoadsStandIn,
	})
	p := executor.PathsFor(box)
	home, k := box.Home(), p.KimiHome()
	start := home + "/workspace"

	login := "default_model = \"managed:kimi-code/k\"\n\n[providers.\"managed:kimi-code\"]\ntype = \"kimi\"\n"
	config := login + "\n[[hooks]]\nevent = \"SessionStart\"\ncommand = \"true\"\n\n[tools]\ndisabled = [\"Grep\"]\n"
	homeMCP := `{"mcpServers":{"home-mcp":{"command":"home-mcp"}},"note":"the home's"}`
	planted := map[string]string{
		"the repository's AGENTS.md":        start + "/AGENTS.md",
		"the repository's skill":            start + "/.kimi-code/skills/repo/SKILL.md",
		"the repository's .agents skill":    start + "/.agents/skills/repo-agents/SKILL.md",
		"the repository's agent":            start + "/.kimi-code/agents/repo.md",
		"the repository's permission rules": start + "/.kimi-code/local.toml",
		"the repository's MCP servers":      start + "/.mcp.json",
		"the start directory's MCP servers": start + "/.kimi-code/mcp.json",
		"the home's AGENTS.md":              k + "/AGENTS.md",
		"the home's .agents AGENTS.md":      home + "/.agents/AGENTS.md",
		"the home's skill":                  k + "/skills/home/SKILL.md",
		"the home's .agents skill":          home + "/.agents/skills/home-agents/SKILL.md",
		"the home's plugin record":          k + "/plugins/installed.json",
		"the home's plugin":                 k + "/plugins/home/kimi.plugin.json",
		"the home's saved trust":            k + "/workspace-trust/wd_workspace_0123456789ab",
		"the home's agent":                  k + "/agents/home.md",
		"the login":                         k + "/credentials/kimi-code.json",
	}
	for _, file := range planted {
		plantFile(t, file, "planted\n")
	}
	plantFile(t, k+"/config.toml", config)
	plantFile(t, k+"/mcp.json", homeMCP)

	run := func(req sandbox.RunRequest) (files []string, env map[string]string) {
		t.Helper()
		handle, err := runner.Start(t.Context(), box, req)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		awaitOver(t, runner, box, handle, time.Minute, "the stand-in only records, so the run is stuck")
		if got := strings.TrimSpace(boxFile(t, box, home+"/seen/pwd")); got != start {
			t.Fatalf("the driver started in %q, want the start directory %q:\n%s", got, start, boxFile(t, box, p.Err()))
		}
		env = map[string]string{}
		for _, line := range strings.Split(boxFile(t, box, home+"/seen/env"), "\n") {
			if name, value, ok := strings.Cut(line, "="); ok {
				env[name] = value
			}
		}
		return strings.Split(strings.TrimSpace(boxFile(t, box, home+"/seen/files")), "\n"), env
	}
	present := func(t *testing.T, files []string, place, file string) {
		t.Helper()
		if !slices.Contains(files, "."+strings.TrimPrefix(file, home)) {
			t.Errorf("%s (%s) was not there as Kimi started, so the run did not load it", place, file)
		}
	}

	files, env := run(sandbox.RunRequest{Brief: "work", System: "Work well.",
		MCPServers: map[string]sandbox.MCPServer{
			"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://engine.example.com/mcp/t"}}})
	for place, file := range planted {
		t.Run(place, func(t *testing.T) { present(t, files, place, file) })
	}
	t.Run("the home's config.toml as it stands", func(t *testing.T) {
		if got := boxFile(t, box, k+"/config.toml"); got != config {
			t.Errorf("config.toml is %q, want the home's own, its hook and settings beside the login", got)
		}
	})
	t.Run("Crewlet's own beside the home's", func(t *testing.T) {
		var mcp struct {
			Servers map[string]map[string]any `json:"mcpServers"`
			Note    string                    `json:"note"`
		}
		if raw := boxFile(t, box, home+"/seen/mcp"); json.Unmarshal([]byte(raw), &mcp) != nil ||
			mcp.Servers["home-mcp"]["command"] != "home-mcp" || mcp.Note != "the home's" ||
			mcp.Servers["crewlet"]["url"] != "http://engine.example.com/mcp/t" {
			t.Errorf("the home's MCP file as Kimi started is %s, want its own server and key beside the run's", raw)
		}
		var settings struct {
			Servers   []string `json:"servers"`
			MCPConfig string   `json:"mcpConfig"`
		}
		if raw := boxFile(t, box, home+"/seen/settings"); json.Unmarshal([]byte(raw), &settings) != nil ||
			!slices.Equal(settings.Servers, []string{"crewlet"}) || settings.MCPConfig != p.KimiMCPConfig() {
			t.Errorf("the driver's settings are %s, want the run's servers held to the home's MCP file", raw)
		}
		if got := boxFile(t, box, home+"/seen/profile"); !strings.Contains(got, `  - "mcp__*"`) ||
			!strings.Contains(got, "Work well.") {
			t.Errorf("the run's profile is %q, want every server's tools and the run's instructions", got)
		}
	})
	t.Run("the workspace trusted", func(t *testing.T) {
		if env["KIMI_CODE_TRUST_WORKSPACE"] != "1" {
			t.Errorf("KIMI_CODE_TRUST_WORKSPACE = %q, want 1", env["KIMI_CODE_TRUST_WORKSPACE"])
		}
	})
	t.Run("the home's MCP file its own again", func(t *testing.T) {
		if got := boxFile(t, box, k+"/mcp.json"); got != homeMCP {
			t.Errorf("after the run the home's MCP file is %s, want %s", got, homeMCP)
		}
	})

	earlier := strings.TrimSpace(boxFile(t, box, home+"/seen/profile"))
	files, _ = run(sandbox.RunRequest{Brief: "work"})
	t.Run("a later run in the same box", func(t *testing.T) {
		if got := strings.TrimSpace(boxFile(t, box, home+"/seen/profile")); got == earlier ||
			strings.Contains(got, "Work well.") {
			t.Errorf("the later run's profiles are %q, want its own alone", got)
		}
		for place, file := range planted {
			present(t, files, place, file)
		}
		if got := boxFile(t, box, k+"/config.toml"); got != config {
			t.Errorf("config.toml is %q after a second run, want the home's own", got)
		}
	})
}

// A KIMI RUN CUT OFF LEAVES NO SERVER OF ITS OWN IN THE HOME PAST THE NEXT
// RUN. The home's MCP file holds a server of its own and one by the bridge's
// name; the first run's script is killed as its driver starts, so the run's
// servers stand in the file. A person then adds a server. The next run starts
// from the file as it stands less the cut-off run's servers, its own beside
// them, and once it ends the file holds the home's servers, the bridge's name
// its own again, and the person's, with no run's server or token in it.
// Mutations, each red: start from the held copy as before (the person's
// server is lost); take nothing out by the record (the cut-off run's second
// server stays); delete a recorded name the home held (its own entry is gone).
func TestAKimiRunCutOffLeavesNoServerOfItsOwnInTheHome(t *testing.T) {
	t.Parallel()
	runner, box := standInBox(t, executor.NewKimi(), map[string]string{
		"kimi": "#!/bin/sh\ncase \"$1\" in --version) echo 2.7.2; exit 0;; esac\nexit 1\n",
		"node": "#!/bin/sh\ncat \"$KIMI_CODE_HOME/mcp.json\" > \"$HOME/seen-mcp\"\n" +
			"if [ -e \"$HOME/cut\" ]; then rm -f \"$HOME/cut\"; kill -KILL \"$PPID\"; fi\n",
	})
	p := executor.PathsFor(box)
	homeMCP := `{"mcpServers":{"home-mcp":{"command":"home-mcp"},"crewlet":{"command":"home-crewlet"}},"note":"the home's"}`
	plantFile(t, p.KimiMCPConfig(), homeMCP)
	plantFile(t, box.Home()+"/cut", "")
	run := func(servers map[string]sandbox.MCPServer) {
		t.Helper()
		handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "work", MCPServers: servers})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		awaitOver(t, runner, box, handle, time.Minute, "the stand-in only records, so the run is stuck")
	}
	read := func(t *testing.T, file string) map[string]any {
		t.Helper()
		var mcp struct {
			Servers map[string]any `json:"mcpServers"`
			Note    string         `json:"note"`
		}
		if raw := boxFile(t, box, file); json.Unmarshal([]byte(raw), &mcp) != nil || mcp.Note != "the home's" {
			t.Fatalf("%s is %q, want the home's file", file, raw)
		}
		return mcp.Servers
	}
	bridge := func(token string) sandbox.MCPServer {
		return sandbox.MCPServer{Transport: sandbox.TransportHTTP, URL: "http://engine.example.com/mcp/" + token}
	}

	run(map[string]sandbox.MCPServer{"crewlet": bridge("TOKEN-CUT"), "seat-tools": bridge("TOKEN-CUT-TOOLS")})
	cut := read(t, p.KimiMCPConfig())
	if _, ok := cut["seat-tools"]; !ok {
		t.Fatalf("the home's MCP file after the cut-off run is %v, want the run's servers left in it", cut)
	}
	cut["added-later"] = map[string]any{"command": "added-later"}
	edited, err := json.Marshal(map[string]any{"mcpServers": cut, "note": "the home's"})
	if err != nil {
		t.Fatal(err)
	}
	plantFile(t, p.KimiMCPConfig(), string(edited))

	run(map[string]sandbox.MCPServer{"crewlet": bridge("TOKEN-NEXT")})
	t.Run("the next run starts without the cut-off run's servers", func(t *testing.T) {
		seen := read(t, box.Home()+"/seen-mcp")
		if _, ok := seen["seat-tools"]; ok || seen["added-later"] == nil || seen["home-mcp"] == nil ||
			!strings.Contains(fmt.Sprint(seen["crewlet"]), "TOKEN-NEXT") {
			t.Errorf("the next run's driver started on %v, want the home's and the person's servers and its own", seen)
		}
	})
	t.Run("after it the home's file holds the home's and the person's alone", func(t *testing.T) {
		raw := boxFile(t, box, p.KimiMCPConfig())
		after := read(t, p.KimiMCPConfig())
		if strings.Contains(raw, "TOKEN") || after["seat-tools"] != nil || after["added-later"] == nil ||
			fmt.Sprint(after["home-mcp"]) != "map[command:home-mcp]" ||
			fmt.Sprint(after["crewlet"]) != "map[command:home-crewlet]" {
			t.Errorf("after the next run the home's MCP file is %s, want the home's own servers and the "+
				"person's, no run's", raw)
		}
		for _, f := range []string{p.KimiHomeMCPConfig(), p.KimiMCPOurs()} {
			if got := boxFile(t, box, f); got != "" {
				t.Errorf("%s is left after the run: %q", f, got)
			}
		}
	})
}
