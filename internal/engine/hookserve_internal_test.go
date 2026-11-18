package engine

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// takeToken is the take a hook's answer names.
var takeToken = regexp.MustCompile(`\[crewlet take ([0-9a-f-]{36})\]`)

// takeOf is the take answer names, failing the case where it names none.
func takeOf(t *testing.T, answer string) string {
	t.Helper()
	m := takeToken.FindStringSubmatch(answer)
	if m == nil {
		t.Fatalf("the hook was answered %q, which names no take", answer)
	}
	return m[1]
}

// noteOn is note id on turnID's record.
func noteOn(t *testing.T, store sandbox.PendingStore, turnID, id string) sandbox.RunNote {
	t.Helper()
	for _, n := range mustRow(t, store, turnID).Steer.Notes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("the record holds no note %s", id)
	return sandbox.RunNote{}
}

// servedNotRead fails the case unless note id on turnID's record waits, served
// to job launch's hook by take: a serve, and never a read.
func servedNotRead(t *testing.T, store sandbox.PendingStore, turnID, id, launch, take string) {
	t.Helper()
	n := noteOn(t, store, turnID, id)
	live, ok := n.LiveServe()
	if n.Read != nil || !ok || live.Launch != launch || (take != "" && live.Take != take) {
		t.Errorf("note %s holds read %+v and serves %+v, want it waiting, served to job %s by its take", id,
			n.Read, n.Serves, launch)
	}
}

// lostServeStore is a run store whose hooks' serves land and answer an error
// while lose holds, as a serve does whose compare-and-swap the store answered
// after its caller's deadline; its hook settles wait while held is open, and
// fail as many times as failSettles says.
type lostServeStore struct {
	sandbox.PendingStore
	mu          sync.Mutex
	lose        int
	failSettles int
	held        chan struct{}
	settles     []sandbox.HookSettle
}

func (s *lostServeStore) ServeHook(ctx context.Context, turnID string, serve sandbox.HookServe) (
	[]sandbox.RunNote, error) {
	served, err := s.PendingStore.ServeHook(ctx, turnID, serve)
	s.mu.Lock()
	losing := s.lose > 0
	if losing {
		s.lose--
	}
	s.mu.Unlock()
	if err != nil || !losing {
		return served, err
	}
	return nil, context.DeadlineExceeded
}

func (s *lostServeStore) SettleHook(ctx context.Context, turnID string, settle sandbox.HookSettle) (
	[]sandbox.RunNote, error) {
	s.mu.Lock()
	s.settles = append(s.settles, settle)
	failing, held := s.failSettles > 0, s.held
	if failing {
		s.failSettles--
	}
	s.mu.Unlock()
	if held != nil {
		select {
		case <-held:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if failing {
		return nil, context.DeadlineExceeded
	}
	return s.PendingStore.SettleHook(ctx, turnID, settle)
}

// asked is every hook settle the store was asked.
func (s *lostServeStore) asked() []sandbox.HookSettle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.settles)
}

