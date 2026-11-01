package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHETHER A QUIET CHANNEL IS STILL OWED ITS ANSWER: the idle sweep spares a
// channel whose seat still holds the ask somewhere it will take it up from —
// on a live run's record, or in its inbox held by a run, a person's pause or a
// spent budget window — and closes one nothing holds, as it closed every idle
// channel before.

// fakeSeatRuns is the fleet's executor records as [colleagueOwed] reads them.
type fakeSeatRuns struct {
	runs []sandbox.PendingRun
	err  error
}

func (f fakeSeatRuns) ListActiveForSeat(_ context.Context, handle string) ([]sandbox.PendingRun, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []sandbox.PendingRun
	for _, run := range f.runs {
		if run.AgentHandle == handle {
			out = append(out, run)
		}
	}
	return out, nil
}

// quietAsk is the channel every case judges: the CEO's ask of the Lead.
var quietAsk = a2a.Channel{ID: "a2a-quiet", Requester: "ceo", Target: "lead"}

// A QUIET CHANNEL IS OWED FOR AS LONG AS ITS SEAT HOLDS THE ASK, AND NO LONGER.
//
// Mutation: drop any one of the four readings, and its case is judged "not
// owed" and the channel closed under an answer that is still coming; read the
// run records without matching the channel or the seat, or count a run parked
// on its own question as holding the inbox, and a channel nothing holds is
// spared for ever.
func TestAQuietChannelIsOwedWhileItsSeatHoldsTheAsk(t *testing.T) {
	t.Parallel()
	asked := &sandbox.ColleagueAsk{ChannelID: quietAsk.ID}
	another := &sandbox.ColleagueAsk{ChannelID: "a2a-other"}
	for _, tc := range []struct {
		name   string
		runs   []sandbox.PendingRun
		paused bool
		spent  bool
		want   bool
	}{
		{name: "its ask on the record of a run parked on a person's question", runs: []sandbox.PendingRun{
			{AgentHandle: "lead", Status: sandbox.StatusAwaiting, ColleagueAsk: asked},
		}, want: true},
		{name: "waiting in its inbox behind a run that holds the seat", runs: []sandbox.PendingRun{
			{AgentHandle: "lead", Status: sandbox.StatusRunning, ColleagueAsk: another},
		}, want: true},
		{name: "waiting in its inbox behind an answer's resume", runs: []sandbox.PendingRun{
			{AgentHandle: "lead", Status: sandbox.StatusAnswered},
		}, want: true},
		{name: "waiting in its inbox while a person has the seat paused", paused: true, want: true},
		{name: "waiting in its inbox while the seat's budget window is spent", spent: true, want: true},
		{name: "nothing holds it: the seat's one run is parked on another ask's question",
			runs: []sandbox.PendingRun{
				{AgentHandle: "lead", Status: sandbox.StatusAwaiting, ColleagueAsk: another},
			}, want: false},
		{name: "nothing holds it: another seat's run is the one working", runs: []sandbox.PendingRun{
			{AgentHandle: "swe", Status: sandbox.StatusRunning, ColleagueAsk: asked},
		}, want: false},
		{name: "nothing holds it at all", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newParkRig(t, "100")
			if tc.spent {
				r.spend(t, 100)
			}
			pauses := map[uuid.UUID]coord.SeatPause{}
			if tc.paused {
				pauses[seatOf("lead")] = coord.SeatPause{Seat: seatOf("lead"), By: "jane", At: parkedAt}
			}
			r.e.adoptSeatPauses(t.Context(), pauses)
			owed := colleagueOwed{engine: r.e, runs: fakeSeatRuns{runs: tc.runs}}

			got, err := owed.AnswerOwed(t.Context(), quietAsk)
			if err != nil {
				t.Fatalf("AnswerOwed: %v", err)
			}
			if got != tc.want {
				t.Fatalf("owed = %v, want %v", got, tc.want)
			}
		})
	}
}

