package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
)

// receiptThread is a [FakeRunner.MainThread] that answers receipts, and a
// count where one is asked, and records what it was asked.
type receiptThread struct {
	mu       sync.Mutex
	receipts TakeReceipts
	asked    []MainThreadAsk
}

func (r *receiptThread) read(_ context.Context, _ Sandbox, ask MainThreadAsk) MainThreadReading {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, ask)
	reading := MainThreadReading{Receipts: r.receipts}
	if ask.Count {
		reading.Count = &ResumedCount{Tokens: 1, Restart: new("")}
	}
	return reading
}

func (r *receiptThread) asks() []MainThreadAsk {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.asked)
}

// sessionShows is a thread whose session shows each named take taken.
func sessionShows(takes ...string) *receiptThread {
	return &receiptThread{receipts: TakeReceipts{Held: takes}}
}

// confirmedSpy records every note a coordinator publishes as confirmed
// ([CoordinatorOptions.SteersConfirmed]).
type confirmedSpy struct {
	mu        sync.Mutex
	published []string
}

func (s *confirmedSpy) confirmed(_ context.Context, _ PendingRun, notes []RunNote) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range notes {
		s.published = append(s.published, n.ID)
	}
}

func (s *confirmedSpy) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.published)
}

// watchConfirmed installs a confirmedSpy on coordinator.
func watchConfirmed(coordinator *Coordinator) *confirmedSpy {
	spy := &confirmedSpy{}
	coordinator.steersConfirmed = spy.confirmed
	return spy
}

// verdictStore is a run store whose verdicts on a job's hooks' serves
// ([PendingStore.SettleHook]) go unanswered while silent and for the first
// refusals, land with their answer lost while losing, and answer only after
// slow; RowCrossing is crossing where set. Every other call is the store
// beneath it.
type verdictStore struct {
	PendingStore
	mu       sync.Mutex
	silent   bool
	losing   bool
	refusals int
	slow     time.Duration
	crossing time.Duration
}

func (s *verdictStore) SettleHook(ctx context.Context, turnID string, settle HookSettle) ([]RunNote, error) {
	s.mu.Lock()
	silent, losing, slow := s.silent, s.losing, s.slow
	if s.refusals > 0 {
		s.refusals--
		silent = true
	}
	s.mu.Unlock()
	if slow > 0 {
		wait := time.NewTimer(slow) //fixedwait:simulated a store slow to answer the verdict, on the case's bubble clock
		select {
		case <-ctx.Done():
			wait.Stop()
			return nil, fmt.Errorf("sandbox: settle run %s's hooks' serves: %w", turnID, ctx.Err())
		case <-wait.C:
		}
	}
	if silent {
		return nil, fmt.Errorf("sandbox: settle run %s's hooks' serves: %w", turnID, errStoreUnanswered)
	}
	answered, err := s.PendingStore.SettleHook(ctx, turnID, settle)
	if err == nil && losing {
		return nil, fmt.Errorf("sandbox: settle run %s's hooks' serves: %w", turnID, errStoreUnanswered)
	}
	return answered, err
}

func (s *verdictStore) RowCrossing() time.Duration {
	if s.crossing > 0 {
		return s.crossing
	}
	return s.PendingStore.RowCrossing()
}

// answer has every later verdict answered.
func (s *verdictStore) answer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silent, s.losing = false, false
}

// silentVerdicts puts a verdictStore under coordinator, silent, and gives its
// verdicts a short room, so a case's handed-back read returns at once.
func silentVerdicts(coordinator *Coordinator) *verdictStore {
	store := &verdictStore{PendingStore: coordinator.pending, silent: true}
	coordinator.pending = store
	coordinator.verdictWait, coordinator.verdictRetry = 20*time.Millisecond, 5*time.Millisecond
	return store
}

// serveNote files note id on t1 and serves it to job by a hook at boundary,
// its take "take-"+id.
func serveNote(t *testing.T, s PendingStore, job, id string, boundary types.SteerBoundary) {
	t.Helper()
	fileNote(t, s, "t1", id)
	served, err := s.ServeHook(t.Context(), "t1", HookServe{IDs: []string{id}, Launch: job,
		Statuses: []string{StatusLaunching, StatusRunning},
		Serve:    HookServed{Take: "take-" + id, Boundary: string(boundary)}})
	if err != nil || len(served) != 1 {
		t.Fatalf("serving note %s served %d (%v), want it", id, len(served), err)
	}
}

// readOf is note id's read on t1's row, nil where it waits.
func readOf(t *testing.T, rig *coordRig, id string) *NoteRead {
	t.Helper()
	for _, n := range rig.get("t1").Steer.Notes {
		if n.ID == id {
			return n.Read
		}
	}
	t.Fatalf("t1 holds no note %s", id)
	return nil
}

// A COLLECTION SETTLES ITS JOB'S SERVES BEFORE IT STAMPS ITS CAPTURE: the take's
// cursor stops before a note a hook was served and its session has not
// confirmed, so the verdict the session gives lands first, and the capture
// counts the note it confirmed.
//
// Mutation: settle after the take, and the cursor stops before the note.
func TestACollectionSettlesItsJobsServesBeforeItStampsItsCapture(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		launched := rig.launchKept(t, "t1")
		serveNote(t, rig.pending, launched.LaunchID, "n1", types.SteerBoundaryToolBatch)
		rig.runner.MainThread = sessionShows("take-n1").read
		session := rig.runner.Session
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
		if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadCollect); err != nil {
			t.Fatalf("readBox: %v", err)
		}
		rig.awaitKeeps()
		read := readOf(t, rig, "n1")
		if read == nil || read.Take != "take-n1" {
			t.Fatalf("the note reads %+v, want it read off its serve", read)
		}
		if capture := rig.get("t1").Capture; capture == nil || capture.NoteCursor != read.Order {
			t.Fatalf("the capture is stamped %+v, want its cursor at the confirmed note's place %d", capture,
				read.Order)
		}
	})
}

