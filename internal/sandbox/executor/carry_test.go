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

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// sessionHarness is a stand-in `claude` for the cases about a run's session, run in
// a real box: it answers `--version` with release, records its argv and its
// environment in the box's home, and files the session it was started in where
// the pinned layout names — its transcript, its sidecar directory and its task
// list, as Claude Code does — beside what a capture must never take: the
// session's file checkpoints, the harness's own state, a login, auto memory and
// another session. A session it is asked to resume must already be filed, in
// any project directory, as Claude Code's must from 2.1.223 ("No conversation
// found"). It ends on a successful result naming its session.
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
echo checkpoint > "$cfg/file-history/$s/edit"
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
	res, err := runner.Collect(t.Context(), box, handle)
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

// transcriptOf is where a session's transcript lies in box under the pinned
// layout.
func transcriptOf(box sandbox.Sandbox, session string) string {
	return executor.PathsFor(box).ClaudeConfigDir() + "/projects/" + executor.ClaudeProjectDir + "/" +
		session + ".jsonl"
}

// THE WRAPPER PINS WHERE CLAUDE CODE KEEPS ITS SESSIONS, over whatever set it.
// CLAUDE_CONFIG_DIR is the box home's `.claude`, CLAUDE_CODE_PROJECT_DIR_NAME
// the one constant every run files its sessions under, and auto memory is off —
// exported by the wrapper after it unsets the variables that switch hooks off,
// and before the harness, so a value the seat's environment, the image or a profile
// the login shell sources gave any of them is replaced. On a real box, with the
// seat's environment naming another of each: the harness sees the pinned ones.
//
// Mutation: export the pins before the unsets' statement, or drop one, and the
// order or the seat's value reaches the harness; export them around the shell
// rather than in it, and a profile replaces them.
func TestTheWrapperPinsClaudeCodesSessionLayout(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	script := startAndScript(t, runner, b, sandbox.RunRequest{Brief: "x"})
	unset := strings.Index(script, "unset CLAUDE_CODE_SIMPLE CLAUDE_CODE_SAFE_MODE; ")
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
// `^[A-Za-z0-9_-]{1,64}$` (2.1.316), and ignores one past it, which would file
// every session under the box's working directory again.
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
// RESUMES ITS OWN — NEVER BOTH. The id a fresh run is started under is minted
// before the harness starts and returned by Start, so a run killed before its result
// line still names its session; a run that continues one is started with
// `--resume` alone, since the harness refuses `--session-id` beside it unless the
// session is forked. A session the box holds is resumed wherever a release
// filed it — under the pinned project directory, or under the one an older
// release named after the box's working directory — and one the box does not
// hold is never resumed: the run starts the session minted for it instead,
// says why on its error stream and in what it reports, and reports that it did
// not continue.
//
// Mutation: pass --session-id beside --resume, resume a session the box does
// not hold, look for it under the pinned directory alone, or report no reason,
// and a row goes red.
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
// THE OPTION. By default a conversation records the system prompt its first
// request rendered and sends that record on every resume until it is
// compacted, whatever a later launch appends — so every resumed run is started
// with `--system-prompt-snapshot off`, its own box's included; but 2.1.263, a
// release boxes were measured on, refuses the option as unknown, so the script
// passes it from 2.1.267 alone. A fresh run records its own, and is started
// without it. The transcript's trim is the same kind of gate: on from 2.1.304,
// whose trim loses nothing, and UNSET before it, whatever the seat set. A
// version that is not three parts reads as a current release, as the engine's
// own reading takes it. Under every sh a box may run the script with.
//
// Mutation: pass the option whatever the release, or only on a carried
// session, and the 2.1.263 or the same-box rows go red; export the trim
// whatever the release, and the 2.1.303 row does; read the parts once cut,
// and the two-part row reads as an old release.
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
				// The box holds the session a resumed run continues, as
				// the one its last run ran in does.
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

// THE CAPTURE IS THE SESSION AND NOTHING ELSE. Once the harness has exited, the run
// packs the session it ran into one gzipped tar, moved into place whole: its
// transcript, its sidecar directory (the subagents' transcripts and the tool
// results spilled to files) and its task list, named relative to the harness's
// configuration directory — and never the login beside them, the harness's own
// state, the session's file checkpoints, auto memory or another session. The
// collection is told where it is, which session it holds, the release that
// wrote it and its size, and is handed it as a stream.
//
// Mutation: pack the configuration directory, or the project directory, and
// the login, the state or the other session is in the archive; drop the task
// list or the sidecars, and the session comes back without them.
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

// A SESSION CARRIED INTO A FRESH BOX IS RESTORED THERE AND RESUMED. The run's
// capture, streamed out of the box it ran in, is handed to a run in another box
// that continues the same session: written in as the run starts, restored by the
// run's script under the pinned layout before the harness starts — the transcript,
// its sidecars and its task list — and resumed there with its system prompt
// rendered afresh. The archive is removed once restored, and the run reports
// that it continued the session.
//
// Mutation: write the archive before the clear, restore it anywhere but the
// pinned project directory, or leave the resume to a box that already held the
// session, and the second run starts a session of its own.
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
	// The transcript carried two lines: the first run's, and the second's,
	// which the harness appended to the one it resumed.
	if n := strings.Count(boxFile(t, b, transcriptOf(b, ran.SessionID)), "\n"); n != 2 {
		t.Errorf("the resumed transcript has %d lines, want the carried one and the resumed run's", n)
	}
	if left := boxFile(t, b, paths(b).SessionCarried()); left != "" {
		t.Errorf("the carried archive was left in the box after its restore (%d bytes)", len(left))
	}
}

