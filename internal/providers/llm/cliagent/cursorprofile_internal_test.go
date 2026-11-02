package cliagent

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// The invariants the built-in cursor-agent profile keeps, measured against
// Cursor Agent 2026.10.22 and read from the 2026.09.10 bundle (see the
// profile's comments): a call runs in a directory it has never seen without
// stopping for trust, asks the service to leave workspace context out, keeps
// the engine's text in the transcript, and leaves nothing in the seat's home
// that outlives the calls that share it.

// cursorAgent is the shipped profile, which every case here reads.
func cursorAgent(t *testing.T) cliprofile.Profile {
	t.Helper()
	p, ok := cliprofile.Builtin("cursor-agent")
	if !ok {
		t.Fatal("no built-in cursor-agent profile")
	}
	return p
}

// A CALL RUNS IN ITS NEW DIRECTORY WITHOUT STOPPING FOR TRUST, and asks the
// service to strip workspace-sourced context. Every call's directory is new,
// and print mode refuses an untrusted one: without --trust every call exited 1
// on "Workspace Trust Required". Nothing that widens what a call may do comes
// with it: --force and --yolo run every command, --approve-mcps starts every
// MCP server, and --disable-project-configs would drop the seeded permission
// file that denies the shell.
func TestCursorAgentRunsUnpromptedWithoutWorkspaceContext(t *testing.T) {
	t.Parallel()
	p := cursorAgent(t)
	for _, want := range []string{"-p", "--trust", "--exclude-workspace-context"} {
		if !slices.Contains(p.CompleteArgs, want) {
			t.Errorf("complete_args = %q, want %s", p.CompleteArgs, want)
		}
	}
	for _, never := range []string{"-f", "--force", "--yolo", "--approve-mcps", "--disable-project-configs"} {
		if slices.Contains(p.CompleteArgs, never) {
			t.Errorf("complete_args = %q carries %s", p.CompleteArgs, never)
		}
	}
}

// A CURSOR AGENT RUN IS CHARGED THE COUNTS ITS RESULT EVENT CARRIES, under the
// names it gives them: `usage` in camelCase, the input with the cached parts
// taken out, as the 2026.10.22 bundle writes it (not measured: no call through
// a stand-in gets an answer).
//
// Mutation: read the counts under snake_case names, as the profile once did,
// and the run is estimated; read the input as the whole prompt, and its cached
// part is not charged.
func TestCursorAgentIsChargedTheCountsItsResultCarries(t *testing.T) {
	t.Parallel()
	p := cursorAgent(t)
	got := extract(p, `{"type":"result","subtype":"success","is_error":false,"result":"done",`+
		`"usage":{"inputTokens":60,"outputTokens":12,"cacheReadTokens":30,"cacheWriteTokens":10}}`)
	if !got.reported {
		t.Fatal("the result's counts were not read, so the run would be charged an estimate")
	}
	if input := p.Usage.PromptTokens(got.input, got.cacheRead, got.cacheWrite); input != 100 ||
		got.output != 12 || got.cacheRead != 30 || got.cacheWrite != 10 {
		t.Errorf("charged %d in (%d read, %d written) and %d out, want 100 in (30, 10) and 12 out",
			input, got.cacheRead, got.cacheWrite, got.output)
	}
}

// THE ENGINE'S TEXT RIDES THE TRANSCRIPT, under the note that says which of the
// two prompts is the role — for a seat and a side pass alike. The service
// assembles the system prompt; the run request carries the user message and no
// system prompt, and a file in the call's directory is the workspace context
// the call asks the service to leave out. Asked from inside the child, which
// echoes the stdin it was handed.
func TestCursorAgentCarriesTheSystemPromptInTheTranscript(t *testing.T) {
	t.Parallel()
	const identity = "You are Agent PM at Nimbus."
	for name, agent := range map[string]bool{"a seat at work": true, "a side pass": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			profile := cursorAgent(t)
			route := profile.RouteFor(agent)
			if route.Channel.Declared() || !route.KeepsDefault {
				t.Fatalf("route = %+v, want the transcript under the CLI's own prompt", route)
			}
			dir := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(dir) })
			p, err := New(Config{
				Key: "cursor", Model: "gpt-5", Agent: "cursor-agent", StateDir: dir,
				Overrides: map[string]any{"binary": os.Args[0], "output": "text"},
				Timeout:   20 * time.Second, MaxConcurrent: 1,
				Env:  fakeChildEnv(map[string]string{"FAKE_ECHO_STDIN": "1"}),
				Auth: Auth{Mode: AuthSubscription},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			comp, err := ask(t, p, llm.Request{Agent: agent, Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: identity},
				{Role: llm.RoleUser, Content: "status?"},
			}})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			stdin, err := base64.StdEncoding.DecodeString(strings.TrimSpace(comp.Content))
			if err != nil {
				t.Fatalf("the child echoed %q: %v", comp.Content, err)
			}
			if want := "## system\n\n" + harnessNote + "\n\n" + identity; !strings.Contains(string(stdin), want) {
				t.Errorf("stdin = %q, want the system section %q", stdin, want)
			}
		})
	}
}

// NOTHING A CALL LEAVES IN THE SEAT'S HOME OUTLIVES ITS GENERATION. --trust
// writes its marker per call directory under .cursor/projects, beside that
// call's transcript and a worker log, and a chat directory per call — one more
// entry for every call the seat makes, all measured on 2026.10.22.
func TestCursorAgentPrunesWhatACallLeaves(t *testing.T) {
	t.Parallel()
	p := cursorAgent(t)
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	ws, err := Shared(dir, "cursor-agent", p)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	const project = ".cursor/projects/state-cursor-agent-seats-agent-pm-work-call-1"
	left := []string{
		project + "/.workspace-trusted",
		project + "/worker.log",
		project + "/agent-transcripts/893411a1-6f96-49e0-811b-89d238f575a7/893411a1-6f96-49e0-811b-89d238f575a7.jsonl",
		".cursor/chats/258ab4dea5f1/893411a1-6f96-49e0-811b-89d238f575a7/meta.json",
	}
	for _, rel := range left {
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
			t.Errorf("%s outlived the prune (%v): the next call of the seat would find it", rel, err)
		}
	}
}

// A SIGNED-OUT CLI NAMES ITS LOGIN AS WHAT FAILED, in the words every variant of
// its message opens with in both builds — measured on 2026.10.22 — so the seat
// says its login is what failed rather than reporting a server error.
func TestCursorAgentNamesALoginItCannotUse(t *testing.T) {
	t.Parallel()
	const stderr = "Error: Authentication required. Please run 'agent login' first, " +
		"or set CURSOR_API_KEY environment variable.\n"
	hit, ok := classifyMarkers(cursorAgent(t), cliOutput{stderr: stderr, failed: true})
	if !ok || hit.Kind != llm.KindAuth {
		t.Errorf("classified %+v (matched %v), want the login named for:\n%s", hit, ok, stderr)
	}
}
