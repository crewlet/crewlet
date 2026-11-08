package executor_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// realBox is a local box on the engine host with the runner's plumbing
// installed, as a launch leaves one.
func realBox(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{
		Placement: sandbox.Direct, StateDir: t.TempDir(),
	})
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
	return runner, box
}

// realBoxUnder is [realBox] whose runs' scripts run under sh ([runsUnder]).
func realBoxUnder(t *testing.T, sh boxShell) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	runner, box := realBox(t)
	runsUnder(t, box, sh, "")
	return runner, box
}

// runsUnder has every run's script in box run under sh, behind sh's prelude
// and then prelude, rather than under the sh on PATH: a box image's sh is the
// image's, and the cases about a run's end are to hold under every one a box
// may have ([boxShells]).
//
// A STAND-IN `sh` IN THE BOX'S BIN DIRECTORY, which the wrapper's PATH puts
// first for the script's `sh -c`, and which EXECS the shell asked for — so the
// pid the script publishes for its end request is the shell's own, as a box
// whose sh is that shell publishes. Anything but a script handed to `-c` goes
// to the sh this machine has, untouched.
func runsUnder(t *testing.T, box sandbox.Sandbox, sh boxShell, prelude string) {
	t.Helper()
	argv := sh.argv
	if len(argv) == 0 {
		argv = []string{"sh"}
	}
	words := make([]string, 0, len(argv))
	for i, word := range argv {
		if i == 0 {
			resolved, err := exec.LookPath(word)
			if err != nil {
				t.Fatalf("no %s on this machine's PATH to run the script under: %v", word, err)
			}
			word = resolved
		}
		words = append(words, shQuote(word))
	}
	plain, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("no sh on this machine's PATH: %v", err)
	}
	writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/sh", "#!/bin/sh\n"+
		`if [ "$1" = -c ] && [ "$#" -eq 2 ]; then exec `+strings.Join(words, " ")+` -c `+
		shQuote(sh.prelude+prelude)+`"$2"; fi`+"\n"+
		"exec "+shQuote(plain)+` "$@"`+"\n")
}

// steerWiring is what an executor run's launch hands its harness for its turn's
// notes: the bridge's server and its unlisted tool, as the engine names them.
var steerWiring = sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take",
	URL: "http://127.0.0.1:7/mcp/tok/steer"}

// CLAUDE'S COMMAND CARRIES NO BRIEF, AND NAMES ITS SETTINGS.
//
// The brief is the prompt `-p` reads on stdin: a brief carries every note the
// turn has read, one argument is capped at 128 KiB, and a command line is
// readable by every account on a direct box's host. So no part of it — a quote
// included — is on the command line at all. Every run names the settings every
// run has; a run wired to its notes names the settings holding its hooks too,
// for its release to choose between, and one that is not never names them. And
// nothing that drops the repository's own settings (--setting-sources, which
// also drops its CLAUDE.md) or doubles the stream (--include-hook-events) is
// ever passed.
//
// Mutation: put the brief back after -p and the first row goes red; name the
// hooked settings whatever the request and the unwired row does; drop
// --settings from the unwired run's arguments and it goes red too.
func TestClaudesCommandCarriesNoBriefAndNamesItsSettings(t *testing.T) {
	t.Parallel()
	p := executor.Paths{Home: "/box"}
	const brief = `fix it'; rm -rf / # "quoted" BRIEF-MARKER`
	for name, tc := range map[string]struct {
		steer  sandbox.SteerWiring
		hooked bool
	}{
		"wired to its notes": {steer: steerWiring, hooked: true},
		"wired to none":      {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cmd := commandOf(claude(), sandbox.RunRequest{Brief: brief, Steer: tc.steer}, p, "")
			// The brief's own words, never the script's: the script removes
			// files of its own with an `rm -rf` of a quoted path.
			for _, piece := range []string{"BRIEF-MARKER", "rm -rf / #", "fix it"} {
				if strings.Contains(cmd, piece) {
					t.Fatalf("the brief is on the command line (%q):\n%s", piece, cmd)
				}
			}
			// EVERY RUN is one script, which reads the harness's version before it
			// starts it ([claudeRelease]), the harness on a line of its own.
			cmd = scriptOf(t, cmd)
			if !strings.Contains(cmd, "\nclaude -p --output-format stream-json --verbose ") {
				t.Errorf("-p is not the bare print flag, with the prompt left to stdin:\n%s", cmd)
			}
			if every := "--settings '" + p.ClaudeSettings() + "'"; !strings.Contains(cmd, every) {
				t.Errorf("the run does not name %s, the settings every run has:\n%s", every, cmd)
			}
			hooked := "--settings '" + p.ClaudeNotesSettings() + "'"
			if got := strings.Contains(cmd, hooked); got != tc.hooked {
				t.Errorf("names %s: %v, want %v:\n%s", hooked, got, tc.hooked, cmd)
			}
			for _, never := range []string{"--setting-sources", "--include-hook-events"} {
				if strings.Contains(cmd, never) {
					t.Errorf("the command passes %s:\n%s", never, cmd)
				}
			}
		})
	}
}

