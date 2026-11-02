package executor_test

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// The run that continues a turn's session, on a real box and under the run's
// own script: which brief its harness reads, what is restored over what, the
// transcript cut back to its last whole record, and the environment a
// continued session runs in.

// briefHarness is [sessionHarness] that also records the brief it was handed
// on its stdin, in the box's home.
func briefHarness(release string) string {
	return strings.Replace(sessionHarness(release), "for a; do printf", `cat > "$HOME/stdin"`+"\nfor a; do printf", 1)
}

// briefBox is a real box with a stand-in `claude` of release that records its
// brief ([briefHarness]).
func briefBox(t *testing.T, release string) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	runner, box := realBox(t)
	writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/claude", briefHarness(release))
	return runner, box
}

// A RUN THAT CANNOT CONTINUE ITS SESSION IS HANDED THE FRESH BRIEF. Whether a
// session is taken up is decided in the box, by its script — the session may be
// neither held there nor restorable — so a run asked to continue one carries
// the brief a session of its own needs beside the one that continues: its
// harness reads the continuation where it takes the session up, and the fresh
// brief where its script started a new session in the session's place, so a
// new session never answers a brief written for a model that remembers.
//
// Mutation: hand the harness the continuation whatever its restore came to, or
// the fresh brief where it continued, and a row goes red.
func TestARunThatCannotContinueItsSessionIsHandedTheFreshBrief(t *testing.T) {
	t.Parallel()
	runner, box := briefBox(t, "2.1.310")
	_, first := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "begin"})
	for _, tc := range []struct {
		name, resume string
		continued    bool
		want         string
	}{
		{name: "its session held", resume: first.SessionID, continued: true, want: "carry on where you stopped"},
		{name: "a session not in it", resume: uuid.NewString(), want: "the task, from the top"},
	} {
		_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "carry on where you stopped",
			FreshBrief: "the task, from the top", Resume: tc.resume})
		if res.Continued != tc.continued || (!tc.continued && res.FellBack == "") {
			t.Fatalf("%s: the run continued %v (fell back: %q), want %v", tc.name, res.Continued, res.FellBack,
				tc.continued)
		}
		if got := boxFile(t, box, box.Home()+"/stdin"); got != tc.want {
			t.Errorf("%s: the harness read %q, want %q", tc.name, got, tc.want)
		}
	}
}

// AN OPENCODE RUN THAT CANNOT CONTINUE ITS SESSION READS THE FRESH BRIEF, under
// every sh a box may run its script with: `opencode run` reads the brief the
// script points its stdin at, the continuation where the box holds the session
// and the fresh brief where the script starts the session minted in its place.
//
// Mutation: leave the script's stdin on the continuation after it starts anew,
// and the fallback row goes red.
func TestAnOpenCodeRunThatCannotContinueItsSessionReadsTheFreshBrief(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for name, tc := range map[string]struct {
			held []string
			want string
		}{
			"the session held":    {held: []string{carriedOpenCode}, want: `fix "the" flake`},
			"nothing to continue": {want: "the task, from the top"},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				run := runScript(t, scriptOpts{shell: sh, session: &continuingOpenCode, held: tc.held,
					freshBrief: "the task, from the top",
					req:        sandbox.RunRequest{Resume: carriedOpenCode, MCPServers: bridgeOnly}})
				if run.code != 0 {
					t.Fatalf("exit %d:\n%s", run.code, run.stderr)
				}
				if got := readIn(t, run.dir, "brief"); got != tc.want {
					t.Errorf("`opencode run` read %q, want %q", got, tc.want)
				}
			})
		}
	})
}

// A SESSION ITS HARNESS WAS CUT OFF IN IS RESTORED OVER THE BOX'S OWN COPY. The
// capture of a job that did not exit cleanly was packed by the engine, and the
// box's copy may hold what its harness never finished — or what it went on to
// do after the capture, which the continuation lists instead — so the archive
// replaces the copy even where the box holds the archive's digest; a capture
// taken at a clean exit keeps a copy that holds it.
//
// Mutation: keep a copy that holds the archive whatever was asked, and the
// box's own line survives.
func TestACutOffSessionIsRestoredOverTheBoxsOwnCopy(t *testing.T) {
	t.Parallel()
	runner, box := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "begin"})
	described, body, err := runner.OpenCapture(t.Context(), box)
	if err != nil || body == nil {
		t.Fatalf("OpenCapture = %v, %v", body, err)
	}
	archive, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		t.Fatal(err)
	}
	captured := boxFile(t, box, transcriptOf(box, ran.SessionID))
	own := captured + `{"type":"user","line":"after the capture"}` + "\n"
	if err := box.WriteFile(t.Context(), transcriptOf(box, ran.SessionID), []byte(own)); err != nil {
		t.Fatal(err)
	}
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
		Carried: &sandbox.CarriedSession{Archive: described, Body: strings.NewReader(string(archive)),
			Replace: true}})
	if !res.Continued {
		t.Fatalf("the run did not continue the session restored over its box's copy:\n%s",
			boxFile(t, box, paths(box).Err()))
	}
	got := boxFile(t, box, transcriptOf(box, ran.SessionID))
	if strings.Contains(got, "after the capture") || !strings.HasPrefix(got, captured) {
		t.Errorf("the box's own copy survived the archive that replaces it:\n%s", got)
	}
}

