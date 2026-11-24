package executor_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// sessionHarness is a stand-in `claude` of release for a run's session cases in a
// real box: it answers `--version`, records its argv and environment in the box's
// home, and files its session where the pinned layout names (transcript, sidecar
// directory, task list) beside what a capture must never take: file-edit
// snapshots, the harness's own state, a login, auto memory and another session.
// It refuses to resume a session not filed in some project directory, as Claude
// Code does from 2.1.223 ("No conversation found"), and ends on a success naming
// its session.
func sessionHarness(release string) string {
	return `#!/bin/sh
if [ "$1" = --version ]; then echo '` + release + ` (Claude Code)'; exit 0; fi
for a; do printf '%s\n' "$a"; done > "$HOME/argv"
env > "$HOME/env"
s= prev=; for a; do case "$prev" in --session-id|--resume) s=$a;; esac; prev=$a; done
cfg=$CLAUDE_CONFIG_DIR p=$CLAUDE_CONFIG_DIR/projects/$CLAUDE_CODE_PROJECT_DIR_NAME
found=; for f in "$cfg"/projects/*/"$s".jsonl; do [ -s "$f" ] && found=1; done
case " $* " in *" --resume "*) [ -n "$found" ] || { echo "No conversation found with session ID: $s" >&2; exit 1; };; esac
mkdir -p "$p/$s/subagents" "$p/$s/tool-results" "$cfg/tasks/$s" "$cfg/file-history/$s" "$p/memory"
printf '{"type":"user","sessionId":"%s"}\n' "$s" >> "$p/$s.jsonl"
echo subagent > "$p/$s/subagents/agent-1.jsonl"
echo spilled > "$p/$s/tool-results/out.txt"
echo task > "$cfg/tasks/$s/1.json"
echo snapshot > "$cfg/file-history/$s/edit"
echo '{"state":"the harness'\''s own"}' > "$cfg/.claude.json"
echo '{"token":"a login"}' > "$cfg/.credentials.json"
echo remembered > "$p/memory/MEMORY.md"
echo '{"type":"user"}' > "$p/another-session.jsonl"
printf '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"%s"}\n' "$s"
`
}

// sessionBox is a real box with the runner's plumbing and a stand-in `claude`
// of release in it ([sessionHarness]).
func sessionBox(t *testing.T, release string) (*executor.Runner, sandbox.Sandbox) {
	t.Helper()
	runner, box := realBox(t)
	writeBoxStandIn(t, box, executor.PathsFor(box).BinDir()+"/claude", sessionHarness(release))
	return runner, box
}

// runToEnd starts req in box, waits for its run to end and collects it.
func runToEnd(t *testing.T, runner *executor.Runner, box sandbox.Sandbox,
	req sandbox.RunRequest) (sandbox.RunHandle, sandbox.Result) {
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
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	res, err := runner.Collect(t.Context(), box, handle, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return handle, res
}

// boxFile is a file in box, read whole; "" where there is none.
func boxFile(t *testing.T, box sandbox.Sandbox, path string) string {
	t.Helper()
	raw, err := box.ReadFile(t.Context(), path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(raw)
}

// argvOf is the argv the stand-in recorded in box.
func argvOf(t *testing.T, box sandbox.Sandbox) []string {
	t.Helper()
	return strings.Split(strings.TrimSuffix(boxFile(t, box, box.Home()+"/argv"), "\n"), "\n")
}

// transcriptOf is where the pinned layout puts session's transcript in box.
func transcriptOf(box sandbox.Sandbox, session string) string {
	return executor.PathsFor(box).ClaudeConfigDir() + "/projects/" + executor.ClaudeProjectDir + "/" +
		session + ".jsonl"
}

// THE WRAPPER PINS WHERE CLAUDE CODE KEEPS ITS SESSIONS, over whatever set it:
// CLAUDE_CONFIG_DIR (the box home's `.claude`), CLAUDE_CODE_PROJECT_DIR_NAME (one
// constant for every run) and auto memory off, exported inside the login shell
// after the unsets of the variables that switch hooks off and before the harness,
// so no value from the seat's environment, the image or a profile survives.
//
// Mutation: export the pins before the unsets, or drop one, and the order or the
// seat's value reaches the harness; export them outside the shell, and a profile
// replaces them.
func TestTheWrapperPinsClaudeCodesSessionLayout(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	script := startAndScript(t, runner, b, sandbox.RunRequest{Brief: "x"})
	unset := strings.Index(script, "unset CLAUDE_CODE_SIMPLE CLAUDE_CODE_SAFE_MODE CLAUDE_CODE_REMOTE "+
		"DISABLE_AUTO_COMPACT DISABLE_COMPACT CLAUDE_AUTOCOMPACT_PCT_OVERRIDE CLAUDE_CODE_BLOCKING_LIMIT_OVERRIDE; ")
	pinned := strings.Index(script, "export CLAUDE_CONFIG_DIR=")
	start := strings.Index(script, "claude -p ")
	if unset < 0 || pinned < unset || start < pinned {
		t.Fatalf("the pins are not exported after the unsets and before the harness "+
			"(unset at %d, export at %d, harness at %d):\n%s", unset, pinned, start, script)
	}
	for _, want := range []string{p.ClaudeConfigDir(), "CLAUDE_CODE_PROJECT_DIR_NAME=",
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY="} {
		if !strings.Contains(script[pinned:start], want) {
			t.Errorf("the pins lack %q:\n%s", want, script[pinned:start])
		}
	}
	oc := executor.NewOpenCode()
	if script := startAndScript(t, oc, box(t, oc), sandbox.RunRequest{Brief: "x"}); strings.Contains(script, "CLAUDE_") {
		t.Errorf("an OpenCode run pins Claude Code's variables:\n%s", script)
	}

	pinRunner, pinBox := sessionBox(t, "2.1.310")
	runToEnd(t, pinRunner, pinBox, sandbox.RunRequest{Brief: "x", Env: map[string]string{
		"CLAUDE_CONFIG_DIR": "/elsewhere", "CLAUDE_CODE_PROJECT_DIR_NAME": "theirs",
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY": "0",
	}})
	env := strings.Split(boxFile(t, pinBox, pinBox.Home()+"/env"), "\n")
	for _, want := range []string{
		"CLAUDE_CONFIG_DIR=" + executor.PathsFor(pinBox).ClaudeConfigDir(),
		"CLAUDE_CODE_PROJECT_DIR_NAME=" + executor.ClaudeProjectDir,
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("the harness's environment lacks %s, the seat's own value replaced", want)
		}
	}
}

// THE PROJECT DIRECTORY'S NAME IS ONE CLAUDE CODE TAKES: it holds the name to
// `^[A-Za-z0-9_-]{1,64}$` (2.1.316) and ignores one past it, filing every
// session under the box's working directory instead.
func TestTheProjectDirectoryIsANameClaudeCodeTakes(t *testing.T) {
	t.Parallel()
	name := executor.ClaudeProjectDir
	if name == "" || len(name) > 64 || strings.ContainsFunc(name, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-'
	}) {
		t.Fatalf("the project directory %q is not a name Claude Code takes", name)
	}
}

