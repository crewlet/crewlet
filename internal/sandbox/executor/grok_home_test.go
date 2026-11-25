package executor_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/crewlet/crewlet/internal/envfile"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// grokLoads, Grok Build's row: A GROK RUN LOADS WHAT A PERSON'S GROK LOADS.
// A real box whose home holds what Grok Build 1.0.92 loads from a home that is
// its working directory: as `grok` starts, every such file is whole, logins and
// sessions too, and only a skill an earlier run wrote (`crewlet:*`) is gone;
// nothing above the home is touched; each compatibility switch reaches it as
// set, and only the variables that move what the runner writes and reads, or
// who decides a call, do not; the box's settings stand beside the run's
// server, on a second run too, and alone once Grok exits; and a brief
// beginning with `/` is words.
// Mutations, each red: remove the home's skills whole; keep an earlier run's
// skill; unset a compatibility switch; drop a name from grokSwitchesOff; write
// the run's settings alone or into the last run's; write the brief as it came.
func grokLoads(t *testing.T) {
	c := homeCase{
		left: []string{".grok/skills/crewlet:earlier/SKILL.md"},
		kept: []string{".grok/AGENTS.md", ".grok/CLAUDE.md", ".grok/rules/kept.md", ".grok/skills/kept/SKILL.md",
			".grok/commands/kept.md", ".grok/agents/kept.md", ".grok/workflows/kept.rhai", ".grok/hooks/hooks.json",
			".grok/plugins/kept/plugin.json", ".grok/memory/MEMORY.md", ".grok/requirements.toml", "AGENTS.md",
			"CLAUDE.md", ".mcp.json", "workspace/.grok/rules/kept.md",
			".grok/auth.json", ".grok/sessions/%2Fhome%2Fbox/s/chat_history.jsonl", ".claude/settings.json",
			".bashrc", "workspace/checkout/AGENTS.md"},
	}
	runner, box := standInBox(t, executor.NewGrok(), map[string]string{"grok": "#!/bin/sh\n" +
		"case \"$1\" in --version) echo 'grok 1.0.92 (b06756fd6256)'; exit 0;; esac\n" +
		"cp \"$HOME/.grok/config.toml\" \"$HOME/observed.config.toml\"\n" + observedRun})
	above := fillHome(t, box, c)
	const settings = "[compat.claude]\nmcps = false\n\n[mcp_servers.docs]\nurl = \"https://docs.example.com/mcp\"\n"
	if err := os.WriteFile(filepath.Join(box.Home(), ".grok", "config.toml"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	off := []string{"GROK_HOME", "GROK_CONFIG", "GROK_CONFIG_PATH", "GROK_AUTO_PERMISSION_MODE", "GROK_LEADER_SOCKET"}
	var on []string
	for _, vendor := range []string{"CLAUDE", "CURSOR"} {
		for _, cell := range []string{"MCPS", "HOOKS", "SKILLS", "RULES", "AGENTS"} {
			on = append(on, "GROK_"+vendor+"_"+cell+"_ENABLED")
		}
	}
	env := map[string]string{}
	for _, name := range slices.Concat(off, on) {
		env[name] = "true"
	}
	env["GROK_HOME"] = box.Home() + "/elsewhere"
	bridge := map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:1/mcp"}}
	for run := range 2 {
		if run > 0 {
			if err := runner.ClearArtifacts(t.Context(), box); err != nil {
				t.Fatal(err)
			}
		}
		runObserved(t, runner, box, sandbox.RunRequest{Brief: " /session-info", Model: "grok-4.6", Env: env,
			MCPServers: bridge})
		var read struct {
			Compat  map[string]map[string]bool
			Servers map[string]map[string]any `toml:"mcp_servers"`
		}
		raw, err := os.ReadFile(filepath.Join(box.Home(), "observed.config.toml"))
		if _, derr := toml.Decode(string(raw), &read); err != nil || derr != nil {
			t.Fatalf("run %d: the settings %q (%v, %v)", run, raw, err, derr)
		}
		ours := 0
		for name := range read.Servers {
			if strings.HasPrefix(name, "crewlet_") {
				ours++
			}
		}
		if _, set := read.Compat["claude"]["mcps"]; !set || read.Servers["docs"]["url"] != "https://docs.example.com/mcp" ||
			ours != 1 {
			t.Errorf("run %d: Grok was handed %s, want the box's own beside this run's one server", run, raw)
		}
		if after, err := os.ReadFile(filepath.Join(box.Home(), ".grok", "config.toml")); err != nil ||
			string(after) != settings {
			t.Errorf("run %d: the box's settings after the run are %q (%v), want its own back", run, after, err)
		}
	}
	checkHome(t, box, c, above)

	got := observedEnv(t, box)
	for _, name := range off {
		if value, ok := got[name]; ok {
			t.Errorf("grok started with %s=%q, which moves what its runner writes and reads", name, value)
		}
	}
	for _, name := range on {
		if got[name] != "true" {
			t.Errorf("grok started with %s=%q, want the box's own true", name, got[name])
		}
	}
	if brief := boxFile(t, box, executor.PathsFor(box).Brief()); brief != "session-info" {
		t.Errorf("grok was handed %q, want the brief's words, no command", brief)
	}
}

// A GROK RUN'S REQUESTS GO WHERE A PERSON'S WOULD: each variable measured on
// 1.0.92 to move or sign its requests reaches `grok` as the box's login
// profile or the entry sets it, and an operator's own exporter set in the
// run's environment reaches it untouched.
//
// Mutations, each red: unset a routing variable the entry does not set; strip
// the OTLP exporter names again; drop a variable from the runner's declaration.
func TestAGrokRunsRequestsGoWhereAPersonsWould(t *testing.T) {
	t.Parallel()
	runner, box := standInBox(t, executor.NewGrok(), map[string]string{"grok": "#!/bin/sh\n" +
		"case \"$1\" in --version) echo 'grok 1.0.92 (b06756fd6256)'; exit 0;; esac\n" + observedRun})
	const entry, image = "https://gateway.example.com/v1", "http://image.example.com"
	// THE MEASURED LIST, kept here, and the runner's declaration held to it.
	routing := []string{"GROK_XAI_API_BASE_URL", "GROK_MODELS_BASE_URL", "GROK_CODE_XAI_API_KEY",
		"GROK_AUTH_PROVIDER_COMMAND"}
	if got := slices.Sorted(slices.Values(executor.GrokRouting)); !slices.Equal(got,
		slices.Sorted(slices.Values(routing))) {
		t.Errorf("the Grok runner declares %v, and 1.0.92 was measured routing by %v", got, routing)
	}
	var profile strings.Builder
	for _, name := range routing {
		if name != "GROK_MODELS_BASE_URL" {
			line, err := envfile.FormatAssignment(name, image)
			if err != nil {
				t.Fatal(err)
			}
			profile.WriteString(line + "\n")
		}
	}
	if err := box.WriteFile(t.Context(), box.Home()+"/.profile", []byte(profile.String())); err != nil {
		t.Fatal(err)
	}
	runObserved(t, runner, box, sandbox.RunRequest{Brief: "x", Model: "grok-4.6", Env: map[string]string{
		"GROK_MODELS_BASE_URL": entry, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector.example.com"}})
	env := observedEnv(t, box)
	for _, name := range routing {
		want := image
		if name == "GROK_MODELS_BASE_URL" {
			want = entry
		}
		if got := env[name]; got != want {
			t.Errorf("grok started with %s=%q, want %q", name, got, want)
		}
	}
	if got := env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"]; got != "http://collector.example.com" {
		t.Errorf("grok started with OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=%q, want the operator's own", got)
	}
}

// A GROK RUN CUT OFF LEAVES NO TABLE OF ITS OWN IN THE BOX'S SETTINGS PAST
// THE NEXT RUN. The box's settings hold a key, a server and a hook of its
// own; the first run's script is killed as Grok starts, so its servers and
// note hooks stand in the file. A person then adds a server. The next run
// starts from the settings as they stand less the cut-off run's tables, its
// own beside them, and once it ends the file holds the box's key, server and
// hook and the person's server, with no run's server, token or hook in it.
// Mutations, each red: start from the kept copy as before (the person's
// server is lost); take no server out by the record; keep the note hooks.
func TestAGrokRunCutOffLeavesNoTableOfItsOwnInTheBoxsSettings(t *testing.T) {
	t.Parallel()
	runner, box := standInBox(t, executor.NewGrok(), map[string]string{"grok": "#!/bin/sh\n" +
		"case \"$1\" in --version) echo 'grok 1.0.92 (b06756fd6256)'; exit 0;; inspect) exit 1;; esac\n" +
		"cp \"$HOME/.grok/config.toml\" \"$HOME/seen.toml\"\n" +
		"if [ -e \"$HOME/cut\" ]; then rm -f \"$HOME/cut\"; kill -KILL \"$PPID\"; exit 1; fi\n" + observedRun})
	p := executor.PathsFor(box)
	const own = "model = \"grok-4.6\"\n\n[mcp_servers.homesrv]\ncommand = \"home-srv\"\n\n" +
		"[[hooks.Stop]]\nhooks = [{ type = \"command\", command = \"home-hook\" }]\n"
	plantFile(t, p.GrokConfig(), own)
	plantFile(t, box.Home()+"/cut", "")
	type settings struct {
		Model   string
		Servers map[string]map[string]any `toml:"mcp_servers"`
		Hooks   map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	read := func(t *testing.T, file string) (settings, string) {
		t.Helper()
		var s settings
		raw := boxFile(t, box, file)
		if _, err := toml.Decode(raw, &s); err != nil || s.Model != "grok-4.6" {
			t.Fatalf("%s is %q (%v), want the box's settings", file, raw, err)
		}
		return s, raw
	}
	helpers := func(s settings, event string) (ours int, home bool) {
		for _, e := range s.Hooks[event] {
			for _, h := range e.Hooks {
				ours += strings.Count(h.Command, p.GrokNoteHelper())
				home = home || h.Command == "home-hook"
			}
		}
		return ours, home
	}
	run := func(servers map[string]sandbox.MCPServer) {
		t.Helper()
		handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{Brief: "work", MCPServers: servers,
			Steer: loadsSteer})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		awaitOver(t, runner, box, handle, time.Minute, "the stand-in only records, so the run is stuck")
	}
	bridge := func(token string) sandbox.MCPServer {
		return sandbox.MCPServer{Transport: sandbox.TransportHTTP, URL: "http://engine.example.com/mcp/" + token}
	}

	run(map[string]sandbox.MCPServer{"crewlet": bridge("TOKEN-CUT"), "seat_tools": bridge("TOKEN-CUT-TOOLS")})
	if _, raw := read(t, p.GrokConfig()); !strings.Contains(raw, "TOKEN-CUT-TOOLS") {
		t.Fatalf("the box's settings after the cut-off run are %q, want the run's tables left in them", raw)
	}
	plantFile(t, p.GrokConfig(), boxFile(t, box, p.GrokConfig())+"\n[mcp_servers.added]\ncommand = \"added\"\n")

	run(map[string]sandbox.MCPServer{"crewlet": bridge("TOKEN-NEXT")})
	t.Run("the next run starts without the cut-off run's tables", func(t *testing.T) {
		seen, raw := read(t, box.Home()+"/seen.toml")
		ours, home := helpers(seen, "Stop")
		if strings.Contains(raw, "TOKEN-CUT") || !strings.Contains(raw, "TOKEN-NEXT") || seen.Servers["added"] == nil ||
			seen.Servers["homesrv"] == nil || ours != 1 || !home {
			t.Errorf("the next run's Grok started on %s, want the box's and the person's tables and its own once", raw)
		}
	})
	t.Run("after it the box's settings hold the box's and the person's alone", func(t *testing.T) {
		after, raw := read(t, p.GrokConfig())
		ours, home := helpers(after, "Stop")
		if strings.Contains(raw, "TOKEN") || strings.Contains(raw, p.GrokNoteHelper()) || ours != 0 || !home ||
			len(after.Servers) != 2 || after.Servers["added"] == nil || after.Servers["homesrv"] == nil {
			t.Errorf("after the next run the box's settings are %s, want the box's own tables and the person's", raw)
		}
		for _, f := range []string{p.GrokConfigBox(), p.GrokConfigOurs()} {
			if got := boxFile(t, box, f); got != "" {
				t.Errorf("%s is left after the run: %q", f, got)
			}
		}
	})
}
