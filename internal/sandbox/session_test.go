package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// sessionWrites is the executor log's writes as a case plays them: every
// capture and end asked, and what each attempt answers.
type sessionWrites struct {
	mu       sync.Mutex
	captures []capturedRecord
	ends     []endedRecord
	// refuse answers every capture attempt while it is set, as a log that
	// did not answer does.
	refuse error
}

type capturedRecord struct {
	call executordomain.Call
	n    executordomain.NewCapture
}

type endedRecord struct {
	call executordomain.Call
	n    executordomain.NewEnd
}

func (w *sessionWrites) Capture(_ context.Context, c executordomain.Call,
	n executordomain.NewCapture) (executordomain.CaptureResult, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.captures = append(w.captures, capturedRecord{call: c, n: n})
	if w.refuse != nil {
		return executordomain.CaptureResult{Result: statelog.Result{Outcome: statelog.OutcomeUnknown}}, w.refuse
	}
	return executordomain.CaptureResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}, Recorded: true}, nil
}

func (w *sessionWrites) End(_ context.Context, c executordomain.Call, n executordomain.NewEnd) (statelog.Result, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ends = append(w.ends, endedRecord{call: c, n: n})
	return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
}

func (w *sessionWrites) answer(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.refuse = err
}

func (w *sessionWrites) captured() []capturedRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]capturedRecord(nil), w.captures...)
}

func (w *sessionWrites) ended() []endedRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]endedRecord(nil), w.ends...)
}

// sessionKeep is the engine's part of keeping a session, as a case plays it.
type sessionKeep struct {
	mu      sync.Mutex
	policy  SessionPolicy
	putErr  error
	secrets []string
	// uploaded is what every put read.
	uploaded [][]byte
}

func (k *sessionKeep) Policy(context.Context) (SessionPolicy, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.policy, nil
}

func (k *sessionKeep) Secrets(context.Context, PendingRun) []string { return k.secrets }

func (k *sessionKeep) MintedAt(context.Context, string, string, time.Time) (time.Time, error) {
	return time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC), nil
}

func (k *sessionKeep) Put(_ context.Context, _, _ string, archive io.ReadCloser) (executordomain.SealedObject, error) {
	defer func() { _ = archive.Close() }()
	body, err := io.ReadAll(archive)
	if err != nil {
		return executordomain.SealedObject{}, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.putErr != nil {
		return executordomain.SealedObject{}, k.putErr
	}
	k.uploaded = append(k.uploaded, body)
	sum := sha256.Sum256(body)
	return executordomain.SealedObject{ContentKey: "k1", Object: objstore.Object{
		Key: objstore.KeyAt(time.Now()), Hash: objstore.Hash(hex.EncodeToString(sum[:])), Size: int64(len(body)),
	}}, nil
}

// keepingRig is a coordinator rig that keeps sessions: its runner's boxes hold
// one, and its records are the case's to read and to answer.
func keepingRig(t *testing.T) (*coordRig, *sessionWrites, *sessionKeep) {
	t.Helper()
	rig := newCoordRig(t)
	writes := &sessionWrites{}
	keep := &sessionKeep{policy: SessionPolicy{Keep: true, SeatBudget: 64 << 20},
		secrets: []string{"the-entrys-own-token"}}
	rig.coordinator.sessions, rig.coordinator.keep = writes, keep
	rig.coordinator.sessionRetry, rig.coordinator.recordBudget = time.Millisecond, 200*time.Millisecond
	rig.runner.Session = &FakeSession{
		Archive: SessionArchive{Harness: "claude-code", SessionID: "s-1", Format: 1, Version: "2.1.310",
			Size: 13, Path: "/home/user/.crewlet/session-capture"},
		Body: []byte("the transcript"),
	}
	return rig, writes, keep
}

// collectHeld collects turnID's finished job and leaves its row in place: the
// resume is refused, so the claim goes back with everything the collection
// stamped on it.
func collectHeld(t *testing.T, rig *coordRig, turnID string) {
	t.Helper()
	rig.resumer.failWith(errors.New("held for the case to read the row"))
	payload, ev := rig.completion(turnID)
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("the refused resume was acknowledged")
	}
}

