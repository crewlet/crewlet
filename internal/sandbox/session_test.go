package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// sessionWrites is the executor log's writes as a case plays them, answered as
// the engine answers them ([LogAnswer]): every capture and end asked, in the
// order asked, and what each attempt answers. An END TOMBSTONES ITS TURN, as
// the log's does: a capture of an ended turn is refused, so a case can see an
// end that came before the turn's last capture; and a second end of a turn
// lands as the first, as the log's refusal of an end already made does.
type sessionWrites struct {
	mu       sync.Mutex
	captures []capturedRecord
	ends     []EndRecord
	order    []string
	ended    map[string]bool
	// refuse answers every capture attempt while it is set, as a log that
	// did not answer does.
	refuse error
	// holding, while set, answers no capture attempt until the attempt's own
	// context ends, as a log that holds on to a write does.
	holding bool
}

// Begin answers a turn's first read held wherever it names a base and the turn
// has not ended, as the log does where nothing moved the base before it landed.
// What a begin carries is the conversation cases' to read (conversationLog);
// these cases launch turns that read no conversation.
func (w *sessionWrites) Begin(_ context.Context, b BeginRecord) (BeginAnswer, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended[b.Turn] {
		return BeginAnswer{Log: LogRefused}, fmt.Errorf("the turn has ended: %s", b.Turn)
	}
	return BeginAnswer{Log: LogLanded, Held: b.BaseTurn != ""}, nil
}

type capturedRecord struct {
	CaptureRecord
	err error
	// deadline is the deadline the attempt was sent with, zero for none.
	deadline time.Time
}

// operation is what the capture's operation derives from
// ([SessionWrites.Capture]): two attempts with one operation are one capture.
func (r capturedRecord) operation() string {
	return fmt.Sprint(r.Stamp.Minted.UTC(), "/", r.Handle, "/", r.Turn, "/", r.Stamp.Launch, "/",
		r.Stamp.CaptureSeq)
}

func (w *sessionWrites) Capture(ctx context.Context, c CaptureRecord) (LogAnswer, error) {
	deadline, _ := ctx.Deadline()
	w.mu.Lock()
	w.order = append(w.order, "capture")
	if w.holding {
		at := len(w.captures)
		w.captures = append(w.captures, capturedRecord{CaptureRecord: c, deadline: deadline})
		w.mu.Unlock()
		<-ctx.Done()
		w.mu.Lock()
		w.captures[at].err = ctx.Err()
		w.mu.Unlock()
		return LogUnsettled, ctx.Err()
	}
	defer w.mu.Unlock()
	switch {
	case w.refuse != nil:
		w.captures = append(w.captures, capturedRecord{CaptureRecord: c, err: w.refuse, deadline: deadline})
		return LogUnsettled, w.refuse
	case w.ended[c.Turn]:
		err := fmt.Errorf("the turn has ended: %s", c.Turn)
		w.captures = append(w.captures, capturedRecord{CaptureRecord: c, err: err, deadline: deadline})
		return LogRefused, err
	}
	w.captures = append(w.captures, capturedRecord{CaptureRecord: c, deadline: deadline})
	return LogLanded, nil
}

func (w *sessionWrites) End(_ context.Context, e EndRecord) (LogAnswer, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.order = append(w.order, "end")
	if w.ended == nil {
		w.ended = map[string]bool{}
	}
	if w.ended[e.Turn] {
		return LogLanded, nil
	}
	w.ended[e.Turn] = true
	w.ends = append(w.ends, e)
	return LogLanded, nil
}

func (w *sessionWrites) answer(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.refuse = err
}

// holdOn makes every capture attempt from here on wait for its own context to
// end, answered by nothing.
func (w *sessionWrites) holdOn() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.holding = true
}

func (w *sessionWrites) captured() []capturedRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]capturedRecord(nil), w.captures...)
}

func (w *sessionWrites) endedRecords() []EndRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]EndRecord(nil), w.ends...)
}

func (w *sessionWrites) calls() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.order)
}

// sessionKeep is the engine's part of keeping a session, as a case plays it:
// the handed secrets "sealed" as the turn they belong to beside their bytes, a
// spool of the archive as it came, and an upload that keeps what it read.
type sessionKeep struct {
	mu     sync.Mutex
	policy SessionPolicy
	// now is the rig's clock, which a spool's key is minted on: the
	// coordinator ages the key against that clock ([objstore.RecordWithin]).
	now    func() time.Time
	putErr error
	// sealErr, when set, fails every seal once its archive has streamed into
	// the spool: a spool left half written. refuseSealOf, when set, fails the
	// seal of the one archive whose bytes it holds, the same way, and
	// refusePutOf the upload of the one spool whose bytes it holds: a case's
	// two reads of one job told apart by what each took.
	sealErr                   error
	refuseSealOf, refusePutOf []byte
	// gate, when set, holds every upload until it is closed: a keep that
	// takes its time. mintGate, when set, holds every keep at its first step,
	// before its upload, the same way.
	gate     chan struct{}
	mintGate chan struct{}
	// uploaded is what every upload read.
	uploaded [][]byte
}

func (k *sessionKeep) Policy(context.Context) (SessionPolicy, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.policy, nil
}

func (k *sessionKeep) Keeps() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.policy.Keep
}

func (k *sessionKeep) SealHanded(turnID string, handed []byte) (string, error) {
	return turnID + "|" + string(handed), nil
}

func (k *sessionKeep) OpenHanded(turnID, sealed string) ([]byte, error) {
	body, ok := strings.CutPrefix(sealed, turnID+"|")
	if !ok {
		return nil, errors.New("sealed for another turn")
	}
	return []byte(body), nil
}

func (k *sessionKeep) MintedAt(ctx context.Context, _, _ string, _ time.Time) (time.Time, error) {
	k.mu.Lock()
	gate := k.mintGate
	k.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		}
	}
	return time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC), nil
}

func (k *sessionKeep) Seal(_, _ string, archive io.Reader, dst io.Writer) (executorkeep.SealedSpool, error) {
	var read bytes.Buffer
	n, err := io.Copy(dst, io.TeeReader(archive, &read))
	if err != nil {
		return executorkeep.SealedSpool{}, err
	}
	k.mu.Lock()
	sealErr := k.sealErr
	if k.refuseSealOf != nil && bytes.Equal(read.Bytes(), k.refuseSealOf) {
		sealErr = errors.New("the keyring would not seal this archive")
	}
	k.mu.Unlock()
	if sealErr != nil {
		return executorkeep.SealedSpool{}, sealErr
	}
	return executorkeep.SealedSpool{Key: objstore.KeyAt(k.now()), ContentKey: "k1", Size: n}, nil
}

