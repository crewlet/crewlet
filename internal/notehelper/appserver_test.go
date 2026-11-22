package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// joined is the adapter joined to app's thread "thread-A", reporting nothing.
func joined(t *testing.T, app *fakeAppServer) *appServer {
	t.Helper()
	return joinedWith(t, app, events{}, "")
}

// joinedWith is joined reporting ev, the run's skills in skillsDir.
func joinedWith(t *testing.T, app *fakeAppServer, ev events, skillsDir string) *appServer {
	t.Helper()
	conn, err := dialSocket(context.Background(), app.path, quickPacing)
	if err != nil {
		t.Fatal(err)
	}
	a, err := joinAppServer(context.Background(), conn, "thread-A", quickPacing, ev, skillsDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.close)
	return a
}

// turnNow is the adapter's turn in flight.
func turnNow(a *appServer) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active
}

// awaitTurn blocks until the adapter's turn in flight is want.
func awaitTurn(a *appServer, want string) {
	for {
		a.mu.Lock()
		active, changed := a.active, a.changed
		a.mu.Unlock()
		if active == want {
			return
		}
		<-changed
	}
}

// fence answers once the app-server has read everything sent before it: the
// socket is one ordered stream, read by one loop.
func fence(t *testing.T, a *appServer) {
	t.Helper()
	if _, err := a.call(context.Background(), "fence", map[string]any{}); err != nil {
		t.Fatal(err)
	}
}

// JOINING TAKES THE TURN IN FLIGHT FROM THE NEWEST LISTED TURN: the resume
// asks for no history (excludeTurns, the full one deprecated), and
// thread/turns/list, newest first, names the turn while it is in progress;
// a newest turn completed is no turn at all.
func TestJoiningTakesTheTurnInFlightFromTheNewestListedTurn(t *testing.T) {
	t.Parallel()
	app := newFakeAppServer(t, "turn-7", 0)
	a := joined(t, app)
	if got := turnNow(a); got != "turn-7" {
		t.Errorf("joined with turn %q in flight, want the newest listed, turn-7", got)
	}
	if r := app.resumed.all(); len(r) != 1 || r[0].ThreadID != "thread-A" || !r[0].ExcludeTurns {
		t.Errorf("resumed %+v, want thread-A with excludeTurns", r)
	}

	idle := newFakeAppServer(t, "", 0)
	if got := turnNow(joined(t, idle)); got != "" {
		t.Errorf("joined with turn %q in flight where the newest listed turn completed, want none", got)
	}
}

