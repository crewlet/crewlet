package executor_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// copilotLoadsStandIn is the `copilot` a Copilot run's row runs: it answers
// the release check, then records where it started, its argv and environment,
// the MCP file its argv names, the home's settings, the run's instructions and
// every file under the box home as the harness starts.
const copilotLoadsStandIn = `#!/bin/sh
case "$1" in --version) echo 'GitHub Copilot CLI 1.0.114.'; exit 0;; esac
s="$HOME/seen"; mkdir -p "$s"
pwd > "$s/pwd"; printf '%s\n' "$@" > "$s/argv"; env > "$s/env"
for a in "$@"; do case "$a" in @*) cat "${a#@}" > "$s/mcp";; esac; done
cat "$COPILOT_HOME/settings.json" > "$s/settings"
cat "$COPILOT_HOME/copilot-instructions.md" > "$s/instructions"
(cd "$HOME" && find . -path ./seen -prune -o -type f -print) | sort > "$s/files"
`

// copilotLoads is the Copilot row of TestEveryRunLoadsWhatAPersonsSessionLoads.
// The start directory (the box's workspace) holds a repository's instructions,
// settings, hooks, extension, skills and MCP servers, and the Copilot home a
// person's own of each and more: as `copilot` starts every one is there, the
// home's settings and MCP file keep their entries beside the run's, the notes
// extension sits beside the home's, the prompt-mode switches are on and the
// added instruction and skill directories reach it. A later run with neither
// notes nor a cap finds the earlier run's extension and instructions gone.
//
// Mutations, each red: clear any of the home's places again; unset
// COPILOT_SKILLS_DIRS; drop a prompt-mode pin; write the settings whole.
func copilotLoads(t *testing.T) {
	runner, box := standInBox(t, executor.NewCopilot(), map[string]string{"copilot": copilotLoadsStandIn})
	p := executor.PathsFor(box)
	home, h := box.Home(), p.CopilotHome()
	start := home + "/workspace"

	planted := map[string]string{
		"the repository's AGENTS.md":               start + "/AGENTS.md",
		"the repository's copilot-instructions.md": start + "/.github/copilot-instructions.md",
		"the repository's instruction file":        start + "/.github/instructions/repo.instructions.md",
		"the repository's settings":                start + "/.github/copilot/settings.json",
		"the repository's hook":                    start + "/.github/hooks/repo.json",
		"the repository's extension":               start + "/.github/extensions/repo/extension.mjs",
		"the repository's skill":                   start + "/.github/skills/repo/SKILL.md",
		"the repository's .agents skill":           start + "/.agents/skills/repo-agents/SKILL.md",
		"the repository's MCP servers":             start + "/.mcp.json",
		"the home's instruction file":              h + "/instructions/home.instructions.md",
		"the home's hook":                          h + "/hooks/home.json",
		"the home's extension":                     h + "/extensions/home/extension.mjs",
		"the home's .agents skill":                 home + "/.agents/skills/home-agents/SKILL.md",
		"the home's skill":                         h + "/skills/home/SKILL.md",
		"the home's MCP servers":                   h + "/mcp-config.json",
		"the home's agent":                         h + "/agents/home.agent.md",
		"the home's plugin":                        h + "/installed-plugins/_direct/home/plugin.json",
		"the home's language server":               h + "/lsp-config.json",
		"the home's saved provider":                h + "/providers.json",
		"an instruction directory the run names":   home + "/more/instructions/AGENTS.md",
		"a skill directory the run names":          home + "/more/skills/extra/SKILL.md",
		"the login":                                h + "/config.json",
	}
	for _, file := range planted {
		plantFile(t, file, "planted\n")
	}
	plantFile(t, h+"/settings.json", `{"model":"home-model"}`)
	plantFile(t, h+"/mcp-config.json", `{"mcpServers":{"home-mcp":{"type":"local","command":"home-mcp"}}}`)

	run := func(req sandbox.RunRequest) (files []string, env map[string]string) {
		t.Helper()
		handle, err := runner.Start(t.Context(), box, req)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		awaitOver(t, runner, box, handle, time.Minute, "the stand-in only records, so the run is stuck")
		if got := strings.TrimSpace(boxFile(t, box, home+"/seen/pwd")); got != start {
			t.Fatalf("copilot started in %q, want the start directory %q:\n%s", got, start, boxFile(t, box, p.Err()))
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
			t.Errorf("%s (%s) was not there as copilot started, so the run did not load it", place, file)
		}
	}

	added := map[string]string{"COPILOT_CUSTOM_INSTRUCTIONS_DIRS": home + "/more/instructions",
		"COPILOT_SKILLS_DIRS": home + "/more/skills"}
	files, env := run(sandbox.RunRequest{Brief: "work", System: "Work well.", Env: added,
		Limits: sandbox.Limits{MaxTurns: 3}, MCPServers: map[string]sandbox.MCPServer{
			"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://engine.example.com/mcp/t"}}})
	for place, file := range planted {
		t.Run(place, func(t *testing.T) { present(t, files, place, file) })
	}
	t.Run("Crewlet's own beside the home's", func(t *testing.T) {
		present(t, files, "the run's notes extension", p.CopilotExtension()+"/extension.mjs")
		present(t, files, "the run's standing instructions", p.CopilotInstructions())
		if got := boxFile(t, box, home+"/seen/instructions"); !strings.Contains(got, "Work well.") {
			t.Errorf("the run's instructions are %q, want its own", got)
		}
		var settings map[string]any
		if raw := boxFile(t, box, home+"/seen/settings"); json.Unmarshal([]byte(raw), &settings) != nil ||
			settings["model"] != "home-model" || settings["experimental"] != true {
			t.Errorf("the home's settings as copilot started are %s, want its own model beside the run's keys", raw)
		}
		if raw := boxFile(t, box, h+"/mcp-config.json"); !strings.Contains(raw, `"home-mcp"`) {
			t.Errorf("the home's MCP servers are %s, want its own server kept", raw)
		}
		if raw := boxFile(t, box, home+"/seen/mcp"); !strings.Contains(raw, `"crewlet"`) ||
			strings.Contains(raw, "home-mcp") {
			t.Errorf("the run's own MCP file is %s, want Crewlet's server alone beside the home's", raw)
		}
		argv := strings.Split(boxFile(t, box, home+"/seen/argv"), "\n")
		if at := slices.Index(argv, "--enable-mcp-server"); at < 0 || argv[at+1] != "crewlet" ||
			!slices.Contains(argv, "--additional-mcp-config") {
			t.Errorf("Crewlet's server is not added and enabled by name: %q", argv)
		}
	})
	t.Run("the switches a person's session loads by", func(t *testing.T) {
		for _, name := range []string{"GITHUB_COPILOT_PROMPT_MODE_REPO_HOOKS",
			"GITHUB_COPILOT_PROMPT_MODE_WORKSPACE_MCP", "GITHUB_COPILOT_PROMPT_MODE_EXTENSIONS"} {
			if env[name] != "true" {
				t.Errorf("%s = %q, want true", name, env[name])
			}
		}
		for name, want := range added {
			if env[name] != want {
				t.Errorf("%s = %q, want the run's %q", name, env[name], want)
			}
		}
	})

	files, _ = run(sandbox.RunRequest{Brief: "work"})
	t.Run("a later run in the same box", func(t *testing.T) {
		if file := p.CopilotExtension() + "/extension.mjs"; slices.Contains(files, "."+strings.TrimPrefix(file, home)) {
			t.Errorf("%s from the earlier run was still there", file)
		}
		if got := boxFile(t, box, home+"/seen/instructions"); strings.Contains(got, "Work well.") {
			t.Errorf("the run's instructions still hold the earlier run's: %q", got)
		}
		for place, file := range planted {
			present(t, files, place, file)
		}
		if _, err := os.Stat(h + "/extensions/home/extension.mjs"); err != nil {
			t.Errorf("the home's extension went with the run's: %v", err)
		}
	})
}