// CLAUDE'S BRIEF IS READ FROM STDIN: the runner writes it to the brief's file,
// whole, and the wrapper points the harness's standard input at that file rather
// than at /dev/null — which still ends, so a headless run never waits on input.
//
// Mutation: drop BriefOnStdin from Claude's Output and the harness reads nothing.
func TestClaudesBriefIsReadFromStdin(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	const brief = "fix the flake\n\n## A note from ada, sent while this turn was running\n\nuse `main` & \"staging\""
	script := startAndScript(t, runner, b, sandbox.RunRequest{Brief: brief, Steer: steerWiring})

	written, err := b.ReadFile(t.Context(), p.Brief())
	if err != nil || string(written) != brief {
		t.Fatalf("the brief's file holds %q (%v), want the brief whole", written, err)
	}
	if want := `< '\''` + p.Brief() + `'\''`; !strings.Contains(script, want) {
		t.Errorf("the harness's stdin is not the brief's file (%s):\n%s", want, script)
	}
	// THE HARNESS'S OWN STDIN: the version the script reads first is asked with
	// none, which is no prompt at all, and so is the pack after the harness has
	// exited; the harness's own line names none.
	_, line, _ := strings.Cut(script, "claude -p")
	if line, _, _ = strings.Cut(line, "\n"); strings.Contains(line, "< /dev/null") {
		t.Errorf("the harness's stdin is closed, so it runs no prompt:\n%s", script)
	}
}

// THE WRAPPER UNSETS THE VARIABLES THAT SWITCH HOOKS OFF, as a statement of its
// own before the harness's: bare mode (CLAUDE_CODE_SIMPLE) drops every hook, and
// safe mode (CLAUDE_CODE_SAFE_MODE) drops hooks and the --mcp-config bridge;
// either can come from the seat's environment, the image or a profile the login
// shell sources, and only the script, which runs after all three, reaches every
// one. For every Claude run — the bridge is every run's tools — and for no
// other harness, whose environment those names mean nothing to.
//
// Mutation: drop Unset from Claude's Output, or emit it after the harness, and this
// goes red.
func TestTheWrapperUnsetsTheVariablesThatSwitchHooksOff(t *testing.T) {
	t.Parallel()
	for name, req := range map[string]sandbox.RunRequest{
		"wired to its notes": {Brief: "x", Steer: steerWiring},
		"wired to none":      {Brief: "x"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := executor.NewClaudeCode()
			script := startAndScript(t, runner, box(t, runner), req)
			unset := strings.Index(script, "unset CLAUDE_CODE_SIMPLE CLAUDE_CODE_SAFE_MODE DISABLE_AUTO_COMPACT DISABLE_COMPACT; ")
			launch := strings.Index(script, "claude -p")
			if unset < 0 || launch < 0 || unset > launch {
				t.Fatalf("the variables are not unset before the harness starts (unset at %d, harness at %d):\n%s",
					unset, launch, script)
			}
		})
	}
	runner := executor.NewOpenCode()
	if script := startAndScript(t, runner, box(t, runner), sandbox.RunRequest{Brief: "x"}); strings.Contains(script, "CLAUDE_CODE_") {
		t.Errorf("an OpenCode run unsets Claude Code's variables:\n%s", script)
	}
}