// EVERY LAUNCH OF A TURN IS NUMBERED ONE PAST THE LAUNCH BEFORE IT, in the write
// that names it — the first 1, each relaunch the next — so every capture of the
// turn's session carries a place that only grows; and a row an older build
// wrote, which numbered nothing, takes 1 at its next launch here.
//
// Mutation: mint the ordinal anywhere but the launch's own write, start it at
// 0, or reset it on a relaunch, and a relaunch's capture is refused as stale or
// collides with the launch before it.
func TestEachLaunchOfATurnIsNumberedPastTheOneBefore(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	ctx := t.Context()
	launch := func() PendingRun {
		opened, err := rig.pending.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentHandle: "swe",
			AgentID: sweID, Role: "SWE", Harness: "claude-code"}, rigLease)
		if err != nil {
			t.Fatalf("BeginLaunch: %v", err)
		}
		return opened
	}
	first := launch()
	second := launch()
	third := launch()
	if first.LaunchSeq != 1 || second.LaunchSeq != 2 || third.LaunchSeq != 3 {
		t.Fatalf("the launches are numbered %d, %d, %d, want 1, 2, 3",
			first.LaunchSeq, second.LaunchSeq, third.LaunchSeq)
	}
	if stored := rig.get("t1"); stored.LaunchSeq != 3 || stored.LaunchID != third.LaunchID {
		t.Fatalf("the row holds launch %s at %d, want the third's %s at 3",
			stored.LaunchID, stored.LaunchSeq, third.LaunchID)
	}
	// A ROW AN OLDER BUILD WROTE numbers nothing; its next launch here is 1.
	if _, _, err := rig.pending.mutateAny(ctx, "t1", func(run *PendingRun) bool {
		run.LaunchSeq = 0
		return true
	}); err != nil {
		t.Fatalf("writing the older build's row: %v", err)
	}
	if next := launch(); next.LaunchSeq != 1 {
		t.Fatalf("the first launch under this build of an older row is %d, want 1", next.LaunchSeq)
	}
}

// A CAPTURE'S PLACE ONLY GROWS ON THE ROW. A newer capture takes the row, the
// same one moves on only to its outcome, nothing older or backwards lands, and
// a capture that landed is the turn's kept one wherever it is newer than that
// — an unresolved capture answered after a newer one began included.
//
// Mutation: let an equal place take any state, or keep only the newest
// attempt, and a late `begun` takes a landed capture back, or the answered
// older capture is never kept.
func TestACapturesPlaceOnlyGrowsOnTheRow(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launch("t1")
	ctx := t.Context()
	stamp := func(launch, seq int64, state CaptureState) bool {
		took, err := rig.pending.StampCapture(ctx, "t1", SessionCapture{LaunchSeq: launch, CaptureSeq: seq,
			State: state, Launch: "job", Object: "o"})
		if err != nil {
			t.Fatalf("StampCapture: %v", err)
		}
		return took
	}
	for _, step := range []struct {
		launch, seq int64
		state       CaptureState
		take        bool
	}{
		{1, 1, CaptureBegun, true},
		{1, 1, CaptureBegun, false},
		{1, 1, CaptureUnresolved, true},
		{1, 2, CaptureBegun, true},
		{1, 1, CaptureLanded, true}, // the older answer lands as the kept one
		{1, 1, CaptureRefused, false},
		{1, 2, CaptureRefused, true},
		{1, 2, CaptureLanded, false},
		{2, 1, CaptureLanded, true},
	} {
		if got := stamp(step.launch, step.seq, step.state); got != step.take {
			t.Fatalf("stamping (%d, %d) %s was taken %v, want %v", step.launch, step.seq, step.state,
				got, step.take)
		}
	}
	run := rig.get("t1")
	if run.Capture == nil || run.Capture.LaunchSeq != 2 || run.Capture.State != CaptureLanded {
		t.Errorf("the newest capture is %+v, want (2, 1) landed", run.Capture)
	}
	if run.Kept == nil || run.Kept.LaunchSeq != 2 || run.Kept.CaptureSeq != 1 {
		t.Errorf("the kept capture is %+v, want (2, 1)", run.Kept)
	}
}

