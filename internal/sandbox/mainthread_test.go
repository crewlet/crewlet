package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// countingThread is a [FakeRunner.MainThread] that answers count and records
// what it was asked, in order.
type countingThread struct {
	mu    sync.Mutex
	count *ResumedCount
	asked []MainThreadAsk
}

func (c *countingThread) read(_ context.Context, _ Sandbox, ask MainThreadAsk) MainThreadReading {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, ask)
	if c.count == nil {
		return MainThreadReading{}
	}
	counted := *c.count
	return MainThreadReading{Count: &counted}
}

func (c *countingThread) asks() []MainThreadAsk {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]MainThreadAsk(nil), c.asked...)
}

// A COLLECTION RECORDS THE COUNT OF THE COPY ITS BOX HOLDS. The read that collects
// a Claude Code job reads the job's main thread once and writes on the job's own
// record, with how its harness ended and in the same write, what Claude Code
// counts the copy at, beside the least window the run reported: of a job whose
// harness ended its own run and of one cut off alike, since the count decides
// the copy's guard too. A stop's read and a loss's ask nothing, nor does an
// OpenCode job's collection: no relaunch carries their round on in place, and
// OpenCode's thread is not one Claude Code counts.
//
// Mutation: leave the count out of the record's write (markHarnessEnd handed no
// count), or ask the thread on every read, and a case goes red.
func TestACollectionRecordsTheCountOfTheCopyItsBoxHolds(t *testing.T) {
	t.Parallel()
	bound := &ResumedCount{Tokens: 42_000, Restart: new("")}
	for name, tc := range map[string]struct {
		read   BoxRead
		result Result
		want   *ResumedCount
	}{
		"an own run's collection": {read: BoxReadCollect,
			result: Result{Success: true, Text: "done", SessionID: "s-1", ReportedWindow: 200_000,
				Captured: &SessionArchive{SessionID: "s-1"}},
			want: &ResumedCount{Tokens: 42_000, Window: 200_000, Restart: new("")}},
		"a cut-off job's collection": {read: BoxReadCollect,
			result: Result{Success: true, Text: "done", SessionID: "s-1", HarnessCutOff: true},
			want:   &ResumedCount{Tokens: 42_000, Restart: new("")}},
		"a stop's read": {read: BoxReadStop, result: Result{Success: true, Text: "done", SessionID: "s-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			thread := &countingThread{count: bound}
			rig.runner.MainThread = thread.read
			rig.launch("t1")
			run := rig.get("t1")
			rig.runner.Finish(tc.result)
			if _, _, err := rig.coordinator.readBox(t.Context(), run, tc.read); err != nil {
				t.Fatalf("readBox: %v", err)
			}
			if got := rig.get("t1").Launch.ResumedCount; !got.same(tc.want) {
				t.Errorf("the job's record holds the count %+v, want %+v", got, tc.want)
			}
			if asked := len(thread.asks()); (tc.want != nil) != (asked == 1) || asked > 1 {
				t.Errorf("the read asked the thread %d times, want %v", asked, tc.want != nil)
			}
		})
	}
	t.Run("an OpenCode job's collection", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		thread := &countingThread{count: bound}
		rig.runner.MainThread = thread.read
		rig.launch("t1")
		run := rig.get("t1")
		run.Harness = "opencode"
		got := rig.coordinator.readMainThread(t.Context(), run, nil, rig.runner, BoxReadCollect,
			capturePoint{final: true}, Result{})
		if got.Count != nil || len(thread.asks()) != 0 {
			t.Errorf("an OpenCode job's collection counted %+v, asking %d times; want nothing asked",
				got.Count, len(thread.asks()))
		}
	})
}