// A COLLECTION READS RECEIPTS ONLY WHERE ITS JOB HAS SERVED NOTES: the takes asked
// are the job's own serves of a note or item still unread, each once, live or
// lost to a cancel, at the event its boundary answers; and a stop's or a loss's
// read asks the thread nothing where none is held. Beside them, the job's
// serves in order carry its read notes' in their places, and none where no
// note is read.
//
// Mutation: ask a read note's serve, an absent one, another job's or another
// job's item's, ask a stop with no serve, leave a read note out of the served
// order, or ask the thread without it, and a case goes red.
func TestACollectionReadsReceiptsOnlyWhereItsJobHasServedNotes(t *testing.T) {
	t.Parallel()
	const job = "job-1"
	serve := func(take, launch string, boundary types.SteerBoundary, lost ServeLoss) []HookServed {
		return []HookServed{{Take: take, Launch: launch, Boundary: string(boundary), Lost: lost}}
	}
	batch, finish := types.SteerBoundaryToolBatch, types.SteerBoundaryFinish
	run := PendingRun{TurnID: "t1", LaunchID: job, Harness: "claude-code"}
	run.Steer.Notes = []RunNote{
		{ID: "batch", Serves: serve("take-batch", job, batch, "")},
		{ID: "finish", Serves: serve("take-finish", job, finish, "")},
		{ID: "cancelled", Serves: serve("take-cancelled", job, batch, ServeCancelled)},
		{ID: "absent", Serves: serve("take-absent", job, batch, ServeAbsent)},
		{ID: "read", Read: &NoteRead{Launch: job, Take: "take-read"}, Serves: serve("take-read", job, batch, "")},
		{ID: "another job's", Serves: serve("take-other", "job-0", batch, "")},
		{ID: "item", For: job, Serves: serve("take-item", job, batch, "")},
		{ID: "another job's item", For: "job-0", Serves: serve("take-elsewhere", job, batch, "")},
		{ID: "the same take", Serves: serve("take-batch", job, batch, "")},
		{ID: "a brief's", Serves: serve("take-brief", job, types.SteerBoundaryBrief, "")},
	}
	want := []AskedTake{{Take: "take-batch", Boundary: batch}, {Take: "take-finish", Boundary: finish},
		{Take: "take-cancelled", Boundary: batch}, {Take: "take-item", Boundary: batch}}
	if got := askedTakes(run); !slices.Equal(got, want) {
		t.Errorf("the read asks %+v, want %+v", got, want)
	}
	served := slices.Insert(slices.Clone(want), 3, AskedTake{Take: "take-read", Boundary: batch, Read: true})
	if got := servedTakes(run); !slices.Equal(got, served) {
		t.Errorf("the job's serves in order are %+v, want %+v", got, served)
	}
	unread := run
	unread.Steer.Notes = slices.DeleteFunc(slices.Clone(run.Steer.Notes), func(n RunNote) bool { return n.Read != nil })
	if got := servedTakes(unread); got != nil {
		t.Errorf("a job with no read note names its serves %+v, want none beside its takes", got)
	}

	rig := newCoordRig(t)
	launched := rig.launch("t1")
	thread := sessionShows()
	rig.runner.MainThread = thread.read
	for _, read := range []BoxRead{BoxReadStop, BoxReadLoss} {
		rig.coordinator.readMainThread(t.Context(), rig.get("t1"), nil, rig.runner, read, capturePoint{}, Result{}, summaryAsk{})
	}
	if asked := thread.asks(); len(asked) != 0 {
		t.Errorf("a stop and a loss with no serve asked the thread %+v, want nothing", asked)
	}
	rig.coordinator.readMainThread(t.Context(), rig.get("t1"), nil, rig.runner, BoxReadCollect,
		capturePoint{final: true}, Result{}, summaryAsk{})
	if asked := thread.asks(); len(asked) != 1 || !asked[0].Count || len(asked[0].Takes) != 0 {
		t.Errorf("a collection with no serve asked %+v, want its count alone", asked)
	}
	serveNote(t, rig.pending, launched.LaunchID, "n1", finish)
	rig.coordinator.readMainThread(t.Context(), rig.get("t1"), nil, rig.runner, BoxReadStop,
		capturePoint{cutOff: true}, Result{}, summaryAsk{})
	asked := thread.asks()
	if len(asked) != 2 || asked[1].Count || asked[1].Served != nil ||
		!slices.Equal(asked[1].Takes, []AskedTake{{Take: "take-n1", Boundary: finish}}) {
		t.Errorf("a stop with a served note asked %+v, want its take, no count and no served order", asked)
	}
	if _, err := rig.pending.SettleHook(t.Context(), "t1", HookSettle{Launch: launched.LaunchID,
		Held: []string{"take-n1"}}); err != nil || readOf(t, rig, "n1") == nil {
		t.Fatalf("settling n1 held: %v, read %+v", err, readOf(t, rig, "n1"))
	}
	serveNote(t, rig.pending, launched.LaunchID, "n2", finish)
	rig.coordinator.readMainThread(t.Context(), rig.get("t1"), nil, rig.runner, BoxReadStop,
		capturePoint{cutOff: true}, Result{}, summaryAsk{})
	asked = thread.asks()
	n1, n2 := AskedTake{Take: "take-n1", Boundary: finish, Read: true}, AskedTake{Take: "take-n2", Boundary: finish}
	if len(asked) != 3 || !slices.Equal(asked[2].Takes, []AskedTake{n2}) ||
		!slices.Equal(asked[2].Served, []AskedTake{n1, n2}) {
		t.Errorf("a stop after a read note asked %+v, want n2 asked and both serves in order", asked)
	}
}