// AN OPENCODE SESSION ITS RUN WAS CUT OFF IN IS IMPORTED OVER THE BOX'S COPY,
// under every sh: the copy is deleted and the export imported though the box
// keeps the export's digest — an export holds only settled messages, and the
// copy may hold a step that never settled.
//
// Mutation: keep a copy that holds the export whatever was asked, and the
// import never happens.
func TestACutOffOpenCodeSessionIsImportedOverTheBoxsCopy(t *testing.T) {
	t.Parallel()
	replacing := continuingOpenCode
	replacing.Replaces = true
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, session: &replacing, held: []string{carriedOpenCode},
			carried: openCodeExport(carriedOpenCode), ownsCarried: true,
			req: sandbox.RunRequest{Resume: carriedOpenCode, MCPServers: bridgeOnly}})
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		deleted, imported := run.callIndex("session delete"), run.callIndex("session import --standalone")
		if deleted < 0 || imported < deleted {
			t.Fatalf("the box's copy was not replaced by the export:\n%s", strings.Join(run.calls, "\n"))
		}
		if got := sessionOf(t, run); got != carriedOpenCode {
			t.Errorf("the run worked in %q, want the restored %q", got, carriedOpenCode)
		}
	})
}

// A TRANSCRIPT CUT OFF MID-LINE IS CUT BACK TO ITS LAST WHOLE RECORD BEFORE IT
// IS RESUMED. A harness killed while it appended leaves a line with no newline,
// which no reader of a JSON-lines transcript can use; resumed as it is, the
// next record would be written onto its end. The script cuts it, and a
// transcript that ends whole is left as it is.
//
// Mutation: resume the transcript as the box left it, and the torn line is
// welded to the next run's first record.
func TestATranscriptCutOffMidLineIsCutBackBeforeItIsResumed(t *testing.T) {
	t.Parallel()
	runner, box := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "begin"})
	whole := boxFile(t, box, transcriptOf(box, ran.SessionID))
	if err := box.WriteFile(t.Context(), transcriptOf(box, ran.SessionID),
		[]byte(whole+`{"type":"assistant","message":{"content":[{"type":"tool_us`)); err != nil {
		t.Fatal(err)
	}
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID})
	if !res.Continued {
		t.Fatalf("the run did not continue its session:\n%s", boxFile(t, box, paths(box).Err()))
	}
	got := boxFile(t, box, transcriptOf(box, ran.SessionID))
	if strings.Contains(got, "tool_us") || !strings.HasPrefix(got, whole) ||
		strings.Count(got, "\n") != strings.Count(whole, "\n")+1 {
		t.Errorf("the torn line was resumed as it was:\n%s", got)
	}
}

// A CONTINUED CLAUDE CODE SESSION RUNS UNDER THE ENGINE'S WINDOW AND LEAVES ITS
// INTERRUPTED TURN AS IT IS. Every run compacts at the engine's window
// (sandbox.ExecutorContextWindow), whatever the seat's environment set it to —
// a window of its own, or compaction switched off altogether
// (DISABLE_AUTO_COMPACT, DISABLE_COMPACT) — so no request of it, or of a run
// that continues it, carries more; and a resumed run is started without
// CLAUDE_CODE_RESUME_INTERRUPTED_TURN, which would have it run the turn its
// last process was cut off in again before it reads its brief.
//
// Mutation: drop the window from the pins or a switch from the unsets, or
// leave the variable set on a resume, and the harness's environment shows it.
func TestAContinuedClaudeSessionRunsUnderTheEnginesWindow(t *testing.T) {
	t.Parallel()
	runner, box := sessionBox(t, "2.1.310")
	seat := map[string]string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "1000000", "CLAUDE_CODE_RESUME_INTERRUPTED_TURN": "1",
		"DISABLE_AUTO_COMPACT": "1", "DISABLE_COMPACT": "1"}
	_, ran := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "begin", Env: seat})
	_, res := runToEnd(t, runner, box, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID, Env: seat})
	if !res.Continued {
		t.Fatalf("the run did not continue its session:\n%s", boxFile(t, box, paths(box).Err()))
	}
	env := strings.Split(boxFile(t, box, box.Home()+"/env"), "\n")
	if want := "CLAUDE_CODE_AUTO_COMPACT_WINDOW=" + strconv.Itoa(sandbox.ExecutorContextWindow); !slices.Contains(env, want) {
		t.Errorf("the harness's environment lacks %s, the seat's own value replaced", want)
	}
	if slices.ContainsFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, "CLAUDE_CODE_RESUME_INTERRUPTED_TURN=")
	}) {
		t.Error("a resumed run kept CLAUDE_CODE_RESUME_INTERRUPTED_TURN")
	}
	for _, off := range []string{"DISABLE_AUTO_COMPACT=", "DISABLE_COMPACT="} {
		if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, off) }) {
			t.Errorf("the harness's environment keeps %s1, which switches its compaction off", off)
		}
	}
}

