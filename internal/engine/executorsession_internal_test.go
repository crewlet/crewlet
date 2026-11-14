package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// leakedToken is a credential a box or a tool put in an error the engine quotes.
const leakedToken = "ghp_" + "Ab3dEf6hIj9kLm2nOp5qRs8tUv1wXy4zAb7d"

// executorLogSpy is the executor log's writes as a case reads them: every end
// a turn's ending recorded, and EVERY ATTEMPT AT ONE, counted — the log refuses
// a second end of a turn ([executordomain.ErrTurnEnded]), which the engine
// reads as the end it already has, so a turn ended twice shows only in the
// count ([awaitEnds]). An end tombstones its turn, as the log's does: a capture
// of an ended turn is refused. These cases' engines keep no session — they have
// no object store — so the order of a capture and its turn's end is the
// sandbox package's cases' to hold, where the keep is the case's.
type executorLogSpy struct {
	mu    sync.Mutex
	ends  []executordomain.NewEnd
	ended map[string]bool
	calls map[string]int
}

// Begin answers a turn's first read held wherever it names a base, as the log
// does where nothing moved the base before it landed. What a begin carries is
// the conversation cases' to read (conversation_internal_test.go's headLog);
// these cases are about how a turn ends.
func (s *executorLogSpy) Begin(_ context.Context, _ executordomain.Call,
	n executordomain.NewBegin) (executordomain.BeginResult, error) {
	return executordomain.BeginResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied},
		Held: n.BaseTurn != ""}, nil
}

func (s *executorLogSpy) Capture(_ context.Context, _ executordomain.Call,
	n executordomain.NewCapture) (executordomain.CaptureResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended[n.Turn] {
		return executordomain.CaptureResult{}, fmt.Errorf("%w: %s", executordomain.ErrTurnEnded, n.Turn)
	}
	return executordomain.CaptureResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}, Recorded: true}, nil
}

func (s *executorLogSpy) End(_ context.Context, _ executordomain.Call, n executordomain.NewEnd) (statelog.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended == nil {
		s.ended, s.calls = map[string]bool{}, map[string]int{}
	}
	s.calls[n.Turn]++
	if s.ended[n.Turn] {
		return statelog.Result{}, fmt.Errorf("%w: %s", executordomain.ErrTurnEnded, n.Turn)
	}
	s.ended[n.Turn] = true
	s.ends = append(s.ends, n)
	return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
}

// endCalls is how many times an end of turnID was asked of the log, the ones
// it refused as a second included.
func (s *executorLogSpy) endCalls(turnID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[turnID]
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
// the count too, and FAILS the case on an end asked of the log more often than
// it was recorded: the log's refusal of a second end is taken by the engine for
// the end it already has, so a turn ended by two paths records one end and
// shows only in the asking.
func awaitEnds(t *testing.T, spy *executorLogSpy, turnID string, want int) []executordomain.NewEnd {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(spy.endsOf(turnID)) < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	ends := spy.endsOf(turnID)
	if asked := spy.endCalls(turnID); asked > len(ends) {
		t.Errorf("the end of %s was asked of the log %d times for %d recorded: two paths ended the turn",
			turnID, asked, len(ends))
	}
	return ends
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
		!end.Ending.Outcome.Valid() || end.Ending.Outcome == executorkeep.OutcomeStopped {
		t.Fatalf("the end is %+v, want the turn's own decision %q", end, decision)
	}
}

// A PARKED TURN A PERSON STOPPED ENDS AS STOPPED ON THE EXECUTOR LOG, by the
// run's ending, which no frame of the turn reaches: who stopped it and why,
// their reason carried as theirs.
//
// Mutation: drop the end from the stop's close, and the stopped turn ends
// nowhere on the executor log.
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
	if ending := ends[0].Ending; ending.Outcome != executorkeep.OutcomeStopped ||
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
	if len(ends) != 1 || ends[0].Ending.Outcome != executorkeep.OutcomeStopped ||
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
	if end.Ending.Outcome != executorkeep.OutcomeLost || !strings.Contains(end.Ending.Failure, "lost") ||
		end.WorkKey != "wk-1" || end.Handle != "swe" {
		t.Fatalf("the end is %+v, want the turn lost, its seat and its unit of work", end)
	}
}