// THE SETTINGS FILES ARE THE MEASURED SHAPES, and nothing more. Every run's
// holds the harness's own retention sweep off and nothing else; a run wired to
// its notes has them again with its hooks: `disableAllHooks` written false (a
// repository's own true otherwise switches every hook off without a word), one
// mcp_tool hook at PostToolBatch and one at Stop, each calling the tool the
// launch named on the server it named, with the event and the agent id the
// harness fills in, under a ten-second timeout. No field newer than the oldest
// release each is handed — print mode ignores a file it cannot validate, hooks,
// retention and all — which the whole-document comparisons hold: a key added is
// a key this case did not measure.
//
// Mutation: tag disableAllHooks omitempty and the hooked document loses it;
// write the hooks into every run's settings and the unwired document gains
// them.
func TestTheSettingsFilesAreTheMeasuredShapes(t *testing.T) {
	t.Parallel()
	shape := func(t *testing.T, b sandbox.Sandbox, file, measured string) {
		t.Helper()
		raw, err := b.ReadFile(t.Context(), file)
		if err != nil {
			t.Fatalf("no settings were written at %s: %v", file, err)
		}
		var got, want any
		if err = json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("the settings are not JSON: %v\n%s", err, raw)
		}
		if err = json.Unmarshal([]byte(measured), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("the settings at %s are not the measured shape:\n%s", file, raw)
		}
	}
	const every = `{"cleanupPeriodDays": 36500}`

	b := sandbox.NewFakeSandbox("box-1")
	p := executor.PathsFor(b)
	if _, err := (executor.ClaudeCode{}).WriteConfig(t.Context(), b,
		sandbox.RunRequest{Brief: "x", Steer: steerWiring}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	shape(t, b, p.ClaudeSettings(), every)
	shape(t, b, p.ClaudeNotesSettings(), `{
		"cleanupPeriodDays": 36500,
		"disableAllHooks": false,
		"hooks": {
			"PostToolBatch": [{"hooks": [{"type": "mcp_tool", "server": "crewlet", "tool": "steer_take",
				"input": {"event": "${hook_event_name}", "agent": "${agent_id}"}, "timeout": 10}]}],
			"Stop": [{"hooks": [{"type": "mcp_tool", "server": "crewlet", "tool": "steer_take",
				"input": {"event": "${hook_event_name}", "agent": "${agent_id}"}, "timeout": 10}]}]
		}
	}`)
	if executor.ClaudeHookTimeout != 10*time.Second {
		t.Errorf("the hook timeout is %v, and the file says ten seconds", executor.ClaudeHookTimeout)
	}

	// A RUN WIRED TO NO NOTES WRITES NO HOOKS: the assistant's deep work has
	// no inbox, and hooks calling a tool its bridge does not serve would be a
	// call at every step for nothing. It has every run's settings all the same.
	plain := sandbox.NewFakeSandbox("box-2")
	if _, err := (executor.ClaudeCode{}).WriteConfig(t.Context(), plain,
		sandbox.RunRequest{Brief: "x"}, executor.PathsFor(plain)); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	shape(t, plain, executor.PathsFor(plain).ClaudeSettings(), every)
	if raw, _ := plain.ReadFile(t.Context(), executor.PathsFor(plain).ClaudeNotesSettings()); raw != nil {
		t.Errorf("a run wired to no notes wrote the notes' hooks: %s", raw)
	}
}

