package harness

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// The invariants the built-in kimi-code profile keeps, each measured against
// Kimi Code through this package's own code path (see the profile's comments,
// which say on which release): every call hands the harness an agent file it accepts, a
// seat's text comes before anything per call, and nothing a call writes about
// itself outlives the calls that share the seat's home.

// kimiCode is the shipped profile, which every case here reads.
func kimiCode(t *testing.T) harnessprofile.Profile {
	t.Helper()
	p, ok := harnessprofile.Builtin("kimi-code")
	if !ok {
		t.Fatal("no built-in kimi-code profile")
	}
	return p
}

// fakeChannel is a shipped channel as an override of the fake harness's profile:
// the same argv and the same file.
func fakeChannel(ch harnessprofile.Channel) map[string]any {
	args := []any{}
	for _, a := range ch.Args {
		args = append(args, a)
	}
	return map[string]any{
		"args": args,
		"file": map[string]any{"name": ch.File.Name, "template": ch.File.Template},
	}
}

// EVERY CALL HANDS THE HARNESS AN AGENT FILE WITH A BODY, and a seat's text
// opens it. Kimi Code refuses a body that trims to nothing ("Missing prompt
// body", exit 1, measured on 2.7.3). The replacing body is the engine's text
// alone, empty for a call with no system prompt, which therefore takes the
// appending one: driven through Complete with the shipped channels, for each
// kind of call with and without a system prompt. Where there is text, NOTHING
// comes before it: the vendor's default carries the call's directory, its
// listing and the AGENTS.md chain, so it follows the engine's text.
//
// Mutation: send a call with no system prompt down its own kind's route in
// routeFor, and a side pass with none hands the harness an empty body.
func TestKimiCodeNeverHandsItsHarnessAnAgentFileWithNoBody(t *testing.T) {
	t.Parallel()
	kimi := kimiCode(t)
	p := fakeProvider(t, map[string]string{"FAKE_PROMPT_FILE": "--agent-file"}, map[string]any{
		"system_prompt":        fakeChannel(kimi.SystemPrompt),
		"append_system_prompt": fakeChannel(kimi.AppendSystemPrompt),
	})
	const seat, side = "You are Agent PM at Nimbus.", "Classify the message."
	for name, tc := range map[string]struct {
		agent  bool
		system string
		opens  string // what the body must begin with
		vendor bool   // whether the vendor's default follows
	}{
		"a seat with its prompt":      {agent: true, system: seat, opens: harnessNote, vendor: true},
		"a seat with none":            {agent: true, opens: "${base_prompt}", vendor: true},
		"a side pass with its prompt": {agent: false, system: side, opens: side},
		"a side pass with none":       {agent: false, opens: "${base_prompt}", vendor: true},
	} {
		msgs := []llm.Message{{Role: llm.RoleUser, Content: "the build is red"}}
		if tc.system != "" {
			msgs = append([]llm.Message{{Role: llm.RoleSystem, Content: tc.system}}, msgs...)
		}
		got, err := ask(t, p, llm.Request{Agent: tc.agent, Messages: msgs})
		if err != nil {
			t.Fatalf("%s: Complete: %v", name, err)
		}
		_, _, file := splitFileProbe(t, got.Content)
		_, body, found := strings.Cut(strings.TrimPrefix(file, "---\n"), "---\n")
		if !found {
			t.Fatalf("%s: the agent file has no frontmatter:\n%s", name, file)
		}
		// What the harness reads: the body, trimmed.
		body = strings.TrimSpace(body)
		if body == "" {
			t.Errorf("%s: the agent file's body is empty, which the harness refuses: %q", name, file)
			continue
		}
		if !strings.HasPrefix(body, tc.opens) {
			t.Errorf("%s: the body opens %.80q, want it to open with %.80q", name, body, tc.opens)
		}
		if got := strings.HasSuffix(body, "${base_prompt}"); got != tc.vendor {
			t.Errorf("%s: the vendor's default follows = %v, want %v:\n%s", name, got, tc.vendor, body)
		}
		if tc.system != "" && !strings.Contains(body, tc.system) {
			t.Errorf("%s: the body does not carry the call's system prompt:\n%s", name, body)
		}
	}
}