// A NOTE READ ELSEWHERE KEEPS ITS SERVE IN THE SERVED ORDER: a note served to
// the job and then read by a later launch's brief, or by the review, may have
// reached the job's record all the same, so its serve keeps its place before
// an unread take of the same words. A text matcher then spends that row on it,
// and the unread take, with no row of its own, is not held off it (executor's
// TestAReadCodexNoteKeepsItsPlace and TestAReadQwenNoteKeepsItsPlace).
//
// Mutation: keep a read note's serve only where this job's own confirm read
// it, and the brief's and the review's serves drop out.
func TestANoteReadElsewhereKeepsItsServeInTheServedOrder(t *testing.T) {
	t.Parallel()
	const job, digest = "job-1", "d-same"
	batch := types.SteerBoundaryToolBatch
	serve := func(take string) []HookServed {
		return []HookServed{{Take: take, Launch: job, Boundary: string(batch), Digest: digest}}
	}
	run := PendingRun{TurnID: "t1", LaunchID: job, Harness: "codex"}
	run.Steer.Notes = []RunNote{
		{ID: "briefed", Serves: serve("take-briefed"), Served: job,
			Read: &NoteRead{Launch: "job-2", Boundary: string(types.SteerBoundaryBrief)}},
		{ID: "reviewed", Serves: serve("take-reviewed"), Served: job,
			Read: &NoteRead{Boundary: "round", Unanswered: true}},
		{ID: "unread", Serves: serve("take-unread")},
	}
	unread := AskedTake{Take: "take-unread", Boundary: batch, Digest: digest}
	if got := askedTakes(run); !slices.Equal(got, []AskedTake{unread}) {
		t.Errorf("the read asks %+v, want the unread take alone", got)
	}
	want := []AskedTake{{Take: "take-briefed", Boundary: batch, Digest: digest, Read: true},
		{Take: "take-reviewed", Boundary: batch, Digest: digest, Read: true}, unread}
	if got := servedTakes(run); !slices.Equal(got, want) {
		t.Errorf("the job's serves in order are %+v, want the read notes' before the unread take: %+v", got, want)
	}
}

// EVERY ENDING READ PUBLISHES WHAT ITS JOB'S HOOKS READ: a collection's, a
// stop's and a loss's read each hand the engine the notes the job's session
// confirmed and the wake items it took, for the engine to claim, never a late
// call's item; and a verdict that landed with its answer lost hands the read
// back, and the read made again, asking nothing, publishes the note once.
//
// Mutation: publish at a collection alone, only off the verdict's answer, every
// item, or no wake item, and a case goes red.
func TestEveryEndingReadPublishesWhatItsJobsHooksRead(t *testing.T) {
	t.Parallel()
	for name, read := range map[string]func(*coordRig) error{
		"a collection": func(rig *coordRig) error {
			rig.runner.Finish(wholeAccount())
			_, _, err := rig.coordinator.readBox(rig.t.Context(), rig.get("t1"), BoxReadCollect)
			return err
		},
		"a stop": func(rig *coordRig) error {
			_, _, err := rig.coordinator.readBox(rig.t.Context(), rig.get("t1"), BoxReadStop)
			return err
		},
		"a loss": func(rig *coordRig) error {
			_, _, err := rig.coordinator.readLost(rig.t.Context(), rig.get("t1"))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			spy := watchConfirmed(rig.coordinator)
			launched := rig.launch("t1")
			serveNote(t, rig.pending, launched.LaunchID, "n1", types.SteerBoundaryToolBatch)
			rig.runner.MainThread = sessionShows("take-n1").read
			if err := read(rig); err != nil {
				t.Fatalf("the read: %v", err)
			}
			if got := spy.ids(); !slices.Equal(got, []string{"n1"}) {
				t.Errorf("%s published %v, want the note its session confirmed", name, got)
			}
		})
	}
	t.Run("an item read off a serve", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		spy := watchConfirmed(rig.coordinator)
		run := rig.launch("t1")
		run.Harness = "claude-code"
		served := func(id string) RunNote {
			return RunNote{ID: id, Read: &NoteRead{Launch: run.LaunchID, Take: "take-" + id},
				Serves: []HookServed{{Take: "take-" + id, Launch: run.LaunchID,
					Boundary: string(types.SteerBoundaryToolBatch)}}}
		}
		item := served("item")
		item.For = run.LaunchID
		run.Steer.Notes = []RunNote{served("n1"), item}
		if err := rig.coordinator.settleReceipts(t.Context(), run, TakeReceipts{}, BoxReadCollect); err != nil {
			t.Fatalf("the publish: %v", err)
		}
		if got := spy.ids(); !slices.Equal(got, []string{"n1"}) {
			t.Errorf("published %v, want the person's note alone, never the store's item", got)
		}
	})
	t.Run("a wake item read off a serve", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		spy := watchConfirmed(rig.coordinator)
		run := rig.launch("t1")
		run.Harness = "claude-code"
		run.Steer.Notes = []RunNote{{ID: "wake", For: run.LaunchID,
			Wake: &WakeItem{Wakes: []SealedWake{{ID: "w-1"}}}, Read: &NoteRead{Launch: run.LaunchID, Take: "take-wake"},
			Serves: []HookServed{{Take: "take-wake", Launch: run.LaunchID,
				Boundary: string(types.SteerBoundaryToolBatch)}}}}
		if err := rig.coordinator.settleReceipts(t.Context(), run, TakeReceipts{}, BoxReadCollect); err != nil {
			t.Fatalf("the publish: %v", err)
		}
		if got := spy.ids(); !slices.Equal(got, []string{"wake"}) {
			t.Errorf("published %v, want the wake item its job's session took, for the engine to claim", got)
		}
	})
	t.Run("a verdict landed with its answer lost", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		spy := watchConfirmed(rig.coordinator)
		launched := rig.launch("t1")
		serveNote(t, rig.pending, launched.LaunchID, "n1", types.SteerBoundaryToolBatch)
		thread := sessionShows("take-n1")
		rig.runner.MainThread = thread.read
		rig.runner.Finish(wholeAccount())
		store := silentVerdicts(rig.coordinator)
		store.silent, store.losing = false, true
		if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadCollect); !errors.Is(err,
			ErrStoreSilent) {
			t.Fatalf("a read whose verdict's answer was lost = %v, want it handed back", err)
		}
		store.answer()
		if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadCollect); err != nil {
			t.Fatalf("the read made again: %v", err)
		}
		if asked := thread.asks(); len(asked) != 2 || len(asked[1].Takes) != 0 {
			t.Fatalf("the reads asked %+v, want the second to ask nothing of a note already read", asked)
		}
		if got := spy.ids(); !slices.Equal(got, []string{"n1"}) {
			t.Errorf("the two reads published %v, want the note once", got)
		}
	})
}