// A HOOK'S TAKE SERVES, AND PUBLISHES NOTHING. The take writes the notes it
// answers SERVED to its job, under a fresh take its answer names, before it
// answers; it records no read, since the harness may give the answer up or
// never be handed it, and publishes nothing: the note is read, and published
// delivered, only once the job's session shows the hook took it. A later take
// of the same job hands the note over no more while its serve is live.
//
// Mutation: record a read at the take (today's rule), publish it delivered
// there, answer no take, or offer a live-served note again, and a check goes
// red.
func TestAHooksTakeServesAndPublishesNothing(t *testing.T) {
	t.Parallel()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, published := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, inbox, 1)
	answer := inbox.Take(t.Context(), mcpbridge.SteerEventToolBatch)
	if !strings.Contains(answer, "use staging") {
		t.Fatalf("the hook was answered %q, want the note", answer)
	}
	take := takeOf(t, answer)
	servedNotRead(t, store, "run-1", "n-1", launch, take)
	if live, _ := noteOn(t, store, "run-1", "n-1").LiveServe(); live.Boundary != string(types.SteerBoundaryToolBatch) ||
		live.Iteration != 1 {
		t.Errorf("the serve is %+v, want it at a tool batch of the job's first iteration", live)
	}
	if again := inbox.Take(t.Context(), mcpbridge.SteerEventFinish); again != "" {
		t.Errorf("the run's finish was handed the live-served note again: %q", again)
	}
	// THE RECORD'S END IS THE SIGNAL: an ending reports the note unconfirmed at
	// its serve, synchronously, and nothing before it published a thing.
	if got := steered(published); len(got) != 0 {
		t.Errorf("the take published %+v, want nothing", got)
	}
	run := mustRow(t, store, "run-1")
	run.Ending = &sandbox.RecordedEnding{ID: "end-1"}
	if err := runs.ended(t.Context(), run); err != nil {
		t.Fatalf("ended: %v", err)
	}
	if got := steered(published); len(got) != 1 || got[0].Outcome != types.SteerUnconfirmed ||
		got[0].Boundary != types.SteerBoundaryToolBatch || got[0].LaunchID != launch {
		t.Errorf("the turn's notes were recorded %+v, want the note unconfirmed at the batch that served it",
			got)
	}
}

// A LIVE-SERVED NOTE TAKES NO ROOM IN THE NEXT ANSWER. The store serves a note
// to a job once while its serve is live, so a take that fitted its answer
// around that note would hand the harness less than fits: the take passes it
// over before it measures the answer, and the notes after it are served.
//
// Mutation: offer the live-served note to the take, and the newest note,
// which fits beside the one after it, waits for a later boundary.
func TestALiveServedNoteTakesNoRoomInTheNextAnswer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	inbox := boundInbox(t, runs, store, "run-1", launch)
	text := strings.Repeat("\U0001F680", steer.MaxNoteRunes)
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: text, By: "founder"})
	takeOf(t, inbox.Take(ctx, mcpbridge.SteerEventFinish))
	for _, id := range []string{"n-2", "n-3"} {
		fileSealed(t, runs, "run-1", steer.Note{ID: id, Text: text, By: "founder"})
	}
	take := takeOf(t, inbox.Take(ctx, mcpbridge.SteerEventFinish))
	for _, id := range []string{"n-2", "n-3"} {
		servedNotRead(t, store, "run-1", id, launch, take)
	}
}

// A SERVE WHOSE WRITE FAILED IS REMOVED OFF THE PATH. A serve the store
// recorded after the hook's five seconds ran out (its write landed, its answer
// an error) answers the hook nothing, so no session holds the note from it:
// the inbox keeps its take unanswered and hands the run's settle loop a settle
// of it, asked again while the store does not answer, which removes the serve.
// The note waits again, served to nobody, and the review reads it; nothing
// publishes it delivered.
//
// Mutation: skip the settle, or give it up on its first failure, and the serve
// stays live, so the job's next boundary passes the note over.
func TestAServeWhoseWriteFailedIsRemovedOffThePath(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	lost := &lostServeStore{PendingStore: store, lose: 1, failSettles: 1}
	runs, published := steersOver(t, steerRing, lost)
	runs.stopped = ctx.Done()
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, inbox, 1)
	if got := inbox.Take(ctx, mcpbridge.SteerEventToolBatch); got != "" {
		t.Fatalf("a serve whose write answered an error answered the hook %q, want nothing", got)
	}
	served := noteOn(t, store, "run-1", "n-1").Serves
	if len(served) != 1 {
		t.Fatalf("the premise: the lost serve's write did not land: %+v", served)
	}
	eventually(t, "the lost serve removed", func() bool {
		n := noteOn(t, store, "run-1", "n-1")
		return n.Waiting() && n.Served == "" && len(n.Serves) == 0
	})
	for _, settle := range lost.asked() {
		if settle.Launch != launch || !slices.Equal(settle.Unanswered, []string{served[0].Take}) {
			t.Errorf("the loop asked %+v, want the job's lost take settled unanswered", settle)
		}
	}
	if taken := claimedSource(t, store, runs, launch).Take(ctx, reviewRound(1)); len(taken) != 1 ||
		taken[0].ID != "n-1" {
		t.Errorf("the review took %+v, want the note", taken)
	}
	for _, rec := range steered(published) {
		if rec.NoteID == "n-1" && rec.Phase != types.PhaseReview {
			t.Errorf("the note nobody's hook was handed is recorded %+v", rec)
		}
	}
}

