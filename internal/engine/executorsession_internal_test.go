package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// leakedToken is a credential a box or a tool put in an error the engine quotes.
const leakedToken = "ghp_" + "Ab3dEf6hIj9kLm2nOp5qRs8tUv1wXy4zAb7d"

// executorLogSpy is the executor log's writes as a case reads them: every end
// a turn's ending recorded, and every capture. AN END TOMBSTONES ITS TURN, as
// the log's does: a capture of an ended turn is refused
// ([executordomain.ErrTurnEnded]) and counted, so a case sees an end that came
// before the turn's last capture.
type executorLogSpy struct {
	mu       sync.Mutex
	ends     []executordomain.NewEnd
	ended    map[string]bool
	afterEnd int
}

func (s *executorLogSpy) Capture(_ context.Context, _ executordomain.Call,
	n executordomain.NewCapture) (executordomain.CaptureResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended[n.Turn] {
		s.afterEnd++
		return executordomain.CaptureResult{}, fmt.Errorf("%w: %s", executordomain.ErrTurnEnded, n.Turn)
	}
	return executordomain.CaptureResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}, Recorded: true}, nil
}

func (s *executorLogSpy) End(_ context.Context, _ executordomain.Call, n executordomain.NewEnd) (statelog.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended == nil {
		s.ended = map[string]bool{}
	}
	if s.ended[n.Turn] {
		return statelog.Result{}, fmt.Errorf("%w: %s", executordomain.ErrTurnEnded, n.Turn)
	}
	s.ended[n.Turn] = true
	s.ends = append(s.ends, n)
	return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
}

// capturesAfterEnd is how many captures the log refused because their turn
// had ended first.
func (s *executorLogSpy) capturesAfterEnd() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.afterEnd
}

// endsOf is every end recorded for turnID.
func (s *executorLogSpy) endsOf(turnID string) []executordomain.NewEnd {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []executordomain.NewEnd
	for _, n := range s.ends {
		if n.Turn == turnID {
			out = append(out, n)
		}
	}
	return out
}

// loggingEngine is [spendingEngine] whose sandbox runtime records on spy.
func loggingEngine(t *testing.T) (*Engine, *executorLogSpy) {
	t.Helper()
	e, _ := spendingEngine(t)
	spy := &executorLogSpy{}
	e.executorLog = spy
	return e, spy
}

// loggingReviewCase is an engine whose one seat's turns park on their executor
// and whose reviewer is model, its executor log recording on spy, over store
// where one is given and the fleet's own run store otherwise.
func loggingReviewCase(t *testing.T, model *reviewWatch,
	store func(sandbox.PendingStore) sandbox.PendingStore) (*Engine, *runRig, *executorLogSpy) {
	t.Helper()
	e, _ := indicatingWith(t, notify.StatusAlways, model)
	steerable(t, e)
	spy := &executorLogSpy{}
	e.executorLog = spy
	var pending sandbox.PendingStore = sandbox.NewCoordStore(e.backends.Fleet)
	if store != nil {
		pending = store(pending)
	}
	rig := rigRunsOn(t, e, pending, modelPlayer(t, model.run))
	// ROOM FOR THE ROUND A REVIEW SENDS BACK.
	e.Company().Config.TurnEngine.MaxIterations = 2
	return e, rig, spy
}

// awaitEnds is every end recorded for turnID once there are want of them: a
// turn's end is owed on its run's record and recorded by the run's ending, off
// the frame that ended the turn, so a case waits for it. It waits a little past
// the count too, so an end recorded twice shows.
func awaitEnds(t *testing.T, spy *executorLogSpy, turnID string, want int) []executordomain.NewEnd {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(spy.endsOf(turnID)) < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	return spy.endsOf(turnID)
}