// A FRESH RUN STARTS UNDER THE SESSION ITS START NAMED, AND A CONTINUED ONE
// RESUMES ITS OWN — NEVER BOTH. A fresh run's id is minted before the harness
// starts and returned by Start, so a run killed before its result line still
// names its session; a continuing run passes `--resume` alone, as the harness
// refuses `--session-id` beside it unless forked. A held session is resumed
// under the pinned project directory or an older release's, named after the
// working directory; an unheld one never is: the run starts its start's fresh
// session, and its report and error stream say it did not continue and why.
//
// Mutation: pass --session-id beside --resume, resume an unheld session, look
// only under the pinned directory, report no reason, or name a fresh session in
// the handle other than the fallback's, and a row goes red.
func TestAFreshRunNamesItsSessionAndAContinuedOneResumesIt(t *testing.T) {
	t.Parallel()
	const asked = "0f1e2d3c-4b5a-4968-8776-655443322110"
	for name, tc := range map[string]struct {
		resume   string
		filed    string // where the box holds the asked session, under projects/
		resumed  bool
		fellBack string
	}{
		"fresh":           {},
		"continued, held": {resume: asked, filed: executor.ClaudeProjectDir, resumed: true},
		"continued, held where a release before the pin filed it": {resume: asked, filed: "-home-user-workspace", resumed: true},
		"continued, held nowhere":                                 {resume: asked, fellBack: "it is not in this box"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, b := sessionBox(t, "2.1.310")
			if tc.filed != "" {
				held := executor.PathsFor(b).ClaudeConfigDir() + "/projects/" + tc.filed + "/" + asked + ".jsonl"
				if err := b.WriteFile(t.Context(), held, []byte(`{"type":"user"}`+"\n")); err != nil {
					t.Fatal(err)
				}
			}
			handle, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x", Resume: tc.resume})
			argv := argvOf(t, b)
			resumes, starts := after(argv, "--resume"), after(argv, "--session-id")
			if resumes != "" && starts != "" {
				t.Fatalf("the harness was started with both --resume and --session-id: %q", argv)
			}
			switch {
			case tc.resumed:
				if resumes != asked || !res.Continued || res.SessionID != asked {
					t.Errorf("resumed %q (continued %v, session %q), want %s resumed", resumes, res.Continued,
						res.SessionID, asked)
				}
			default:
				if starts == "" || starts == asked || res.Continued || res.SessionID != starts {
					t.Errorf("started under %q (continued %v, session %q), want a session of its own",
						starts, res.Continued, res.SessionID)
				}
			}
			if tc.resume == "" && handle.SessionID != starts {
				t.Errorf("Start named the session %q, and the harness was started under %q", handle.SessionID, starts)
			}
			if tc.resume != "" && handle.SessionID != asked {
				t.Errorf("Start named the session %q, want the one it was asked to continue, %q",
					handle.SessionID, asked)
			}
			// AND THE FRESH SESSION only a continuing run's start names: the
			// one a run that fell back was started under.
			switch {
			case tc.resume == "" && handle.FreshSessionID != "":
				t.Errorf("a fresh run's start named a second session, %q", handle.FreshSessionID)
			case tc.resume != "" && (handle.FreshSessionID == "" || handle.FreshSessionID == asked):
				t.Errorf("a continuing run's start named %q as the session it starts of its own", handle.FreshSessionID)
			case tc.fellBack != "" && starts != handle.FreshSessionID:
				t.Errorf("the run fell back to %q, and its start named %q as the session it starts of its own",
					starts, handle.FreshSessionID)
			}
			stderr := boxFile(t, b, paths(b).Err())
			if said := strings.Contains(stderr, "starts a new session"); said != (tc.fellBack != "") ||
				!strings.Contains(stderr, tc.fellBack) {
				t.Errorf("the error stream said it started a new session: %v, want %v (%q):\n%s", said,
					tc.fellBack != "", tc.fellBack, stderr)
			}
			if (res.FellBack != "") != (tc.fellBack != "") || !strings.Contains(res.FellBack, tc.fellBack) {
				t.Errorf("the run reports it fell back because %q, want %q", res.FellBack, tc.fellBack)
			}
			if res.HarnessVersion != "2.1.310" {
				t.Errorf("the run's release is %q, want the box's 2.1.310", res.HarnessVersion)
			}
		})
	}
}

// A RESUMED RUN RENDERS ITS SYSTEM PROMPT AFRESH, FROM THE RELEASE THAT TAKES
// THE OPTION. Otherwise a conversation resends its first request's prompt on
// every resume until compacted, whatever a later launch appends, so every
// resumed run, the same box's too, gets `--system-prompt-snapshot off`: from
// 2.1.267 alone, as 2.1.263 rejects it. A fresh run, recording its own, goes
// without. The transcript's trim is gated likewise: on from 2.1.304, whose trim
// loses nothing, and UNSET before it whatever the seat set. A version not three
// parts reads as current, as the engine reads it. Under every sh a box may have.
//
// Mutation: pass the option whatever the release, or only on a carried session,
// and the 2.1.263 or same-box rows go red; export the trim whatever the release,
// and the 2.1.303 row does; read the parts once cut, and the two-part row reads old.
func TestASnapshotIsTurnedOffOnEveryResumedRunFromTheReleaseThatTakesIt(t *testing.T) {
	t.Parallel()
	const session = "4b8e1f20-9c1d-4e5f-8a7b-6c5d4e3f2a10"
	standIn := `#!/bin/sh
if [ "$1" = --version ]; then printf '%s' "$FAKE_VERSION"; exit 0; fi
for a in "$@"; do printf '%s\n' "$a"; done > "$FAKE_DIR/argv"
printf '%s' "${CLAUDE_CODE_TRANSCRIPT_LOCAL_GC-unset}" > "$FAKE_DIR/gc"
`
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for _, tc := range []struct {
			version         string
			resumed         bool
			snapshotOff, gc bool
		}{
			{"2.1.263 (Claude Code)", true, false, false},
			{"2.1.266 (Claude Code)", true, false, false},
			{"2.1.267 (Claude Code)", true, true, false},
			{"2.1.303 (Claude Code)", true, true, false},
			{"2.1.304 (Claude Code)", true, true, true},
			{"2.1.310 (Claude Code)", true, true, true},
			{"Claude Code, a version of its own", true, true, true},
			{"2.1 (Claude Code)", true, true, true},
			{"2.1.310 (Claude Code)", false, false, true},
			{"2.1.263 (Claude Code)", false, false, false},
		} {
			t.Run(tc.version+map[bool]string{true: " resumed", false: " fresh"}[tc.resumed], func(t *testing.T) {
				dir := t.TempDir()
				bin := filepath.Join(dir, "bin")
				p := executor.Paths{Home: filepath.Join(dir, "home")}
				// The box holds the session a resumed run continues.
				transcript := p.ClaudeConfigDir() + "/projects/" + executor.ClaudeProjectDir + "/" + session + ".jsonl"
				for _, d := range []string{bin, p.WorkDir(), filepath.Dir(transcript)} {
					if err := os.MkdirAll(d, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(transcript, []byte(`{"type":"user"}`+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				writeStandIn(t, filepath.Join(bin, "claude"), standIn)
				req := sandbox.RunRequest{Brief: "x"}
				if tc.resumed {
					req.Resume = session
				}
				script := scriptOf(t, commandOf(claude(), req, p, ""))
				argv := sh.argv
				if len(argv) == 0 {
					argv = []string{"sh"}
				}
				cmd := exec.Command(argv[0], append(append([]string{}, argv[1:]...), "-c", sh.prelude+script)...) //nolint:gosec // the script under test
				cmd.Env = []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "FAKE_DIR=" + dir,
					"FAKE_VERSION=" + tc.version, "CLAUDE_CODE_TRANSCRIPT_LOCAL_GC=1"}
				var stderr strings.Builder
				cmd.Stdin, cmd.Stderr = strings.NewReader("x"), &stderr
				if err := cmd.Run(); err != nil {
					t.Fatalf("the script: %v\n%s", err, stderr.String())
				}
				args := lines(t, filepath.Join(dir, "argv"))
				off := slices.Contains(args, "--system-prompt-snapshot") &&
					after(args, "--system-prompt-snapshot") == "off"
				if off != tc.snapshotOff {
					t.Errorf("on %q the harness was started with --system-prompt-snapshot off: %v, want %v (argv %q)",
						tc.version, off, tc.snapshotOff, args)
				}
				if got := after(args, "--resume") == session; got != tc.resumed {
					t.Errorf("on %q the harness resumed the session: %v, want %v (argv %q)", tc.version, got,
						tc.resumed, args)
				}
				gc, err := os.ReadFile(filepath.Join(dir, "gc")) //nolint:gosec // under the case's own dir
				if err != nil {
					t.Fatal(err)
				}
				if on := string(gc) == "1"; on != tc.gc {
					t.Errorf("on %q the transcript's trim reached the harness as %q, want on: %v", tc.version, gc, tc.gc)
				}
			})
		}
	})
}

// archiveMembers is every member of a gzipped tar and its content, by name.
func archiveMembers(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	zr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("the capture is not gzip: %v", err)
	}
	tr := tar.NewReader(zr)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("reading the capture's members: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", h.Name, err)
		}
		out[strings.TrimPrefix(h.Name, "./")] = string(body)
	}
}

