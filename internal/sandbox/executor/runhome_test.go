package executor_test

import (
	"context"
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

// homeCase is what a real box's home holds before a run of one harness, and
// what the run's harness must find there as it starts: each file left there
// gone, each kept file whole. Paths are the home's; `workspace/` is the box's
// working directory, which a checkout is cloned below.
type homeCase struct {
	left []string
	kept []string
}

// observedRun records, from a stand-in harness, its argv, its environment and
// its stdin in the box home, then answers as a finished run.
const observedRun = `printf '%s\n' "$@" > "$HOME/observed.argv"
env > "$HOME/observed.env"
cat > "$HOME/observed.brief"
echo '{"type":"result","status":"success"}'
`

// fillHome writes every left and kept file of c under box's home, and an
// `.env` in the directory above the home, which no run may touch.
func fillHome(t *testing.T, box sandbox.Sandbox, c homeCase) (above string) {
	t.Helper()
	for _, rel := range slices.Concat(c.left, c.kept) {
		path := filepath.Join(box.Home(), rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("written before the run\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	above = filepath.Join(filepath.Dir(box.Home()), ".env")
	if err := os.WriteFile(above, []byte("ABOVE=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return above
}

// runObserved starts req in box and waits for its end.
func runObserved(t *testing.T, runner *executor.Runner, box sandbox.Sandbox, req sandbox.RunRequest) {
	t.Helper()
	handle, err := runner.Start(t.Context(), box, req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	awaitOver(t, runner, box, handle, time.Minute, "a stand-in that records and answers takes a second")
}

// checkHome holds box's home to c after a run: one case per file, and the
// directory above the home untouched.
func checkHome(t *testing.T, box sandbox.Sandbox, c homeCase, above string) {
	t.Helper()
	for _, rel := range c.left {
		t.Run("left "+rel, func(t *testing.T) {
			if _, err := os.Stat(filepath.Join(box.Home(), rel)); !os.IsNotExist(err) {
				t.Errorf("%s was still there when the run started (%v): the run would load it", rel, err)
			}
		})
	}
	for _, rel := range c.kept {
		t.Run("kept "+rel, func(t *testing.T) {
			if _, err := os.Stat(filepath.Join(box.Home(), rel)); err != nil {
				t.Errorf("%s, the box's and not configuration the run loads, is gone: %v", rel, err)
			}
		})
	}
	if _, err := os.Stat(above); err != nil {
		t.Errorf("the run removed %s, above the box's home: %v", above, err)
	}
}

// observedEnv is the stand-in harness's environment, by name.
func observedEnv(t *testing.T, box sandbox.Sandbox) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, line := range strings.Split(boxFile(t, box, box.Home()+"/observed.env"), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			env[name] = value
		}
	}
	return env
}

// A GEMINI RUN LOADS ONLY WHAT ITS LAUNCH WRITES. A real box whose home an
// earlier run and its image filled with everything Gemini CLI 0.70.0 was
// measured loading (skills, policies, commands, memory and environment files,
// a working directory's own settings): as `gemini` starts, each is gone; the
// login, an extension, the session store and a checkout's GEMINI.md are whole;
// nothing above the home is touched; the working directory holds the run's own
// environment file; no extension loads; every variable that moves a request is
// set, the prompt the script's; and a brief beginning with `/` is words.
//
// Mutations, each red: drop an entry from geminiHome; drop the seed; walk
// above the home; drop `--extensions none`; drop a pin; drop readsCommands.
func TestAGeminiRunLoadsOnlyWhatItsLaunchWrites(t *testing.T) {
	t.Parallel()
	c := homeCase{
		left: []string{".gemini/skills/kept/SKILL.md", ".gemini/policies/kept.toml", ".gemini/commands/kept.toml",
			".gemini/GEMINI.md", ".gemini/.env", ".env", "GEMINI.md", "workspace/GEMINI.md", "workspace/.env",
			"workspace/.gemini/settings.json", "workspace/.gemini/skills/kept/SKILL.md",
			"workspace/.gemini/commands/kept.toml"},
		kept: []string{".gemini/oauth_creds.json", ".gemini/extensions/kept/gemini-extension.json",
			".gemini/tmp/crewlet/chats/session.json", "workspace/checkout/GEMINI.md"},
	}
	runner, box := geminiBox(t, observedRun)
	above := fillHome(t, box, c)
	runObserved(t, runner, box, sandbox.RunRequest{Brief: " /init the repository", Model: "gemini-2.5-pro"})
	checkHome(t, box, c, above)

	if seed := boxFile(t, box, box.Home()+"/workspace/.gemini/.env"); !strings.HasPrefix(seed, "# ") {
		t.Errorf("the working directory's .gemini/.env is %q, want the run's own, a comment alone", seed)
	}
	if argv := boxFile(t, box, box.Home()+"/observed.argv"); !strings.Contains(argv, "--extensions\nnone\n") {
		t.Errorf("gemini started with\n%s\nwant --extensions none", argv)
	}
	env := observedEnv(t, box)
	for name, want := range map[string]string{
		"GEMINI_SYSTEM_MD": executor.PathsFor(box).GeminiSystem(), "GEMINI_WRITE_SYSTEM_MD": "false",
		"CODE_ASSIST_ENDPOINT": "https://cloudcode-pa.googleapis.com", "GOOGLE_GEMINI_BASE_URL": "",
		"GOOGLE_VERTEX_BASE_URL": "",
	} {
		if got, ok := env[name]; !ok || got != want {
			t.Errorf("gemini started with %s=%q (set: %t), want %q set: an env file fills what is unset",
				name, got, ok, want)
		}
	}
	if brief := boxFile(t, box, box.Home()+"/observed.brief"); brief != "init the repository" {
		t.Errorf("gemini was handed %q, want the brief's words, no command", brief)
	}
}

// A QWEN RUN LOADS ONLY WHAT ITS LAUNCH WRITES. The same box for Qwen Code
// 0.28.4's measured places (skills, commands, rules, memory files at the home
// and the working directory, environment files, the working directory's own
// settings), its helper a stand-in: as the helper starts, each is gone, the
// login, an installed extension, the session store and a checkout's own
// QWEN.md are whole, nothing above the home is touched, the working directory
// holds the run's own environment file, the key's endpoint and the prompt's
// variables are set, and a brief beginning with `/` arrives as words.
//
// Mutations, each red: drop an entry from qwenHome; drop the seed; drop a pin;
// drop readsCommands.
func TestAQwenRunLoadsOnlyWhatItsLaunchWrites(t *testing.T) {
	t.Parallel()
	c := homeCase{
		left: []string{".qwen/skills/kept/SKILL.md", ".qwen/commands/kept.md", ".qwen/rules/kept.md",
			".qwen/QWEN.md", ".qwen/AGENTS.md", ".qwen/.env", ".env", "QWEN.md", "AGENTS.md", "workspace/QWEN.md",
			"workspace/AGENTS.md", "workspace/.env", "workspace/.qwen/settings.json", "workspace/.qwen/rules/kept.md"},
		kept: []string{".qwen/subscription_credentials.json", ".qwen/extensions/kept/qwen-extension.json",
			".qwen/projects/crewlet/chats/session.jsonl", "workspace/checkout/QWEN.md"},
	}
	runner, box := qwenBox(t, observedRun)
	above := fillHome(t, box, c)
	runObserved(t, runner, box, sandbox.RunRequest{Brief: "/init the repository", Model: "qwen3-coder-plus",
		Env: map[string]string{"OPENAI_API_KEY": "key-placeholder"}})
	checkHome(t, box, c, above)

	if seed := boxFile(t, box, box.Home()+"/workspace/.qwen/.env"); !strings.HasPrefix(seed, "# ") {
		t.Errorf("the working directory's .qwen/.env is %q, want the run's own, a comment alone", seed)
	}
	env := observedEnv(t, box)
	for name, want := range map[string]string{"QWEN_SYSTEM_MD": "false", "QWEN_WRITE_SYSTEM_MD": "false",
		"OPENAI_BASE_URL": "https://dashscope.aliyuncs.com/compatible-mode/v1"} {
		if got, ok := env[name]; !ok || got != want {
			t.Errorf("the helper started with %s=%q (set: %t), want %q set: an env file fills what is unset",
				name, got, ok, want)
		}
	}
	if brief := boxFile(t, box, box.Home()+"/observed.brief"); brief != "init the repository" {
		t.Errorf("the helper was handed %q, want the brief's words, no command", brief)
	}
}

// A GROK RUN LOADS WHAT A PERSON'S GROK LOADS. A real box whose home holds what
// Grok Build 1.0.92 loads from a home that is its working directory: as `grok`
// starts, every such file is whole, logins and sessions too, and only a skill
// an earlier run wrote (`crewlet:*`) is gone; nothing above the home is
// touched; each compatibility switch reaches it as set, and only the variables
// that move what the runner writes and reads, or who decides a call, do not;
// the box's settings stand beside the run's server, on a second run too, and
// alone once Grok exits; and a brief beginning with `/` is words.
//
// Mutations, each red: remove the home's skills whole; keep an earlier run's
// skill; unset a compatibility switch; drop a name from grokSwitchesOff; write
// the run's settings alone or into the last run's; write the brief as it came.
func TestAGrokRunLoadsWhatAPersonsGrokLoads(t *testing.T) {
	t.Parallel()
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
// profile or the entry sets it, and the engine's own OTLP exporter variables,
// which read no span of Grok's, do not.
//
// Mutations, each red: unset a routing variable the entry does not set; drop
// the script's OTLP unset; drop a variable from the runner's declaration.
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
	if value, ok := env["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"]; ok {
		t.Errorf("grok started with OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=%q, the engine's own export", value)
	}
}

// pinsOf is h's pins for req, by name.
func pinsOf(t *testing.T, h interface {
	Pins(ctx context.Context, req sandbox.RunRequest, paths executor.Paths) []executor.Pinned
}, req sandbox.RunRequest) map[string]string {
	t.Helper()
	pins := map[string]string{}
	for _, pin := range h.Pins(t.Context(), req, executor.Paths{Home: "/home/box"}) {
		pins[pin.Name] = pin.Value
	}
	return pins
}

// A GEMINI RUN'S REQUESTS GO WHERE ITS ENTRY SAYS, OR TO GEMINI CLI'S OWN
// ENDPOINTS, never where an env file says: a login's Code Assist endpoint, a
// key's base URL and Vertex AI's are pinned to the entry's value or, for an
// entry that names none, to Gemini CLI's own (the Code Assist URL named, the
// base URLs empty, as Gemini CLI reads an unset one); and the prompt
// variables are `false`, so no file replaces the prompt the script builds.
//
// Mutations, each red: pin only what the entry names; pin the Code Assist
// endpoint empty; leave a prompt variable unpinned.
func TestAGeminiRunsRequestsGoWhereItsEntrySays(t *testing.T) {
	t.Parallel()
	gateway := "https://gateway.example.com"
	for name, tc := range map[string]struct {
		env  map[string]string
		want map[string]string
	}{
		"a login naming no endpoint": {map[string]string{"GOOGLE_GENAI_USE_GCA": "true"},
			map[string]string{"CODE_ASSIST_ENDPOINT": "https://cloudcode-pa.googleapis.com"}},
		"a login naming one": {map[string]string{"CODE_ASSIST_ENDPOINT": gateway},
			map[string]string{"CODE_ASSIST_ENDPOINT": gateway}},
		"a key naming no base URL": {map[string]string{"GEMINI_API_KEY": "key-placeholder"},
			map[string]string{"GOOGLE_GEMINI_BASE_URL": ""}},
		"a key behind a gateway": {map[string]string{"GEMINI_API_KEY": "key-placeholder", "GOOGLE_GEMINI_BASE_URL": gateway},
			map[string]string{"GOOGLE_GEMINI_BASE_URL": gateway}},
		"vertex naming no base URL": {map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "true"},
			map[string]string{"GOOGLE_VERTEX_BASE_URL": ""}},
		"vertex behind a gateway": {map[string]string{"GOOGLE_VERTEX_BASE_URL": gateway},
			map[string]string{"GOOGLE_VERTEX_BASE_URL": gateway}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pins := pinsOf(t, executor.GeminiCLI{}, sandbox.RunRequest{Env: tc.env})
			want := map[string]string{"GEMINI_SYSTEM_MD": "false", "GEMINI_WRITE_SYSTEM_MD": "false"}
			for k, v := range tc.want {
				want[k] = v
			}
			for k, v := range want {
				if got, ok := pins[k]; !ok || got != v {
					t.Errorf("%s pinned %q (pinned: %t), want %q", k, got, ok, v)
				}
			}
		})
	}
	if unset := (executor.GeminiCLI{}).Output(executor.Paths{Home: "/home/box"}).Unset; slices.Contains(unset, "GEMINI_SYSTEM_MD") {
		t.Errorf("GEMINI_SYSTEM_MD is unset (%v), which leaves it for an env file to fill", unset)
	}
}

// A QWEN RUN'S KEY GOES WHERE ITS ENTRY SAYS, OR TO QWEN CODE'S OWN ENDPOINT,
// never where an env file says, and no file replaces its prompt.
//
// Mutations, each red: pin OPENAI_BASE_URL only where the entry names it;
// leave QWEN_SYSTEM_MD unset rather than pinned.
func TestAQwenRunsKeyGoesWhereItsEntrySays(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"naming no endpoint": {map[string]string{"OPENAI_API_KEY": "key-placeholder"},
			"https://dashscope.aliyuncs.com/compatible-mode/v1"},
		"naming one": {map[string]string{"OPENAI_API_KEY": "key-placeholder", "OPENAI_BASE_URL": "https://example.com/v1"},
			"https://example.com/v1"},
		"a subscription login": {nil, "https://dashscope.aliyuncs.com/compatible-mode/v1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pins := pinsOf(t, executor.QwenCode{}, sandbox.RunRequest{Env: tc.env})
			if pins["OPENAI_BASE_URL"] != tc.want || pins["QWEN_SYSTEM_MD"] != "false" ||
				pins["QWEN_WRITE_SYSTEM_MD"] != "false" {
				t.Errorf("the run pins %v, want OPENAI_BASE_URL %q and the prompt variables false", pins, tc.want)
			}
		})
	}
}
