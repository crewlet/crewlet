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
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// summaryThread is a job's main thread holding a summary, as a runner that
// reads one answers: the summary it holds where the read asks for it, and
// every ask kept for the case to read.
type summaryThread struct {
	mu    sync.Mutex
	text  string
	read  ThreadSummary
	asked []MainThreadAsk
}

func (s *summaryThread) answer(_ context.Context, _ Sandbox, ask MainThreadAsk) MainThreadReading {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, ask)
	if !ask.Summary {
		return MainThreadReading{}
	}
	if s.read != (ThreadSummary{}) {
		return MainThreadReading{Summary: s.read}
	}
	return MainThreadReading{Summary: ThreadSummary{Text: s.text, Bytes: int64(len(s.text))}}
}

func (s *summaryThread) asks() []MainThreadAsk {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked)
}

// summaryRig is a keeping rig whose runner reads summaries, its job's thread
// holding text.
func summaryRig(t *testing.T, text string) (*coordRig, *sessionWrites, *sessionKeep, *summaryThread) {
	t.Helper()
	rig, writes, keep := keepingRig(t)
	thread := &summaryThread{text: text}
	rig.runner.Thread = ThreadTraits{Summaries: true}
	rig.runner.MainThread = thread.answer
	return rig, writes, keep, thread
}

// endOf owes and publishes turnID's end, answering what the log recorded.
func endOf(t *testing.T, rig *coordRig, writes *sessionWrites, turnID string) EndRecord {
	t.Helper()
	if _, err := rig.coordinator.OweEnd(t.Context(), turnID, executorkeep.Ending{
		Outcome: executorkeep.OutcomeCompleted}); err != nil {
		t.Fatalf("OweEnd: %v", err)
	}
	if _, err := rig.coordinator.publishEnd(t.Context(), rig.get(turnID)); err != nil {
		t.Fatalf("publishEnd: %v", err)
	}
	ends := writes.endedRecords()
	if len(ends) != 1 {
		t.Fatalf("recorded %d ends, want the turn's one", len(ends))
	}
	return ends[0]
}