// THE CAPTURE IS THE SESSION AND NOTHING ELSE. Once the harness exits, the run
// packs its session into one gzipped tar, moved into place whole: transcript,
// sidecar directory (subagents' transcripts, spilled tool results) and task list,
// named relative to the harness's configuration directory, never the login, the
// harness's state, file-edit snapshots, auto memory or another session. The
// collection is told its path, session, writing release and size, and
// OpenCapture streams it.
//
// Mutation: pack the configuration or project directory, and the login, the
// state or the other session is in it; drop the task list or the sidecars, and
// the session comes back without them.
func TestTheCapturePacksTheSessionAndNothingElse(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	if res.Captured == nil {
		t.Fatalf("the run's session was not captured:\n%s", boxFile(t, b, paths(b).Err()))
	}
	described, body, err := runner.OpenCapture(t.Context(), b)
	if err != nil || body == nil {
		t.Fatalf("OpenCapture = %v, %v", body, err)
	}
	defer func() { _ = body.Close() }()
	if described != *res.Captured {
		t.Errorf("OpenCapture describes %+v, and the collection %+v", described, *res.Captured)
	}
	session := res.SessionID
	if described.Harness != executor.ClaudeCodeName || described.SessionID != session ||
		described.Format != executor.ClaudeArchiveFormat || described.Version != "2.1.310" ||
		described.Path != paths(b).SessionCapture() || described.Size <= 0 {
		t.Errorf("the capture is described as %+v, want session %s of Claude Code 2.1.310 at %s",
			described, session, paths(b).SessionCapture())
	}
	members := archiveMembers(t, body)
	project := "projects/" + executor.ClaudeProjectDir + "/"
	for _, want := range []string{project + session + ".jsonl", project + session + "/subagents/agent-1.jsonl",
		project + session + "/tool-results/out.txt", "tasks/" + session + "/1.json"} {
		if _, ok := members[want]; !ok {
			t.Errorf("the capture lacks %s: %v", want, slices.Sorted(maps.Keys(members)))
		}
	}
	for name := range members {
		if !strings.HasPrefix(name, project+session+".jsonl") && !strings.HasPrefix(name, project+session+"/") &&
			!strings.HasPrefix(name, "tasks/"+session+"/") {
			t.Errorf("the capture holds %s, which is no part of the session", name)
		}
	}
	if !strings.Contains(members[project+session+".jsonl"], session) {
		t.Errorf("the capture's transcript is not the session's: %q", members[project+session+".jsonl"])
	}
	// AND THE BOX KEEPS ITS DIGEST, as one its own copy of the session holds.
	capture := []byte(boxFile(t, b, paths(b).SessionCapture()))
	if digests := boxFile(t, b, paths(b).SessionDigests()); !strings.Contains(digests,
		session+" "+digestOf(t, capture)+"\n") {
		t.Errorf("the box keeps no digest of the capture it packed: %q", digests)
	}
}

// A SESSION CARRIED INTO A FRESH BOX IS RESTORED THERE AND RESUMED. One run's
// capture, streamed out of its box, is handed to a run in another box continuing
// the session: written in as the run starts, restored by its script under the
// pinned layout (transcript, sidecars, task list) before the harness starts,
// removed once restored, and resumed with its system prompt rendered afresh; the
// run reports it continued.
//
// Mutation: write the archive before the clear, restore it outside the pinned
// project directory, or resume only a session the box already held, and the
// second run starts a session of its own.
func TestACarriedSessionIsRestoredIntoAFreshBoxAndResumed(t *testing.T) {
	t.Parallel()
	first, a := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, first, a, sandbox.RunRequest{Brief: "begin"})
	described, body, err := first.OpenCapture(t.Context(), a)
	if err != nil || body == nil {
		t.Fatalf("OpenCapture = %v, %v", body, err)
	}
	defer func() { _ = body.Close() }()

	second, b := sessionBox(t, "2.1.310")
	_, res := runToEnd(t, second, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
		Carried: &sandbox.CarriedSession{Archive: described, Body: body}})
	argv := argvOf(t, b)
	if after(argv, "--resume") != ran.SessionID || after(argv, "--system-prompt-snapshot") != "off" {
		t.Fatalf("the carried session was not resumed afresh: argv %q\n%s", argv, boxFile(t, b, paths(b).Err()))
	}
	if !res.Continued || res.SessionID != ran.SessionID {
		t.Errorf("the run reports continued %v in session %q, want %s continued", res.Continued, res.SessionID,
			ran.SessionID)
	}
	config := paths(b).ClaudeConfigDir()
	for _, restored := range []string{
		transcriptOf(b, ran.SessionID),
		config + "/projects/" + executor.ClaudeProjectDir + "/" + ran.SessionID + "/subagents/agent-1.jsonl",
		config + "/tasks/" + ran.SessionID + "/1.json",
	} {
		if boxFile(t, b, restored) == "" {
			t.Errorf("%s was not restored", restored)
		}
	}
	// Two lines: the carried run's, and the one the resumed harness appended.
	if n := strings.Count(boxFile(t, b, transcriptOf(b, ran.SessionID)), "\n"); n != 2 {
		t.Errorf("the resumed transcript has %d lines, want the carried one and the resumed run's", n)
	}
	if left := boxFile(t, b, paths(b).SessionCarried()); left != "" {
		t.Errorf("the carried archive was left in the box after its restore (%d bytes)", len(left))
	}
}

// A BOX WHOSE COPY HOLDS THE CARRIED ARCHIVE IS NEVER WRITTEN OVER. A reused box
// that packed the archive (its digest is among the box's own) and ran on past it
// holds a copy newer than that archive, an older capture carried back after the
// last one failed: the run resumes the box's own copy, keeping every line the
// archive lacks.
//
// Mutation: restore whatever the box holds, or compare by id rather than the
// archive's digest, and the box's own line is lost.
func TestABoxWhoseCopyHoldsTheCarriedArchiveIsNotOverwritten(t *testing.T) {
	t.Parallel()
	first, a := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, first, a, sandbox.RunRequest{Brief: "begin"})
	described, body, err := first.OpenCapture(t.Context(), a)
	if err != nil || body == nil {
		t.Fatalf("OpenCapture = %v, %v", body, err)
	}
	defer func() { _ = body.Close() }()
	// The box goes on with the session past the capture.
	own := boxFile(t, a, transcriptOf(a, ran.SessionID)) + `{"type":"user","line":"the box's own"}` + "\n"
	if err := a.WriteFile(t.Context(), transcriptOf(a, ran.SessionID), []byte(own)); err != nil {
		t.Fatal(err)
	}

	_, res := runToEnd(t, first, a, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
		Carried: &sandbox.CarriedSession{Archive: described, Body: body}})
	if !res.Continued {
		t.Fatalf("the run did not continue the session its box held:\n%s", boxFile(t, a, paths(a).Err()))
	}
	if got := boxFile(t, a, transcriptOf(a, ran.SessionID)); !strings.HasPrefix(got, own) {
		t.Errorf("the box's own session was written over by the carried one:\n%s", got)
	}
}

