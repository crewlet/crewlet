package harness

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// The invariants the built-in kimi-code profile keeps, each measured against
// Kimi Code 0.42.0 and 2.3.1 through this package's own Complete (see the
// profile's comments): every call hands the CLI an agent file it accepts, a
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

// fakeChannel is a shipped channel as an override of the fake CLI's profile:
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

// EVERY CALL HANDS THE CLI AN AGENT FILE WITH A BODY, and a seat's text opens it.
//
// Kimi Code trims the body and refuses one that trims to nothing ("Missing
// prompt body", exit 1, on both builds measured) — so a call whose file came
// out empty fails whatever it asked. The replacing body is the engine's text
// alone and is empty for a call with no system prompt, which therefore has to
// take the appending one: driven through Complete with the shipped channels,
// for each kind of call with and without a system prompt.
//
// And where there is text, NOTHING comes before it: the vendor's default
// carries the call's directory, its listing and the AGENTS.md chain, so it
// follows the engine's text rather than leading it.
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
		// What the CLI reads: the body, trimmed.
		body = strings.TrimSpace(body)
		if body == "" {
			t.Errorf("%s: the agent file's body is empty, which the CLI refuses: %q", name, file)
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

// NOTHING A CALL WRITES ABOUT ITSELF OUTLIVES IT. Measured on both builds,
// every call left in the seat's home its session (whose wire log carries the
// prompts and tool results), an entry in the session index, its logs, an entry
// in `workspaces.json` naming its directory — never removed by the CLI, so the
// registry grew by one per call — and, with the vendor's telemetry on, a
// `telemetry` queue of events kept for the next start to send. Telemetry is
// off, so the queue is never made, and every one of them is pruned.
func TestKimiCodeKeepsNothingOfACallInItsHome(t *testing.T) {
	t.Parallel()
	p := kimiCode(t)
	for _, want := range []string{
		".kimi-code/sessions",
		".kimi-code/session_index.jsonl",
		".kimi-code/logs",
		".kimi-code/workspaces.json",
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