// A COLLECTION KEEPS THE JOB'S SESSION THROUGH THE EXECUTOR LOG'S WRITES, before
// the box is paused: the archive the run's script packed at its harness's exit,
// handed the secrets the engine put in the box to scrub, uploaded, recorded at
// the launch's ordinal and its own, its digest kept in the box, stamped landed
// on the row with what the job had answered and read through — and the launch's
// own record says it was kept, and which session it ran.
//
// Mutation: take the session after the pause, pack it on demand for a job that
// packed its own, drop the secrets, stamp before the record lands, or leave the
// record's fields off `executor_collected`, and a check here goes red.
func TestACollectionKeepsTheJobsSessionThroughTheLogsWrites(t *testing.T) {
	t.Parallel()
	rig, writes, keep := keepingRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	session := rig.runner.Session
	rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
		Captured: &session.Archive})
	collectHeld(t, rig, "t1")

	takes := rig.runner.Takes()
	if len(takes) != 1 || takes[0].Pack {
		t.Fatalf("takes %+v, want one take of the archive the run's script packed", takes)
	}
	if strings.Join(takes[0].Scrub, ",") != "the-entrys-own-token" {
		t.Errorf("the take scrubs %v, want the engine's secrets", takes[0].Scrub)
	}
	if !rig.provider.Box(rig.get("t1").SandboxID).Paused() {
		t.Error("the collection did not pause the box after keeping its session")
	}
	captures := writes.captured()
	if len(captures) != 1 {
		t.Fatalf("recorded %d captures, want 1", len(captures))
	}
	got := captures[0].n
	if got.LaunchSeq != 1 || got.CaptureSeq != 1 || got.SessionID != "s-1" || got.Harness != "claude-code" ||
		got.Object.Size != int64(len(session.Body)) || got.SeatBudget != 64<<20 || got.ContentKey != "k1" {
		t.Errorf("the capture recorded %+v", got)
	}
	if string(keep.uploaded[0]) != "the transcript" {
		t.Errorf("uploaded %q, want the box's archive", keep.uploaded[0])
	}
	if session.Kept() != 1 {
		t.Errorf("the box recorded %d digests of the archive kept, want 1", session.Kept())
	}
	run := rig.get("t1")
	if run.Kept == nil || run.Kept.State != CaptureLanded || run.Kept.Object != got.Object.Key.String() ||
		!run.Kept.Final {
		t.Fatalf("the row keeps %+v, want the landed, final capture", run.Kept)
	}
	recs := rig.collected()
	if len(recs) != 1 || recs[0].SessionKeptBytes != int64(len(session.Body)) ||
		recs[0].HarnessSession != "s-1" || recs[0].HarnessVersion != "2.1.310" || recs[0].SessionNotKept != "" {
		t.Errorf("the launch's record is %+v, want the session it ran and its kept size", recs)
	}
}

// A JOB KEPT WHOLE IS NOT KEPT AGAIN. A collection retried after a resume that
// failed reads the same finished job, whose session nothing has changed: a
// second upload would hold the store's room for a day for the same bytes.
//
// Mutation: drop the check for a final capture of the job, and the retry
// uploads and records the session a second time.
func TestAJobKeptWholeIsNotKeptAgain(t *testing.T) {
	t.Parallel()
	rig, writes, keep := keepingRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	session := rig.runner.Session
	rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
	collectHeld(t, rig, "t1")
	collectHeld(t, rig, "t1")
	if n := len(writes.captured()); n != 1 || len(keep.uploaded) != 1 {
		t.Fatalf("recorded %d captures and %d uploads of one finished job, want 1 each", n, len(keep.uploaded))
	}
}

// A RECORD THE LOG DOES NOT ANSWER IS ASKED AGAIN UNDER ITS ONE OPERATION — the
// operation its turn, its launch and its place derive — and is left UNRESOLVED
// on the row once its budget is spent; the turn's end asks it once more under
// that same operation before it ends the turn, and the next launch's capture is
// another operation.
//
// Mutation: derive the operation per attempt, skip the end's ask, or derive it
// from the turn alone, and the operations below stop matching or collide.
func TestARecordIsAskedAgainUnderOneOperationAndByTheTurnsEnd(t *testing.T) {
	t.Parallel()
	rig, writes, _ := keepingRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	session := rig.runner.Session
	writes.answer(errors.New("estate: the write was sent to a data node that did not answer"))
	rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
	collectHeld(t, rig, "t1")

	asked := writes.captured()
	if len(asked) < 2 {
		t.Fatalf("the capture was asked %d times, want it asked again", len(asked))
	}
	for _, again := range asked[1:] {
		if again.call.OpID != asked[0].call.OpID {
			t.Fatalf("asked again under %s, want the first attempt's %s", again.call.OpID, asked[0].call.OpID)
		}
	}
	if run := rig.get("t1"); run.Capture == nil || run.Capture.State != CaptureUnresolved || run.Kept != nil {
		t.Fatalf("the row holds %+v, kept %+v, want the capture unresolved and nothing kept",
			run.Capture, run.Kept)
	}

	writes.answer(nil)
	rig.coordinator.EndTurn(t.Context(), TurnEnd{Handle: "swe", TurnID: "t1",
		Ending: executordomain.Ending{Outcome: executordomain.OutcomeCompleted}})
	all := writes.captured()
	if last := all[len(all)-1]; last.call.OpID != asked[0].call.OpID {
		t.Fatalf("the end asked the capture under %s, want its own %s", last.call.OpID, asked[0].call.OpID)
	}
	if ends := writes.ended(); len(ends) != 1 || ends[0].n.Ending.Outcome != executordomain.OutcomeCompleted {
		t.Fatalf("ends %+v, want the turn's one end after its capture", ends)
	}
	if run := rig.get("t1"); run.Kept == nil || run.Kept.State != CaptureLanded {
		t.Fatalf("the row keeps %+v, want the capture the end's ask landed", run.Kept)
	}

	// THE NEXT LAUNCH'S CAPTURE IS ANOTHER OPERATION.
	if _, err := rig.pending.BeginLaunch(t.Context(), PendingRun{TurnID: "t1", AgentHandle: "swe",
		AgentID: sweID, Role: "SWE", Harness: "claude-code"}, rigLease); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	next := executordomain.CaptureOpID(time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC), "swe", "t1",
		rig.get("t1").LaunchID, 1)
	if next == asked[0].call.OpID {
		t.Fatal("the next launch's first capture derives the operation the last launch's did")
	}
}