// A COPILOT RUN KEEPS THE PERSON'S OWN INSTRUCTIONS AND SETTINGS. Copilot
// 1.0.114 reads one copilot-instructions.md and one settings.json from its
// home: as `copilot` starts the instructions file holds the run's text with
// the person's after it, and the settings the run's `experimental` beside the
// person's keys; once the run ends both files are the person's again, byte
// for byte, a link the person keeps one still a link.
//
// Mutations, each red: write the run's text alone; drop the script's put-back;
// put back by moving the held copy over the link.
func TestACopilotRunKeepsThePersonsOwnInstructionsAndSettings(t *testing.T) {
	t.Parallel()
	runner, box := standInBox(t, executor.NewCopilot(), map[string]string{"copilot": copilotLoadsStandIn})
	p := executor.PathsFor(box)
	const settings, own = `{"theme":"dark","experimental":false}`, "PERSON'S OWN\n"
	plantFile(t, p.CopilotSettings(), settings)
	dotfile := box.Home() + "/dotfiles/copilot-instructions.md"
	plantFile(t, dotfile, own)
	if err := os.Symlink(dotfile, p.CopilotInstructions()); err != nil {
		t.Fatal(err)
	}
	runObserved(t, runner, box, sandbox.RunRequest{Brief: "go", System: "SEAT"})

	if got := boxFile(t, box, box.Home()+"/seen/instructions"); !strings.HasPrefix(got, "SEAT") ||
		!strings.HasSuffix(got, "\n\n"+own) {
		t.Errorf("copilot started with instructions %q, want the run's and the person's after them", got)
	}
	var seen map[string]any
	if err := json.Unmarshal([]byte(boxFile(t, box, box.Home()+"/seen/settings")), &seen); err != nil ||
		seen["experimental"] != true || seen["theme"] != "dark" {
		t.Errorf("copilot started with settings %v (%v), want the run's experimental beside the person's", seen, err)
	}
	if got := boxFile(t, box, p.CopilotSettings()); got != settings {
		t.Errorf("the person's settings read %s after the run, want %s", got, settings)
	}
	if fi, err := os.Lstat(p.CopilotInstructions()); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the person's linked instructions are no link after the run (%v)", err)
	}
	if got := boxFile(t, box, dotfile); got != own {
		t.Errorf("the person's instructions read %q after the run, want %q", got, own)
	}
}