// A STEER NAMES THE TURN IN FLIGHT, AND NONE IS SENT BETWEEN TURNS: the turn
// is the one listed at the join, then each turn/started's; turn/completed
// leaves none, and a note handed over then is not sent.
func TestASteerNamesTheTurnInFlightAndNoneIsSentBetweenTurns(t *testing.T) {
	t.Parallel()
	app := newFakeAppServer(t, "turn-1", 0)
	a := joined(t, app)
	ctx := context.Background()
	if err := a.deliver(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	app.completeTurn(t, "thread-A", "turn-1")
	awaitTurn(a, "")
	if err := a.deliver(ctx, "between"); !errors.Is(err, errNotNow) {
		t.Errorf("deliver between turns = %v, want errNotNow", err)
	}
	app.startTurn(t, "thread-A", "turn-2")
	awaitTurn(a, "turn-2")
	if err := a.deliver(ctx, "two"); err != nil {
		t.Fatal(err)
	}
	fence(t, a)
	steers := app.steers.all()
	if len(steers) != 2 || steers[0].ExpectedTurnID != "turn-1" || steers[1].ExpectedTurnID != "turn-2" ||
		steers[1].Input[0].Text != "two" {
		t.Errorf("steers %+v: want \"one\" on turn-1 and \"two\" on turn-2, nothing between", steers)
	}
}

// A STEER OF THE WRONG TURN IS STEERED ONCE MORE, ONTO THE TURN CODEX NAMES:
// "expected active turn id `X` but found `Y`" makes Y the turn in flight and
// the same note goes there; X never comes back.
func TestASteerOfTheWrongTurnGoesOnceMoreToTheTurnCodexNames(t *testing.T) {
	t.Parallel()
	app := newFakeAppServer(t, "turn-1", 0)
	a := joined(t, app)
	app.setTurn("turn-2")
	if err := a.deliver(context.Background(), "words"); err != nil {
		t.Fatalf("deliver = %v, want the note taken on turn-2", err)
	}
	steers := app.steers.all()
	if len(steers) != 2 || steers[0].ExpectedTurnID != "turn-1" || steers[1].ExpectedTurnID != "turn-2" ||
		steers[1].Input[0].Text != "words" {
		t.Errorf("steers %+v: want turn-1 refused, then the same note on turn-2", steers)
	}
	if got := turnNow(a); got != "turn-2" {
		t.Errorf("turn in flight %q, want the one Codex named, turn-2", got)
	}
	a.begin("turn-1")
	if got := turnNow(a); got != "turn-2" {
		t.Errorf("turn in flight %q after a late word of turn-1, want turn-2 still", got)
	}
}

// A STEER WITH NO TURN IN FLIGHT HOLDS THE NOTE: "no active turn to steer"
// is the turn over, so the note waits for the next turn (errNotNow), never
// counted as taken nor as refused.
func TestASteerWithNoTurnInFlightHoldsTheNote(t *testing.T) {
	t.Parallel()
	app := newFakeAppServer(t, "turn-1", 0)
	a := joined(t, app)
	app.setTurn("")
	if err := a.deliver(context.Background(), "words"); !errors.Is(err, errNotNow) {
		t.Errorf("deliver with no turn in flight = %v, want errNotNow", err)
	}
	if got := turnNow(a); got != "" {
		t.Errorf("turn in flight %q after Codex said there is none, want none", got)
	}
}

// ANY OTHER REFUSAL IS THE NOTE REFUSED: an error the helper logs and a
// later poll hands over again, the turn in flight unchanged.
func TestAnyOtherRefusalIsTheNoteRefused(t *testing.T) {
	t.Parallel()
	app := newFakeAppServer(t, "turn-1", 1)
	a := joined(t, app)
	err := a.deliver(context.Background(), "words")
	var refused *rpcError
	if !errors.As(err, &refused) || errors.Is(err, errNotNow) {
		t.Errorf("deliver = %v, want the app-server's refusal", err)
	}
	if got := turnNow(a); got != "turn-1" {
		t.Errorf("turn in flight %q after a refusal, want turn-1 still", got)
	}
}

// A TURN IS FOLLOWED FROM ITS OWN THREAD'S NOTICES ONLY, AND A FINISHED ONE
// NEVER REOPENS: a subagent's thread's turn/completed ends nothing of the
// run's, and a word of a completed turn arriving late (a turns list answered
// after the turn's turn/completed) starts nothing.
func TestATurnIsFollowedFromItsOwnThreadsNoticesOnly(t *testing.T) {
	t.Parallel()
	app := newFakeAppServer(t, "", 0)
	a := joined(t, app)
	app.startTurn(t, "thread-A", "turn-1")
	awaitTurn(a, "turn-1")
	app.notify(t, "turn/completed", map[string]any{"threadId": "thread-sub", "turn": map[string]any{"id": "turn-1"}})
	app.startTurn(t, "thread-A", "turn-2")
	awaitTurn(a, "turn-2")
	// Read in order, so the subagent's notice is behind: had it ended turn-1,
	// turn-1 would never be the turn in flight again.
	app.startTurn(t, "thread-A", "turn-1")
	awaitTurn(a, "turn-1")

	app.completeTurn(t, "thread-A", "turn-1")
	awaitTurn(a, "")
	a.begin("turn-1")
	if got := turnNow(a); got != "" {
		t.Errorf("turn in flight %q after a late word of the completed turn-1, want none", got)
	}
}

// foundTurn reads exactly Codex's wording, and nothing else as a turn.
func TestFoundTurnReadsCodexsRefusalOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg, named, found string
		ok                bool
	}{
		{"expected active turn id `a-1` but found `b-2`", "a-1", "b-2", true},
		{"expected active turn id `a-1` but found `b-2` and more", "", "", false},
		{"no active turn to steer", "", "", false},
		{"expected active turn id `a`1` but found `b`", "", "", false},
		{"expected active turn id a-1 but found b-2", "a-1", "b-2", true},
		{"expected active turn id a 1 but found b-2", "", "", false},
		{"expected active turn id a-1 but found ", "", "", false},
	}
	for _, c := range cases {
		named, found, ok := foundTurn(c.msg)
		if named != c.named || found != c.found || ok != c.ok {
			t.Errorf("foundTurn(%q) = %q, %q, %v; want %q, %q, %v", c.msg, named, found, ok, c.named, c.found, c.ok)
		}
	}
}