// A LOST SESSION KEEPS ITS SUMMARY. Every box read asks the job's main thread
// for its newest summary as written, and the take keeps it scrubbed of what its
// capture is scrubbed of, as an object of its own, whatever came of the
// session: landed, too large at the take or at the seal, a take its box
// refused, an upload the store refused, a record the log never answered, no
// spool room, a run out of context or refused by its provider. The run's row
// names it in the take's own stamp, the turn's end names the same, and the
// session's outcome is what it would have been without it.
//
// Mutation: keep the summary only in a spooled session's keep, or before the
// scrub, and the refused cases lose it or keep the scrubbed value.
func TestALostSessionKeepsItsSummary(t *testing.T) {
	t.Parallel()
	const text = "## Next Move\n\nRerun the cache test with " + handedToken + " set."
	kept := strings.ReplaceAll(text, handedToken, "[scrubbed]")
	for name, c := range map[string]struct {
		arm    func(*coordRig, *sessionWrites, *sessionKeep) func()
		result func(*FakeSession) Result
		loss   executorkeep.Loss
	}{
		"kept": {},
		"too large at the take": {loss: executorkeep.LossTooLarge,
			arm: func(rig *coordRig, _ *sessionWrites, _ *sessionKeep) func() {
				rig.runner.Session.TakeErr = fmt.Errorf("executor: past its bound: %w", executorkeep.ErrSessionTooLarge)
				return func() {}
			}},
		"too large at the seal": {loss: executorkeep.LossTooLarge,
			arm: func(_ *coordRig, _ *sessionWrites, keep *sessionKeep) func() {
				keep.sealErr = executorkeep.ErrSessionTooLarge
				return func() {}
			}},
		"a take its box refused": {loss: executorkeep.LossKeepFailed,
			arm: func(rig *coordRig, _ *sessionWrites, _ *sessionKeep) func() {
				rig.runner.Session.TakeErr = errors.New("the box's session could not be read")
				return func() {}
			}},
		"no spool room": {loss: executorkeep.LossNoSpoolRoom,
			arm: func(rig *coordRig, _ *sessionWrites, _ *sessionKeep) func() {
				return rig.coordinator.trySpoolRoom(spoolRoomBytes).release
			}},
		"an upload the store refused": {loss: executorkeep.LossStoreRefused,
			arm: func(_ *coordRig, _ *sessionWrites, keep *sessionKeep) func() {
				keep.putErr = objstore.ErrFull
				return func() {}
			}},
		"a record the log never answered": {loss: executorkeep.LossLogRefused,
			arm: func(_ *coordRig, writes *sessionWrites, _ *sessionKeep) func() {
				writes.refuse = errors.New("no data node answered")
				return func() {}
			}},
		"a run out of context": {loss: executorkeep.LossContextExhausted, result: finishedOutOfContext},
		"a request its provider refused": {loss: executorkeep.LossRequestRefused,
			result: func(session *FakeSession) Result {
				return Result{Error: "refused", RequestRefused: true, SessionID: "s-1", Captured: &session.Archive}
			}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, keep, thread := summaryRig(t, text)
				rig.launchKept(t, "t1")
				if c.arm != nil {
					defer c.arm(rig, writes, keep)()
				}
				result := Result{Success: true, SessionID: "s-1", Captured: &rig.runner.Session.Archive}
				if c.result != nil {
					result = c.result(rig.runner.Session)
				}
				rig.runner.Finish(result)
				collectHeld(t, rig, "t1")

				if asks := thread.asks(); len(asks) != 1 || !asks[0].Summary {
					t.Fatalf("the read asked %+v, want the summary once", asks)
				}
				if put := keep.summariesPut(); !slices.Equal(put, []string{kept}) {
					t.Fatalf("kept the summaries %q, want the thread's one, scrubbed of what its capture is", put)
				}
				row := rig.get("t1")
				if row.Summary == nil || row.Summary.Kept != int64(len(kept)) || row.Summary.Session == "" ||
					row.Summary.Session != row.SessionRan() || row.Summary.Harness != row.Harness ||
					row.Summary.Digest != summaryDigest(kept) {
					t.Fatalf("the run's row names %+v, want the summary the read kept", row.Summary)
				}
				switch {
				case c.loss == "" && (row.Capture == nil || row.Capture.State != CaptureLanded):
					t.Fatalf("the row stamps %+v, want the session landed beside its summary", row.Capture)
				case c.loss != "" && (row.Capture == nil || row.Capture.Loss != c.loss):
					t.Fatalf("the row stamps %+v, want the capture lost for %s as without a summary",
						row.Capture, c.loss)
				}
				end := endOf(t, rig, writes, "t1")
				named, err := row.Summary.Named()
				if err != nil || end.Ending.Summary == nil || *end.Ending.Summary != named {
					t.Fatalf("the turn's end names %+v, want the summary its row names (%v)", end.Ending.Summary, err)
				}
			})
		})
	}
}

// A STOP'S READ NAMES ITS SUMMARY BEFORE THE TURN'S END IS RECORDED: its
// ending records the end as soon as its read returns and its captures settle,
// so the summary rides the take's own stamp, or the keep the end waits for.
//
// Mutation: name the summary on a write of its own after the read, and the end
// names none.
func TestAStopsSummaryIsNamedByTheTurnsEnd(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, keep, _ := summaryRig(t, "## Objective\n\nShip the fix.")
		rig.launchKept(t, "t1")
		rig.runner.Finish(Result{Text: "halfway through"})
		if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder"}); err != nil {
			t.Fatalf("StopSeat: %v", err)
		}
		rig.fireRetries()
		rig.awaitKeeps()
		rig.fireRetries()
		ends := writes.endedRecords()
		if len(ends) != 1 || ends[0].Ending.Summary == nil || len(keep.summariesPut()) != 1 {
			t.Fatalf("recorded %+v, kept %q summaries, want the stopped turn's end naming its one summary",
				ends, keep.summariesPut())
		}
	})
}

