package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A COLLEAGUE'S ASK IS ANSWERED BY THE FRAME THAT ENDS THE TURN, WHEREVER IT
// ENDS.
//
// Every agent seat's turn launches its executor and parks on the run, and the
// segment that ends it is resumed off the seat's control topic — or closed by a
// person's stop of the run — on a frame that never saw the ask. Answered only
// from the dispatch frame, which answers nothing for a turn that parked, every
// a2a_ask to an agent seat went unanswered and its channel waited for the idle
// sweep. These cases drive the real path: the ask the a2a service publishes,
// the dispatch that parks on the launch, and the run's collection or its stop.

// askingCompany is spendingEngine's company with an agent seat that asks swe,
// and room under the delegation cap for the ask's own hop.
func askingCompany(t *testing.T, e *Engine) {
	t.Helper()
	c := e.Company()
	c.Org.Roles = append(c.Org.Roles, &org.Role{Name: "CEO", DeclaredHandle: "ceo", LLM: org.ProviderKeys{"only"}})
	c.Config.TurnEngine.DelegationDepthLimit = 3
}

// askSwe opens ceo's channel to swe and answers the wake it published, as the
// seat's inbox delivers it, carrying trace as an ask made inside a traced turn
// does.
func askSwe(t *testing.T, e *Engine, q *memory.Queue, brief string, trace events.TraceContext) (string, *events.Event) {
	t.Helper()
	channelID, err := e.a2aService(e.Company()).Open(t.Context(), a2a.Ask{
		Requester: "ceo", Target: "swe", Brief: brief, SenderRole: "CEO",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, ev := range q.History() {
		if req, ok := events.DataAs[*types.A2ARequest](ev); ok && req.ChannelID == channelID {
			wake := *ev
			wake.TraceID, wake.SpanID, wake.ParentSpanID = trace.TraceID, trace.SpanID, trace.ParentSpanID
			return channelID, &wake
		}
	}
	t.Fatal("the a2a service published no wake for the ask")
	return "", nil
}

// answersOn is every reply woken on channelID, in order.
func answersOn(q *memory.Queue, channelID string) []*events.Event {
	var out []*events.Event
	for _, ev := range q.History() {
		if msg, ok := events.DataAs[*types.A2AMessage](ev); ok && msg.ChannelID == channelID {
			out = append(out, ev)
		}
	}
	return out
}

// channelOpen reports whether channelID is still open.
func channelOpen(t *testing.T, e *Engine, channelID string) bool {
	t.Helper()
	ch, err := a2a.NewCoordStore(e.backends.Fleet).Get(t.Context(), channelID)
	if err != nil {
		t.Fatalf("channel %s: %v", channelID, err)
	}
	return ch.Open()
}

// A TURN THAT PARKED ON ITS RUN ANSWERS ITS ASKER FROM THE SEGMENT THAT ENDS IT:
// the dispatch that parked answers nothing, the launch writes the ask onto the
// run's record, and the resume that collects the run and ends the turn replies
// on the ask's channel — the turn's account, the question echoed, the ask's
// trace continued and the answering RUN as the reply's parent — and closes it.
//
// Mutation: drop the resume's answer, and the asker is never woken; drop the
// ask from the launch's record, and the resume has nothing to answer.
func TestAParkedTurnAnswersItsAskerFromTheSegmentThatEndsIt(t *testing.T) {
	t.Parallel()
	e, _ := spendingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	askingCompany(t, e)
	rig := rigRuns(t, e, modelPlayer(t, billingRun))
	trace := events.TraceContext{TraceID: "0af7651916cd43dd8448eb211c80319c", SpanID: "b7ad6b7169203331"}
	channelID, ask := askSwe(t, e, q, "Which branch carries the fix?", trace)

	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-ask", RunID: "run-ask", WorkSince: time.Now().UTC(),
		Events: []*events.Event{ask},
		// AS THE DISPATCH STATES THEM, off the trigger ([delegationOf]).
		Depth: ask.DelegationDepth, DelegationChain: ask.DelegationChain,
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; want the turn parked on its executor's run", res, err)
	}
	if got := answersOn(q, channelID); len(got) != 0 || !channelOpen(t, e, channelID) {
		t.Fatalf("the parked turn answered %d time(s) or closed the channel: it has not finished", len(got))
	}
	carried := rig.row(t, "run-ask").ColleagueAsk
	if carried == nil || carried.ChannelID != channelID || carried.Question != "Which branch carries the fix?" ||
		carried.TraceID != trace.TraceID || len(carried.Chain) != 1 || carried.Chain[0] != "ceo" {
		t.Fatalf("the run's record carries ask %+v, want the channel, question, chain and trace of the ask", carried)
	}

	if err := rig.collect(t, "run-ask"); err != nil {
		t.Fatalf("collecting the run: %v", err)
	}

	got := answersOn(q, channelID)
	if len(got) != 1 {
		t.Fatalf("the turn's end answered %d time(s), want once", len(got))
	}
	var ended *types.AgentTurnCompleted
	for _, ev := range q.History() {
		if done, ok := events.DataAs[*types.AgentTurnCompleted](ev); ok && done.TurnID == "run-ask" && !done.Suspended {
			ended = done
		}
	}
	if ended == nil || ended.Decision != "done" || ended.Response == "" {
		t.Fatalf("the resumed segment ended the turn as %+v, want it done with an answer", ended)
	}
	reply := got[0]
	msg, _ := events.DataAs[*types.A2AMessage](reply)
	if msg.Sender != "swe" || msg.Content != ended.Response ||
		msg.Question != "Which branch carries the fix?" {
		t.Fatalf("the reply is %+v, want swe's answer %q, echoing the question", msg, ended.Response)
	}
	if reply.TraceID != trace.TraceID || reply.SpanID != trace.SpanID {
		t.Errorf("the reply runs under %s/%s, want the ask's trace %s/%s",
			reply.TraceID, reply.SpanID, trace.TraceID, trace.SpanID)
	}
	if reply.ParentTurnID != "run-ask" || reply.DelegationDepth != ask.DelegationDepth {
		t.Errorf("the reply names parent %q at depth %d, want the answering run at the ask's depth %d",
			reply.ParentTurnID, reply.DelegationDepth, ask.DelegationDepth)
	}
	if channelOpen(t, e, channelID) {
		t.Error("the answered channel was left open for the idle sweep")
	}
}

// A STOP THAT ENDS A PARKED TURN TELLS ITS ASKER SO: the stop's close is the
// turn's end, so the asker is answered from it, with the stop rather than a
// request to ask again.
//
// Mutation: drop the answer from the stop's close, and the asker waits on an
// open channel.
func TestAStoppedParkedTurnTellsItsAskerItWasStopped(t *testing.T) {
	t.Parallel()
	e, _ := spendingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	askingCompany(t, e)
	rigRuns(t, e, halfwayPlay)
	channelID, ask := askSwe(t, e, q, "Can you rerun the migration?", events.TraceContext{})

	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-ask", RunID: "run-ask", WorkSince: time.Now().UTC(),
		Events: []*events.Event{ask},
		// AS THE DISPATCH STATES THEM, off the trigger ([delegationOf]).
		Depth: ask.DelegationDepth, DelegationChain: ask.DelegationChain,
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; want the turn parked on its executor's run", res, err)
	}
	pause := janeStops()
	e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat, Pause: &pause})
	closedAsStopped(t, q, "run-ask")

	got := answersOn(q, channelID)
	if len(got) != 1 {
		t.Fatalf("the stopped turn answered %d time(s), want once", len(got))
	}
	msg, _ := events.DataAs[*types.A2AMessage](got[0])
	if !strings.Contains(msg.Content, "a person stopped my turn") {
		t.Fatalf("the asker was told %q, want that a person stopped the turn", msg.Content)
	}
	if channelOpen(t, e, channelID) {
		t.Error("the stopped turn's channel was left open for the idle sweep")
	}
}

