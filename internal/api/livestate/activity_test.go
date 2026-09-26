package livestate_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/clientsource"
)

// ONE SEAT-STATE VOCABULARY. Every case here reads the state off the overlay the
// engine serves, which is the only place a reader may take it from.

func stateOf(t *testing.T, s *livestate.LiveState, role string) (livestate.Activity, livestate.StoppedReason) {
	t.Helper()
	rows := s.MergeAgents([]map[string]any{{"role": role}})
	activity, _ := rows[0]["activity"].(livestate.Activity)
	if !activity.Valid() {
		t.Fatalf("seat %q carries activity %#v, which is not a word of the vocabulary", role, rows[0]["activity"])
	}
	reason, _ := rows[0]["stopped_reason"].(*livestate.StoppedReason)
	switch {
	case activity == livestate.ActivityStopped && reason == nil:
		t.Fatalf("seat %q is stopped with no reason", role)
	case activity != livestate.ActivityStopped && reason != nil:
		t.Fatalf("seat %q is %s and still carries stopped reason %q", role, activity, *reason)
	case reason != nil:
		return activity, *reason
	}
	return activity, ""
}

func wantState(t *testing.T, s *livestate.LiveState, role string, activity livestate.Activity,
	reason livestate.StoppedReason,
) {
	t.Helper()
	gotActivity, gotReason := stateOf(t, s, role)
	if gotActivity != activity || gotReason != reason {
		t.Errorf("%s: activity %q (%q), want %q (%q)", role, gotActivity, gotReason, activity, reason)
	}
}

// codingRun is one run as the durable record states it.
func codingRun(turnID, role string, status livestate.SandboxStatus, started time.Time) livestate.SandboxRecord {
	return livestate.SandboxRecord{
		Entry: livestate.SandboxEntry{
			TurnID: turnID, Role: role, AgentHandle: "coder", Status: status,
			StartedAt: started.Format(time.RFC3339Nano),
		},
		LaunchID:  "l-" + turnID,
		WrittenAt: started,
	}
}

// A RUN PARKED ON A QUESTION FOR THIRTEEN HOURS STILL NEEDS SOMEBODY.
//
// The client used to fold the running-runs panel in on its own, from a set the
// projection aged out at twelve hours — so the run that most needed a person
// fell out of every ring at hour twelve. The state is read from the run record,
// which nothing ages out.
func TestARunParkedForThirteenHoursReadsNeeds(t *testing.T) {
	t.Parallel()
	s := sandboxState(t)
	s.SetPlacement(map[string]bool{"Coder": true})
	s.ReconcileSandboxes([]livestate.SandboxRecord{
		codingRun("tn-old", "Coder", livestate.SandboxAwaiting, fixtureNow.Add(-13*time.Hour)),
	}, fixtureNow)

	wantState(t, s, "Coder", livestate.ActivityNeeds, "")
}

// THE RUN'S RECORD, NOT THE TURN'S STAGE. A parked turn says a run was
// launched; only the record says whether that run is still running or has
// stopped to ask. Reading the park as "needs" would call a seat whose box is
// busily compiling one waiting on a person, and a reseed — the box reaped, the
// question kept — is waiting too.
func TestNeedsIsReadFromTheRunRecordNotTheParkedTurn(t *testing.T) {
	t.Parallel()
	s := sandboxState(t)
	s.SetPlacement(map[string]bool{"Coder": true})
	s.Apply(env("agent_turn_completed", map[string]any{
		"role": "Coder", "turn_id": "tn-1", "suspended": true,
	}))
	s.ReconcileSandboxes([]livestate.SandboxRecord{
		codingRun("tn-1", "Coder", livestate.SandboxRunning, fixtureNow.Add(-time.Minute)),
	}, fixtureNow)
	wantState(t, s, "Coder", livestate.ActivityWorking, "")

	s.ReconcileSandboxes([]livestate.SandboxRecord{
		codingRun("tn-1", "Coder", livestate.SandboxReseed, fixtureNow.Add(-time.Minute)),
	}, fixtureNow.Add(time.Second))
	wantState(t, s, "Coder", livestate.ActivityNeeds, "")

	// And a park whose run the record no longer holds is not work: the
	// turn is ended only by events, and a run lost without one would
	// otherwise keep the seat busy for good.
	s.ReconcileSandboxes(nil, fixtureNow.Add(2*time.Second))
	wantState(t, s, "Coder", livestate.ActivityIdle, "")
}