// A COUNT OF A JOB NOT OVER IS UNREAD: a job its box would not end may still write
// its thread, so the read asks nothing of it and the count says the thread was
// not read whole, which reads as a copy that may compact. A runner that reads no
// thread, and one that answers nothing, are unread too.
//
// Mutation: read the thread of a job not over, or take no answer for a bound,
// and a case goes red.
func TestACountOfAJobNotOverIsUnread(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	thread := &countingThread{count: &ResumedCount{Tokens: 42_000, Restart: new("")}}
	rig.runner.MainThread = thread.read
	rig.launch("t1")
	run := rig.get("t1")
	got := rig.coordinator.readMainThread(t.Context(), run, nil, rig.runner, BoxReadCollect,
		capturePoint{final: false}, Result{})
	if got.Count == nil || got.Count.Mark != CountUnread || len(thread.asks()) != 0 {
		t.Errorf("a job not over counted %+v, asking the thread %d times; want unread, asking nothing",
			got.Count, len(thread.asks()))
	}
	thread.count = nil
	got = rig.coordinator.readMainThread(t.Context(), run, nil, rig.runner, BoxReadCollect,
		capturePoint{final: true}, Result{})
	if got.Count == nil || got.Count.Mark != CountUnread {
		t.Errorf("a read that answered nothing counted %+v, want unread", got.Count)
	}
}

// AN UNBOUNDED COUNT IS LOGGED WITH WHY, BY ITS MARK: the log of a collection's
// read that could not bound the copy says the thread was read whole only where
// it was. A thread not read whole is logged with what may have stopped the
// read, and a release the count's rules were not read in and a saved compaction
// with the thread not read at all, each found before it is opened; a mark the
// whole thread's bytes cannot bound is logged as one read whole.
//
// Mutation: log every mark as a thread read whole, and a case goes red.
func TestAnUnboundedCountIsLoggedWithWhyByItsMark(t *testing.T) {
	logs := captureLogs(t)
	rig := newCoordRig(t)
	rig.launch("t1")
	run := rig.get("t1")
	says := map[CountMark]string{CountUnread: "the thread was not read whole",
		CountRelease: "the thread was not read", CountPrecompact: "the thread was not read",
		CountMedia: "the thread was read whole"}
	for mark := range says {
		rig.runner.MainThread = (&countingThread{count: &ResumedCount{Mark: mark}}).read
		rig.coordinator.readMainThread(t.Context(), run, nil, rig.runner, BoxReadCollect, capturePoint{final: true},
			Result{})
	}
	logged := map[CountMark]string{}
	for line := range strings.Lines(logs.String()) {
		if !strings.Contains(line, "executor_session_count_unbounded") {
			continue
		}
		for mark := range says {
			if strings.Contains(line, "mark="+string(mark)+" ") {
				logged[mark] = line
			}
		}
	}
	for mark, want := range says {
		line, ok := logged[mark]
		switch {
		case !ok:
			t.Errorf("a count marked %s is not logged:\n%s", mark, logs.String())
		case !strings.Contains(line, want):
			t.Errorf("a count marked %s is logged without %q: %s", mark, want, line)
		case mark != CountMedia && strings.Contains(line, "the thread was read whole"):
			t.Errorf("a count marked %s is logged as of a thread read whole: %s", mark, line)
		}
	}
}

// THE MAIN THREAD IS READ BEFORE THE TAKE, WITHIN ITS WAIT. The read of a job's
// main thread is made on the box's host before the take, whose own budget it
// leaves whole, and within the wait the coordinator is built with
// ([MainThreadReadWait]), armed as the read starts: in a synctest bubble, the
// deadline the runner sees is the bubble's instant plus that wait, and no
// session has been taken when it reads.
//
// Mutation: read the thread after the take, arm no wait, or build the
// coordinator with another wait, and a check goes red.
func TestTheMainThreadIsReadBeforeTheTakeWithinItsWait(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		var (
			deadline time.Time
			armed    bool
			taken    = -1
			at       time.Time
		)
		rig.runner.MainThread = func(ctx context.Context, _ Sandbox, _ MainThreadAsk) MainThreadReading {
			deadline, armed = ctx.Deadline()
			taken, at = len(rig.runner.Takes()), time.Now()
			return MainThreadReading{Count: &ResumedCount{Tokens: 1, Restart: new("")}}
		}
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
		if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadCollect); err != nil {
			t.Fatalf("readBox: %v", err)
		}
		rig.awaitKeeps()
		if taken != 0 {
			t.Errorf("the thread was read with %d sessions taken, want before the take", taken)
		}
		if !armed || !deadline.Equal(at.Add(MainThreadReadWait)) {
			t.Errorf("the read's deadline is %v (armed %v), want the instant it began plus its wait, %v",
				deadline, armed, at.Add(MainThreadReadWait))
		}
	})
}

