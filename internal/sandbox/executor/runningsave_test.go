package executor_test

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// What a runner offers the saving of a session its run still works in: whether
// it saves at all, how far its job's stream has run, and the release of a save
// no take will read — and a save of a session whose subagents work, taken and
// read for what it holds, end to end on the runner's own pack, take and reading.

// A SAVE NO TAKE WILL READ IS REMOVED FROM ITS BOX by its release, and nothing
// else is: the save released is gone, another taker's save of the same box is
// still there, and a take of the released save finds nothing; a release under
// a context already cancelled still removes its save, since what gave a save up
// is often that cancellation; and a path that is no save — the job's own
// capture, its record of its session — is refused, and left where it is.
//
// Mutation: make the release do nothing, and the released save is still in the
// box; let it remove any path it is handed, and the capture or the record is
// gone, or no error says it was refused; run it under its caller's context, and
// the save released under a cancelled one stays.
func TestASaveNotTakenIsRemovedFromItsBox(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	p := paths(b)
	saved, err := runner.PackSave(t.Context(), b, takerA)
	if err != nil || saved.Path == "" {
		t.Fatalf("PackSave = %+v, %v", saved, err)
	}
	other, err := runner.PackSave(t.Context(), b, takerB)
	if err != nil || other.Path == "" {
		t.Fatalf("PackSave for the second taker = %+v, %v", other, err)
	}

	if err := runner.ReleaseSave(t.Context(), b, saved.Path); err != nil {
		t.Fatalf("the release of a save was refused: %v", err)
	}
	if boxHas(t, b, saved.Path) {
		t.Errorf("the released save is still at %s", saved.Path)
	}
	if !boxHas(t, b, other.Path) {
		t.Errorf("the release of one save removed another taker's, at %s", other.Path)
	}
	if taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Save: saved.Path,
		MaxBytes: takeBound}); err != nil || taken.Body != nil {
		t.Errorf("a take of a released save = %+v, %v; want nothing", taken, err)
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runner.ReleaseSave(cancelled, b, other.Path); err != nil {
		t.Fatalf("the release of a save under a cancelled context was refused: %v", err)
	}
	if boxHas(t, b, other.Path) {
		t.Errorf("the save released under a cancelled context is still at %s", other.Path)
	}

	for _, notSave := range []string{p.SessionCapture(), p.SessionRecord()} {
		if !boxHas(t, b, notSave) {
			t.Fatalf("the run left nothing at %s to be refused the release of", notSave)
		}
		if err := runner.ReleaseSave(t.Context(), b, notSave); err == nil {
			t.Errorf("the release of %s, which is no save, was made", notSave)
		}
		if !boxHas(t, b, notSave) {
			t.Errorf("a release refused for %s removed it all the same", notSave)
		}
	}
}

// A SESSION'S STREAMED LENGTH IS ITS HARNESS'S EVENT STREAM: the size of the
// file the harness's stdout is redirected into — Claude Code's stream-json,
// OpenCode's event log — and never another file of the run's, so a step of the
// run moves it by exactly what the step printed, and a box whose job printed
// nothing reads zero. Each file beside it (the other harness's stream, the
// result, Claude Code's transcript) is written at a size of its own, so a read
// of any of them answers a length the stream's is not.
//
// Mutation: read Claude Code's transcript or its result in place of its
// stream, or OpenCode's in place of its event log, and the length read is
// another file's.
func TestASessionsStreamedLengthIsItsHarnesssEventStream(t *testing.T) {
	t.Parallel()
	const session = "5e55100a-0000-4000-8000-0000000000ab"
	for name, tc := range map[string]struct {
		runner *executor.Runner
		stream func(executor.Paths) string
	}{
		"Claude Code": {runner: executor.NewClaudeCode(), stream: executor.Paths.Stream},
		"OpenCode":    {runner: executor.NewOpenCode(), stream: executor.Paths.Result},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := sandbox.NewFakeSandbox("box-1")
			p := paths(b)
			if got, err := tc.runner.Streamed(t.Context(), b); err != nil || got != 0 {
				t.Fatalf("Streamed of a box whose job printed nothing = %d, %v; want 0", got, err)
			}
			// THE RUN'S RECORD NAMES ITS SESSION, so the transcript beside the
			// stream is the very one a read of the session's transcript reads.
			if err := b.WriteFile(t.Context(), p.SessionRecord(), []byte("id "+session+"\ncontinued no\n")); err != nil {
				t.Fatal(err)
			}
			files := map[string]int{p.Stream(): 300, p.Result(): 500, transcriptOf(b, session): 700}
			for file, size := range files {
				if err := b.WriteFile(t.Context(), file, bytes.Repeat([]byte("x"), size)); err != nil {
					t.Fatal(err)
				}
			}
			stream := tc.stream(p)
			want := int64(files[stream])
			if got, err := tc.runner.Streamed(t.Context(), b); err != nil || got != want {
				t.Errorf("Streamed = %d, %v; want %d, the size of %s", got, err, want, stream)
			}
			step := []byte(`{"type":"assistant","a step":"of the run"}` + "\n")
			if err := b.WriteFile(t.Context(), stream, append(bytes.Repeat([]byte("x"), int(want)), step...)); err != nil {
				t.Fatal(err)
			}
			if got, err := tc.runner.Streamed(t.Context(), b); err != nil || got != want+int64(len(step)) {
				t.Errorf("Streamed once a step was printed = %d, %v; want %d", got, err, want+int64(len(step)))
			}
		})
	}
}