// A STOP AND A LOSS CONFIRM, AND DENY NOTHING: their read ends the job, so the
// thread is asked as one its harness did not end, which proves no take absent,
// under the release that ran the job, which the release gate needs to confirm
// any; and the note its session shows taken is read.
//
// Mutation: skip the thread at a stop or a loss, ask it as one its harness
// ended, or skip the loss's collection of the release, and a case goes red.
func TestAStopAndALossConfirmAndDenyNothing(t *testing.T) {
	t.Parallel()
	for name, read := range map[string]func(*coordRig) error{
		"a stop": func(rig *coordRig) error {
			_, _, err := rig.coordinator.readBox(rig.t.Context(), rig.get("t1"), BoxReadStop)
			return err
		},
		"a loss": func(rig *coordRig) error {
			_, _, err := rig.coordinator.readLost(rig.t.Context(), rig.get("t1"))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			launched := rig.launch("t1")
			serveNote(t, rig.pending, launched.LaunchID, "n1", types.SteerBoundaryFinish)
			thread := sessionShows("take-n1")
			rig.runner.MainThread = thread.read
			rig.runner.EndsAs(Result{HarnessVersion: "2.1.0"})
			if err := read(rig); err != nil {
				t.Fatalf("the read: %v", err)
			}
			if asked := thread.asks(); len(asked) != 1 || asked[0].Ended || asked[0].Count ||
				asked[0].Release != "2.1.0" {
				t.Errorf("%s asked %+v, want one ask of a thread its harness did not end, under its job's "+
					"release, with no count", name, asked)
			}
			if read := readOf(t, rig, "n1"); read == nil || read.Take != "take-n1" {
				t.Errorf("%s left the note %+v, want it read off its serve", name, read)
			}
		})
	}
}

// A STOP WHOSE VERDICT THE STORE DOES NOT ANSWER IS KEPT: the stop is never
// ended uncharged for it, nor its box reclaimed, and once the store answers the
// stop's retry confirms the note and ends the run.
//
// Mutation: go on past the verdict's silence, and the stop ends with the note
// unconfirmed.
func TestAStopWhoseVerdictTheStoreDoesNotAnswerIsKept(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spy := watchConfirmed(rig.coordinator)
	launched := rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	serveNote(t, rig.pending, launched.LaunchID, "n1", types.SteerBoundaryToolBatch)
	rig.runner.MainThread = sessionShows("take-n1").read
	store := silentVerdicts(rig.coordinator)

	rig.stop(t)
	rig.fireRetries()
	run, found, err := rig.pending.Get(t.Context(), "t1")
	if err != nil || !found || run.Stop == nil || run.Stop.Collected {
		t.Fatalf("the stop whose verdict went unanswered left %+v (found %v, %v), want the stop kept", run.Stop,
			found, err)
	}
	if slices.Contains(rig.provider.KilledIDs(), launched.SandboxID) {
		t.Fatal("the stop reclaimed the box its verdict is still to be read from")
	}

	store.answer()
	rig.fireRetries()
	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Fatal("the stop is still kept once its verdict landed")
	}
	if got := spy.ids(); !slices.Equal(got, []string{"n1"}) {
		t.Errorf("the stop published %v, want the note its session confirmed", got)
	}
}