// THE SETTINGS FILES ARE WRITTEN PRIVATE, UNDER THE BOX'S WORK DIRECTORY, on a
// real box: outside the checkout, which a commit carries and the run edits, and
// written afresh at the mode a box writes every file with even where a previous
// run left one wider — the run's artefacts are cleared before it starts, and a
// box write replaces a file whole rather than keeping the replaced one's mode.
//
// Mutation: drop the settings from ClearArtifacts and write a local box's file
// in place (O_TRUNC), and the previous run's wide files keep their mode — either
// alone still holds it.
func TestTheSettingsFilesAreWrittenPrivateUnderTheWorkDir(t *testing.T) {
	t.Parallel()
	runner, box := realBox(t)
	p := executor.PathsFor(box)
	files := map[string]string{
		// What each holds of this run's own writing.
		p.ClaudeSettings():      `"cleanupPeriodDays"`,
		p.ClaudeNotesSettings(): `"steer_take"`,
	}
	for file := range files {
		if !strings.HasPrefix(file, p.WorkDir()+"/") || strings.Contains(file, "/"+sandbox.WorkspaceSubdir+"/") {
			t.Fatalf("the settings file %q is not under the work directory %q, out of the checkout",
				file, p.WorkDir())
		}
		// A PREVIOUS RUN'S FILE, left world-readable.
		if err := os.WriteFile(file, []byte(`{"hooks":{}}`), 0o644); err != nil {
			t.Fatalf("stage the previous run's file: %v", err)
		}
		if err := os.Chmod(file, 0o644); err != nil {
			t.Fatalf("chmod: %v", err)
		}
	}
	installClaude(t, box, `echo '{"type":"result","subtype":"success","is_error":false,"result":"done"}'`)
	waitRun(t, runner, box, sandbox.RunRequest{Brief: "x", Steer: steerWiring})

	for file, own := range files {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatalf("stat the settings: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("the settings file %s is mode %o, want 0600", file, mode)
		}
		if raw, _ := os.ReadFile(file); !strings.Contains(string(raw), own) { //nolint:gosec // the case's own box
			t.Errorf("%s holds the previous run's settings: %s", file, raw)
		}
	}
}

// A HOOK-STOPPED RUN FAILS, AND SAYS WHY. Measured on 2.1.263 and 2.1.310: a
// repository's own hook that exits 2 ends the loop as subtype success, no error
// and an empty result, its terminal reason hook_stopped — which, read as it
// reports itself, was a run that succeeded and said nothing. A run whose loop
// completed, and a result from a harness that reports no terminal reason, stay
// successful.
//
// Mutation: drop the terminal reason from succeeded() and the stopped rows go
// green as successes.
func TestAHookStoppedRunFailsWithItsReason(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"2.1.263", "2.1.310"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			stopped := claude().Parse(fixture(t, version, "result-hook-stopped.json"))
			if stopped.Success {
				t.Fatal("a run a hook stopped read as a success")
			}
			for _, want := range []string{"hook_stopped", "a hook in the run stopped its loop"} {
				if !strings.Contains(stopped.Error, want) {
					t.Errorf("the failure does not say %q: %s", want, stopped.Error)
				}
			}
			completed := claude().Parse(fixture(t, version, "result-completed.json"))
			if !completed.Success || completed.Text != "final after stop note" {
				t.Errorf("a completed run = success %v, text %q, error %q; want the answer after "+
					"its finish's note", completed.Success, completed.Text, completed.Error)
			}
		})
	}
	if absent := claude().Parse(`{"type":"result","subtype":"success","is_error":false,"result":"done"}`); !absent.Success {
		t.Errorf("a result with no terminal reason read as failed: %+v", absent)
	}
	// THE TRANSCRIPT SAYS IT TOO, from the stream's own last line.
	dec := claude().Events()
	dec.Line([]byte(fixture(t, "2.1.310", "result-hook-stopped.json")))
	if got := dec.Result().Transcript; !strings.Contains(got, "[error] the run ended: a hook in the run stopped its loop") {
		t.Errorf("the transcript does not say the hook stopped the run: %q", got)
	}
}

// ClaudeCode READS NOTES, and says so to the runner it is driven by.
func TestClaudeCodeReadsNotes(t *testing.T) {
	t.Parallel()
	if !executor.NewClaudeCode().ReadsNotes() {
		t.Fatal("the Claude Code runner says its harness reads no notes, so no run is told notes arrive")
	}
}