// A SEAT A PEER HOLDS IS IDLE. Placement is the lease table's answer for the
// whole fleet, so a seat this node does not run is not offline; one NO node
// holds is stopped, and says so.
func TestPlacementIsTheFleetsAndUnplacedIsAStop(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	// Before the lease table has been read, nothing is claimed about it.
	wantState(t, s, "Lead", livestate.ActivityIdle, "")

	change := s.SetPlacement(map[string]bool{"Lead": true, "Coder": false})
	wantState(t, s, "Lead", livestate.ActivityIdle, "")
	wantState(t, s, "Coder", livestate.ActivityStopped, livestate.StoppedUnplaced)
	if _, moved := change.Agents["Coder"]; !moved {
		t.Errorf("a placement that stopped a seat did not push it: %v", change.Agents)
	}
	if _, moved := change.Agents["Lead"]; !moved {
		t.Errorf("a seat's first placement did not push it: %v", change.Agents)
	}

	// Only what MOVED is pushed.
	change = s.SetPlacement(map[string]bool{"Lead": true, "Coder": true})
	if want := []string{"Coder"}; !slices.Equal(keys(change.Agents), want) {
		t.Errorf("moved = %v, want only the seat that was placed %v", keys(change.Agents), want)
	}
	wantState(t, s, "Coder", livestate.ActivityIdle, "")
}

// A SPENT WINDOW STOPS THE SEAT — its own or the company's — until the window
// ends, and not a moment past it.
func TestARefusingBudgetWindowStopsTheSeat(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 14, 12, 30, 0, 0, time.UTC)
	s := livestate.New(livestate.WithClock(func() time.Time { return now }))
	s.SetPlacement(map[string]bool{"Lead": true, "Coder": true})

	own := meterReport("m-1", 1, map[string]any{
		"role": "Lead", "agent_id": "a-1", "handle": "lead",
		"windows": []any{dayWindow(400, 400, "2026-06-14T12:00:00Z")},
	})
	s.Apply(env("budget_meters", own, streamOnly))
	wantState(t, s, "Lead", livestate.ActivityStopped, livestate.StoppedBudget)
	wantState(t, s, "Coder", livestate.ActivityIdle, "")

	// THE COMPANY'S WINDOW STOPS EVERY SEAT, including one the frame never
	// names — and the push says so for each of them.
	org := meterReport("m-1", 2)
	org["org"] = map[string]any{"windows": []any{dayWindow(1000, 1000, "2026-06-14T12:10:00Z")}}
	change := s.Apply(env("budget_meters", org, streamOnly))
	wantState(t, s, "Coder", livestate.ActivityStopped, livestate.StoppedBudget)
	if _, moved := change.Agents["Coder"]; !moved {
		t.Errorf("the company's window stopped a seat and did not push it: %v", change.Agents)
	}

	// Past the window's end the park has lifted, frame or no frame.
	now = time.Date(2026, 6, 15, 0, 0, 1, 0, time.UTC)
	wantState(t, s, "Coder", livestate.ActivityIdle, "")
}

// A PAUSED SEAT IS STOPPED, and a paused seat still finishing its turn is
// working: what it is doing now is the truer word, and `paused` says the rest.
func TestAPausedSeatIsStoppedOnceItsTurnEnds(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_phase_started", map[string]any{
		"role": "CTO", "turn_id": "tn-1", "phase": "execute",
	}, at("2026-06-14T11:58:00Z"), id("ph")))
	change := s.Apply(env("seat_paused", map[string]any{
		"role": "CTO", "paused_by": "jane-token", "paused_at": "2026-06-14T11:59:00Z",
	}, at("2026-06-14T11:59:00Z"), id("p1")))
	wantState(t, s, "CTO", livestate.ActivityWorking, "")
	if o := overlayOf(t, s, "CTO"); o.Paused == nil {
		t.Error("a paused working seat does not say it is paused")
	}
	if _, moved := change.Agents["CTO"]; !moved {
		t.Error("the pause did not push the seat")
	}

	s.Apply(env("agent_turn_completed", map[string]any{"role": "CTO", "turn_id": "tn-1"},
		at("2026-06-14T12:00:00Z"), id("tc")))
	wantState(t, s, "CTO", livestate.ActivityStopped, livestate.StoppedPaused)

	s.Apply(env("seat_resumed", map[string]any{"role": "CTO"}, at("2026-06-14T12:05:00Z"), id("r1")))
	wantState(t, s, "CTO", livestate.ActivityIdle, "")
}

// A FAILED LAST TURN IS NOT A STOP. The seat takes its next wake like any
// other; why the last one failed is `last_error` and `last_turn`, separately.
func TestAFailedLastTurnLeavesTheSeatIdle(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	base := map[string]any{"role": "Lead", "turn_id": "tn-1", "phase": "execute", "iteration": 0}
	s.Apply(env("agent_phase_started", base))
	s.Apply(env("agent_phase_completed", with(base, map[string]any{"failed": true, "error": "boom"}),
		at("2026-06-14T12:00:05Z"), id("pc")))
	s.Apply(env("agent_turn_completed", map[string]any{"role": "Lead", "turn_id": "tn-1"},
		at("2026-06-14T12:00:06Z"), id("tc")))

	wantState(t, s, "Lead", livestate.ActivityIdle, "")
	o := overlayOf(t, s, "Lead")
	if o.LastError == nil {
		t.Error("the failure is not on last_error")
	}
	if o.LastTurn == nil || o.LastTurn.Outcome != livestate.OutcomeFailed {
		t.Errorf("last turn = %+v, want it failed", o.LastTurn)
	}
}