// A STOP'S ACCOUNT OF WHETHER THE TURN REPLIED READS EVERY ROUND'S CALLS: a turn
// that replied in an earlier round and was stopped in a later one replied, and
// one whose calls were elided from the log, the kept ones showing none, or
// whose log the ending could not read, is not known to have — never "did not
// reply" off half a log, or none. Unless the log kept the dropped calls'
// identities: a reply among them replied, and a tally of other tools is a
// turn known not to have. And a log holding a call still in flight, its outcome
// unknown, is a turn not known to have replied either: that call may be the
// reply.
//
// Mutation: read the current round's calls alone, answer an elided log with a
// definite no, or read an unread log as an empty one, and the turn's end says
// a reply that went out did not; ignore the identities, and a reply from the
// middle of a long run is unknown; ignore the calls in flight, and a reply
// still running is said not to have gone.
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
	run := sandbox.PendingRun{AgentHandle: "swe", Reply: reply, Stop: stop}
	earlier := e.runEnding(t.Context(), company, run, sandbox.RunCalls{Log: sandbox.BridgeLog{
		PriorCalls: []string{"vendor_post"}}}, colleagueAnswer{})
	if earlier.Outcome != executorkeep.OutcomeStopped || !earlier.Delivered || earlier.DeliveryUnknown {
		t.Fatalf("a turn that replied in an earlier round ended %+v, want its reply delivered", earlier)
	}
	none := e.runEnding(t.Context(), company, run, sandbox.RunCalls{}, colleagueAnswer{})
	if none.Delivered || none.DeliveryUnknown {
		t.Fatalf("a turn that never replied ended %+v, want its reply known not to have gone", none)
	}
	elided := e.runEnding(t.Context(), company, run, sandbox.RunCalls{Log: sandbox.BridgeLog{Elided: 4}}, colleagueAnswer{})
	if elided.Delivered || !elided.DeliveryUnknown {
		t.Fatalf("a log whose calls were elided ended %+v, want whether it replied not known", elided)
	}
	unread := e.runEnding(t.Context(), company, run, sandbox.RunCalls{Unread: errors.New("the store is down")},
		colleagueAnswer{})
	if unread.Delivered || !unread.DeliveryUnknown {
		t.Fatalf("a log the ending could not read ended %+v, want whether it replied not known", unread)
	}
	// THE ROUND'S TALLY of the calls its log dropped, by identity.
	round := []sandbox.BridgeCall{{Name: "read_file", Seq: 1}}
	tallied := e.runEnding(t.Context(), company, run, sandbox.RunCalls{Log: sandbox.BridgeLog{Calls: round, Elided: 4,
		ElidedCalls: []sandbox.ElidedCall{{Name: "read_file", Digest: "a", Calls: 3},
			{Name: "vendor_post", Digest: "b", Calls: 1}}}}, colleagueAnswer{})
	if !tallied.Delivered || tallied.DeliveryUnknown {
		t.Fatalf("a log whose dropped calls hold the reply ended %+v, want its reply delivered", tallied)
	}
	reads := e.runEnding(t.Context(), company, run, sandbox.RunCalls{Log: sandbox.BridgeLog{Calls: round, Elided: 4,
		ElidedCalls: []sandbox.ElidedCall{{Name: "read_file", Digest: "a", Calls: 4}}}}, colleagueAnswer{})
	if reads.Delivered || reads.DeliveryUnknown {
		t.Fatalf("a log whose dropped calls are all reads ended %+v, want it known not to have replied", reads)
	}
	inFlight := e.runEnding(t.Context(), company, run, sandbox.RunCalls{Log: sandbox.BridgeLog{Calls: round,
		InFlight: []sandbox.BridgeCall{{Name: "vendor_post", Seq: 2}}}}, colleagueAnswer{})
	if inFlight.Delivered || !inFlight.DeliveryUnknown {
		t.Fatalf("a log holding a call still in flight ended %+v, want whether it replied not known", inFlight)
	}
}

