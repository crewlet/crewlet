package executor_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// seatSystem stands in for an executor run's system prompt: who the
// seat is, which no prompt the harness runs may carry.
const seatSystem = "You are Agent SWE at Acme. SEAT-IDENTITY-MARKER"

// systemOfRun is the system text a started run left in its box, and the
// prompt its harness was given.
func systemOfRun(t *testing.T, runner *executor.Runner, req sandbox.RunRequest) (b *sandbox.FakeSandbox, system, script string) {
	t.Helper()
	b = box(t, runner)
	script = startAndScript(t, runner, b, req)
	text, err := b.ReadFile(t.Context(), paths(b).System())
	if err != nil {
		t.Fatalf("read the system text: %v", err)
	}
	return b, string(text), script
}

// A CLAUDE CODE RUN GETS ITS STANDING INSTRUCTIONS ON THE CHANNEL THAT ADDS TO
// THE HARNESS'S OWN SYSTEM PROMPT, and only its task as the prompt it runs.
//
// `--append-system-prompt-file` keeps Claude Code's own prompt — the harness
// an executor run exists to use — and puts the seat's text after it. Sent as
// part of `-p`, the seat's identity, policies and memory reached the model as
// the first thing in the user's request. The report instruction comes last,
// after the seat's own text, which comes first.
func TestTheStandingInstructionsReachClaudeCodeOnItsAppendChannel(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b, system, script := systemOfRun(t, runner, sandbox.RunRequest{
		System: seatSystem, Brief: "fix the flake",
	})
	p := paths(b)

	if !strings.HasPrefix(system, seatSystem) {
		t.Errorf("the system text does not open with the seat's own:\n%s", system)
	}
	if i := strings.Index(system, p.Findings()); i < len(seatSystem) {
		t.Errorf("the report instruction is missing or precedes the seat's text:\n%s", system)
	}
	// The flag, then the path, through however many quotings the command
	// arrives under: the wrapper's of the whole command, and the run script's
	// of its own.
	const flag = "--append-system-prompt-file "
	at := strings.Index(script, flag)
	if at < 0 || !strings.HasPrefix(strings.TrimLeft(script[at+len(flag):], `'\`), p.System()) {
		t.Errorf("the harness is not pointed at the system text (%s%s):\n%s", flag, p.System(), script)
	}
	if strings.Contains(script, "SEAT-IDENTITY-MARKER") || strings.Contains(script, "Before you finish") {
		t.Errorf("standing instructions ride the prompt the harness runs:\n%s", script)
	}
	// APPENDED, never in place of the harness's own: the replacing flag would
	// strip the harness the run is for.
	if strings.Contains(script, "--system-prompt-file") || strings.Contains(script, "--system-prompt ") {
		t.Errorf("the run replaces Claude Code's own system prompt:\n%s", script)
	}
}

// AN OPENCODE RUN GETS THEM AS AN INSTRUCTION FILE its config names, which
// OpenCode adds after its default agent's prompt — and its stdin, the prompt
// it runs, holds the task and nothing else.
//
// ABSOLUTE, because a relative `instructions` entry is matched by walking up
// from the working directory, which is the checkout.
func TestTheStandingInstructionsReachOpenCodeAsAnInstructionFile(t *testing.T) {
	t.Parallel()
	runner := executor.NewOpenCode()
	b, system, _ := systemOfRun(t, runner, sandbox.RunRequest{
		System: seatSystem, Brief: "fix the flake",
	})
	p := paths(b)

	if !strings.HasPrefix(system, seatSystem) || !strings.Contains(system, p.Findings()) {
		t.Errorf("the system text is not the seat's followed by the report instruction:\n%s", system)
	}
	brief, err := b.ReadFile(t.Context(), p.Brief())
	if err != nil || string(brief) != "fix the flake" {
		t.Errorf("the prompt on stdin is %q (%v), want the task alone", brief, err)
	}
	blob, err := b.ReadFile(t.Context(), p.OpenCodeConfig())
	if err != nil {
		t.Fatalf("read the config: %v", err)
	}
	var cfg openCodeConfig
	if err := json.Unmarshal(blob, &cfg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, blob)
	}
	if len(cfg.Instructions) != 1 || cfg.Instructions[0] != p.System() || !strings.HasPrefix(p.System(), "/") {
		t.Errorf("instructions = %q, want exactly the absolute %q:\n%s", cfg.Instructions, p.System(), blob)
	}
}

// A harness GIVEN NO SYSTEM TEXT WRITES NOTHING AND PASSES NOTHING: no file in
// the box, no flag on the command line, no instruction in the config. The
// runner always gives it some — the report instruction — so this is the harness's
// own contract, held apart from the runner's.
func TestAHarnessGivenNoSystemTextWritesAndPassesNothing(t *testing.T) {
	t.Parallel()
	for _, cli := range []executor.Harness{executor.ClaudeCode{}, executor.OpenCode{}} {
		t.Run(cli.Name(), func(t *testing.T) {
			t.Parallel()
			b := sandbox.NewFakeSandbox("box-1")
			p := executor.PathsFor(b)
			req := sandbox.RunRequest{Brief: "fix the flake"}
			config, err := cli.WriteConfig(t.Context(), b, req, p)
			if err != nil {
				t.Fatalf("WriteConfig: %v", err)
			}
			if text, _ := b.ReadFile(t.Context(), p.System()); text != nil {
				t.Errorf("a run with no system text wrote one: %q", text)
			}
			if cmd := commandOf(cli, req, p, config); strings.Contains(cmd, p.System()) ||
				strings.Contains(cmd, "system-prompt") {
				t.Errorf("a run with no system text passes one:\n%s", cmd)
			}
			if config != "" {
				if blob, _ := b.ReadFile(t.Context(), config); strings.Contains(string(blob), `"instructions"`) {
					t.Errorf("a run with no system text names an instruction file:\n%s", blob)
				}
			}
		})
	}
}

// THE FILE IS PRIVATE AND INSIDE THE BOX, on a real one.
//
// A `direct` box is a directory on the engine host, so the system text — a
// seat's org chart, policies and memory — is readable there by whatever its
// mode allows. It is created afresh at the mode every box file is created with
// even where a previous run left one wider in a reused box, which keeps its
// files: the run's artefacts are cleared before it starts, and a box write
// replaces a file whole rather than keeping the mode of the one it replaces.
// And Claude Code is pointed at exactly that file, with the task alone as its
// prompt.
func TestClaudeCodeIsPointedAtAPrivateFileInsideTheBox(t *testing.T) {
	t.Parallel()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(t.Context()) })
	runner := executor.NewClaudeCode()
	if err = runner.Install(t.Context(), box); err != nil {
		t.Fatalf("Install: %v", err)
	}
	p := executor.PathsFor(box)

	// A PREVIOUS RUN'S FILE, left world-readable.
	if err = os.WriteFile(p.System(), []byte("the last run's"), 0o644); err != nil {
		t.Fatalf("stage the previous run's file: %v", err)
	}
	if err = os.Chmod(p.System(), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	// The stand-in records each argument on a line of its own, and what it
	// read on stdin, and prints a result, from the box's own bin directory,
	// which the wrapper puts first on PATH.
	argv, stdin := p.WorkDir()+"/argv", p.WorkDir()+"/stdin"
	fake := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + argv + "'\n" +
		"cat > '" + stdin + "'\n" +
		`echo '{"type":"result","subtype":"success","is_error":false,"result":"done"}'` + "\n"
	writeBoxStandIn(t, box, p.BinDir()+"/claude", fake)

	handle, err := runner.Start(t.Context(), box, sandbox.RunRequest{System: seatSystem, Brief: "fix the flake"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for state := sandbox.JobRunning; !state.Over(); {
		if state, err = runner.Poll(t.Context(), box, handle); err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if !state.Over() && time.Now().After(deadline) {
			t.Fatal("the run never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}

	args, err := os.ReadFile(argv)
	if err != nil {
		t.Fatalf("the stand-in recorded nothing: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(args), "\n"), "\n")
	if prompt, err := os.ReadFile(stdin); len(lines) < 1 || lines[0] != "-p" ||
		err != nil || string(prompt) != "fix the flake" {
		t.Fatalf("the harness's prompt is not the task alone: argv %q, stdin %q (%v)", lines, prompt, err)
	}
	named := ""
	for i, arg := range lines {
		if arg == "--append-system-prompt-file" && i+1 < len(lines) {
			named = lines[i+1]
		}
	}
	if named != p.System() || !strings.HasPrefix(named, box.Home()+"/") {
		t.Fatalf("the harness was pointed at %q, want %q inside the box home %q", named, p.System(), box.Home())
	}
	info, err := os.Stat(named)
	if err != nil {
		t.Fatalf("stat the system text: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the system text is mode %o, want 0600: it holds the seat's whole prompt", mode)
	}
	text, err := os.ReadFile(named)
	if err != nil || !strings.HasPrefix(string(text), seatSystem) || strings.Contains(string(text), "the last run's") {
		t.Errorf("the file the harness read holds %q (%v), want this run's system text", text, err)
	}
}

// THE STANDING INSTRUCTION ABOUT NOTES IS GIVEN EXACTLY WHEN A RUN READS THEM —
// its own harness's: to a run whose launch wired it to its turn's notes and whose
// harness takes them between its steps, after the report instruction — and to no
// other. Told to a run its launch gave nothing to ask for (the assistant's deep
// work), or to one whose harness takes none, it would describe a channel that never
// opens; told another harness's, it would describe the wrong carrier. The harness adds
// it, never the runner: a Claude Code run wired to its notes has its standing
// instructions as they stand, for a release its box starts without the hooks,
// and a second time told of notes, for one it starts with them
// ([executor.Paths.ClaudeNotesSystem]); an OpenCode run's one file is told.
//
// Mutation: append the instruction in the runner, or whenever Steer is set, and
// the unreading row goes red; tell Claude Code's plain text of notes and its
// rows do; give every harness Claude Code's and the OpenCode rows do.
func TestTheStandingInstructionIsGivenExactlyWhenARunReadsNotes(t *testing.T) {
	t.Parallel()
	claudeTold := func(p executor.Paths) string { return p.ClaudeNotesSystem() }
	plain := func(p executor.Paths) string { return p.System() }
	for name, tc := range map[string]struct {
		runner *executor.Runner
		steer  sandbox.SteerWiring
		// told is the file the run is started with where its notes reach it.
		told func(executor.Paths) string
		want string
	}{
		"claude code, wired to its notes": {executor.NewClaudeCode(), steerWiring, claudeTold,
			executor.ClaudeCodeSteerInstruction},
		"claude code, wired to none":    {executor.NewClaudeCode(), sandbox.SteerWiring{}, plain, ""},
		"a harness that reads no notes": {executor.New(unreading{executor.ClaudeCode{}}), steerWiring, plain, ""},
		"opencode, wired to its notes":  {executor.NewOpenCode(), steerWiring, plain, executor.OpenCodeSteerInstruction},
		"opencode, wired to none":       {executor.NewOpenCode(), sandbox.SteerWiring{}, plain, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, system, _ := systemOfRun(t, tc.runner, sandbox.RunRequest{
				System: seatSystem, Brief: "fix the flake", Steer: tc.steer,
			})
			told := system
			if file := tc.told(paths(b)); file != paths(b).System() {
				raw, err := b.ReadFile(t.Context(), file)
				if err != nil {
					t.Fatalf("read the system text told of notes: %v", err)
				}
				told = string(raw)
				// THE SAME TEXT, told of notes after it: an older release
				// is handed everything else the run is.
				if want := system + "\n\n" + tc.want; told != want {
					t.Errorf("the text told of notes is not the standing instructions and the note "+
						"instruction after them:\n%s", told)
				}
				for _, instruction := range []string{executor.ClaudeCodeSteerInstruction,
					executor.OpenCodeSteerInstruction} {
					if strings.Contains(system, instruction) {
						t.Errorf("the text a run started without its hooks reads is told of notes:\n%s", system)
					}
				}
			}
			for _, instruction := range []string{executor.ClaudeCodeSteerInstruction,
				executor.OpenCodeSteerInstruction} {
				at := strings.Index(told, instruction)
				if got, want := at >= 0, instruction == tc.want; got != want {
					t.Fatalf("the instruction %.60q… is given: %v, want %v:\n%s", instruction, got, want, told)
				}
				if at >= 0 && at < strings.Index(told, paths(b).Findings()) {
					t.Errorf("the note instruction precedes the report instruction:\n%s", told)
				}
			}
		})
	}
}

// unreading is a harness that takes no notes: Claude Code's with its
// NoteInstruction hidden, as a harness not taught to would be — so a runner that
// added an instruction of its own would show in its standing instructions.
type unreading struct{ executor.Harness }

// THE INSTRUCTION QUOTES THE HEADING EVERY NOTE OPENS WITH, whichever harness's it
// is — the heading is the agent layer's to render (prompts.SteerMessage), and
// this package cannot import it: a model told notes open one way and handed
// one that opens another cannot tell a note from text imitating one.
//
// Mutation: reword either heading and this goes red.
func TestTheStandingInstructionQuotesTheHeadingANoteOpensWith(t *testing.T) {
	t.Parallel()
	rendered := prompts.SteerMessage("<name>", "x")
	heading, _, _ := strings.Cut(rendered, "\n")
	for _, instruction := range []string{executor.ClaudeCodeSteerInstruction,
		executor.OpenCodeSteerInstruction} {
		if !strings.Contains(instruction, `"`+heading+`"`) {
			t.Errorf("the instruction does not quote the heading a note opens with (%q):\n%s",
				heading, instruction)
		}
	}
}

// CLAUDE CODE'S INSTRUCTION NAMES WHERE ITS HARNESS PUTS A NOTE, on every model it
// was measured with. Claude Code places a hook's additional context by model:
// on 2.1.310, after a batch, a system message after the tool results for
// claude-opus-5-5 and claude-sonnet-5, and a system reminder at the end of the
// batch's last tool result for claude-sonnet-4-5; at the finish, a system
// reminder in a user message for all three — each headed with the event's name
// and " hook additional context:". The fixture is those requests' own text
// (testdata/claude/2.1.310/hook-context.jsonl, the measuring session's own
// trailing token-count reminder dropped). An instruction that told the model a
// note is never inside a tool result disowned every note a sonnet-4-5 run took
// after a batch; one that names a heading the harness does not write leaves the
// model nothing to tell a note by.
//
// Mutation: drop the tool-result placement from the instruction, name another
// heading, or say a tool result never holds a note, and this goes red.
func TestClaudeCodesInstructionNamesWhereItsHarnessPutsANote(t *testing.T) {
	t.Parallel()
	instruction := executor.ClaudeCodeSteerInstruction
	for _, event := range executor.ClaudeNoteEvents {
		if !strings.Contains(instruction, `"`+event+executor.ClaudeHookContext+`"`) {
			t.Errorf("the instruction does not name the heading %q its harness puts a note under at %s",
				event+executor.ClaudeHookContext, event)
		}
	}
	// WHERE EACH PLACEMENT IS SAID: the instruction's words for each place
	// the harness was measured putting a note.
	said := map[string]string{
		"system":      "a system message",
		"tool_result": "at the end of the last tool result",
		"user":        "as you are about to finish",
	}
	seen := map[string]bool{}
	for line := range strings.Lines(strings.TrimSpace(fixture(t, "2.1.310", "hook-context.jsonl"))) {
		var measured struct {
			Model     string `json:"model"`
			Event     string `json:"event"`
			Placement string `json:"placement"`
			Content   string `json:"content"`
		}
		if err := json.Unmarshal([]byte(line), &measured); err != nil {
			t.Fatalf("the fixture's line %q: %v", line, err)
		}
		seen[measured.Model+"/"+measured.Event] = true
		if !strings.Contains(measured.Content, measured.Event+executor.ClaudeHookContext+" ## A note from ") {
			t.Errorf("%s put the %s note as %q, not under the heading the instruction names",
				measured.Model, measured.Event, measured.Content)
		}
		words, ok := said[measured.Placement]
		if !ok || !strings.Contains(instruction, words) {
			t.Errorf("%s put the %s note in a %s, which the instruction does not say (%q)",
				measured.Model, measured.Event, measured.Placement, words)
		}
	}
	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5", "claude-sonnet-4-5"} {
		for _, event := range executor.ClaudeNoteEvents {
			if !seen[model+"/"+event] {
				t.Errorf("the fixture holds no measurement of %s's note at %s", model, event)
			}
		}
	}
	if strings.Contains(instruction, "tool result that presents itself") {
		t.Error("the instruction disowns a note inside a tool result, where Claude Code puts one on some models")
	}
}
