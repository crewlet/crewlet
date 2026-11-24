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
// for its newest summary, scrubbed of what its capture is, and keeps it as an
// object of its own whatever came of the session: landed, too large at the
// take or at the seal, a take its box refused, an upload the store refused, a
// record the log never answered, no spool room, a run out of context or
// refused by its provider. The run's row names it, the turn's end names the
// same, and the session's own outcome is what it would have been without it.
//
// Mutation: keep the summary inside the take, after a refusal returns, or with
// the session's upload, and every case that loses the session loses it too.
func TestALostSessionKeepsItsSummary(t *testing.T) {
	t.Parallel()
	const text = "## Next Move\n\nRerun the cache test with the fix."
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

				if asks := thread.asks(); len(asks) != 1 || !asks[0].Summary ||
					!slices.Contains(asks[0].Scrub, handedToken) {
					t.Fatalf("the read asked %+v, want the summary once, scrubbed of what the box was handed", asks)
				}
				if put := keep.summariesPut(); !slices.Equal(put, []string{text}) {
					t.Fatalf("kept the summaries %q, want the thread's one", put)
				}
				row := rig.get("t1")
				if row.Summary == nil || row.Summary.Bytes != int64(len(text)) || row.Summary.Session == "" ||
					row.Summary.Session != row.SessionRan() ||
					row.Summary.Harness != row.Harness || row.Summary.Digest != summaryDigest(text) {
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
				named, err := row.Summary.Kept()
				if err != nil || end.Ending.Summary == nil || *end.Ending.Summary != named {
					t.Fatalf("the turn's end names %+v, want the summary its row names (%v)", end.Ending.Summary, err)
				}
			})
		})
	}
}

// A STOP'S READ KEEPS THE SUMMARY BEFORE THE TURN'S END IS RECORDED: its
// ending records the end as soon as its read returns, so a summary kept after
// the read would be named by no end.
//
// Mutation: keep the summary off the read, as the session's upload is, and the
// end names none.
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

// A SUMMARY IS READ ONLY WHERE IT CAN BE SCRUBBED, AND KEPT ONCE. A read whose
// capture's values cannot be known asks no summary at all; one finding the
// summary the row names already, under a key still fresh, keeps nothing new;
// and once that key is older than summaryFresh the same summary is kept again,
// so a long turn's end still has one the log names.
//
// Mutation: ask the summary with no scrub, drop the digest's check, or keep
// the key past its freshness, and a case goes red.
func TestASummaryIsReadOnlyWhereItCanBeScrubbedAndKeptOnce(t *testing.T) {
	t.Parallel()
	t.Run("no scrub, no summary", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, keep, thread := summaryRig(t, "a summary")
			rig.launch("t1")
			rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &rig.runner.Session.Archive})
			collectHeld(t, rig, "t1")
			for _, ask := range thread.asks() {
				if ask.Summary {
					t.Fatalf("a read whose box's handed values are unknown asked %+v", ask)
				}
			}
			if put := keep.summariesPut(); len(put) != 0 || rig.get("t1").Summary != nil {
				t.Fatalf("kept %q with nothing to scrub it of", put)
			}
		})
	})
	t.Run("kept once while fresh", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, keep, thread := summaryRig(t, "the same summary")
			run := rig.launchKept(t, "t1")
			ask := summaryAsk{asked: true, scrub: []string{handedToken}}
			found := thread.answer(t.Context(), nil, MainThreadAsk{Summary: true}).Summary
			rig.coordinator.keepSummary(t.Context(), run, ask, found)
			rig.coordinator.keepSummary(t.Context(), rig.get("t1"), ask, found)
			if put := keep.summariesPut(); len(put) != 1 {
				t.Fatalf("kept the same summary %d times while its key was fresh, want once", len(put))
			}
			rig.now = rig.now.Add(summaryFresh + time.Second)
			rig.coordinator.keepSummary(t.Context(), rig.get("t1"), ask, found)
			if put := keep.summariesPut(); len(put) != 2 {
				t.Fatalf("kept it %d times once its key was past %s, want it kept again", len(put), summaryFresh)
			}
		})
	})
}

// A SUMMARY THAT IS NOT KEPT CHANGES NOTHING ELSE: a read that could not say
// whether the thread holds one, and an upload the store refused, each leave
// the row naming no summary and the session's capture as it would be.
//
// Mutation: fail the read for a summary not kept, and the session is not kept
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
