package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// testToken is the run's bridge token in the fake route's path: the secret
// the helper must never print.
const testToken = "tok-5ecret-d0-not-print"

// TestMain runs the helper's own main where a case re-runs this binary as
// the helper (NOTEHELPER_TEST_ARGS, a JSON list of its arguments), so a case
// can signal a real process.
func TestMain(m *testing.M) {
	if raw := os.Getenv("NOTEHELPER_TEST_ARGS"); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			os.Exit(99)
		}
		os.Args = append([]string{os.Args[0]}, args...)
		main()
	}
	os.Exit(m.Run())
}

// A NOTE IS STEERED WHOLE ONTO THE RUN'S OWN THREAD AND THE TURN IN FLIGHT:
// the thread the run's script wrote, the turn the app-server says is in
// progress, the note's text byte for byte; and the cursor passes it.
func TestANoteIsSteeredWholeOntoTheRunsThreadAndTurn(t *testing.T) {
	t.Parallel()
	bridge := newFakeBridge(t, testToken)
	app := newFakeAppServer(t, "turn-7", 0)
	threadFile := filepath.Join(t.TempDir(), "thread.id")
	writeThread(t, threadFile, "thread-A")
	text := "Also update the README.\n\n  Two lines, \"quotes\", a tab\tand ünïcödé 🚀"
	bridge.add(note{Seq: 3, ID: "note-1", Message: text})
	h := startHelper(t, bridge, app, threadFile)

	steers := app.steers.await(func(s []steer) bool { return len(s) == 1 })
	got := steers[0]
	if got.ThreadID != "thread-A" || got.ExpectedTurnID != "turn-7" {
		t.Errorf("steered thread %q turn %q, want thread-A turn-7", got.ThreadID, got.ExpectedTurnID)
	}
	if len(got.Input) != 1 || got.Input[0].Type != "text" || got.Input[0].Text != text {
		t.Errorf("steered input %+v, want the one text item %q", got.Input, text)
	}
	bridge.polls.await(func(p []poll) bool { return slices.ContainsFunc(p, func(p poll) bool { return p.after == 3 }) })
	if code := h.stop(); code != exitDone {
		t.Errorf("exit status %d on a signal, want %d; stderr:\n%s", code, exitDone, h.stderr.String())
	}
}

// THE HELPER WAITS FOR THE THREAD FILE: it looks again while the file is
// missing or empty (the script writes it once the harness reports its
// thread), and takes the id once there.
func TestTheHelperWaitsForTheThreadFileToHoldAnID(t *testing.T) {
	t.Parallel()
	var reads record[int]
	answers := []func() ([]byte, error){
		func() ([]byte, error) { return nil, os.ErrNotExist },
		func() ([]byte, error) { return []byte(" \n"), nil },
		func() ([]byte, error) { return []byte("thread-B\n"), nil },
	}
	read := func(string) ([]byte, error) {
		n := len(reads.all())
		reads.add(n)
		return answers[min(n, len(answers)-1)]()
	}
	got, err := waitThread(context.Background(), "thread.id", read, quickPacing)
	if err != nil || got != "thread-B" {
		t.Fatalf("waitThread = %q, %v; want thread-B", got, err)
	}
	if n := len(reads.all()); n != 3 {
		t.Errorf("read the file %d times, want 3: past the missing and the empty file", n)
	}

	// End to end: a file written after the helper started is the thread steered.
	bridge := newFakeBridge(t, testToken)
	app := newFakeAppServer(t, "turn-1", 0)
	threadFile := filepath.Join(t.TempDir(), "thread.id")
	bridge.add(note{Seq: 1, ID: "note-1", Message: "hello"})
	startHelper(t, bridge, app, threadFile)
	writeThread(t, threadFile, "thread-late")
	if s := app.steers.await(func(s []steer) bool { return len(s) == 1 }); s[0].ThreadID != "thread-late" {
		t.Errorf("steered thread %q, want thread-late", s[0].ThreadID)
	}
}