// AN UNANSWERED SERVE IS SERVED AGAIN AT THE NEXT BOUNDARY. A serve whose write
// answered an error and landed is live on the record, but its hook was handed
// nothing: the job's next take serves the note again, removing that serve in
// its own write, whatever the run's settle loop has managed (held here), and
// answers it under a take of its own.
//
// Mutation: keep no unanswered take in the inbox, or write the serve without
// it (ServeHook ignoring Unanswered), and the next take passes the note over
// for the serve its harness never had.
func TestAnUnansweredServeIsServedAgainAtTheNextBoundary(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	lost := &lostServeStore{PendingStore: store, lose: 1, held: make(chan struct{})}
	runs, _ := steersOver(t, steerRing, lost)
	runs.stopped = ctx.Done()
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	if got := inbox.Take(ctx, mcpbridge.SteerEventFinish); got != "" {
		t.Fatalf("a serve whose write answered an error answered the hook %q, want nothing", got)
	}
	first := noteOn(t, store, "run-1", "n-1").Serves
	answer := inbox.Take(ctx, mcpbridge.SteerEventFinish)
	if !strings.Contains(answer, "use staging") {
		t.Fatalf("the next boundary was answered %q, want the note served again", answer)
	}
	again := takeOf(t, answer)
	if len(first) != 1 || again == first[0].Take {
		t.Fatalf("the note was served again under %s after %+v, want a take of its own", again, first)
	}
	servedNotRead(t, store, "run-1", "n-1", launch, again)
	if serves := noteOn(t, store, "run-1", "n-1").Serves; len(serves) != 1 {
		t.Errorf("the note holds serves %+v, want the unanswered one removed by the serve's own write", serves)
	}
	close(lost.held)
}

// A LOST STOP ANSWER WAITS FOR THE REVIEW. The finish is the run's last
// boundary, so a note whose serve there was answered nothing has no next hook
// in its job: it waits, its serve removed off the path where the store
// answers and left live where it does not, and the review's first round reads
// it either way.
//
// Mutation: have the review pass over a served note, and the held case loses
// it.
func TestALostStopAnswerWaitsForTheReview(t *testing.T) {
	t.Parallel()
	for name, held := range map[string]bool{"its settle answered": false, "its settle held": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store := sandbox.NewCoordStore(coordmem.NewFleet())
			lost := &lostServeStore{PendingStore: store, lose: 1}
			if held {
				lost.held = make(chan struct{})
				t.Cleanup(func() { close(lost.held) })
			}
			runs, _ := steersOver(t, steerRing, lost)
			runs.stopped = ctx.Done()
			launch := workingRun(t, store, runs, "run-1")
			fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
			if got := boundInbox(t, runs, store, "run-1", launch).Take(ctx, mcpbridge.SteerEventFinish); got != "" {
				t.Fatalf("a lost Stop answer answered the hook %q", got)
			}
			if !held {
				eventually(t, "the lost serve removed", func() bool {
					return len(noteOn(t, store, "run-1", "n-1").Serves) == 0
				})
			}
			taken := claimedSource(t, store, runs, launch).Take(ctx, reviewRound(1))
			if len(taken) != 1 || taken[0].ID != "n-1" {
				t.Errorf("the review's first round took %+v, want the note", taken)
			}
			if read := readOn(ctx, store, "run-1", "n-1"); read == nil || read.Phase != string(types.PhaseReview) {
				t.Errorf("the note is read %+v, want it read by the review", read)
			}
		})
	}
}