// lostWithServe launches t1, serves it note n1 its session shows taken, and has
// its seat recovered by another node, which finds the job lost with its engine.
func lostWithServe(t *testing.T, rig *coordRig) PendingRun {
	t.Helper()
	launched := rig.launching("t1")
	serveNote(t, rig.pending, launched.LaunchID, "n1", types.SteerBoundaryToolBatch)
	rig.runner.MainThread = sessionShows("take-n1").read
	if err := rig.recoverSeat(t.Context(), "node-b:1", 9); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	return launched
}

// A LOSS'S VERDICT IS RETRIED BY ITS ENDING WITH THE BOX KEPT: a verdict the
// store does not answer keeps the loss's ending, and its box, for the retry,
// which reads the box again once the store answers and only then reclaims it.
//
// Mutation: go on past the verdict's silence, and the box is reclaimed with
// the note unconfirmed.
func TestALossVerdictIsRetriedByItsEndingWithTheBoxKept(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		spy := watchConfirmed(rig.coordinator)
		store := silentVerdicts(rig.coordinator)
		launched := lostWithServe(t, rig)
		rig.fireRetries()
		rig.awaitKeeps()
		if slices.Contains(rig.provider.KilledIDs(), launched.SandboxID) {
			t.Fatal("the loss reclaimed the box its verdict is still to be read from")
		}
		if _, found, _ := rig.pending.Get(t.Context(), "t1"); !found {
			t.Fatal("the loss's ending finished with its verdict unanswered")
		}
		store.answer()
		rig.fireRetries()
		rig.awaitKeeps()
		rig.fireRetries()
		if !slices.Contains(rig.provider.KilledIDs(), launched.SandboxID) {
			t.Fatal("the loss's box was not reclaimed once its verdict landed")
		}
		if got := spy.ids(); !slices.Equal(got, []string{"n1"}) {
			t.Errorf("the loss published %v, want the note its session confirmed", got)
		}
	})
}

// A LOSS ENDING REPORTS WHAT ITS READ CONFIRMED: the ending reads the row again
// after the loss's read, so a note the job's session showed taken is not
// reported unconfirmed off the copy read before it, even where the row already
// owes the turn's end and the ending writes nothing that hands it a fresh one.
//
// Mutation: report from the top read's copy, and the note is reported
// unconfirmed.
func TestALossEndingReportsWhatItsReadConfirmed(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		ends := watchEnds(rig)
		launched := rig.launching("t1")
		serveNote(t, rig.pending, launched.LaunchID, "n1", types.SteerBoundaryToolBatch)
		rig.runner.MainThread = sessionShows("take-n1").read
		if _, wrote, err := rig.pending.OweEnd(t.Context(), "t1", []byte(`{"outcome":"failed"}`)); err != nil ||
			!wrote {
			t.Fatalf("OweEnd = %v, %v", wrote, err)
		}
		if err := rig.recoverSeat(t.Context(), "node-b:1", 9); err != nil {
			t.Fatalf("RecoverSeat: %v", err)
		}
		rig.fireRetries()
		rig.awaitKeeps()
		rig.fireRetries()
		for _, call := range ends.seen() {
			if slices.ContainsFunc(call.ended, func(end string) bool { return strings.HasPrefix(end, "n1:") }) {
				t.Errorf("the ending reported %v, want the note its read confirmed reported by no end", call.ended)
			}
		}
		if !slices.Contains(rig.provider.KilledIDs(), launched.SandboxID) {
			t.Error("the loss's ending did not reach its reclaim")
		}
	})
}

// A LOSS OF A COMPANY THAT KEEPS NO SESSION IS READ FOR ITS SERVES: whatever the
// company keeps, a lost job whose copy holds a serve has its box read before
// the reclaim, for its session's verdict alone, and no session is taken.
//
// Mutation: read a loss's box only where sessions are kept, and the note is
// never confirmed.
func TestALossOfACompanyThatKeepsNoSessionIsReadForItsServes(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spy := watchConfirmed(rig.coordinator)
	launched := lostWithServe(t, rig)
	rig.fireRetries()
	rig.fireRetries()
	if got := spy.ids(); !slices.Equal(got, []string{"n1"}) {
		t.Errorf("the loss published %v, want the note its session confirmed", got)
	}
	if takes := rig.runner.Takes(); len(takes) != 0 {
		t.Errorf("took %d sessions for a company that keeps none", len(takes))
	}
	if !slices.Contains(rig.provider.KilledIDs(), launched.SandboxID) {
		t.Error("the lost job's box was not reclaimed after its read")
	}
}

// failingTwice launches t1 with a served note its session shows taken, its job
// finished, and two failed collections on its record.
func failingTwice(t *testing.T, rig *coordRig) {
	t.Helper()
	launched := rig.launchedBy("t1", "node-a")
	serveNote(t, rig.pending, launched.LaunchID, "n1", types.SteerBoundaryToolBatch)
	rig.runner.MainThread = sessionShows("take-n1").read
	rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &SessionArchive{SessionID: "s-1"}})
	if _, _, err := rig.pending.mutate(t.Context(), "t1", func(run *PendingRun) bool {
		run.Launch.CollectFailures, run.Launch.CollectFailingSince = 2, rig.now.Add(-time.Hour)
		return true
	}); err != nil {
		t.Fatalf("counting two failures: %v", err)
	}
}