// THE BOX READ'S BUDGET HOLDS THE MAIN THREAD'S READ: a Claude Code collection's
// asker waits the read's wait more, at the host's share, whether or not its
// turn has a cap, and an OpenCode collection, a stop's and a loss's wait as
// they did. A Claude Code collection of a turn with a cap is the one read whose
// host may make both the record that the cap ended a job it saw working and the
// thread's read, so it is held with each. Every read is held against the same
// read of a run that names no harness, which asks the thread nothing: a basis
// that asked it would leave a Claude Code collection no more than it, and its
// case goes red. The figures the docs state are held too: twenty seconds more
// than another harness's collection, and in all the budget's own terms with the
// read's wait among them, about 4,680 s for a turn with no cap and about
// 5,954 s for one with a cap at today's bounds, composed rather than pinned
// because one of their terms, the slowest node's crossing of a change of the
// run's row (coord.MaxValueCrossing), is a figure of the store's.
//
// Mutation: add the wait to every read, or to none, or ask the thread for a
// run that names no harness, and a case goes red; drop the wait, or the cap's
// record, from a Claude Code collection of a capped turn alone, and its case
// goes red; change the wait, and the twenty seconds go red; drop the cap's
// record from every read, and the capped turn's figure goes red.
func TestTheBoxReadBudgetHoldsTheMainThreadRead(t *testing.T) {
	t.Parallel()
	share := time.Duration(float64(MainThreadReadWait) / BoxReadAnswerShare)
	// ANY INSTANT: the budget reads only whether the turn has a cap, since the
	// host decides on its own clock whether it has passed.
	capAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	claude, open := PendingRun{Harness: ClaudeCodeHarness}, PendingRun{Harness: "opencode"}
	capped := PendingRun{Harness: ClaudeCodeHarness, Deadline: capAt}
	for name, tc := range map[string]struct {
		run  PendingRun
		read BoxRead
		more time.Duration
	}{
		"a Claude Code collection":                  {run: claude, read: BoxReadCollect, more: share},
		"an OpenCode collection":                    {run: open, read: BoxReadCollect},
		"a Claude Code job's stop":                  {run: claude, read: BoxReadStop},
		"a Claude Code job's loss":                  {run: claude, read: BoxReadLoss},
		"a Claude Code collection of a capped turn": {run: capped, read: BoxReadCollect, more: share},
	} {
		named := tc.run
		named.Harness = ""
		if got, want := BoxReadBudget(tc.run, tc.read), BoxReadBudget(named, tc.read)+tc.more; got != want {
			t.Errorf("%s: the read's budget is %v, want %v", name, got, want)
		}
	}
	if got := BoxReadBudget(claude, BoxReadCollect) - BoxReadBudget(open, BoxReadCollect); got != 20*time.Second {
		t.Errorf("an uncapped Claude Code collection waits %v more than an OpenCode one, want 20s", got)
	}
	held := SessionTakeBudget + sessionRecordBudget + 2*discardGrace + boxReadChanges*coord.MaxValueCrossing +
		MainThreadReadWait
	for name, tc := range map[string]struct {
		run  PendingRun
		held time.Duration
	}{
		"an uncapped": {run: claude, held: held},
		// THE JOB'S END ON TOP, and the record that the cap ended a job its
		// host saw working: a teardown's grace and one more change of the row
		// at the slowest node's crossing.
		"a capped": {run: capped, held: held + JobEndBudget + discardGrace + coord.MaxValueCrossing},
	} {
		if got, want := BoxReadBudget(tc.run, BoxReadCollect),
			pollBound+time.Duration(float64(tc.held)/BoxReadAnswerShare); got != want {
			t.Errorf("%s turn's Claude Code collection's budget is %v, want %v", name, got, want)
		}
	}
}