// A BOX WHOSE COPY DOES NOT HOLD THE CARRIED ARCHIVE HAS IT REPLACED. Box A
// packed the session, box B restored it and ran on, and B's capture is carried
// back into A, whose copy holds only the earlier point: the archive is restored
// over it (transcript, sidecars, task list), the run continues the session as B
// left it, and A keeps the archive's digest as one its copy now holds.
//
// Mutation: keep any copy the box holds, whatever was carried, and the run
// resumes A's older copy, without B's turn.
func TestABoxWhoseCopyDoesNotHoldTheCarriedArchiveHasItReplaced(t *testing.T) {
	t.Parallel()
	runnerA, a := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runnerA, a, sandbox.RunRequest{Brief: "begin"})
	first, firstBody, err := runnerA.OpenCapture(t.Context(), a)
	if err != nil || firstBody == nil {
		t.Fatalf("OpenCapture in A = %v, %v", firstBody, err)
	}
	defer func() { _ = firstBody.Close() }()

	runnerB, b := sessionBox(t, "2.1.310")
	_, went := runToEnd(t, runnerB, b, sandbox.RunRequest{Brief: "go on", Resume: ran.SessionID,
		Carried: &sandbox.CarriedSession{Archive: first, Body: firstBody}})
	if !went.Continued {
		t.Fatalf("B did not continue the session carried to it:\n%s", boxFile(t, b, paths(b).Err()))
	}
	inB := boxFile(t, b, transcriptOf(b, ran.SessionID))
	second, secondBody, err := runnerB.OpenCapture(t.Context(), b)
	if err != nil || secondBody == nil {
		t.Fatalf("OpenCapture in B = %v, %v", secondBody, err)
	}
	defer func() { _ = secondBody.Close() }()

	_, res := runToEnd(t, runnerA, a, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
		Carried: &sandbox.CarriedSession{Archive: second, Body: secondBody}})
	if !res.Continued || res.FellBack != "" {
		t.Fatalf("A did not continue the session carried back to it (fell back: %q):\n%s", res.FellBack,
			boxFile(t, a, paths(a).Err()))
	}
	got := boxFile(t, a, transcriptOf(a, ran.SessionID))
	if !strings.HasPrefix(got, inB) || strings.Count(got, "\n") != strings.Count(inB, "\n")+1 {
		t.Errorf("A's session is not B's carried on by one run:\nA:\n%s\nB:\n%s", got, inB)
	}
	digests := boxFile(t, a, paths(a).SessionDigests())
	if lines := strings.Count(digests, ran.SessionID+" "); lines != 3 {
		t.Errorf("A keeps %d digests of the session, want its own two captures and the archive it "+
			"restored:\n%s", lines, digests)
	}
}

// A CARRIED SESSION THE BOX CANNOT RESTORE IS NEVER RESUMED. A box whose harness
// is below the carry floor or names no release this build reads, and an archive
// holding no transcript, each start the session minted for the run instead, say
// why on the error stream and in the report, and report that the run did not
// continue.
//
// Mutation: resume whatever the restore did, or take an unreadable release (a
// pre-release, as releaseOf reads one) for a current one, and the run
// continues a session no rule was read for; record no reason, and the report
// says nothing of why.
func TestACarriedSessionTheBoxCannotRestoreStartsANewOne(t *testing.T) {
	t.Parallel()
	first, a := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, first, a, sandbox.RunRequest{Brief: "begin"})
	capture := boxFile(t, a, paths(a).SessionCapture())
	described := *ran.Captured

	var empty bytes.Buffer
	zw := gzip.NewWriter(&empty)
	tw := tar.NewWriter(zw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		release, archive, why string
	}{
		"a box older than the floor": {release: "2.1.309", archive: capture, why: "older than 2.1.310"},
		"a box whose release does not read": {release: "nightly", archive: capture,
			why: "named no release this build reads"},
		"a box on a pre-release": {release: "2.1.311-beta.1", archive: capture,
			why: "named no release this build reads"},
		"an archive with no transcript": {release: "2.1.310", archive: empty.String(),
			why: "could not be restored"},
		"an archive that is no archive": {release: "2.1.310", archive: "not a tar", why: "could not be restored"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, b := sessionBox(t, tc.release)
			_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
				Carried: &sandbox.CarriedSession{Archive: described, Body: strings.NewReader(tc.archive)}})
			argv := argvOf(t, b)
			if after(argv, "--resume") != "" || after(argv, "--session-id") == "" {
				t.Fatalf("a session the box could not restore was resumed: argv %q", argv)
			}
			if res.Continued || res.SessionID == ran.SessionID {
				t.Errorf("the run reports continued %v in %q, want a session of its own", res.Continued, res.SessionID)
			}
			if stderr := boxFile(t, b, paths(b).Err()); !strings.Contains(stderr, tc.why) {
				t.Errorf("the error stream does not say %q:\n%s", tc.why, stderr)
			}
			if !strings.Contains(res.FellBack, tc.why) {
				t.Errorf("the run reports it fell back because %q, want %q", res.FellBack, tc.why)
			}
		})
	}
}

// A RUN HANDED A SESSION IT CANNOT RESTORE IS REFUSED BEFORE ANYTHING STARTS:
// another harness's or format's archive, one not the session the run continues,
// and one with no bytes. A run that could only start afresh would be answered as
// if it remembered.
//
// Mutation: drop a check from refuseCarried, and its row starts.
func TestAStartRefusesACarriedSessionItCannotRestore(t *testing.T) {
	t.Parallel()
	good := sandbox.SessionArchive{Harness: executor.ClaudeCodeName, SessionID: "s-1",
		Format: executor.ClaudeArchiveFormat, Version: "2.1.310"}
	for name, tc := range map[string]struct {
		runner *executor.Runner
		req    sandbox.RunRequest
	}{
		"another harness's": {executor.NewOpenCode(), sandbox.RunRequest{Resume: "s-1",
			Carried: &sandbox.CarriedSession{Archive: good, Body: strings.NewReader("x")}}},
		"another format": {executor.NewClaudeCode(), sandbox.RunRequest{Resume: "s-1",
			Carried: &sandbox.CarriedSession{Archive: func() sandbox.SessionArchive {
				a := good
				a.Format++
				return a
			}(), Body: strings.NewReader("x")}}},
		"another session": {executor.NewClaudeCode(), sandbox.RunRequest{Resume: "s-2",
			Carried: &sandbox.CarriedSession{Archive: good, Body: strings.NewReader("x")}}},
		"no session to continue": {executor.NewClaudeCode(), sandbox.RunRequest{
			Carried: &sandbox.CarriedSession{Archive: good, Body: strings.NewReader("x")}}},
		"no bytes": {executor.NewClaudeCode(), sandbox.RunRequest{Resume: "s-1",
			Carried: &sandbox.CarriedSession{Archive: good}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := box(t, tc.runner)
			tc.req.Brief = "x"
			if _, err := tc.runner.Start(t.Context(), b, tc.req); err == nil {
				t.Fatal("a run handed a session it cannot restore started")
			}
			if started := b.Background(); len(started) != 0 {
				t.Errorf("the refused run started %q", started)
			}
		})
	}
}

// versionBox is a fake box whose harness answers a version read with printed, or
// exits code with errText; every command it is asked to run is kept.
func versionBox(printed string, code int, errText string, asked *[]string) *sandbox.FakeSandbox {
	b := sandbox.NewFakeSandbox("box-1")
	b.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		*asked = append(*asked, cmd)
		return sandbox.ExecResult{ExitCode: code, Stdout: printed, Stderr: errText}, nil
	}
	return b
}