// A TURN NO ASK WOKE CARRIES NO ASK on its run's record, so the frame that ends
// it answers nobody.
func TestARunNoAskWokeCarriesNoAsk(t *testing.T) {
	t.Parallel()
	e, _ := spendingEngine(t)
	rig := rigRuns(t, e, halfwayPlay)
	parkOnRun(t, e)
	if carried := rig.row(t, "run-1").ColleagueAsk; carried != nil {
		t.Fatalf("a task's turn carries ask %+v on its run's record", *carried)
	}
}

// loseRun loses turnID's parked run the way a box that vanished does: its box
// is gone when its completion is collected, so the collection gives the run up
// at once and ends it as `collect_unreachable` ([sandbox.Coordinator]) — with
// the turn still suspended into it, and no frame of that turn left to end it.
func (r *runRig) loseRun(t *testing.T, turnID string) {
	t.Helper()
	row := r.row(t, turnID)
	provider, err := r.engine.Company().catalogue.Provider(sandbox.Placement(row.Placement))
	if err != nil {
		t.Fatalf("the provider of run %s's box: %v", turnID, err)
	}
	if err := provider.Kill(t.Context(), row.SandboxID); err != nil {
		t.Fatalf("killing run %s's box: %v", turnID, err)
	}
	completion := types.SandboxRunCompleted{
		Agent: row.AgentID, AgentHandle: row.AgentHandle, RoleName: row.Role,
		TurnID: row.TurnID, WorkKey: row.WorkKey, LaunchID: row.LaunchID,
		SandboxID: row.SandboxID, CodingAgent: row.CodingAgent,
	}
	ev := events.New(completion, events.TraceContext{TraceID: row.TraceID, ParentSpanID: row.SpanID})
	if err := r.engine.sandbox.Load().coordinator.OnCompleted(t.Context(), completion, ev); err != nil {
		t.Fatalf("delivering run %s's completion: %v", turnID, err)
	}
	if _, found, err := r.engine.sandbox.Load().pending.Get(t.Context(), turnID); err != nil || found {
		t.Fatalf("run %s is still on the record after its box was lost (%v): it was not ended", turnID, err)
	}
}