// A RESUMED TURN THAT ENDS RECORDS ITS END ON THE EXECUTOR LOG, once, with the
// turn's OWN decision — the one its completion carries — and how its reply
// went.
//
// Mutation: drop the end from recordResume, or take the decision off the
// reviewer's last word, and the end is missing or says another decision than
// the turn's. (A turn that parked again is
// [TestAResumeThatParksAgainRecordsNoEndUntilItsTurnEnds]'s: this one's
// reviewer never sends a round back.)
func TestAResumedTurnsEndIsRecordedWithItsOwnDecision(t *testing.T) {
	t.Parallel()
	e, spy := loggingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	model := longAccountModel()
	swapModel(t, e, model)
	rig := rigRuns(t, e, modelPlayer(t, model.pick))
	runToItsEnd(t, e, rig, Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1", WorkSince: time.Now().UTC(),
		Events: []*events.Event{taskWake("task-9", "ENG-9")},
	})
	ends := awaitEnds(t, spy, "run-1", 1)
	if len(ends) != 1 {
		t.Fatalf("recorded %d ends of the turn, want 1", len(ends))
	}
	var decision string
	for _, ev := range q.History() {
		if done, ok := events.DataAs[*types.AgentTurnCompleted](ev); ok && done.TurnID == "run-1" && !done.Suspended {
			decision = done.Decision
		}
	}
	end := ends[0]
	if end.Handle != "swe" || end.WorkKey != "wk-1" || end.Ending.Decision != decision ||
		!end.Ending.Outcome.Valid() || end.Ending.Outcome == executordomain.OutcomeStopped {
		t.Fatalf("the end is %+v, want the turn's own decision %q", end, decision)
	}
}

// A PARKED TURN A PERSON STOPPED ENDS AS STOPPED ON THE EXECUTOR LOG, by the
// run's ending, which no frame of the turn reaches: who stopped it and why,
// their reason carried as theirs.
//
// Mutation: drop the end from the stop's close, and the next turn on the
// conversation is never told this one was stopped.
func TestAStoppedParkedTurnEndsAsStoppedOnTheExecutorLog(t *testing.T) {
	t.Parallel()
	e, spy := loggingEngine(t)
	rigRuns(t, e, halfwayPlay)
	parkOnRun(t, e)
	pause := janeStops()
	e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat, Pause: &pause})

	ends := awaitEnds(t, spy, "run-1", 1)
	if len(ends) != 1 {
		t.Fatalf("recorded %d ends of the stopped turn, want 1", len(ends))
	}
	if ending := ends[0].Ending; ending.Outcome != executordomain.OutcomeStopped ||
		ending.StoppedBy != "jane" || ending.StopReason != "wrong repository" {
		t.Fatalf("the end is %+v, want the turn stopped by Jane for her reason", ending)
	}
}

// A LAUNCH STOPPED BEFORE ITS TURN PARKED ENDS ON THE EXECUTOR LOG FROM THE
// STOP'S OWN ENDING, once — never from the frame that launched it, which is
// still unwinding to its park: the ending records the end after its read has
// taken the job's session, and an end the frame recorded first would refuse
// that session by its tombstone.
//
// Mutation: record the end from the dispatch path's refused park again, or hand
// the ending a stop only where a turn parked, and the end comes before the
// stop's capture, or nowhere.
func TestALaunchStoppedBeforeItsParkEndsOnTheExecutorLogOnce(t *testing.T) {
	t.Parallel()
	e, spy := loggingEngine(t)
	store := &parkStopsFirst{PendingStore: sandbox.NewCoordStore(e.backends.Fleet)}
	rigRunsOn(t, e, store, halfwayPlay)
	store.before = func() {
		pause := janeStops()
		e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat, Pause: &pause})
	}
	if _, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1", WorkSince: time.Now().UTC(),
		Events: []*events.Event{taskWake("task-9", "ENG-9")},
	}); !turn.Stopped(err) {
		t.Fatalf("runTurn = %v, want the turn stopped", err)
	}
	ends := awaitEnds(t, spy, "run-1", 1)
	if len(ends) != 1 || ends[0].Ending.Outcome != executordomain.OutcomeStopped ||
		ends[0].Ending.StoppedBy != "jane" {
		t.Fatalf("recorded %+v, want the turn's one end, stopped by Jane", ends)
	}
}