func (k *sessionKeep) Upload(ctx context.Context, spool executorkeep.SealedSpool,
	sealed io.Reader) (executorkeep.SealedObject, error) {

	k.mu.Lock()
	gate := k.gate
	k.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return executorkeep.SealedObject{}, ctx.Err()
		}
	}
	body, err := io.ReadAll(sealed)
	if err != nil {
		return executorkeep.SealedObject{}, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.putErr != nil {
		return executorkeep.SealedObject{}, k.putErr
	}
	if k.refusePutOf != nil && bytes.Equal(body, k.refusePutOf) {
		return executorkeep.SealedObject{}, errors.New("the object store refused this upload")
	}
	k.uploaded = append(k.uploaded, body)
	sum := sha256.Sum256(body)
	return executorkeep.SealedObject{ContentKey: spool.ContentKey, Object: objstore.Object{
		Key: spool.Key, Hash: objstore.Hash(hex.EncodeToString(sum[:])), Size: int64(len(body)),
	}}, nil
}

func (k *sessionKeep) uploads() [][]byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.uploaded)
}

// handedToken is the secret every keeping rig's launch handed its box.
const handedToken = "the-entrys-own-token"

// keepingRig is a coordinator rig that keeps sessions: its runner's boxes hold
// one, and its records are the case's to read and to answer.
func keepingRig(t *testing.T) (*coordRig, *sessionWrites, *sessionKeep) {
	t.Helper()
	return keepingRigWith(t, nil)
}

// keepingRigWith is [keepingRig] whose coordinator is built with what with sets
// on its options ([newCoordRigWith]).
func keepingRigWith(t *testing.T, with func(*CoordinatorOptions)) (*coordRig, *sessionWrites, *sessionKeep) {
	t.Helper()
	rig := newCoordRigWith(t, with)
	writes := &sessionWrites{}
	keep := &sessionKeep{policy: SessionPolicy{Keep: true, SeatBudget: 64 << 20},
		now: func() time.Time { return rig.now }}
	rig.coordinator.sessions, rig.coordinator.keep = writes, keep
	rig.coordinator.sessionRetry, rig.coordinator.recordBudget = time.Millisecond, 200*time.Millisecond
	rig.coordinator.captureTick = time.Millisecond
	// ITS SPOOLS ITS OWN, so what a case leaves in them is the case's to see.
	rig.coordinator.spoolDir = t.TempDir()
	// THE ENGINE'S ACCOUNTS OF A STOP AND A LOSS, as its hooks answer them.
	rig.coordinator.closeStopped = func(_ context.Context, run PendingRun, _ RunCalls) (executorkeep.Ending, error) {
		return executorkeep.Ending{Outcome: executorkeep.OutcomeStopped, StoppedBy: run.Stop.By}, nil
	}
	rig.coordinator.turnLost = func(context.Context, PendingRun, RunCalls) executorkeep.Ending {
		return executorkeep.Ending{Outcome: executorkeep.OutcomeLost}
	}
	rig.runner.Session = &FakeSession{
		Archive: SessionArchive{Harness: "claude-code", SessionID: "s-1", Format: 1, Version: "2.1.310",
			Size: 14, Path: "/home/user/.crewlet/session-capture"},
		Body: []byte("the transcript"),
	}
	return rig, writes, keep
}

// handOver writes beside turnID's row the secrets its launch handed its box,
// named for the row's job, as the launch's attach does ([PendingStore.Handed]).
func (r *coordRig) handOver(t *testing.T, turnID string, handed HandedSecrets) {
	t.Helper()
	raw, err := handed.encode()
	if err != nil {
		t.Fatalf("encoding the handed secrets: %v", err)
	}
	sealed, err := r.coordinator.keep.SealHanded(turnID, raw)
	if err != nil {
		t.Fatalf("sealing the handed secrets: %v", err)
	}
	run := r.get(turnID)
	r.pending.writeHanded(t.Context(), turnID, handedSet{Launch: run.LaunchID, LaunchSeq: run.LaunchSeq, Sealed: sealed})
	if got := r.handedOf(t, turnID); got != sealed {
		t.Fatalf("the handed secrets beside the row read %q, want the seal written", got)
	}
}

// handedOf reads what turnID's job's box was handed, sealed, as a capture
// reads it ([PendingStore.Handed]).
func (r *coordRig) handedOf(t *testing.T, turnID string) string {
	t.Helper()
	sealed, err := r.pending.Handed(t.Context(), r.get(turnID))
	if err != nil {
		t.Fatalf("reading what the box was handed: %v", err)
	}
	return sealed
}

// launchKept launches turnID's job with what its launch handed its box on the
// row, as a keeping node's launch writes it.
func (r *coordRig) launchKept(t *testing.T, turnID string) PendingRun {
	t.Helper()
	r.launch(turnID)
	r.handOver(t, turnID, HandedSecrets{Values: []string{handedToken}})
	r.coordinator.countRun("swe", StatusRunning)
	return r.get(turnID)
}

// awaitKeeps waits for every keep under way ([Coordinator.keepLater]).
func (r *coordRig) awaitKeeps() { r.coordinator.keeping.Wait() }

// spools are the spools left in the rig's spool directory.
func (r *coordRig) spools(t *testing.T) []string {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(r.coordinator.spoolDirectory(), spoolPrefix+"*"))
	if err != nil {
		t.Fatalf("listing the spools: %v", err)
	}
	return left
}

// collectHeld collects turnID's finished job and leaves its row in place: the
// resume is refused, so the claim goes back with everything the collection
// stamped on it.
func collectHeld(t *testing.T, rig *coordRig, turnID string) {
	t.Helper()
	collectLeaving(t, rig, turnID)
	rig.awaitKeeps()
}