// A NOTE IS SERVED TO A JOB AT MOST THREE TIMES. A harness that loses every
// answer (each serve its session proves absent) is served the note again at
// each boundary, three serves in all; past them the take passes it over, says
// so once at WARN (steer_serve_bound), and the note waits for the review,
// which reads it. NOT PARALLEL: it reads the process's log.
//
// Mutation: ignore the bound, count it off by one, or say it at every take,
// and a check goes red.
func TestANoteIsServedToAJobAtMostThreeTimes(t *testing.T) {
	logs := captureSteerLogs(t)
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	for k := range sandbox.MaxNoteServes {
		answer := inbox.Take(ctx, mcpbridge.SteerEventFinish)
		if !strings.Contains(answer, "use staging") {
			t.Fatalf("serve %d answered %q, want the note", k+1, answer)
		}
		if _, err := store.SettleHook(ctx, "run-1", sandbox.HookSettle{Launch: launch,
			Absent: []string{takeOf(t, answer)}}); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if got := inbox.Take(ctx, mcpbridge.SteerEventFinish); got != "" {
			t.Errorf("a note at its bound was served again: %q", got)
		}
	}
	if serves := noteOn(t, store, "run-1", "n-1").Serves; len(serves) != sandbox.MaxNoteServes {
		t.Errorf("the note holds %d serves, want %d", len(serves), sandbox.MaxNoteServes)
	}
	if said := strings.Count(logs.String(), `"steer_serve_bound"`); said != 1 {
		t.Errorf("steer_serve_bound was said %d times, want once:\n%s", said, logs.String())
	}
	if taken := claimedSource(t, store, runs, launch).Take(ctx, reviewRound(1)); len(taken) != 1 ||
		taken[0].ID != "n-1" {
		t.Errorf("the review took %+v, want the note past its bound", taken)
	}
}

// A NOTE SERVED AFTER A LATER ONE SAYS IT WAS SENT BEFORE. A note whose serve
// was lost is served again after a correction a person filed later has already
// reached the job: the harness would take whichever arrived last as the later
// word, so the older note says it was sent before a note already handed over,
// which stands where the two disagree. The review's take says the same of a
// note it reads at a place before a read its conversation holds.
//
// Mutation: render every note as SteerMessage, judge it over a note served to
// another job, or hand the review's note no flag, and a check goes red.
func TestANoteServedAfterALaterOneSaysItWasSentBefore(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, _ := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	inbox := boundInbox(t, runs, store, "run-1", launch)
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	first := inbox.Take(ctx, mcpbridge.SteerEventFinish)
	if strings.Contains(first, "before a later note") {
		t.Errorf("a note served first says it was sent before another: %q", first)
	}
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-2", Text: "no, use production", By: "founder"})
	second := inbox.Take(ctx, mcpbridge.SteerEventFinish)
	if !strings.Contains(second, "use production") || strings.Contains(second, "before a later note") {
		t.Fatalf("the correction was answered %q, want it plainly", second)
	}
	if _, err := store.SettleHook(ctx, "run-1", sandbox.HookSettle{Launch: launch,
		Absent: []string{takeOf(t, first)}, Held: []string{takeOf(t, second)}}); err != nil {
		t.Fatal(err)
	}
	again := inbox.Take(ctx, mcpbridge.SteerEventFinish)
	if !strings.Contains(again, "use staging") || !strings.Contains(again, "before a later note already handed") {
		t.Errorf("the older note served after the correction was answered %q, want it said sent before", again)
	}
	if _, err := store.SettleHook(ctx, "run-1", sandbox.HookSettle{Launch: launch,
		Absent: []string{takeOf(t, again)}}); err != nil {
		t.Fatal(err)
	}
	// THE REVIEW reads the older note at its first serve's place, before the
	// correction's read its conversation opens with.
	taken := claimedSource(t, store, runs, launch).Take(ctx, reviewRound(1))
	if len(taken) != 1 || taken[0].ID != "n-1" || !taken[0].SentBefore {
		t.Errorf("the review took %+v, want the older note said sent before", taken)
	}
}