// A BOX WHOSE COPY HOLDS THE CARRIED ARCHIVE IS NEVER WRITTEN OVER. A reused
// box packed that archive itself — its digest is among the box's own — and ran
// on past it, so its copy of the session is newer than the archive, and an
// archive of it carried back in — an older capture, the last one having
// failed — is not restored over it: the run resumes the box's own, which keeps
// every line the archive lacks.
//
// Mutation: restore whatever the box holds, or compare by id and not by the
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

// A BOX WHOSE COPY DOES NOT HOLD THE CARRIED ARCHIVE HAS IT REPLACED. The
// session went on in another box after this one's copy: box A packed it, box B
// restored that and ran on, and B's capture is carried back into A. A's copy
// holds only the earlier point, so the archive is restored over it — the
// transcript, its sidecars and its task list — and the run continues the
// session as B left it, keeping the archive's digest as one A's copy now
// holds.
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

// A CARRIED SESSION THE BOX CANNOT RESTORE IS NEVER RESUMED. A box whose harness is
// older than the release that continues a carried session, and an archive that
// holds no transcript, each start the session minted for the run in its place,
// say why on the run's error stream and in what the run reports, and report
// that the run did not continue.
//
// Mutation: resume whatever the restore did, and the harness fails the run on a
// session it does not hold; record no reason, and the report says nothing of
// why.
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

// A RUN HANDED A SESSION IT CANNOT RESTORE IS REFUSED BEFORE ANYTHING STARTS: an
// archive another harness wrote, or another format lays out, one that is not the
// session the run continues, and one with no bytes — a run that could only
// start a session of its own would be answered as if it remembered.
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