// THE CONTINUATION CHECK HOLDS THE BOX HARNESS'S RELEASE TO THE FLOORS: the
// writing harness, this build's format, Claude Code's box at or past 2.1.310
// and no older than the writer, and only a session 2.1.300 or later wrote; box
// and writer exactly OpenCode 2.3.2, Codex 0.171.0, Muse Code 1.4.25, Gemini
// CLI 0.70.0, Qwen Code 0.28.4 and Grok Build 1.0.92. An unread box release or
// writer refuses, for every harness. The printed release is returned whatever
// the answer; a harness not answering is no continuation, a box not asked an
// error; one login-shell exec, its bin first on PATH.
//
// Mutation: compare against the writer's floor alone, let an older box take a
// newer writer's session, read an unread box release or writer as current, or
// let a Gemini or Qwen release but the one continue, and a row goes red.
func TestTheContinuationCheckHoldsTheBoxsReleaseToTheFloors(t *testing.T) {
	t.Parallel()
	claudeArchive := func(release string) sandbox.SessionArchive {
		return sandbox.SessionArchive{Harness: executor.ClaudeCodeName, SessionID: "s",
			Format: executor.ClaudeArchiveFormat, Version: release}
	}
	openCodeArchive := func(release string) sandbox.SessionArchive {
		return sandbox.SessionArchive{Harness: executor.OpenCodeName, SessionID: "ses_s",
			Format: executor.OpenCodeArchiveFormat, Version: release}
	}
	geminiArchive := func(release string) sandbox.SessionArchive {
		return sandbox.SessionArchive{Harness: executor.GeminiCLIName, SessionID: "s",
			Format: executor.GeminiArchiveFormat, Version: release}
	}
	qwenArchive := func(release string) sandbox.SessionArchive {
		return sandbox.SessionArchive{Harness: executor.QwenCodeName, SessionID: "s",
			Format: executor.QwenArchiveFormat, Version: release}
	}
	codexArchive := func(release string) sandbox.SessionArchive {
		return sandbox.SessionArchive{Harness: executor.CodexName, SessionID: "019a1f00-0000-7000-8000-000000000001",
			Format: executor.Codex{}.ArchiveFormat(), Version: release}
	}
	museArchive := func(release string) sandbox.SessionArchive {
		return sandbox.SessionArchive{Harness: executor.MuseName, SessionID: "0b5e0000-0000-4000-8000-000000000001",
			Format: executor.MuseArchiveFormat, Version: release}
	}
	grokArchive := func(release string) sandbox.SessionArchive {
		return sandbox.SessionArchive{Harness: executor.GrokName, SessionID: "11111111-2222-4333-8444-0000000000a1",
			Format: executor.GrokArchiveFormat, Version: release}
	}
	const muse = "Muse Code 1.4.25 (1.4.25-R9361.1)\n"
	const grok = "grok 1.0.92 (b06756fd6256)\n"
	for name, tc := range map[string]struct {
		runner    *executor.Runner
		printed   string
		code      int
		archive   sandbox.SessionArchive
		continues bool
		release   string
		why       string
	}{
		"claude at the floor":               {executor.NewClaudeCode(), "2.1.310 (Claude Code)\n", 0, claudeArchive("2.1.310"), true, "2.1.310", ""},
		"claude past the floor":             {executor.NewClaudeCode(), "2.1.316 (Claude Code)\n", 0, claudeArchive("2.1.300"), true, "2.1.316", ""},
		"claude below the floor":            {executor.NewClaudeCode(), "2.1.309 (Claude Code)\n", 0, claudeArchive("2.1.300"), false, "2.1.309", "older than 2.1.310"},
		"claude, a writer too old":          {executor.NewClaudeCode(), "2.1.310 (Claude Code)\n", 0, claudeArchive("2.1.299"), false, "2.1.310", "older than 2.1.300"},
		"claude older than its writer":      {executor.NewClaudeCode(), "2.1.310 (Claude Code)\n", 0, claudeArchive("2.1.312"), false, "2.1.310", "older than the 2.1.312"},
		"claude, an unreadable release":     {executor.NewClaudeCode(), "Claude Code nightly\n", 0, claudeArchive("2.1.310"), false, "", "named no release"},
		"claude, an unreadable writer":      {executor.NewClaudeCode(), "2.1.310 (Claude Code)\n", 0, claudeArchive(""), false, "2.1.310", "named no release"},
		"claude, another harness's session": {executor.NewClaudeCode(), "2.1.310 (Claude Code)\n", 0, openCodeArchive("2.3.2"), false, "2.1.310", "continued only by the harness that wrote it"},
		"claude, another format": {executor.NewClaudeCode(), "2.1.310 (Claude Code)\n", 0,
			sandbox.SessionArchive{Harness: executor.ClaudeCodeName, SessionID: "s", Format: executor.ClaudeArchiveFormat + 1, Version: "2.1.310"},
			false, "2.1.310", "restores claude-code's format"},
		"claude that does not answer":         {executor.NewClaudeCode(), "", 127, claudeArchive("2.1.310"), false, "", "did not say its release"},
		"opencode on its release":             {executor.NewOpenCode(), "opencode v2.3.2\n", 0, openCodeArchive("2.3.2"), true, "2.3.2", ""},
		"opencode, an older box":              {executor.NewOpenCode(), "opencode v2.2.99\n", 0, openCodeArchive("2.3.2"), false, "2.2.99", "continues only on OpenCode 2.3.2"},
		"opencode, a newer box":               {executor.NewOpenCode(), "opencode v2.3.3\n", 0, openCodeArchive("2.3.2"), false, "2.3.3", "continues only on OpenCode 2.3.2"},
		"opencode, an unreadable box":         {executor.NewOpenCode(), "opencode nightly\n", 0, openCodeArchive("2.3.2"), false, "", "named no release"},
		"opencode, an unreadable writer":      {executor.NewOpenCode(), "opencode v2.3.2\n", 0, openCodeArchive(""), false, "2.3.2", "named no release"},
		"opencode, an older writer":           {executor.NewOpenCode(), "opencode v2.3.2\n", 0, openCodeArchive("2.2.99"), false, "2.3.2", "only a session 2.3.2 wrote"},
		"opencode, a newer writer":            {executor.NewOpenCode(), "opencode v2.3.2\n", 0, openCodeArchive("2.3.3"), false, "2.3.2", "only a session 2.3.2 wrote"},
		"opencode, another harness's session": {executor.NewOpenCode(), "opencode v2.3.2\n", 0, claudeArchive("2.1.310"), false, "2.3.2", "continued only by the harness that wrote it"},
		"codex at its release":                {executor.NewCodex(), "codex-cli 0.171.0\n", 0, codexArchive("0.171.0"), true, "0.171.0", ""},
		"codex newer than its release":        {executor.NewCodex(), "codex-cli 0.172.0\n", 0, codexArchive("0.171.0"), false, "0.172.0", "only Codex 0.171.0 continues"},
		"codex older than its release":        {executor.NewCodex(), "codex-cli 0.170.0\n", 0, codexArchive("0.171.0"), false, "0.170.0", "only Codex 0.171.0 continues"},
		"codex, another release's writer":     {executor.NewCodex(), "codex-cli 0.171.0\n", 0, codexArchive("0.172.0"), false, "0.171.0", "only a session Codex 0.171.0 wrote"},
		"codex, an unreadable writer":         {executor.NewCodex(), "codex-cli 0.171.0\n", 0, codexArchive(""), false, "0.171.0", "named no release"},
		"codex, an unreadable release":        {executor.NewCodex(), "codex-cli dev\n", 0, codexArchive("0.171.0"), false, "", "named no release"},
		"muse at its release":                 {executor.NewMuse(), muse, 0, museArchive("1.4.25"), true, "1.4.25", ""},
		"muse newer than its release":         {executor.NewMuse(), "Muse Code 1.4.30 (1.4.30-R1.1)\n", 0, museArchive("1.4.25"), false, "1.4.30", "continues only on 1.4.25"},
		"muse, a pre-release":                 {executor.NewMuse(), "Muse Code 1.4.25-rc.1\n", 0, museArchive("1.4.25"), false, "", "a release it could not read"},
		"muse, another release's writer":      {executor.NewMuse(), muse, 0, museArchive("1.4.30"), false, "1.4.25", "only a session 1.4.25 wrote"},
		"muse, an unreadable writer":          {executor.NewMuse(), muse, 0, museArchive(""), false, "1.4.25", "named no release"},
		"gemini, exact to exact":              {executor.NewGeminiCLI(), "0.70.0\n", 0, geminiArchive("0.70.0"), true, "0.70.0", ""},
		"gemini, exact to newer":              {executor.NewGeminiCLI(), "0.71.2\n", 0, geminiArchive("0.70.0"), false, "0.71.2", "only on 0.70.0, and this box runs 0.71.2"},
		"gemini, newer to exact":              {executor.NewGeminiCLI(), "0.70.0\n", 0, geminiArchive("0.71.0"), false, "0.70.0", "the session was written by 0.71.0"},
		"gemini, an older box":                {executor.NewGeminiCLI(), "0.65.0\n", 0, geminiArchive("0.70.0"), false, "0.65.0", "this box runs 0.65.0"},
		"gemini, an older writer":             {executor.NewGeminiCLI(), "0.70.0\n", 0, geminiArchive("0.65.0"), false, "0.70.0", "written by 0.65.0"},
		"gemini, an unreadable release":       {executor.NewGeminiCLI(), "gemini nightly\n", 0, geminiArchive("0.70.0"), false, "", "named no release"},
		"gemini, an unrecorded writer":        {executor.NewGeminiCLI(), "0.70.0\n", 0, geminiArchive(""), false, "0.70.0", "named no release"},
		"qwen, exact to exact":                {executor.NewQwenCode(), "0.28.4\n", 0, qwenArchive("0.28.4"), true, "0.28.4", ""},
		"qwen, exact to newer":                {executor.NewQwenCode(), "0.29.0\n", 0, qwenArchive("0.28.4"), false, "0.29.0", "only on 0.28.4, and this box runs 0.29.0"},
		"qwen, newer to exact":                {executor.NewQwenCode(), "0.28.4\n", 0, qwenArchive("0.29.0"), false, "0.28.4", "the session was written by 0.29.0"},
		"qwen, an older writer":               {executor.NewQwenCode(), "0.28.4\n", 0, qwenArchive("0.26.1"), false, "0.28.4", "written by 0.26.1"},
		"qwen, an unreadable release":         {executor.NewQwenCode(), "qwen nightly\n", 0, qwenArchive("0.28.4"), false, "", "named no release"},
		"gemini, a release on a CRLF line":    {executor.NewGeminiCLI(), "0.70.0\r\n", 0, geminiArchive("0.70.0"), true, "0.70.0", ""},
		"gemini, a pre-release box":           {executor.NewGeminiCLI(), "0.70.0-nightly\n", 0, geminiArchive("0.70.0"), false, "", "named no release"},
		"qwen, a pre-release box":             {executor.NewQwenCode(), "0.28.4-preview.1\n", 0, qwenArchive("0.28.4"), false, "", "named no release"},
		"qwen, another harness's session":     {executor.NewQwenCode(), "0.28.4\n", 0, geminiArchive("0.70.0"), false, "0.28.4", "continued only by the harness that wrote it"},
		"grok at its release":                 {executor.NewGrok(), grok, 0, grokArchive("1.0.92"), true, "1.0.92", ""},
		"grok newer than its release":         {executor.NewGrok(), "grok 1.0.93 (92384ee02e2d)\n", 0, grokArchive("1.0.92"), false, "1.0.93", "continues only on 1.0.92"},
		"grok, a pre-release":                 {executor.NewGrok(), "grok 1.0.92-rc1 (x)\n", 0, grokArchive("1.0.92"), false, "", "a release it could not read"},
		"grok, another release's writer":      {executor.NewGrok(), grok, 0, grokArchive("1.0.93"), false, "1.0.92", "only a session 1.0.92 wrote"},
		"grok, an unreadable writer":          {executor.NewGrok(), grok, 0, grokArchive(""), false, "1.0.92", "named no release"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var asked []string
			b := versionBox(tc.printed, tc.code, "command not found", &asked)
			check, err := tc.runner.CheckCarry(t.Context(), b, tc.archive)
			if err != nil {
				t.Fatalf("CheckCarry: %v", err)
			}
			if check.Continues != tc.continues || check.Version != tc.release || !strings.Contains(check.Why, tc.why) ||
				(tc.continues && check.Why != "") {
				t.Errorf("CheckCarry = %+v, want continues %v, release %q, why %q", check, tc.continues, tc.release, tc.why)
			}
			if len(asked) != 1 || !strings.HasPrefix(asked[0], "sh -lc ") ||
				!strings.Contains(asked[0], paths(b).BinDir()) || !strings.Contains(asked[0], "--version") {
				t.Errorf("the box was asked %q, want one login-shell version read with its bin directory on PATH", asked)
			}
		})
	}

	broken := sandbox.NewFakeSandbox("box-1")
	broken.ExecFunc = func(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{}, errors.New("the box is unreachable")
	}
	if _, err := executor.NewClaudeCode().CheckCarry(t.Context(), broken, claudeArchive("2.1.310")); err == nil {
		t.Error("a box that could not be asked answered the continuation check")
	}
}

