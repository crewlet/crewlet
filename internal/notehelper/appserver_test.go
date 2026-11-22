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

// A STEER NAMES THE TURN IN FLIGHT, AND NONE IS SENT BETWEEN TURNS: the turn
// is the resumed thread's in-progress one, then each turn/started's;
// turn/completed leaves none, and a note handed over then is not sent (Codex
// would refuse a steer of a finished turn, and the note waits for the next).
func TestASteerNamesTheTurnInFlightAndNoneIsSentBetweenTurns(t *testing.T) {
	t.Parallel()
	app := newFakeAppServer(t, "turn-1", 0)
	a := joined(t, app)
	ctx := context.Background()
	if err := a.deliver(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	app.notify(t, "turn/completed", map[string]any{"threadId": "thread-A", "turn": map[string]any{"id": "turn-1"}})
	awaitTurn(a, "")
	if err := a.deliver(ctx, "between"); !errors.Is(err, errNotNow) {
		t.Errorf("deliver between turns = %v, want errNotNow", err)
	}
	app.notify(t, "turn/started", map[string]any{"threadId": "thread-A", "turn": map[string]any{"id": "turn-2"}})
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

// A TURN IS FOLLOWED FROM ANY NOTIFICATION NAMING IT, NEVER ANOTHER
// THREAD'S, AND A FINISHED ONE NEVER REOPENS: a helper that joined after
// turn/started learns the turn from the next item; a late notification of a
// turn that completed, or one of a subagent's thread, opens nothing.
func TestATurnIsFollowedFromItsOwnThreadsNotificationsOnly(t *testing.T) {
	t.Parallel()
	app := newFakeAppServer(t, "", 0)
	a := joined(t, app)
	app.notify(t, "item/started", map[string]any{"threadId": "thread-sub", "turnId": "turn-sub"})
	app.notify(t, "item/started", map[string]any{"threadId": "thread-A", "turnId": "turn-1"})
	awaitTurn(a, "turn-1")
	app.notify(t, "turn/completed", map[string]any{"threadId": "thread-A", "turn": map[string]any{"id": "turn-1"}})
	app.notify(t, "thread/tokenUsage/updated", map[string]any{"threadId": "thread-A", "turnId": "turn-1"})
	app.notify(t, "item/started", map[string]any{"threadId": "thread-A", "turnId": "turn-2"})
	// turn-2 opens only where neither the subagent's turn nor the finished
	// turn-1 had: the notifications are read in order.
	awaitTurn(a, "turn-2")
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