// READY HOLDS WHILE NO TURN IS IN FLIGHT, so the loop polls the route only
// while a note can be steered: asked under a context already ended, ready
// answers that context's error with no turn, and nil with one.
func TestReadyHoldsWhileNoTurnIsInFlight(t *testing.T) {
	t.Parallel()
	app := newFakeAppServer(t, "", 0)
	a := joined(t, app)
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.ready(ended); !errors.Is(err, context.Canceled) {
		t.Errorf("ready with no turn in flight = %v, want it held until its context ended", err)
	}
	app.startTurn(t, "thread-A", "turn-1")
	awaitTurn(a, "turn-1")
	if err := a.ready(ended); err != nil {
		t.Errorf("ready with turn-1 in flight = %v, want nil", err)
	}
	app.completeTurn(t, "thread-A", "turn-1")
	awaitTurn(a, "")
	if err := a.ready(ended); !errors.Is(err, context.Canceled) {
		t.Errorf("ready once turn-1 completed = %v, want it held again", err)
	}
}

// THE HELPER'S CONNECTION ENDS WITH THE APP-SERVER'S: ready answers errGone,
// so the loop exits rather than waiting on a turn that never comes.
func TestReadyAnswersGoneOnceTheAppServerHangsUp(t *testing.T) {
	t.Parallel()
	app := newFakeAppServer(t, "", 0)
	a := joined(t, app)
	app.hangUp()
	if err := a.ready(context.Background()); !errors.Is(err, errGone) {
		t.Errorf("ready = %v after the app-server hung up, want errGone", err)
	}
}

// tokens notifies a token usage update of thread's turn at total.
func tokens(t *testing.T, app *fakeAppServer, thread, turn string, total int) {
	t.Helper()
	app.notify(t, "thread/tokenUsage/updated", map[string]any{"threadId": thread, "turnId": turn,
		"tokenUsage": map[string]any{"total": map[string]any{"totalTokens": total}, "last": map[string]any{"totalTokens": 10}}})
}

// A MODEL RESPONSE IS A RISE OF THE THREAD'S TOKENS, IN THE TURN IN FLIGHT:
// each update whose total rose is one step of the turn in flight; a repeat of
// the same total, a lower one, another turn's or another thread's is none.
func TestAModelResponseIsARiseOfTheThreadsTokens(t *testing.T) {
	t.Parallel()
	app := newFakeAppServer(t, "turn-1", 0)
	var steps record[string]
	joinedWith(t, app, events{step: func(turn string) { steps.add(turn) }}, "")
	tokens(t, app, "thread-A", "turn-1", 100)
	tokens(t, app, "thread-A", "turn-1", 100)
	tokens(t, app, "thread-A", "turn-1", 90)
	tokens(t, app, "thread-sub", "turn-1", 500)
	tokens(t, app, "thread-A", "turn-x", 200)
	tokens(t, app, "thread-A", "turn-1", 300)
	if got := stepsUpTo(t, app, &steps); got != 2 {
		t.Errorf("turn-1 took %d steps, want two, for the totals 100 and 300", got)
	}
}

