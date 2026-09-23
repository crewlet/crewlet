package api_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The node gate's three lists the dashboard has to keep its own copy of, held
// against the engine's — see internal/clientsource for why a copy exists at
// all and why this side checks it.

// EVERY REMEDY THE GATE SENDS HAS A RENDERING, AND EVERY RENDERING A REMEDY.
//
// An action the engine sends and the dialog does not know is shown as its bare
// name, which tells an operator in a browser nothing they can press — the
// failure the actions exist to end, when the remedy was a sentence naming the
// command line's flags. The other direction is a branch nothing can reach.
func TestTheDashboardKnowsEveryGateAction(t *testing.T) {
	t.Parallel()
	holdList(t, `export const GATE_ACTIONS = \[([^\]]*)\] as const;`,
		statelog.GateActions(), "remedy")
}

// AND AGREES WHICH OF THEM KEEP THE GESTURE'S OWN ID.
//
// A dialog that let go of the id after a `log_full` answer left the operator
// nothing but a FRESH gesture once the ceiling was raised — a second one, which
// writes every log that already held the first record again and re-dates its
// eviction. The dialog's copy decides whether it offers "Finish this gesture".
func TestTheDashboardKeepsTheGesturesOwnIDWhereTheEngineDoes(t *testing.T) {
	t.Parallel()
	holdList(t, `export const GATE_ACTIONS_KEEPING_OPERATION = \[([^\]]*)\] as const;`,
		statelog.GateActionsKeepingOperation(), "operation-keeping action")
}

// THE DIALOG WAITS PAST THE NODE'S OWN BOUND ON A GESTURE.
//
// The node finishes a gesture under its own budget whatever the connection
// does, and a dialog that gave up first — the default thirty seconds did, on a
// one-minute budget — reported "did not answer" about a gesture the node went
// on to finish.
func TestTheDashboardWaitsPastTheGateBudget(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Declaration(clientsource.Tree(t),
		`export const GATE_REQUEST_TIMEOUT_MS = ([0-9_]+);`)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := strconv.ParseInt(strings.ReplaceAll(body, "_", ""), 10, 64)
	if err != nil {
		t.Fatalf("GATE_REQUEST_TIMEOUT_MS = %q is not a number: %v", body, err)
	}
	if wait := time.Duration(ms) * time.Millisecond; wait <= engine.GateBudget {
		t.Errorf("the dashboard waits %s for a gesture the node bounds at %s — it "+
			"gives up on a gesture the node goes on to finish", wait, engine.GateBudget)
	}
}

// holdList compares the dashboard's declared list with the engine's, both ways.
func holdList(t *testing.T, pattern string, want []statelog.GateAction, what string) {
	t.Helper()
	body, err := clientsource.Declaration(clientsource.Tree(t), pattern)
	if err != nil {
		t.Fatal(err)
	}
	client := clientsource.Strings(body)
	if len(client) == 0 {
		t.Fatalf("the dashboard declares no %s at all, so this gate certifies nothing", what)
	}
	for _, a := range want {
		if !slices.Contains(client, string(a)) {
			t.Errorf("the engine sends the %s %q and the dashboard's list (%v) does "+
				"not name it", what, a, client)
		}
	}
	for _, a := range client {
		if !slices.Contains(want, statelog.GateAction(a)) {
			t.Errorf("the dashboard names the %s %q, which the engine never sends as "+
				"one (it sends %v)", what, a, want)
		}
	}
}