// collectLeaving is [collectHeld] that leaves the collection's keep under way,
// for a case to steer while it uploads.
func collectLeaving(t *testing.T, rig *coordRig, turnID string) {
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

// A NEW ROUND FOLDS WHAT ITS TURN'S EARLIER LAUNCHES CALLED INTO THE LOG'S PRIOR
// CALLS, by name and only where the call succeeded, and says so where calls had
// been elided, in the opening that starts the new round's log afresh; a launch
// that carries a round on keeps its calls where they were.
//
// Mutation: drop the fold from the log's opening, or fold a failed call, and a
// stop of the next round reads an earlier round's delivered reply as never
// made, or a failed post as one that went.
func TestANewRoundFoldsItsTurnsEarlierCallsIntoTheLog(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	ctx := t.Context()
	rig.launch("t1")
	if _, _, err := rig.pending.runs.MutateSandboxRunPart(ctx, "t1", coord.RunLog, func(current coord.Record, found bool) ([]byte, error) {
		if !found {
			return nil, errors.New("the launch opened no log to seed")
		}
		var held BridgeLog
		if err := json.Unmarshal(current.Value, &held); err != nil {
			return nil, fmt.Errorf("decoding the log the launch opened: %w", err)
		}
		held.Calls = []BridgeCall{{Name: "chat_post", Seq: 1}, {Name: "ticket_comment", Failed: true, Seq: 2}}
		held.Elided = 3
		return json.Marshal(held)
	}); err != nil {
		t.Fatalf("seeding the round's calls: %v", err)
	}
	if _, err := rig.pending.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentHandle: "swe", AgentID: sweID,
		Role: "SWE", Harness: "claude-code"}, rigLease); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	logged, err := rig.pending.BridgeLog(ctx, rig.get("t1"))
	if err != nil {
		t.Fatalf("reading the new round's log: %v", err)
	}
	if !slices.Equal(logged.PriorCalls, []string{"chat_post"}) || !logged.PriorCallsElided || len(logged.Calls) != 0 {
		t.Fatalf("the new round's log holds prior %v (elided %v) and calls %v, want the earlier "+
			"round's successful post folded in, and its elision said", logged.PriorCalls,
			logged.PriorCallsElided, logged.Calls)
	}
}

// A CAPTURE'S PLACE ONLY GROWS ON THE ROW, AND IS ITS TAKE'S. A newer capture
// takes the row, the same one moves on only to its outcome and only by the
// take that reserved it — the instant it stamped the place at — nothing older
// or backwards lands, and a capture that landed is the turn's kept one
// wherever it is newer than that — an unresolved capture answered after a newer
// one began included. A take's own stamp asked again, the store's answer to
// the first lost, is answered taken: the place is its own.
//
// Mutation: let an equal place take any state, or any take's stamp, keep only
// the newest attempt, or answer a stamp the row holds already as not taken, and
// a late `begun` takes a landed capture back, another read's refusal settles a
// place it never held, the answered older capture is never kept, or a take
// reads its own reservation as another read's.
func TestACapturesPlaceOnlyGrowsOnTheRow(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	rig.launch("t1")
	ctx := t.Context()
	// TWO TAKES OF THE ONE JOB, each stamping at an instant of its own, as a
	// stop's read and a collection's at once do.
	first := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	second := first.Add(time.Second)
	stamp := func(launch, seq int64, state CaptureState, at time.Time) bool {
		took, err := rig.pending.StampCapture(ctx, "t1", SessionCapture{LaunchSeq: launch, CaptureSeq: seq,
			State: state, Launch: "job", Object: "o", At: at})
		if err != nil {
			t.Fatalf("StampCapture: %v", err)
		}
		return took
	}
	for _, step := range []struct {
		launch, seq int64
		state       CaptureState
		at          time.Time
		take        bool
	}{
		{1, 1, CaptureBegun, first, true},
		{1, 1, CaptureBegun, first, true},      // its own reservation, asked again
		{1, 1, CaptureBegun, second, false},    // another read's, of the same place
		{1, 1, CaptureRefused, second, false},  // and that read's refusal of a place it never held
		{1, 1, CaptureUnresolved, first, true}, // the take's own outcome
		{1, 2, CaptureBegun, second, true},
		{1, 1, CaptureLanded, first, true}, // the older answer lands as the kept one
		{1, 1, CaptureRefused, first, false},
		{1, 2, CaptureRefused, second, true},
		{1, 2, CaptureLanded, second, false},
		{2, 1, CaptureLanded, first, true},
	} {
		if got := stamp(step.launch, step.seq, step.state, step.at); got != step.take {
			t.Fatalf("stamping (%d, %d) %s at %s was taken %v, want %v", step.launch, step.seq, step.state,
				step.at.Format(time.TimeOnly), got, step.take)
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

// A COLLECTION TAKES THE JOB'S SESSION BEFORE IT PAUSES THE BOX AND KEEPS IT
// AFTER: the archive the run's script packed at its harness's exit, scrubbed of
// what the launch handed the box and bounded by the archive's bound, spooled,
// the box paused, and only then uploaded and recorded at the launch's ordinal
// and its own, its digest kept in the box, stamped landed on the row with what
// the job had answered and read through — and the launch's own record says
// what was taken, and which session it ran.
//
// Mutation: take the session after the pause (the fake reads no paused box),
// pack it on demand for a job that packed its own, drop what the box was
// handed from the scrub, stamp before the record lands, leave the record's
// fields off `executor_collected`, or leave the spool behind the keep, and a
// check here goes red.
func TestACollectionTakesTheSessionBeforeThePauseAndKeepsItAfter(t *testing.T) {
	t.Parallel()
	rig, writes, keep := keepingRig(t)
	rig.launchKept(t, "t1")
	session := rig.runner.Session
	rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
		Captured: &session.Archive})
	collectHeld(t, rig, "t1")

	takes := rig.runner.Takes()
	if len(takes) != 1 || takes[0].Pack {
		t.Fatalf("takes %+v, want one take of the archive the run's script packed", takes)
	}
	if !slices.Contains(takes[0].Scrub, handedToken) || takes[0].MaxBytes != executorkeep.SessionMaxBytes {
		t.Errorf("the take scrubs %v within %d bytes, want what the launch handed the box within the "+
			"session's bound", takes[0].Scrub, takes[0].MaxBytes)
	}
	if !rig.provider.Box(rig.get("t1").SandboxID).Paused() {
		t.Error("the collection did not pause the box after taking its session")
	}
	captures := writes.captured()
	if len(captures) != 1 {
		t.Fatalf("recorded %d captures, want 1", len(captures))
	}
	got := captures[0].CaptureRecord
	if stamp := got.Stamp; stamp.LaunchSeq != 1 || stamp.CaptureSeq != 1 || stamp.SessionID != "s-1" ||
		stamp.Harness != "claude-code" || got.Object.Size != int64(len(session.Body)) ||
		stamp.SeatBudget != 64<<20 || stamp.ContentKey != "k1" {
		t.Errorf("the capture recorded %+v", got)
	}
	if uploads := keep.uploads(); len(uploads) != 1 || string(uploads[0]) != "the transcript" {
		t.Errorf("uploaded %q, want the box's archive once", uploads)
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
	if len(recs) != 1 || recs[0].SessionTakenBytes != int64(len(session.Body)) ||
		recs[0].HarnessSession != "s-1" || recs[0].HarnessVersion != "2.1.310" || recs[0].SessionNotKept != "" {
		t.Errorf("the launch's record is %+v, want the session it ran and what was taken of it", recs)
	}
	if left := rig.spools(t); len(left) != 0 {
		t.Errorf("the kept session's spool is left on the node's disk: %v", left)
	}
}

// A JOB TAKEN ONCE IT HAD ENDED IS NOT TAKEN AGAIN, whatever that take came to.
// A collection retried after a resume that failed reads the same finished job,
// whose session nothing has changed: taken again, a landed one would be a
// second upload of the same bytes holding the store's room for a day, and a
// refused one the same refusal paid for twice.
//
// Mutation: drop the check for a final capture of the job, or hold it to a
// capture that landed, and the retry takes and uploads the session again.
func TestAJobTakenOnceItHadEndedIsNotTakenAgain(t *testing.T) {
	t.Parallel()
	for name, putErr := range map[string]error{"landed": nil, "refused": objstore.ErrFull} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig, writes, keep := keepingRig(t)
			keep.putErr = putErr
			rig.launchKept(t, "t1")
			session := rig.runner.Session
			rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
			collectHeld(t, rig, "t1")
			collectHeld(t, rig, "t1")
			if takes := rig.runner.Takes(); len(takes) != 1 {
				t.Fatalf("took the finished job's session %d times, want once", len(takes))
			}
			if putErr == nil && (len(writes.captured()) != 1 || len(keep.uploads()) != 1) {
				t.Fatalf("recorded %d captures and %d uploads of one finished job, want 1 each",
					len(writes.captured()), len(keep.uploads()))
			}
		})
	}
}