// A TURN LOST WITH ITS RUN ENDS AS LOST ON THE EXECUTOR LOG, by the run's own
// ending — the loss its collection decided once the run's box was found gone —
// with the loss it recorded, and whether its reply reached anybody as the turn's
// own calls say: here a turn nobody was waiting on, which has nobody to miss it.
//
// Mutation: drop the loss's account from the ending ([sandbox.CoordinatorOptions.TurnLost]),
// or record it after the row's delete, and a lost turn ends nowhere.
func TestALostTurnEndsAsLostOnTheExecutorLog(t *testing.T) {
	t.Parallel()
	e, spy := loggingEngine(t)
	rig := rigRuns(t, e, halfwayPlay)
	parkOnRun(t, e)
	row := rig.row(t, "run-1")
	provider, err := e.Company().catalogue.Provider(sandbox.Placement(row.Placement))
	if err != nil {
		t.Fatalf("the provider of the run's box: %v", err)
	}
	if err := provider.Kill(t.Context(), row.SandboxID); err != nil {
		t.Fatalf("killing the run's box: %v", err)
	}
	completion := types.ExecutorFinished{Agent: row.AgentID, AgentHandle: row.AgentHandle,
		RoleName: row.Role, TurnID: row.TurnID, WorkKey: row.WorkKey, LaunchID: row.LaunchID,
		SandboxID: row.SandboxID, Harness: row.Harness}
	ev := events.New(completion, events.TraceContext{TraceID: row.TraceID, ParentSpanID: row.SpanID})
	if err := e.sandbox.Load().coordinator.OnCompleted(t.Context(), completion, ev); err != nil {
		t.Fatalf("delivering the completion: %v", err)
	}
	ends := awaitEnds(t, spy, "run-1", 1)
	if len(ends) != 1 {
		t.Fatalf("recorded %d ends of the lost turn, want 1", len(ends))
	}
	end := ends[0]
	if end.Ending.Outcome != executordomain.OutcomeLost || !strings.Contains(end.Ending.Failure, "lost") ||
		end.WorkKey != "wk-1" || end.Handle != "swe" {
		t.Fatalf("the end is %+v, want the turn lost, its seat and its unit of work", end)
	}
}

// A STOP'S ACCOUNT OF WHETHER THE TURN REPLIED READS EVERY ROUND'S CALLS: a turn
// that replied in an earlier round and was stopped in a later one replied, and
// one whose calls were elided from the record, the kept ones showing none, is
// not known to have — never "did not reply" off half a record.
//
// Mutation: read the current round's calls alone, or answer an elided record
// with a definite no, and the next turn is told a reply that went out did not.
func TestAStopsAccountReadsEveryRoundsCalls(t *testing.T) {
	t.Parallel()
	e, _ := loggingEngine(t)
	company := e.Company()
	// A TOOL THAT REACHES THE PERSON WAITING, on the surface they wait on.
	if err := company.Tools.Register(namedTool("vendor_post"), tools.Origin("vendor")); err != nil {
		t.Fatalf("registering the delivering tool: %v", err)
	}
	reply := turn.Reply{Kind: turn.ReplyTool, Surface: "vendor"}.String()
	stop := &sandbox.RunStop{By: "jane"}
	earlier := e.runEnding(company, sandbox.PendingRun{AgentHandle: "swe", Reply: reply,
		PriorCalls: []string{"vendor_post"}, Stop: stop}, colleagueAnswer{})
	if earlier.Outcome != executordomain.OutcomeStopped || !earlier.Delivered || earlier.DeliveryUnknown {
		t.Fatalf("a turn that replied in an earlier round ended %+v, want its reply delivered", earlier)
	}
	none := e.runEnding(company, sandbox.PendingRun{AgentHandle: "swe", Reply: reply, Stop: stop},
		colleagueAnswer{})
	if none.Delivered || none.DeliveryUnknown {
		t.Fatalf("a turn that never replied ended %+v, want its reply known not to have gone", none)
	}
	elided := e.runEnding(company, sandbox.PendingRun{AgentHandle: "swe", Reply: reply,
		BridgeCallsElided: 4, Stop: stop}, colleagueAnswer{})
	if elided.Delivered || !elided.DeliveryUnknown {
		t.Fatalf("a record whose calls were elided ended %+v, want whether it replied not known", elided)
	}
}

