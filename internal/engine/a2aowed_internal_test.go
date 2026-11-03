package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
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

// AN ASK BEHIND A PARK IS OWED ON WHICHEVER NODE THE SWEEP RUNS. The idle sweep
// runs once across the fleet, under the maintenance duty, seldom on the node
// holding the seat it judges. The Lead's holder parked its inbox below the
// floor its first launch needs to continue its conversation's session, in a
// window with room for the static floor, which a recount of the shared counters
// finds: a duty node judging by its own parks read the ask behind that park as
// not owed, and closed its channel under the answer still coming. It asks the
// holder instead, and the ask is owed; a holder that does not say is no answer,
// and the channel is spared for the tick. And a seat whose models are all
// limited, which no counter records, holds its asks as surely: its provider
// park is a park too.
//
// Mutation: judge a seat by the judging node's own parks, read a silent holder
// as a seat in no park, or count only the budget's parks, and a case goes red.
func TestAnAskBehindAParkIsOwedOnWhicheverNodeTheSweepRuns(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	holder := newFlooredParkRig(t, "100", "30")
	holder.mu.Lock()
	holder.turnFn = func() (turn.Result, error) {
		// FIFTY LEFT: room for the static floor of thirty, and none for a
		// launch whose session's last request carried fifty beside a brief
		// of ten.
		holder.spendIn(ctx, t, 50)
		return turn.Result{}, executorHeadroom(ctx, staticRemaining{left: 50},
			launchFloor{static: 30}.continuing(50, 10))
	}
	holder.mu.Unlock()
	holder.deliver(t)
	if !slices.Contains(holder.holds(), pauseReasonBudget) {
		t.Fatalf("setup: the holder did not park the Lead: holds %v", holder.holds())
	}
	// THE DUTY NODE: the same company, counters and clock, and no park.
	duty := newFlooredParkRig(t, "100", "30")
	duty.e.backends.Fleet = holder.fleet
	duty.e.adoptSeatPauses(ctx, nil)
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })
	answerer := heldParks{owner: "node-a:1", local: holder.e.inboxParkedHere}
	stop, err := q.Serve(ctx, topics.HeldPark, queue.ConcurrentAnswers, func(_ context.Context, raw []byte) ([]byte, error) {
		return answerer.answer(raw)
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(ctx)) })
	heldBy := func(owner string, budget time.Duration) colleagueOwed {
		return colleagueOwed{engine: duty.e, runs: fakeSeatRuns{}, parks: heldParks{owner: "node-b:1",
			queue: q, leases: leaseTable{lease: &coord.Lease{Owner: owner, ExpiresAt: time.Now().Add(time.Minute)}},
			local: duty.e.inboxParkedHere, budget: budget, now: time.Now}}
	}

	alone := colleagueOwed{engine: duty.e, runs: fakeSeatRuns{}}
	if owed, err := alone.AnswerOwed(ctx, quietAsk); err != nil || owed {
		t.Fatalf("setup: the duty node judging by its own parks reads the ask owed = %v (%v), so the case "+
			"shows nothing", owed, err)
	}
	if owed, err := heldBy("node-a:1", 2*time.Second).AnswerOwed(ctx, quietAsk); err != nil || !owed {
		t.Errorf("the duty node asking the holder reads the ask behind its park owed = %v (%v), want owed",
			owed, err)
	}
	if owed, err := heldBy("node-c:1", 200*time.Millisecond).AnswerOwed(ctx, quietAsk); err == nil {
		t.Errorf("a holder that did not say read as owed = %v with no error, want the channel spared", owed)
	}

	// A SEAT WHOSE MODELS ARE ALL LIMITED, parked by the node judging it.
	model := &limitedModel{reset: parkedAt.Add(14 * time.Minute)}
	limited := newLimitedRig(t, chain.Member{Key: "default", Provider: model})
	model.now = limited.clock
	limited.send(t)
	if !slices.Contains(limited.holds(), pauseReasonProvider) {
		t.Fatalf("setup: the Lead was not parked on its models' limit: holds %v", limited.holds())
	}
	limited.e.adoptSeatPauses(ctx, nil)
	behindLimit := colleagueOwed{engine: limited.e, runs: fakeSeatRuns{}}
	if owed, err := behindLimit.AnswerOwed(ctx, quietAsk); err != nil || !owed {
		t.Errorf("an ask behind a park on the seat's models' limit reads owed = %v (%v), want owed", owed, err)
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