// WHEN SEVERAL STOPS HOLD, the first of [livestate.StoppedReasons] is the one
// stated: a person's pause, then placement, then the budget, then the provider.
func TestTheStoppedReasonsTakeTheirPrecedence(t *testing.T) {
	t.Parallel()
	s := sandboxState(t)
	s.Apply(env("llm_unavailable", map[string]any{"role": "Lead"}, id("u")))
	wantState(t, s, "Lead", livestate.ActivityStopped, livestate.StoppedProvider)

	own := meterReport("m-1", 1, map[string]any{
		"role": "Lead", "agent_id": "a-1", "handle": "lead",
		"windows": []any{dayWindow(400, 400, "2026-06-14T12:00:00Z")},
	})
	s.Apply(env("budget_meters", own, streamOnly, at("2026-06-14T12:00:00Z")))
	wantState(t, s, "Lead", livestate.ActivityStopped, livestate.StoppedBudget)

	s.SetPlacement(map[string]bool{"Lead": false})
	wantState(t, s, "Lead", livestate.ActivityStopped, livestate.StoppedUnplaced)

	s.Apply(env("seat_paused", map[string]any{"role": "Lead", "paused_by": "jane"}, id("p")))
	wantState(t, s, "Lead", livestate.ActivityStopped, livestate.StoppedPaused)

	// And work outranks every stop.
	s.Apply(env("agent_turn_started", map[string]any{"role": "Lead", "turn_id": "tn-9"},
		at("2026-06-14T12:10:00Z"), id("ts")))
	wantState(t, s, "Lead", livestate.ActivityWorking, "")
}

// A RUN'S RECORD MOVING PUSHES ITS SEAT. The reconcile is how a question asked
// while no event reached this process is found, and a panel that learned it
// while the seat's card did not would be two screens disagreeing again.
func TestAReconcileThatMovesARunPushesItsSeat(t *testing.T) {
	t.Parallel()
	s := sandboxState(t)
	s.SetPlacement(map[string]bool{"Coder": true, "Lead": true})
	change := s.ReconcileSandboxes([]livestate.SandboxRecord{
		codingRun("tn-1", "Coder", livestate.SandboxAwaiting, fixtureNow.Add(-time.Hour)),
	}, fixtureNow)
	if !change.Sandboxes {
		t.Error("the run set did not move")
	}
	if want := []string{"Coder"}; !slices.Equal(keys(change.Agents), want) {
		t.Errorf("moved = %v, want %v", keys(change.Agents), want)
	}
	rows := s.OverlayRows([]string{"Coder"})
	if len(rows) != 1 || rows[0]["activity"] != livestate.ActivityNeeds {
		t.Errorf("pushed = %v, want the seat needing somebody", rows)
	}
}

// EVERY ROW STATES THE REASON KEY, null when the seat is not stopped: a client
// merges a pushed row over the one it holds, so an omitted key would leave a
// seat that started again wearing the reason it had stopped for.
func TestTheStoppedReasonIsAlwaysOnTheRow(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_phase_started", map[string]any{"role": "Lead", "turn_id": "tn-1"}))
	rows := s.OverlayRows([]string{"Lead"})
	reason, present := rows[0]["stopped_reason"]
	if !present {
		t.Fatal("the row carries no stopped_reason key")
	}
	if r, _ := reason.(*livestate.StoppedReason); r != nil {
		t.Errorf("stopped_reason = %q on a working seat", *r)
	}
}

// THE DASHBOARD KNOWS EXACTLY THE SEAT STATES THE ENGINE SENDS. Its
// `SeatActivity` and `StoppedReason` unions are the engine's closed sets
// written twice, because the dashboard cannot import a Go identifier: a word
// the engine sends that the union lacks is a state no screen can draw, and one
// the union names that the engine never sends is a branch nothing reaches.
func TestTheDashboardKnowsExactlyTheSeatStatesTheEngineSends(t *testing.T) {
	t.Parallel()
	for _, set := range []struct {
		name   string
		engine []string
	}{
		{"SeatActivity", strs(livestate.Activities)},
		{"StoppedReason", strs(livestate.StoppedReasons)},
	} {
		client, err := clientsource.Union("../"+clientsource.Tree, set.name)
		if err != nil {
			t.Fatal(err)
		}
		engine := slices.Clone(set.engine)
		slices.Sort(engine)
		slices.Sort(client)
		if !slices.Equal(engine, client) {
			t.Errorf("the dashboard's %s union is %v and the engine sends %v", set.name, client, engine)
		}
	}
}

func strs[T ~string](values []T) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return out
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