// THE END CARRIES THE TURN'S OWN DECISION AND BREACH, NEVER THE REVIEWER'S LAST
// WORD: a turn the loop failed on a guard after its reviewer said `done` ended
// failed, saying which guard; what the reviewer said landed and what stopped a
// blocked round are carried; and a stop says who stopped it and why.
//
// Mutation: read the decision off LastReview, drop the breach's account, or
// leave the stop's author off, and a field below says something else.
func TestTheEndCarriesTheTurnsOwnDecisionAndBreach(t *testing.T) {
	t.Parallel()
	res := turn.Result{
		Decision: phase.Failed, Delivered: true,
		Breach:     &turn.Breach{Kind: types.GuardDepthCap, Detail: "three hops"},
		LastReview: &turn.Review{Decision: phase.Done, CompletedWork: "MR !42 is up"},
		LastWork:   &turn.Work{Outcome: turn.OutcomeBlocked, Evidence: "waiting on the founder"},
	}
	reply := turn.Reply{Kind: turn.ReplyTool, Surface: "chatlet"}
	ending := endingOf(res, reply, colleagueAnswer{})
	if ending.Outcome != executordomain.OutcomeFailed || ending.Decision != string(phase.Failed) ||
		!strings.Contains(ending.Failure, string(types.GuardDepthCap)) || ending.CompletedWork != "MR !42 is up" ||
		ending.BlockedOn != "waiting on the founder" || !ending.Delivered || ending.DeliveredTo != "chatlet" {
		t.Fatalf("the ending is %+v", ending)
	}
	leaked := endingOf(turn.Result{Decision: phase.Failed, Breach: &turn.Breach{Kind: types.GuardDepthCap,
		Detail: "panic: token " + leakedToken}}, reply, colleagueAnswer{})
	if strings.Contains(leaked.Failure, leakedToken) {
		t.Fatalf("the breach's account carried a credential the guard's error quoted: %q", leaked.Failure)
	}
	abandoned := abandonedEnding(turn.Result{}, reply, colleagueAnswer{}, "it acted and broke",
		errors.New("push failed: "+leakedToken))
	if abandoned.Outcome != executordomain.OutcomeFailed || strings.Contains(abandoned.Failure, leakedToken) {
		t.Fatalf("the abandoned turn's ending is %+v, want it failed, its error redacted", abandoned)
	}
	stop := &stopError{pause: coord.SeatPause{By: "jane", Reason: "wrong repository", StopRunning: true}}
	stopped := stoppedEnding(turn.Result{Decision: phase.Done}, reply, stop, colleagueAnswer{})
	if stopped.Outcome != executordomain.OutcomeStopped || stopped.StoppedBy != "jane" ||
		stopped.StopReason != "wrong repository" || stopped.Failure != "" {
		t.Fatalf("the stopped ending is %+v", stopped)
	}
}

// A TURN THAT OWED A COLLEAGUE THE ENGINE'S ANSWER ENDS SAYING WHAT THE ENGINE
// SENT AND WHETHER IT WENT — which the turn's own calls cannot say — and a turn
// that owed none says nothing of it.
//
// Mutation: take Delivered off the turn's record for an engine's answer, which
// reads `true` by construction, or drop the kind, and a reply that never went
// reads as delivered.
func TestTheEndSaysWhatTheEngineAnsweredAColleague(t *testing.T) {
	t.Parallel()
	res := turn.Result{Decision: phase.Done, Artifact: "the answer", Delivered: true}
	reply := turn.Reply{Kind: turn.ReplyEngine}
	for _, tc := range []struct {
		name      string
		answered  colleagueAnswer
		answer    string
		delivered bool
	}{
		{"replied", colleagueAnswer{owed: true, kind: "artifact", replied: true}, "artifact", true},
		{"refused", colleagueAnswer{owed: true, kind: "artifact"}, "artifact", false},
		{"closed unanswered", colleagueAnswer{owed: true, kind: "artifact", closed: true}, "channel_closed", false},
		{"closed answered", colleagueAnswer{owed: true, kind: "artifact", closed: true, replied: true},
			"channel_closed", true},
	} {
		ending := endingOf(res, reply, tc.answered)
		if ending.Answer != tc.answer || ending.Delivered != tc.delivered || ending.DeliveredTo != replyA2A {
			t.Errorf("%s: the ending is %+v", tc.name, ending)
		}
	}
	if ending := endingOf(res, turn.Reply{Kind: turn.ReplyNone}, colleagueAnswer{}); ending.Answer != "" {
		t.Errorf("a turn that owed no answer ended saying %q", ending.Answer)
	}
	for _, tc := range []struct {
		res  turn.Result
		err  error
		kind string
	}{
		{res, nil, "artifact"},
		{turn.Result{Decision: phase.Failed, Breach: &turn.Breach{Kind: types.GuardDepthCap}}, nil, "guard"},
		{turn.Result{Decision: phase.Skipped}, nil, "unproduced"},
		{turn.Result{}, &runLostError{reason: "box_gone"}, "lost"},
	} {
		if _, kind := answerContent(tc.res, tc.err); kind != tc.kind {
			t.Errorf("answerContent(%+v, %v) is a %q, want %q", tc.res.Decision, tc.err, kind, tc.kind)
		}
	}
}