// A RECORD THE LOG DOES NOT ANSWER IS ASKED AGAIN UNDER ITS ONE OPERATION — the
// operation its turn, its launch and its place derive — and is left UNRESOLVED
// on the row once its budget is spent; the turn's end asks it once more under
// that same operation before it records the end, and the next launch's capture
// is another operation.
//
// The operation is derived from the record's place by the engine, which states
// the record as the log's (internal/engine's
// TestTheSandboxsRecordsAreTheLogsUnderTheOperationTheirPlaceDerives); here,
// every attempt carries the one place.
//
// IN A SYNCTEST BUBBLE, so the asks again are counted against the rig's record
// budget on the bubble's clock, which only their own waits advance: on the wall
// clock a starved runner could spend the budget inside the first ask.
//
// Mutation: stamp the place anew per attempt, skip the end's ask, or keep the
// last launch's place for the next, and the places below stop matching or
// collide.
func TestARecordIsAskedAgainUnderOneOperationAndByTheTurnsEnd(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig, writes, _ := keepingRig(t)
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		writes.answer(errors.New("estate: the write was sent to a data node that did not answer"))
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
		collectHeld(t, rig, "t1")

		asked := writes.captured()
		if len(asked) < 2 {
			t.Fatalf("the capture was asked %d times, want it asked again", len(asked))
		}
		for _, again := range asked[1:] {
			if again.operation() != asked[0].operation() {
				t.Fatalf("asked again under %s, want the first attempt's %s", again.operation(), asked[0].operation())
			}
		}
		if run := rig.get("t1"); run.Capture == nil || run.Capture.State != CaptureUnresolved || run.Kept != nil {
			t.Fatalf("the row holds %+v, kept %+v, want the capture unresolved and nothing kept",
				run.Capture, run.Kept)
		}

		writes.answer(nil)
		ctx := t.Context()
		if owed, err := rig.coordinator.OweEnd(ctx, "t1", executorkeep.Ending{
			Outcome: executorkeep.OutcomeCompleted}); err != nil || !owed {
			t.Fatalf("OweEnd = %v, %v", owed, err)
		}
		if _, err := rig.coordinator.publishEnd(ctx, rig.get("t1")); err != nil {
			t.Fatalf("publishEnd: %v", err)
		}
		all := writes.captured()
		if last := all[len(all)-1]; last.operation() != asked[0].operation() {
			t.Fatalf("the end asked the capture under %s, want its own %s", last.operation(), asked[0].operation())
		}
		ends := writes.endedRecords()
		if len(ends) != 1 || ends[0].Ending.Outcome != executorkeep.OutcomeCompleted || ends[0].Ending.Unkept != nil {
			t.Fatalf("ends %+v, want the turn's one end after its capture landed, naming nothing unkept", ends)
		}
		if run := rig.get("t1"); run.Kept == nil || run.Kept.State != CaptureLanded || len(run.OwedEnd) != 0 {
			t.Fatalf("the row keeps %+v and owes %s, want the capture the end's ask landed and the end "+
				"cleared", run.Kept, run.OwedEnd)
		}

		// THE NEXT LAUNCH'S CAPTURE IS ANOTHER OPERATION.
		if _, err := rig.pending.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentHandle: "swe",
			AgentID: sweID, Role: "SWE", Harness: "claude-code"}, rigLease); err != nil {
			t.Fatalf("relaunch: %v", err)
		}
		next := capturedRecord{CaptureRecord: CaptureRecord{Handle: "swe", Turn: "t1", Stamp: SessionCapture{
			Minted: asked[0].Stamp.Minted, Launch: rig.get("t1").LaunchID, CaptureSeq: 1}}}
		if next.operation() == asked[0].operation() {
			t.Fatal("the next launch's first capture derives the operation the last launch's did")
		}
	})
}

// AN UPLOAD THE STORE REFUSES IS RECORDED AS REFUSED, with why, and THE TURN'S
// END SAYS SO: the row's newest capture is refused, nothing is recorded on the
// log, and the end the turn owes names that capture's place and why, so the
// executor log does not go on naming an older capture of the turn as its last.
//
// Mutation: leave a refused upload stamped `begun`, record it anyway, or leave
// it off the end, and the row says nothing about it, names bytes the store
// never took, or the next turn continues an older session believing it the
// turn's last.
func TestARefusedUploadIsRecordedAsRefusedAndRidesTheEnd(t *testing.T) {
	t.Parallel()
	rig, writes, keep := keepingRig(t)
	keep.putErr = objstore.ErrFull
	rig.launchKept(t, "t1")
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
	if recs := rig.collected(); len(recs) != 1 || recs[0].SessionTakenBytes == 0 {
		t.Errorf("the launch's record is %+v, want it to say the session was taken to be kept", recs)
	}
	ctx := t.Context()
	if _, err := rig.coordinator.OweEnd(ctx, "t1", executorkeep.Ending{
		Outcome: executorkeep.OutcomeCompleted}); err != nil {
		t.Fatalf("OweEnd: %v", err)
	}
	if _, err := rig.coordinator.publishEnd(ctx, rig.get("t1")); err != nil {
		t.Fatalf("publishEnd: %v", err)
	}
	ends := writes.endedRecords()
	if len(ends) != 1 {
		t.Fatalf("recorded %d ends, want the turn's one", len(ends))
	}
	unkept := ends[0].Ending.Unkept
	if unkept == nil || unkept.LaunchSeq != 1 || unkept.CaptureSeq != 1 ||
		!strings.Contains(unkept.Reason, "reservation is spent") {
		t.Fatalf("the end names %+v unkept, want the launch's refused capture and why", unkept)
	}
}