// OpenCode READS NOTES, through its plugin, and says so to the runner it is
// driven by — so its runs are told notes arrive, and one that never asked for
// them is said at its collection.
func TestOpenCodeReadsNotes(t *testing.T) {
	t.Parallel()
	if !executor.NewOpenCode().ReadsNotes() {
		t.Fatal("the OpenCode runner says its harness reads no notes, so no run is told notes arrive")
	}
}

// fixture is a measured stream's file, sanitized, for one Claude Code release.
func fixture(t *testing.T, version, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "claude", version, name))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	return string(raw)
}

// installClaude puts a stand-in `claude` on a real box's PATH (the box's own
// bin directory, which the wrapper puts first), running body, and answering
// `--version` as the newest release measured does.
func installClaude(t *testing.T, box sandbox.Sandbox, body string) {
	t.Helper()
	p := executor.PathsFor(box)
	version := `[ "$1" = --version ] && { echo '2.1.310 (Claude Code)'; exit 0; }` + "\n"
	writeBoxStandIn(t, box, p.BinDir()+"/claude", "#!/bin/sh\n"+version+body+"\n")
}

// waitRun starts req on a real box and waits for it to finish.
func waitRun(t *testing.T, runner *executor.Runner, box sandbox.Sandbox, req sandbox.RunRequest) {
	t.Helper()
	handle, err := runner.Start(t.Context(), box, req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		state, err := runner.Poll(t.Context(), box, handle)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if state.Over() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lines is a file's lines, as a stand-in recorded them one to a line.
func lines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the stand-in recorded nothing at %s: %v", path, err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// after is the argument following flag in argv, or "".
func after(argv []string, flag string) string {
	if i := slices.Index(argv, flag); i >= 0 && i+1 < len(argv) {
		return argv[i+1]
	}
	return ""
}

// A CLAUDE CODE OLDER THAN ITS NOTE FLOOR STARTS WITHOUT THE HOOKS, AND IS NOT
// TOLD OF NOTES. From 2.1.118 a release runs a settings file's mcp_tool hooks,
// and before 2.1.163 it drops what a Stop hook answers: a note taken as the run
// finishes would be recorded read, and published delivered, by a run that never
// saw it. So a run wired to its notes reads the box's version first and names
// the settings holding its hooks, and the standing instructions told of notes,
// only on 2.1.163 or later; an older release is handed the settings every run
// has and the standing instructions alone, and the run's error stream says why
// — its notes then wait for the review, and its collection says it never asked.
// A version it cannot read is taken for a current release. Under every sh a
// box may run the script with.
//
// Mutation: name the hooked settings whatever the version, compare the patch
// alone, or hand an older release the instructions told of notes, and this
// goes red.
func TestAClaudeCodeOlderThanItsNoteFloorStartsWithoutTheHooks(t *testing.T) {
	t.Parallel()
	p := executor.Paths{Home: "/box"}
	script := scriptOf(t, commandOf(claude(), sandbox.RunRequest{Brief: "x", System: "be careful",
		Steer: steerWiring}, p, ""))
	standIn := `#!/bin/sh
if [ "$1" = --version ]; then printf '%s' "$FAKE_VERSION"; exit 0; fi
for a in "$@"; do printf '%s\n' "$a"; done > "$FAKE_DIR/argv"
`
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for _, tc := range []struct {
			version string
			hooks   bool
		}{
			{"2.1.150 (Claude Code)", false},
			{"2.1.162 (Claude Code)", false},
			{"2.0.300 (Claude Code)", false},
			{"1.9.999 (Claude Code)", false},
			{"2.1.163 (Claude Code)", true},
			{"2.1.310 (Claude Code)", true},
			{"2.2.0 (Claude Code)", true},
			{"10.0.0 (Claude Code)", true},
			{"", true},
			{"Claude Code, a version of its own", true},
		} {
			t.Run(tc.version, func(t *testing.T) {
				dir := t.TempDir()
				bin := filepath.Join(dir, "bin")
				if err := os.MkdirAll(bin, 0o700); err != nil {
					t.Fatal(err)
				}
				writeStandIn(t, filepath.Join(bin, "claude"), standIn)
				argv := sh.argv
				if len(argv) == 0 {
					argv = []string{"sh"}
				}
				cmd := exec.Command(argv[0], append(append([]string{}, argv[1:]...), "-c", sh.prelude+script)...) //nolint:gosec // the script under test
				cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "FAKE_DIR=" + dir,
					"FAKE_VERSION=" + tc.version}
				var stderr strings.Builder
				cmd.Stdin, cmd.Stderr = strings.NewReader("x"), &stderr
				if err := cmd.Run(); err != nil {
					t.Fatalf("the script: %v\n%s", err, stderr.String())
				}
				raw, err := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // under the case's own dir
				if err != nil {
					t.Fatalf("the harness never started: %v\n%s", err, stderr.String())
				}
				args := strings.Split(strings.TrimSpace(string(raw)), "\n")
				// ONE SETTINGS FILE, the one the hooks decide: with them
				// where they are read, every run's where they are not.
				settings := p.ClaudeSettings()
				if tc.hooks {
					settings = p.ClaudeNotesSettings()
				}
				if n := strings.Count(strings.Join(args, "\n"), "--settings"); n != 1 ||
					after(args, "--settings") != settings {
					t.Errorf("on %q the harness was handed %d settings files, %q, want %q alone (argv %q)",
						tc.version, n, after(args, "--settings"), settings, args)
				}
				if said := strings.Contains(stderr.String(), "older than 2.1.163"); said == tc.hooks {
					t.Errorf("on %q the run's error stream said it ran without its hooks: %v, want %v\n%s",
						tc.version, said, !tc.hooks, stderr.String())
				}
				// ONE SYSTEM TEXT, the one the hooks decide: told of notes
				// where they are read, as it stands where they are not.
				want := p.System()
				if tc.hooks {
					want = p.ClaudeNotesSystem()
				}
				if n := strings.Count(strings.Join(args, "\n"), "--append-system-prompt-file"); n != 1 ||
					after(args, "--append-system-prompt-file") != want {
					t.Errorf("on %q the harness was handed %d system texts, %q, want %q alone (argv %q)",
						tc.version, n, after(args, "--append-system-prompt-file"), want, args)
				}
			})
		}
	})
}

