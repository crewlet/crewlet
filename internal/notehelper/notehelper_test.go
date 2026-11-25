package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
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

// A NOTES ROUTE THAT REFUSES THE HELPER ENDS ONLY ITS POLLS: a 401 is a token
// that no longer resolves, so asking again would ask forever, but the cap and
// the skill reports go on, since a helper that ended would leave the turn
// unbounded. After the refusal, with a turn in flight and a note waiting, the
// cap and the skill reports work and the route is asked nothing more. The
// refusal's one line says note polling has ended, which only the path that
// ends the polls writes (a loop that polls on logs the refusal alone), so the
// case reads that line rather than a window.
//
// Mutation: end the helper on the notes route's refusal, or log it, back off
// and poll again, and this goes red.
func TestANotesRouteThatRefusesTheHelperEndsOnlyItsPolls(t *testing.T) {
	t.Parallel()
	bridge := newFakeBridge(t, testToken, 401)
	app := newFakeAppServer(t, "turn-1", 0)
	dir := filepath.Join(t.TempDir(), "skills")
	alpha := writeSkill(t, dir, "alpha", "# Alpha\n")
	threadFile := filepath.Join(t.TempDir(), "thread.id")
	writeThread(t, threadFile, "thread-A")
	h := startHelperWith(t, bridge, app, threadFile, append([]string{"-notes-env", "NOTES_ROUTE", "-step-cap", "2",
		"-skills-env", "SKILLS_ROUTE", "-skills-dir", dir}, digestFlags(alpha)...)...)
	h.stderr.await(func(s string) bool { return strings.Contains(s, "refused the helper") })
	if line := h.stderr.String(); !strings.Contains(line, "(401 Unauthorized): note polling has ended") {
		t.Fatalf("the refusal was logged as\n%s\nwant it to say note polling has ended", line)
	}
	bridge.add(note{Seq: 1, ID: "note-1", Message: "a note no poll should fetch"})
	tokens(t, app, "thread-A", "turn-1", 100)
	tokens(t, app, "thread-A", "turn-1", 200)
	interrupted := make(chan struct{})
	go func() {
		app.interrupts.await(func(c []interruptCall) bool { return len(c) == 1 })
		close(interrupted)
	}()
	select {
	case code := <-h.code:
		h.code <- code
		t.Fatalf("the helper ended with %d on the notes route's 401, its cap unheld:\n%s", code, h.stderr.String())
	case <-interrupted:
	}
	commandDone(t, app, "thread-A", 0, alpha.body, "/", action("read", alpha.path))
	bridge.skills.await(func(r []skillReport) bool { return len(r) == 1 })
	if code := h.stop(); code != exitDone {
		t.Errorf("exit status %d, want %d; stderr:\n%s", code, exitDone, h.stderr.String())
	}
	if p := bridge.polls.all(); len(p) != 1 {
		t.Errorf("the route was polled %d times, want once: a refusal ends the polls", len(p))
	}
	if s := app.steers.all(); len(s) != 0 {
		t.Errorf("the helper steered %d notes after the route refused it", len(s))
	}
	if n := strings.Count(h.stderr.String(), "refused the helper"); n != 1 {
		t.Errorf("the refusal was logged %d times, want once", n)
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
	app.joinedTurn()
	if v := bridge.verbs.all(); len(v) > 0 {
		t.Errorf("the helper polled %v with no turn in flight", v)
	}
	app.startTurn(t, "thread-A", "turn-9")
	if s := app.steers.await(func(s []steer) bool { return len(s) == 1 }); s[0].ExpectedTurnID != "turn-9" {
		t.Errorf("steered turn %q, want the one started, turn-9", s[0].ExpectedTurnID)
	}
}

// A NOTE STEERED AS ITS TURN ENDED WAITS FOR THE NEXT TURN, never handed
// over as taken: Codex answers "no active turn to steer", and the same note
// is steered onto the turn that starts next.
func TestANoteSteeredAsItsTurnEndedWaitsForTheNextTurn(t *testing.T) {
	t.Parallel()
	bridge := newFakeBridge(t, testToken)
	app := newFakeAppServer(t, "turn-1", 0)
	threadFile := filepath.Join(t.TempDir(), "thread.id")
	writeThread(t, threadFile, "thread-A")
	h := startHelper(t, bridge, app, threadFile)
	bridge.verbs.await(func(v []string) bool { return len(v) > 0 })
	// The turn ends with its notice not yet sent, and a note arrives.
	app.setTurn("")
	bridge.add(note{Seq: 1, ID: "note-1", Message: "late words"})
	app.steers.await(func(s []steer) bool { return len(s) == 1 })
	app.startTurn(t, "thread-A", "turn-2")
	steers := app.steers.await(func(s []steer) bool { return len(s) == 2 })
	if steers[1].ExpectedTurnID != "turn-2" || steers[1].Input[0].Text != "late words" {
		t.Errorf("second steer %+v: want the same note on turn-2", steers[1])
	}
	polls := bridge.polls.await(func(p []poll) bool { return slices.ContainsFunc(p, func(p poll) bool { return p.after == 1 }) })
	if n := len(slices.DeleteFunc(slices.Clone(polls), func(p poll) bool { return p.after != 1 })); n != 1 {
		t.Errorf("polls %+v: the cursor passed the note %d times, want once, after turn-2 took it", polls, n)
	}
	if strings.Contains(h.stderr.String(), "refused") {
		t.Errorf("a steer of an ended turn was logged as a refusal:\n%s", h.stderr.String())
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
// exit 2, and none prints the route. A digest of the right length that is
// not hex is refused by the hex check alone. Each run is bounded
// ([refusalBound]), so a helper that runs on instead fails the exit status.
//
// Mutation: drop any of the command line's checks, the hex decode's or a
// skill named twice included, and its row goes red.
func TestTheHelperRefusesACommandLineItCannotRunWith(t *testing.T) {
	t.Parallel()
	route := "http://127.0.0.1:1/mcp/" + testToken + "/steer"
	full := []string{"-adapter", "codex-app-server", "-socket", "/nowhere/s", "-thread-file", "/nowhere/t", "-notes-env", "NOTES_ROUTE"}
	cases := map[string]struct {
		args []string
		env  string
	}{
		"the route on the command line": {append(slices.Clone(full), "-notes", route), route},
		"nothing to do":                 {full[:6], route},
		"-cap-marker with no cap":       {append(slices.Clone(full), "-cap-marker", "/nowhere/m"), route},
		"a cap below 0":                 {append(slices.Clone(full), "-step-cap", "-1"), route},
		"-skills-env with no dir":       {append(slices.Clone(full), "-skills-env", "NOTES_ROUTE"), route},
		"-skills-dir with no route":     {append(slices.Clone(full), "-skills-dir", "/nowhere/skills"), route},
		"an empty route":                {full, ""},
		"a route that is no address":    {full, "mcp/" + testToken},
		"an adapter it does not have":   {append([]string{"-adapter", "other"}, full[2:]...), route},
		"no -socket":                    {append(slices.Clone(full[:2]), full[4:]...), route},
		"no -thread-file":               {append(slices.Clone(full[:4]), full[6:]...), route},
		"skills with no digests": {append(slices.Clone(full), "-skills-env", "NOTES_ROUTE", "-skills-dir",
			"/nowhere/skills"), route},
		"a digest with no skills": {append(slices.Clone(full), "-skill-digest", "a="+strings.Repeat("0", 64)), route},
		"a digest it cannot read": {append(slices.Clone(full), "-skills-env", "NOTES_ROUTE", "-skills-dir",
			"/nowhere/skills", "-skill-digest", "a=not-hex"), route},
		"a digest of its length that is not hex": {append(slices.Clone(full), "-skills-env", "NOTES_ROUTE",
			"-skills-dir", "/nowhere/skills", "-skill-digest", "a="+strings.Repeat("g", 64)), route},
		"a skill named twice": {append(slices.Clone(full), "-skills-env", "NOTES_ROUTE", "-skills-dir",
			"/nowhere/skills", "-skill-digest", "a="+strings.Repeat("0", 64), "-skill-digest",
			"a="+strings.Repeat("1", 64)), route},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stderr lockedBuffer
			env := func(string) string { return c.env }
			ctx, cancel := context.WithTimeout(t.Context(), refusalBound)
			defer cancel()
			if code := run(ctx, c.args, env, &stderr, quickPacing); code != exitUsage {
				t.Errorf("exit status %d, want %d; stderr:\n%s", code, exitUsage, stderr.String())
			}
			if strings.Contains(stderr.String(), testToken) {
				t.Errorf("stderr carries the route's token:\n%s", stderr.String())
			}
		})
	}
}

// THE STEP CAP INTERRUPTS THE TURN AT ITS NTH RESPONSE, MARKED FIRST, WITH NO
// NOTES ROUTE: with -step-cap 2 and no -notes-env the second response of the
// turn in flight sends turn/interrupt naming the thread and the turn, with
// -cap-marker already on disk holding the turn, and no route is ever polled.
// Every response Codex sends once the helper's list of turns is answered
// counts: the turn it lists is in flight before the reader reads on.
//
// Mutation: take the list's answer outside the reader's order (after the call
// returns), and the responses can arrive with no turn in flight and the cap
// never fires.
func TestTheStepCapInterruptsTheTurnAtItsNthResponse(t *testing.T) {
	t.Parallel()
	bridge := newFakeBridge(t, testToken)
	app := newFakeAppServer(t, "turn-1", 0)
	marker := filepath.Join(t.TempDir(), "capped")
	app.set("", marker)
	threadFile := filepath.Join(t.TempDir(), "thread.id")
	writeThread(t, threadFile, "thread-A")
	h := startHelperWith(t, bridge, app, threadFile, "-step-cap", "2", "-cap-marker", marker)
	app.joinedTurn()
	tokens(t, app, "thread-A", "turn-1", 100)
	tokens(t, app, "thread-A", "turn-1", 200)
	calls := app.interrupts.await(func(c []interruptCall) bool { return len(c) == 1 })
	if c := calls[0]; c.ThreadID != "thread-A" || c.TurnID != "turn-1" || !c.marked {
		t.Errorf("interrupt %+v: want thread-A and turn-1, the marker written first", c)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "turn-1\n" {
		t.Errorf("marker %q (%v), want turn-1", data, err)
	}
	if code := h.stop(); code != exitDone {
		t.Errorf("exit status %d, want %d; stderr:\n%s", code, exitDone, h.stderr.String())
	}
	if v := bridge.verbs.all(); len(v) > 0 {
		t.Errorf("a cap-only helper asked the notes route %v", v)
	}
}

// -INTERRUPT-ONCE EXITS 0 ONLY WHERE IT CUT A TURN SHORT: it interrupts the
// turn in flight and exits 0; with no turn to interrupt, none listed (no
// interrupt sent) or one listed that ended before the interrupt reached it,
// it exits 3, so the run's guard stops the harness before a later turn runs
// with no helper bounding it. With no thread file it cannot tell, and so does
// an interrupt Codex failed: it exits 1.
//
// Mutation: exit 0 where no turn was in flight, and the "no turn" rows go red.
func TestInterruptOnceExitsZeroOnlyWhereItCutATurnShort(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		turn, listed    string
		noThread, fails bool
		code            int
		interrupts      []string
	}{
		"a turn in flight":            {turn: "turn-5", code: exitDone, interrupts: []string{"turn-5"}},
		"no turn":                     {code: exitNothing},
		"a turn that ended meanwhile": {listed: "turn-5", code: exitNothing, interrupts: []string{"turn-5"}},
		"no thread file":              {turn: "turn-5", noThread: true, code: exitError},
		"an interrupt that failed":    {turn: "turn-5", fails: true, code: exitError, interrupts: []string{"turn-5"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app := newFakeAppServer(t, c.turn, 0)
			app.set(c.listed, "")
			if c.fails {
				app.failInterrupts()
			}
			threadFile := filepath.Join(t.TempDir(), "thread.id")
			if !c.noThread {
				writeThread(t, threadFile, "thread-A")
			}
			var stderr lockedBuffer
			args := []string{"-adapter", "codex-app-server", "-socket", app.path, "-thread-file", threadFile, "-interrupt-once"}
			if code := run(context.Background(), args, func(string) string { return "" }, &stderr, quickPacing); code != c.code {
				t.Errorf("exit status %d, want %d; stderr:\n%s", code, c.code, stderr.String())
			}
			var got []string
			for _, i := range app.interrupts.all() {
				if i.ThreadID != "thread-A" {
					t.Errorf("an interrupt named thread %q, want thread-A", i.ThreadID)
				}
				got = append(got, i.TurnID)
			}
			if !slices.Equal(got, c.interrupts) {
				t.Errorf("interrupted %v, want %v", got, c.interrupts)
			}
		})
	}
}

// A SKILL LOAD IS REPORTED TO THE SKILL ROUTE, WHICH IS NEVER PRINTED: a
// whole read of a skill's SKILL.md posts {"skill": name} under the run's
// token, and a report the route turns away is logged naming "the skill
// route" alone.
func TestASkillLoadIsReportedAndTheSkillRouteNeverPrinted(t *testing.T) {
	t.Parallel()
	bridge := newFakeBridge(t, testToken)
	bridge.skillStatus.Store(http.StatusInternalServerError)
	app := newFakeAppServer(t, "turn-1", 0)
	dir := filepath.Join(t.TempDir(), "skills")
	alpha := writeSkill(t, dir, "alpha", "# Alpha\n")
	beta := writeSkill(t, dir, "beta", "# Beta\n")
	threadFile := filepath.Join(t.TempDir(), "thread.id")
	writeThread(t, threadFile, "thread-A")
	h := startHelperWith(t, bridge, app, threadFile, append([]string{"-skills-env", "SKILLS_ROUTE", "-skills-dir",
		dir}, digestFlags(alpha, beta)...)...)
	app.joinedTurn()
	commandDone(t, app, "thread-A", 0, alpha.body, "/", action("read", alpha.path))
	commandDone(t, app, "thread-A", 0, beta.body, "/", action("read", beta.path))
	// One report at a time: beta's arrival is behind alpha's log line.
	reports := bridge.skills.await(func(r []skillReport) bool { return len(r) == 2 })
	for i, want := range []string{"alpha", "beta"} {
		if r := reports[i]; r.token != testToken || r.method != http.MethodPost || r.skill != want {
			t.Errorf("report %d %+v: want a POST of %s under the run's token", i, r, want)
		}
	}
	h.stop()
	stderr := h.stderr.String()
	if !strings.Contains(stderr, "reporting skill alpha to the skill route: it answered 500") {
		t.Errorf("stderr does not log alpha's refused report:\n%s", stderr)
	}
	if strings.Contains(stderr, testToken) || strings.Contains(stderr, bridge.srv.URL) {
		t.Errorf("stderr carries the skill route or its token:\n%s", stderr)
	}
}

// refusalBound is how long a command line's refusal may take: a liveness
// bound, never a verdict. A refusal returns before the helper waits on
// anything, so it is never reached; a helper that runs on past a check waits
// on the box's files instead, and this ends it with exit 0, which the case's
// exit-status assertion fails, rather than the package's timeout.
const refusalBound = 10 * time.Second

// THE CAP IS THE RUN'S, NOT A TURN'S: with -step-cap 1 the first turn's
// response interrupts it, and a turn a note opens after that is interrupted
// as it starts, before any response of its own, its marker naming it.
//
// Mutation: report no turn's start to the cap, or count each turn from one,
// and the second turn runs on uninterrupted.
func TestTheStepCapInterruptsATurnOpenedPastTheRunsCap(t *testing.T) {
	t.Parallel()
	bridge := newFakeBridge(t, testToken)
	app := newFakeAppServer(t, "turn-1", 0)
	marker := filepath.Join(t.TempDir(), "capped")
	app.set("", marker)
	threadFile := filepath.Join(t.TempDir(), "thread.id")
	writeThread(t, threadFile, "thread-A")
	h := startHelperWith(t, bridge, app, threadFile, "-step-cap", "1", "-cap-marker", marker)
	app.joinedTurn()
	tokens(t, app, "thread-A", "turn-1", 100)
	app.interrupts.await(func(c []interruptCall) bool { return len(c) == 1 })
	app.completeTurn(t, "thread-A", "turn-1")
	app.startTurn(t, "thread-A", "turn-2")
	calls := app.interrupts.await(func(c []interruptCall) bool { return len(c) == 2 })
	if c := calls[1]; c.ThreadID != "thread-A" || c.TurnID != "turn-2" || !c.marked {
		t.Errorf("interrupt %+v: want turn-2 of thread-A as it started, the marker written first", c)
	}
	if code := h.stop(); code != exitDone {
		t.Errorf("exit status %d, want %d; stderr:\n%s", code, exitDone, h.stderr.String())
	}
}
