package main

import (
	"context"
	"errors"
	"testing"
)

// joined is the adapter joined to app's thread "thread-A".
func joined(t *testing.T, app *fakeAppServer) *appServer {
	t.Helper()
	conn, err := dialSocket(context.Background(), app.path, quickPacing)
	if err != nil {
		t.Fatal(err)
	}
	a, err := joinAppServer(context.Background(), conn, "thread-A", quickPacing)
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
	}
	for _, c := range cases {
		named, found, ok := foundTurn(c.msg)
		if named != c.named || found != c.found || ok != c.ok {
			t.Errorf("foundTurn(%q) = %q, %q, %v; want %q, %q, %v", c.msg, named, found, ok, c.named, c.found, c.ok)
		}
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