// EVERY CLAUDE CODE RUN IS STARTED WITH ITS BOX'S SWEEP HELD OFF: whatever its
// release and whatever it is wired to, the settings file its harness is handed
// holds `cleanupPeriodDays` at a hundred years — a run wired to its notes on a
// release that reads them in the file with its hooks, and every other run, one
// below the note floor among them, in the file every run has. The box's Claude
// Code sweeps its configuration directory about five seconds into every run,
// by each file's own mtime, and a session carried into the box keeps the
// mtimes it was packed with: at the harness's default of thirty days a
// continued conversation's turn lost its oldest tool results and its task list
// to its own box (measured on 2.1.310). Played on a real box, the harness a
// stand-in that keeps the file its --settings names.
//
// Mutation: drop cleanupPeriodDays from every run's settings, embed no
// retention in the hooked ones, or start a run below the floor or wired to
// none without --settings, and a row goes red.
func TestEveryClaudeCodeRunIsStartedWithItsBoxsSweepHeldOff(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		version string
		steer   sandbox.SteerWiring
		hooks   bool
	}{
		"wired to its notes, on a release that reads them": {"2.1.310 (Claude Code)", steerWiring, true},
		"wired to its notes, below the note floor":         {"2.1.150 (Claude Code)", steerWiring, false},
		"wired to its notes, on a release it cannot read":  {"Claude Code, a version of its own", steerWiring, true},
		"wired to none":                       {"2.1.310 (Claude Code)", sandbox.SteerWiring{}, false},
		"wired to none, below the note floor": {"2.1.150 (Claude Code)", sandbox.SteerWiring{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, box := realBox(t)
			p := executor.PathsFor(box)
			record := t.TempDir()
			writeBoxStandIn(t, box, p.BinDir()+"/claude", "#!/bin/sh\n"+
				`[ "$1" = --version ] && { echo `+shQuote(tc.version)+`; exit 0; }`+"\n"+
				`d=`+shQuote(record)+"\n"+
				`for a in "$@"; do printf '%s\n' "$a"; done > "$d/argv"`+"\n"+
				`prev=; for a in "$@"; do [ "$prev" = --settings ] && cp "$a" "$d/settings.json"; prev=$a; done`+"\n"+
				`echo '{"type":"result","subtype":"success","is_error":false,"result":"done"}'`+"\n")
			waitRun(t, runner, box, sandbox.RunRequest{Brief: "x", System: "be careful", Steer: tc.steer})

			argv := lines(t, filepath.Join(record, "argv"))
			want := p.ClaudeSettings()
			if tc.hooks {
				want = p.ClaudeNotesSettings()
			}
			if n := strings.Count(strings.Join(argv, "\n"), "--settings"); n != 1 || after(argv, "--settings") != want {
				t.Fatalf("the harness was handed %d settings files, %q, want %q alone (argv %q)",
					n, after(argv, "--settings"), want, argv)
			}
			var settings struct {
				CleanupPeriodDays *int            `json:"cleanupPeriodDays"`
				Hooks             json.RawMessage `json:"hooks"`
			}
			readJSON(t, filepath.Join(record, "settings.json"), &settings)
			if settings.CleanupPeriodDays == nil || *settings.CleanupPeriodDays != 36500 {
				t.Errorf("the settings the harness was handed hold cleanupPeriodDays %v, want 36500: "+
					"its box would sweep the session it carries", settings.CleanupPeriodDays)
			}
			if hooked := len(settings.Hooks) > 0; hooked != tc.hooks {
				t.Errorf("the settings the harness was handed hold hooks: %v, want %v", hooked, tc.hooks)
			}
		})
	}
}