// usageHarness is a stand-in `claude` that files a session whose transcript
// records two requests' usage on its main thread, then one on a subagent's
// sidechain and a later assistant message that recorded none — each after the
// main thread's last — and a person's message quoting the words a usage line
// is found by.
func usageHarness() string {
	return `#!/bin/sh
if [ "$1" = --version ]; then echo '2.1.310 (Claude Code)'; exit 0; fi
s= prev=; for a; do case "$prev" in --session-id|--resume) s=$a;; esac; prev=$a; done
p=$CLAUDE_CONFIG_DIR/projects/$CLAUDE_CODE_PROJECT_DIR_NAME
mkdir -p "$p"
cat >> "$p/$s.jsonl" <<'EOF'
{"type":"user","message":{"content":"begin"}}
{"type":"assistant","message":{"usage":{"input_tokens":10,"cache_creation_input_tokens":200,"cache_read_input_tokens":3000,"output_tokens":7}}}
{"type":"user","message":{"content":"a tool's answer quoting \"usage\" and \"assistant\""}}
{"type":"assistant","message":{"usage":{"input_tokens":20,"cache_creation_input_tokens":300,"cache_read_input_tokens":4000,"output_tokens":9}}}
{"type":"assistant","isSidechain":true,"message":{"usage":{"input_tokens":99999,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}
EOF
printf '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"%s"}\n' "$s"
`
}