// stepsUpTo is how many steps of turn-1 were counted before a step of a new
// turn-2: notifications are read in order, so turn-2's step is behind every
// update sent before it.
func stepsUpTo(t *testing.T, app *fakeAppServer, steps *record[string]) int {
	t.Helper()
	app.startTurn(t, "thread-A", "turn-2")
	tokens(t, app, "thread-A", "turn-2", 1_000_000)
	got := steps.await(func(s []string) bool { return len(s) > 0 && s[len(s)-1] == "turn-2" })
	return len(got) - 1
}

// A COMPACTION'S RISE IS NO STEP: Codex's compaction request raises the
// thread's tokens inside its contextCompaction item's window, and is no model
// response; the rises before and after it are.
func TestACompactionsRiseIsNoStep(t *testing.T) {
	t.Parallel()
	app := newFakeAppServer(t, "turn-1", 0)
	var steps record[string]
	joinedWith(t, app, events{step: func(turn string) { steps.add(turn) }}, "")
	compaction := func(method string) {
		app.notify(t, method, map[string]any{"threadId": "thread-A", "turnId": "turn-1",
			"item": map[string]any{"type": "contextCompaction", "id": "c-1"}})
	}
	tokens(t, app, "thread-A", "turn-1", 100)
	compaction("item/started")
	tokens(t, app, "thread-A", "turn-1", 100)
	tokens(t, app, "thread-A", "turn-1", 700)
	compaction("item/completed")
	tokens(t, app, "thread-A", "turn-1", 700)
	tokens(t, app, "thread-A", "turn-1", 800)
	if got := stepsUpTo(t, app, &steps); got != 2 {
		t.Errorf("turn-1 took %d steps, want two, for 100 and 800, none for the compaction's 700", got)
	}
}

// AN INTERRUPT NAMES THE THREAD AND THE TURN, BOTH: the turn in flight where
// none is named; none sent with no turn in flight; and Codex's "no active
// turn to interrupt" is the turn over, not a failure.
func TestAnInterruptNamesTheThreadAndTheTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	app := newFakeAppServer(t, "turn-7", 0)
	a := joined(t, app)
	if err := a.interrupt(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if c := app.interrupts.all(); len(c) != 1 || c[0].ThreadID != "thread-A" || c[0].TurnID != "turn-7" {
		t.Errorf("interrupts %+v: want one naming thread-A and turn-7", c)
	}
	if err := a.interrupt(ctx, "turn-7"); !errors.Is(err, errNotNow) {
		t.Errorf("interrupt of an ended turn = %v, want errNotNow", err)
	}

	// Codex refuses a turn not in flight, naming the one that is, and
	// interrupts nothing: that turn is interrupted, once more.
	moved := newFakeAppServer(t, "turn-1", 0)
	m := joined(t, moved)
	moved.setTurn("turn-2")
	if err := m.interrupt(ctx, ""); err != nil {
		t.Fatalf("interrupt after the turn moved on = %v, want turn-2 interrupted", err)
	}
	if c := moved.interrupts.all(); len(c) != 2 || c[0].TurnID != "turn-1" || c[1].TurnID != "turn-2" {
		t.Errorf("interrupts %+v: want turn-1 refused, then turn-2", c)
	}
	if got := turnNow(m); got != "turn-2" {
		t.Errorf("turn in flight %q, want the one Codex named, turn-2", got)
	}

	idle := joined(t, newFakeAppServer(t, "", 0))
	if err := idle.interrupt(ctx, ""); !errors.Is(err, errNotNow) {
		t.Errorf("interrupt with no turn in flight = %v, want errNotNow", err)
	}
}

// commandDone is a commandExecution item/completed of thread-A: exit, output, cwd
// and its parsed actions.
func commandDone(t *testing.T, app *fakeAppServer, thread string, exit int, output, cwd string, actions ...map[string]any) {
	t.Helper()
	app.notify(t, "item/completed", map[string]any{"threadId": thread, "turnId": "turn-1", "item": map[string]any{
		"type": "commandExecution", "status": "completed", "exitCode": exit, "cwd": cwd,
		"aggregatedOutput": output, "commandActions": actions, "command": "/bin/bash -lc '…'"}})
}