// A RUN THAT RAN OUT OF CONTEXT IS TOLD FROM EVERY OTHER FAILED ONE, by the
// harness's own field first, because a failure's words can quote anything.
func TestAContextExhaustedRunIsToldByTheHarnesssField(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, line string
		want       bool
	}{
		{"the terminal reason", `{"type":"result","subtype":"success","is_error":true,` +
			`"terminal_reason":"prompt_too_long","result":"Prompt is too long"}`, true},
		{"automatic compaction thrashing", `{"type":"result","subtype":"success","is_error":true,` +
			`"terminal_reason":"rapid_refill_breaker","result":"Autocompact is thrashing: the context refilled ` +
			`to the limit within 3 turns of the previous compact, 3 times in a row."}`, true},
		{"the blocking limit", `{"type":"result","subtype":"success","is_error":true,` +
			`"terminal_reason":"blocking_limit","result":"Context limit reached"}`, true},
		{"the thrashing sentence, on a release with no terminal reason",
			`{"type":"result","subtype":"success","is_error":true,"result":"Autocompact is thrashing: ` +
				`the context refilled to the limit within 3 turns of the previous compact, 3 times in a row."}`, true},
		{"the sentence, on a release with no terminal reason",
			`{"type":"result","subtype":"success","is_error":true,"result":"Prompt is too long"}`, true},
		{"the API's sentence in errors", `{"type":"result","subtype":"error_during_execution","is_error":true,` +
			`"errors":["input length and max_tokens exceed context limit: 200000"]}`, true},
		{"the field outweighs a quotation", `{"type":"result","subtype":"success","is_error":true,` +
			`"terminal_reason":"api_error","result":"tool said: Prompt is too long"}`, false},
		{"another failure", `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"cap"}`, false},
		{"a run that succeeded and wrote the words",
			`{"type":"result","subtype":"success","is_error":false,"result":"Prompt is too long, I trimmed it"}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := claude().Parse(c.line); got.ContextExhausted != c.want {
				t.Errorf("ContextExhausted = %v, want %v, on %s", got.ContextExhausted, c.want, c.line)
			}
		})
	}
}