// A CLAIM IS TOLD THE COUNT THE READ MADE. The claim the collection is made for
// was read before the read wrote the job's record, and the resume that follows on
// it judges a round carried on in the job's box by the count the claim carries:
// so the collection merges the read's count onto the claim by the rule the record
// follows.
//
// Mutation: leave the claim's count as it was read, and the resume reads none.
func TestAClaimIsToldTheCountTheReadMade(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.runner.MainThread = (&countingThread{count: &ResumedCount{Tokens: 42_000, Restart: new("")}}).read
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1"})
	var (
		mu      sync.Mutex
		claimed []*ResumedCount
	)
	rig.resumer.failWith(errors.New("held for the case to read the claim"))
	rig.resumer.during = func(_ context.Context, run PendingRun) {
		mu.Lock()
		defer mu.Unlock()
		claimed = append(claimed, run.Launch.ResumedCount)
	}
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Logf("OnCompleted: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := (&ResumedCount{Tokens: 42_000, Restart: new("")}); len(claimed) != 1 || !claimed[0].same(want) {
		t.Errorf("the resume on the claim read the counts %+v, want once %+v", claimed, want)
	}
}

// THE CAPTURE STAMPS ITS COUNT. A take that counts the session it keeps stamps the
// count on the capture beside its context, with the least window the job's run
// reported: what a launch that continues the capture judges its first request by.
//
// Mutation: stamp no count, or no window, and this goes red.
func TestTheCaptureStampsItsCount(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		session.Count = &ResumedCount{Tokens: 42_000, Restart: new("u-1")}
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive,
			ReportedWindow: 200_000})
		if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadCollect); err != nil {
			t.Fatalf("readBox: %v", err)
		}
		rig.awaitKeeps()
		run := rig.get("t1")
		want := &ResumedCount{Tokens: 42_000, Window: 200_000, Restart: new("u-1")}
		if run.Capture == nil || !run.Capture.ResumedCount.same(want) {
			t.Fatalf("the capture is stamped %+v, want the count %+v", run.Capture, want)
		}
	})
}

// AN OLDER BUILD'S WRITE CARRIES THE COUNT. A fleet mid-upgrade rewrites the job's
// record and the capture's stamp on a build that does not know the field, and a
// relaunch that reads them may run on this build: so the count and the restart
// basis ride each record under their own keys, which a build that does not
// declare them keeps verbatim and writes back.
//
// Mutation: leave a field off the wire, or drop the carried keys from the
// encode, and a case goes red.
func TestAnOlderBuildsWriteCarriesTheCount(t *testing.T) {
	t.Parallel()
	count := &ResumedCount{Tokens: 42_000, Mark: CountMedia, Window: 200_000, Restart: new("u-1")}
	carried := func(t *testing.T, typ reflect.Type, keys []string, value any, read func([]byte) any) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		declared := wireKeys(typ)
		older := maps.Clone(declared)
		for _, key := range keys {
			if !declared[key] {
				t.Fatalf("%s declares no key %s: %v", typ, key, declared)
			}
			delete(older, key)
		}
		var plain map[string]json.RawMessage
		extra, err := unmarshalCarrying(raw, &plain, older)
		if err != nil {
			t.Fatal(err)
		}
		written, err := marshalCarrying(struct{}{}, older, extra)
		if err != nil {
			t.Fatal(err)
		}
		if got := read(written); !reflect.DeepEqual(got, read(raw)) {
			t.Errorf("after the older build's write %s reads %+v, want %+v: %s", typ, got, read(raw), written)
		}
	}
	carried(t, reflect.TypeFor[LaunchRecord](), []string{"resumed_count", "basis_restart"},
		LaunchRecord{ResumedCount: count, BasisRestart: new("u-0")}, func(raw []byte) any {
			var back LaunchRecord
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			return []any{back.ResumedCount, back.BasisRestart}
		})
	carried(t, reflect.TypeFor[SessionCapture](), []string{"resumed_count"},
		SessionCapture{ResumedCount: count}, func(raw []byte) any {
			var back SessionCapture
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			return back.ResumedCount
		})
}