// THE CONTINUATION CHECK SAYS WHETHER THE BOX'S RESUME WAS READ. Only a Claude
// Code release whose resume this build read (2.1.310, 2.1.322) rebuilds a
// hook's note as the job's session showed it, so only its box is handed a brief
// that counts such a read held: any other release, one the box printed that
// cannot be read, a harness that does not answer, and OpenCode answer not read.
//
// Mutation: answer the resume read whatever the release, or for any Claude
// Code release, and a row goes red.
func TestTheContinuationCheckSaysWhetherTheBoxsResumeWasRead(t *testing.T) {
	t.Parallel()
	cc := executor.NewClaudeCode()
	claude := sandbox.SessionArchive{Harness: executor.ClaudeCodeName, SessionID: "s",
		Format: executor.ClaudeArchiveFormat, Version: "2.1.310"}
	for name, tc := range map[string]struct {
		runner  *executor.Runner
		printed string
		code    int
		archive sandbox.SessionArchive
		read    bool
	}{
		"claude 2.1.310":                {runner: cc, printed: "2.1.310 (Claude Code)\n", archive: claude, read: true},
		"claude 2.1.322":                {runner: cc, printed: "2.1.322 (Claude Code)\n", archive: claude, read: true},
		"claude 2.1.316":                {runner: cc, printed: "2.1.316 (Claude Code)\n", archive: claude},
		"claude, an unreadable release": {runner: cc, printed: "Claude Code nightly\n", archive: claude},
		"claude that does not answer":   {runner: cc, code: 127, archive: claude},
		"opencode": {runner: executor.NewOpenCode(), printed: "opencode v2.3.2\n",
			archive: sandbox.SessionArchive{Harness: executor.OpenCodeName, SessionID: "ses_s",
				Format: executor.OpenCodeArchiveFormat, Version: "2.3.2"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var asked []string
			check, err := tc.runner.CheckCarry(t.Context(), versionBox(tc.printed, tc.code, "command not found",
				&asked), tc.archive)
			if err != nil {
				t.Fatalf("CheckCarry: %v", err)
			}
			if check.ResumeRead != tc.read {
				t.Errorf("CheckCarry = %+v, want the resume read: %v", check, tc.read)
			}
		})
	}
}

// A TAKE SAYS WHETHER ITS ARCHIVE'S RESUME WAS READ, by the predicate the
// continuation check reports a box's by: a Claude Code archive of a release
// whose resume this build read (2.1.310, 2.1.322) and of no other, the only
// Claude Code archive whose capture is raised (internal/sandbox's placeAtNewest).
//
// Mutation: report it whatever the release, or never, and a row goes red.
func TestATakeSaysWhetherItsArchivesResumeWasRead(t *testing.T) {
	t.Parallel()
	for release, want := range map[string]bool{"2.1.310": true, "2.1.322": true, "2.1.316": false} {
		t.Run(release, func(t *testing.T) {
			t.Parallel()
			runner, b := sessionBox(t, release)
			runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
			taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Spool: testSpool(t), MaxBytes: takeBound})
			if err != nil || taken.Body == nil {
				t.Fatalf("TakeSession = %+v, %v", taken, err)
			}
			if err := taken.Body.Close(); err != nil {
				t.Errorf("closing the take: %v", err)
			}
			if taken.ResumeRead != want {
				t.Errorf("the take of an archive %s wrote says its resume was read: %v, want %v",
					release, taken.ResumeRead, want)
			}
		})
	}
}

