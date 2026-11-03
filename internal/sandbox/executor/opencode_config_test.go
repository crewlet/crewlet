package executor_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// openCodeBox is a local box on the engine host, its boxes kept under
// stateDir, with an OpenCode runner's plumbing installed, as a launch leaves
// one.
func openCodeBox(t *testing.T, stateDir string) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: stateDir})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(t.Context()) })
	runner := executor.NewOpenCode()
	if err = runner.Install(t.Context(), box); err != nil {
		t.Fatalf("Install: %v", err)
	}
	return runner, box
}

// seatServers is a run's MCP surface with a value of every kind a server is
// configured with — the bridge's url, a remote server's url and headers, a
// local server's environment — each one distinctive, and some holding what
// JSON writes escaped.
func seatServers() map[string]sandbox.MCPServer {
	return map[string]sandbox.MCPServer{
		"crewlet": {Name: "crewlet", Transport: sandbox.TransportHTTP,
			URL: "http://127.0.0.1:7/mcp/run-token-placeholder"},
		"linear": {Name: "linear", Transport: sandbox.TransportHTTP,
			URL: "https://example.com/mcp?team=a&b=<c>",
			Headers: map[string]string{
				"Authorization": "Bearer header-value-placeholder",
				"X-Quoted":      `say "header" \ there`,
			}},
		"files": {Name: "files", Transport: sandbox.TransportStdio,
			Command: "mcp-files", Args: []string{"--root", "/src"},
			Env: map[string]string{
				"API_TOKEN": "env-value-placeholder",
				"MULTI":     "env line one\nenv line two",
			}},
	}
}

// serverValues is every value seatServers configures, by where it sits.
func serverValues(servers map[string]sandbox.MCPServer) map[string]string {
	out := map[string]string{}
	for name, s := range servers {
		if s.URL != "" {
			out[name+" url"] = s.URL
		}
		for k, v := range s.Headers {
			out[name+" header "+k] = v
		}
		for k, v := range s.Env {
			out[name+" environment "+k] = v
		}
	}
	return out
}

// configValues is every value a decoded configuration gives its servers, by
// where it sits, as serverValues names them.
func configValues(cfg openCodeConfig) map[string]string {
	out := map[string]string{}
	if cfg.MCP == nil {
		return out
	}
	for name, s := range cfg.MCP.Servers {
		if s.URL != "" {
			out[name+" url"] = s.URL
		}
		for k, v := range s.Headers {
			out[name+" header "+k] = v
		}
		for k, v := range s.Environment {
			out[name+" environment "+k] = v
		}
	}
	return out
}

// referencedPath is the path a whole `{file:<path>}` reference names, and
// false for a value that is not one.
func referencedPath(ref string) (string, bool) {
	inner, prefixed := strings.CutPrefix(ref, "{file:")
	path, suffixed := strings.CutSuffix(inner, "}")
	return path, prefixed && suffixed && path != ""
}

// fileReference is how OpenCode 2 finds a reference in a configuration's text
// (core/src/config/variable.ts, the same in 2.0.32 and 2.1.4).
var fileReference = regexp.MustCompile(`\{file:[^}]+\}`)

// asOpenCodeReadsIt is a configuration's text with every `{file:<path>}`
// reference replaced as OpenCode 2 replaces it before it parses the text: the
// file's content, its surrounding whitespace trimmed, JSON-escaped — and empty
// for a file it cannot read, which is not an error there and is one here. A
// reference after `//` on its line is left alone, as OpenCode leaves one in a
// comment.
func asOpenCodeReadsIt(t *testing.T, raw string) string {
	t.Helper()
	var b strings.Builder
	cursor := 0
	for _, m := range fileReference.FindAllStringIndex(raw, -1) {
		b.WriteString(raw[cursor:m[0]])
		cursor = m[1]
		token := raw[m[0]:m[1]]
		lineStart := strings.LastIndexByte(raw[:m[0]], '\n') + 1
		if strings.HasPrefix(strings.TrimSpace(raw[lineStart:m[0]]), "//") {
			b.WriteString(token)
			continue
		}
		path := strings.TrimSuffix(strings.TrimPrefix(token, "{file:"), "}")
		if !filepath.IsAbs(path) {
			t.Errorf("the reference %s is not absolute, so OpenCode resolves it against the checkout", token)
		}
		content, err := os.ReadFile(path) //nolint:gosec // a path the run's configuration names, under the test's box
		if err != nil {
			t.Errorf("the reference %s names no file OpenCode can read: %v", token, err)
		}
		escaped, err := json.Marshal(strings.TrimSpace(string(content)))
		if err != nil {
			t.Fatal(err)
		}
		b.Write(escaped[1 : len(escaped)-1])
	}
	b.WriteString(raw[cursor:])
	return b.String()
}