// A RESUMED SEGMENT THAT PARKS AGAIN RECORDS NO END — its turn goes on, on the
// run its reviewer's send-back relaunched — and the turn's one end, with the
// decision that ended it, is recorded once the relaunched run's segment ends
// it.
//
// Mutation: owe the end of a resumed segment that parked again, and the turn's
// end carries that segment's send-back rather than the decision that ended it.
func TestAResumeThatParksAgainRecordsNoEndUntilItsTurnEnds(t *testing.T) {
	t.Parallel()
	e, rig, spy := loggingReviewCase(t, &reviewWatch{sendBack: 1}, nil)
	parkOnARun(t, e, rig, "run-1")
	if err := rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if row := rig.row(t, "run-1"); row.Status != sandbox.StatusRunning {
		t.Fatalf("the resumed segment did not park again: %s", row.Status)
	}
	if ends := awaitEnds(t, spy, "run-1", 0); len(ends) != 0 {
		t.Fatalf("recorded %+v for a turn that parked again, want nothing yet", ends)
	}
	if err := rig.collect(t, "run-1"); err != nil {
		t.Fatalf("collecting the relaunched run: %v", err)
	}
	ends := awaitEnds(t, spy, "run-1", 1)
	if len(ends) != 1 || ends[0].Ending.Decision != string(phase.Done) {
		t.Fatalf("recorded %+v, want the turn's one end, done", ends)
	}
}

// A RESUMED TURN A PERSON STOPPED BEFORE ITS FIRST ROUND ENDS AS STOPPED ON THE
// EXECUTOR LOG, ONCE: the stop lands once the run's completion is claimed —
// which leaves the claimed run to its turn — and the resumed turn's fence closes
// it at its first round, owing the end on the run's record for the run's ending
// to record, with who stopped it and why.
//
// Mutation: drop the end from the resumed turn's stop, or record it from the
// frame and again from the ending, and the turn ends nowhere, or twice.
func TestAResumedTurnStoppedAtItsFirstRoundEndsAsStopped(t *testing.T) {
	t.Parallel()
	e, spy := loggingEngine(t)
	rig := rigRuns(t, e, halfwayPlay)
	parkOnRun(t, e)
	var once sync.Once
	for _, runner := range rig.runners {
		runner.CollectFunc = func(context.Context) error {
			once.Do(func() {
				pause := janeStops()
				e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat, Pause: &pause})
			})
			return nil
		}
	}
	if err := rig.collect(t, "run-1"); err != nil {
		t.Logf("the stopped resume's delivery answered %v", err)
	}
	ends := awaitEnds(t, spy, "run-1", 1)
	if len(ends) != 1 || ends[0].Ending.Outcome != executordomain.OutcomeStopped ||
		ends[0].Ending.StoppedBy != "jane" || ends[0].Ending.StopReason != "wrong repository" {
		t.Fatalf("recorded %+v, want the turn's one end, stopped by Jane for her reason", ends)
	}
}