// THE SWITCH OFF KEEPS NOTHING, SEALS NOTHING, AND STILL ENDS THE TURN — through
// the run's own ending, as every end is recorded. With
// `turn_engine.conversation_session.enabled: false` a launch seals nothing for a
// scrub, a stop's read takes no session and nothing is stamped, while the
// turn's end — the account, not the session — is owed on the row and recorded
// by the stop's ending as it is with the switch on, the row deleted after it.
//
// Mutation: read the switch after the take, seal a launch's secrets with it
// off, or let it gate the end anywhere on the ending's path — its owe, its
// decision, its record — and a session is taken, a seal is written, or the
// turn's end goes unrecorded.
func TestTheSwitchOffKeepsNothingAndStillEndsTheTurn(t *testing.T) {
	t.Parallel()
	rig, writes, keep := keepingRig(t)
	keep.policy.Keep = false
	ctx := t.Context()
	if _, err := rig.coordinator.Launch(ctx, rig.manager, launchReq("t1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	rig.suspend("t1")
	if handed := rig.handedOf(t, "t1"); handed != "" {
		t.Fatalf("the launch sealed %q for a scrub with the switch off", handed)
	}
	rig.runner.Finish(Result{Text: "halfway through"})
	if err := rig.coordinator.StopSeat(ctx, "swe", RunStop{By: "founder"}); err != nil {
		t.Fatalf("StopSeat: %v", err)
	}
	rig.fireRetries()
	rig.awaitKeeps()
	rig.fireRetries()

	if takes := rig.runner.Takes(); len(takes) != 0 {
		t.Fatalf("took %d sessions with the switch off", len(takes))
	}
	if captures := writes.captured(); len(captures) != 0 {
		t.Fatalf("recorded %d captures with the switch off", len(captures))
	}
	ends := writes.endedRecords()
	if len(ends) != 1 || ends[0].Ending.Outcome != executorkeep.OutcomeStopped {
		t.Fatalf("recorded %+v with the switch off, want the stopped turn's one end", ends)
	}
	if _, found, _ := rig.pending.Get(ctx, "t1"); found {
		t.Fatal("the stopped run's record was left behind its end")
	}
}

// A STOP TAKES THE STOPPED JOB'S SESSION OFF THE FRAME THAT DECIDED IT, BEFORE
// ITS BOX GOES: the pause watch decides the stop and hands the rest to the
// ending's retry, whose read ends the job — here one already over — and takes
// the session its script left, final, and reclaims the box after; the capture
// is recorded before the turn's end, which says the turn was stopped.
//
// Mutation: take the stop's session on the frame that decided it, as the read
// once did, packed as it stands, or after the reclaim (the fake reads no
// reclaimed box), and the frame is held for an upload, the job's own end is
// packed over, or there is no box left to read.
func TestAStopTakesItsSessionOffTheFrameAndBeforeTheReclaim(t *testing.T) {
	t.Parallel()
	rig, writes, _ := keepingRig(t)
	launched := rig.launchKept(t, "t1")
	rig.runner.Finish(Result{Text: "halfway through"})
	if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder", Reason: "wrong repo"}); err != nil {
		t.Fatalf("StopSeat: %v", err)
	}
	if takes := rig.runner.Takes(); len(takes) != 0 {
		t.Fatalf("the frame that decided the stop took %d sessions itself", len(takes))
	}
	rig.fireRetries()
	rig.awaitKeeps()
	rig.fireRetries()

	takes := rig.runner.Takes()
	if len(takes) != 1 || takes[0].Pack {
		t.Fatalf("takes %+v, want the stopped job's session taken as its end left it", takes)
	}
	captures := writes.captured()
	if len(captures) != 1 || captures[0].Stamp.LaunchSeq != launched.LaunchSeq || captures[0].err != nil {
		t.Fatalf("recorded %+v, want the stopped launch's capture landed", captures)
	}
	if killed := rig.provider.KilledIDs(); len(killed) != 1 || killed[0] != launched.SandboxID {
		t.Fatalf("killed %v, want the stopped job's box reclaimed after its session was taken", killed)
	}
	if got := writes.calls(); !slices.Equal(got, []string{"capture", "end"}) {
		t.Fatalf("the log was written %v, want the capture before the turn's end", got)
	}
	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Fatal("the stopped run's record was left behind its end")
	}
}

// AN END WAITS FOR ITS TURN'S CAPTURE TO LAND. A stop's take is kept off the read,
// and its upload can take its time; the turn's end tombstones the turn, so an
// end recorded first would refuse the turn's last session as it landed. The
// ending's retry waits for the capture on the row, and records the end after.
//
// Mutation: drop the wait before the end's record, and the capture lands after
// the end and is refused by its tombstone.
func TestAnEndWaitsForItsTurnsCaptureToLand(t *testing.T) {
	t.Parallel()
	rig, writes, keep := keepingRig(t)
	keep.gate = make(chan struct{})
	rig.launchKept(t, "t1")
	rig.runner.Finish(Result{Text: "halfway through"})
	if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder"}); err != nil {
		t.Fatalf("StopSeat: %v", err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(keep.gate)
	}()
	rig.fireRetries()
	rig.awaitKeeps()

	captures := writes.captured()
	if len(captures) != 1 || captures[0].err != nil {
		t.Fatalf("recorded %+v, want the stop's capture landed", captures)
	}
	if got := writes.calls(); !slices.Equal(got, []string{"capture", "end"}) {
		t.Fatalf("the log was written %v, want the end after the capture it waited for", got)
	}
}

// failsAReadOfABegunCapture fails the first read of a run whose ending is
// decided while a capture of its turn is still being kept — the read an ending
// makes as it waits for that capture — and says so on failed.
type failsAReadOfABegunCapture struct {
	PendingStore
	once   sync.Once
	failed chan struct{}
}

func (s *failsAReadOfABegunCapture) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	run, found, err := s.PendingStore.Get(ctx, turnID)
	if err != nil || !found || run.Ending == nil || run.Capture == nil || run.Capture.State != CaptureBegun {
		return run, found, err
	}
	fail := false
	s.once.Do(func() {
		fail = true
		close(s.failed)
	})
	if fail {
		return PendingRun{}, false, errors.New("the coordination store did not answer")
	}
	return run, found, err
}

