package executor_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// piLoadsStandIn is a `pi` that answers the release check every run's script
// asks first, and otherwise records where it started, its argv, every file
// under the home, and the agent directory's settings and MCP servers as it
// found them.
const piLoadsStandIn = `#!/bin/sh
case "$1" in --version) echo 1.6.0; exit 0;; esac
s="$HOME/seen"; rm -rf "$s"; mkdir -p "$s"
pwd > "$s/pwd"
printf '%s\n' "$@" > "$s/argv"
(cd "$HOME" && find . -type f ! -path './seen/*') > "$s/files"
cat "$PI_CODING_AGENT_DIR/settings.json" > "$s/settings" 2> /dev/null
cat "$PI_CODING_AGENT_DIR/mcp.json" > "$s/mcp" 2> /dev/null
cat > /dev/null
`

// piLoads is Pi's row of TestEveryRunLoadsWhatAPersonsSessionLoads. The
// working directory is trusted (`--approve`) and nothing of discovery is
// switched off, so pi loads the repository's `.pi` and the agent directory's
// own beside the run's extension and skills; pi's hooks are its extensions'
// handlers. Every planted file is there, unchanged, as pi starts; only `bin`
// is cleared. A later run in the box holds none of the earlier run's skills.
//
// Mutation: leave the working directory untrusted, add a --no-* switch, clear
// or write a file of the agent directory's other than `bin`, keep `bin`, or
// drop Crewlet's extension or its server: this goes red.
func piLoads(t *testing.T) {
	runner, box := standInBox(t, executor.NewPi(), map[string]string{"pi": piLoadsStandIn})
	p := executor.PathsFor(box)
	home, a := box.Home(), p.PiAgentDir()
	start := home + "/workspace"
	read := func(path string) string {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(raw)
	}
	plant := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	const homeMCP = `{"mcpServers":{"home-mcp":{"command":"home-mcp"}}}`
	const homeSettings = `{"theme":"home","shellCommandPrefix":"set -o pipefail"}`
	planted := map[string]string{
		"the repository's AGENTS.md":          start + "/AGENTS.md",
		"the repository's system prompt":      start + "/.pi/SYSTEM.md",
		"the repository's appended prompt":    start + "/.pi/APPEND_SYSTEM.md",
		"the repository's settings":           start + "/.pi/settings.json",
		"the repository's MCP servers":        start + "/.pi/mcp.json",
		"the repository's extension (hooks)":  start + "/.pi/extensions/repo.ts",
		"the repository's skill":              start + "/.pi/skills/repo/SKILL.md",
		"the repository's .agents skill":      start + "/.agents/skills/repo-agents/SKILL.md",
		"the repository's prompt template":    start + "/.pi/prompts/repo.md",
		"the home's AGENTS.md":                a + "/AGENTS.md",
		"the home's system prompt":            a + "/SYSTEM.md",
		"the home's appended prompt":          a + "/APPEND_SYSTEM.md",
		"the home's settings":                 a + "/settings.json",
		"the home's MCP servers":              a + "/mcp.json",
		"the home's extension (hooks)":        a + "/extensions/home.ts",
		"the home's skill":                    a + "/skills/home/SKILL.md",
		"the home's .agents skill":            home + "/.agents/skills/home-agents/SKILL.md",
		"the home's prompt template":          a + "/prompts/home.md",
		"the home's theme":                    a + "/themes/home.json",
		"the home's provider definitions":     a + "/models.json",
		"the home's keybindings":              a + "/keybindings.json",
		"the login":                           a + "/auth.json",
		"the home's installed package":        a + "/npm/node_modules/home-pkg/package.json",
		"the home's saved trust of a project": a + "/trust.json",
	}
	for _, file := range planted {
		plant(file, "planted\n")
	}
	plant(start+"/.pi/mcp.json", `{"mcpServers":{"repo-mcp":{"command":"repo-mcp"}}}`)
	plant(a+"/mcp.json", homeMCP)
	plant(a+"/settings.json", homeSettings)
	plant(a+"/bin/rg", "#!/bin/sh\necho planted\n")

	run := func(req sandbox.RunRequest) (files, argv []string) {
		t.Helper()
		handle, err := runner.Start(t.Context(), box, req)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		awaitOver(t, runner, box, handle, time.Minute, "the stand-in only records, so the run is stuck")
		if got := strings.TrimSpace(read(home + "/seen/pwd")); got != start {
			t.Fatalf("pi started in %q, want the start directory %q:\n%s", got, start, read(p.Err()))
		}
		return strings.Split(strings.TrimSpace(read(home+"/seen/files")), "\n"),
			strings.Split(strings.TrimSpace(read(home+"/seen/argv")), "\n")
	}
	present := func(files []string, file string) bool {
		return slices.Contains(files, "."+strings.TrimPrefix(file, home))
	}

	files, argv := run(sandbox.RunRequest{Brief: "work", System: "Work well.",
		Skills: []sandbox.Skill{{Name: "deploy", Description: "Deploys.", Body: "Do it."}},
		SkillLoads: sandbox.SkillWiring{Server: "crewlet", Tool: "skill_loaded",
			URL: "http://engine.example.com/mcp/t/skill"},
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Name: "crewlet", Transport: sandbox.TransportHTTP,
			URL: "http://engine.example.com/mcp/t", AlwaysLoaded: true}}})
	for place, file := range planted {
		t.Run(place, func(t *testing.T) {
			if !present(files, file) {
				t.Errorf("%s (%s) was not there as pi started, so the run did not load it", place, file)
			}
		})
	}
	t.Run("the working directory trusted, nothing switched off", func(t *testing.T) {
		if !slices.Contains(argv, "--approve") {
			t.Errorf("pi ran without --approve, so the repository's .pi is not loaded: %q", argv)
		}
		for _, off := range []string{"--no-approve", "--no-extensions", "--no-skills", "--no-prompt-templates",
			"--no-themes", "--no-context-files", "--no-mcp"} {
			if slices.Contains(argv, off) {
				t.Errorf("pi ran with %s: %q", off, argv)
			}
		}
	})
	t.Run("Crewlet's own beside the home's", func(t *testing.T) {
		extensions := []string{}
		for i, arg := range argv[:len(argv)-1] {
			if arg == "-e" {
				extensions = append(extensions, argv[i+1])
			}
		}
		if !slices.Equal(extensions, []string{p.PiExtension()}) {
			t.Errorf("pi loads %q by name, want the run's extension alone, its MCP support its own", extensions)
		}
		if at := slices.Index(argv, "--skill"); at < 0 || argv[at+1] != p.PiSkills() ||
			!present(files, p.PiSkills()+"/crewlet:deploy/SKILL.md") {
			t.Errorf("the run's skills are not named beside the home's: %q", argv)
		}
		if ext := read(p.PiExtension()); !strings.Contains(ext, `"name":"crewlet"`) ||
			!strings.Contains(ext, "http://engine.example.com/mcp/t") || !strings.Contains(ext, a+"/mcp.json") {
			t.Errorf("the run's extension does not register Crewlet's server and check the home's mcp.json")
		}
		if got := read(home + "/seen/mcp"); got != homeMCP {
			t.Errorf("the home's MCP servers read %q as pi started, want its own kept beside Crewlet's", got)
		}
		if got := read(home + "/seen/settings"); got != homeSettings {
			t.Errorf("the home's settings read %q as pi started, want them as the home wrote them", got)
		}
	})
	t.Run("bin cleared", func(t *testing.T) {
		if present(files, a+"/bin/rg") {
			t.Errorf("the agent directory's bin/rg was there as pi started, first on its shell's PATH")
		}
	})

	files, _ = run(sandbox.RunRequest{Brief: "again"})
	t.Run("a later run keeps none of the earlier run's skills", func(t *testing.T) {
		if present(files, p.PiSkills()+"/crewlet:deploy/SKILL.md") {
			t.Errorf("the earlier run's skill was still there")
		}
		for place, file := range planted {
			if !present(files, file) {
				t.Errorf("%s (%s) was gone by the later run", place, file)
			}
		}
	})
}