// WHAT THE RUN DID IS ITS SCRIPT'S RECORD: the session the harness ran (the
// result's, else the record's), whether it continued the one asked, why not, and
// the release `--version` printed; a capture the box holds is described for that
// session. Only a key's first line counts, since the release is written last as
// printed and may run on, and an id no harness mints is no id, the record's or
// the harness's, whose output is as much the box's as the record is.
//
// Mutation: read a key's last line, prefer the record's id over the harness's,
// take the harness's whatever it is, drop the reason, or read a release of any
// length, and a case goes red.
func TestWhatTheRunDidIsItsScriptsRecord(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Stream(), `{"type":"result","subtype":"success","is_error":false,"result":"done"}`+"\n")
	b.Put(p.Result(), `{"type":"result","subtype":"success","is_error":false,"result":"done"}`)
	b.Put(p.ExitCode(), "0")
	b.Put(p.SessionRecord(), "id 9a8b7c6d-0000-4000-8000-000000000001\ncontinued yes\nwhy \n"+
		"release 2.1.310 (Claude Code)\nid a-line-the-harness-printed\ncontinued no\nwhy the harness's own words\n")
	b.Put(p.SessionCapture(), "an archive")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.SessionID != "9a8b7c6d-0000-4000-8000-000000000001" || !res.Continued || res.HarnessVersion != "2.1.310" ||
		res.FellBack != "" {
		t.Errorf("the run reads session %q, continued %v, fell back %q, release %q; want the record's first lines",
			res.SessionID, res.Continued, res.FellBack, res.HarnessVersion)
	}
	want := sandbox.SessionArchive{Harness: executor.ClaudeCodeName, SessionID: res.SessionID,
		Format: executor.ClaudeArchiveFormat, Version: "2.1.310", Size: int64(len("an archive")),
		Path: p.SessionCapture()}
	if res.Captured == nil || *res.Captured != want {
		t.Errorf("the capture is described as %+v, want %+v", res.Captured, want)
	}

	// THE HARNESS'S OWN WORD for its session, where it gave one.
	b.Put(p.Result(), `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"from-the-harness"}`)
	if res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil)); err != nil || res.SessionID != "from-the-harness" {
		t.Errorf("the run reads session %q (%v), want its harness's own", res.SessionID, err)
	}

	// BUT ONLY ONE A HARNESS MINTS: an id in the output no harness mints gives
	// way to the record's.
	for _, named := range []string{"../../.credentials.json", strings.Repeat("s", 129)} {
		b.Put(p.Result(), `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"`+
			named+`"}`)
		if res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil)); err != nil ||
			res.SessionID != "9a8b7c6d-0000-4000-8000-000000000001" {
			t.Errorf("a harness that named %.40q (%d bytes) as its session read as %.40q (%v), want the record's",
				named, len(named), res.SessionID, err)
		}
	}

	// AN ID NO HARNESS MINTS is no id, and a capture is described for none.
	b.Put(p.Result(), `{"type":"result","subtype":"success","is_error":false,"result":"done"}`)
	b.Put(p.SessionRecord(), "id ../../.credentials.json\ncontinued yes\nrelease 2.1.310 (Claude Code)\n")
	if res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil)); err != nil || res.SessionID != "" ||
		res.Captured != nil {
		t.Errorf("a record naming no session id read as session %q, captured %+v (%v)", res.SessionID,
			res.Captured, err)
	}

	// A RUN THAT STARTED ANEW SAYS WHY, as its script recorded it.
	const why = "it is not in this box, and no archive of it was carried here"
	b.Put(p.SessionRecord(), "id 9a8b7c6d-0000-4000-8000-000000000002\ncontinued no\nwhy "+why+
		"\nrelease 2.1.310 (Claude Code)\n")
	if res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil)); err != nil || res.Continued ||
		res.FellBack != why {
		t.Errorf("a run that started anew reads continued %v, fell back %q (%v); want %q", res.Continued,
			res.FellBack, err, why)
	}

	// A RELEASE PAST WHAT THE EXECUTOR LOG RECORDS is none, on the run and on
	// its capture's description.
	b.Put(p.SessionRecord(), "id 9a8b7c6d-0000-4000-8000-000000000001\ncontinued yes\nwhy \nrelease "+
		strings.Repeat("2", 1<<10)+".1.310 (Claude Code)\n")
	if res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil)); err != nil || res.HarnessVersion != "" ||
		res.Captured == nil || res.Captured.Version != "" {
		described := -1
		if res.Captured != nil {
			described = len(res.Captured.Version)
		}
		t.Errorf("a release a KiB long reads as %d bytes, and %d on the capture (%v); want none on either",
			len(res.HarnessVersion), described, err)
	}

	// NO CAPTURE, NONE DESCRIBED — and none streamed.
	b.Put(p.SessionRecord(), "id 9a8b7c6d-0000-4000-8000-000000000001\ncontinued no\nwhy \nrelease 2.1.310 (Claude Code)\n")
	b.Put(p.SessionCapture(), "")
	if res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil)); err != nil || res.Captured != nil || res.Continued {
		t.Errorf("a run with no capture reads captured %+v, continued %v (%v)", res.Captured, res.Continued, err)
	}
	if described, body, err := runner.OpenCapture(t.Context(), b); err != nil || body != nil ||
		described != (sandbox.SessionArchive{}) {
		t.Errorf("OpenCapture of a box with none = %+v, %v, %v", described, body, err)
	}
}

// A BOX WITHOUT TAR AND GZIP PACKS NOTHING, SAYS SO, AND THE RUN IS ITS HARNESS'S.
// The capture is the run's last step and never its outcome: without them the
// session is not packed, the error stream says so, and the run's status is the
// harness's own.
//
// Mutation: let the pack's failure be the script's status, or pack silently,
// and the case goes red.
func TestABoxWithoutTarPacksNothingAndSaysSo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	p := executor.Paths{Home: filepath.Join(dir, "home")}
	for _, d := range []string{bin, p.WorkDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The box's PATH holds the harness and what it runs, and no tar or gzip.
	if path, err := exec.LookPath("mkdir"); err != nil || os.Symlink(path, filepath.Join(bin, "mkdir")) != nil {
		t.Fatalf("mkdir for the box: %v", err)
	}
	writeStandIn(t, filepath.Join(bin, "claude"), `#!/bin/sh
if [ "$1" = --version ]; then echo '2.1.310 (Claude Code)'; exit 0; fi
s= prev=; for a; do case "$prev" in --session-id|--resume) s=$a;; esac; prev=$a; done
mkdir -p "$CLAUDE_CONFIG_DIR/projects/$CLAUDE_CODE_PROJECT_DIR_NAME"
echo '{"type":"user"}' > "$CLAUDE_CONFIG_DIR/projects/$CLAUDE_CODE_PROJECT_DIR_NAME/$s.jsonl"
exit 3
`)
	script := scriptOf(t, commandOf(claude(), sandbox.RunRequest{Brief: "x"}, p, ""))
	cmd := exec.Command("sh", "-c", script) //nolint:gosec // the script under test
	cmd.Env = []string{"PATH=" + bin, "CLAUDE_CONFIG_DIR=" + p.ClaudeConfigDir(),
		"CLAUDE_CODE_PROJECT_DIR_NAME=" + executor.ClaudeProjectDir}
	var stderr strings.Builder
	cmd.Stdin, cmd.Stderr = strings.NewReader("x"), &stderr
	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("the script ended %v, want the harness's own status 3:\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "has no tar and gzip") {
		t.Errorf("the error stream does not say the session was not packed:\n%s", stderr.String())
	}
	if _, err := os.Stat(p.SessionCapture()); !os.IsNotExist(err) {
		t.Errorf("a box without tar packed a capture (%v)", err)
	}
}