// snapshot is every file under dir, by its path relative to dir, as its mode
// and content.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			out[rel+"/"] = info.Mode().String()
			return nil
		}
		content, err := os.ReadFile(path) //nolint:gosec // under the test's own box
		if err != nil {
			return err
		}
		out[rel] = info.Mode().String() + " " + string(content)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// THE RUN'S CONFIGURATION IS WRITTEN BESIDE ITS OTHER ARTEFACTS, AND NOTHING IN
// THE CHECKOUT. The checkout is the repository's: its own opencode.json and
// .opencode/ stay as they were, byte for byte and mode for mode, and nothing is
// added beside them — the run's configuration is handed to its server as
// content, which OpenCode ranks above them. On a real box, so the modes are a
// backend's own: the configuration is written 0600.
//
// Mutation: write the configuration to <home>/workspace/opencode.json and the
// checkout's snapshot changes and the returned path is not the work dir's.
func TestTheRunsConfigurationIsWrittenUnderItsWorkDirectory(t *testing.T) {
	t.Parallel()
	_, box := openCodeBox(t, t.TempDir())
	p := executor.PathsFor(box)
	checkout := filepath.Join(p.Home, sandbox.WorkspaceSubdir)
	repository := map[string]string{
		"opencode.json":           `{"share":"auto","mcp":{"servers":{"crewlet":{"disabled":true}}}}` + "\n",
		".opencode/opencode.json": `{"permissions":[{"action":"*","resource":"*","effect":"deny"}]}`,
		"main.go":                 "package main\n",
	}
	for name, content := range repository {
		path := filepath.Join(checkout, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // a checkout's own directory
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // a checkout's own file
			t.Fatal(err)
		}
	}
	before := snapshot(t, checkout)

	path, err := executor.OpenCode{}.WriteConfig(t.Context(), box, sandbox.RunRequest{
		System: "the seat's standing instructions", MCPServers: seatServers(),
		Limits: sandbox.Limits{MaxTurns: 40},
	}, p)
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	if path != p.OpenCodeConfig() || !strings.HasPrefix(path, p.WorkDir()+"/") {
		t.Errorf("the configuration is at %q, want %q under the work directory %q",
			path, p.OpenCodeConfig(), p.WorkDir())
	}
	after := snapshot(t, checkout)
	for name, was := range before {
		if after[name] != was {
			t.Errorf("the checkout's %s changed:\nwas %q\nnow %q", name, was, after[name])
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("the run wrote %s into the checkout", name)
		}
	}
	info, err := os.Stat(p.OpenCodeConfig())
	if err != nil {
		t.Fatalf("stat the configuration: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the configuration is mode %o, want 0600", mode)
	}
}