// A KILLED COPILOT RUN'S SETTINGS AND INSTRUCTIONS LEAVE THE HOME AT THE NEXT
// START, whichever harness it runs: the held copies go back (an empty file
// comes back empty), a file the home did not have goes, and no held copy stays.
//
// Mutations, each red: drop Copilot's homeRestore from the clear; delete an
// empty held file's home file.
func TestAKilledCopilotRunsFilesLeaveTheHomeAtTheNextStart(t *testing.T) {
	t.Parallel()
	runner, box := realBox(t)
	p := executor.PathsFor(box)
	plantFile(t, p.CopilotHeldSettings(), "")
	plantFile(t, p.CopilotSettings(), `{"experimental":true}`)
	plantFile(t, p.CopilotHeldInstructions()+".none", "")
	plantFile(t, p.CopilotInstructions(), "SEAT")
	if err := runner.ClearArtifacts(t.Context(), box); err != nil {
		t.Fatalf("the clear: %v", err)
	}
	if got, err := os.ReadFile(p.CopilotSettings()); err != nil || len(got) != 0 {
		t.Errorf("the home's empty settings read %q (%v) after the clear, want them empty", got, err)
	}
	for _, f := range []string{p.CopilotInstructions(), p.CopilotHeldSettings(), p.CopilotHeldInstructions() + ".none"} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s is still there after the clear (%v)", f, err)
		}
	}
}

// A PUT-BACK THAT FAILS FAILS THE CLEAR, naming the file: the next run would
// otherwise start on a home file still holding a killed run's settings.
//
// Mutation: run the clear's put-backs without checking their status.
func TestAClearWhosePutBackFailsFailsNamingTheFile(t *testing.T) {
	t.Parallel()
	runner, box := realBox(t)
	p := executor.PathsFor(box)
	plantFile(t, p.CopilotHeldSettings(), `{"theme":"dark"}`)
	if err := os.MkdirAll(p.CopilotSettings(), 0o700); err != nil {
		t.Fatal(err)
	}
	err := runner.ClearArtifacts(t.Context(), box)
	if err == nil || !strings.Contains(err.Error(), p.CopilotSettings()) {
		t.Fatalf("the clear = %v, want it to fail naming %s", err, p.CopilotSettings())
	}
	if _, err := os.Stat(p.CopilotHeldSettings()); err != nil {
		t.Errorf("the held copy is gone although it was not put back (%v)", err)
	}
}