// WHAT A SESSION'S LAST REQUEST CARRIED IS READ OFF ITS ARCHIVE AS IT IS KEPT —
// the context a launch that continues it sends again, which that launch's
// floor is made of. Claude Code's: the whole input of the last assistant
// message on the main thread that recorded its usage, cache reads and writes
// included — never a subagent's, never a later message that recorded none.
// OpenCode's: the last settled assistant message that measured a prompt,
// never one that failed.
//
// Mutation: read the first usage rather than the last, count a sidechain or a
// failed message, or leave the cache out, and a figure goes red.
func TestTheContextASessionCarriesIsReadOffItsArchive(t *testing.T) {
	t.Parallel()
	t.Run("claude-code", func(t *testing.T) {
		t.Parallel()
		runner, b := realBox(t)
		writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/claude", usageHarness())
		if _, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"}); res.Captured == nil {
			t.Fatalf("the run's session was not captured:\n%s", boxFile(t, b, paths(b).Err()))
		}
		taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{MaxBytes: takeBound})
		if err != nil || taken.Body == nil || taken.Context == nil {
			t.Fatalf("TakeSession = %+v, %v", taken, err)
		}
		if _, err := io.ReadAll(taken.Body); err != nil {
			t.Fatal(err)
		}
		_ = taken.Body.Close()
		if got := taken.Context(); got != 20+300+4000 {
			t.Errorf("the session's last request carried %d tokens, want 4320", got)
		}
	})
	t.Run("opencode", func(t *testing.T) {
		t.Parallel()
		runner, b := openCodeBox(t, t.TempDir())
		dir := t.TempDir()
		writeBoxStandIn(t, b, paths(b).BinDir()+"/opencode",
			strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`, "d="+shQuote(dir), 1))
		const session = "ses_0123456789abcdef0123456789"
		if err := b.WriteFile(t.Context(), paths(b).SessionRecord(),
			[]byte("id "+session+"\nrelease 2.0.32\n")); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "sessions", session), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		messages := `[{"id":"msg_1","type":"user","text":"x"},` +
			`{"id":"msg_2","type":"assistant","tokens":{"input":10,"output":5,"reasoning":0,"cache":{"read":1000,"write":50}}},` +
			`{"id":"msg_3","type":"assistant","error":null,"tokens":{"input":20,"output":9,"cache":{"read":2000,"write":80}}},` +
			`{"id":"msg_4","type":"assistant","error":{"name":"APIError"},"tokens":{"input":999999,"cache":{"read":0,"write":0}}},` +
			`{"id":"msg_5","type":"user","text":"a later message"}]`
		if err := os.WriteFile(filepath.Join(dir, "export-messages"), []byte(messages), 0o600); err != nil {
			t.Fatal(err)
		}
		taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Pack: true, MaxBytes: takeBound})
		if err != nil || taken.Body == nil || taken.Context == nil {
			t.Fatalf("TakeSession = %+v, %v", taken, err)
		}
		if _, err := io.ReadAll(taken.Body); err != nil {
			t.Fatal(err)
		}
		_ = taken.Body.Close()
		if got := taken.Context(); got != 20+2000+80 {
			t.Errorf("the session's last request carried %d tokens, want 2100", got)
		}
	})
}

// A RUNNER RESTORES ONLY WHAT IT COULD CONTINUE, decided without a box: a
// session its own harness wrote, in the one format it packs, by a release whose
// sessions another box continues — the answer every reader of whether a session
// continues takes beside the launch, before the box's own release is asked.
//
// Mutation: drop the writer's floor, or answer for another harness's session,
// and a row goes red.
func TestARunnerRestoresOnlyWhatItCouldContinue(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		runner   *executor.Runner
		archive  sandbox.SessionArchive
		restores bool
		why      string
	}{
		"claude's own": {executor.NewClaudeCode(), sandbox.SessionArchive{Harness: executor.ClaudeCodeName,
			Format: executor.ClaudeArchiveFormat, Version: "2.1.310"}, true, ""},
		"claude, a writer too old": {executor.NewClaudeCode(), sandbox.SessionArchive{Harness: executor.ClaudeCodeName,
			Format: executor.ClaudeArchiveFormat, Version: "2.1.299"}, false, "older than 2.1.300"},
		"claude, opencode's session": {executor.NewClaudeCode(), sandbox.SessionArchive{Harness: executor.OpenCodeName,
			Format: executor.OpenCodeArchiveFormat, Version: "2.0.32"}, false, "continued only by the harness that wrote it"},
		"opencode's own": {executor.NewOpenCode(), sandbox.SessionArchive{Harness: executor.OpenCodeName,
			Format: executor.OpenCodeArchiveFormat, Version: "2.2.5"}, true, ""},
		"opencode, another format": {executor.NewOpenCode(), sandbox.SessionArchive{Harness: executor.OpenCodeName,
			Format: executor.OpenCodeArchiveFormat + 1, Version: "2.2.5"}, false, "format"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			restores, why := tc.runner.Restores(tc.archive)
			if restores != tc.restores || !strings.Contains(why, tc.why) || (restores && why != "") {
				t.Errorf("Restores = %v, %q; want %v saying %q", restores, why, tc.restores, tc.why)
			}
		})
	}
}

// A CONTINUED OPENCODE RUN IS CONFIGURED WITH ITS OWN INSTRUCTIONS, as a fresh
// one is. OpenCode keeps, per session, the instructions it last told the model,
// and an import writes none of that state (session/transfer.ts at 2.0.32 and
// 2.2.5), so the first request of a session carried into a box reads the run's
// instruction file as the session's first: its system prompt is this run's
// standing instructions, and no earlier launch's ride the conversation as
// updates. What holds that on the engine's side is that a run continuing a
// session is configured exactly as one starting one: its instruction file
// named, and written afresh with its own instructions.
//
// Mutation: configure a run that continues a session without its instruction
// file, or leave an earlier run's in place, and this goes red.
func TestAContinuedOpenCodeRunIsConfiguredWithItsOwnInstructions(t *testing.T) {
	t.Parallel()
	_, box := openCodeBox(t, t.TempDir())
	p := executor.PathsFor(box)
	configure := func(req sandbox.RunRequest) string {
		t.Helper()
		path, err := executor.OpenCode{}.WriteConfig(t.Context(), box, req, p)
		if err != nil {
			t.Fatalf("WriteConfig: %v", err)
		}
		return boxFile(t, box, path)
	}
	fresh := configure(sandbox.RunRequest{System: "the first launch's instructions"})
	continued := configure(sandbox.RunRequest{System: "this launch's instructions", Resume: carriedOpenCode,
		Carried: &sandbox.CarriedSession{Archive: sandbox.SessionArchive{Harness: executor.OpenCodeName,
			SessionID: carriedOpenCode, Format: executor.OpenCodeArchiveFormat, Version: "2.0.32"},
			Body: strings.NewReader(openCodeExport(carriedOpenCode))}})
	if continued != fresh || !strings.Contains(continued, `"instructions"`) || !strings.Contains(continued, p.System()) {
		t.Errorf("the continued run is configured\n%s\nwant the fresh run's configuration, naming %s:\n%s",
			continued, p.System(), fresh)
	}
	if got := boxFile(t, box, p.System()); got != "this launch's instructions" {
		t.Errorf("the continued run's instruction file holds %q, want its own instructions", got)
	}
}