// A REFUSED STEER IS HANDED OVER AGAIN, AND NOTHING IS EVER CONFIRMED: the
// cursor stays before a note the harness refused, so the next poll asks for
// it again; the helper never posts the route's read; and the error stream
// names the note by id, never by its text.
func TestARefusedSteerIsHandedOverAgainAndNothingIsConfirmed(t *testing.T) {
	t.Parallel()
	bridge := newFakeBridge(t, testToken)
	app := newFakeAppServer(t, "turn-1", 2)
	threadFile := filepath.Join(t.TempDir(), "thread.id")
	writeThread(t, threadFile, "thread-A")
	bridge.add(note{Seq: 1, ID: "note-1", Message: "a private remark"})
	h := startHelper(t, bridge, app, threadFile)

	steers := app.steers.await(func(s []steer) bool { return len(s) == 3 })
	for i, s := range steers {
		if s.Input[0].Text != "a private remark" {
			t.Errorf("steer %d carried %q, want the refused note again", i, s.Input[0].Text)
		}
	}
	polls := bridge.polls.await(func(p []poll) bool { return slices.ContainsFunc(p, func(p poll) bool { return p.after == 1 }) })
	first := slices.IndexFunc(polls, func(p poll) bool { return p.after == 1 })
	if first < 3 {
		t.Errorf("polls %+v: the cursor passed the note after %d polls, before its two refusals were asked again", polls, first)
	}
	if code := h.stop(); code != exitDone {
		t.Errorf("exit status %d, want %d", code, exitDone)
	}
	for _, v := range bridge.verbs.all() {
		if !strings.HasSuffix(v, " notes") {
			t.Errorf("the helper sent %q to the notes route; it only polls", v)
		}
	}
	stderr := h.stderr.String()
	if strings.Count(stderr, "refused note note-1") != 2 {
		t.Errorf("stderr names the refusal %d times, want 2:\n%s", strings.Count(stderr, "refused note note-1"), stderr)
	}
	if strings.Contains(stderr, "private remark") {
		t.Errorf("stderr carries the note's text:\n%s", stderr)
	}
}

// THE ROUTE IS NEVER PRINTED: it holds the run's token, and a failed poll
// whose error quotes the address (a dropped connection) or a refused status
// is logged naming "the notes route" alone.
func TestTheRouteIsNeverPrinted(t *testing.T) {
	t.Parallel()
	bridge := newFakeBridge(t, testToken, -1, 503)
	app := newFakeAppServer(t, "turn-1", 0)
	threadFile := filepath.Join(t.TempDir(), "thread.id")
	writeThread(t, threadFile, "thread-A")
	bridge.add(note{Seq: 1, ID: "note-1", Message: "hello"})
	h := startHelper(t, bridge, app, threadFile)
	app.steers.await(func(s []steer) bool { return len(s) == 1 })
	h.stop()

	stderr := h.stderr.String()
	// A dropped connection and a 503 (the transport answers a drop on a
	// fresh connection with the error that quotes the address).
	if n := strings.Count(stderr, "polling the notes route"); n != 2 {
		t.Errorf("stderr logs %d failed polls, want 2:\n%s", n, stderr)
	}
	if strings.Contains(stderr, testToken) || strings.Contains(stderr, bridge.srv.URL) {
		t.Errorf("stderr carries the route or its token:\n%s", stderr)
	}
}

// THE POLLS CARRY THE RUN'S TOKEN, AND A 503 IS PAUSED ON, NOT THE END: the
// bridge turns a poll away with a 503 while an opening ends or a node stops,
// and the helper asks again and hands the note over.
func TestThePollsCarryTheRunsTokenAndAskAgainAfterA503(t *testing.T) {
	t.Parallel()
	bridge := newFakeBridge(t, testToken, 503, 503)
	app := newFakeAppServer(t, "turn-1", 0)
	threadFile := filepath.Join(t.TempDir(), "thread.id")
	writeThread(t, threadFile, "thread-A")
	bridge.add(note{Seq: 1, ID: "note-1", Message: "hello"})
	h := startHelper(t, bridge, app, threadFile)
	app.steers.await(func(s []steer) bool { return len(s) == 1 })
	h.stop()
	polls := bridge.polls.all()
	if len(polls) < 3 {
		t.Fatalf("polls %+v: want two turned away and one answered", polls)
	}
	for _, p := range polls {
		if p.token != testToken {
			t.Errorf("a poll carried token %q, want the run's", p.token)
		}
	}
}

// A ROUTE THAT REFUSES THE HELPER ENDS IT: a 401 is a token that no longer
// resolves, and asking again would ask forever.
func TestARouteThatRefusesTheHelperEndsIt(t *testing.T) {
	t.Parallel()
	bridge := newFakeBridge(t, testToken, 401)
	app := newFakeAppServer(t, "turn-1", 0)
	threadFile := filepath.Join(t.TempDir(), "thread.id")
	writeThread(t, threadFile, "thread-A")
	h := startHelper(t, bridge, app, threadFile)
	if code := <-h.code; code != exitError {
		t.Errorf("exit status %d on a 401, want %d", code, exitError)
	}
	h.code <- exitError
	if !strings.Contains(h.stderr.String(), "refused the helper") {
		t.Errorf("stderr does not say the route refused it:\n%s", h.stderr.String())
	}
}