// THE TAKE READS ITS SCRUB'S VALUES ONCE FOR ITSELF AND ITS SUMMARY: a read
// whose take spools the session and one whose take is refused before it each
// ask what the box was handed once, the summary scrubbed of the same values.
//
// Mutation: read the values again for the summary, and a case asks twice.
func TestTheTakeReadsItsScrubsValuesOnce(t *testing.T) {
	t.Parallel()
	for name, refused := range map[string]bool{"a take that spools": false, "a take refused untaken": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, _, keep, _ := summaryRig(t, "a summary naming "+handedToken)
				counted := &handedReads{PendingStore: rig.coordinator.pending}
				rig.coordinator.pending = counted
				rig.launchKept(t, "t1")
				result := Result{Success: true, SessionID: "s-1", Captured: &rig.runner.Session.Archive}
				if refused {
					result = finishedOutOfContext(rig.runner.Session)
				}
				rig.runner.Finish(result)
				collectHeld(t, rig, "t1")
				if n := counted.reads(); n != 1 {
					t.Errorf("the read asked what the box was handed %d times, want once", n)
				}
				if put := keep.summariesPut(); len(put) != 1 || strings.Contains(put[0], handedToken) {
					t.Errorf("kept %q, want the summary scrubbed of the values the take read", put)
				}
			})
		})
	}
}

// handedReads counts the reads of what a run's box was handed.
type handedReads struct {
	PendingStore
	mu sync.Mutex
	n  int
}

func (h *handedReads) Handed(ctx context.Context, run PendingRun) (string, error) {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
	return h.PendingStore.Handed(ctx, run)
}

func (h *handedReads) reads() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// A SUMMARY IS KEPT ONLY SCRUBBED, AND ONCE: a read whose capture's values
// cannot be known keeps no summary; a read keeps its own once, answering the
// same stamp again; and a read finding the summary the row names already,
// under a key still fresh, keeps nothing new, while once that key is older
// than summaryFresh the same summary is kept again.
//
// Mutation: keep a summary unscrubbed, keep a read's twice, drop the digest's
// check, or keep the key fresh for ever, and a case goes red.
func TestASummaryIsKeptOnlyScrubbedAndOnce(t *testing.T) {
	t.Parallel()
	t.Run("no scrub, no summary", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, keep, _ := summaryRig(t, "a summary")
			rig.launch("t1")
			rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &rig.runner.Session.Archive})
			collectHeld(t, rig, "t1")
			if put := keep.summariesPut(); len(put) != 0 || rig.get("t1").Summary != nil {
				t.Fatalf("kept %q with nothing to scrub it of", put)
			}
		})
	})
	t.Run("kept once while fresh", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, keep, _ := summaryRig(t, "the same summary")
			run := rig.launchKept(t, "t1")
			read := func() *readSummary {
				return &readSummary{found: ThreadSummary{Text: "the same summary", Bytes: 16}, keeper: rig.runner,
					scrubbed: true}
			}
			first := read()
			run.Summary = rig.coordinator.keepFound(t.Context(), run, first)
			if run.Summary == nil || rig.coordinator.keepFound(t.Context(), run, first) != run.Summary {
				t.Fatal("the first read did not keep its summary once, answering it again")
			}
			if again := rig.coordinator.keepFound(t.Context(), run, read()); again != nil {
				t.Fatalf("a later read kept the same summary again while its key was fresh: %+v", again)
			}
			rig.now = rig.now.Add(summaryFresh + time.Second)
			if again := rig.coordinator.keepFound(t.Context(), run, read()); again == nil {
				t.Fatal("did not keep the same summary again once its key was past its freshness")
			}
			if put := keep.summariesPut(); len(put) != 2 {
				t.Fatalf("kept it %d times, want twice", len(put))
			}
		})
	})
}

// A SUMMARY THAT IS NOT KEPT CHANGES NOTHING ELSE: a read that could not say
// whether the thread holds one, an upload the store refused, and a thread
// holding none each leave the row naming no summary and the session's capture
// as it would be.
//
// Mutation: fail the take for a summary not kept, and the session is not kept
// either.
func TestASummaryNotKeptChangesNothingElse(t *testing.T) {
	t.Parallel()
	for name, arm := range map[string]func(*sessionKeep, *summaryThread){
		"unread":       func(_ *sessionKeep, thread *summaryThread) { thread.read = ThreadSummary{Unread: "cut"} },
		"refused":      func(keep *sessionKeep, _ *summaryThread) { keep.summaryErr = objstore.ErrFull },
		"held by none": func(_ *sessionKeep, thread *summaryThread) { thread.text = "" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, keep, thread := summaryRig(t, "a summary")
				arm(keep, thread)
				rig.launchKept(t, "t1")
				rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &rig.runner.Session.Archive})
				collectHeld(t, rig, "t1")
				row := rig.get("t1")
				if row.Summary != nil || row.Capture == nil || row.Capture.State != CaptureLanded {
					t.Fatalf("the row names summary %+v and capture %+v, want no summary and the session landed",
						row.Summary, row.Capture)
				}
				if end := endOf(t, rig, writes, "t1"); end.Ending.Summary != nil {
					t.Fatalf("the end names %+v, want none", end.Ending.Summary)
				}
			})
		})
	}
}