// A LOST OR STOPPED PICKUP'S RUN IS HELD TO ITS ITEM (ADR-0122): a comment the
// run's calls show delivered nothing while the item is still the seat's with no
// typed answer on it; a typed answer, or an item that left the seat, lets the
// delivery stand; a read that fails, or answers what this build does not know,
// leaves it unknown; a run whose calls delivered nothing reads no answer; and a
// turn no pickup handed an item reads none either.
//
// Mutation: answer the ending from the calls alone, and a run lost after only a
// comment is recorded as having answered its pickup.
func TestALostPickupsRunIsHeldToItsItem(t *testing.T) {
	t.Parallel()
	reads := 0
	read := func(a types.PickupAnswer, err error) func(context.Context) (types.PickupAnswer, error) {
		return func(context.Context) (types.PickupAnswer, error) { reads++; return a, err }
	}
	for _, c := range []struct {
		name             string
		answer           types.PickupAnswer
		err              error
		delivered, known bool
	}{
		{"a comment alone", types.PickupUnanswered, nil, false, true},
		{"a typed answer", types.PickupTyped, nil, true, true},
		{"an item trashed under the run", types.PickupRemoved, nil, true, true},
		{"an item finished by somebody else", types.PickupFinished, nil, true, true},
		{"an item reassigned", types.PickupReassigned, nil, true, true},
		{"a read that failed", "", errors.New("no data node answered"), false, false},
		{"an answer this build does not know", "pondered", nil, false, false},
	} {
		delivered, known := heldToItem(t.Context(), "turn-1", "w-1", true, true, read(c.answer, c.err))
		if delivered != c.delivered || known != c.known {
			t.Errorf("%s: delivered %v known %v, want %v and %v", c.name, delivered, known,
				c.delivered, c.known)
		}
	}
	before := reads
	if delivered, known := heldToItem(t.Context(), "turn-1", "w-1", false, true,
		read(types.PickupTyped, nil)); delivered || !known || reads != before {
		t.Errorf("a run whose calls delivered nothing: delivered %v known %v after %d reads",
			delivered, known, reads-before)
	}
	if delivered, known := heldToItem(t.Context(), "turn-1", "", true, true, nil); !delivered || !known {
		t.Errorf("a turn with no pickup's item: delivered %v known %v, want its calls' answer",
			delivered, known)
	}
}