// EVERY SERVER VALUE IS READ FROM A FILE OF ITS OWN. Each MCP server's url, each
// header value and each environment value is a `{file:<path>}` reference to a
// file under the run's values directory, written 0600 and holding the value as
// it is; the configuration's content holds none of them. And the configuration
// OpenCode reads once it has replaced every reference — the text read as it
// reads it — gives every server exactly the values the run was launched with,
// a value holding a quote, a backslash or a line break included.
//
// The box's home holds a `&`, which encoding/json writes as `\u0026` unless told
// not to: a reference so written names a file that is not there.
//
// Mutation: write a header's value into the configuration rather than a
// reference, and the content holds it; let encoding/json escape the text for
// HTML, and the references name nothing.
func TestEveryServerValueIsReadFromAFile(t *testing.T) {
	t.Parallel()
	_, box := openCodeBox(t, filepath.Join(t.TempDir(), "r&d"))
	p := executor.PathsFor(box)
	servers := seatServers()
	want := serverValues(servers)

	path, err := executor.OpenCode{}.WriteConfig(t.Context(), box, sandbox.RunRequest{MCPServers: servers}, p)
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the run's configuration, under the test's box
	if err != nil {
		t.Fatalf("read the configuration: %v", err)
	}
	var written openCodeConfig
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}

	refs := configValues(written)
	if len(refs) != len(want) {
		t.Errorf("the configuration gives its servers %d values, want %d:\n%s", len(refs), len(want), raw)
	}
	for at, value := range want {
		ref := refs[at]
		file, ok := referencedPath(ref)
		if !ok {
			t.Errorf("%s is %q, want a reference to the file holding it", at, ref)
			continue
		}
		if !strings.HasPrefix(file, p.OpenCodeValues()+"/") {
			t.Errorf("%s names %q, outside the run's values directory %q", at, file, p.OpenCodeValues())
		}
		info, err := os.Stat(file)
		if err != nil {
			t.Errorf("%s names %q, which is not there: %v", at, file, err)
			continue
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s's file is mode %o, want 0600", at, mode)
		}
		if got, _ := os.ReadFile(file); string(got) != value { //nolint:gosec // under the test's box
			t.Errorf("%s's file holds %q, want %q", at, got, value)
		}
		quoted, _ := json.Marshal(value)
		if strings.Contains(string(raw), value) || strings.Contains(string(raw), string(quoted[1:len(quoted)-1])) {
			t.Errorf("the configuration's content holds %s's value:\n%s", at, raw)
		}
	}

	var read openCodeConfig
	if err := json.Unmarshal([]byte(asOpenCodeReadsIt(t, string(raw))), &read); err != nil {
		t.Fatalf("the configuration OpenCode reads does not parse: %v", err)
	}
	for at, value := range want {
		if got := configValues(read)[at]; got != value {
			t.Errorf("OpenCode reads %s as %q, want %q", at, got, value)
		}
	}
}

