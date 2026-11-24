package executor_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// museLoadsStandIn records every `muse` call's words in $HOME/muse-calls and,
// for the run's `muse exec`, its argv, every file under the home and the
// home's settings as the harness starts; the release check is answered.
const museLoadsStandIn = `#!/bin/sh
printf '%s\n' "$*" >> "$HOME/muse-calls"
case "$1" in --version) echo 'Muse Code 1.4.25 (1.4.25-R9361.1)'; exit 0;; exec) ;; *) exit 0;; esac
printf '%s\n' "$@" > "$HOME/observed.argv"
(cd "$HOME" && find . -type f) > "$HOME/observed.files"
cp "$HOME/.config/muse/settings.json" "$HOME/observed.settings"
`

// museLoads is Muse Code's row of TestEveryRunLoadsWhatAPersonsSessionLoads.
// A real box whose home and start directory (`workspace/`) hold what a
// person's `muse exec` loads (measured on 1.4.25): instructions, settings with
// an MCP server, a plugin (Muse's hooks come only from plugins), skills, and
// another harness's personal rules and skills, which it loads without
// `--no-foreign-personal-context`. As `muse exec` starts each is there, the
// home's settings with the run's instructions set in them, telemetry off as
// `-c`; its notes plugin (its hook) replaces an earlier one; its MCP server is
// added beside the home's after the last run's goes, and taken out once Muse
// exits; its skills replace an earlier run's `crewlet:*`. Mutations, each red:
// clear a home place again; restore the flag; write the settings whole; drop
// the plugin's removal, the servers' (either end) or the skills' clear.
func museLoads(t *testing.T) {
	runner, box := standInBox(t, executor.NewMuse(), map[string]string{"muse": museLoadsStandIn})
	p := executor.PathsFor(box)
	home := []string{".config/muse/AGENTS.md", ".agents/skills/home/SKILL.md", ".config/muse/skills/home/SKILL.md",
		".local/share/muse/plugins/installed.json", ".local/share/muse/plugins/cache/local/home/0/package/hooks.json",
		".claude/CLAUDE.md", ".claude/skills/home/SKILL.md", ".codex/AGENTS.md",
		"workspace/AGENTS.md", "workspace/.agents/skills/repo/SKILL.md", "workspace/.mcp.json"}
	for _, rel := range home {
		plantFile(t, filepath.Join(box.Home(), rel), "a person's own\n")
	}
	// THE PLUGIN STORE AS MUSE WRITES IT, which it must read to load any plugin.
	plantFile(t, p.MuseInstalled(), `{"version":1,"plugins":{"home":{"enabled":true}}}`)
	settings := `{"schema_version":1,"provider_retry":{"max_retries":3},` +
		`"mcpServers":{"home":{"type":"stdio","command":"home-server"}}}`
	plantFile(t, p.MuseSettings(), strings.Replace(settings, `"mcpServers":{`,
		`"mcpServers":{"crewlet-0ld000":{"type":"http","url":"http://e/mcp/OLD-TOKEN"},`, 1))
	plantFile(t, p.MuseSkills()+"/crewlet:earlier/SKILL.md", "an earlier run's\n")
	plantFile(t, p.MuseServersAdded(), "crewlet-0ld000\n")

	runObserved(t, runner, box, sandbox.RunRequest{Brief: "x", System: "SEAT", Skills: handedSkills(),
		SkillLoads: skillWiring(),
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://e/mcp"}},
		Steer:      sandbox.SteerWiring{Server: "crewlet", Tool: mcpbridge.SteerTool, URL: "http://e/steer"}})

	files := strings.Split(boxFile(t, box, box.Home()+"/observed.files"), "\n")
	for _, rel := range home {
		if !slices.Contains(files, "./"+rel) {
			t.Errorf("%s was gone as muse exec started: the run would not load it", rel)
		}
	}
	for _, s := range handedSkills() {
		if !slices.Contains(files, "./.agents/skills/"+s.Listed()+"/SKILL.md") {
			t.Errorf("the run's own skill %s was not there beside the home's", s.Listed())
		}
	}
	if slices.Contains(files, "./.agents/skills/crewlet:earlier/SKILL.md") {
		t.Error("an earlier run's skill crewlet:earlier was still there")
	}
	var got, want map[string]any
	if err := json.Unmarshal([]byte(boxFile(t, box, box.Home()+"/observed.settings")), &got); err != nil {
		t.Fatalf("the home's settings as muse exec started: %v", err)
	}
	_ = json.Unmarshal([]byte(settings), &want)
	if run, _ := got["run"].(map[string]any); run == nil || !strings.HasPrefix(fmt.Sprint(run["system_prompt_append"]), "SEAT") {
		t.Errorf("the home's settings held %v as muse exec started, want the run's standing instructions", got)
	}
	delete(got, "run")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the home's settings were %v as muse exec started, want the home's own %v kept", got, want)
	}
	argv := boxFile(t, box, box.Home()+"/observed.argv")
	if !strings.Contains(argv, "\n-c\ntelemetry.enabled=false\n--prompt-file\n") ||
		strings.Contains(argv, "--no-foreign-personal-context") {
		t.Errorf("muse exec started with\n%s\nwant telemetry off as -c and no foreign context held out", argv)
	}
	calls := boxFile(t, box, box.Home()+"/muse-calls")
	last := -1
	_, added, _ := strings.Cut(calls, "mcp add-json ")
	added, _, _ = strings.Cut(added, " ")
	for _, step := range []string{"plugins remove crewlet-notes", "plugins install " + p.MuseNotesPlugin(),
		"plugins approve crewlet-notes", "mcp add-json crewlet-", "exec ", "mcp remove " + added + " --user"} {
		at := strings.Index(calls, step)
		if at <= last {
			t.Fatalf("%q is not after the step before it in the calls:\n%s", step, calls)
		}
		last = at
	}
	if added == "crewlet-0ld000" || strings.Contains(calls, "crewlet-0ld000") {
		t.Errorf("the run added or removed the last run's server by muse, want it gone before:\n%s", calls)
	}
	if got := boxFile(t, box, p.MuseServersAdded()); got != "" {
		t.Errorf("the record of the servers added holds %q after the run, want them all taken out", got)
	}
}