// THE CONTINUATION CHECK READS THE BOX HARNESS'S RELEASE, AND HOLDS IT TO THE
// FLOORS. A session carried from another box is continued only by the harness that
// wrote it, in the format this build restores, on a box whose harness is at or
// after its floor — Claude Code 2.1.310, OpenCode 2.0.32 — never older than the
// release that wrote the session, and, for Claude Code, written by 2.1.300 or
// later. A release that cannot be read is taken for a current one. The release
// the box printed is returned whatever the answer, for the run's record; a box
// whose harness does not answer is no continuation, and one that cannot be asked is
// an error. Read with one login-shell exec, the box's bin directory first on
// its PATH.
//
// Mutation: compare against the writer's floor alone, let an older box take a
// newer writer's session, or read an unreadable release as too old, and a row
// goes red.
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
		"claude, an unreadable release":     {executor.NewClaudeCode(), "Claude Code nightly\n", 0, claudeArchive("2.1.310"), true, "", ""},
		"claude, an unreadable writer":      {executor.NewClaudeCode(), "2.1.310 (Claude Code)\n", 0, claudeArchive(""), true, "2.1.310", ""},
		"claude, another harness's session": {executor.NewClaudeCode(), "2.1.310 (Claude Code)\n", 0, openCodeArchive("2.0.32"), false, "2.1.310", "continued only by the harness that wrote it"},
		"claude, another format": {executor.NewClaudeCode(), "2.1.310 (Claude Code)\n", 0,
			sandbox.SessionArchive{Harness: executor.ClaudeCodeName, SessionID: "s", Format: executor.ClaudeArchiveFormat + 1, Version: "2.1.310"},
			false, "2.1.310", "restores claude-code's format"},
		"claude that does not answer":         {executor.NewClaudeCode(), "", 127, claudeArchive("2.1.310"), false, "", "did not say its release"},
		"opencode at the floor":               {executor.NewOpenCode(), "opencode v2.0.32\n", 0, openCodeArchive("2.0.32"), true, "2.0.32", ""},
		"opencode below the floor":            {executor.NewOpenCode(), "opencode v2.0.31\n", 0, openCodeArchive("2.0.31"), false, "2.0.31", "older than 2.0.32"},
		"opencode older than its writer":      {executor.NewOpenCode(), "opencode v2.1.4\n", 0, openCodeArchive("2.2.5"), false, "2.1.4", "older than the 2.2.5"},
		"opencode, another harness's session": {executor.NewOpenCode(), "opencode v2.1.4\n", 0, claudeArchive("2.1.310"), false, "2.1.4", "continued only by the harness that wrote it"},
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

// WHAT THE RUN DID IS ITS SCRIPT'S RECORD. The record names the session the harness
// ran — the result's where its harness reported none — whether it continued the one
// it was asked to, why it did not where it was asked, and the harness's release,
// read off what `--version` printed; a capture the box holds is described for
// the record's session. Only a record's first line of each key is its own,
// since the release is written last as the harness printed it and may run on, and
// an id no harness mints is no id.
//
// Mutation: read the last line of a key, take the record's id over the harness's
// own, or drop the reason, and a case goes red.
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

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
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
	if res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}); err != nil || res.SessionID != "from-the-harness" {
		t.Errorf("the run reads session %q (%v), want its harness's own", res.SessionID, err)
	}

	// AN ID NO HARNESS MINTS is no id, and a capture is described for none.
	b.Put(p.Result(), `{"type":"result","subtype":"success","is_error":false,"result":"done"}`)
	b.Put(p.SessionRecord(), "id ../../.credentials.json\ncontinued yes\nrelease 2.1.310 (Claude Code)\n")
	if res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}); err != nil || res.SessionID != "" ||
		res.Captured != nil {
		t.Errorf("a record naming no session id read as session %q, captured %+v (%v)", res.SessionID,
			res.Captured, err)
	}

	// A RUN THAT STARTED ANEW SAYS WHY, as its script recorded it.
	const why = "it is not in this box, and no archive of it was carried here"
	b.Put(p.SessionRecord(), "id 9a8b7c6d-0000-4000-8000-000000000002\ncontinued no\nwhy "+why+
		"\nrelease 2.1.310 (Claude Code)\n")
	if res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}); err != nil || res.Continued ||
		res.FellBack != why {
		t.Errorf("a run that started anew reads continued %v, fell back %q (%v); want %q", res.Continued,
			res.FellBack, err, why)
	}

	// NO CAPTURE, NONE DESCRIBED — and none streamed.
	b.Put(p.SessionRecord(), "id 9a8b7c6d-0000-4000-8000-000000000001\ncontinued no\nwhy \nrelease 2.1.310 (Claude Code)\n")
	b.Put(p.SessionCapture(), "")
	if res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}); err != nil || res.Captured != nil || res.Continued {
		t.Errorf("a run with no capture reads captured %+v, continued %v (%v)", res.Captured, res.Continued, err)
	}
	if described, body, err := runner.OpenCapture(t.Context(), b); err != nil || body != nil ||
		described != (sandbox.SessionArchive{}) {
		t.Errorf("OpenCapture of a box with none = %+v, %v, %v", described, body, err)
	}
}

// A BOX WITHOUT TAR AND GZIP PACKS NOTHING, SAYS SO, AND THE RUN IS ITS HARNESS'S.
// The capture is the run's last step and never its outcome: on a box whose
// image carries neither, the session is not packed, the run's error stream says
// so, and the run's status is the harness's own.
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