// remoteVerdicts makes the rig's coordinator node-b, collecting t1 off node-a's
// host, whose verdicts go unanswered.
func remoteVerdicts(t *testing.T, rig *coordRig) *verdictStore {
	t.Helper()
	store := &verdictStore{PendingStore: rig.pending, silent: true}
	host := rig.hostOn(t, "node-a", store)
	host.verdictWait, host.verdictRetry = 20*time.Millisecond, 5*time.Millisecond
	rig.onNode(t, "node-b", hostAnswers{host: host}, nil)
	return store
}

// A VERDICT THE STORE DOES NOT ANSWER ENDS THE RUN OF FAILED COLLECTIONS: the
// box answered before the verdict went unanswered, here or on its host, so the
// claim goes back uncounted with the failures behind it ended.
//
// Mutation: leave the box's answer unmarked by the error, and the two failures
// stand.
func TestAVerdictTheStoreDoesNotAnswerEndsTheRunOfFailedCollections(t *testing.T) {
	t.Parallel()
	for name, arrange := range map[string]func(*testing.T, *coordRig){
		"here": func(_ *testing.T, rig *coordRig) {
			rig.coordinator.node = "node-a"
			silentVerdicts(rig.coordinator)
		},
		"on another host": func(t *testing.T, rig *coordRig) { remoteVerdicts(t, rig) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			arrange(t, rig)
			failingTwice(t, rig)
			if err := collectAgain(t, rig); !errors.Is(err, ErrStoreSilent) {
				t.Fatalf("the collection = %v, want it handed back for the store's silence", err)
			}
			if got := rig.get("t1"); got.Status != StatusRunning || got.Launch.CollectFailures != 0 ||
				!got.Launch.CollectFailingSince.IsZero() {
				t.Errorf("the run reads %s with %d failures since %v, want it running with none", got.Status,
					got.Launch.CollectFailures, got.Launch.CollectFailingSince)
			}
		})
	}
}

// A ROW HANDED BACK ON A SILENT VERDICT SAYS ITS HARNESS ENDED: the hand-back is
// the write that returns the row to running, so it carries how the job's
// harness ended, here or through a host's reply, and no row reads running for a
// job whose harness is over with nothing on it saying so.
//
// Mutation: drop the end from the release's write, or from the reply, and a
// case goes red.
func TestARowHandedBackOnASilentVerdictSaysItsHarnessEnded(t *testing.T) {
	t.Parallel()
	for name, arrange := range map[string]func(*testing.T, *coordRig){
		"here": func(_ *testing.T, rig *coordRig) {
			rig.coordinator.node = "node-a"
			silentVerdicts(rig.coordinator)
		},
		"on another host": func(t *testing.T, rig *coordRig) { remoteVerdicts(t, rig) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			arrange(t, rig)
			failingTwice(t, rig)
			if err := collectAgain(t, rig); !errors.Is(err, ErrStoreSilent) {
				t.Fatalf("the collection = %v, want it handed back for the store's silence", err)
			}
			if got := rig.get("t1"); got.Status != StatusRunning || got.Launch.HarnessEnd != HarnessEndOwnRun {
				t.Errorf("the row reads %s with the harness end %q, want running, its harness's own run said",
					got.Status, got.Launch.HarnessEnd)
			}
		})
	}
}