// A READ THAT FAILS WHILE A CAPTURE IS KEPT IS NO ANSWER. A turn's end waits for
// the turn's capture to land, reading the run's row on a tick, because the end
// tombstones the turn and a capture recorded after it is refused. A read that
// failed ended that wait: the end was recorded over the capture still being
// kept, its tombstone refused the turn's last session, and the end named the
// capture begun and never finished.
//
// Mutation: end the wait on a read that failed ([Coordinator.awaitCapture]), and
// the end comes before the capture, which the log refuses.
func TestAReadThatFailsWhileACaptureIsKeptIsNoAnswer(t *testing.T) {
	t.Parallel()
	rig, writes, keep := keepingRig(t)
	keep.gate = make(chan struct{})
	rig.launchKept(t, "t1")
	rig.runner.Finish(Result{Text: "halfway through"})
	store := &failsAReadOfABegunCapture{PendingStore: rig.coordinator.pending, failed: make(chan struct{})}
	rig.coordinator.pending = store
	if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder"}); err != nil {
		t.Fatalf("StopSeat: %v", err)
	}
	// THE KEEP LANDS once a read has failed while it was under way — or, should
	// none ever fail, after a while, so the case reports that instead of hanging.
	go func() {
		select {
		case <-store.failed:
		case <-time.After(5 * time.Second):
		}
		close(keep.gate)
	}()
	rig.fireRetries()
	rig.awaitKeeps()

	select {
	case <-store.failed:
	default:
		t.Fatal("the premise: no read of the run failed while its capture was kept")
	}
	if got := writes.calls(); !slices.Equal(got, []string{"capture", "end"}) {
		t.Fatalf("the log was written %v, want the end after the capture it waited for", got)
	}
	if captures := writes.captured(); len(captures) != 1 || captures[0].err != nil {
		t.Fatalf("recorded %+v, want the stop's capture landed", captures)
	}
	if ends := writes.endedRecords(); len(ends) != 1 || ends[0].Ending.Unkept != nil {
		t.Fatalf("ended %+v, want the turn's one end naming nothing unkept", ends)
	}
}

// AN END NAMES WHAT ITS OWN ASK OF AN UNRESOLVED CAPTURE SETTLED, even where the
// row cannot be read back after it: the end asks the log again for a capture
// whose record was never answered, and stamps the answer; a read of the row that
// then failed left the end naming a capture that had just landed as one whose
// record was never answered.
//
// Mutation: drop what settleCapture answered where the read after it fails
// ([Coordinator.settleUnresolved]), and the end names the landed capture unkept.
func TestAnEndNamesWhatItsAskOfAnUnresolvedCaptureSettled(t *testing.T) {
	t.Parallel()
	rig, writes, _ := keepingRig(t)
	rig.launchKept(t, "t1")
	session := rig.runner.Session
	writes.answer(errors.New("estate: the write was sent to a data node that did not answer"))
	rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
	collectHeld(t, rig, "t1")
	if run := rig.get("t1"); run.Capture == nil || run.Capture.State != CaptureUnresolved {
		t.Fatalf("the premise: the row holds %+v, want its capture unresolved", run.Capture)
	}
	writes.answer(nil)
	ctx := t.Context()
	if owed, err := rig.coordinator.OweEnd(ctx, "t1", executorkeep.Ending{
		Outcome: executorkeep.OutcomeCompleted}); err != nil || !owed {
		t.Fatalf("OweEnd = %v, %v", owed, err)
	}
	owing := rig.get("t1")
	rig.coordinator.pending = &refusingStore{inner: rig.pending, refuse: []string{"Get"}}
	if _, err := rig.coordinator.publishEnd(ctx, owing); err != nil {
		t.Fatalf("publishEnd: %v", err)
	}
	ends := writes.endedRecords()
	if len(ends) != 1 || ends[0].Ending.Unkept != nil {
		t.Fatalf("ended %+v, want the turn's one end naming nothing unkept: its capture landed", ends)
	}
	if run := rig.get("t1"); run.Kept == nil || run.Kept.State != CaptureLanded {
		t.Fatalf("the row keeps %+v, want the capture the end's ask landed", run.Kept)
	}
}

// A TAKE THAT CANNOT KNOW WHAT ITS BOX WAS HANDED IS REFUSED, never kept
// scrubbed of less: a row that keeps no sealed secrets — a launch that could
// not seal them — or a seal that does not open is a session the engine could
// carry its own credentials out of the box in.
//
// Mutation: scrub with what the node's company says now, or with nothing, where
// the row keeps none, and the session is kept.
func TestATakeThatCannotKnowWhatItsBoxWasHandedIsRefused(t *testing.T) {
	t.Parallel()
	rig, writes, keep := keepingRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	session := rig.runner.Session
	rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
	collectHeld(t, rig, "t1")

	if takes := rig.runner.Takes(); len(takes) != 0 {
		t.Fatalf("took the session %d times with nothing known of what the box was handed", len(takes))
	}
	if len(writes.captured()) != 0 || len(keep.uploads()) != 0 {
		t.Fatal("kept a session nobody could scrub")
	}
	run := rig.get("t1")
	if run.Capture == nil || run.Capture.State != CaptureRefused ||
		!strings.Contains(run.Capture.Reason, "cannot be read back") {
		t.Fatalf("the row holds %+v, want the take refused saying why", run.Capture)
	}
}