// A TURN LOST WITH ITS RUN TELLS ITS ASKER SO: the run's loss ends the parked
// turn with no frame of it left to answer, so the run's ending answers — once,
// as the answering seat and run, saying the coding run was lost and why, the
// question echoed — and closes the channel, rather than leaving the asker to an
// idle sweep that wakes nobody.
//
// Mutation: drop the ending's hand-over ([sandbox.CoordinatorOptions.TurnLost])
// or the engine's answer from it, and the asker waits on an open channel; drop
// the loss from [answerContent], and the asker is asked for more detail about a
// question nobody got to.
func TestALostRunsTurnTellsItsAskerItWasLost(t *testing.T) {
	t.Parallel()
	e, _ := spendingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	askingCompany(t, e)
	rig := rigRuns(t, e, halfwayPlay)
	channelID, ask := askSwe(t, e, q, "Which branch carries the fix?", events.TraceContext{})

	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-ask", RunID: "run-ask", WorkSince: time.Now().UTC(),
		Events: []*events.Event{ask},
		// AS THE DISPATCH STATES THEM, off the trigger ([delegationOf]).
		Depth: ask.DelegationDepth, DelegationChain: ask.DelegationChain,
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; want the turn parked on its executor's run", res, err)
	}

	rig.loseRun(t, "run-ask")

	got := answersOn(q, channelID)
	if len(got) != 1 {
		t.Fatalf("the lost turn answered %d time(s), want once", len(got))
	}
	msg, _ := events.DataAs[*types.A2AMessage](got[0])
	if msg.Sender != "swe" || msg.Question != "Which branch carries the fix?" {
		t.Errorf("the reply is %+v, want swe's, echoing the question", msg)
	}
	if !strings.Contains(msg.Content, "was lost") ||
		!strings.Contains(msg.Content, types.SandboxFailureCollect) {
		t.Errorf("the asker was told %q, want that the coding run was lost, and why", msg.Content)
	}
	if got[0].ParentTurnID != "run-ask" {
		t.Errorf("the reply names parent %q, want the answering run", got[0].ParentTurnID)
	}
	if channelOpen(t, e, channelID) {
		t.Error("the lost turn's channel was left open for the idle sweep")
	}
}