// EVERY CARRYING RUNNER SAVES: Claude Code's packs a save beside its working
// harness, and OpenCode's exports one beside its running server, so a save is
// offered a run of either.
//
// Mutation: drop OpenCode's PackSave, and its row goes red.
func TestEveryCarryingRunnerSaves(t *testing.T) {
	t.Parallel()
	for name, runner := range map[string]*executor.Runner{"Claude Code": executor.NewClaudeCode(),
		"OpenCode": executor.NewOpenCode()} {
		if !runner.Saves() {
			t.Errorf("the %s runner says it packs no saves", name)
		}
	}
}

// The measured session a save of subagents' work is made of: probe P2(g)'s
// (testdata/claude/2.1.310/holdings/G), a foreground subagent answered by its
// Agent call's own result and a background one by its task notification, each
// having made one call of the bridge's that it answered.
const (
	gSession    = "5e55100a-0000-4000-8000-000000000121"
	gBackground = "a0000000000000001"
	gForeground = "a0000000000000002"
)

// gCalls are the two subagents' calls, as the bridge's log keeps them.
var (
	gBackgroundCall = sandbox.LoggedCall{ID: "toolu_g_bg1", Asked: true, Output: "crewlet g_bg1 done"}
	gForegroundCall = sandbox.LoggedCall{ID: "toolu_g_fg1", Asked: true, Output: "crewlet g_fg1 done"}
)

// fileMeasuredSession files P2(g)'s session in b's harness configuration as
// session, the session b's run worked in, in place of what the run's stand-in
// filed there — every file renamed and every record restated for session —
// with the background subagent's thread written all but its last byte, the
// line break that ends its last record: a write the pack catches before it has
// landed. It answers that thread's path in b and its whole content.
func fileMeasuredSession(t *testing.T, b sandbox.Sandbox, session string) (thread, whole string) {
	t.Helper()
	cfg := paths(b).ClaudeConfigDir()
	sidecar := cfg + "/projects/" + executor.ClaudeProjectDir + "/" + session
	if res, err := b.Exec(t.Context(), "rm -rf "+shQuote(sidecar), sandbox.ExecOptions{}); err != nil ||
		res.ExitCode != 0 {
		t.Fatalf("clearing the stand-in's sidecar directory: %v (exit %d) %s", err, res.ExitCode, res.Stderr)
	}
	root := filepath.Join("testdata", "claude", "2.1.310", "holdings", "G")
	err := filepath.WalkDir(root, func(file string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		dest := cfg + "/" + strings.ReplaceAll(filepath.ToSlash(rel), gSession, session)
		content := strings.ReplaceAll(string(raw), gSession, session)
		if strings.HasSuffix(dest, "/agent-"+gBackground+".jsonl") {
			thread, whole = dest, content
			content = strings.TrimSuffix(content, "\n")
		}
		return b.WriteFile(t.Context(), dest, []byte(content))
	})
	if err != nil || thread == "" || !strings.HasSuffix(whole, "\n") {
		t.Fatalf("filing P2(g)'s session: %v; the background subagent's thread at %q", err, thread)
	}
	return thread, whole
}

// takenWhole is the save at path of b's job taken, scrubbed of scrub, and read
// to its end: the archive the take handed back, as its take describes it.
func takenWhole(t *testing.T, runner *executor.Runner, b sandbox.Sandbox, path string,
	scrub ...string) (sandbox.SessionArchive, []byte) {

	t.Helper()
	taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Save: path, Scrub: scrub,
		MaxBytes: takeBound})
	if err != nil || taken.Body == nil {
		t.Fatalf("the take of the save at %s = %+v, %v", path, taken, err)
	}
	archive, err := io.ReadAll(taken.Body)
	_ = taken.Body.Close()
	if err != nil {
		t.Fatalf("reading the save taken: %v", err)
	}
	return taken.Archive, archive
}