// A BOX WITHOUT FIND RESUMES ITS SESSION, ITS THREADS AS THEY WERE, AND SAYS
// SO. A resume cuts back each subagent thread's torn last line, walking the
// session's sidecar directory with find; without find the run still resumes
// (the transcript's own cut takes no find), leaves every thread as it was, and
// says on its error stream that it cut none back, rather than silently hand the
// harness threads that may be torn.
//
// Mutation: walk the threads without asking the box for find, or say nothing
// where it has none, and the case goes red.
func TestABoxWithoutFindResumesItsSessionAndSaysItCutNoThread(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	p := executor.Paths{Home: filepath.Join(dir, "home")}
	const session = "3c2b1a09-8f7e-4d6c-9b5a-4f3e2d1c0b9a"
	project := filepath.Join(p.ClaudeConfigDir(), "projects", executor.ClaudeProjectDir)
	for _, d := range []string{bin, p.WorkDir(), filepath.Join(project, session, "subagents")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	transcript := filepath.Join(project, session+".jsonl")
	thread := filepath.Join(project, session, "subagents", "agent-1.jsonl")
	const torn = tornWhole + `{"type":"us`
	for _, file := range []string{transcript, thread} {
		if err := os.WriteFile(file, []byte(torn), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// THE BOX'S PATH holds the harness and the transcript cut's tools, and no find.
	for _, tool := range []string{"mkdir", "rm", "mv", "tail", "wc", "head"} {
		if path, err := exec.LookPath(tool); err != nil || os.Symlink(path, filepath.Join(bin, tool)) != nil {
			t.Fatalf("%s for the box: %v", tool, err)
		}
	}
	argv := filepath.Join(dir, "argv")
	writeStandIn(t, filepath.Join(bin, "claude"), `#!/bin/sh
if [ "$1" = --version ]; then echo '2.1.310 (Claude Code)'; exit 0; fi
for a; do printf '%s\n' "$a"; done > `+shellword.Quote(argv)+"\n")
	script := scriptOf(t, commandOf(claude(), sandbox.RunRequest{Brief: "x", Resume: session}, p, ""))
	cmd := exec.CommandContext(t.Context(), "sh", "-c", script) //nolint:gosec // the script under test
	cmd.Env = []string{"PATH=" + bin, "CLAUDE_CONFIG_DIR=" + p.ClaudeConfigDir(),
		"CLAUDE_CODE_PROJECT_DIR_NAME=" + executor.ClaudeProjectDir}
	var stderr strings.Builder
	cmd.Stdin, cmd.Stderr = strings.NewReader("x"), &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the script ended %v:\n%s", err, stderr.String())
	}
	if raw, err := os.ReadFile(argv); err != nil || !strings.Contains(string(raw), "--resume\n"+session+"\n") {
		t.Fatalf("the harness was not resumed on the session (%v):\n%s\n%s", err, raw, stderr.String())
	}
	if !strings.Contains(stderr.String(), "this box has no find, so no subagent's thread of the session was cut back") {
		t.Errorf("the error stream does not say that no thread was cut back:\n%s", stderr.String())
	}
	for file, want := range map[string]string{transcript: tornWhole, thread: torn} {
		if raw, err := os.ReadFile(file); err != nil || string(raw) != want {
			t.Errorf("after the resume %s holds %q (%v), want %q", file, raw, err, want)
		}
	}
}

// A TORN TRANSCRIPT A PACK PADDED WITH NULS IS CUT BACK WHEN IT IS RESTORED,
// under every sh a box may have. GNU tar NUL-pads a file that shrank while it
// read it to the size it first saw (measured on 1.35), and a transcript can
// shrink while a pack of its still-working job reads it: 2.1.310 rewrites it in
// place to withdraw a message. Restored, it ends in the torn line and its
// padding with no newline, and is cut back to its last whole line before the
// resume; the cut counts the last byte's newline, since command substitution
// drops a NUL and a test of the byte's text reads the padding as a whole line.
//
// Mutation: read the last byte as text rather than count its newline, or cut no
// transcript, and the padded line is resumed, the harness's next record written
// onto it.
func TestATornTranscriptAPackPaddedWithNULsIsCutBackWhenItIsRestored(t *testing.T) {
	t.Parallel()
	const cutOff = `{"type":"assistant","message":{"content":[{"type":"tool_us`
	ending := cutOff + strings.Repeat("\x00", 64)
	// THE FIRST BOX'S HARNESS leaves its transcript as such a pack holds it,
	// which its run's script then packs byte for byte.
	harness := strings.Replace(sessionHarness("2.1.310"), `printf '{"type":"result"`,
		`printf '%s' `+shellword.Quote(cutOff)+` >> "$p/$s.jsonl" && head -c 64 /dev/zero >> "$p/$s.jsonl"`+"\n"+
			`printf '{"type":"result"`, 1)
	forEachShell(t, func(t *testing.T, sh boxShell) {
		first, a := realBox(t)
		writeBoxStandIn(t, a, executor.PathsFor(a).BinDir()+"/claude", harness)
		_, ran := runToEnd(t, first, a, sandbox.RunRequest{Brief: "begin"})
		held := boxFile(t, a, transcriptOf(a, ran.SessionID))
		whole, ok := strings.CutSuffix(held, ending)
		if !ok {
			t.Fatalf("the first box's transcript does not end in a torn line and its padding:\n%q", held)
		}
		described, body, err := first.OpenCapture(t.Context(), a)
		if err != nil || body == nil {
			t.Fatalf("OpenCapture = %v, %v", body, err)
		}
		defer func() { _ = body.Close() }()

		second, b := realBoxUnder(t, sh)
		writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/claude", sessionHarness("2.1.310"))
		_, res := runToEnd(t, second, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
			Carried: &sandbox.CarriedSession{Archive: described, Body: body}})
		if !res.Continued {
			t.Fatalf("the carried session was not continued:\n%s", boxFile(t, b, paths(b).Err()))
		}
		got := boxFile(t, b, transcriptOf(b, ran.SessionID))
		if strings.Contains(got, "tool_us") || strings.ContainsRune(got, 0) || !strings.HasPrefix(got, whole) ||
			strings.Count(got, "\n") != strings.Count(whole, "\n")+1 {
			t.Errorf("the restored transcript was resumed with its torn line and its padding:\n%q", got)
		}
	})
}

// A BOX CONTINUES ITS OWN COPY ONLY WHERE THE RELEASE THAT WROTE IT IS ITS
// ONE SUPPORTED RELEASE, for every runner whose harness holds a release to its
// own copy (Codex, Gemini CLI, Qwen Code, Copilot, Kimi Code, Muse Code, Pi,
// Grok Build): the box on that release still refuses a copy its last run's
// record says a newer release wrote, as a box downgraded since would hold, and
// one with no record at all, naming why; the copy that release wrote is
// continued; a Gemini CLI or Qwen Code box of an unread release refuses its
// own copy too.
//
// Mutation: check the box's release alone in CheckInPlace, read the writer
// from anywhere but the box's record, take a missing record for a current
// writer, or name only a carried one for an unread release: a row goes red.
func TestABoxContinuesOnlyACopyItsOneReleaseWrote(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		runner             *executor.Runner
		printed, newer     string
		supported, refused string
	}{
		{executor.NewCodex(), "codex-cli 0.171.0", "codex-cli 0.172.0", "0.171.0", "0.172.0"},
		{executor.NewGeminiCLI(), "0.70.0", "0.71.0", "0.70.0", "0.71.0"},
		{executor.NewQwenCode(), "0.28.4", "0.28.5", "0.28.4", "0.28.5"},
		{executor.NewCopilot(), "GitHub Copilot CLI 1.0.114.", "GitHub Copilot CLI 1.0.115.", "1.0.114", "1.0.115"},
		{executor.NewKimi(), "2.7.3", "2.7.4", "2.7.3", "2.7.4"},
		{executor.NewMuse(), "Muse Code 1.4.25 (1.4.25-R9361.1)", "Muse Code 1.4.26 (1.4.26-R1.1)", "1.4.25", "1.4.26"},
		{executor.NewPi(), "1.6.0", "1.7.0", "1.6.0", "1.7.0"},
		{executor.NewGrok(), "grok 1.0.92 (b06756fd6256)", "grok 1.0.93 (92384ee02e2d)", "1.0.92", "1.0.93"},
	} {
		for writer, want := range map[string]string{c.printed: "", c.newer: c.refused, "": "left no record"} {
			var asked []string
			b := versionBox(c.printed+"\n", 0, "", &asked)
			if writer != "" {
				record := "id s-1\ncontinued yes\nwhy \nrelease " + writer + "\n"
				if err := b.WriteFile(t.Context(), paths(b).SessionRecord(), []byte(record)); err != nil {
					t.Fatal(err)
				}
			}
			check, err := c.runner.CheckInPlace(t.Context(), b)
			if err != nil {
				t.Fatalf("%s, writer %q: CheckInPlace: %v", c.runner.Name(), writer, err)
			}
			if check.Continues != (want == "") || !strings.Contains(check.Why, want) || check.Version != c.supported {
				t.Errorf("%s, a copy written by %q: %+v, want continued %v naming %q", c.runner.Name(), writer,
					check, want == "", want)
			}
		}
	}
	for _, runner := range []*executor.Runner{executor.NewGeminiCLI(), executor.NewQwenCode()} {
		var asked []string
		b := versionBox("nightly build\n", 0, "", &asked)
		record := "id s-1\ncontinued yes\nwhy \nrelease nightly build\n"
		if err := b.WriteFile(t.Context(), paths(b).SessionRecord(), []byte(record)); err != nil {
			t.Fatal(err)
		}
		check, err := runner.CheckInPlace(t.Context(), b)
		if err != nil || check.Continues || !strings.Contains(check.Why, "its own copy") || check.Version != "" {
			t.Errorf("%s, a box whose release does not read: %+v, %v; want its own copy not continued, "+
				"saying so", runner.Name(), check, err)
		}
	}
}
