package codingagent_test

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
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// steerWiring is what an executor run's launch hands its CLI for its turn's
// notes: the bridge's server and its unlisted tool, as the engine names them.
var steerWiring = sandbox.SteerWiring{Server: "crewlet", Tool: "steer_take",
	URL: "http://127.0.0.1:7/mcp/tok/steer"}

// CLAUDE'S COMMAND CARRIES NO BRIEF, AND NAMES ITS SETTINGS WHERE IT HAS SOME.
//
// The brief is the prompt `-p` reads on stdin: a brief carries every note the
// turn has read, one argument is capped at 128 KiB, and a command line is
// readable by every account on a direct box's host. So no part of it — a quote
// included — is on the command line at all. A run wired to its notes names the
// settings file holding its hooks; one that is not names none. And nothing that
// drops the repository's own settings (--setting-sources, which also drops its
// CLAUDE.md) or doubles the stream (--include-hook-events) is ever passed.
//
// Mutation: put the brief back after -p and the first row goes red; name the
// settings whatever the request and the unwired row does.
func TestClaudesCommandCarriesNoBriefAndNamesItsSettings(t *testing.T) {
	t.Parallel()
	p := codingagent.Paths{Home: "/box"}
	const brief = `fix it'; rm -rf / # "quoted" BRIEF-MARKER`
	for name, tc := range map[string]struct {
		steer    sandbox.SteerWiring
		settings bool
	}{
		"wired to its notes": {steer: steerWiring, settings: true},
		"wired to none":      {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cmd := claude().Command(sandbox.RunRequest{Brief: brief, Steer: tc.steer}, p, "")
			for _, piece := range []string{"BRIEF-MARKER", "rm -rf", "fix it"} {
				if strings.Contains(cmd, piece) {
					t.Fatalf("the brief is on the command line (%q):\n%s", piece, cmd)
				}
			}
			// A RUN WIRED TO ITS NOTES is one script, which reads the CLI's
			// version before it starts it ([claudeNotesFloor]).
			invocation := "claude -p --output-format stream-json --verbose "
			if tc.settings {
				cmd = scriptOf(t, cmd)
				invocation = "exec " + invocation
			}
			if !strings.HasPrefix(cmd, invocation) && !strings.Contains(cmd, "; "+invocation) {
				t.Errorf("-p is not the bare print flag, with the prompt left to stdin:\n%s", cmd)
			}
			want := "--settings '" + p.ClaudeSettings() + "'"
			if got := strings.Contains(cmd, want); got != tc.settings {
				t.Errorf("names %s: %v, want %v:\n%s", want, got, tc.settings, cmd)
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
// whole, and the wrapper points the CLI's standard input at that file rather
// than at /dev/null — which still ends, so a headless run never waits on input.
//
// Mutation: drop BriefOnStdin from Claude's Output and the CLI reads nothing.
func TestClaudesBriefIsReadFromStdin(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	const brief = "fix the flake\n\n## A note from ada, sent while this turn was running\n\nuse `main` & \"staging\""
	script := startAndScript(t, runner, b, sandbox.RunRequest{Brief: brief, Steer: steerWiring})

	written, err := b.ReadFile(t.Context(), p.Brief())
	if err != nil || string(written) != brief {
		t.Fatalf("the brief's file holds %q (%v), want the brief whole", written, err)
	}
	if want := `< '\''` + p.Brief() + `'\''`; !strings.Contains(script, want) {
		t.Errorf("the CLI's stdin is not the brief's file (%s):\n%s", want, script)
	}
	// THE CLI'S OWN STDIN: the version the script reads first is asked with
	// none, which is no prompt at all.
	if _, cli, _ := strings.Cut(script, "claude -p"); strings.Contains(cli, "< /dev/null") {
		t.Errorf("the CLI's stdin is closed, so it runs no prompt:\n%s", script)
	}
}

// THE WRAPPER UNSETS THE VARIABLES THAT SWITCH HOOKS OFF, as a statement of its
// own before the CLI's: bare mode (CLAUDE_CODE_SIMPLE) drops every hook, and
// safe mode (CLAUDE_CODE_SAFE_MODE) drops hooks and the --mcp-config bridge;
// either can come from the seat's environment, the image or a profile the login
// shell sources, and only the script, which runs after all three, reaches every
// one. For every Claude run — the bridge is every run's tools — and for no
// other CLI, whose environment those names mean nothing to.
//
// Mutation: drop Unset from Claude's Output, or emit it after the CLI, and this
// goes red.
func TestTheWrapperUnsetsTheVariablesThatSwitchHooksOff(t *testing.T) {
	t.Parallel()
	for name, req := range map[string]sandbox.RunRequest{
		"wired to its notes": {Brief: "x", Steer: steerWiring},
		"wired to none":      {Brief: "x"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := codingagent.NewClaudeCode()
			script := startAndScript(t, runner, box(t, runner), req)
			unset := strings.Index(script, "unset CLAUDE_CODE_SIMPLE CLAUDE_CODE_SAFE_MODE; ")
			cli := strings.Index(script, "claude -p")
			if unset < 0 || cli < 0 || unset > cli {
				t.Fatalf("the variables are not unset before the CLI starts (unset at %d, CLI at %d):\n%s",
					unset, cli, script)
			}
		})
	}
	runner := codingagent.NewOpenCode()
	if script := startAndScript(t, runner, box(t, runner), sandbox.RunRequest{Brief: "x"}); strings.Contains(script, "CLAUDE_CODE_") {
		t.Errorf("an OpenCode run unsets Claude Code's variables:\n%s", script)
	}
}

// THE SETTINGS FILE IS THE MEASURED SHAPE, and nothing more: `disableAllHooks`
// written false (a repository's own true otherwise switches every hook off
// without a word), one mcp_tool hook at PostToolBatch and one at Stop, each
// calling the tool the launch named on the server it named, with the event and
// the agent id the CLI fills in, under a ten-second timeout. No field newer than
// 2.1.263 — print mode ignores a file it cannot validate, hooks and all — which
// the whole-document comparison holds: a key added is a key this case did not
// measure.
//
// Mutation: tag disableAllHooks omitempty and the document loses it.
func TestTheSettingsFileIsTheMeasuredShape(t *testing.T) {
	t.Parallel()
	b := sandbox.NewFakeSandbox("box-1")
	p := codingagent.PathsFor(b)
	if _, err := (codingagent.ClaudeCode{}).WriteConfig(t.Context(), b,
		sandbox.RunRequest{Brief: "x", Steer: steerWiring}, p); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	raw, err := b.ReadFile(t.Context(), p.ClaudeSettings())
	if err != nil {
		t.Fatalf("no settings were written: %v", err)
	}
	var got, want any
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the settings are not JSON: %v\n%s", err, raw)
	}
	const measured = `{
		"disableAllHooks": false,
		"hooks": {
			"PostToolBatch": [{"hooks": [{"type": "mcp_tool", "server": "crewlet", "tool": "steer_take",
				"input": {"event": "${hook_event_name}", "agent": "${agent_id}"}, "timeout": 10}]}],
			"Stop": [{"hooks": [{"type": "mcp_tool", "server": "crewlet", "tool": "steer_take",
				"input": {"event": "${hook_event_name}", "agent": "${agent_id}"}, "timeout": 10}]}]
		}
	}`
	if err = json.Unmarshal([]byte(measured), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the settings file is not the measured shape:\n%s", raw)
	}
	if codingagent.ClaudeHookTimeout != 10*time.Second {
		t.Errorf("the hook timeout is %v, and the file says ten seconds", codingagent.ClaudeHookTimeout)
	}

	// A RUN WIRED TO NO NOTES WRITES NONE: the assistant's deep work has no
	// inbox, and hooks calling a tool its bridge does not serve would be a
	// call at every step for nothing.
	plain := sandbox.NewFakeSandbox("box-2")
	if _, err = (codingagent.ClaudeCode{}).WriteConfig(t.Context(), plain,
		sandbox.RunRequest{Brief: "x"}, codingagent.PathsFor(plain)); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	if raw, _ := plain.ReadFile(t.Context(), codingagent.PathsFor(plain).ClaudeSettings()); raw != nil {
		t.Errorf("a run wired to no notes wrote settings: %s", raw)
	}
}

// THE SETTINGS FILE IS WRITTEN PRIVATE, UNDER THE BOX'S WORK DIRECTORY, on a real
// box: outside the checkout, which a commit carries and the run edits, and
// written afresh at the mode a box writes every file with even where a previous
// run left one wider — the run's artefacts are cleared before it starts, and a
// box write replaces a file whole rather than keeping the replaced one's mode.
//
// Mutation: drop the settings from ClearArtifacts and write a local box's file
// in place (O_TRUNC), and the previous run's wide file keeps its mode — either
// alone still holds it.
func TestTheSettingsFileIsWrittenPrivateUnderTheWorkDir(t *testing.T) {
	t.Parallel()
	runner, box := realBox(t)
	p := codingagent.PathsFor(box)
	if !strings.HasPrefix(p.ClaudeSettings(), p.WorkDir()+"/") ||
		strings.Contains(p.ClaudeSettings(), "/"+sandbox.WorkspaceSubdir+"/") {
		t.Fatalf("the settings file %q is not under the work directory %q, out of the checkout",
			p.ClaudeSettings(), p.WorkDir())
	}
	// A PREVIOUS RUN'S FILE, left world-readable.
	if err := os.WriteFile(p.ClaudeSettings(), []byte(`{"hooks":{}}`), 0o644); err != nil {
		t.Fatalf("stage the previous run's file: %v", err)
	}
	if err := os.Chmod(p.ClaudeSettings(), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	installClaude(t, box, `echo '{"type":"result","subtype":"success","is_error":false,"result":"done"}'`)
	waitRun(t, runner, box, sandbox.RunRequest{Brief: "x", Steer: steerWiring})

	info, err := os.Stat(p.ClaudeSettings())
	if err != nil {
		t.Fatalf("stat the settings: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the settings file is mode %o, want 0600", mode)
	}
	if raw, _ := os.ReadFile(p.ClaudeSettings()); !strings.Contains(string(raw), `"steer_take"`) {
		t.Errorf("the file holds the previous run's settings: %s", raw)
	}
}

// A HOOK-STOPPED RUN FAILS, AND SAYS WHY. Measured on 2.1.263 and 2.1.310: a
// repository's own hook that exits 2 ends the loop as subtype success, no error
// and an empty result, its terminal reason hook_stopped — which, read as it
// reports itself, was a run that succeeded and said nothing. A run whose loop
// completed, and a result from a CLI that reports no terminal reason, stay
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
	if !codingagent.NewClaudeCode().ReadsNotes() {
		t.Fatal("the Claude Code runner says its CLI reads no notes, so no run is told notes arrive")
	}
}

// OpenCode READS NOTES, through its plugin, and says so to the runner it is
// driven by — so its runs are told notes arrive, and one that never asked for
// them is said at its collection.
func TestOpenCodeReadsNotes(t *testing.T) {
	t.Parallel()
	if !codingagent.NewOpenCode().ReadsNotes() {
		t.Fatal("the OpenCode runner says its CLI reads no notes, so no run is told notes arrive")
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

// installClaude puts a stand-in `claude` on a real box's PATH — the shim
// directory the wrapper puts first — running body, and answering `--version`
// as the newest release measured does.
func installClaude(t *testing.T, box sandbox.Sandbox, body string) {
	t.Helper()
	p := codingagent.PathsFor(box)
	version := `[ "$1" = --version ] && { echo '2.1.310 (Claude Code)'; exit 0; }` + "\n"
	writeBoxStandIn(t, box, p.BinDir()+"/claude", "#!/bin/sh\n"+version+body+"\n")
}

// waitRun starts req on a real box and waits for it to finish.
func waitRun(t *testing.T, runner *codingagent.Runner, box sandbox.Sandbox, req sandbox.RunRequest) {
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
// its settings, and the standing instructions told of notes, only on 2.1.163 or
// later; an older release is handed the standing instructions alone, and the
// run's error stream says why — its notes then wait for the review, and its
// collection says it never asked. A version it cannot read is taken for a
// current release. Under every sh a box may run the script with.
//
// Mutation: name the settings whatever the version, compare the patch alone, or
// hand an older release the instructions told of notes, and this goes red.
func TestAClaudeCodeOlderThanItsNoteFloorStartsWithoutTheHooks(t *testing.T) {
	t.Parallel()
	p := codingagent.Paths{Home: "/box"}
	script := scriptOf(t, claude().Command(sandbox.RunRequest{Brief: "x", System: "be careful",
		Steer: steerWiring}, p, ""))
	standIn := `#!/bin/sh
if [ "$1" = --version ]; then printf '%s' "$FAKE_VERSION"; exit 0; fi
for a in "$@"; do printf '%s\n' "$a"; done > "$FAKE_DIR/argv"
`
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for _, tc := range []struct {
			version  string
			settings bool
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
					t.Fatalf("the CLI never started: %v\n%s", err, stderr.String())
				}
				args := strings.Split(strings.TrimSpace(string(raw)), "\n")
				named := slices.Contains(args, "--settings")
				if named != tc.settings {
					t.Errorf("on %q the CLI was started with --settings: %v, want %v (argv %q)",
						tc.version, named, tc.settings, args)
				}
				if named && args[slices.Index(args, "--settings")+1] != p.ClaudeSettings() {
					t.Errorf("--settings names %q, want %q", args[slices.Index(args, "--settings")+1],
						p.ClaudeSettings())
				}
				if said := strings.Contains(stderr.String(), "older than 2.1.163"); said == tc.settings {
					t.Errorf("on %q the run's error stream said it ran without its hooks: %v, want %v\n%s",
						tc.version, said, !tc.settings, stderr.String())
				}
				// ONE SYSTEM TEXT, the one the hooks decide: told of notes
				// where they are read, as it stands where they are not.
				want := p.System()
				if tc.settings {
					want = p.ClaudeNotesSystem()
				}
				if n := strings.Count(strings.Join(args, "\n"), "--append-system-prompt-file"); n != 1 ||
					after(args, "--append-system-prompt-file") != want {
					t.Errorf("on %q the CLI was handed %d system texts, %q, want %q alone (argv %q)",
						tc.version, n, after(args, "--append-system-prompt-file"), want, args)
				}
			})
		}
	})
}