// holdingsOf is the reading of archive, a save taken as described, for the
// two subagents' calls, by the release that resumes it.
func holdingsOf(t *testing.T, runner *executor.Runner, described sandbox.SessionArchive,
	archive []byte) sandbox.SessionHoldings {

	t.Helper()
	got, err := runner.ReadHoldings(t.Context(), described, func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(archive)), nil
	}, sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{gBackgroundCall, gForegroundCall}, Resumer: rhResumer})
	if err != nil || !got.Read || got.Kind != sandbox.HoldingsByID {
		t.Fatalf("the reading of the save = %+v, %v; want a reading of its records filed by id", got, err)
	}
	return got
}

// A SAVE MADE DURING A SUBAGENT'S WORK IS TAKEN, AND TOLD WHAT THE SUBAGENT DID.
// P2(g)'s session is saved while the background subagent's last line has not
// ended: the pack holds, the thread having only grown; the take is scrubbed of
// the value the box was handed; and the reading holds the foreground
// subagent's answered call and tells the background one's under its heading,
// a line not ended being no record its resumed harness loads. Saved again once
// the line has landed, both calls are held.
//
// Mutation: count a torn last line of a subagent's thread as a record, and the
// first save's reading holds the background subagent's call; leave the
// subagents' directory out of the pack, and neither save's reading holds the
// foreground subagent's call.
func TestASaveDuringASubagentIsTakenAndToldWhatTheSubagentDid(t *testing.T) {
	t.Parallel()
	runner, b := sessionBox(t, "2.1.310")
	_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	p := paths(b)
	thread, whole := fileMeasuredSession(t, b, ran.SessionID)
	const secret = "the-value-the-box-was-handed-7c1e"
	spilled := p.ClaudeConfigDir() + "/projects/" + executor.ClaudeProjectDir + "/" + ran.SessionID +
		"/tool-results/handed.txt"
	if err := b.WriteFile(t.Context(), spilled, []byte("a tool printed "+secret+"\n")); err != nil {
		t.Fatal(err)
	}
	// THE THREAD'S LAST LINE BREAK LANDS once the first save's tar has read it,
	// and never again.
	landed := shQuote(b.Home() + "/line-break-landed")
	writeBoxStandIn(t, b, p.BinDir()+"/tar", rewritingTar(t, afterTar(`[ -e `+landed+` ] || { printf '\n' >> `+
		shQuote(thread)+` && : > `+landed+`; }`)))
	threadMember := strings.TrimPrefix(thread, p.ClaudeConfigDir()+"/")
	spilledMember := strings.TrimPrefix(spilled, p.ClaudeConfigDir()+"/")

	saved, err := runner.PackSave(t.Context(), b, takerA)
	if err != nil {
		t.Fatalf("the save made as a subagent's thread grew was refused: %v", err)
	}
	if got := boxFile(t, b, thread); got != whole {
		t.Fatal("the subagent's last line break never landed after the save's tar read its thread")
	}
	described, archive := takenWhole(t, runner, b, saved.Path, secret)
	members := archiveMembers(t, bytes.NewReader(archive))
	if got := members[threadMember]; got != strings.TrimSuffix(whole, "\n") {
		t.Errorf("the save holds the background subagent's thread as %d bytes, want it as tar read it, its "+
			"last line not yet ended (%d bytes)", len(got), len(whole)-1)
	}
	if _, ok := members[spilledMember]; !ok {
		t.Errorf("the save holds no %s, the file the box's value was written to", spilledMember)
	}
	for name, content := range members {
		if strings.Contains(content, secret) {
			t.Errorf("the save taken holds the value the box was handed, in %s", name)
		}
	}
	got := holdingsOf(t, runner, described, archive)
	if held, under := got.Holds(gForegroundCall); !held || under != "" || !got.Subagents[gForeground].Reported {
		t.Errorf("the foreground subagent's call: Holds = %t under %q, the subagent %+v; want it held",
			held, under, got.Subagents[gForeground])
	}
	report := got.Subagents[gBackground]
	if held, under := got.Holds(gBackgroundCall); held || under != gBackground || report.Reported ||
		!strings.Contains(report.Why, "its thread is not whole") {
		t.Errorf("the background subagent's call: Holds = %t under %q, the subagent %+v; want it told under "+
			"that subagent, its thread not whole", held, under, report)
	}

	// SAVED AGAIN once the line has landed, both calls are held.
	again, err := runner.PackSave(t.Context(), b, takerA)
	if err != nil {
		t.Fatalf("the save made once the subagent's line landed was refused: %v", err)
	}
	described, archive = takenWhole(t, runner, b, again.Path, secret)
	got = holdingsOf(t, runner, described, archive)
	for agent, call := range map[string]sandbox.LoggedCall{gBackground: gBackgroundCall,
		gForeground: gForegroundCall} {
		if held, under := got.Holds(call); !held || under != "" || !got.Subagents[agent].Reported {
			t.Errorf("once the line landed, %s's call: Holds = %t under %q, the subagent %+v; want it held",
				agent, held, under, got.Subagents[agent])
		}
	}
}