// A SUMMARY'S KEEP NEVER HOLDS THE SESSION'S: a node that stops while its
// summary's upload is held, its session's upload not yet begun, still uploads
// the session and stamps it landed, naming no summary, and its stop ends.
//
// Mutation: upload the summary before the session in the keep, under the
// keep's own context, and the stop's drain cuts the session's upload.
func TestASummaryKeepNeverHoldsTheSessionsKeep(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, keep, _ := summaryRig(t, "a summary")
		keep.summaryGate, keep.gate = make(chan struct{}), make(chan struct{})
		rig.launchKept(t, "t1")
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &rig.runner.Session.Archive})
		collectLeaving(t, rig, "t1")
		synctest.Wait()
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			rig.coordinator.Stop()
		}()
		synctest.Wait()
		close(keep.gate)
		<-stopped
		row := rig.get("t1")
		if len(keep.uploads()) != 1 || row.Capture == nil || row.Capture.State != CaptureLanded {
			t.Fatalf("uploaded %d sessions, the row stamps %+v: want the session landed beside a held summary",
				len(keep.uploads()), row.Capture)
		}
		if row.Summary != nil {
			t.Fatalf("the row names %+v, a summary whose upload never ended", row.Summary)
		}
	})
}

// A PARKED TURN'S END NAMES A FRESH COPY OF ITS SUMMARY: a turn whose summary
// was kept longer ago than objstore.RecordWithin, which the executor log names
// no object past, still ends naming a readable summary, copied under a fresh
// key, with the words its run's row named.
//
// Mutation: name the row's summary whatever its age, and the end names a key
// the log leaves out.
func TestAParkedTurnsEndNamesAFreshCopyOfItsSummary(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, keep, _ := summaryRig(t, "## Next Move\n\nAsk again.")
		rig.launchKept(t, "t1")
		rig.runner.Finish(finishedOutOfContext(rig.runner.Session))
		collectHeld(t, rig, "t1")
		row := rig.get("t1")
		if row.Summary == nil {
			t.Fatal("the premise: no summary was kept")
		}
		rig.now = rig.now.Add(objstore.RecordWithin + time.Hour)
		end := endOf(t, rig, writes, "t1")
		named := end.Ending.Summary
		if named == nil || named.Object.Key.String() == row.Summary.Object ||
			rig.now.Sub(named.Object.Key.Minted()) > summaryFresh {
			t.Fatalf("the end names %+v, want a copy minted now of %s", named, row.Summary.Object)
		}
		keep.mu.Lock()
		words, copied := keep.copied[named.Object.Key]
		keep.mu.Unlock()
		if !copied || words != "## Next Move\n\nAsk again." {
			t.Fatalf("the copy holds %q (%v), want the summary's words", words, copied)
		}
	})
}

// A SUMMARY STAMP CARRIES THE KEYS A LATER BUILD ADDS, as every section of the
// run's row that a later build may grow does (wirekeys.go): read and written
// back by this build, it keeps them.
//
// Mutation: decode the stamp without its Extra, and the key is gone.
func TestASummaryStampCarriesTheKeysALaterBuildAdds(t *testing.T) {
	t.Parallel()
	raw := `{"launch_seq":1,"object":"x","digest":"d","later":{"a":1}}`
	var stamp SummaryStamp
	if err := json.Unmarshal([]byte(raw), &stamp); err != nil {
		t.Fatal(err)
	}
	back, err := json.Marshal(stamp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(back), `"later":{"a":1}`) {
		t.Fatalf("the stamp wrote back %s, want the later build's key kept", back)
	}
}