// AN UPLOAD THE STORE REFUSES IS RECORDED AS REFUSED, with why, so a later
// launch knows the launch's latest session was not kept rather than reading an
// older capture as its latest; nothing is recorded on the log.
//
// Mutation: leave a refused upload stamped `begun`, or record it anyway, and
// the row says nothing about it, or names bytes the store never took.
func TestARefusedUploadIsRecordedAsRefused(t *testing.T) {
	t.Parallel()
	rig, writes, keep := keepingRig(t)
	keep.putErr = objstore.ErrFull
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	session := rig.runner.Session
	rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
	collectHeld(t, rig, "t1")

	if n := len(writes.captured()); n != 0 {
		t.Fatalf("recorded %d captures of an upload the store refused", n)
	}
	run := rig.get("t1")
	if run.Capture == nil || run.Capture.State != CaptureRefused ||
		!strings.Contains(run.Capture.Reason, "reservation is spent") || run.Kept != nil {
		t.Fatalf("the row holds %+v, kept %+v, want the capture refused for the store's room",
			run.Capture, run.Kept)
	}
	if recs := rig.collected(); len(recs) != 1 || recs[0].SessionNotKept == "" || recs[0].SessionKeptBytes != 0 {
		t.Errorf("the launch's record is %+v, want it to say the session was not kept", recs)
	}
}

// THE SWITCH OFF KEEPS NOTHING AND STILL ENDS THE TURN. With
// `turn_engine.conversation_session.enabled: false` no session is taken out of
// a box and nothing is stamped, while the turn's end — the account, not the
// session — is still recorded.
//
// Mutation: read the switch after the take, or let it gate the end, and a
// session is taken, or the turn's end goes unrecorded.
func TestTheSwitchOffKeepsNothingAndStillEndsTheTurn(t *testing.T) {
	t.Parallel()
	rig, writes, keep := keepingRig(t)
	keep.policy.Keep = false
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	session := rig.runner.Session
	rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
	collectHeld(t, rig, "t1")
	if takes := rig.runner.Takes(); len(takes) != 0 {
		t.Fatalf("took %d sessions with the switch off", len(takes))
	}
	if run := rig.get("t1"); run.Capture != nil || run.Kept != nil {
		t.Fatalf("stamped %+v with the switch off", run.Capture)
	}
	rig.coordinator.EndTurn(t.Context(), TurnEnd{Handle: "swe", TurnID: "t1",
		Ending: executordomain.Ending{Outcome: executordomain.OutcomeFailed}})
	if ends := writes.ended(); len(ends) != 1 {
		t.Fatalf("recorded %d ends with the switch off, want the turn's one", len(ends))
	}
}

// A STOP KEEPS THE STOPPED JOB'S SESSION BEFORE ITS BOX GOES: the job never
// reached its own pack, so the session is packed now, in the read that charges
// what it spent, and the box is reclaimed after.
//
// Mutation: take the stop's session as the run's script left it, or after the
// reclaim, and nothing is packed, or there is no box left to pack in.
func TestAStopKeepsTheJobsSessionBeforeItsBoxGoes(t *testing.T) {
	t.Parallel()
	rig, writes, _ := keepingRig(t)
	launched := rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder", Reason: "wrong repo"}); err != nil {
		t.Fatalf("StopSeat: %v", err)
	}
	takes := rig.runner.Takes()
	if len(takes) != 1 || !takes[0].Pack {
		t.Fatalf("takes %+v, want the stopped job's session packed now", takes)
	}
	captures := writes.captured()
	if len(captures) != 1 || captures[0].n.LaunchSeq != launched.LaunchSeq {
		t.Fatalf("recorded %+v, want the stopped launch's capture", captures)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != launched.SandboxID {
		t.Fatalf("killed %v, want the stopped job's box reclaimed after its session was kept", killed)
	}
}