// WHAT CANNOT BE READ IS NOT "NOT OWED": the run records, the pauses before
// this node has read them, or a budget counter, each unreadable, is an error,
// which the sweep spares the channel on for the tick — closing is the step
// nothing can undo.
//
// Mutation: answer any of them "not owed", and the channel is closed on a
// store blip.
func TestAQuietChannelItCannotJudgeIsAnError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		rig   func(*testing.T, *parkRig)
		runs  fakeSeatRuns
		cause error
	}{
		{name: "its seat's runs could not be read",
			rig:  func(t *testing.T, r *parkRig) { r.e.adoptSeatPauses(t.Context(), nil) },
			runs: fakeSeatRuns{err: errors.New("the coordination store is unreachable")}},
		{name: "this node has not read the pauses", rig: func(*testing.T, *parkRig) {},
			cause: errPausesUnread},
		{name: "its budget counters could not be read", rig: func(t *testing.T, r *parkRig) {
			r.e.adoptSeatPauses(t.Context(), nil)
			r.e.backends.Fleet = unreachableFleet{fleetBase: r.fleet}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newParkRig(t, "100")
			tc.rig(t, r)
			owed, err := colleagueOwed{engine: r.e, runs: tc.runs}.AnswerOwed(t.Context(), quietAsk)
			if err == nil {
				t.Fatalf("owed = %v with no error, want the unreadable answer reported", owed)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("err = %v, want %v", err, tc.cause)
			}
		})
	}
}

// idleSweep is the engine's own idle-channel job, as the maintenance duty runs
// it.
func idleSweep(t *testing.T, e *Engine) maintenance.Job {
	t.Helper()
	for _, job := range e.maintenanceJobs() {
		if job.Name == "a2a_channels_idle" {
			return job
		}
	}
	t.Fatal("the engine's maintenance jobs carry no idle-channel sweep")
	return maintenance.Job{}
}

// THE ENGINE'S OWN SWEEP SPARES THE ASK A PARKED RUN IS WORKING ON, AND CLOSES
// ONE NOTHING HOLDS: run past every idle horizon, the duty's job leaves the
// channel whose ask the parked turn's run carries open — so the turn's end
// still answers on it — and closes the one asked of a seat with no run, no
// pause and no spent window.
//
// Mutation: build the engine's sweeper with a judge that owes nothing, and the
// parked turn's channel is closed under it.
func TestTheEnginesSweepSparesTheAskAParkedRunIsWorkingOn(t *testing.T) {
	t.Parallel()
	e, _ := spendingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	askingCompany(t, e)
	c := e.Company()
	c.Org.Roles = append(c.Org.Roles, &org.Role{Name: "QA", DeclaredHandle: "qa", LLM: org.ProviderKeys{"only"}})
	e.adoptSeatPauses(t.Context(), nil)
	rigRuns(t, e, halfwayPlay)
	parked, ask := askSwe(t, e, q, "Which branch carries the fix?", events.TraceContext{})
	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-ask", RunID: "run-ask", WorkSince: time.Now().UTC(),
		Events: []*events.Event{ask},
		Depth:  ask.DelegationDepth, DelegationChain: ask.DelegationChain,
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; want the turn parked on its executor's run", res, err)
	}
	unheld, err := e.a2aService(c).Open(t.Context(), a2a.Ask{Requester: "ceo", Target: "qa", Brief: "?"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	now := time.Now().UTC().Add(24 * time.Hour)
	if _, err := idleSweep(t, e).Run(t.Context(), now, now); err != nil {
		t.Fatalf("the idle sweep: %v", err)
	}
	if !channelOpen(t, e, parked) {
		t.Error("the sweep closed the channel whose ask the parked run carries: its answer will be refused")
	}
	if channelOpen(t, e, unheld) {
		t.Error("the sweep left open a channel nothing holds")
	}
}