// THE ENTRIES THE RUN OWNS SAY THEY ARE ON. OpenCode combines every document it
// reads, and one beneath the run's — the checkout's, the image's — may disable a
// server or an agent under the name the run declares; so every server the run
// declares, and the agent its turn cap is set on, states `disabled: false`
// rather than leave the field to whichever document set it.
//
// Mutation: drop `disabled` from a server's entry, or from the agent's.
func TestTheEntriesTheRunOwnsStateDisabledFalse(t *testing.T) {
	t.Parallel()
	b := sandbox.NewFakeSandbox("box-1")
	path, err := executor.OpenCode{}.WriteConfig(t.Context(), b, sandbox.RunRequest{
		MCPServers: seatServers(), Limits: sandbox.Limits{MaxTurns: 40},
	}, executor.PathsFor(b))
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	raw, _ := b.ReadFile(t.Context(), path)
	var cfg struct {
		DefaultAgent string `json:"default_agent"`
		Agents       map[string]struct {
			Disabled *bool `json:"disabled"`
		} `json:"agents"`
		MCP struct {
			Servers map[string]struct {
				Disabled *bool `json:"disabled"`
			} `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, raw)
	}
	if len(cfg.MCP.Servers) != len(seatServers()) {
		t.Fatalf("the configuration declares %d servers, want %d:\n%s", len(cfg.MCP.Servers), len(seatServers()), raw)
	}
	for name, s := range cfg.MCP.Servers {
		if s.Disabled == nil || *s.Disabled {
			t.Errorf("the server %s does not state disabled: false:\n%s", name, raw)
		}
	}
	agent, ok := cfg.Agents[cfg.DefaultAgent]
	if !ok || agent.Disabled == nil || *agent.Disabled {
		t.Errorf("the run's agent %q does not state disabled: false:\n%s", cfg.DefaultAgent, raw)
	}
}

// THE NOTES PLUGIN IS WRITTEN FOR A RUN WIRED TO ITS NOTES, AND ITS ROUTE IS A
// REFERENCE. The run's configuration names one plugin: the directory the
// engine's plugin is written into as index.js, byte for byte the plugin this
// build embeds, with the run's notes route as its one option — a `{file:}`
// reference to a 0600 file holding the route, so the configuration's content
// holds no token. A run wired to no notes is handed no plugin, and none is
// written.
//
// Mutation: write the route into the option rather than a reference, or name
// the plugin whatever the run's wiring.
func TestTheNotesPluginIsWrittenAndItsRouteReferenced(t *testing.T) {
	t.Parallel()
	_, box := openCodeBox(t, t.TempDir())
	p := executor.PathsFor(box)
	route := "http://127.0.0.1:7/mcp/run-token-placeholder/steer"
	path, err := executor.OpenCode{}.WriteConfig(t.Context(), box, sandbox.RunRequest{
		MCPServers: seatServers(),
		Steer:      sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take", URL: route},
	}, p)
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the run's configuration, under the test's box
	if err != nil {
		t.Fatalf("read the configuration: %v", err)
	}
	if strings.Contains(string(raw), "run-token-placeholder") {
		t.Errorf("the configuration's content holds the notes route:\n%s", raw)
	}
	plugin := steerPluginOf(t, path)
	if plugin.Package != p.OpenCodeSteerPlugin() {
		t.Errorf("the plugin is named at %q, want %q", plugin.Package, p.OpenCodeSteerPlugin())
	}
	file, ok := referencedPath(plugin.Options.URL)
	if !ok || !strings.HasPrefix(file, p.OpenCodeValues()+"/") {
		t.Fatalf("the plugin's route is %q, want a reference to a file under %q", plugin.Options.URL, p.OpenCodeValues())
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the route's file: %v, %v; want it there at 0600", info, err)
	}
	if got := readReference(t, plugin.Options.URL); got != route {
		t.Errorf("the route reads as %q, want %q", got, route)
	}
	written, err := os.ReadFile(filepath.Join(plugin.Package, "index.js")) //nolint:gosec // under the test's box
	if err != nil {
		t.Fatalf("read the plugin: %v", err)
	}
	embedded, err := os.ReadFile("opencode_steer.js")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, embedded) {
		t.Error("the plugin written into the box is not opencode_steer.js")
	}

	// A RUN WIRED TO NO NOTES: no plugin named, none written.
	_, bare := openCodeBox(t, t.TempDir())
	bp := executor.PathsFor(bare)
	path, err = executor.OpenCode{}.WriteConfig(t.Context(), bare, sandbox.RunRequest{MCPServers: seatServers()}, bp)
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	raw, _ = os.ReadFile(path) //nolint:gosec // the run's configuration, under the test's box
	if strings.Contains(string(raw), `"plugins"`) {
		t.Errorf("a run wired to no notes names a plugin:\n%s", raw)
	}
	if _, err := os.Stat(bp.OpenCodeSteerPlugin()); !os.IsNotExist(err) {
		t.Errorf("a run wired to no notes has a plugin written (%v)", err)
	}
}

// homed is a box whose home is not the default, for the homes a reference
// cannot name.
type homed struct {
	*sandbox.FakeSandbox
	home string
}

func (h homed) Home() string { return h.home }

// A HOME A REFERENCE CANNOT NAME IS REFUSED, AND SAYS WHERE IT COMES FROM. A
// reference is found in the configuration's text before it is parsed, so a
// `}` ends it early, a `{` opens one of its own (`{file:…}`, `{env:…}`), a
// character JSON escapes is read as part of the path, and a relative one is
// resolved against the checkout — each a server OpenCode would be handed empty
// values for, a plugin with no route, or an instruction file that is not
// there, with a warning only in its own log. Such a home is refused before
// anything is written, whichever of them the run names, naming the home and the
// setting that places a local box's. A `&`, `<` or `>` is carried as itself, and
// is no reason to refuse.
//
// Mutation: drop the check, or the brace's part of it, and a refused home
// writes a configuration.
func TestAHomeAReferenceCannotNameIsRefused(t *testing.T) {
	t.Parallel()
	for _, home := range []string{"/srv/boxes}/home", "/srv/{file:boxes/home", "/srv/{env:BOXES/home",
		`/srv/"boxes"/home`, `/srv/a\b/home`, "/srv/line\u2028sep/home", "/srv/\x01/home", "srv/relative/home"} {
		// Every run names a path under the home: a server's values, the
		// notes plugin, the instruction file — each alone refused.
		for what, req := range map[string]sandbox.RunRequest{
			"servers":      {MCPServers: seatServers()},
			"notes":        {Steer: steerWiring},
			"instructions": {System: "the seat's standing instructions"},
		} {
			b := homed{FakeSandbox: sandbox.NewFakeSandbox("box-1"), home: home}
			p := executor.PathsFor(b)
			_, err := executor.OpenCode{}.WriteConfig(t.Context(), b, req, p)
			if err == nil || !strings.Contains(err.Error(), "providers.sandbox.local.state_dir") ||
				!strings.Contains(err.Error(), fmt.Sprintf("%q", home)) {
				t.Errorf("home %q, %s: WriteConfig = %v, want it refused naming the home and the setting", home, what, err)
			}
			if written, _ := b.ReadFile(t.Context(), p.OpenCodeConfig()); written != nil {
				t.Errorf("home %q, %s: a refused run wrote its configuration:\n%s", home, what, written)
			}
		}
	}
	b := homed{FakeSandbox: sandbox.NewFakeSandbox("box-1"), home: "/srv/r&d <boxes>/home"}
	if _, err := (executor.OpenCode{}).WriteConfig(t.Context(), b,
		sandbox.RunRequest{MCPServers: seatServers()}, executor.PathsFor(b)); err != nil {
		t.Errorf("a home holding & < > was refused: %v", err)
	}
}

// A REUSED BOX KEEPS NONE OF AN EARLIER RUN'S CONFIGURATION. The clear before
// every run removes the configuration, the whole values directory and the notes
// plugin's, so a run configured with fewer values than the last finds only its
// own — never the last run's beside them — a run wired to no notes keeps no
// plugin, and every file it writes is created afresh.
//
// Mutation: leave the values directory or the plugin's out of the clear and the
// earlier run's files are still there.
func TestAReusedBoxKeepsNoEarlierRunsConfiguration(t *testing.T) {
	t.Parallel()
	runner, box := openCodeBox(t, t.TempDir())
	p := executor.PathsFor(box)
	if _, err := (executor.OpenCode{}).WriteConfig(t.Context(), box,
		sandbox.RunRequest{MCPServers: seatServers(), Steer: steerWiring}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	if err := runner.ClearArtifacts(t.Context(), box); err != nil {
		t.Fatalf("ClearArtifacts: %v", err)
	}
	for _, gone := range []string{p.OpenCodeConfig(), p.OpenCodeValues(), p.OpenCodeSteerPlugin()} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s survived the clear (%v)", gone, err)
		}
	}

	if _, err := (executor.OpenCode{}).WriteConfig(t.Context(), box, sandbox.RunRequest{
		MCPServers: map[string]sandbox.MCPServer{"crewlet": seatServers()["crewlet"]},
	}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	entries, err := os.ReadDir(p.OpenCodeValues())
	if err != nil {
		t.Fatalf("read the values directory: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("a run with one value finds %v in its values directory, want its own alone", names)
	}
}
