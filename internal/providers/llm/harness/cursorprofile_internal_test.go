package harness

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// The invariants the built-in cursor-agent profile keeps, measured against
// Cursor Agent 2026.11.22-e9042b2 (see the profile's comments): a call runs in a directory it has never seen without
// stopping for trust, asks the service to leave workspace context out, keeps
// the engine's text in the transcript, and leaves nothing in the seat's home
// that outlives the calls that share it.

// A LOGOUT REVOKES THE LOGIN AT CURSOR: its CLI has `logout` ("Sign out and
// clear stored authentication", its --help), so `crewlet llm logout` runs it
// rather than only removing the local files.
//
// Mutation: drop the cursor-agent profile's logout_args, and this fails.
func TestCursorAgentLogsOutWithItsOwnCommand(t *testing.T) {
	t.Parallel()
	if got := cursorAgent(t).LogoutArgs; !slices.Equal(got, []string{"logout"}) {
		t.Errorf("logout_args = %q, want [logout]", got)
	}
}

// cursorAgent is the shipped profile, which every case here reads.
func cursorAgent(t *testing.T) harnessprofile.Profile {
	t.Helper()
	p, ok := harnessprofile.Builtin("cursor-agent")
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
// taken out (measured on 2026.11.22-e9042b2: 100 sent, 40 of them cached,
// reported 60 and 40).
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
				t.Fatalf("route = %+v, want the transcript under the harness's own prompt", route)
			}
			dir := t.TempDir()
			t.Cleanup(func() { forgetWorkspace(dir) })
			p, err := New(Config{
				Key: "cursor", Model: "gpt-5", Harness: "cursor-agent", StateDir: dir,
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
// call's transcript and its servers' approvals, a chat directory per call,
// and a cache of the sandbox's walk under .cursor/sandbox-policies — one more
// entry for every call the seat makes, measured on 2026.11.22-e9042b2.
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
		project + "/mcp-approvals.json",
		".cursor/sandbox-policies/walk-cache/2dd8b872885de1acaa56777ff4385a3a3f9e15ce34ded1e6b6e251f929310f65.walk",
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

// A SIGNED-OUT HARNESS NAMES ITS LOGIN AS WHAT FAILED, in the words every variant of
// its message opens with in both builds — measured on 2026.10.22 — so the seat
// says its login is what failed rather than reporting a server error.
func TestCursorAgentNamesALoginItCannotUse(t *testing.T) {
	t.Parallel()
	const stderr = "Error: Authentication required. Please run 'agent login' first, " +
		"or set CURSOR_API_KEY environment variable.\n"
	hit, ok := classifyMarkers(cursorAgent(t), harnessOutput{stderr: stderr, failed: true})
	if !ok || hit.Kind != llm.KindAuth {
		t.Errorf("classified %+v (matched %v), want the login named for:\n%s", hit, ok, stderr)
	}
}

// A CURSOR AGENT CALL WITH INSTRUCTIONS ABOVE IT IS NEVER RUN: Cursor reads
// every AGENTS.md, CLAUDE.md, CLAUDE.local.md and .cursor/rules from its
// call's directory up to the root into the model's request, and nothing in the
// call's directory stops that walk (measured on 2026.11.22-e9042b2). So a seat
// whose state directory has one of them above it is refused its call, naming
// the file, and one with none runs.
//
// Mutation: drop the profile's ancestor_files, or stop the walk below the
// root: a row goes red.
func TestACursorAgentCallWithInstructionsAboveItIsNeverRun(t *testing.T) {
	t.Parallel()
	p := cursorAgent(t)
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", "CLAUDE.local.md", ".cursor/rules"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			above := t.TempDir()
			planted := filepath.Join(above, name)
			if err := os.MkdirAll(filepath.Dir(planted), 0o700); err != nil {
				t.Fatal(err)
			}
			if name == ".cursor/rules" {
				if err := os.Mkdir(planted, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(planted, []byte("INSTRUCTIONS FROM ABOVE\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(above, "deep", "state")
			t.Cleanup(func() { forgetWorkspace(state) })
			ws, err := Shared(state, "cursor-agent", p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ws.Acquire(t.Context(), "agent-pm", "call-1", nil); err == nil ||
				!strings.Contains(err.Error(), planted) {
				t.Fatalf("a call with %s above it ran (%v), want it refused naming the file", planted, err)
			}
		})
	}
	state := filepath.Join(t.TempDir(), "state")
	t.Cleanup(func() { forgetWorkspace(state) })
	ws, err := Shared(state, "cursor-agent", p)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ws.Acquire(t.Context(), "agent-pm", "call-1", nil)
	if err != nil {
		t.Fatalf("a call with nothing above it was refused: %v", err)
	}
	if err := c.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// THE DOCTOR REPORTS INSTRUCTIONS ABOVE A CURSOR ENTRY'S STATE DIRECTORY, with
// or without its smoke test, in the words each refused call uses, naming the
// file, as a configuration problem; an entry with none above it raises none.
//
// Mutation: skip the doctor's check, or report it only when the smoke test
// runs: a row goes red.
func TestTheDoctorReportsInstructionsAboveACursorEntry(t *testing.T) {
	t.Parallel()
	for _, smoke := range []bool{false, true} {
		above := t.TempDir()
		planted := filepath.Join(above, "CLAUDE.md")
		if err := os.WriteFile(planted, []byte("INSTRUCTIONS FROM ABOVE\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		state := filepath.Join(above, "state")
		t.Cleanup(func() { forgetWorkspace(state) })
		p, err := New(Config{Key: "cursor", Harness: "cursor-agent", Model: "gpt-5", StateDir: state,
			Timeout: time.Second, MaxConcurrent: 1,
			Overrides: map[string]any{"binary": filepath.Join(t.TempDir(), "no-cursor-here")}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		d := p.Diagnose(t.Context(), DiagnoseOptions{Smoke: smoke})
		check, _ := d.Check(CheckSmoke)
		if check.OK || check.Kind != CheckKindConfig || !strings.Contains(check.Remedy, planted) {
			t.Errorf("smoke %v: the doctor's smoke line is %+v, want a configuration problem naming %s",
				smoke, check, planted)
		}
	}
	state := filepath.Join(t.TempDir(), "state")
	t.Cleanup(func() { forgetWorkspace(state) })
	p, err := New(Config{Key: "cursor", Harness: "cursor-agent", Model: "gpt-5", StateDir: state,
		Timeout: time.Second, MaxConcurrent: 1,
		Overrides: map[string]any{"binary": filepath.Join(t.TempDir(), "no-cursor-here")}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.AncestorRefusal(); err != nil {
		t.Errorf("an entry with nothing above it was refused: %v", err)
	}
}