// THE CALL A PERSON APPROVED IS A DELIVERY OF ITS TURN. A tool at Needs approval
// that reaches the party waiting is held, and the run parks on it; the person
// allows it once, the resume makes it on the engine host, and the job that
// carries the work on opens. Whether the turn replied is read off the run's row
// — by a stop's or a loss's account of the turn, and by the collection's own
// delivery check over the round's calls — and the call was nowhere on it: the
// job carrying the work on clears the held call, whose result was the only
// trace of it. A reply that went out was recorded as one that did not, and the
// next turn told so.
//
// Mutation: drop the call's append to the log after its result is recorded
// ([sandbox.PendingStore.RecordHeldResult]), or have heldCommit record it as
// failed whatever the surface answered, and both readers say the turn did not
// reply.
func TestTheCallAPersonApprovedIsADeliveryOfItsTurn(t *testing.T) {
	t.Parallel()
	e, _ := loggingEngine(t)
	company := e.Company()
	// A TOOL THAT REACHES THE PERSON WAITING, on the surface they wait on.
	if err := company.Tools.Register(namedTool("vendor_post"), tools.Origin("vendor")); err != nil {
		t.Fatalf("registering the delivering tool: %v", err)
	}
	reply := turn.Reply{Kind: turn.ReplyTool, Surface: "vendor"}.String()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": []byte("crewlet-test-sealing-key-32bytes")}})
	if err != nil {
		t.Fatal(err)
	}
	seal := sandbox.NewCallSeal(cipher)
	store := sandbox.NewCoordStore(e.backends.Fleet)
	ctx := t.Context()
	row := sandbox.PendingRun{TurnID: "run-1", AgentHandle: "swe", AgentID: "a-1", Role: "SWE",
		Harness: "claude-code", Reply: reply}
	opened, err := store.BeginLaunch(ctx, row, sandbox.Fence{})
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	launch := opened.LaunchID

	// HELD, and parked on: the call to vendor_post the job made.
	held := sandbox.HeldCall{ID: "0190f3a8-0000-7000-8000-000000000001", LaunchID: launch,
		Server: "vendor", Tool: "vendor_post", Raw: "post", Digest: "sha256:d1", HeldAt: time.Now().UTC()}
	if held.Arguments, err = seal.SealArguments("run-1", held, map[string]any{"body": "done"}); err != nil {
		t.Fatal(err)
	}
	const question = "May swe call vendor_post on the vendor MCP server?"
	if got, err := store.RecordAsk(ctx, "run-1", launch, sandbox.Ask{Question: question,
		Decision: sandbox.HeldDecision(question, nil), To: sandbox.AskToManager, Held: &held}); err != nil ||
		got != sandbox.AskRecorded {
		t.Fatalf("RecordAsk = %v, %v", got, err)
	}
	if err := store.MarkAwaiting(ctx, "run-1", sandbox.Clarification{AskedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	if _, ok, err := store.RecordAnswer(ctx, "run-1", launch, sandbox.RecordedAnswer{
		Sealed: []byte("sealed: once"), Via: types.AnswerViaOperator, EventIDs: []string{"r1"}},
		sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("RecordAnswer = %v, %v", ok, err)
	}
	claimed, ok, err := store.ClaimForResume(ctx, "run-1", sandbox.RecordedAnswerTail(launch), sandbox.Fence{})
	if err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}

	// MADE, as the resume makes it: marked, then its result recorded through
	// the engine's commit with what the surface answered.
	commit := heldCommit{seal: seal, held: &sandbox.HeldResume{TurnID: "run-1", Call: *claimed.HeldCall,
		Choice: sandbox.HeldOnce,
		Mark: func(ctx context.Context) (sandbox.HeldCall, bool, error) {
			return store.MarkHeldCall(ctx, "run-1", launch, sandbox.Fence{})
		},
		Record: func(ctx context.Context, made sandbox.HeldMade) error {
			recorded, err := store.RecordHeldResult(ctx, "run-1", launch, made, sandbox.Fence{})
			if err == nil && !recorded {
				err = errors.New("the result was not recorded")
			}
			return err
		},
	}}
	if prior, err := commit.Begin(ctx); err != nil || !prior.First {
		t.Fatalf("Begin = %+v, %v, want this mark the first", prior, err)
	}
	if err := commit.Record(ctx, sandbox.HeldResult{Output: "posted"}, toolloop.ToolResult{Output: "posted",
		Origin: tools.Origin("vendor"), Server: "vendor", Source: tools.Origin("vendor")}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// NEITHER ITS ARGUMENTS NOR THEIR DIGEST RIDE ITS RECORD: they are sealed
	// on the held call, and a digest in the clear would confirm a guess at
	// them. The record says the call's identity is withheld, which a later
	// segment's call log counts as a call of its tool whose arguments are not
	// known.
	recorded, _, err := store.Get(ctx, "run-1")
	if err != nil {
		t.Fatalf("reading the run the call was approved on: %v", err)
	}
	if logged, err := store.BridgeLog(ctx, recorded); err != nil || len(logged.Calls) == 0 {
		t.Fatalf("the approved call's record = %+v (%v)", logged.Calls, err)
	} else if made := logged.Calls[len(logged.Calls)-1]; made.Digest != "" || made.Args != "" ||
		!made.Approved || !made.Withheld() {
		t.Fatalf("the approved call is recorded as %+v, want it approved with its arguments and their "+
			"digest withheld", made)
	}
	// THE JOB THAT CARRIES THE WORK ON opens, the held call cleared with the
	// question it was.
	carried := row
	carried.Continues = true
	if _, err := store.BeginLaunch(ctx, carried, sandbox.Fence{}); err != nil {
		t.Fatalf("the carry-on BeginLaunch: %v", err)
	}
	got, found, err := store.Get(ctx, "run-1")
	if err != nil || !found || got.HeldCall != nil {
		t.Fatalf("the carried-on row = %+v, %v, %v, want it found with no held call", got.HeldCall, found, err)
	}

	// A STOP'S ACCOUNT reads the round's calls, off the run's log as the
	// ending reads it...
	logged, err := store.BridgeLog(ctx, got)
	if err != nil {
		t.Fatalf("reading the carried-on run's log: %v", err)
	}
	stopped := got
	stopped.Stop = &sandbox.RunStop{By: "jane"}
	if ending := e.runEnding(t.Context(), company, stopped, sandbox.RunCalls{Log: logged}, colleagueAnswer{}); !ending.Delivered ||
		ending.DeliveryUnknown {
		t.Fatalf("a turn whose reply was the approved call ended %+v, want its reply delivered", ending)
	}
	// ...and so does the collection's own delivery check, over the bridged
	// log the resumed turn's record is built from.
	var calls []ledger.Call
	for _, call := range bridgedCalls(logged.Calls) {
		calls = append(calls, call.Call)
	}
	if !turn.Answered(calls, deliverySurface(e.seatRegistry(company, "swe")), turn.ParseReply(reply)) {
		t.Fatalf("the collection's check reads %+v as no reply, want the approved call its delivery", calls)
	}
}

// THE ENGINE'S VIEW OF A SEAT'S TOOLS SAYS WHICH WRITES IT PRESUMES, as the
// runner's does: an account of a turn's calls read off this surface would
// otherwise name as an act a search through a tool that no hint marks as a
// read or as a write.
//
// Mutation: leave PresumedWrites out of deliverySurface, and the search is
// named as an act.
func TestTheEnginesViewOfASeatsToolsSaysWhichWritesItPresumes(t *testing.T) {
	t.Parallel()
	reg := tools.NewRegistry()
	for name, ann := range map[string]tools.Annotations{
		"jira_search_issues": {},
		"jira_get_issue":     {ReadOnly: mcp.Yes},
	} {
		if err := reg.RegisterWith(namedTool(name), tools.Origin("jira"), ann); err != nil {
			t.Fatal(err)
		}
	}
	s := deliverySurface(reg)
	if !slices.Equal(s.PresumedWrites, []string{"jira_search_issues"}) {
		t.Errorf("PresumedWrites = %v, want only the tool its server leaves unmarked", s.PresumedWrites)
	}
	if account := turn.ActedAccount([]ledger.Call{{Name: "jira_search_issues"}}, s); !strings.Contains(account,
		"cannot tell it from a write") {
		t.Errorf("account = %q, want the search named as a call the engine cannot tell from a write", account)
	}
}

// THE END CARRIES THE TURN'S OWN DECISION AND BREACH, NEVER THE REVIEWER'S LAST
// WORD: a turn the loop failed on a guard after its reviewer said `done` ended
// failed, saying which guard; what the reviewer said landed and what stopped a
// blocked round are carried; a stop says who stopped it and why; and a turn
// abandoned, after it broke or as its first launch failed once its run was
// recorded, ended failed, saying so, with the error that abandoned it redacted
// in its own account, before any path to the log redacts it again.
//
// Mutation: read the decision off LastReview, drop the breach's account, leave
// the stop's author off, or quote an abandoned turn's or launch's error
// unredacted, and a field below says something else.
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
	if ending.Outcome != executorkeep.OutcomeFailed || ending.Decision != string(phase.Failed) ||
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
	if abandoned.Outcome != executorkeep.OutcomeFailed || strings.Contains(abandoned.Failure, leakedToken) {
		t.Fatalf("the abandoned turn's ending is %+v, want it failed, its error redacted", abandoned)
	}
	launchEnd := launchAbandonedEnding(turn.Result{}, reply, colleagueAnswer{},
		errors.New("the box refused to start its job: "+leakedToken))
	if launchEnd.Outcome != executorkeep.OutcomeFailed ||
		!strings.Contains(launchEnd.Failure, "abandoned after its run was recorded") ||
		!strings.Contains(launchEnd.Failure, "the box refused to start its job") ||
		strings.Contains(launchEnd.Failure, leakedToken) {
		t.Fatalf("the abandoned launch's ending is %+v, want it failed, saying why, its error redacted",
			launchEnd)
	}
	stop := &stopError{pause: coord.SeatPause{By: "jane", Reason: "wrong repository", StopRunning: true}}
	stopped := stoppedEnding(turn.Result{Decision: phase.Done}, reply, stop, colleagueAnswer{})
	if stopped.Outcome != executorkeep.OutcomeStopped || stopped.StoppedBy != "jane" ||
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
		runner.CollectFunc = func(ctx context.Context) error {
			once.Do(func() {
				pause := janeStops()
				e.applySeatPause(ctx, coord.SeatPauseUpdate{Seat: pause.Seat, Pause: &pause})
			})
			return nil
		}
	}
	if err := rig.collect(t, "run-1"); err != nil {
		t.Logf("the stopped resume's delivery answered %v", err)
	}
	ends := awaitEnds(t, spy, "run-1", 1)
	if len(ends) != 1 || ends[0].Ending.Outcome != executorkeep.OutcomeStopped ||
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
// the turn's end is asked of the log twice.
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
	if len(ends) != 1 || ends[0].Ending.Outcome != executorkeep.OutcomeStopped ||
		ends[0].Ending.StoppedBy != "jane" {
		t.Fatalf("recorded %+v, want the turn's one end, stopped by Jane", ends)
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

// A STOP DECIDED ON THE RUN A TURN WAS LAUNCHING IS TOLD FROM ONE THE TURN'S OWN
// FENCE MET, however the turn's frames wrap it: the run's ending answers the
// first kind's colleague and records its end, from the run's record, and the
// frame the second's. Answered by both, the asker woke to two answers.
//
// Mutation: leave the run's own stop unmarked at its launch, or read the mark
// off the outer error alone, and a row below reads otherwise.
func TestAStopDecidedOnTheRunIsToldFromTheTurnsOwn(t *testing.T) {
	t.Parallel()
	ofRun := &stopError{pause: coord.SeatPause{By: "jane"}, handle: "swe", ofRun: true}
	fenced := &stopError{pause: coord.SeatPause{By: "jane"}, handle: "swe"}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"the run's own stop":                       {ofRun, true},
		"the run's own stop, wrapped by its frame": {fmt.Errorf("the executor phase: %w", ofRun), true},
		"the turn's own fence":                     {fenced, false},
		"no stop at all":                           {errors.New("the box would not start"), false},
	} {
		if got := stoppedByRun(tc.err); got != tc.want {
			t.Errorf("%s: stoppedByRun = %v, want %v", name, got, tc.want)
		}
	}
}

// A RESUMED TURN THAT PANICS ONCE IT HAS BEGUN ENDS FAILED ON THE EXECUTOR LOG,
// saying it panicked and with what, redacted. The coordinator settles a begun
// turn that broke without a word of its own, and the frame that owes the turn's
// end on every other way out is what panicked: it ended nowhere. A turn that
// panics before it began owes nothing here — its run's ending announces it
// lost, and owes the turn's end itself.
//
// Mutation: drop the end from the panic's recovery, or owe it for a resume
// whose turn never began, and a row below differs.
func TestAResumedTurnThatPanicsOnceBegunEndsFailed(t *testing.T) {
	t.Parallel()
	for name, begins := range map[string]bool{"once begun": true, "before it began": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, spy := loggingEngine(t)
			rigRuns(t, e, halfwayPlay)
			req := sandbox.ResumeRequest{
				Run: sandbox.PendingRun{TurnID: "run-9", AgentHandle: "swe", WorkKey: "wk-9",
					WorkSince: time.Now().UTC()},
				Begin: func(context.Context, sandbox.ResumeCarry) error { return nil },
			}
			err := e.resumeGuarded(t.Context(), req, func(ctx context.Context, guarded sandbox.ResumeRequest) error {
				if begins {
					if beginErr := guarded.Begin(ctx, sandbox.ResumeCollects); beginErr != nil {
						return beginErr
					}
				}
				panic("the runner's registry was nil, its token " + leakedToken)
			})
			if !errors.Is(err, sandbox.ErrResumeAbandoned) {
				t.Fatalf("the panicking resume answered %v, want it abandoned", err)
			}
			ends := spy.endsOf("run-9")
			if !begins {
				if len(ends) != 0 {
					t.Fatalf("recorded %+v for a turn that never began", ends)
				}
				return
			}
			if len(ends) != 1 || ends[0].Ending.Outcome != executorkeep.OutcomeFailed ||
				!strings.Contains(ends[0].Ending.Failure, "panicked") ||
				strings.Contains(ends[0].Ending.Failure, leakedToken) {
				t.Fatalf("recorded %+v, want the turn failed, saying it panicked, redacted", ends)
			}
		})
	}
}