// THE SCRUB READS THE BOX'S LOGIN AGAIN WHERE IT IS REFRESHED: in the box, whose
// harness refreshed it while it worked, and on the engine host, whose calls
// refresh the shared copy — each refresh a token the launch never saw.
//
// Mutation: scrub with the login as it was seeded alone, and a refreshed token
// leaves the box in the session.
func TestTheScrubReadsTheLoginAgainWhereItIsRefreshed(t *testing.T) {
	t.Parallel()
	rig, _, _ := keepingRig(t)
	launched := rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	host := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(host, []byte(`{"accessToken":"host-refreshed-token-0001"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.handOver(t, "t1", HandedSecrets{Values: []string{handedToken},
		Logins: map[string]string{".claude/.credentials.json": host}})
	box := rig.provider.Box(launched.SandboxID)
	if err := box.WriteFile(t.Context(), box.Home()+"/.claude/.credentials.json",
		[]byte(`{"accessToken":"box-refreshed-token-0002"}`)); err != nil {
		t.Fatal(err)
	}
	session := rig.runner.Session
	rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
	collectHeld(t, rig, "t1")

	takes := rig.runner.Takes()
	if len(takes) != 1 {
		t.Fatalf("took %d sessions, want one", len(takes))
	}
	for _, want := range []string{handedToken, "host-refreshed-token-0001", "box-refreshed-token-0002"} {
		if !slices.Contains(takes[0].Scrub, want) {
			t.Errorf("the take scrubs %v, which leaves %q in the session", takes[0].Scrub, want)
		}
	}
}

// A LOST RUN'S BOX IS TAKEN BEFORE ITS ENDING RECLAIMS IT: a launch whose engine
// restarted before its turn could park is reaped as its seat is recovered, and
// its box — on this node's host, still there — is read for its session by the
// ending's retry, attached, its job ended, the session its end left taken, and
// the box reclaimed after.
//
// Mutation: drop the loss's take, or make it after the reclaim, and the lost
// job's session is gone with its box.
func TestALostRunsBoxIsTakenBeforeItsEndingReclaimsIt(t *testing.T) {
	t.Parallel()
	rig, writes, _ := keepingRig(t)
	launched := rig.launching("t1")
	rig.handOver(t, "t1", HandedSecrets{Values: []string{handedToken}})
	if err := rig.recoverSeat(t.Context(), "node-b:1", 9); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	rig.fireRetries()
	rig.awaitKeeps()
	rig.fireRetries()

	takes := rig.runner.Takes()
	if len(takes) != 1 || takes[0].Pack {
		t.Fatalf("takes %+v, want the lost job's session taken as its end left it, before its box went", takes)
	}
	if len(writes.captured()) != 1 {
		t.Fatalf("recorded %d captures of the lost job, want 1", len(writes.captured()))
	}
	if killed := rig.provider.KilledIDs(); !slices.Contains(killed, launched.SandboxID) {
		t.Fatalf("killed %v, want the lost job's box reclaimed after", killed)
	}
}

// A LOST RUN'S BOX ON ANOTHER NODE'S HOST IS ASKED OF THAT HOST, which takes the
// session there before the box goes; and the host takes it only where the run's
// row licenses it, a decided loss.
//
// Mutation: skip the ask for a box on another host, or let a host take a box no
// loss is decided on, and the session is lost with its box, or a live job's
// box is read under a forged request.
func TestALostRunsBoxOnAnotherHostIsAskedOfThatHost(t *testing.T) {
	t.Parallel()
	rig, _, _ := keepingRig(t)
	asks := &boxAsks{result: Result{}}
	rig.onNode(t, "node-b", asks, &nudges{})
	run := rig.launchedBy("t1", "node-a")
	rig.handOver(t, "t1", HandedSecrets{Values: []string{handedToken}})
	rig.coordinator.finish(t.Context(), rig.get("t1"), ending{fence: fenceOf(run),
		note: &failureNote{reason: "engine_restarted", detail: "the engine that ran the job restarted"}})
	rig.fireRetries()
	rig.awaitKeeps()
	rig.fireRetries()
	if got := asks.reads(); !slices.Contains(got, BoxReadLoss) {
		t.Fatalf("asked the host %v, want the lost job's box read for its session", got)
	}

	// AND THE HOST REFUSES A LOSS READ NO LOSS LICENSES.
	host, _, _ := keepingRig(t)
	live := host.launchKept(t, "t2")
	if _, _, err := host.coordinator.ReadHostedBox(t.Context(), "t2", live.LaunchID, live.SandboxID,
		BoxReadLoss); !errors.Is(err, ErrBoxNotHosted) {
		t.Fatalf("a loss read of a live job was answered %v, want it refused", err)
	}
}

// A REFUSED RESERVATION UPLOADS NOTHING, AND A REFUSED OUTCOME CLAIMS NOTHING:
// a capture whose place the row would not take keeps no session, and one whose
// landing the row would not take is not read back as landed.
//
// Mutation: go on to the spool when the begun stamp was refused, or read the
// outcome off the record rather than the row, and a session is uploaded that
// no row places, or the row claims a capture it never recorded.
func TestARefusedCaptureStampUploadsAndClaimsNothing(t *testing.T) {
	t.Parallel()
	rig, writes, keep := keepingRig(t)
	rig.launchKept(t, "t1")
	rig.coordinator.pending = &refusingStore{inner: rig.coordinator.pending, refuse: []string{"StampCapture"}}
	session := rig.runner.Session
	rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
	collectHeld(t, rig, "t1")

	if len(keep.uploads()) != 0 || len(writes.captured()) != 0 {
		t.Fatalf("uploaded %d and recorded %d captures whose place the row refused",
			len(keep.uploads()), len(writes.captured()))
	}
	if run := rig.get("t1"); run.Capture != nil || run.Kept != nil {
		t.Fatalf("the row claims %+v kept %+v though every stamp was refused", run.Capture, run.Kept)
	}
}

// A ROW THAT OWES ITS TURN'S END IS NOT DELETED, and the end is decided once:
// the delete refuses until the end is recorded and cleared, a second end
// offered finds the first, and a clear of an end the row does not owe is
// nothing.
//
// Mutation: let Finish delete a row that owes an end, let a second OweEnd
// replace the first, clear whatever the row owes, or clear it without marking
// it recorded, and an end is lost with its row, recorded twice, or cleared
// unrecorded.
func TestARowOwingItsTurnsEndIsKeptUntilTheEndIsRecorded(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	ctx := t.Context()
	rig.launch("t1")
	first, second := []byte(`{"outcome":"completed"}`), []byte(`{"outcome":"failed"}`)
	if owing, wrote, err := rig.pending.OweEnd(ctx, "t1", first); err != nil || !wrote || string(owing.OwedEnd) != string(first) {
		t.Fatalf("OweEnd = %s, %v, %v", owing.OwedEnd, wrote, err)
	}
	if owing, wrote, err := rig.pending.OweEnd(ctx, "t1", second); err != nil || wrote || string(owing.OwedEnd) != string(first) {
		t.Fatalf("a second end was taken (%s, %v, %v), want the first kept", owing.OwedEnd, wrote, err)
	}
	decided, ok, err := rig.pending.DecideEnding(ctx, "t1", Decision{License: License{WhileIn: Active,
		Launch: EveryLaunch}, Reclaim: true})
	if err != nil || !ok {
		t.Fatalf("DecideEnding = %v, %v", ok, err)
	}
	if _, _, err := rig.pending.Finish(ctx, "t1", decided.Ending.ID); !errors.Is(err, ErrEndOwed) {
		t.Fatalf("Finish = %v, want the row kept for the end it owes", err)
	}
	if cleared, err := rig.pending.ClearEnd(ctx, "t1", second); err != nil || cleared {
		t.Fatalf("ClearEnd of an end the row does not owe = %v, %v", cleared, err)
	}
	if cleared, err := rig.pending.ClearEnd(ctx, "t1", first); err != nil || !cleared {
		t.Fatalf("ClearEnd = %v, %v", cleared, err)
	}
	// RECORDED, AND STILL DECIDED ONCE: an end offered after the turn's was
	// recorded and cleared takes nothing, and the record says it is recorded.
	if owing, wrote, err := rig.pending.OweEnd(ctx, "t1", second); err != nil || wrote ||
		len(owing.OwedEnd) != 0 || !owing.EndRecorded {
		t.Fatalf("an end offered after the turn's was recorded = %s (recorded %v), %v, %v",
			owing.OwedEnd, owing.EndRecorded, wrote, err)
	}
	if _, gone, err := rig.pending.Finish(ctx, "t1", decided.Ending.ID); err != nil || !gone {
		t.Fatalf("Finish after the end was cleared = %v, %v", gone, err)
	}
}

// A CAPTURE'S BUDGET IS ASKED BEFORE ITS UPLOAD: a seat whose other live turns
// already hold its budget uploads nothing the log would refuse, and the
// refusal is stamped saying so.
//
// Mutation: drop the budget's check before the upload, and a seat whose live
// turns fill its budget uploads a session on every launch that the log refuses,
// holding the store's room for a day each time.
func TestACaptureTheSeatsBudgetCannotHoldIsNotUploaded(t *testing.T) {
	t.Parallel()
	rig, writes, keep := keepingRig(t)
	keep.policy.SeatBudget = 20
	rig.launchKept(t, "t0")
	if _, err := rig.pending.StampCapture(t.Context(), "t0", SessionCapture{LaunchSeq: 1, CaptureSeq: 1,
		Launch: rig.get("t0").LaunchID, State: CaptureLanded, ObjectSize: 15, Object: "o"}); err != nil {
		t.Fatalf("StampCapture: %v", err)
	}
	rig.launchKept(t, "t1")
	session := rig.runner.Session
	rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
	collectHeld(t, rig, "t1")

	if len(keep.uploads()) != 0 || len(writes.captured()) != 0 {
		t.Fatal("uploaded a session the seat's budget could not hold")
	}
	if run := rig.get("t1"); run.Capture == nil || run.Capture.State != CaptureRefused ||
		!strings.Contains(run.Capture.Reason, "max_bytes_per_seat") {
		t.Fatalf("the row holds %+v, want the capture refused for the seat's budget", run.Capture)
	}
}

// EVERY COLLECTION TAKES WHAT ITS JOB LEFT, AND SAYS WHETHER THE JOB HAD ENDED:
// a job that is over is taken as over, and final — the archive the run's
// script packed at its harness's exit, as it is, and for a job that ended
// without packing (one that failed before it got there) one the runner packs
// now where the script left none ([SessionTake.Pack]). A job its turn's cap
// passed while it still ran is ENDED first, as a stop's is, and so is one
// whose poll cannot say whether it still works, since a poll that cannot say
// is no reason to leave a job working past its cap: each is taken as its end
// left it, and final.
//
// AND ONLY A JOB ITS END COULD NOT STOP — its script heard the request and
// went on, and the kill past the wait could not reach it — is packed now as it
// stands and NOT final, so a collection retried after it takes the job again
// rather than passing over what it did meanwhile.
//
// Mutation: read a job that never packed as one with nothing to take, or as
// one still working, pack a job its cap ended as it stands rather than ending
// it, as the collection once did, take a job whose poll failed as it stands,
// or call the capture of a job its end could not stop final, and a row below
// differs.
func TestEveryCollectionTakesWhatItsJobLeft(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                                   string
		capped, packed, pollFails, unstoppable bool
		wantPack, final                        bool
	}{
		{name: "packed by its script", packed: true, final: true},
		{name: "ended without packing", final: true},
		{name: "ended at its turn's cap while it ran", capped: true, final: true},
		{name: "at its turn's cap, its poll unanswered", capped: true, pollFails: true, final: true},
		{name: "at its turn's cap, its end unable to stop it", capped: true, unstoppable: true, wantPack: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig, writes, _ := keepingRig(t)
			if tc.capped {
				launched := rig.launchUntil("t1", rig.now.Add(-time.Minute))
				rig.handOver(t, "t1", HandedSecrets{Values: []string{handedToken}})
				rig.coordinator.countRun("swe", StatusRunning)
				if tc.pollFails {
					// A POLL THAT CANNOT SAY is no reason to leave the job
					// working past its cap: it is asked to end all the same.
					rig.runner.PollErr = errors.New("the box's exec did not answer")
				}
				if tc.unstoppable {
					// A SCRIPT THAT HEARS THE REQUEST AND GOES ON, and a kill
					// past the wait that reaches nothing of the job.
					rig.coordinator.endWait = 50 * time.Millisecond
					rig.runner.EndFunc = func(context.Context, Sandbox, RunHandle) (EndAsk, error) {
						return EndAsked, nil
					}
					rig.provider.Box(launched.SandboxID).KillJobFunc = func(context.Context, string) error {
						return ErrNoJobGroup
					}
				}
				// PAST ITS TURN'S CAP a resume that fails after the collection
				// is the turn's last, and the run is ended rather than handed
				// back: its row stays, its ending decided, until this node's
				// retry finishes it — which this case never fires.
				rig.resumer.failWith(errors.New("held for the case to read the row"))
				payload, ev := rig.completion("t1")
				if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
					t.Logf("OnCompleted: %v", err)
				}
				rig.awaitKeeps()
			} else {
				rig.launchKept(t, "t1")
				res := Result{Success: true, SessionID: "s-1"}
				if tc.packed {
					res.Captured = &rig.runner.Session.Archive
				}
				rig.runner.Finish(res)
				collectHeld(t, rig, "t1")
			}
			takes := rig.runner.Takes()
			if len(takes) != 1 || takes[0].Pack != tc.wantPack {
				t.Fatalf("takes %+v, want one, packed now: %v", takes, tc.wantPack)
			}
			if captures := writes.captured(); len(captures) != 1 || captures[0].err != nil {
				t.Fatalf("recorded %+v, want the one capture", captures)
			}
			if kept := rig.get("t1").Kept; kept == nil || kept.State != CaptureLanded || kept.Final != tc.final {
				t.Fatalf("the row keeps %+v, want a landed capture, final: %v", kept, tc.final)
			}
		})
	}
}