// action is one parsed command action.
func action(kind, path string) map[string]any {
	return map[string]any{"type": kind, "command": kind, "path": path, "name": filepath.Base(path)}
}

// ONLY A WHOLE READ OF A SKILL'S OWN FILE IS A LOAD: a command that exited 0,
// whose one action reads <skills>/<name>/SKILL.md and whose output is that
// file byte for byte. A listing, a test, an echo, a failed or partial read,
// another file, a script, a link, two reads in one command or another
// thread's command is none.
func TestOnlyAWholeReadOfASkillsOwnFileIsALoad(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "skills")
	alpha := writeSkill(t, dir, "alpha", "# Alpha\n\nUse the alpha tools.\n")
	beta := writeSkill(t, dir, "beta", "# Beta\n")
	other := filepath.Join(dir, "alpha", "NOTES.md")
	if err := os.WriteFile(other, []byte(alpha.body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "gamma"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "gamma", "SKILL.md")
	if err := os.Symlink(alpha.path, link); err != nil {
		t.Fatal(err)
	}
	outside := writeSkill(t, filepath.Join(t.TempDir(), "elsewhere"), "alpha", alpha.body)

	app := newFakeAppServer(t, "turn-1", 0)
	var loads record[string]
	joinedWith(t, app, events{skill: func(name string) { loads.add(name) }}, dir)
	commandDone(t, app, "thread-A", 0, "SKILL.md\n", "/", action("listFiles", filepath.Dir(alpha.path)))
	commandDone(t, app, "thread-A", 0, "", "/", action("unknown", ""))
	commandDone(t, app, "thread-A", 0, alpha.path+"\n", "/", action("unknown", ""))
	commandDone(t, app, "thread-A", 1, "cat: no such file\n", "/", action("read", alpha.path))
	commandDone(t, app, "thread-A", 0, alpha.body, "/", action("read", other))
	commandDone(t, app, "thread-A", 0, alpha.body, "/", action("read", outside.path))
	commandDone(t, app, "thread-A", 0, "# Alpha\n", "/", action("read", alpha.path))
	commandDone(t, app, "thread-A", 0, alpha.body, "/", action("unknown", ""))
	commandDone(t, app, "thread-A", 0, alpha.body, "/", action("read", link))
	commandDone(t, app, "thread-A", 0, alpha.body+beta.body, "/", action("read", alpha.path), action("read", beta.path))
	commandDone(t, app, "thread-sub", 0, alpha.body, "/", action("read", alpha.path))
	// A relative path read in the skills directory, then another skill.
	commandDone(t, app, "thread-A", 0, alpha.body, dir, action("read", "alpha/SKILL.md"))
	commandDone(t, app, "thread-A", 0, beta.body, "/", action("read", beta.path))
	// Read in order: the last load is behind every command before it.
	got := loads.await(func(l []string) bool { return len(l) > 0 && l[len(l)-1] == "beta" })
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("loads %v: want alpha (the relative read) and beta, nothing else", got)
	}
}

// skillFile is one skill's SKILL.md as written.
type skillFile struct{ path, body string }

// writeSkill writes <dir>/<name>/SKILL.md holding body.
func writeSkill(t *testing.T, dir, name, body string) skillFile {
	t.Helper()
	path := filepath.Join(dir, name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return skillFile{path: path, body: body}
}

// skillOf takes exactly <dir>/<name>/SKILL.md, name one plain element.
func TestSkillOfTakesExactlyASkillsOwnFile(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/h/skills/alpha/SKILL.md":        "alpha",
		"/h/skills/alpha/sub/SKILL.md":    "",
		"/h/skills/SKILL.md":              "",
		"/h/skills/alpha/skill.md":        "",
		"/h/other/alpha/SKILL.md":         "",
		"/h/skills/../skills2/a/SKILL.md": "",
	}
	for path, want := range cases {
		got, ok := skillOf("/h/skills", filepath.Clean(path))
		if got != want || ok != (want != "") {
			t.Errorf("skillOf(%q) = %q, %v; want %q", path, got, ok, want)
		}
	}
}