// A SEND-BACK WHOSE RELAUNCHED RUN IS STOPPED BEFORE ITS PARK ENDS ON THE
// EXECUTOR LOG FROM THE STOP'S OWN ENDING, ONCE: the resumed segment that
// relaunched it records nothing, so the end is recorded after the stop's read
// has taken the relaunched job's session, which the end's tombstone would
// otherwise refuse.
//
// Mutation: record the end from the resumed segment's refused park again, and
// the turn's end is recorded twice, or comes before the stop's capture.
func TestARelaunchStoppedBeforeItsParkEndsOnTheExecutorLogOnce(t *testing.T) {
	t.Parallel()
	var parks *parkStopsFirst
	e, rig, spy := loggingReviewCase(t, &reviewWatch{sendBack: 1}, func(s sandbox.PendingStore) sandbox.PendingStore {
		parks = &parkStopsFirst{PendingStore: s}
		return parks
	})
	parkOnARun(t, e, rig, "run-1")
	// THE NEXT PARK, the relaunch's, lets the stop land first.
	parks.mu.Lock()
	parks.done = false
	parks.before = func() {
		pause := janeStops()
		e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat, Pause: &pause})
	}
	parks.mu.Unlock()
	if err := rig.collect(t, "run-1"); err != nil {
		t.Logf("the stopped relaunch's delivery answered %v", err)
	}
	ends := awaitEnds(t, spy, "run-1", 1)
	if len(ends) != 1 || ends[0].Ending.Outcome != executordomain.OutcomeStopped ||
		ends[0].Ending.StoppedBy != "jane" {
		t.Fatalf("recorded %+v, want the turn's one end, stopped by Jane", ends)
	}
	if refused := spy.capturesAfterEnd(); refused != 0 {
		t.Errorf("%d captures of the turn came after its end", refused)
	}
}

// WHAT THE ENGINE ANSWERED A COLLEAGUE IS WHAT ITS REPLY DID, read off the reply
// itself: an answer the channel took is replied; the same answer again, to the
// channel the first one closed — the frame that ends the turn repeated after a
// crash — is closed and still replied, since the channel holds that answer; one
// to a channel closed with nothing said on it is closed and not replied; and
// one the service refused for anything else is not replied either. Each says
// what it sent.
//
// Mutation: say replied before the reply is sent, ignore its error, or read
// every closed channel as never answered, and a row below reads otherwise.
func TestWhatTheEngineAnsweredAColleagueIsWhatItsReplyDid(t *testing.T) {
	t.Parallel()
	e := watchdogEngine(t)
	company := e.Company()
	svc := e.a2aService(company)
	if svc == nil {
		t.Fatal("no a2a service on an engine with a fleet and a queue")
	}
	open := func() *sandbox.ColleagueAsk {
		t.Helper()
		id, err := svc.Open(t.Context(), a2a.Ask{
			Requester: "ceo", Target: "cto", Brief: "What broke last night?", SenderRole: "CEO",
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return &sandbox.ColleagueAsk{ChannelID: id, Question: "What broke last night?"}
	}
	cto := answeringTurn{handle: "cto", runID: "run-answer", workKey: "wk-answer", depth: 1}
	res := turn.Result{Decision: phase.Done, Artifact: "the deploy at 02:14 rolled back"}

	ask := open()
	if got := e.answerColleague(t.Context(), company, cto, ask, res, nil); !got.owed || !got.replied ||
		got.closed || got.kind != "artifact" || got.sent == "" {
		t.Errorf("an answer the channel took: %+v, want it replied", got)
	}
	if got := e.answerColleague(t.Context(), company, cto, ask, res, nil); !got.closed || !got.replied {
		t.Errorf("the same answer again, to the channel it closed: %+v, want closed and replied", got)
	}
	swept := open()
	if err := svc.Close(t.Context(), a2a.Closure{ChannelID: swept.ChannelID}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := e.answerColleague(t.Context(), company, cto, swept, res, nil); !got.closed || got.replied {
		t.Errorf("an answer to a channel closed with nothing said on it: %+v, want closed, not replied", got)
	}
	stranger := answeringTurn{handle: "nobody-on-the-channel", runID: "run-other", workKey: "wk-other"}
	if got := e.answerColleague(t.Context(), company, stranger, open(), res, nil); !got.owed || got.replied ||
		got.closed || got.sent == "" {
		t.Errorf("an answer the service refused: %+v, want it owed, sent and not replied", got)
	}
}