// plantFile writes body to file, making its directories.
func plantFile(t *testing.T, file, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A MUSE RUN KEEPS THE HOME'S OWN STANDING INSTRUCTIONS. Muse Code 1.4.25 reads
// one `run.system_prompt_append`, from the home's settings: as `muse exec`
// starts it holds the home's own append with the run's after it, and once the
// run ends the file is the home's again, byte for byte.
//
// Mutations, each red: write the run's text in place of the home's; drop the
// script's restore.
func TestAMuseRunKeepsTheHomesOwnStandingInstructions(t *testing.T) {
	t.Parallel()
	runner, box := standInBox(t, executor.NewMuse(), map[string]string{"muse": museLoadsStandIn})
	p := executor.PathsFor(box)
	const home = `{"schema_version":1,"run":{"system_prompt_append":"HOME"},"provider_retry":{"max_retries":3}}`
	plantFile(t, p.MuseSettings(), home)

	runObserved(t, runner, box, sandbox.RunRequest{Brief: "x", System: "SEAT",
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://e/mcp"}}})

	var got struct {
		Run struct {
			Append string `json:"system_prompt_append"`
		} `json:"run"`
	}
	if err := json.Unmarshal([]byte(boxFile(t, box, box.Home()+"/observed.settings")), &got); err != nil {
		t.Fatalf("the home's settings as muse exec started: %v", err)
	}
	if !strings.HasPrefix(got.Run.Append, "HOME\n\nSEAT\n") {
		t.Errorf("muse exec started with run.system_prompt_append %q, want the home's own and the run's after it",
			got.Run.Append)
	}
	if after := boxFile(t, box, p.MuseSettings()); after != home {
		t.Errorf("the home's settings read %s after the run, want them back as the home had them, %s", after, home)
	}
	if _, err := os.Stat(p.MuseHeldSettings()); !os.IsNotExist(err) {
		t.Errorf("the held copy of the home's settings is still there after the run (%v)", err)
	}
}

// A KILLED MUSE RUN'S LEFTOVERS GO AT THE NEXT START. A run killed before its
// script put the home's settings back leaves its servers and its standing
// instructions in them; the clear before the next run takes the servers out
// and puts the home's own append back (or none, where it had none), keeping
// every other key, an edit made since included, and no held copy stays.
//
// Mutations, each red: skip the append's return in the clear; keep the held
// copy; write the held copy whole over the file.
func TestAKilledMuseRunsInstructionsLeaveTheHomeAtTheNextStart(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, held, want string
	}{
		{name: "the home's own append",
			held: `{"schema_version":1,"run":{"system_prompt_append":"HOME"}}`,
			want: `{"provider_retry":{"max_retries":5},"run":{"system_prompt_append":"HOME"},"schema_version":1}`},
		{name: "no append of the home's", held: `{"schema_version":1}`,
			want: `{"provider_retry":{"max_retries":5},"schema_version":1}`},
		{name: "no settings of the home's", held: "",
			want: `{"provider_retry":{"max_retries":5},"schema_version":1}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runner, box := standInBox(t, executor.NewMuse(), map[string]string{"muse": museLoadsStandIn})
			p := executor.PathsFor(box)
			plantFile(t, p.MuseHeldSettings(), c.held)
			plantFile(t, p.MuseServersAdded(), "crewlet-0ld000\n")
			plantFile(t, p.MuseSettings(), `{"schema_version":1,"run":{"system_prompt_append":"HOME\n\nSEAT"},`+
				`"provider_retry":{"max_retries":5},"mcpServers":{"crewlet-0ld000":{"url":"http://e/mcp/OLD-TOKEN"}}}`)
			if err := runner.ClearArtifacts(t.Context(), box); err != nil {
				t.Fatalf("the clear: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(boxFile(t, box, p.MuseSettings())), &got); err != nil {
				t.Fatalf("the home's settings after the clear: %v", err)
			}
			if servers, _ := got["mcpServers"].(map[string]any); len(servers) == 0 {
				delete(got, "mcpServers")
			}
			if out, _ := json.Marshal(got); string(out) != c.want {
				t.Errorf("the home's settings read %s after the clear, want %s", out, c.want)
			}
			for _, f := range []string{p.MuseHeldSettings(), p.MuseServersAdded()} {
				if _, err := os.Stat(f); !os.IsNotExist(err) {
					t.Errorf("%s is still there after the clear (%v)", f, err)
				}
			}
		})
	}
}