// A COLLECTION DENIES ONLY ON A THREAD THAT PROVES ABSENCE. A serve the job's
// session proves it never took is lost, so the note waits unserved: the job's
// next boundary is served it, the review reads it, a continuation is told it
// plainly rather than as a note it may already show, and an ending that finds
// it unread says it was missed at the serve.
//
// Mutation: tell a continuation a note whose every serve is absent under the
// header, or end it as unconfirmed, and a check goes red.
func TestACollectionDeniesOnlyOnAThreadThatProvesAbsence(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, published := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	take := takeOf(t, inbox.Take(ctx, mcpbridge.SteerEventFinish))
	if _, err := store.SettleHook(ctx, "run-1", sandbox.HookSettle{Launch: launch,
		Absent: []string{take}}); err != nil {
		t.Fatal(err)
	}
	run := mustRow(t, store, "run-1")
	if served := servedNotes(run); len(served) != 0 {
		t.Errorf("a continuation is told %v under the header, want the absent note told plainly", served)
	}
	run.Ending = &sandbox.RecordedEnding{ID: "end-1"}
	if err := runs.ended(ctx, run); err != nil {
		t.Fatalf("ended: %v", err)
	}
	if got := steered(published); len(got) != 1 || got[0].Outcome != types.SteerMissed ||
		got[0].Boundary != types.SteerBoundaryFinish || got[0].LaunchID != launch {
		t.Errorf("the ending recorded %+v, want the note missed at the serve its session lacks", got)
	}
	if again := inbox.Take(ctx, mcpbridge.SteerEventFinish); !strings.Contains(again, "use staging") {
		t.Errorf("the job's next boundary was answered %q, want the note served again", again)
	}
}

// AN UNREAD SESSION LEAVES ITS SERVES UNCONFIRMED. A collection that could not
// read the job's session decides nothing: the serve stays live, so the job is
// served the note no more, a continuation is told it as a note it may already
// show, the review reads it, and an ending that finds it unread says it was
// unconfirmed at the hook's boundary.
//
// Mutation: read a serve no session showed as held, or tell a continuation it
// plainly, and a check goes red.
func TestAnUnreadSessionLeavesItsServesUnconfirmed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := sandbox.NewCoordStore(coordmem.NewFleet())
	runs, published := steersOver(t, steerRing, store)
	launch := workingRun(t, store, runs, "run-1")
	fileSealed(t, runs, "run-1", steer.Note{ID: "n-1", Text: "use staging", By: "founder"})
	inbox := boundInbox(t, runs, store, "run-1", launch)
	viewHolds(t, inbox, 1)
	take := takeOf(t, inbox.Take(ctx, mcpbridge.SteerEventToolBatch))
	if got := inbox.Take(ctx, mcpbridge.SteerEventFinish); got != "" {
		t.Errorf("the job was served the note again while its serve stands: %q", got)
	}
	servedNotRead(t, store, "run-1", "n-1", launch, take)
	run := mustRow(t, store, "run-1")
	if served := servedNotes(run); !slices.Equal(served, []string{"n-1"}) {
		t.Errorf("a continuation is told %v under the header, want the served note", served)
	}
	ending := run
	ending.Ending = &sandbox.RecordedEnding{ID: "end-1"}
	if err := runs.ended(ctx, ending); err != nil {
		t.Fatalf("ended: %v", err)
	}
	if got := steered(published); len(got) != 1 || got[0].Outcome != types.SteerUnconfirmed ||
		got[0].Boundary != types.SteerBoundaryToolBatch {
		t.Errorf("the ending recorded %+v, want the note unconfirmed at the hook's batch", got)
	}
	if taken := claimedSource(t, store, runs, launch).Take(ctx, reviewRound(1)); len(taken) != 1 ||
		taken[0].ID != "n-1" {
		t.Errorf("the review took %+v, want the served note", taken)
	}
}