// A HOST'S ANSWERED FLAG CROSSES THE WIRE: a host whose store did not answer the
// verdict after the box answered says so, with how the job's harness ended,
// and its asker reads the store's silence with the box's answer on it; a host
// whose store did not answer its read of the row says nothing of the box.
//
// Mutation: drop the flag AnswerBoxRead sets, its key on the wire, or Err's
// wrap of it, and a case goes red.
func TestAHostsAnsweredFlagCrossesTheWire(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	store := &verdictStore{PendingStore: rig.pending, silent: true}
	host := rig.hostOn(t, "node-a", store)
	host.verdictWait, host.verdictRetry = 20*time.Millisecond, 5*time.Millisecond
	failingTwice(t, rig)
	run := rig.get("t1")
	if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1", CompletionTail(run.LaunchID),
		rigLease); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	read := BoxReadRequest{Version: BoxReadWireVersion, ID: "read-1", Node: "node-a", TurnID: "t1",
		LaunchID: run.LaunchID, SandboxID: run.SandboxID, Read: BoxReadCollect}
	reply := AnswerBoxRead(t.Context(), host, read)
	raw, err := json.Marshal(reply)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"answered":true`) || !strings.Contains(string(raw), `"harness_end":"own_run"`) {
		t.Fatalf("the host answered %s, want the box's answer and its harness's end flagged", raw)
	}
	got, err := crossed(reply)
	if err != nil {
		t.Fatalf("the reply did not cross: %v", err)
	}
	var answered *boxAnsweredError
	if err := got.Err(); !errors.Is(err, ErrStoreSilent) || !errors.As(err, &answered) ||
		answered.end != HarnessEndOwnRun {
		t.Fatalf("the asker reads the reply as %v, want the store's silence after the box answered", err)
	}

	silent := &silentStore{PendingStore: rig.pending, silent: []string{"Get"}}
	unread := AnswerBoxRead(t.Context(), rig.hostOn(t, "node-a", silent), read)
	if unread.Answered || !unread.Unsettled || errors.As(unread.Err(), &answered) {
		t.Errorf("a host whose store did not answer the row's read answered %+v, want the silence alone", unread)
	}
}

// THE VERDICT IS GIVEN ITS ROW'S CROSSING: a store that answers past the
// verdict's reason but inside the time a row at its bound takes to cross lands
// the verdict, rather than handing the collection back for ever.
//
// Mutation: hold the verdict to its flat reason, and the read is handed back.
func TestTheVerdictIsGivenItsRowsCrossing(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig := newCoordRig(t)
		launched := rig.launch("t1")
		serveNote(t, rig.pending, launched.LaunchID, "n1", types.SteerBoundaryToolBatch)
		rig.runner.MainThread = sessionShows("take-n1").read
		rig.runner.Finish(wholeAccount())
		rig.coordinator.pending = &verdictStore{PendingStore: rig.pending, slow: hookVerdictWait + time.Second,
			crossing: time.Minute}
		if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadCollect); err != nil {
			t.Fatalf("a verdict answered inside its row's crossing = %v, want it landed", err)
		}
		if read := readOf(t, rig, "n1"); read == nil {
			t.Fatal("the verdict answered inside its row's crossing did not land")
		}
	})
}

// A LOCAL VERDICT SILENCE IS NEVER GIVEN UP, AT A STOP OR A LOSS: where this node
// hosts the box, a verdict the store did not answer is this node's own store's
// silence, whatever its read of the row answered, so the ending is kept, and
// its box, however old the stop or the loss, never ended with nothing read.
//
// Mutation: drop the hosts check from readStopped's or keepLostSession's
// give-up, and a day-old ending is given up with its note unconfirmed.
func TestALocalVerdictSilenceIsNeverGivenUpAtAStopOrALoss(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]func(*testing.T, *coordRig) PendingRun{
		"a stop": func(t *testing.T, rig *coordRig) PendingRun {
			launched := rig.launch("t1")
			rig.coordinator.countRun("swe", StatusRunning)
			serveNote(t, rig.pending, launched.LaunchID, "n1", types.SteerBoundaryToolBatch)
			rig.runner.MainThread = sessionShows("take-n1").read
			rig.stop(t)
			return launched
		},
		"a loss": lostWithServe,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			silentVerdicts(rig.coordinator)
			launched := end(t, rig)
			rig.fireRetries()
			rig.now = rig.now.Add(24 * time.Hour)
			rig.fireRetries()
			rig.fireRetries()
			if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || !found {
				t.Errorf("%s whose verdict this node's own store did not answer was ended a day on (found %v, "+
					"%v), want it kept", name, found, err)
			}
			if slices.Contains(rig.provider.KilledIDs(), launched.SandboxID) {
				t.Errorf("%s reclaimed the box its verdict is still to be read from", name)
			}
		})
	}
}

// A REMOTE LOSS'S VERDICT SILENCE KEEPS ITS ENDING UNTIL ITS HOST IS GIVEN UP: a
// host whose store did not answer the verdict says so, and the loss's ending is
// kept with its box; where this node's own read of the row answered, the
// silence is the host's alone, given up as a stop's is, and the loss then ends,
// its box left to its host's reclaim (reclaimBox); where it did not, the
// silence is this node's too, and is kept whatever the bound.
//
// Mutation: drop ReadHostedBox's return of the loss read's error, and the box
// is reclaimed at once; drop keepLostSession's give-up, and it is kept for ever.
func TestARemoteLossesVerdictSilenceKeepsItsEndingUntilItsHostIsGivenUp(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	remoteVerdicts(t, rig)
	told := &nudges{}
	rig.coordinator.nudge = told.send
	launched := rig.launchedBy("t1", "node-a")
	reclaimed := func() bool {
		return slices.ContainsFunc(told.all(), func(n Nudge) bool {
			return n.Why == NudgeReleased && n.LaunchID == launched.LaunchID
		})
	}
	serveNote(t, rig.pending, launched.LaunchID, "n1", types.SteerBoundaryToolBatch)
	rig.runner.MainThread = sessionShows("take-n1").read
	rig.coordinator.finish(t.Context(), rig.get("t1"), ending{fence: fenceOf(launched),
		note: &failureNote{reason: "engine_restarted", detail: "the engine that ran the job restarted"}})
	rig.fireRetries()
	rig.fireRetries()
	if _, found, _ := rig.pending.Get(t.Context(), "t1"); !found {
		t.Fatal("the loss's ending finished with its host's verdict unanswered")
	}
	if reclaimed() || slices.Contains(rig.provider.KilledIDs(), launched.SandboxID) {
		t.Fatal("the loss reclaimed the box its host's verdict is still to be read from")
	}

	rig.now = rig.now.Add(24 * time.Hour)
	if err := rig.coordinator.keepLostSession(t.Context(), rig.get("t1"), false); !errors.Is(err,
		ErrStoreSilent) {
		t.Fatalf("a day-old loss whose row this node's store did not answer = %v, want it kept", err)
	}
	rig.fireRetries()
	rig.fireRetries()
	if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
		t.Error("the loss is still kept a day on, its host's store alone silent, want its host given up")
	}
	if !reclaimed() {
		t.Error("the loss given up did not reach its reclaim: node-a's host was not told its box is released")
	}
	if slices.Contains(rig.provider.KilledIDs(), launched.SandboxID) {
		t.Error("node-b killed a box on node-a's host, which only that host reclaims")
	}
}

// A VERDICT THE STORE REFUSES ONCE IS ASKED AGAIN WITHIN ITS ROOM: a refusal is
// no silence while the room lasts, so the second attempt lands the verdict and
// the read goes on, never handed back.
//
// Mutation: make one attempt, and the read is handed back.
func TestAVerdictTheStoreRefusesOnceIsAskedAgainWithinItsRoom(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	launched := rig.launch("t1")
	serveNote(t, rig.pending, launched.LaunchID, "n1", types.SteerBoundaryToolBatch)
	rig.runner.MainThread = sessionShows("take-n1").read
	rig.runner.Finish(wholeAccount())
	rig.coordinator.pending = &verdictStore{PendingStore: rig.pending, refusals: 1}
	rig.coordinator.verdictWait, rig.coordinator.verdictRetry = time.Minute, 5*time.Millisecond
	if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadCollect); err != nil {
		t.Fatalf("a verdict refused once = %v, want it asked again and landed", err)
	}
	if read := readOf(t, rig, "n1"); read == nil {
		t.Fatal("the verdict asked again did not land")
	}
}

// A POLL SERVE A SESSION RECORD HOLDS IS CONFIRMED THROUGH THE ONE RECEIPTS
// PATH: a poll recorded under the inbox id its harness admitted the note by is
// asked of the job's record at its read, beside its item's digest for a record
// that holds text; a record holding the item settles the note and publishes
// it, and a record that cannot be read decides nothing, leaving the note
// waiting. A runner that confirms off its record and reads no thread is asked
// the takes alone, never a count.
//
// Mutation: ask no poll serve of the record, drop its digest from the ask,
// settle it off another id, publish an undecided record's notes, or ask such a
// runner a count, and a case goes red.
func TestAPollServeASessionRecordHoldsIsConfirmedThroughTheReceipts(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		receipts  TakeReceipts
		published []string
	}{
		"a record holding the inbox id": {TakeReceipts{Held: []string{"msg_n1"}}, []string{"n1"}},
		"a record that cannot be read":  {TakeReceipts{Why: "the session record could not be read"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			rig.runner.Thread = ThreadTraits{Receipts: true}
			spy := watchConfirmed(rig.coordinator)
			launched := rig.launch("t1")
			fileNote(t, rig.pending, "t1", "n1")
			if _, err := rig.pending.ServeNotes(t.Context(), "t1", NoteServe{IDs: []string{"n1"},
				Launch: launched.LaunchID, Statuses: []string{StatusLaunching, StatusRunning},
				Inbox: map[string]InboxItem{"n1": {ID: "msg_n1", Digest: "digest-n1"}}}); err != nil {
				t.Fatalf("the poll: %v", err)
			}
			thread := &receiptThread{receipts: tc.receipts}
			rig.runner.MainThread = thread.read
			if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadStop); err != nil {
				t.Fatalf("the read: %v", err)
			}
			asked := thread.asks()
			if len(asked) != 1 || asked[0].Count || !slices.Equal(asked[0].Takes,
				[]AskedTake{{Take: "msg_n1", Boundary: types.SteerBoundaryStep, Digest: "digest-n1"}}) {
				t.Errorf("the record was asked %+v, want the poll's inbox id and digest alone and no count", asked)
			}
			if got := spy.ids(); !slices.Equal(got, tc.published) {
				t.Errorf("published %v, want %v", got, tc.published)
			}
			if read := readOf(t, rig, "n1"); (read != nil) != (len(tc.published) > 0) {
				t.Errorf("the note's read is %+v", read)
			}
		})
	}
}

// A WAKE SERVED ONCE SETTLES AT THE RUN'S END, off the record the end leaves: a
// read before the harness flushed its record decides nothing and leaves the
// wake waiting on its one serve (MaxWakeServes), and the collection, re-reading
// the record, settles it and publishes it, so the engine claims it rather than
// running it as a turn of its own.
//
// Mutation: lose the wake's serve to an undecided read, or settle the end's
// collection without re-reading the record, and this goes red.
func TestAWakeServedOnceSettlesAtTheRunsEndOffItsRecord(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.runner.Thread = ThreadTraits{Receipts: true}
	spy := watchConfirmed(rig.coordinator)
	launched := rig.launch("t1")
	wake, err := rig.pending.FileWake(t.Context(), "t1", WakeFiling{Launch: launched.LaunchID,
		Wake: SealedWake{ID: "w-1", Sealed: []byte(strings.Repeat("s", 64))}})
	if err != nil {
		t.Fatalf("filing the wake: %v", err)
	}
	if _, err := rig.pending.ServeNotes(t.Context(), "t1", NoteServe{IDs: []string{wake.ID},
		Launch: launched.LaunchID, Statuses: []string{StatusLaunching, StatusRunning},
		Wakes: map[string]int{wake.ID: 1}, Inbox: map[string]InboxItem{wake.ID: {ID: "msg_w"}}}); err != nil {
		t.Fatalf("the poll: %v", err)
	}
	if err := rig.coordinator.settleReceipts(t.Context(), rig.get("t1"),
		TakeReceipts{Why: "the session record could not be read"}, BoxReadStop); err != nil {
		t.Fatalf("the early read: %v", err)
	}
	if got := spy.ids(); len(got) != 0 || readOf(t, rig, wake.ID) != nil {
		t.Fatalf("an undecided read published %v or read the wake", got)
	}
	thread := sessionShows("msg_w")
	rig.runner.MainThread = thread.read
	rig.runner.Finish(wholeAccount())
	if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadCollect); err != nil {
		t.Fatalf("the collection: %v", err)
	}
	if got := spy.ids(); !slices.Equal(got, []string{wake.ID}) || readOf(t, rig, wake.ID) == nil {
		t.Errorf("the run's end published %v, want the wake its record holds, read", got)
	}
}