// NOTHING A CALL WRITES ABOUT ITSELF OUTLIVES IT. Measured on 2.7.3, every
// call left in the seat's home its session (whose wire log carries the prompts
// and tool results), an entry in the session index, its logs, an entry in
// `projects.json` naming its directory (never removed by the harness, so the
// registry grew by one per call) and the registry's lock file, and, with the
// vendor's telemetry on, a `telemetry` directory for the events kept for the
// next start. Telemetry is off, so it is never made, and every one is pruned.
//
// Mutation: drop projects.json from volatile_paths, and every call's
// directory stays named in the seat's home.
func TestKimiCodeKeepsNothingOfACallInItsHome(t *testing.T) {
	t.Parallel()
	p := kimiCode(t)
	for _, want := range []string{
		".kimi-code/sessions",
		".kimi-code/session_index.jsonl",
		".kimi-code/logs",
		".kimi-code/projects.json",
		".kimi-code/.projects-write-lock",
		".kimi-code/telemetry",
	} {
		if !slices.Contains(p.VolatilePaths, want) {
			t.Errorf("volatile_paths = %q lacks %q", p.VolatilePaths, want)
		}
	}
	env := childEnv(p, &Checkout{Home: t.TempDir(), Cache: t.TempDir(), Work: t.TempDir()}, nil, Auth{})
	if got := env["KIMI_DISABLE_TELEMETRY"]; got != "1" {
		t.Errorf("KIMI_DISABLE_TELEMETRY = %q, want 1: the vendor's telemetry is a "+
			"connection that is not the call's own", got)
	}
}

// A KIMI CODE CALL IS TOLD NOTHING FROM ITS HOME'S OWN CONFIG: 2.7.3 reads the
// skills under `.kimi-code/skills` and `.agents/skills`, the personal AGENTS.md
// in the home and under `.agents`, an installed plugin and a saved workspace
// trust with this profile's flags (measured), so every one goes with the
// prune; the login stays.
//
// Mutation: drop any of them from volatile_paths, and it outlives the prune.
func TestAKimiCodeCallIsToldNothingFromItsHomesConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	ws, err := Shared(dir, "kimi-code", kimiCode(t))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	left := []string{
		".kimi-code/skills/planted/SKILL.md",
		".kimi-code/AGENTS.md",
		".kimi-code/plugins/planted/kimi.plugin.json",
		".kimi-code/workspace-trust/wd_ws_0123456789ab",
		".agents/skills/planted/SKILL.md",
		".agents/AGENTS.md",
		".agents/agents.md",
	}
	kept := []string{".kimi-code/credentials/kimi-code.json", ".kimi-code/config.toml"}
	for _, rel := range append(slices.Clone(left), kept...) {
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := ws.prune(home); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for _, rel := range left {
		if _, err := os.Stat(filepath.Join(home, rel)); !os.IsNotExist(err) {
			t.Errorf("%s outlived the prune (%v): the seat's next call would be told it", rel, err)
		}
	}
	for _, rel := range kept {
		if _, err := os.Stat(filepath.Join(home, rel)); err != nil {
			t.Errorf("%s went with the prune: %v", rel, err)
		}
	}
}

// A SEAT HOME IS SEEDED WITH KIMI CODE'S LOGIN AND NOTHING ELSE OF ITS
// config.toml: a [[hooks]] table, a plugin entry and a tool setting the shared
// login came to hold beside the login reach no seat's home, since the harness
// runs a hook it reads there; the token file and the login's tables do.
//
// Mutation: seed config.toml as the shared login holds it, or drop a login
// table from the profile's login_keys.
func TestAKimiCodeSeatHomeIsSeededWithTheLoginAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	ws, err := Shared(dir, "kimi-code", kimiCode(t))
	if err != nil {
		t.Fatal(err)
	}
	login := "default_model = \"managed:kimi-code/k\"\n\n[providers.\"managed:kimi-code\"]\ntype = \"kimi\"\n"
	shared := map[string]string{
		"config.toml": login + "\n[[hooks]]\nevent = \"SessionStart\"\ncommand = \"touch /tmp/planted\"\n" +
			"\n[plugins.planted]\nenabled = true\n\n[tools]\ndisabled = [\"Bash\"]\n",
		"kimi-code.json": `{"access_token":"t"}`,
	}
	if err := os.MkdirAll(ws.CredentialsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range shared {
		if err := os.WriteFile(filepath.Join(ws.CredentialsDir(), name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	co, err := ws.Acquire(t.Context(), "pm", "call-1", nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = co.Release(context.WithoutCancel(t.Context())) })
	config, err := os.ReadFile(filepath.Join(co.Home, ".kimi-code", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"hooks", "touch /tmp/planted", "plugins", "tools"} {
		if strings.Contains(string(config), gone) {
			t.Errorf("the seat home is seeded with %q in %q", gone, config)
		}
	}
	if !strings.Contains(string(config), "managed:kimi-code") {
		t.Errorf("the seat home is seeded with %q, want the login's tables", config)
	}
	token, err := os.ReadFile(filepath.Join(co.Home, ".kimi-code", "credentials", "kimi-code.json"))
	if err != nil || string(token) != shared["kimi-code.json"] {
		t.Errorf("the seat home's token file holds %q (%v)", token, err)
	}
}