// A LAUNCH RECORDS THE RESTART OF THE SESSION IT HANDS ITS RUN: the newest place
// that session's thread restarts at, as the launch knows it, so a later relaunch
// knows whether the job restarted the session after its brief. A relaunch that
// continues the copy the job before left in its box records that job's
// collection's read of it, read by the reset in the write that replaced that
// record, a read marked for what its bytes cannot bound as much as a bound, and
// nothing known where that read did not see the thread whole; one that
// continues a kept capture records the capture's own read of its archive, which
// is marked here; and one that starts a session of its own records a thread
// that restarts nowhere. A mark of what a whole thread's bytes cannot bound
// says nothing of where it restarts, so such a read is a known basis.
//
// Mutation: record nothing, the copy's count where it did not read the thread
// whole, a marked count's as not known at either place, or a fresh session's as
// not known, and a case goes red.
func TestALaunchRecordsTheRestartOfTheSessionItHandsItsRun(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		read *ResumedCount
		want *string
	}{
		"the copy in its box, read whole":     {read: &ResumedCount{Tokens: 1, Restart: new("u-9")}, want: new("u-9")},
		"the copy in its box, not read whole": {read: &ResumedCount{Mark: CountUnread}},
		"the copy in its box, read whole and marked": {read: &ResumedCount{Mark: CountMedia, Restart: new("u-9")},
			want: new("u-9")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			askAndAnswer(t, rig, false, func(ctx context.Context, r PendingRun, asked *AskedRun) {
				if _, err := rig.pending.MarkHarnessEnd(ctx, "t1", r.LaunchID, EndRead{End: HarnessEndOwnRun,
					Count: tc.read}); err != nil {
					t.Errorf("MarkHarnessEnd: %v", err)
				}
				if _, err := rig.launchVia(ctx, rig.manager, continuing(asked)); err != nil {
					t.Errorf("relaunch: %v", err)
				}
			})
			if got := rig.get("t1").Launch.BasisRestart; deref(got) != deref(tc.want) {
				t.Errorf("the job continuing the copy in its box records the restart %s, want %s", deref(got),
					deref(tc.want))
			}
		})
	}
	t.Run("a kept capture", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, restore, kept := keptTurn(t)
			if _, _, err := rig.pending.mutate(t.Context(), "t1", func(run *PendingRun) bool {
				run.Kept.ResumedCount = &ResumedCount{Mark: CountMedia, Restart: new("u-7")}
				return true
			}); err != nil {
				t.Fatalf("counting the kept capture: %v", err)
			}
			if _, err := launchRestoring(t, rig, restore, continuingKept(kept)); err != nil {
				t.Fatalf("relaunch: %v", err)
			}
			if got := rig.get("t1").Launch.BasisRestart; deref(got) != "u-7" {
				t.Errorf("the job continuing the kept capture records the restart %s, want the capture's u-7",
					deref(got))
			}
		})
	})
	t.Run("a session of its own", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		if _, err := rig.coordinator.Launch(t.Context(), rig.manager, launchReq("t1")); err != nil {
			t.Fatalf("launch: %v", err)
		}
		if got := rig.get("t1").Launch.BasisRestart; got == nil || *got != "" {
			t.Errorf("a job starting a session of its own records the restart %s, want one that restarts nowhere",
				deref(got))
		}
	})
}

// deref is what a restart reads, "<nil>" for none known.
func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// A CAPTURE'S COUNT IS HELD TO WHAT A ROW CARRIES: a stamp naming a mark past its
// bound, or a restart past a uuid's thirty-six bytes, is refused before it reaches
// the row, as every text a box describes its session by is; and the take's own
// stamp writes such a mark as unread rather than lose the capture to the refusal.
//
// Mutation: let the stamp carry a mark or a restart past its bound, or stamp the
// take's count unbounded, and a case goes red.
func TestACapturesCountIsHeldToWhatARowCarries(t *testing.T) {
	t.Parallel()
	long := CountMark(strings.Repeat("m", maxCountMarkBytes+1))
	for name, count := range map[string]*ResumedCount{
		"a mark past its bound":    {Mark: long},
		"a restart past its bound": {Tokens: 1, Restart: new(strings.Repeat("u", RestartIDMaxBytes+1))},
	} {
		if err := (SessionCapture{ResumedCount: count}).Bounded(); !errors.Is(err, ErrCaptureUnbounded) {
			t.Errorf("%s: the stamp is refused with %v, want %v", name, err, ErrCaptureUnbounded)
		}
	}
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		session.Count = &ResumedCount{Mark: long}
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
		if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadCollect); err != nil {
			t.Fatalf("readBox: %v", err)
		}
		rig.awaitKeeps()
		run := rig.get("t1")
		if run.Capture == nil || run.Capture.ResumedCount == nil || run.Capture.ResumedCount.Mark != CountUnread {
			t.Errorf("the take's count past its bound is stamped %+v, want the capture kept and its count unread",
				run.Capture)
		}
	})
}