// THE HELPER ENDS, AT ONCE AND CLEANLY, WHEN THE APP-SERVER HANGS UP: the
// run is over. A poll held by the bridge is let go rather than waited out
// (the case's poll bound is an hour).
func TestTheHelperEndsCleanlyWhenTheAppServerHangsUp(t *testing.T) {
	t.Parallel()
	bridge := newFakeBridge(t, testToken)
	app := newFakeAppServer(t, "turn-1", 0)
	threadFile := filepath.Join(t.TempDir(), "thread.id")
	writeThread(t, threadFile, "thread-A")
	h := startHelper(t, bridge, app, threadFile)
	// A poll is held: nothing waits on the route.
	bridge.verbs.await(func(v []string) bool { return len(v) > 0 })
	app.hangUp()
	if code := <-h.code; code != exitDone {
		t.Errorf("exit status %d once the app-server hung up, want %d; stderr:\n%s", code, exitDone, h.stderr.String())
	}
	h.code <- exitDone
}

// NOTES ARE POLLED ONLY WHILE A TURN IS IN FLIGHT: a thread with no turn
// running asks the route for nothing, and the turn that starts is the one
// steered.
func TestNotesWaitForATurnInFlight(t *testing.T) {
	t.Parallel()
	bridge := newFakeBridge(t, testToken)
	app := newFakeAppServer(t, "", 0)
	threadFile := filepath.Join(t.TempDir(), "thread.id")
	writeThread(t, threadFile, "thread-A")
	bridge.add(note{Seq: 1, ID: "note-1", Message: "hello"})
	startHelper(t, bridge, app, threadFile)
	app.resumed.await(func(r []string) bool { return len(r) == 1 })
	if v := bridge.verbs.all(); len(v) > 0 {
		t.Errorf("the helper polled %v with no turn in flight", v)
	}
	app.notify(t, "turn/started", map[string]any{"threadId": "thread-A", "turn": map[string]any{"id": "turn-9"}})
	if s := app.steers.await(func(s []steer) bool { return len(s) == 1 }); s[0].ExpectedTurnID != "turn-9" {
		t.Errorf("steered turn %q, want the one started, turn-9", s[0].ExpectedTurnID)
	}
}

// SIGTERM AND SIGINT END THE HELPER WITH EXIT 0: the run's script stops it
// with the harness, and a stop is not an error in the run's error stream.
func TestASignalEndsTheHelperWithExitZero(t *testing.T) {
	t.Parallel()
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Parallel()
			bridge := newFakeBridge(t, testToken)
			app := newFakeAppServer(t, "turn-1", 0)
			threadFile := filepath.Join(t.TempDir(), "thread.id")
			writeThread(t, threadFile, "thread-A")
			args, _ := json.Marshal([]string{"-adapter", "codex-app-server", "-socket", app.path,
				"-thread-file", threadFile, "-notes-env", "NOTES_ROUTE"})
			cmd := exec.Command(os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), "NOTEHELPER_TEST_ARGS="+string(args), "NOTES_ROUTE="+bridge.route())
			var stderr lockedBuffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// The first poll comes after main armed its signals.
			bridge.verbs.await(func(v []string) bool { return len(v) > 0 })
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Errorf("the helper ended with %v on %s, want exit 0; stderr:\n%s", err, sig, stderr.String())
			}
			// The route reached the helper by its environment alone.
			if strings.Contains(string(args), testToken) || strings.Contains(stderr.String(), testToken) {
				t.Errorf("the route's token is on the helper's command line %s or in its stderr:\n%s", args, stderr.String())
			}
		})
	}
}

// THE HELPER STARTS ONLY WITH WHAT IT NEEDS, AND TAKES NO ROUTE ON ITS
// COMMAND LINE, which every process in the box can read: each refusal is
// exit 2, and none prints the route.
func TestTheHelperRefusesACommandLineItCannotRunWith(t *testing.T) {
	t.Parallel()
	route := "http://127.0.0.1:1/mcp/" + testToken + "/steer"
	full := []string{"-adapter", "codex-app-server", "-socket", "/nowhere/s", "-thread-file", "/nowhere/t", "-notes-env", "NOTES_ROUTE"}
	cases := map[string]struct {
		args []string
		env  string
	}{
		"the route on the command line": {append(slices.Clone(full), "-notes", route), route},
		"no -notes-env":                 {full[:6], route},
		"an empty route":                {full, ""},
		"a route that is no address":    {full, "mcp/" + testToken},
		"an adapter it does not have":   {append([]string{"-adapter", "other"}, full[2:]...), route},
		"no -socket":                    {append(slices.Clone(full[:2]), full[4:]...), route},
		"no -thread-file":               {append(slices.Clone(full[:4]), full[6:]...), route},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stderr lockedBuffer
			env := func(string) string { return c.env }
			if code := run(context.Background(), c.args, env, &stderr, quickPacing); code != exitUsage {
				t.Errorf("exit status %d, want %d; stderr:\n%s", code, exitUsage, stderr.String())
			}
			if strings.Contains(stderr.String(), testToken) {
				t.Errorf("stderr carries the route's token:\n%s", stderr.String())
			}
		})
	}
}
